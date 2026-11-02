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
func setHold(t *model.Ticket, hold model.Hold, reason string) {
	if t.Hold != model.HoldNone && holdRank[t.Hold] < holdRank[hold] {
		return
	}
	t.Hold, t.HoldReason = hold, reason
}

// releaseHold clears the Ticket's hold if it is one of holds.
func releaseHold(t *model.Ticket, holds ...model.Hold) {
	if slices.Contains(holds, t.Hold) {
		t.Hold, t.HoldReason = model.HoldNone, ""
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
func (s *Store) latestDispatch(t *model.Ticket) *model.DispatchIntent {
	if t.DispatchSeq == 0 {
		return nil
	}
	d, ok := s.dispatches.get(string(model.NewDispatchID(t.ID, t.DispatchSeq)))
	if !ok {
		return nil
	}
	return d
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
	t := s.tickets.put(string(id), model.Ticket{
		ID: id, Repo: s.settings.repo, Thread: event.Thread, Title: title, Branch: model.TicketBranch(id),
		CreatedAt: now, Lifecycle: model.LifecycleOpen, Gen: 1,
	})
	if event.Thread.IsPR {
		t.PRs = []model.PRLink{{Number: event.Thread.Number}}
		s.syncLinks(t)
	}
	s.subscribe(event.Thread, id, title)
	s.assignments.put(model.AssignmentKey(id, 1), model.Assignment{Ticket: id, Gen: 1, Reason: model.ReasonLaunch, State: model.AssignmentOpen, StartedAt: now})
	s.ticketLedger(id)
	s.addInput(t, event.ID, model.InputAuthoritative, true, now)
	s.queueDispatch(t, model.DispatchLaunch, now, time.Time{}, 0)
	return id
}

// addInput appends one Input to a Ticket.
func (s *Store) addInput(t *model.Ticket, event model.EventID, class model.InputClass, wakes bool, now time.Time) {
	t.InputSeq++
	s.inputs.add(model.InputKey(t.ID, t.InputSeq), model.Input{Ticket: t.ID, Seq: t.InputSeq, Event: event, Class: class, Wakes: wakes, At: now})
	if d := s.latestDispatch(t); d != nil && d.State == model.DispatchSubmitted {
		s.work.dispatch = true
	}
}

// deliverInput appends an Input to an open Ticket and wakes it when the
// Input's class may.
func (s *Store) deliverInput(id model.TicketID, event model.EventID, class model.InputClass, wakes bool, now time.Time) {
	t := s.tickets.edit(string(id))
	if t == nil || t.Lifecycle != model.LifecycleOpen {
		return
	}
	s.addInput(t, event, class, wakes, now)
	if class == model.InputAuthoritative {
		releaseHold(t, model.HoldFailed, model.HoldBlocked, model.HoldWakeLimit)
	}
	if wakes {
		s.wake(t, class, now)
	}
}

// wake queues a dispatch for new waking Input unless one is already queued
// or live (it then covers the Input), a hold blocks automatic wakes, or the
// machine auto-wake limit is reached. Revoked and budget holds still queue:
// the dispatch waits until the operator or a new budget period releases
// them.
func (s *Store) wake(t *model.Ticket, class model.InputClass, now time.Time) {
	if t.Lifecycle != model.LifecycleOpen || t.Stopped {
		return
	}
	if d := s.latestDispatch(t); d != nil && pending(d.State) {
		return
	}
	switch t.Hold {
	case model.HoldFailed, model.HoldBlocked, model.HoldWakeLimit:
		return
	}
	if class == model.InputMachine {
		day := now.UTC().Format(time.DateOnly)
		if t.AutoWakeDay != day {
			t.AutoWakeDay, t.AutoWakes = day, 0
		}
		if t.AutoWakes >= s.settings.maxAutoWakes {
			setHold(t, model.HoldWakeLimit, "automatic wakes reached "+strconv.Itoa(s.settings.maxAutoWakes)+" today")
			return
		}
		t.AutoWakes++
	}
	kind := model.DispatchWake
	if t.SessionID == "" {
		kind = model.DispatchLaunch
	}
	s.queueDispatch(t, kind, now, time.Time{}, 0)
}

// queueDispatch appends a queued dispatch of the Ticket's current
// generation.
func (s *Store) queueDispatch(t *model.Ticket, kind model.DispatchKind, now, retryAt time.Time, attempts int) *model.DispatchIntent {
	t.DispatchSeq++
	id := model.NewDispatchID(t.ID, t.DispatchSeq)
	s.work.dispatch = true
	return s.dispatches.put(string(id), model.DispatchIntent{
		ID: id, Ticket: t.ID, Seq: t.DispatchSeq, Gen: t.Gen, Kind: kind, Class: model.SlotNormal,
		State: model.DispatchQueued, ThroughInput: t.InputSeq,
		RequestID: model.LaunchRequestID(t.ID, t.DispatchSeq, t.Gen),
		Attempts:  attempts, RetryAt: retryAt, QueuedAt: now,
	})
}

// cancelQueued cancels the Ticket's queued dispatch, if any.
func (s *Store) cancelQueued(t *model.Ticket, reason string, now time.Time) {
	d := s.latestDispatch(t)
	if d == nil || d.State != model.DispatchQueued {
		return
	}
	edit := s.dispatches.edit(string(d.ID))
	edit.State, edit.CancelReason, edit.FinishedAt = model.DispatchCancelled, reason, now
}

// endAssignment ends the Ticket's current generation.
func (s *Store) endAssignment(t *model.Ticket, reason string, now time.Time) {
	a := s.assignments.edit(model.AssignmentKey(t.ID, t.Gen))
	if a == nil || a.State == model.AssignmentEnded {
		return
	}
	a.State, a.EndedAt, a.EndReason = model.AssignmentEnded, now, reason
}

// newGeneration ends the current Assignment as replaced, starts the next
// one and queues its first dispatch.
func (s *Store) newGeneration(t *model.Ticket, reason model.AssignmentReason, kind model.DispatchKind, now time.Time) {
	s.cancelQueued(t, "replaced by generation "+strconv.Itoa(int(t.Gen+1)), now)
	s.endAssignment(t, "replaced", now)
	t.Gen++
	s.assignments.put(model.AssignmentKey(t.ID, t.Gen), model.Assignment{Ticket: t.ID, Gen: t.Gen, Reason: reason, State: model.AssignmentOpen, StartedAt: now})
	s.queueDispatch(t, kind, now, time.Time{}, 0)
}

// reopenTicket reopens an ended, non-stopped Ticket under its Mandate for
// an authoritative mention.
func (s *Store) reopenTicket(id model.TicketID, event model.EventID, now time.Time) {
	t := s.tickets.edit(string(id))
	if t == nil || t.Lifecycle != model.LifecycleEnded || t.Stopped {
		return
	}
	t.Lifecycle, t.Outcome, t.EndedBy, t.EndedAt = model.LifecycleOpen, "", "", time.Time{}
	t.Hold, t.HoldReason = model.HoldNone, ""
	s.syncLinks(t)
	for _, ref := range s.ownThreads(t) {
		s.subscribe(ref, t.ID, "")
	}
	s.addInput(t, event, model.InputAuthoritative, true, now)
	s.newGeneration(t, model.ReasonReopen, model.DispatchLaunch, now)
}

// stopTicket ends a Ticket as stopped, permanently: queued dispatches and
// pending intents are cancelled; a live dispatch keeps its slot until the
// dispatcher cancels the run and settles it, and attempting intents still
// reconcile.
func (s *Store) stopTicket(id model.TicketID, by string, now time.Time) bool {
	t := s.tickets.edit(string(id))
	if t == nil || t.Stopped {
		return false
	}
	if t.Lifecycle == model.LifecycleOpen {
		t.Lifecycle, t.Outcome, t.EndedBy, t.EndedAt = model.LifecycleEnded, model.OutcomeStopped, by, now
		s.watchUntil(t, now)
	}
	t.Stopped = true
	s.cancelQueued(t, "stopped", now)
	s.endAssignment(t, "stopped", now)
	for _, intentID := range s.intentsOf(t.ID) {
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
func (s *Store) endTicket(t *model.Ticket, outcome model.Outcome, by string, now time.Time) {
	t.Lifecycle, t.Outcome, t.EndedBy, t.EndedAt = model.LifecycleEnded, outcome, by, now
	s.cancelQueued(t, "ticket ended", now)
	s.endAssignment(t, "ticket_ended", now)
	s.watchUntil(t, now)
	s.work.dispatch = true
}

// watchUntil keeps the Ticket's threads polled for the retention period.
func (s *Store) watchUntil(t *model.Ticket, now time.Time) {
	until := now.Add(s.settings.retention)
	for _, ref := range s.ownThreads(t) {
		thread := s.threads.edit(strconv.Itoa(ref.Number))
		if thread != nil && thread.WatchUntil.Before(until) {
			thread.WatchUntil = until
		}
	}
}

// ownThreads is the request thread plus every PR link.
func (s *Store) ownThreads(t *model.Ticket) []model.ThreadRef {
	refs := []model.ThreadRef{t.Thread}
	for _, link := range t.PRs {
		if link.Number != t.Thread.Number {
			refs = append(refs, model.ThreadRef{Number: link.Number, IsPR: true})
		}
	}
	return refs
}

// syncLinks copies closed and merged state from PR records into links.
func (s *Store) syncLinks(t *model.Ticket) {
	for i := range t.PRs {
		pr, ok := s.prs.get(strconv.Itoa(t.PRs[i].Number))
		if !ok {
			continue
		}
		t.PRs[i].Closed = pr.State == "closed" || pr.Merged
		t.PRs[i].Merged = pr.Merged
	}
}

// unfinished reports work that defers every end except stop: a live
// dispatch, or an authoritative Input not yet handled.
func (s *Store) unfinished(t *model.Ticket) bool {
	if d := s.latestDispatch(t); d != nil && live(d.State) {
		return true
	}
	for seq := t.HandledSeq + 1; seq <= t.InputSeq; seq++ {
		input, ok := s.inputs.get(model.InputKey(t.ID, seq))
		if ok && input.Class == model.InputAuthoritative {
			return true
		}
	}
	return false
}

// completion decides whether an open Ticket ends now: the completion set
// (own PRs if any, else every link) all closed ends it merged or closed by
// the latest close; otherwise a current-generation dropped or ready report
// (ready only without PR links). Unfinished work defers every end.
func completion(t model.Ticket, prs map[int]model.PullRequest, unfinished bool) (model.Outcome, bool) {
	if t.Lifecycle != model.LifecycleOpen || unfinished {
		return "", false
	}
	var set []model.PRLink
	for _, link := range t.PRs {
		if link.Own {
			set = append(set, link)
		}
	}
	if len(set) == 0 {
		set = t.PRs
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
	report := t.LastReport
	if report == nil || report.Gen != t.Gen {
		return "", false
	}
	switch {
	case report.Status == model.ReportDropped:
		return model.OutcomeDropped, true
	case report.Status == model.ReportReady && len(t.PRs) == 0:
		return model.OutcomeCompleted, true
	}
	return "", false
}

// evaluateEnd runs the completion rule for a Ticket and ends it if due.
func (s *Store) evaluateEnd(id model.TicketID, now time.Time) {
	t, ok := s.tickets.get(string(id))
	if !ok || t.Lifecycle != model.LifecycleOpen {
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
	outcome, end := completion(*edit, prs, s.unfinished(edit))
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
// at most one open Ticket.
func (s *Store) bindOwnPR(id model.TicketID, number int, now time.Time) bool {
	key := strconv.Itoa(number)
	pr := s.prs.edit(key)
	if pr == nil {
		pr = s.prs.put(key, model.PullRequest{Number: number, HeadRef: model.TicketBranch(id), HeadRepo: s.settings.repo, State: "open", UpdatedAt: now})
	}
	if pr.Owner != "" && pr.Owner != id {
		if other, ok := s.tickets.get(string(pr.Owner)); ok && other.Lifecycle == model.LifecycleOpen {
			return false
		}
	}
	pr.Owner = id
	t := s.tickets.edit(string(id))
	if t == nil {
		return false
	}
	found := false
	for i := range t.PRs {
		if t.PRs[i].Number == number {
			t.PRs[i].Own, found = true, true
		}
	}
	if !found {
		t.PRs = append(t.PRs, model.PRLink{Number: number, Own: true})
	}
	s.syncLinks(t)
	s.subscribe(model.ThreadRef{Number: number, IsPR: true}, id, "")
	return true
}
