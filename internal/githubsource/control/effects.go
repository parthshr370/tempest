package control

import (
	"context"
	"fmt"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
)

// unsettled intents block later writes of the same Ticket.
func unsettled(state model.EffectState) bool {
	return state == model.EffectPending || state == model.EffectAttempting
}

// Due lists, per Ticket, its oldest unsettled intent when it is ready: a
// pending one past RetryAt, or an attempting one left by a restart. A
// paused repository returns none. A stopped Ticket's pending intents were
// cancelled; its attempting ones are still returned so they reconcile.
func (s *Store) Due(ctx context.Context, now time.Time) ([]model.OutboxIntent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(); err != nil {
		return nil, err
	}
	if s.pause().Paused {
		return nil, nil
	}
	var due []model.OutboxIntent
	for _, id := range s.tickets.ids() {
		ticket, _ := s.tickets.get(id)
		for _, intentID := range s.ticketOutbox[ticket.ID] {
			intent, _ := s.outbox.get(string(intentID))
			if !unsettled(intent.State) {
				continue
			}
			if intent.State == model.EffectAttempting || !ticket.Stopped && !intent.RetryAt.After(now) {
				due = append(due, clone(*intent))
			}
			break
		}
	}
	return due, nil
}

// Attempt commits the attempting state before the request is sent. An
// intent already attempting is returned unchanged (its earlier attempt
// must be reconciled, not resent). Past the attempt limit the intent fails
// and an error is returned.
func (s *Store) Attempt(ctx context.Context, id model.OutboxID, now time.Time) (model.OutboxIntent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.rollback()
	if err := s.check(); err != nil {
		return model.OutboxIntent{}, err
	}
	current, ok := s.outbox.get(string(id))
	if !ok {
		return model.OutboxIntent{}, fmt.Errorf("control: outbox intent %s: %w", id, model.ErrNotFound)
	}
	if current.State == model.EffectAttempting {
		return clone(*current), nil
	}
	if current.State != model.EffectPending {
		return clone(*current), errState("outbox intent "+string(id), current.State, "attempt")
	}
	if s.pause().Paused {
		return clone(*current), fmt.Errorf("control: outbox intent %s: %w", id, model.ErrPaused)
	}
	ticket, _ := s.tickets.get(string(current.Ticket))
	intent := s.outbox.edit(string(id))
	switch {
	case ticket != nil && ticket.Stopped:
		intent.State, intent.LastError, intent.SettledAt = model.EffectCancelled, "ticket stopped", now
		if _, err := s.commit(ctx, fmt.Sprintf("attempt:%s:%d", id, current.Attempts+1)); err != nil {
			return model.OutboxIntent{}, err
		}
		return clone(*s.mustIntent(id)), fmt.Errorf("control: outbox intent %s: %w", id, model.ErrStopped)
	case current.Attempts >= maxEffectAttempts:
		intent.State, intent.LastError, intent.SettledAt = model.EffectFailed, fmt.Sprintf("gave up after %d attempts: %s", current.Attempts, current.LastError), now
		if _, err := s.commit(ctx, fmt.Sprintf("attempt:%s:%d", id, current.Attempts+1)); err != nil {
			return model.OutboxIntent{}, err
		}
		return clone(*s.mustIntent(id)), fmt.Errorf("control: outbox intent %s exhausted %d attempts", id, maxEffectAttempts)
	}
	intent.State, intent.AttemptedAt, intent.RetryAt = model.EffectAttempting, now, time.Time{}
	intent.Attempts++
	if _, err := s.commit(ctx, fmt.Sprintf("attempt:%s:%d", id, intent.Attempts)); err != nil {
		return model.OutboxIntent{}, err
	}
	return clone(*s.mustIntent(id)), nil
}

func (s *Store) mustIntent(id model.OutboxID) *model.OutboxIntent {
	intent, ok := s.outbox.get(string(id))
	if !ok {
		panic("control: committed outbox intent " + string(id) + " missing from the projection")
	}
	return intent
}

// Settle records an attempt's outcome. Pending (absence proven, retry
// later) becomes cancelled for a stopped Ticket. A confirmed open_pr binds
// the PR to its Ticket as own in the same commit.
func (s *Store) Settle(ctx context.Context, id model.OutboxID, settlement model.EffectSettlement) (model.OutboxIntent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.rollback()
	if err := s.check(); err != nil {
		return model.OutboxIntent{}, err
	}
	current, ok := s.outbox.get(string(id))
	if !ok {
		return model.OutboxIntent{}, fmt.Errorf("control: outbox intent %s: %w", id, model.ErrNotFound)
	}
	if current.State != model.EffectAttempting {
		if settledAs(current, settlement) {
			return clone(*current), nil
		}
		return clone(*current), errState("outbox intent "+string(id), current.State, "settle")
	}
	switch settlement.State {
	case model.EffectConfirmed, model.EffectFailed, model.EffectUnknown, model.EffectPending, model.EffectCancelled:
	default:
		return clone(*current), fmt.Errorf("control: cannot settle outbox intent %s as %q", id, settlement.State)
	}
	now := settlement.SettledAt
	if now.IsZero() {
		now = s.clock.Now()
	}
	ticket, _ := s.tickets.get(string(current.Ticket))
	intent := s.outbox.edit(string(id))
	intent.State, intent.LastError = settlement.State, settlement.Error
	if settlement.Result != nil {
		result := *settlement.Result
		intent.Result = &result
	}
	switch {
	case settlement.State == model.EffectPending && ticket != nil && ticket.Stopped:
		intent.State, intent.SettledAt = model.EffectCancelled, now
	case settlement.State == model.EffectPending:
		intent.RetryAt = settlement.RetryAt
	default:
		intent.SettledAt = now
	}
	if intent.State == model.EffectConfirmed && intent.Kind == model.EffectOpenPR && intent.Result != nil && intent.Result.RemoteID > 0 {
		s.bindOwnPR(intent.Ticket, int(intent.Result.RemoteID))
	}
	s.work.outbox = true
	if _, err := s.commit(ctx, fmt.Sprintf("settle:%s:%d", id, current.Attempts)); err != nil {
		return model.OutboxIntent{}, err
	}
	return clone(*s.mustIntent(id)), nil
}

// settledAs reports a repeated settlement whose first commit landed.
func settledAs(intent *model.OutboxIntent, settlement model.EffectSettlement) bool {
	switch {
	case intent.State == settlement.State:
		return true
	case settlement.State == model.EffectPending && intent.State == model.EffectCancelled:
		return true
	}
	return false
}

// Ticket returns one Ticket.
func (s *Store) Ticket(ctx context.Context, id model.TicketID) (model.Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(); err != nil {
		return model.Ticket{}, err
	}
	ticket, ok := s.tickets.get(string(id))
	if !ok {
		return model.Ticket{}, fmt.Errorf("control: ticket %s: %w", id, model.ErrNotFound)
	}
	return clone(*ticket), nil
}
