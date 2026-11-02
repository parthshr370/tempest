package control

import (
	"errors"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/githubsource/model"
)

// unknownIntent enqueues a comment and drives it through attempts refused
// before writing to an unknown last attempt, as the outbox would.
func (h *harness) unknownIntent(id model.TicketID, call string, refused int) model.OutboxIntent {
	h.t.Helper()
	intent, err := h.store.Enqueue(h.ctx, model.WorkerCall{SessionID: sessionOf(id), ToolCallID: call, At: h.clock.Now()}, model.EffectRequest{Kind: model.EffectComment, Payload: model.EffectPayload{Body: "hello " + call}})
	if err != nil {
		h.t.Fatal(err)
	}
	for range refused {
		if _, err := h.store.Attempt(h.ctx, intent.ID, h.clock.Now()); err != nil {
			h.t.Fatal(err)
		}
		if _, err := h.store.Settle(h.ctx, intent.ID, model.EffectSettlement{State: model.EffectPending, Error: "429"}); err != nil {
			h.t.Fatal(err)
		}
	}
	if _, err := h.store.Attempt(h.ctx, intent.ID, h.clock.Now()); err != nil {
		h.t.Fatal(err)
	}
	unknown, err := h.store.Settle(h.ctx, intent.ID, model.EffectSettlement{State: model.EffectUnknown, Error: "not found"})
	if err != nil || unknown.State != model.EffectUnknown {
		h.t.Fatalf("unknown: %+v %v", unknown, err)
	}
	return unknown
}

func (h *harness) effect(id model.TicketID, intent model.OutboxID) model.OutboxIntent {
	h.t.Helper()
	effect, err := h.store.Effect(h.ctx, sessionOf(id), intent)
	if err != nil {
		h.t.Fatal(err)
	}
	return effect
}

// An operator resend moves an unknown intent back to pending with an audit
// record; it publishes before the Ticket's later writes and gets a fresh
// attempt limit even after the old one was spent.
func TestResendUnknownEffect(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	id, _ := h.launched(440, 1)
	first := h.unknownIntent(id, "c1", maxEffectAttempts-1)
	if first.Attempts != maxEffectAttempts {
		t.Fatalf("attempts %d", first.Attempts)
	}
	later, _ := h.store.Enqueue(h.ctx, model.WorkerCall{SessionID: sessionOf(id), ToolCallID: "c2"}, model.EffectRequest{Kind: model.EffectComment, Payload: model.EffectPayload{Body: "later"}})
	operator := h.store.Operator()
	request := model.OperatorRequest{ID: "resend-1", Target: string(first.ID), Reason: "checked: not on GitHub"}
	if err := operator.ResolveEffect(h.ctx, request, model.EffectPending); err != nil {
		t.Fatal(err)
	}
	got := h.effect(id, first.ID)
	if got.State != model.EffectPending || !got.SettledAt.IsZero() || !got.RetryAt.IsZero() || !strings.Contains(got.LastError, "checked: not on GitHub") {
		t.Fatalf("after resend: %+v", got)
	}
	if status, _ := operator.Status(h.ctx); status.UnknownEffects != 0 {
		t.Fatalf("unknown effects %d", status.UnknownEffects)
	}
	h.store.mu.Lock()
	audit, ok := h.store.ops.get("resend-1")
	h.store.mu.Unlock()
	if !ok || audit.Method != methodResend || audit.Target != string(first.ID) || audit.Reason != request.Reason {
		t.Fatalf("audit: %+v %v", audit, ok)
	}
	// The same request ID is idempotent; a second resend of a pending
	// intent is refused.
	seq := h.status().Seq
	if err := operator.ResolveEffect(h.ctx, request, model.EffectPending); err != nil {
		t.Fatalf("repeated request: %v", err)
	}
	if h.status().Seq != seq {
		t.Fatal("repeated request committed")
	}
	if err := operator.ResolveEffect(h.ctx, model.OperatorRequest{ID: "resend-2", Target: string(first.ID)}, model.EffectPending); err == nil {
		t.Fatal("resent a pending intent")
	}
	// It is the Ticket's oldest unsettled write: due before the later one.
	due, _ := h.store.Due(h.ctx, h.clock.Now())
	if len(due) != 1 || due[0].ID != first.ID {
		t.Fatalf("due: %+v", due)
	}
	// A restart keeps the resend and the fresh limit.
	h.reopen()
	for n := range maxEffectAttempts {
		attempted, err := h.store.Attempt(h.ctx, first.ID, h.clock.Now())
		if err != nil || attempted.State != model.EffectAttempting || attempted.Attempts != maxEffectAttempts+n+1 {
			t.Fatalf("attempt %d after resend: %+v %v", n+1, attempted, err)
		}
		h.store.Settle(h.ctx, first.ID, model.EffectSettlement{State: model.EffectPending, Error: "429"})
	}
	exhausted, err := h.store.Attempt(h.ctx, first.ID, h.clock.Now())
	if err == nil || exhausted.State != model.EffectFailed || !strings.Contains(exhausted.LastError, "gave up after 5 attempts") {
		t.Fatalf("attempt past the fresh limit: %+v %v", exhausted, err)
	}
	if due, _ := h.store.Due(h.ctx, h.clock.Now()); len(due) != 1 || due[0].ID != later.ID {
		t.Fatalf("due after the resent write settled: %+v", due)
	}
}

func TestResendRefusals(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	id, _ := h.launched(441, 1)
	operator := h.store.Operator()
	if err := operator.ResolveEffect(h.ctx, model.OperatorRequest{ID: "r0", Target: "missing"}, model.EffectPending); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("missing intent: %v", err)
	}
	unknown := h.unknownIntent(id, "c1", 0)
	confirmed := h.unknownIntent(id, "c2", 0)
	if err := operator.ResolveEffect(h.ctx, model.OperatorRequest{ID: "r1", Target: string(confirmed.ID)}, model.EffectConfirmed); err != nil {
		t.Fatal(err)
	}
	if err := operator.ResolveEffect(h.ctx, model.OperatorRequest{ID: "r2", Target: string(confirmed.ID)}, model.EffectPending); err == nil {
		t.Fatal("resent a confirmed intent")
	}
	if err := operator.ResolveEffect(h.ctx, model.OperatorRequest{ID: "r3"}, model.EffectPending); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("no target: %v", err)
	}
	if err := operator.Stop(h.ctx, model.OperatorRequest{ID: "stop", Target: string(id)}); err != nil {
		t.Fatal(err)
	}
	if err := operator.ResolveEffect(h.ctx, model.OperatorRequest{ID: "r4", Target: string(unknown.ID)}, model.EffectPending); !errors.Is(err, model.ErrStopped) {
		t.Fatalf("resend for a stopped ticket: %v", err)
	}
	if got := h.effect(id, unknown.ID); got.State != model.EffectUnknown {
		t.Fatalf("refused resend changed the intent: %+v", got)
	}
	// Failing it is still allowed after the stop.
	if err := operator.ResolveEffect(h.ctx, model.OperatorRequest{ID: "r5", Target: string(unknown.ID), Reason: "gave up"}, model.EffectFailed); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) status() model.StatusView {
	h.t.Helper()
	status, err := h.store.Operator().Status(h.ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	return status
}
