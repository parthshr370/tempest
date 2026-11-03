package control

import (
	"context"
	"strconv"

	"go.harness.dev/harness/internal/githubsource/model"
)

// CurrentReviews returns the reviews of a PR's current head that are still
// in force. Reviews of an older head never describe the current one.
func CurrentReviews(pr model.PullRequest) []model.ReviewFact {
	var current []model.ReviewFact
	for _, review := range pr.Reviews {
		if review.CommitSHA == pr.HeadSHA && review.State != "DISMISSED" {
			current = append(current, review)
		}
	}
	return current
}

// CurrentCI returns the PR's CI snapshot only when it describes the current
// head.
func CurrentCI(pr model.PullRequest) *model.CISnapshot {
	if pr.CI == nil || pr.CI.SHA != pr.HeadSHA {
		return nil
	}
	return pr.CI
}

// awaitingReview reports an open PR whose current head is not failing CI
// and has no changes requested at that head.
func awaitingReview(pr model.PullRequest) bool {
	if pr.State == "closed" || pr.Merged {
		return false
	}
	if ci := CurrentCI(pr); ci != nil && len(ci.Failing) > 0 {
		return false
	}
	for _, review := range CurrentReviews(pr) {
		if review.State == "CHANGES_REQUESTED" {
			return false
		}
	}
	return true
}

// Stage derives an open Ticket's attention-first stage from its records;
// the first matching condition wins. dispatch is the Ticket's newest
// dispatch (nil if none); prs are the records of its PR links; outbox holds
// its intents.
func Stage(ticket model.Ticket, dispatch *model.DispatchIntent, prs []model.PullRequest, outbox []model.OutboxIntent) model.Stage {
	if ticket.Lifecycle != model.LifecycleOpen {
		return model.StageEnded
	}
	for _, intent := range outbox {
		if intent.State == model.EffectUnknown {
			return model.StageDelivery
		}
	}
	if ticket.DeliveryFailed != "" {
		return model.StageDelivery
	}
	report := ticket.LastReport
	if report != nil && report.Gen != ticket.Gen {
		report = nil
	}
	if ticket.Hold == model.HoldBlocked && report != nil && report.Question != nil {
		return model.StageAnswer
	}
	running := dispatch != nil && pending(dispatch.State)
	if !running && report != nil && report.Status == model.ReportReady && reviewable(ticket, prs) {
		return model.StageReview
	}
	switch ticket.Hold {
	case model.HoldBlocked:
		return model.StageBlocked
	case model.HoldFailed:
		return model.StageFailed
	case model.HoldNone:
	default:
		return model.StageHeld
	}
	switch {
	case dispatch != nil && dispatch.State == model.DispatchQueued:
		return model.StageQueued
	case dispatch != nil && live(dispatch.State):
		return model.StageWorking
	}
	return model.StageIdle
}

// reviewable reports that the completion set has an open PR awaiting a
// person: own PRs if any, else every link.
func reviewable(ticket model.Ticket, prs []model.PullRequest) bool {
	byNumber := make(map[int]model.PullRequest, len(prs))
	for _, pr := range prs {
		byNumber[pr.Number] = pr
	}
	hasOwn := false
	for _, link := range ticket.PRs {
		hasOwn = hasOwn || link.Own
	}
	for _, link := range ticket.PRs {
		if hasOwn && !link.Own {
			continue
		}
		if pr, ok := byNumber[link.Number]; ok && awaitingReview(pr) {
			return true
		}
	}
	return false
}

// prRecords returns the PR records of a Ticket's links.
func (s *Store) prRecords(ticket *model.Ticket) []model.PullRequest {
	var prs []model.PullRequest
	for _, link := range ticket.PRs {
		if pr, ok := s.prs.get(strconv.Itoa(link.Number)); ok {
			prs = append(prs, clone(*pr))
		}
	}
	return prs
}

func (s *Store) intents(ticket *model.Ticket) []model.OutboxIntent {
	ids := s.intentsOf(ticket.ID)
	intents := make([]model.OutboxIntent, 0, len(ids))
	for _, id := range ids {
		intent, _ := s.outbox.get(string(id))
		intents = append(intents, clone(*intent))
	}
	return intents
}

func (s *Store) stageOf(ticket *model.Ticket) model.Stage {
	return Stage(*ticket, s.latestDispatch(ticket), s.prRecords(ticket), s.intents(ticket))
}

// detail assembles a Ticket's full view.
func (s *Store) detail(ctx context.Context, ticket *model.Ticket) (model.TicketDetail, error) {
	detail := model.TicketDetail{
		Ticket:       clone(*ticket),
		Stage:        s.stageOf(ticket),
		Assignments:  []model.Assignment{},
		Dispatches:   []model.DispatchIntent{},
		Outbox:       s.intents(ticket),
		PullRequests: s.prRecords(ticket),
	}
	detail.Mandate, _ = s.mandates.get(string(ticket.ID))
	for gen := uint32(1); gen <= ticket.Gen; gen++ {
		if assignment, ok := s.assignments.get(model.AssignmentKey(ticket.ID, gen)); ok {
			detail.Assignments = append(detail.Assignments, clone(*assignment))
		}
	}
	for seq := uint64(1); seq <= ticket.DispatchSeq; seq++ {
		if dispatch, ok := s.dispatches.get(string(model.NewDispatchID(ticket.ID, seq))); ok {
			detail.Dispatches = append(detail.Dispatches, clone(*dispatch))
		}
	}
	if ledger, ok := s.ledgers.get(string(model.TicketLedger(ticket.ID))); ok {
		detail.Ledger = clone(*ledger)
	}
	inputs, err := s.inputViews(ctx, ticket, 0, maxInputLimit)
	if err != nil {
		return model.TicketDetail{}, err
	}
	detail.Inputs = inputs
	if detail.Inputs == nil {
		detail.Inputs = []model.InputView{}
	}
	return detail, nil
}

func (s *Store) summary(ticket *model.Ticket) model.TicketSummary {
	summary := model.TicketSummary{
		ID: ticket.ID, Title: ticket.Title, Thread: ticket.Thread, Lifecycle: ticket.Lifecycle, Outcome: ticket.Outcome,
		Stage: s.stageOf(ticket), Hold: ticket.Hold, PRs: clone(ticket.PRs), SessionID: ticket.SessionID,
	}
	if ticket.LastReport != nil && ticket.LastReport.Gen == ticket.Gen && ticket.LastReport.Question != nil {
		question := *ticket.LastReport.Question
		summary.Question = &question
	}
	if ledger, ok := s.ledgers.get(string(model.TicketLedger(ticket.ID))); ok {
		summary.Spent = ledger.Spent
	}
	return summary
}

// attention reports a person/* stage.
func attention(stage model.Stage) bool {
	switch stage {
	case model.StageDelivery, model.StageAnswer, model.StageReview, model.StageBlocked, model.StageFailed, model.StageHeld:
		return true
	}
	return false
}

// stageRank orders stages for the attention listing.
var stageRank = map[model.Stage]int{
	model.StageDelivery: 0, model.StageAnswer: 1, model.StageReview: 2, model.StageBlocked: 3,
	model.StageFailed: 4, model.StageHeld: 5, model.StageQueued: 6, model.StageWorking: 7, model.StageIdle: 8, model.StageEnded: 9,
}
