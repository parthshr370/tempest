package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
)

// Operator is the model.Control view of a Store. It is a separate type
// because model.Control's Status and Ticket differ in signature from
// model.WorkerGateway.Status and model.EffectStore.Ticket.
type Operator struct{ store *Store }

var _ model.Control = (*Operator)(nil)

// Operator returns the operator surface ACP methods call.
func (s *Store) Operator() *Operator { return &Operator{store: s} }

// Operator command methods recorded in gh/op.
const (
	methodStop    = "stop"
	methodRetry   = "retry"
	methodPause   = "pause"
	methodUnpause = "unpause"
	methodGrant   = "grant"
	methodResolve = "resolve_effect"
)

// Status summarizes the repository from the control journal.
func (o *Operator) Status(ctx context.Context) (model.StatusView, error) {
	s := o.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(); err != nil {
		return model.StatusView{}, err
	}
	view := model.StatusView{Repo: s.settings.repo, Account: s.repo.Account, Pause: s.pause(), Occupancy: s.occupancy(), Seq: s.seq}
	for _, id := range s.tickets.ids() {
		ticket, _ := s.tickets.get(id)
		if ticket.Lifecycle == model.LifecycleOpen {
			view.OpenTickets++
			if attention(s.stageOf(ticket)) {
				view.Attention++
			}
			if dispatch := s.latestDispatch(ticket); dispatch != nil && dispatch.State == model.DispatchQueued {
				view.Queued++
			}
		}
	}
	for _, id := range s.outbox.ids() {
		if intent, _ := s.outbox.get(id); intent.State == model.EffectUnknown {
			view.UnknownEffects++
		}
	}
	_, repoLedger := s.peekLedgers("", s.clock.Now())
	view.RepoLedger = clone(repoLedger)
	return view, nil
}

// Tickets lists Tickets newest first. The attention filter lists open
// Tickets with person/* stages first; open and ended filter by lifecycle.
func (o *Operator) Tickets(ctx context.Context, query model.TicketQuery) (model.TicketPage, error) {
	s := o.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(); err != nil {
		return model.TicketPage{}, err
	}
	var rows []model.TicketSummary
	var created []time.Time
	for _, id := range s.tickets.ids() {
		ticket, _ := s.tickets.get(id)
		switch query.Filter {
		case model.FilterOpen, model.FilterAttention:
			if ticket.Lifecycle != model.LifecycleOpen {
				continue
			}
		case model.FilterEnded:
			if ticket.Lifecycle != model.LifecycleEnded {
				continue
			}
		case model.FilterAll, "":
		default:
			return model.TicketPage{}, fmt.Errorf("control: unknown ticket filter %q", query.Filter)
		}
		rows = append(rows, s.summary(ticket))
		created = append(created, ticket.CreatedAt)
	}
	order := make([]int, len(rows))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		if query.Filter == model.FilterAttention {
			if diff := stageRank[rows[a].Stage] - stageRank[rows[b].Stage]; diff != 0 {
				return diff
			}
		}
		if !created[a].Equal(created[b]) {
			if created[a].After(created[b]) {
				return -1
			}
			return 1
		}
		if rows[a].ID < rows[b].ID {
			return -1
		}
		return 1
	})
	offset, err := parseCursor(query.Cursor)
	if err != nil {
		return model.TicketPage{}, err
	}
	limit := pageLimit(query.Limit)
	page := model.TicketPage{Tickets: []model.TicketSummary{}}
	for index := offset; index < len(order) && len(page.Tickets) < limit; index++ {
		page.Tickets = append(page.Tickets, rows[order[index]])
	}
	if next := offset + len(page.Tickets); next < len(order) {
		page.NextCursor = strconv.Itoa(next)
	}
	return page, nil
}

// Ticket returns one Ticket's detail.
func (o *Operator) Ticket(ctx context.Context, id model.TicketID) (model.TicketDetail, error) {
	s := o.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(); err != nil {
		return model.TicketDetail{}, err
	}
	ticket, ok := s.tickets.get(string(id))
	if !ok {
		return model.TicketDetail{}, fmt.Errorf("control: ticket %s: %w", id, model.ErrNotFound)
	}
	return s.detail(ctx, ticket)
}

// Events pages stored events with their decisions, newest first: of one
// thread, of one Ticket (its Inputs), or all.
func (o *Operator) Events(ctx context.Context, query model.EventQuery) (model.EventPage, error) {
	s := o.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(); err != nil {
		return model.EventPage{}, err
	}
	var ids []model.EventID
	switch {
	case query.Ticket != "":
		ticket, ok := s.tickets.get(string(query.Ticket))
		if !ok {
			return model.EventPage{}, fmt.Errorf("control: ticket %s: %w", query.Ticket, model.ErrNotFound)
		}
		for seq := uint64(1); seq <= ticket.InputSeq; seq++ {
			if input, ok := s.inputs.get(model.InputKey(ticket.ID, seq)); ok {
				ids = append(ids, input.Event)
			}
		}
	case query.Thread != 0:
		ids = s.threadEvents[query.Thread]
	default:
		for _, id := range s.events.all() {
			ids = append(ids, model.EventID(id))
		}
	}
	offset, err := parseCursor(query.Cursor)
	if err != nil {
		return model.EventPage{}, err
	}
	limit := pageLimit(query.Limit)
	page := model.EventPage{Events: []model.EventView{}}
	for index := offset; index < len(ids) && len(page.Events) < limit; index++ {
		view, err := s.eventView(ctx, ids[len(ids)-1-index])
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

// operate runs one audited operator command: a repeated request ID returns
// without acting again; otherwise apply stages the change and the command
// record commits with it.
func (s *Store) operate(ctx context.Context, request model.OperatorRequest, method string, grant *model.Spend, apply func(now time.Time) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.rollback()
	if err := s.check(); err != nil {
		return err
	}
	if request.ID == "" {
		return errors.New("control: operator request id is required")
	}
	if s.ops.has(request.ID) {
		return nil
	}
	now := request.At
	if now.IsZero() {
		now = s.clock.Now()
	}
	if err := apply(now); err != nil {
		return err
	}
	s.ops.add(request.ID, model.OperatorCommand{ID: request.ID, Method: method, Target: request.Target, Reason: request.Reason, Grant: grant, At: now})
	_, err := s.commit(ctx, "op:"+request.ID)
	return err
}

// Stop ends the target Ticket as stopped, permanently.
func (o *Operator) Stop(ctx context.Context, request model.OperatorRequest) error {
	s := o.store
	return s.operate(ctx, request, methodStop, nil, func(now time.Time) error {
		if _, ok := s.tickets.get(request.Target); !ok {
			return fmt.Errorf("control: ticket %s: %w", request.Target, model.ErrNotFound)
		}
		s.stopTicket(model.TicketID(request.Target), "operator", now)
		return nil
	})
}

// Retry releases any hold of an open Ticket and starts a new generation.
// It also re-authorizes a Ticket whose Mandate actor was revoked. A Ticket
// whose worker is still running cannot be retried until it settles.
func (o *Operator) Retry(ctx context.Context, request model.OperatorRequest) error {
	s := o.store
	return s.operate(ctx, request, methodRetry, nil, func(now time.Time) error {
		ticket := s.tickets.edit(request.Target)
		if ticket == nil {
			return fmt.Errorf("control: ticket %s: %w", request.Target, model.ErrNotFound)
		}
		if ticket.Stopped {
			return fmt.Errorf("control: ticket %s: %w", ticket.ID, model.ErrStopped)
		}
		if ticket.Lifecycle != model.LifecycleOpen {
			return fmt.Errorf("control: ticket %s: %w", ticket.ID, model.ErrEnded)
		}
		if dispatch := s.latestDispatch(ticket); dispatch != nil && live(dispatch.State) {
			return errState("ticket "+string(ticket.ID), "running dispatch "+string(dispatch.ID), "retry")
		}
		ticket.Hold, ticket.HoldReason = model.HoldNone, ""
		s.newGeneration(ticket, model.ReasonRetry, model.DispatchLaunch, now)
		return nil
	})
}

// SetPaused holds or releases dispatch admission and outbox publishing.
func (o *Operator) SetPaused(ctx context.Context, request model.OperatorRequest, paused bool) error {
	s := o.store
	method := methodUnpause
	if paused {
		method = methodPause
	}
	return s.operate(ctx, request, method, nil, func(now time.Time) error {
		raw, err := jsonValue(model.Pause{Paused: paused, Reason: request.Reason, At: now})
		if err != nil {
			return err
		}
		s.meta.put(metaPause, raw)
		s.work.dispatch, s.work.outbox = true, true
		return nil
	})
}

// Grant adds allowance to the target Ticket's ledger, or to the current
// repository period's ledger when Target is empty. Budget holds are
// re-checked at the next admission.
func (o *Operator) Grant(ctx context.Context, request model.OperatorRequest, amount model.Spend) error {
	s := o.store
	if amount.MicroUSD < 0 || amount.TotalTokens < 0 || amount.MicroUSD == 0 && amount.TotalTokens == 0 {
		return fmt.Errorf("control: grant must add a positive amount")
	}
	grant := model.Spend{MicroUSD: amount.MicroUSD, TotalTokens: amount.TotalTokens}
	return s.operate(ctx, request, methodGrant, &grant, func(now time.Time) error {
		var ledger *model.Ledger
		if request.Target == "" {
			ledger = s.repoLedger(now)
		} else {
			if _, ok := s.tickets.get(request.Target); !ok {
				return fmt.Errorf("control: ticket %s: %w", request.Target, model.ErrNotFound)
			}
			ledger = s.ticketLedger(model.TicketID(request.Target))
		}
		ledger.Granted = addSpend(ledger.Granted, grant)
		s.work.dispatch = true
		return nil
	})
}

// ResolveEffect settles an unknown intent as confirmed or failed after the
// operator checked GitHub.
func (o *Operator) ResolveEffect(ctx context.Context, request model.OperatorRequest, state model.EffectState) error {
	s := o.store
	if state != model.EffectConfirmed && state != model.EffectFailed {
		return fmt.Errorf("control: an unknown effect resolves only to confirmed or failed, not %q", state)
	}
	return s.operate(ctx, request, methodResolve, nil, func(now time.Time) error {
		current, ok := s.outbox.get(request.Target)
		if !ok {
			return fmt.Errorf("control: outbox intent %s: %w", request.Target, model.ErrNotFound)
		}
		if current.State != model.EffectUnknown {
			return errState("outbox intent "+request.Target, current.State, "resolve")
		}
		intent := s.outbox.edit(request.Target)
		intent.State, intent.SettledAt = state, now
		intent.LastError = "resolved by operator: " + request.Reason
		s.work.outbox = true
		return nil
	})
}

func jsonValue(value any) (json.RawMessage, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("control: encode %T: %w", value, err)
	}
	return raw, nil
}
