package control

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
	"go.harness.dev/harness/tempest/durable"
)

// workFlags records what a staged transaction created for the loops.
type workFlags struct {
	dispatch bool
	outbox   bool
}

// Cursor returns a committed repository-wide read position.
func (s *Store) Cursor(ctx context.Context, key model.CursorKey) (model.Cursor, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(); err != nil {
		return model.Cursor{}, false, err
	}
	cursor, ok := s.cursors.get(string(key))
	if !ok {
		return model.Cursor{}, false, nil
	}
	return clone(*cursor), true, nil
}

// Watched lists threads subscribed by an open Ticket, flagged for
// reconciliation, or within their retention window, in number order.
// Gone threads are never read again.
func (s *Store) Watched(ctx context.Context, now time.Time) ([]model.WatchedThread, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(); err != nil {
		return nil, err
	}
	var watched []model.WatchedThread
	for _, key := range s.threads.ids() {
		thread, _ := s.threads.get(key)
		if thread.State == threadGone || !s.watching(thread, now) {
			continue
		}
		entry := model.WatchedThread{Thread: clone(*thread)}
		if pr, ok := s.prs.get(key); ok {
			copied := clone(*pr)
			entry.PR = &copied
		}
		watched = append(watched, entry)
	}
	sortThreads(watched)
	return watched, nil
}

const threadGone = "gone"

func (s *Store) watching(thread *model.Thread, now time.Time) bool {
	if thread.NeedsReconcile || thread.WatchUntil.After(now) {
		return true
	}
	for _, id := range thread.Tickets {
		if ticket, ok := s.tickets.get(string(id)); ok && ticket.Lifecycle == model.LifecycleOpen {
			return true
		}
	}
	return false
}

func sortThreads(threads []model.WatchedThread) {
	for i := 1; i < len(threads); i++ {
		for j := i; j > 0 && threads[j].Thread.Ref.Number < threads[j-1].Thread.Ref.Number; j-- {
			threads[j], threads[j-1] = threads[j-1], threads[j]
		}
	}
}

// Ingest decides and commits one batch. A batch above the event or body
// bounds is split into consecutive commits; positions, cursor and PR facts
// travel with the last part, so a crash between parts rereads the rest and
// known EventIDs are skipped. Completion is decided once, in the last
// part, after every event and snapshot is staged (settleCompletion). A
// batch already committed (same resource and content) commits nothing.
func (s *Store) Ingest(ctx context.Context, batch model.IntakeBatch) (model.IngestResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(); err != nil {
		return model.IngestResult{}, err
	}
	digest, err := durable.Digest(batch)
	if err != nil {
		return model.IngestResult{}, fmt.Errorf("control: digest batch: %w", err)
	}
	parts := splitEvents(batch.Events)
	result := model.IngestResult{Seq: s.seq}
	for index, events := range parts {
		last := index == len(parts)-1
		commitID := fmt.Sprintf("ingest:%s:%s:%d", batch.Resource, digest, index)
		done, err := s.applied(ctx, commitID)
		if err != nil {
			return result, err
		}
		if done {
			continue
		}
		s.work = workFlags{}
		decisions, err := s.stageEvents(events)
		if err != nil {
			s.rollback()
			return result, err
		}
		if last {
			s.stageReadState(batch)
			s.settleCompletion(batch, s.clock.Now())
		}
		receipt, err := s.commit(ctx, commitID)
		if err != nil {
			return result, err
		}
		result.Seq = receipt.Seq
		result.NewEvents += len(decisions)
		result.Decisions = append(result.Decisions, decisions...)
		result.Dispatch = result.Dispatch || s.work.dispatch
		result.Outbox = result.Outbox || s.work.outbox
	}
	return result, nil
}

// splitEvents cuts events into parts within MaxBatchEvents and
// MaxBatchBodyBytes. An empty batch is one empty part.
func splitEvents(events []model.Event) [][]model.Event {
	parts := [][]model.Event{nil}
	bodyBytes := 0
	for _, event := range events {
		current := parts[len(parts)-1]
		if len(current) > 0 && (len(current) == model.MaxBatchEvents || bodyBytes+len(event.Body) > model.MaxBatchBodyBytes) {
			parts = append(parts, nil)
			bodyBytes = 0
		}
		parts[len(parts)-1] = append(parts[len(parts)-1], event)
		bodyBytes += len(event.Body)
	}
	return parts
}

// stageEvents stores each unknown event with its Decision and applies it.
func (s *Store) stageEvents(events []model.Event) ([]model.Decision, error) {
	now := s.clock.Now()
	var decisions []model.Decision
	for _, event := range events {
		if event.ID == "" || s.events.has(string(event.ID)) {
			continue
		}
		event = s.boundBody(event)
		s.events.add(string(event.ID), event)
		s.applyPRFact(event)
		decision, err := s.decide(event, now)
		if err != nil {
			return nil, err
		}
		s.decisions.add(string(event.ID), decision)
		decisions = append(decisions, decision)
	}
	return decisions, nil
}

// boundBody enforces the configured body limit on events the source did
// not already cut.
func (s *Store) boundBody(event model.Event) model.Event {
	limit := s.settings.maxEventBytes
	if len(event.Body) <= limit {
		return event
	}
	if event.BodyBytes == 0 {
		event.BodyBytes = len(event.Body)
	}
	cut := limit
	for cut > 0 && !utf8Start(event.Body[cut]) {
		cut--
	}
	event.Body, event.Truncated = event.Body[:cut], true
	return event
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// decide applies the admission rules to one event and stages their
// effects.
func (s *Store) decide(event model.Event, now time.Time) (model.Decision, error) {
	view := admissionView{Policy: s.policy, Account: s.settings.account, Mode: s.settings.mode, Subscribers: s.subscribers(event)}
	result := admit(event, view)
	decision := model.Decision{Event: event.ID, Kind: result.Kind, Reason: fmt.Sprintf("rule %d: %s", result.Rule, result.Reason), PolicyDigest: s.policy.Digest, DecidedAt: now}
	switch result.Action {
	case actRequest:
		decision.Tickets = []model.TicketID{s.createTicket(event, now)}
	case actReopen:
		s.reopenTicket(result.Tickets[0], event, now)
		decision.Tickets = result.Tickets
	case actInput:
		for _, id := range result.Tickets {
			s.deliverInput(id, event.ID, result.Class, result.Wakes && !result.NoWake[id], now)
		}
		decision.Tickets = result.Tickets
		// PR state facts end Tickets only through settleCompletion, once the
		// whole batch is staged.
	case actStop:
		for _, id := range result.Tickets {
			s.stopTicket(id, "human:"+event.Actor.Login, now)
		}
		decision.Tickets = result.Tickets
	case actStatus:
		if err := s.enqueueStatus(result.Tickets[0], event, now); err != nil {
			return model.Decision{}, err
		}
		decision.Tickets = result.Tickets
	}
	return decision, nil
}

// subscribers lists the Tickets subscribed to an event's thread, oldest
// first, with whether a pr_head event is the Ticket's own push.
func (s *Store) subscribers(event model.Event) []subscriber {
	thread, ok := s.threads.get(strconv.Itoa(event.Thread.Number))
	if !ok || event.Thread.Number == 0 {
		return nil
	}
	subscribers := make([]subscriber, 0, len(thread.Tickets))
	for _, id := range thread.Tickets {
		ticket, ok := s.tickets.get(string(id))
		if !ok {
			continue
		}
		sub := subscriber{ID: id, Lifecycle: ticket.Lifecycle, Stopped: ticket.Stopped, EndedAt: ticket.EndedAt}
		if event.Kind == model.EventPRHead {
			sub.OwnHead = s.pushedSHA(id, event.CommitSHA)
		}
		subscribers = append(subscribers, sub)
	}
	return subscribers
}

// pushedSHA reports whether the Ticket's outbox pushed, or is pushing, sha.
func (s *Store) pushedSHA(id model.TicketID, sha string) bool {
	if sha == "" {
		return false
	}
	for _, intentID := range s.intentsOf(id) {
		intent, _ := s.outbox.get(string(intentID))
		if intent.Kind == model.EffectPush && intent.Payload.SHA == sha && (intent.State == model.EffectPending || intent.State == model.EffectAttempting || intent.State == model.EffectConfirmed) {
			return true
		}
	}
	return false
}

// applyPRFact updates the PR record an event describes. Facts for a PR no
// Ticket follows and no batch described are not stored.
//
// State facts (pr_closed, pr_merged, pr_reopened) are ordered per PR by
// GitHub's time, not arrival, through orderPRState, the rule snapshots
// use too. A pr_head older than UpdatedAt is stale too, but does not move
// UpdatedAt: the source emits it with the snapshot's updated_at, which a
// close in the same read shares. CI is pinned to the head SHA and reviews
// accumulate, so neither needs order.
func (s *Store) applyPRFact(event model.Event) {
	if !event.Thread.IsPR || event.Thread.Number == 0 {
		return
	}
	key := strconv.Itoa(event.Thread.Number)
	pr := s.prs.edit(key)
	if pr == nil {
		thread, ok := s.threads.get(key)
		if !ok || len(thread.Tickets) == 0 {
			return
		}
		pr = s.prs.put(key, model.PullRequest{Number: event.Thread.Number, State: "open"})
	}
	switch event.Kind {
	case model.EventPRHead:
		if event.At.Before(pr.UpdatedAt) {
			return
		}
		pr.HeadSHA = event.CommitSHA
		if pr.CI != nil && pr.CI.SHA != pr.HeadSHA {
			pr.CI = nil
		}
	case model.EventPRCI:
		if event.CI != nil && (pr.HeadSHA == "" || event.CI.SHA == pr.HeadSHA) {
			snapshot := *event.CI
			pr.CI = &snapshot
			pr.HeadSHA = snapshot.SHA
		}
	case model.EventReview:
		known := false
		for _, review := range pr.Reviews {
			known = known || review.ID == event.CommentID
		}
		if !known {
			pr.Reviews = append(pr.Reviews, model.ReviewFact{ID: event.CommentID, Actor: event.Actor, State: event.ReviewState, CommitSHA: event.CommitSHA, SubmittedAt: event.At})
		}
	case model.EventReviewDismissed:
		for i := range pr.Reviews {
			if pr.Reviews[i].ID == event.CommentID {
				pr.Reviews[i].State = "DISMISSED"
			}
		}
	case model.EventPRClosed, model.EventPRMerged, model.EventPRReopened:
		state, merged := "closed", event.Kind == model.EventPRMerged
		if event.Kind == model.EventPRReopened {
			state = "open"
		}
		if orderPRState(*pr, event.At, state, merged) != prSupersedes {
			return
		}
		pr.UpdatedAt = event.At
		switch event.Kind {
		case model.EventPRClosed:
			pr.State, pr.Merged, pr.ClosedAt = state, false, event.At
		case model.EventPRMerged:
			pr.State, pr.Merged, pr.MergeSHA, pr.ClosedAt = state, true, event.CommitSHA, event.At
		case model.EventPRReopened:
			pr.State, pr.Merged, pr.MergeSHA, pr.ClosedAt = state, false, "", time.Time{}
		}
	}
}

// prOrder is how an observed PR state compares with the stored record.
type prOrder int

const (
	// prStale: observed before the record's UpdatedAt.
	prStale prOrder = iota
	// prOutranked: observed in the same GitHub second as the record's
	// UpdatedAt, but the stored state ranks higher.
	prOutranked
	// prSupersedes: observed later, or in the same second at an equal or
	// higher rank.
	prSupersedes
)

// orderPRState is the one ordering rule for a PR's state, shared by state
// events and whole snapshots: state and merged observed at GitHub time at,
// against current. UpdatedAt is the GitHub time of the newest state fact
// or snapshot applied; a placeholder (zero UpdatedAt) knows no GitHub fact,
// so the first one supersedes it.
func orderPRState(current model.PullRequest, at time.Time, state string, merged bool) prOrder {
	switch {
	case current.UpdatedAt.IsZero() || at.After(current.UpdatedAt):
		return prSupersedes
	case at.Before(current.UpdatedAt):
		return prStale
	case prStateRank(state, merged) < prStateRank(current.State, current.Merged):
		return prOutranked
	}
	return prSupersedes
}

// prStateRank orders PR states observed in one GitHub second (GitHub
// reports whole seconds). Merged is final. A reopen in the same second as
// a close must follow it, so open outranks closed: a tie never ends a
// Ticket on a PR that may be open.
func prStateRank(state string, merged bool) int {
	switch {
	case merged:
		return 2
	case state == "open":
		return 1
	}
	return 0
}

// stageReadState moves the cursor and thread positions forward and adopts
// the batch's PR facts.
func (s *Store) stageReadState(batch model.IntakeBatch) {
	if batch.Cursor != nil && batch.Cursor.Key != "" {
		s.applyCursor(*batch.Cursor)
	}
	for _, update := range batch.Threads {
		s.applyThread(update, batch.ReadAt)
	}
	for _, pr := range batch.PRs {
		s.applyPR(pr)
	}
}

// settleCompletion runs the completion rule for every open Ticket
// subscribed to a PR the batch carries a state fact or snapshot for, once
// all of them are staged. Deciding per event or per snapshot would end a
// Ticket on an intermediate state, a close that a later reopen or
// snapshot of the same batch supersedes, and an ended Ticket is never
// repaired by the newer fact. Events already known count too: after a
// crash between the parts of a split batch, the reread settles the facts
// the earlier part committed.
func (s *Store) settleCompletion(batch model.IntakeBatch, now time.Time) {
	var numbers []int
	for _, event := range batch.Events {
		if event.Thread.Number != 0 && (event.Kind == model.EventPRClosed || event.Kind == model.EventPRMerged || event.Kind == model.EventPRReopened) {
			numbers = append(numbers, event.Thread.Number)
		}
	}
	for _, pr := range batch.PRs {
		numbers = append(numbers, pr.Number)
	}
	slices.Sort(numbers)
	for _, number := range slices.Compact(numbers) {
		s.refreshLinks(number, now)
	}
}

// applyCursor never moves a cursor's Since backward.
func (s *Store) applyCursor(next model.Cursor) {
	current := s.cursors.edit(string(next.Key))
	if current == nil {
		s.cursors.put(string(next.Key), next)
		return
	}
	if next.Since.Before(current.Since) {
		return
	}
	enabledAt := current.EnabledAt
	*current = next
	if !enabledAt.IsZero() {
		current.EnabledAt = enabledAt
	}
}

// advancePosition returns the later of two stream positions.
func advancePosition(current model.Position, next *model.Position) model.Position {
	if next == nil {
		return current
	}
	if next.Since.After(current.Since) || next.Since.Equal(current.Since) && next.LastID > current.LastID {
		return *next
	}
	return current
}

func (s *Store) applyThread(update model.ThreadUpdate, readAt time.Time) {
	key := strconv.Itoa(update.Ref.Number)
	thread := s.threads.edit(key)
	if thread == nil {
		thread = s.threads.put(key, model.Thread{Ref: update.Ref})
	}
	if update.Ref.IsPR {
		thread.Ref.IsPR = true
	}
	fresh := update.LastActivityAt.IsZero() || !update.LastActivityAt.Before(thread.LastActivityAt)
	if fresh {
		if update.Title != "" {
			thread.Title = update.Title
			s.titleTickets(thread)
		}
		if update.State != "" {
			thread.State = update.State
		}
		if update.Author != nil {
			thread.Author = *update.Author
		}
		if update.LastActivityAt.After(thread.LastActivityAt) {
			thread.LastActivityAt = update.LastActivityAt
		}
	}
	thread.Comments = advancePosition(thread.Comments, update.Comments)
	thread.ReviewComments = advancePosition(thread.ReviewComments, update.ReviewComments)
	thread.Reviews = advancePosition(thread.Reviews, update.Reviews)
	if update.Reconcile != nil {
		switch {
		case *update.Reconcile && update.NotifiedAt.IsZero():
			thread.NeedsReconcile = true
		case *update.Reconcile:
			// Notifications are relisted for a lookback window; one already
			// recorded must not re-flag a thread that was read since.
			if update.NotifiedAt.After(thread.NotifiedAt) {
				thread.NeedsReconcile, thread.NotifiedAt = true, update.NotifiedAt
			}
		case !thread.NotifiedAt.After(readAt):
			// A notification newer than this read keeps the flag: the read
			// may have started before the thread changed.
			thread.NeedsReconcile = false
		}
	}
}

// titleTickets names subscribed Tickets created before their thread's title
// was read (a request in an earlier part of a split batch).
func (s *Store) titleTickets(thread *model.Thread) {
	for _, id := range thread.Tickets {
		ticket, ok := s.tickets.get(string(id))
		if ok && ticket.Title == "" && ticket.Thread.Number == thread.Ref.Number {
			s.tickets.edit(string(id)).Title = thread.Title
		}
	}
}

// applyPR adopts the source's view of a PR through orderPRState: an older
// snapshot is ignored; one from the same second whose state the stored one
// outranks updates the metadata but keeps the stored state, merge and
// close. Owner is control's and is kept; an open PR on a Ticket's branch,
// from this repository, authored by the account binds to it.
func (s *Store) applyPR(next model.PullRequest) {
	key := strconv.Itoa(next.Number)
	if current, exists := s.prs.get(key); exists {
		switch orderPRState(*current, next.UpdatedAt, next.State, next.Merged) {
		case prStale:
			return
		case prOutranked:
			next.State, next.Merged, next.MergeSHA, next.ClosedAt = current.State, current.Merged, current.MergeSHA, current.ClosedAt
		}
		next.Owner = current.Owner
	} else {
		next.Owner = ""
	}
	if next.CI != nil && next.CI.SHA != next.HeadSHA {
		next.CI = nil
	}
	s.prs.put(key, next)
	s.autoBind(next)
}

// autoBind binds an open PR the account opened from a Ticket branch of this
// repository, covering an open_pr whose outcome was never settled.
func (s *Store) autoBind(pr model.PullRequest) {
	if pr.State != "open" || pr.Owner != "" || pr.Author.ID != s.settings.account || !strings.EqualFold(pr.HeadRepo, s.settings.repo) {
		return
	}
	id, ok := strings.CutPrefix(pr.HeadRef, "agent/")
	if !ok {
		return
	}
	ticket, exists := s.tickets.get(id)
	if !exists || ticket.Lifecycle != model.LifecycleOpen {
		return
	}
	s.bindOwnPR(model.TicketID(id), pr.Number)
}

// refreshLinks syncs PR state into open subscribed Tickets and re-runs
// completion, so a PR fact without an event still ends a Ticket.
func (s *Store) refreshLinks(number int, now time.Time) {
	thread, ok := s.threads.get(strconv.Itoa(number))
	if !ok {
		return
	}
	for _, id := range thread.Tickets {
		ticket, ok := s.tickets.get(string(id))
		if !ok || ticket.Lifecycle != model.LifecycleOpen {
			continue
		}
		s.evaluateEnd(id, now)
	}
}
