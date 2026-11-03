package control

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
	"go.harness.dev/harness/internal/retry"
	"go.harness.dev/harness/tempest/durable"
)

// errState reports a call that does not fit the record's current state.
func errState(subject string, state any, call string) error {
	return fmt.Errorf("control: %s is %v, cannot %s: %w", subject, state, call, durable.ErrConflict)
}

// occupancy counts slots held by live dispatches, from the records.
func (s *Store) occupancy() model.Occupancy {
	occupancy := model.Occupancy{MaxNormal: s.settings.maxNormal, MaxHuman: humanSlots}
	for _, id := range s.tickets.ids() {
		ticket, _ := s.tickets.get(id)
		dispatch := s.latestDispatch(ticket)
		if dispatch == nil || !live(dispatch.State) {
			continue
		}
		if dispatch.Slot == model.SlotHuman {
			occupancy.Human++
		} else {
			occupancy.Normal++
		}
	}
	return occupancy
}

// dispatchClass decides the scheduler class: human when the newest
// pending waking Input is authoritative or an operator retry restarts a
// worker; launches of new workers are always normal.
func (s *Store) dispatchClass(ticket *model.Ticket, dispatch *model.DispatchIntent) model.SlotClass {
	if ticket.SessionID == "" {
		return model.SlotNormal
	}
	if dispatch.Kind == model.DispatchLaunch {
		if assignment, ok := s.assignments.get(model.AssignmentKey(ticket.ID, ticket.Gen)); ok && assignment.Reason == model.ReasonRetry {
			return model.SlotHuman
		}
	}
	for seq := ticket.InputSeq; seq > ticket.HandledSeq; seq-- {
		input, ok := s.inputs.get(model.InputKey(ticket.ID, seq))
		if !ok || !input.Wakes {
			continue
		}
		if input.Class == model.InputAuthoritative {
			return model.SlotHuman
		}
		return model.SlotNormal
	}
	return model.SlotNormal
}

// authorized revalidates the current generation's authority at admission.
// Walking back from the current generation, the first generation an
// operator retry or a reopen started decides: a retry is the operator's
// authority, a reopen is its reopening actor's Mandate. Recovery
// generations inherit; generation 1 is the request's Mandate. A Mandate
// authorizes only while its actor is listed.
func (s *Store) authorized(ticket *model.Ticket) bool {
	for gen := ticket.Gen; gen > 1; gen-- {
		assignment, ok := s.assignments.get(model.AssignmentKey(ticket.ID, gen))
		if !ok {
			continue
		}
		if assignment.Reason == model.ReasonRetry {
			return true
		}
		if assignment.Reason == model.ReasonReopen {
			break
		}
	}
	mandate, ok := s.mandate(ticket)
	if !ok {
		return false
	}
	for _, actor := range s.policy.Actors {
		if actor.ID == mandate.Actor.ID {
			return true
		}
	}
	return false
}

// mandate returns the Mandate of the Ticket's current generation: the
// newest reopen Mandate (keyed AssignmentKey(ticket, gen)) at or below the
// current generation, else the request's Mandate (keyed by TicketID).
func (s *Store) mandate(ticket *model.Ticket) (model.Mandate, bool) {
	for gen := ticket.Gen; gen > 1; gen-- {
		if mandate, ok := s.mandates.get(model.AssignmentKey(ticket.ID, gen)); ok {
			return mandate, true
		}
	}
	return s.mandates.get(string(ticket.ID))
}

func (s *Store) runBudget(reservation model.Spend) model.RunBudget {
	budget := s.settings.run
	budget.MaxCostUSD = float64(reservation.MicroUSD) / 1e6
	if reservation.TotalTokens > 0 {
		budget.MaxTotalTokens = int(reservation.TotalTokens)
	}
	return budget
}

func (s *Store) admitted(dispatch *model.DispatchIntent) model.AdmittedDispatch {
	ticket, _ := s.tickets.get(string(dispatch.Ticket))
	mandate, _ := s.mandate(ticket)
	return model.AdmittedDispatch{Dispatch: clone(*dispatch), Ticket: clone(*ticket), Mandate: mandate, Budget: s.runBudget(dispatch.Reservation)}
}

// AdmitNext admits queued dispatches into free slots and reserves both
// ledgers for each, in one commit. Tickets whose Mandate actor is no longer
// listed are held revoked; Tickets that cannot reserve are held for budget.
func (s *Store) AdmitNext(ctx context.Context, now time.Time) ([]model.AdmittedDispatch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.rollback()
	if err := s.check(); err != nil {
		return nil, err
	}
	if s.pause().Paused {
		return nil, nil
	}
	var candidates []candidate
	for _, id := range s.tickets.ids() {
		ticket, _ := s.tickets.get(id)
		if ticket.Lifecycle != model.LifecycleOpen || ticket.Stopped || (ticket.Hold != model.HoldNone && ticket.Hold != model.HoldBudget) {
			continue
		}
		dispatch := s.latestDispatch(ticket)
		if dispatch == nil || dispatch.State != model.DispatchQueued || dispatch.RetryAt.After(now) {
			continue
		}
		if !s.authorized(ticket) {
			mandate, _ := s.mandate(ticket)
			setHold(s.tickets.edit(id), model.HoldRevoked, fmt.Sprintf("mandate actor %s (%d) is no longer listed", mandate.Actor.Login, mandate.Actor.ID))
			continue
		}
		candidates = append(candidates, candidate{Dispatch: dispatch.ID, Class: s.dispatchClass(ticket, dispatch), HasSession: ticket.SessionID != "", QueuedAt: dispatch.QueuedAt})
	}
	classes := make(map[model.DispatchID]model.SlotClass, len(candidates))
	for _, c := range candidates {
		classes[c.Dispatch] = c.Class
	}
	var admittedIDs []string
	placements := schedule(candidates, s.occupancy(), func(c candidate) bool {
		dispatch, _ := s.dispatches.get(string(c.Dispatch))
		ticketLedger, repoLedger := s.peekLedgers(dispatch.Ticket, now)
		amount, reason, ok := reservation(s.settings.runMicro, s.settings.runTokens, s.settings.minRunMicro, ticketLedger, repoLedger)
		ticket := s.tickets.edit(string(dispatch.Ticket))
		if !ok {
			setHold(ticket, model.HoldBudget, reason)
			return false
		}
		releaseHold(ticket, model.HoldBudget)
		reserve(s.ticketLedger(dispatch.Ticket), dispatch.ID, amount)
		reserve(s.repoLedger(now), dispatch.ID, amount)
		edit := s.dispatches.edit(string(dispatch.ID))
		edit.Reservation, edit.AdmittedAt, edit.ThroughInput = amount, now, ticket.InputSeq
		return true
	})
	for _, p := range placements {
		edit := s.dispatches.edit(string(p.Dispatch))
		edit.State, edit.Slot, edit.Class = model.DispatchAdmitted, p.Slot, classes[p.Dispatch]
		admittedIDs = append(admittedIDs, fmt.Sprintf("%s:%d", p.Dispatch, edit.Attempts))
	}
	if _, err := s.commit(ctx, "admit:"+strings.Join(admittedIDs, ",")+s.suffix()); err != nil {
		return nil, err
	}
	out := make([]model.AdmittedDispatch, 0, len(placements))
	for _, p := range placements {
		dispatch, _ := s.dispatches.get(string(p.Dispatch))
		out = append(out, s.admitted(dispatch))
	}
	return out, nil
}

// Active lists dispatches holding a slot, oldest admission first.
func (s *Store) Active(ctx context.Context) ([]model.AdmittedDispatch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(); err != nil {
		return nil, err
	}
	var out []model.AdmittedDispatch
	for _, id := range s.tickets.ids() {
		ticket, _ := s.tickets.get(id)
		if dispatch := s.latestDispatch(ticket); dispatch != nil && live(dispatch.State) {
			out = append(out, s.admitted(dispatch))
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && activeBefore(out[j].Dispatch, out[j-1].Dispatch); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

func activeBefore(a, b model.DispatchIntent) bool {
	if !a.AdmittedAt.Equal(b.AdmittedAt) {
		return a.AdmittedAt.Before(b.AdmittedAt)
	}
	return a.ID < b.ID
}

// Deliveries lists running Tickets whose worker has not been given some
// Inputs: those after both the dispatch's ThroughInput and DeliveredSeq.
func (s *Store) Deliveries(ctx context.Context) ([]model.Delivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(); err != nil {
		return nil, err
	}
	var out []model.Delivery
	for _, id := range s.tickets.ids() {
		ticket, _ := s.tickets.get(id)
		dispatch := s.latestDispatch(ticket)
		if ticket.Stopped || dispatch == nil || dispatch.State != model.DispatchSubmitted || ticket.SessionID == "" {
			continue
		}
		through := max(ticket.DeliveredSeq, dispatch.ThroughInput)
		if ticket.InputSeq <= through {
			continue
		}
		views, err := s.inputViews(ctx, ticket, through, int(ticket.InputSeq-through))
		if err != nil {
			return nil, err
		}
		out = append(out, model.Delivery{Ticket: ticket.ID, Gen: ticket.Gen, SessionID: ticket.SessionID, Inputs: views})
	}
	return out, nil
}

// liveDispatch returns a dispatch for a step call, or an error when it is
// missing or no longer holds a slot.
func (s *Store) liveDispatch(id model.DispatchID, call string) (*model.DispatchIntent, error) {
	dispatch, ok := s.dispatches.get(string(id))
	if !ok {
		return nil, fmt.Errorf("control: dispatch %s: %w", id, model.ErrNotFound)
	}
	if !live(dispatch.State) {
		return nil, errState("dispatch "+string(id), dispatch.State, call)
	}
	return dispatch, nil
}

// Preparing records that the dispatcher started worktree and session work.
func (s *Store) Preparing(ctx context.Context, id model.DispatchID, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.rollback()
	if err := s.check(); err != nil {
		return err
	}
	dispatch, err := s.liveDispatch(id, "start preparing")
	if err != nil {
		return err
	}
	if dispatch.State != model.DispatchAdmitted {
		return nil
	}
	s.dispatches.edit(string(id)).State = model.DispatchPreparing
	_, err = s.commit(ctx, "preparing:"+string(id)+s.suffix())
	return err
}

// Prepared records the Ticket's worktree once it exists and its prepare
// command, if any, succeeded.
func (s *Store) Prepared(ctx context.Context, id model.TicketID, worktree, baseSHA string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.rollback()
	if err := s.check(); err != nil {
		return err
	}
	ticket := s.tickets.edit(string(id))
	if ticket == nil {
		return fmt.Errorf("control: ticket %s: %w", id, model.ErrNotFound)
	}
	if worktree == "" {
		return fmt.Errorf("control: prepared ticket %s without a worktree", id)
	}
	ticket.Worktree, ticket.BaseSHA, ticket.Prepared = worktree, baseSHA, true
	_, err := s.commit(ctx, "prepared:"+string(id)+s.suffix())
	return err
}

// Bind records the Ticket's worker session. A Ticket has one session for
// its whole life and a session serves one Ticket.
func (s *Store) Bind(ctx context.Context, binding model.SessionBinding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.rollback()
	if err := s.check(); err != nil {
		return err
	}
	if binding.SessionID == "" {
		return fmt.Errorf("control: bind ticket %s without a session", binding.Ticket)
	}
	ticket := s.tickets.edit(string(binding.Ticket))
	if ticket == nil {
		return fmt.Errorf("control: ticket %s: %w", binding.Ticket, model.ErrNotFound)
	}
	if ticket.SessionID != "" && ticket.SessionID != binding.SessionID {
		return errState("ticket "+string(binding.Ticket), "bound to session "+ticket.SessionID, "bind "+binding.SessionID)
	}
	if existing, ok := s.sessions.get(binding.SessionID); ok {
		if existing.Ticket != binding.Ticket {
			return errState("session "+binding.SessionID, "bound to ticket "+string(existing.Ticket), "bind "+string(binding.Ticket))
		}
		return nil
	}
	if binding.BoundAt.IsZero() {
		binding.BoundAt = s.clock.Now()
	}
	if binding.Worktree == "" {
		binding.Worktree = ticket.Worktree
	}
	ticket.SessionID = binding.SessionID
	s.sessions.put(binding.SessionID, binding)
	_, err := s.commit(ctx, "bind:"+binding.SessionID)
	return err
}

// Submitted records the worker session's durable receipt for a dispatch.
func (s *Store) Submitted(ctx context.Context, id model.DispatchID, receipt model.SubmissionReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.rollback()
	if err := s.check(); err != nil {
		return err
	}
	dispatch, err := s.liveDispatch(id, "record a submission")
	if err != nil {
		return err
	}
	if receipt.RequestID != "" && receipt.RequestID != dispatch.RequestID {
		return fmt.Errorf("control: dispatch %s expects request %s, got %s", id, dispatch.RequestID, receipt.RequestID)
	}
	ticket := s.tickets.edit(string(dispatch.Ticket))
	if receipt.ThroughInput > ticket.InputSeq {
		return fmt.Errorf("control: ticket %s has %d inputs, dispatch %s cannot consume through %d", ticket.ID, ticket.InputSeq, id, receipt.ThroughInput)
	}
	if dispatch.State == model.DispatchSubmitted {
		if dispatch.SubmissionID != receipt.SubmissionID {
			return errState("dispatch "+string(id), "submitted as "+dispatch.SubmissionID, "record submission "+receipt.SubmissionID)
		}
		// The same submission again is a replay, or a continuation run under
		// the same lease (BeginContinue, or the recovery steer under the same
		// RequestID) that consumed the queued Inputs through a higher seq.
		if receipt.ThroughInput <= dispatch.ThroughInput {
			return nil
		}
		edit := s.dispatches.edit(string(id))
		edit.ThroughInput = receipt.ThroughInput
		if receipt.RunID != "" {
			edit.RunID = receipt.RunID
		}
		ticket.DeliveredSeq = max(ticket.DeliveredSeq, edit.ThroughInput)
		_, err = s.commit(ctx, fmt.Sprintf("submitted:%s:%d", id, edit.ThroughInput))
		return err
	}
	edit := s.dispatches.edit(string(id))
	edit.State, edit.SubmissionID, edit.RunID = model.DispatchSubmitted, receipt.SubmissionID, receipt.RunID
	edit.SubmittedAt = receipt.At
	if edit.SubmittedAt.IsZero() {
		edit.SubmittedAt = s.clock.Now()
	}
	edit.ThroughInput = max(edit.ThroughInput, receipt.ThroughInput)
	ticket.DeliveredSeq = max(ticket.DeliveredSeq, edit.ThroughInput)
	_, err = s.commit(ctx, "submitted:"+string(id))
	return err
}

// Delivered records Inputs through seq accepted by the running worker of
// generation gen. Deliveries to a replaced generation are ignored.
func (s *Store) Delivered(ctx context.Context, id model.TicketID, gen uint32, throughSeq uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.rollback()
	if err := s.check(); err != nil {
		return err
	}
	ticket, ok := s.tickets.get(string(id))
	if !ok {
		return fmt.Errorf("control: ticket %s: %w", id, model.ErrNotFound)
	}
	if gen != ticket.Gen || throughSeq <= ticket.DeliveredSeq {
		return nil
	}
	if throughSeq > ticket.InputSeq {
		return fmt.Errorf("control: ticket %s has %d inputs, cannot deliver through %d", id, ticket.InputSeq, throughSeq)
	}
	s.tickets.edit(string(id)).DeliveredSeq = throughSeq
	_, err := s.commit(ctx, fmt.Sprintf("delivered:%s:%d:%d", id, gen, throughSeq))
	return err
}

// stopKind groups run stop reasons by what Finished does with them.
type stopKind int

const (
	stopCompleted stopKind = iota
	stopBudget
	stopTransient
	stopFailed
	stopAborted
	stopUnknown
)

// classifyStop maps a run outcome to its Finished rule. Errors are split
// by the provider retry taxonomy.
func classifyStop(outcome model.RunOutcome, now time.Time) stopKind {
	reason := outcome.StopReason
	switch {
	case reason == "completed":
		return stopCompleted
	case strings.HasPrefix(reason, "max_"):
		return stopBudget
	case reason == "error":
		if retry.Classify(retry.Failure{Message: outcome.Error}, now).Retryable {
			return stopTransient
		}
		return stopFailed
	case reason == "aborted":
		return stopAborted
	case reason == "unknown":
		return stopUnknown
	}
	return stopFailed
}

// Finished settles a dispatch's runs: ledgers (the whole reservation when
// usage is unknown), slot release, the outcome's hold or follow-up
// dispatch, completion, and the next wake for Inputs that arrived after
// delivery.
func (s *Store) Finished(ctx context.Context, id model.DispatchID, outcome model.RunOutcome, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.rollback()
	if err := s.check(); err != nil {
		return err
	}
	current, ok := s.dispatches.get(string(id))
	if !ok {
		return fmt.Errorf("control: dispatch %s: %w", id, model.ErrNotFound)
	}
	if current.State == model.DispatchFinished {
		return nil
	}
	if !live(current.State) {
		return errState("dispatch "+string(id), current.State, "finish")
	}
	dispatch := s.dispatches.edit(string(id))
	s.settleDispatch(dispatch, &outcome)
	dispatch.State, dispatch.FinishedAt = model.DispatchFinished, now
	recorded := outcome
	dispatch.Outcome = &recorded
	ticket := s.tickets.edit(string(dispatch.Ticket))
	if assignment := s.assignments.edit(model.AssignmentKey(ticket.ID, dispatch.Gen)); assignment != nil {
		assignment.Runs++
	}
	s.work.dispatch = true
	if !ticket.Stopped && ticket.Lifecycle == model.LifecycleOpen {
		s.afterRun(ticket, dispatch, outcome, now)
		s.evaluateEnd(ticket.ID, now)
	}
	_, err := s.commit(ctx, "finished:"+string(id))
	return err
}

// afterRun applies the run-outcome rules of an open Ticket.
func (s *Store) afterRun(ticket *model.Ticket, dispatch *model.DispatchIntent, outcome model.RunOutcome, now time.Time) {
	switch classifyStop(outcome, now) {
	case stopCompleted:
		// The dispatch consumed what it was submitted with (raised by
		// continuation receipts); Inputs only accepted by the session queue
		// after that stay unhandled and wake the next dispatch.
		ticket.HandledSeq = max(ticket.HandledSeq, dispatch.ThroughInput)
		if class, ok := s.pendingWake(ticket); ok {
			s.wake(ticket, class, now)
		}
	case stopBudget:
		ticketLedger, repoLedger := s.peekLedgers(ticket.ID, now)
		if _, reason, ok := reservation(s.settings.runMicro, s.settings.runTokens, s.settings.minRunMicro, ticketLedger, repoLedger); !ok {
			setHold(ticket, model.HoldBudget, reason)
		}
	case stopTransient:
		if dispatch.Attempts >= maxDispatchAttempts {
			setHold(ticket, model.HoldFailed, fmt.Sprintf("run failed %d times in a row: %s", dispatch.Attempts+1, outcome.Error))
			return
		}
		backoff := transientBackoff << dispatch.Attempts
		s.queueDispatch(ticket, model.DispatchWake, now, now.Add(backoff), dispatch.Attempts+1)
	case stopFailed:
		setHold(ticket, model.HoldFailed, failureText(outcome))
	case stopAborted, stopUnknown:
		s.newGeneration(ticket, model.ReasonRecovery, model.DispatchRecovery, now)
	}
}

func failureText(outcome model.RunOutcome) string {
	if outcome.Error == "" {
		return "run stopped: " + outcome.StopReason
	}
	return "run stopped: " + outcome.StopReason + ": " + outcome.Error
}

// pendingWake reports whether waking Inputs remain unhandled and the class
// of the newest one.
func (s *Store) pendingWake(ticket *model.Ticket) (model.InputClass, bool) {
	for seq := ticket.InputSeq; seq > ticket.HandledSeq; seq-- {
		input, ok := s.inputs.get(model.InputKey(ticket.ID, seq))
		if ok && input.Wakes {
			return input.Class, true
		}
	}
	return "", false
}

// Abandon settles a dispatch that never submitted. Its reservation is
// released uncharged. A transient failure requeues it at failure.RetryAt up
// to three times; any other failure, or a fourth, holds the Ticket failed.
func (s *Store) Abandon(ctx context.Context, id model.DispatchID, failure model.DispatchFailure) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.rollback()
	if err := s.check(); err != nil {
		return err
	}
	current, ok := s.dispatches.get(string(id))
	if !ok {
		return fmt.Errorf("control: dispatch %s: %w", id, model.ErrNotFound)
	}
	switch current.State {
	case model.DispatchCancelled:
		return nil
	case model.DispatchQueued:
		// A transient abandon whose commit landed but whose acknowledgment
		// was lost left the dispatch requeued; repeating it is a no-op.
		if current.Attempts > 0 && failure.Transient {
			return nil
		}
	}
	if current.State != model.DispatchAdmitted && current.State != model.DispatchPreparing {
		return errState("dispatch "+string(id), current.State, "abandon")
	}
	now := failure.At
	if now.IsZero() {
		now = s.clock.Now()
	}
	dispatch := s.dispatches.edit(string(id))
	s.settleDispatch(dispatch, nil)
	dispatch.Reservation, dispatch.Slot, dispatch.AdmittedAt = model.Spend{}, "", time.Time{}
	ticket := s.tickets.edit(string(dispatch.Ticket))
	s.work.dispatch = true
	switch {
	case ticket.Stopped || ticket.Lifecycle != model.LifecycleOpen:
		dispatch.State, dispatch.CancelReason, dispatch.FinishedAt = model.DispatchCancelled, failure.Reason, now
	case failure.Transient && dispatch.Attempts < maxDispatchAttempts:
		dispatch.State, dispatch.RetryAt = model.DispatchQueued, failure.RetryAt
		dispatch.Attempts++
	default:
		dispatch.State, dispatch.CancelReason, dispatch.FinishedAt = model.DispatchCancelled, failure.Reason, now
		setHold(ticket, model.HoldFailed, "dispatch failed: "+failure.Reason)
	}
	s.evaluateEnd(ticket.ID, now)
	_, err := s.commit(ctx, fmt.Sprintf("abandon:%s:%d", id, current.Attempts))
	return err
}

// Inputs pages a Ticket's Inputs after afterSeq with their events.
func (s *Store) Inputs(ctx context.Context, id model.TicketID, afterSeq uint64, limit int) ([]model.InputView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(); err != nil {
		return nil, err
	}
	ticket, ok := s.tickets.get(string(id))
	if !ok {
		return nil, fmt.Errorf("control: ticket %s: %w", id, model.ErrNotFound)
	}
	return s.inputViews(ctx, ticket, afterSeq, limit)
}

const (
	defaultPageLimit = 50
	maxPageLimit     = 100
	maxInputLimit    = 1000
)

func (s *Store) inputViews(ctx context.Context, ticket *model.Ticket, afterSeq uint64, limit int) ([]model.InputView, error) {
	if limit <= 0 {
		limit = maxPageLimit
	}
	limit = min(limit, maxInputLimit)
	var views []model.InputView
	for seq := afterSeq + 1; seq <= ticket.InputSeq && len(views) < limit; seq++ {
		input, ok := s.inputs.get(model.InputKey(ticket.ID, seq))
		if !ok {
			return nil, fmt.Errorf("control: input %s missing", model.InputKey(ticket.ID, seq))
		}
		event, err := s.readEvent(ctx, input.Event)
		if err != nil {
			return nil, err
		}
		views = append(views, model.InputView{Input: input, Event: event})
	}
	return views, nil
}
