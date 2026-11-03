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

// boundTicket resolves the Ticket of a worker session through its binding;
// tool arguments never name it.
func (s *Store) boundTicket(sessionID string) (*model.Ticket, error) {
	binding, ok := s.sessions.get(sessionID)
	if !ok {
		return nil, fmt.Errorf("control: session %s: %w", sessionID, model.ErrNotBound)
	}
	ticket, ok := s.tickets.get(string(binding.Ticket))
	if !ok {
		return nil, fmt.Errorf("control: ticket %s: %w", binding.Ticket, model.ErrNotFound)
	}
	return ticket, nil
}

// BoundTicket is the Ticket a session is bound to as its worker.
func (s *Store) BoundTicket(sessionID string) (model.TicketID, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	binding, ok := s.sessions.get(sessionID)
	if !ok {
		return "", false
	}
	return binding.Ticket, true
}

// writable rejects writes for stopped and ended Tickets.
func writable(ticket *model.Ticket) error {
	if ticket.Stopped {
		return fmt.Errorf("control: ticket %s: %w", ticket.ID, model.ErrStopped)
	}
	if ticket.Lifecycle != model.LifecycleOpen {
		return fmt.Errorf("control: ticket %s: %w", ticket.ID, model.ErrEnded)
	}
	return nil
}

func validReport(status model.ReportStatus) bool {
	switch status {
	case model.ReportProgress, model.ReportWaiting, model.ReportReady, model.ReportBlocked, model.ReportFailed, model.ReportDropped:
		return true
	}
	return false
}

// Report records the worker's structured report, once per tool call. A
// non-progress report marks the Inputs delivered so far handled; blocked
// and failed hold the Ticket; ready and dropped end it once no work is
// unfinished, which for a running worker is when its dispatch finishes.
func (s *Store) Report(ctx context.Context, call model.WorkerCall, request model.ReportRequest) (model.Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.rollback()
	if err := s.check(); err != nil {
		return model.Ticket{}, err
	}
	current, err := s.boundTicket(call.SessionID)
	if err != nil {
		return model.Ticket{}, err
	}
	commitID := "report:" + string(current.ID) + ":" + call.ToolCallID
	done, err := s.applied(ctx, commitID)
	if err != nil {
		return model.Ticket{}, err
	}
	if done {
		return clone(*current), nil
	}
	if err := writable(current); err != nil {
		return model.Ticket{}, err
	}
	if call.ToolCallID == "" {
		return model.Ticket{}, fmt.Errorf("control: report without a tool call id")
	}
	if !validReport(request.Status) {
		return model.Ticket{}, fmt.Errorf("control: unknown report status %q", request.Status)
	}
	if len(request.Summary) > maxReportTextBytes || request.Question != nil && len(request.Question.Text) > maxReportTextBytes {
		return model.Ticket{}, fmt.Errorf("control: report text exceeds %d bytes", maxReportTextBytes)
	}
	at := call.At
	if at.IsZero() {
		at = s.clock.Now()
	}
	ticket := s.tickets.edit(string(current.ID))
	// A report covers what its dispatch consumed (ThroughInput), not what
	// the session's queue merely accepted (DeliveredSeq): an accepted Input
	// may still be unread when the worker reports.
	through := ticket.HandledSeq
	var during model.DispatchID
	if dispatch := s.latestDispatch(ticket); dispatch != nil && dispatch.State == model.DispatchSubmitted {
		through, during = max(through, dispatch.ThroughInput), dispatch.ID
	}
	ticket.LastReport = &model.Report{Status: request.Status, Summary: request.Summary, Question: request.Question, Gen: ticket.Gen, ThroughInput: through, ToolCallID: call.ToolCallID, At: at}
	s.reports.put(string(ticket.ID), during)
	if request.Status != model.ReportProgress {
		ticket.HandledSeq = through
	}
	switch request.Status {
	case model.ReportBlocked:
		reason := request.Summary
		if request.Question != nil {
			reason = request.Question.Text
		}
		setHold(ticket, model.HoldBlocked, reason)
	case model.ReportFailed:
		setHold(ticket, model.HoldFailed, "worker reported failure: "+request.Summary)
	}
	s.evaluateEnd(ticket.ID, at)
	if _, err := s.commit(ctx, commitID); err != nil {
		return model.Ticket{}, err
	}
	result, _ := s.tickets.get(string(current.ID))
	return clone(*result), nil
}

// intentsOf lists a Ticket's outbox intents in Seq order, staged ones
// included.
func (s *Store) intentsOf(id model.TicketID) []model.OutboxID {
	ids := slices.Clone(s.ticketOutbox[id])
	for key, intent := range s.outbox.dirty {
		if intent.Ticket != id {
			continue
		}
		if _, committed := s.outbox.rows[key]; !committed {
			ids = append(ids, intent.ID)
		}
	}
	slices.SortFunc(ids, func(a, b model.OutboxID) int {
		left, _ := s.outbox.get(string(a))
		right, _ := s.outbox.get(string(b))
		return int(left.Seq) - int(right.Seq)
	})
	return ids
}

// stageIntent stages a new pending outbox intent with the next Seq.
func (s *Store) stageIntent(ticket *model.Ticket, id model.OutboxID, gen uint32, kind model.EffectKind, payload model.EffectPayload, at time.Time) (*model.OutboxIntent, error) {
	digest, err := durable.Digest(payload)
	if err != nil {
		return nil, fmt.Errorf("control: digest payload: %w", err)
	}
	var seq uint64
	if ids := s.intentsOf(ticket.ID); len(ids) > 0 {
		last, _ := s.outbox.get(string(ids[len(ids)-1]))
		seq = last.Seq
	}
	s.work.outbox = true
	return s.outbox.put(string(id), model.OutboxIntent{ID: id, Ticket: ticket.ID, Gen: gen, Kind: kind, State: model.EffectPending, Seq: seq + 1, Payload: payload, PayloadDigest: digest, CreatedAt: at}), nil
}

// ticketThreads is every issue or PR number a Ticket may write to.
func ticketThreads(ticket *model.Ticket) []int {
	numbers := []int{ticket.Thread.Number}
	for _, link := range ticket.PRs {
		if link.Number != ticket.Thread.Number {
			numbers = append(numbers, link.Number)
		}
	}
	return numbers
}

func invalidTarget(format string, args ...any) error {
	return fmt.Errorf("control: %s: %w", fmt.Sprintf(format, args...), model.ErrInvalidTarget)
}

// resolveEffect fills a worker's requested write from the Ticket and
// rejects targets outside it.
func (s *Store) resolveEffect(ticket *model.Ticket, request model.EffectRequest) (model.EffectPayload, error) {
	payload := request.Payload
	if len(payload.Body) > maxEffectBodyBytes {
		return payload, fmt.Errorf("control: body exceeds %d bytes", maxEffectBodyBytes)
	}
	switch request.Kind {
	case model.EffectComment:
		if payload.Number == 0 {
			payload.Number = ticket.Thread.Number
		}
		if !slices.Contains(ticketThreads(ticket), payload.Number) {
			return payload, invalidTarget("#%d is not a thread of ticket %s", payload.Number, ticket.ID)
		}
		if strings.TrimSpace(payload.Body) == "" {
			return payload, fmt.Errorf("control: comment body is empty")
		}
		payload.ReplyTo, payload.Branch, payload.SHA, payload.LeaseSHA, payload.Base, payload.Title, payload.Draft = 0, "", "", "", "", "", nil
	case model.EffectReviewReply:
		if payload.ReplyTo == 0 {
			return payload, fmt.Errorf("control: review reply needs the comment it answers")
		}
		number, known := s.reviewComments[payload.ReplyTo]
		if !known {
			return payload, invalidTarget("review comment %d is not known on a thread of ticket %s", payload.ReplyTo, ticket.ID)
		}
		if payload.Number == 0 {
			payload.Number = number
		}
		if payload.Number != number || !slices.Contains(ticketThreads(ticket), number) {
			return payload, invalidTarget("review comment %d is on #%d, outside ticket %s", payload.ReplyTo, number, ticket.ID)
		}
		if strings.TrimSpace(payload.Body) == "" {
			return payload, fmt.Errorf("control: reply body is empty")
		}
		payload.Branch, payload.SHA, payload.LeaseSHA, payload.Base, payload.Title, payload.Draft = "", "", "", "", "", nil
	case model.EffectPush:
		if err := s.ownBranch(ticket, &payload); err != nil {
			return payload, err
		}
		if payload.SHA == "" {
			return payload, fmt.Errorf("control: push needs a commit sha")
		}
		payload.LeaseSHA = s.leaseSHA(ticket.ID)
		payload.Number, payload.ReplyTo, payload.Body, payload.Title, payload.Draft, payload.Base = 0, 0, "", "", nil, ""
	case model.EffectOpenPR:
		if err := s.ownBranch(ticket, &payload); err != nil {
			return payload, err
		}
		if payload.Base == "" {
			payload.Base = s.settings.base
		}
		if payload.Base != s.settings.base {
			return payload, invalidTarget("base %q is not the configured base %q", payload.Base, s.settings.base)
		}
		if strings.TrimSpace(payload.Title) == "" {
			return payload, fmt.Errorf("control: pull request title is empty")
		}
		payload.Number, payload.ReplyTo, payload.LeaseSHA = 0, 0, ""
	case model.EffectUpdatePR:
		own := false
		for _, link := range ticket.PRs {
			own = own || link.Own && link.Number == payload.Number
		}
		if !own {
			return payload, invalidTarget("#%d is not a pull request ticket %s opened", payload.Number, ticket.ID)
		}
		payload.ReplyTo, payload.Branch, payload.SHA, payload.LeaseSHA, payload.Base = 0, "", "", "", ""
	default:
		return payload, fmt.Errorf("control: unknown effect kind %q", request.Kind)
	}
	return payload, nil
}

// ownBranch accepts only the Ticket's branch.
func (s *Store) ownBranch(ticket *model.Ticket, payload *model.EffectPayload) error {
	if payload.Branch == "" {
		payload.Branch = ticket.Branch
	}
	if payload.Branch != ticket.Branch {
		return invalidTarget("branch %q is not ticket %s's branch %q", payload.Branch, ticket.ID, ticket.Branch)
	}
	return nil
}

// leaseSHA is the remote SHA the next push replaces: the newest earlier
// push that is pending, in flight or confirmed. Pushes publish in order, so
// that is the ref the remote holds when this one runs.
func (s *Store) leaseSHA(id model.TicketID) string {
	ids := s.intentsOf(id)
	for i := len(ids) - 1; i >= 0; i-- {
		intent, _ := s.outbox.get(string(ids[i]))
		if intent.Kind != model.EffectPush {
			continue
		}
		switch intent.State {
		case model.EffectPending, model.EffectAttempting, model.EffectConfirmed:
			return intent.Payload.SHA
		}
	}
	return ""
}

// Enqueue commits a worker's requested write as a pending outbox intent,
// once per tool call.
func (s *Store) Enqueue(ctx context.Context, call model.WorkerCall, request model.EffectRequest) (model.OutboxIntent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.rollback()
	if err := s.check(); err != nil {
		return model.OutboxIntent{}, err
	}
	ticket, err := s.boundTicket(call.SessionID)
	if err != nil {
		return model.OutboxIntent{}, err
	}
	if call.ToolCallID == "" {
		return model.OutboxIntent{}, fmt.Errorf("control: enqueue without a tool call id")
	}
	id := model.NewOutboxID(ticket.ID, ticket.Gen, call.ToolCallID)
	if intent, ok := s.outbox.get(string(id)); ok {
		return clone(*intent), nil
	}
	if err := writable(ticket); err != nil {
		return model.OutboxIntent{}, err
	}
	payload, err := s.resolveEffect(ticket, request)
	if err != nil {
		return model.OutboxIntent{}, err
	}
	at := call.At
	if at.IsZero() {
		at = s.clock.Now()
	}
	if _, err := s.stageIntent(ticket, id, ticket.Gen, request.Kind, payload, at); err != nil {
		return model.OutboxIntent{}, err
	}
	if _, err := s.commit(ctx, "enqueue:"+string(id)); err != nil {
		return model.OutboxIntent{}, err
	}
	intent, _ := s.outbox.get(string(id))
	return clone(*intent), nil
}

// Effect reads an outbox intent of the caller's Ticket.
func (s *Store) Effect(ctx context.Context, sessionID string, id model.OutboxID) (model.OutboxIntent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(); err != nil {
		return model.OutboxIntent{}, err
	}
	ticket, err := s.boundTicket(sessionID)
	if err != nil {
		return model.OutboxIntent{}, err
	}
	intent, ok := s.outbox.get(string(id))
	if !ok || intent.Ticket != ticket.ID {
		return model.OutboxIntent{}, fmt.Errorf("control: outbox intent %s: %w", id, model.ErrNotFound)
	}
	return clone(*intent), nil
}

// Status is the caller Ticket's detail view.
func (s *Store) Status(ctx context.Context, sessionID string) (model.TicketDetail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(); err != nil {
		return model.TicketDetail{}, err
	}
	ticket, err := s.boundTicket(sessionID)
	if err != nil {
		return model.TicketDetail{}, err
	}
	return s.detail(ctx, ticket)
}

// Thread pages stored events of the caller Ticket's thread or one of its
// PRs, oldest first. The cursor is the count of events already read.
func (s *Store) Thread(ctx context.Context, sessionID string, query model.ThreadQuery) (model.EventPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(); err != nil {
		return model.EventPage{}, err
	}
	ticket, err := s.boundTicket(sessionID)
	if err != nil {
		return model.EventPage{}, err
	}
	number := query.Number
	if number == 0 {
		number = ticket.Thread.Number
	}
	if !slices.Contains(ticketThreads(ticket), number) {
		return model.EventPage{}, invalidTarget("#%d is not a thread of ticket %s", number, ticket.ID)
	}
	offset, err := parseCursor(query.Cursor)
	if err != nil {
		return model.EventPage{}, err
	}
	ids := s.threadEvents[number]
	limit := pageLimit(query.Limit)
	page := model.EventPage{Events: []model.EventView{}}
	for index := offset; index < len(ids) && len(page.Events) < limit; index++ {
		view, err := s.eventView(ctx, ids[index])
		if err != nil {
			return model.EventPage{}, err
		}
		page.Events = append(page.Events, view)
	}
	if next := offset + len(page.Events); next < len(ids) {
		page.NextCursor = strconv.Itoa(next)
	}
	return page, nil
}

func (s *Store) eventView(ctx context.Context, id model.EventID) (model.EventView, error) {
	event, err := s.readEvent(ctx, id)
	if err != nil {
		return model.EventView{}, err
	}
	view := model.EventView{Event: event}
	if decision, ok := s.decisions.get(string(id)); ok {
		view.Decision = &decision
	}
	return view, nil
}

func parseCursor(cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	offset, err := strconv.Atoi(cursor)
	if err != nil || offset < 0 {
		return 0, fmt.Errorf("control: invalid cursor %q", cursor)
	}
	return offset, nil
}

func pageLimit(limit int) int {
	if limit <= 0 {
		return defaultPageLimit
	}
	return min(limit, maxPageLimit)
}

// enqueueStatus stages the controller-authored status reply (generation 0)
// for an "@account status" command.
func (s *Store) enqueueStatus(id model.TicketID, event model.Event, now time.Time) error {
	ticket, ok := s.tickets.get(string(id))
	if !ok {
		return fmt.Errorf("control: ticket %s: %w", id, model.ErrNotFound)
	}
	payload := model.EffectPayload{Number: event.Thread.Number, Body: s.statusText(ticket, now)}
	_, err := s.stageIntent(ticket, model.NewOutboxID(id, 0, string(event.ID)), 0, model.EffectComment, payload, now)
	return err
}

// statusText renders the deterministic status reply.
func (s *Store) statusText(ticket *model.Ticket, now time.Time) string {
	var text strings.Builder
	fmt.Fprintf(&text, "Ticket `%s`: %s", ticket.ID, s.stageOf(ticket))
	if ticket.Lifecycle == model.LifecycleEnded {
		fmt.Fprintf(&text, " (%s)", ticket.Outcome)
	}
	text.WriteString(".\n")
	if ticket.Hold != model.HoldNone {
		fmt.Fprintf(&text, "Held: %s.\n", ticket.Hold)
	}
	for _, link := range ticket.PRs {
		state := "open"
		switch {
		case link.Merged:
			state = "merged"
		case link.Closed:
			state = "closed"
		}
		relation := "followed"
		if link.Own {
			relation = "opened by this ticket"
		}
		fmt.Fprintf(&text, "PR #%d: %s, %s.\n", link.Number, state, relation)
	}
	ledger, _ := s.peekLedgers(ticket.ID, now)
	fmt.Fprintf(&text, "Budget: %s spent of %s.\n", usd(ledger.Spent.MicroUSD), usd(sum(ledger.Allowance.MicroUSD, ledger.Granted.MicroUSD)))
	return text.String()
}
