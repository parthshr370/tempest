package control

import (
	"slices"
	"strconv"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
)

// holdRank orders holds by precedence; a lower rank wins.
var holdRank = map[model.Hold]int{
	model.HoldRevoked:   0,
	model.HoldBudget:    1,
	model.HoldFailed:    2,
	model.HoldBlocked:   3,
	model.HoldWakeLimit: 4,
}

// setHold applies a hold unless one of higher precedence is in place.
func setHold(ticket *model.Ticket, hold model.Hold, reason string) {
	if ticket.Hold != model.HoldNone && holdRank[ticket.Hold] < holdRank[hold] {
		return
	}
	ticket.Hold, ticket.HoldReason = hold, reason
}

// releaseHold clears the Ticket's hold if it is one of holds.
func releaseHold(ticket *model.Ticket, holds ...model.Hold) {
	if slices.Contains(holds, ticket.Hold) {
		ticket.Hold, ticket.HoldReason = model.HoldNone, ""
	}
}

func live(state model.DispatchState) bool {
	return state == model.DispatchAdmitted || state == model.DispatchPreparing || state == model.DispatchSubmitted
}

func pending(state model.DispatchState) bool {
	return state == model.DispatchQueued || live(state)
}

// latestDispatch returns the Ticket's newest dispatch. Only the newest can
// be queued or live: a Ticket gets a new dispatch only once the previous one
// settled or was cancelled.
func (s *Store) latestDispatch(ticket *model.Ticket) *model.DispatchIntent {
	if ticket.DispatchSeq == 0 {
		return nil
	}
	dispatch, ok := s.dispatches.get(string(model.NewDispatchID(ticket.ID, ticket.DispatchSeq)))
	if !ok {
		return nil
	}
	return dispatch
}

// subscribe adds a Ticket to a thread's subscription list, creating the
// thread record when it does not exist yet.
func (s *Store) subscribe(ref model.ThreadRef, id model.TicketID, title string) {
	key := strconv.Itoa(ref.Number)
	thread := s.threads.edit(key)
	if thread == nil {
		thread = s.threads.put(key, model.Thread{Ref: ref, Title: title})
	}
	if thread.Ref.IsPR != ref.IsPR && ref.IsPR {
		thread.Ref.IsPR = true
	}
	if !slices.Contains(thread.Tickets, id) {
		thread.Tickets = append(thread.Tickets, id)
	}
	thread.WatchUntil = time.Time{}
}

// createTicket admits a request: Mandate, Ticket, subscription, the first
// Input, Assignment gen 1, the launch dispatch and the Ticket ledger.
func (s *Store) createTicket(event model.Event, now time.Time) model.TicketID {
	id := model.NewTicketID(event.Thread, event.ID)
	title := event.Title
	if thread, ok := s.threads.get(strconv.Itoa(event.Thread.Number)); ok && thread.Title != "" {
		title = thread.Title
	}
	s.mandates.add(string(id), model.Mandate{
		Ticket: id, Repo: s.settings.repo, Thread: event.Thread, Actor: event.Actor,
		Basis: model.BasisExplicitMention, RequestEvent: event.ID, PolicyDigest: s.policy.Digest, CreatedAt: now,
	})
	ticket := s.tickets.put(string(id), model.Ticket{
		ID: id, Repo: s.settings.repo, Thread: event.Thread, Title: title, Branch: model.TicketBranch(id),
		CreatedAt: now, Lifecycle: model.LifecycleOpen, Gen: 1,
	})
	if event.Thread.IsPR {
		ticket.PRs = []model.PRLink{{Number: event.Thread.Number}}
		s.syncLinks(ticket)
	}
	s.subscribe(event.Thread, id, title)
	s.assignments.put(model.AssignmentKey(id, 1), model.Assignment{Ticket: id, Gen: 1, Reason: model.ReasonLaunch, State: model.AssignmentOpen, StartedAt: now})
	s.ticketLedger(id)
	s.addInput(ticket, event.ID, model.InputAuthoritative, true, now)
	s.queueDispatch(ticket, model.DispatchLaunch, now, time.Time{}, 0)
	return id
}

// addInput appends one Input to a Ticket.
func (s *Store) addInput(ticket *model.Ticket, event model.EventID, class model.InputClass, wakes bool, now time.Time) {
	ticket.InputSeq++
	s.inputs.add(model.InputKey(ticket.ID, ticket.InputSeq), model.Input{Ticket: ticket.ID, Seq: ticket.InputSeq, Event: event, Class: class, Wakes: wakes, At: now})
	if dispatch := s.latestDispatch(ticket); dispatch != nil && dispatch.State == model.DispatchSubmitted {
		s.work.dispatch = true
	}
}

// deliverInput appends an Input to an open Ticket and wakes it when the
// Input's class may.
func (s *Store) deliverInput(id model.TicketID, event model.EventID, class model.InputClass, wakes bool, now time.Time) {
	ticket := s.tickets.edit(string(id))
	if ticket == nil || ticket.Lifecycle != model.LifecycleOpen {
		return
	}
	s.addInput(ticket, event, class, wakes, now)
	if class == model.InputAuthoritative {
		releaseHold(ticket, model.HoldFailed, model.HoldBlocked, model.HoldWakeLimit)
	}
	if wakes {
		s.wake(ticket, class, now)
	}
}

// wake queues a dispatch for new waking Input unless one is already queued
// or live (it then covers the Input), a hold blocks automatic wakes, or the
// machine auto-wake limit is reached. Revoked and budget holds still queue:
// the dispatch waits until the operator or a new budget period releases
// them.
func (s *Store) wake(ticket *model.Ticket, class model.InputClass, now time.Time) {
	if ticket.Lifecycle != model.LifecycleOpen || ticket.Stopped {
		return
	}
	if dispatch := s.latestDispatch(ticket); dispatch != nil && pending(dispatch.State) {
		return
	}
	switch ticket.Hold {
	case model.HoldFailed, model.HoldBlocked, model.HoldWakeLimit:
		return
	}
	if class == model.InputMachine {
		day := now.UTC().Format(time.DateOnly)
		if ticket.AutoWakeDay != day {
			ticket.AutoWakeDay, ticket.AutoWakes = day, 0
		}
		if ticket.AutoWakes >= s.settings.maxAutoWakes {
			setHold(ticket, model.HoldWakeLimit, "automatic wakes reached "+strconv.Itoa(s.settings.maxAutoWakes)+" today")
			return
		}
		ticket.AutoWakes++
	}
	kind := model.DispatchWake
	if ticket.SessionID == "" {
		kind = model.DispatchLaunch
	}
	s.queueDispatch(ticket, kind, now, time.Time{}, 0)
}

// queueDispatch appends a queued dispatch of the Ticket's current
// generation.
func (s *Store) queueDispatch(ticket *model.Ticket, kind model.DispatchKind, now, retryAt time.Time, attempts int) *model.DispatchIntent {
	ticket.DispatchSeq++
	id := model.NewDispatchID(ticket.ID, ticket.DispatchSeq)
	s.work.dispatch = true
	return s.dispatches.put(string(id), model.DispatchIntent{
		ID: id, Ticket: ticket.ID, Seq: ticket.DispatchSeq, Gen: ticket.Gen, Kind: kind, Class: model.SlotNormal,
		State: model.DispatchQueued, ThroughInput: ticket.InputSeq,
		RequestID: model.LaunchRequestID(ticket.ID, ticket.DispatchSeq, ticket.Gen),
		Attempts:  attempts, RetryAt: retryAt, QueuedAt: now,
	})
}

// cancelQueued cancels the Ticket's queued dispatch, if any.
func (s *Store) cancelQueued(ticket *model.Ticket, reason string, now time.Time) {
	dispatch := s.latestDispatch(ticket)
	if dispatch == nil || dispatch.State != model.DispatchQueued {
		return
	}
	edit := s.dispatches.edit(string(dispatch.ID))
	edit.State, edit.CancelReason, edit.FinishedAt = model.DispatchCancelled, reason, now
}

// endAssignment ends the Ticket's current generation.
func (s *Store) endAssignment(ticket *model.Ticket, reason string, now time.Time) {
	a := s.assignments.edit(model.AssignmentKey(ticket.ID, ticket.Gen))
	if a == nil || a.State == model.AssignmentEnded {
		return
	}
	a.State, a.EndedAt, a.EndReason = model.AssignmentEnded, now, reason
}

// newGeneration ends the current Assignment as replaced, starts the next
// one and queues its first dispatch.
func (s *Store) newGeneration(ticket *model.Ticket, reason model.AssignmentReason, kind model.DispatchKind, now time.Time) {
	s.cancelQueued(ticket, "replaced by generation "+strconv.Itoa(int(ticket.Gen+1)), now)
	s.endAssignment(ticket, "replaced", now)
	ticket.Gen++
	s.assignments.put(model.AssignmentKey(ticket.ID, ticket.Gen), model.Assignment{Ticket: ticket.ID, Gen: ticket.Gen, Reason: reason, State: model.AssignmentOpen, StartedAt: now})
	s.queueDispatch(ticket, kind, now, time.Time{}, 0)
}

// reopenTicket reopens an ended, non-stopped Ticket for an authoritative
// mention. The new generation gets its own Mandate, keyed
// AssignmentKey(ticket, gen), naming the reopening actor: admission
// revalidates that actor, not the original requester.
func (s *Store) reopenTicket(id model.TicketID, event model.Event, now time.Time) {
	ticket := s.tickets.edit(string(id))
	if ticket == nil || ticket.Lifecycle != model.LifecycleEnded || ticket.Stopped {
		return
	}
	ticket.Lifecycle, ticket.Outcome, ticket.EndedBy, ticket.EndedAt = model.LifecycleOpen, "", "", time.Time{}
	ticket.Hold, ticket.HoldReason = model.HoldNone, ""
	s.syncLinks(ticket)
	for _, ref := range s.ownThreads(ticket) {
		s.subscribe(ref, ticket.ID, "")
	}
	s.addInput(ticket, event.ID, model.InputAuthoritative, true, now)
	s.newGeneration(ticket, model.ReasonReopen, model.DispatchLaunch, now)
	s.mandates.add(model.AssignmentKey(ticket.ID, ticket.Gen), model.Mandate{
		Ticket: ticket.ID, Repo: s.settings.repo, Thread: event.Thread, Actor: event.Actor,
		Basis: model.BasisExplicitMention, RequestEvent: event.ID, PolicyDigest: s.policy.Digest, CreatedAt: now,
	})
}

// stopTicket ends a Ticket as stopped, permanently: queued dispatches and
// pending intents are cancelled; a live dispatch keeps its slot until the
// dispatcher cancels the run and settles it, and attempting intents still
// reconcile.
func (s *Store) stopTicket(id model.TicketID, by string, now time.Time) bool {
	ticket := s.tickets.edit(string(id))
	if ticket == nil || ticket.Stopped {
		return false
	}
	if ticket.Lifecycle == model.LifecycleOpen {
		ticket.Lifecycle, ticket.Outcome, ticket.EndedBy, ticket.EndedAt = model.LifecycleEnded, model.OutcomeStopped, by, now
		s.watchUntil(ticket, now)
	}
	ticket.Stopped = true
	s.cancelQueued(ticket, "stopped", now)
	s.endAssignment(ticket, "stopped", now)
	for _, intentID := range s.intentsOf(ticket.ID) {
		intent, _ := s.outbox.get(string(intentID))
		if intent.State == model.EffectPending {
			edit := s.outbox.edit(string(intentID))
			edit.State, edit.LastError, edit.SettledAt = model.EffectCancelled, "ticket stopped", now
		}
	}
	s.work.dispatch, s.work.outbox = true, true
	return true
}

// endTicket ends an open Ticket with a non-stop outcome.
func (s *Store) endTicket(ticket *model.Ticket, outcome model.Outcome, by string, now time.Time) {
	ticket.Lifecycle, ticket.Outcome, ticket.EndedBy, ticket.EndedAt = model.LifecycleEnded, outcome, by, now
	s.cancelQueued(ticket, "ticket ended", now)
	s.endAssignment(ticket, "ticket_ended", now)
	s.watchUntil(ticket, now)
	s.work.dispatch = true
}

// watchUntil keeps the Ticket's threads polled for the retention period.
func (s *Store) watchUntil(ticket *model.Ticket, now time.Time) {
	until := now.Add(s.settings.retention)
	for _, ref := range s.ownThreads(ticket) {
		thread := s.threads.edit(strconv.Itoa(ref.Number))
		if thread != nil && thread.WatchUntil.Before(until) {
			thread.WatchUntil = until
		}
	}
}

// ownThreads is the request thread plus every PR link.
func (s *Store) ownThreads(ticket *model.Ticket) []model.ThreadRef {
	refs := []model.ThreadRef{ticket.Thread}
	for _, link := range ticket.PRs {
		if link.Number != ticket.Thread.Number {
			refs = append(refs, model.ThreadRef{Number: link.Number, IsPR: true})
		}
	}
	return refs
}

// syncLinks copies closed and merged state from PR records into links.
func (s *Store) syncLinks(ticket *model.Ticket) {
	for i := range ticket.PRs {
		pr, ok := s.prs.get(strconv.Itoa(ticket.PRs[i].Number))
		if !ok {
			continue
		}
		ticket.PRs[i].Closed = pr.State == "closed" || pr.Merged
		ticket.PRs[i].Merged = pr.Merged
	}
}

// unfinished reports work that defers every end except stop: a live
// dispatch, or an authoritative Input not yet handled.
func (s *Store) unfinished(ticket *model.Ticket) bool {
	if dispatch := s.latestDispatch(ticket); dispatch != nil && live(dispatch.State) {
		return true
	}
	return s.authoritativeIn(ticket.ID, ticket.HandledSeq, ticket.InputSeq)
}

// authoritativeIn reports an authoritative Input with after < Seq <= through.
func (s *Store) authoritativeIn(id model.TicketID, after, through uint64) bool {
	for seq := after + 1; seq <= through; seq++ {
		input, ok := s.inputs.get(model.InputKey(id, seq))
		if ok && input.Class == model.InputAuthoritative {
			return true
		}
	}
	return false
}

// completion decides whether an open Ticket ends now: the completion set
// (own PRs if any, else every link) all closed ends it merged or closed by
// the latest close; otherwise a dropped or ready report (ready only without
// PR links). report is the report that may end the Ticket, nil when none
// does (see endingReport). Unfinished work defers every end.
func completion(ticket model.Ticket, prs map[int]model.PullRequest, report *model.Report, unfinished bool) (model.Outcome, bool) {
	if ticket.Lifecycle != model.LifecycleOpen || unfinished {
		return "", false
	}
	var set []model.PRLink
	for _, link := range ticket.PRs {
		if link.Own {
			set = append(set, link)
		}
	}
	if len(set) == 0 {
		set = ticket.PRs
	}
	if len(set) > 0 {
		closed := true
		var latest time.Time
		merged := false
		for _, link := range set {
			if !link.Closed {
				closed = false
				break
			}
			at := prs[link.Number].ClosedAt
			if latest.IsZero() || at.After(latest) || at.Equal(latest) && link.Merged {
				latest, merged = at, link.Merged
			}
		}
		if closed {
			if merged {
				return model.OutcomeMerged, true
			}
			return model.OutcomeClosed, true
		}
	}
	if report == nil {
		return "", false
	}
	switch {
	case report.Status == model.ReportDropped:
		return model.OutcomeDropped, true
	case report.Status == model.ReportReady && len(ticket.PRs) == 0:
		return model.OutcomeCompleted, true
	}
	return "", false
}

// endingReport returns the Ticket's LastReport when it may end the Ticket:
// it is of the current generation, it was made during the newest dispatch
// that ran (queued and cancelled dispatches never ran), that dispatch
// finished completed, and the report covers every authoritative Input the
// dispatch consumed (a continuation may consume one after the report). A
// report followed by a failed, budget-stopped, aborted or unknown run, or a
// later dispatch that finished without a report, ends nothing.
func (s *Store) endingReport(ticket *model.Ticket) *model.Report {
	report := ticket.LastReport
	if report == nil || report.Gen != ticket.Gen {
		return nil
	}
	during, ok := s.reports.get(string(ticket.ID))
	if !ok {
		return nil
	}
	for seq := ticket.DispatchSeq; seq > 0; seq-- {
		dispatch, ok := s.dispatches.get(string(model.NewDispatchID(ticket.ID, seq)))
		if !ok || dispatch.State == model.DispatchQueued || dispatch.State == model.DispatchCancelled {
			continue
		}
		if dispatch.ID != *during || dispatch.State != model.DispatchFinished || dispatch.Outcome == nil || dispatch.Outcome.StopReason != "completed" {
			return nil
		}
		if s.authoritativeIn(ticket.ID, report.ThroughInput, dispatch.ThroughInput) {
			return nil
		}
		return report
	}
	return nil
}

// evaluateEnd runs the completion rule for a Ticket and ends it if due.
func (s *Store) evaluateEnd(id model.TicketID, now time.Time) {
	ticket, ok := s.tickets.get(string(id))
	if !ok || ticket.Lifecycle != model.LifecycleOpen {
		return
	}
	edit := s.tickets.edit(string(id))
	s.syncLinks(edit)
	prs := make(map[int]model.PullRequest, len(edit.PRs))
	for _, link := range edit.PRs {
		if pr, ok := s.prs.get(strconv.Itoa(link.Number)); ok {
			prs[link.Number] = *pr
		}
	}
	outcome, end := completion(*edit, prs, s.endingReport(edit), s.unfinished(edit))
	if !end {
		return
	}
	by := "executor"
	if outcome == model.OutcomeMerged || outcome == model.OutcomeClosed {
		by = "source"
	}
	s.endTicket(edit, outcome, by, now)
}

// bindOwnPR links a PR this Ticket's branch produced as own. A PR binds to
// at most one open Ticket. A PR not read yet gets a placeholder with a zero
// UpdatedAt: no GitHub fact about it is known, so the first one applies.
func (s *Store) bindOwnPR(id model.TicketID, number int) bool {
	key := strconv.Itoa(number)
	pr := s.prs.edit(key)
	if pr == nil {
		pr = s.prs.put(key, model.PullRequest{Number: number, HeadRef: model.TicketBranch(id), HeadRepo: s.settings.repo, State: "open"})
	}
	if pr.Owner != "" && pr.Owner != id {
		if other, ok := s.tickets.get(string(pr.Owner)); ok && other.Lifecycle == model.LifecycleOpen {
			return false
		}
	}
	pr.Owner = id
	ticket := s.tickets.edit(string(id))
	if ticket == nil {
		return false
	}
	found := false
	for i := range ticket.PRs {
		if ticket.PRs[i].Number == number {
			ticket.PRs[i].Own, found = true, true
		}
	}
	if !found {
		ticket.PRs = append(ticket.PRs, model.PRLink{Number: number, Own: true})
	}
	s.syncLinks(ticket)
	s.subscribe(model.ThreadRef{Number: number, IsPR: true}, id, "")
	return true
}
