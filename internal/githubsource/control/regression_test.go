package control

import (
	"math"
	"testing"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
)

// The tests in this file reproduce defects found by the G2 correctness
// review (context/live/verify/g2-check.md); each failed before its fix.

// A ready report completes a Ticket only through the dispatch it was made
// in, and only when that dispatch finished completed.
func TestReadyReportNeedsSuccessfulDispatch(t *testing.T) {
	t.Run("terminal error after ready holds failed", func(t *testing.T) {
		h := newHarness(t)
		id, dispatch := h.launched(801, 8011)
		h.report(id, "ready", model.ReportReady)
		h.finish(dispatch, model.RunOutcome{StopReason: "error", Error: "invalid API key", UsageKnown: true})
		got := h.ticket(id)
		if got.Lifecycle != model.LifecycleOpen || got.Hold != model.HoldFailed {
			t.Fatalf("failed run ended as lifecycle=%s outcome=%s hold=%s", got.Lifecycle, got.Outcome, got.Hold)
		}
	})
	t.Run("budget stop after ready stays open", func(t *testing.T) {
		h := newHarness(t)
		id, dispatch := h.launched(802, 8021)
		h.report(id, "ready", model.ReportReady)
		h.finish(dispatch, model.RunOutcome{StopReason: "max_turns", Usage: model.Spend{MicroUSD: 1}, UsageKnown: true})
		if got := h.ticket(id); got.Lifecycle != model.LifecycleOpen {
			t.Fatalf("budget-stopped run completed: %+v", got)
		}
	})
	t.Run("unknown run after ready recovers instead of completing", func(t *testing.T) {
		h := newHarness(t)
		id, dispatch := h.launched(803, 8031)
		h.report(id, "ready", model.ReportReady)
		h.finish(dispatch, model.RunOutcome{StopReason: "unknown"})
		got := h.ticket(id)
		if got.Lifecycle != model.LifecycleOpen || got.Gen != 2 {
			t.Fatalf("unknown run completed: %+v", got)
		}
		// The recovery dispatch finishes without a report of its own: the
		// previous generation's ready does not end the Ticket.
		admitted := h.runAll()
		if len(admitted) != 1 {
			t.Fatalf("recovery not admitted: %+v", admitted)
		}
		h.finish(admitted[0].Dispatch.ID, completed(1))
		if got := h.ticket(id); got.Lifecycle != model.LifecycleOpen {
			t.Fatalf("earlier generation's report reused: %+v", got)
		}
	})
}

// A queue receipt (Delivered) means accepted, not read: only what a
// dispatch was submitted with counts as handled when it finishes.
func TestDeliveredInputIsNotHandled(t *testing.T) {
	t.Run("accepted follow-up stays unhandled and wakes", func(t *testing.T) {
		h := newHarness(t)
		id, dispatch := h.launched(810, 8101)
		h.report(id, "ready", model.ReportReady)
		h.at(time.Minute)
		h.ingest("followup", comment(810, false, 8102, alice, "new requirement", h.clock.Now()))
		if err := h.store.Delivered(h.ctx, id, 1, 2); err != nil {
			t.Fatal(err)
		}
		h.finish(dispatch, completed(1))
		got := h.ticket(id)
		if got.HandledSeq != 1 || got.Lifecycle != model.LifecycleOpen {
			t.Fatalf("accepted queue receipt consumed as handled: handled=%d lifecycle=%s outcome=%s", got.HandledSeq, got.Lifecycle, got.Outcome)
		}
		admitted := h.runAll()
		if len(admitted) != 1 || admitted[0].Dispatch.ThroughInput != 2 {
			t.Fatalf("follow-up did not wake a dispatch covering it: %+v", admitted)
		}
	})
	t.Run("report after delivery covers only consumed input", func(t *testing.T) {
		h := newHarness(t)
		id, dispatch := h.launched(811, 8111)
		h.at(time.Minute)
		h.ingest("followup", comment(811, false, 8112, alice, "new requirement", h.clock.Now()))
		if err := h.store.Delivered(h.ctx, id, 1, 2); err != nil {
			t.Fatal(err)
		}
		if got := h.report(id, "ready", model.ReportReady); got.HandledSeq != 1 || got.LastReport.ThroughInput != 1 {
			t.Fatalf("report counted an unread delivery: %+v", got)
		}
		h.finish(dispatch, completed(1))
		if got := h.ticket(id); got.Lifecycle != model.LifecycleOpen || got.HandledSeq != 1 {
			t.Fatalf("follow-up lost: %+v", got)
		}
	})
	t.Run("continuation receipt records consumption", func(t *testing.T) {
		h := newHarness(t)
		id, dispatch := h.launched(812, 8121)
		h.at(time.Minute)
		h.ingest("followup", comment(812, false, 8122, alice, "new requirement", h.clock.Now()))
		if err := h.store.Delivered(h.ctx, id, 1, 2); err != nil {
			t.Fatal(err)
		}
		// The run ended with the follow-up still queued; the dispatcher
		// continued under the same lease and records what it consumed.
		receipt := model.SubmissionReceipt{RequestID: h.dispatch(dispatch).RequestID, SubmissionID: "sub-" + string(dispatch), RunID: "run-2", ThroughInput: 2, At: h.clock.Now()}
		for range 2 {
			if err := h.store.Submitted(h.ctx, dispatch, receipt); err != nil {
				t.Fatal(err)
			}
		}
		if got := h.dispatch(dispatch); got.ThroughInput != 2 || got.RunID != "run-2" {
			t.Fatalf("continuation not recorded: %+v", got)
		}
		h.report(id, "ready", model.ReportReady)
		h.finish(dispatch, completed(1))
		if got := h.ticket(id); got.HandledSeq != 2 || got.Lifecycle != model.LifecycleEnded || got.Outcome != model.OutcomeCompleted {
			t.Fatalf("consumed continuation not handled: %+v", got)
		}
	})
	t.Run("report before a consumed human input does not complete", func(t *testing.T) {
		h := newHarness(t)
		id, dispatch := h.launched(813, 8131)
		h.report(id, "ready", model.ReportReady)
		h.at(time.Minute)
		h.ingest("followup", comment(813, false, 8132, alice, "new requirement", h.clock.Now()))
		receipt := model.SubmissionReceipt{SubmissionID: "sub-" + string(dispatch), ThroughInput: 2}
		if err := h.store.Submitted(h.ctx, dispatch, receipt); err != nil {
			t.Fatal(err)
		}
		h.finish(dispatch, completed(1))
		if got := h.ticket(id); got.Lifecycle != model.LifecycleOpen || got.HandledSeq != 2 {
			t.Fatalf("stale ready answered a newer human input: %+v", got)
		}
	})
	t.Run("receipt beyond the inputs is rejected", func(t *testing.T) {
		h := newHarness(t)
		_, dispatch := h.launched(814, 8141)
		receipt := model.SubmissionReceipt{SubmissionID: "sub-" + string(dispatch), ThroughInput: 9}
		if err := h.store.Submitted(h.ctx, dispatch, receipt); err == nil {
			t.Fatal("receipt through a missing input accepted")
		}
	})
}

// A mention reopens (or requests) only when GitHub's time of the event is
// strictly after the end of every ended Ticket on the thread.
func TestMentionMustPostdateTicketEnd(t *testing.T) {
	end := func(t *testing.T) (*harness, model.TicketID) {
		h := newHarness(t)
		id, dispatch := h.launched(820, 8201)
		h.at(time.Hour)
		h.report(id, "ready", model.ReportReady)
		h.finish(dispatch, completed(1))
		if got := h.ticket(id); got.Lifecycle != model.LifecycleEnded || !got.EndedAt.Equal(epoch.Add(time.Hour)) {
			t.Fatalf("setup: %+v", got)
		}
		h.at(2 * time.Hour)
		return h, id
	}
	t.Run("historical mention discovered late is ignored", func(t *testing.T) {
		h, id := end(t)
		result := h.ingest("late", mention(820, false, 8202, alice, "historical request", epoch.Add(30*time.Minute)))
		if got := h.ticket(id); got.Lifecycle != model.LifecycleEnded || got.Gen != 1 {
			t.Fatalf("historical mention predating EndedAt reopened ticket: decision=%s gen=%d", result.Decisions[0].Kind, got.Gen)
		}
		if decision := result.Decisions[0]; decision.Kind != model.DecisionIgnored || len(decision.Tickets) != 0 {
			t.Fatalf("historical mention created work: %+v", decision)
		}
	})
	t.Run("mention at the end instant is not fresh", func(t *testing.T) {
		h, id := end(t)
		result := h.ingest("tie", mention(820, false, 8203, alice, "same instant", epoch.Add(time.Hour)))
		if result.Decisions[0].Kind != model.DecisionIgnored || h.ticket(id).Lifecycle != model.LifecycleEnded {
			t.Fatalf("tie reopened: %+v", result.Decisions[0])
		}
	})
	t.Run("historical issue body does not start a new ticket", func(t *testing.T) {
		h, _ := end(t)
		result := h.ingest("late-open", issueOpened(820, parth, "@tempest-bot fix", true, epoch))
		if result.Decisions[0].Kind != model.DecisionIgnored {
			t.Fatalf("historical opened event: %+v", result.Decisions[0])
		}
	})
	t.Run("mention after the end reopens", func(t *testing.T) {
		h, id := end(t)
		result := h.ingest("fresh", mention(820, false, 8204, alice, "one more", epoch.Add(time.Hour+time.Second)))
		if result.Decisions[0].Kind != model.DecisionReopen || h.ticket(id).Gen != 2 {
			t.Fatalf("fresh mention: %+v", result.Decisions[0])
		}
	})
}

// Reopen is authorized by the reopening actor's Mandate for the new
// generation, revalidated at admission.
func TestReopenAuthorizedByReopeningActor(t *testing.T) {
	setup := func(t *testing.T) (*harness, model.TicketID) {
		h := newHarness(t)
		id, dispatch := h.launched(830, 8301)
		h.report(id, "ready", model.ReportReady)
		h.finish(dispatch, completed(1))
		return h, id
	}
	t.Run("listed reopener is admitted after the original actor left", func(t *testing.T) {
		h, id := setup(t)
		h.config.Admission.Actors = []model.ActorID{{ID: alice.ID, Login: alice.Login}}
		h.reopen()
		h.at(time.Hour)
		h.ingest("reopen", mention(830, false, 8302, alice, "new authorized request", h.clock.Now()))
		if got := h.admit(); len(got) != 1 || got[0].Mandate.Actor.ID != alice.ID || got[0].Mandate.RequestEvent != model.CommentID(8302, h.clock.Now()) {
			t.Fatalf("listed reopen actor not accepted by admission: admitted=%+v hold=%s", got, h.ticket(id).Hold)
		}
	})
	t.Run("removed reopener is revoked even if the original actor is listed", func(t *testing.T) {
		h, id := setup(t)
		h.at(time.Hour)
		h.ingest("reopen", mention(830, false, 8303, alice, "new request", h.clock.Now()))
		h.config.Admission.Actors = []model.ActorID{{ID: parth.ID, Login: parth.Login}}
		h.reopen()
		if got := h.admit(); len(got) != 0 || h.ticket(id).Hold != model.HoldRevoked {
			t.Fatalf("removed reopener admitted: %+v hold=%s", got, h.ticket(id).Hold)
		}
		// An operator retry re-authorizes the Ticket.
		if err := h.store.Operator().Retry(h.ctx, model.OperatorRequest{ID: "retry", Target: string(id)}); err != nil {
			t.Fatal(err)
		}
		if got := h.admit(); len(got) != 1 {
			t.Fatalf("operator retry not admitted: %+v", got)
		}
	})
}

// PR state facts are ordered by GitHub's time per PR, not by arrival.
func TestPRStateFactsOrderedByGitHubTime(t *testing.T) {
	reopened := func(number int, at time.Time) model.Event {
		return model.Event{ID: model.PRReopenedID(number, at), Kind: model.EventPRReopened, Thread: model.ThreadRef{Number: number, IsPR: true}, At: at}
	}
	setup := func(t *testing.T, number int) (*harness, model.TicketID, model.DispatchID) {
		h := newHarness(t)
		id, dispatch := h.launched(number-1, int64(number)*10)
		h.openPR(id, "pr", number)
		h.at(3 * time.Hour)
		return h, id, dispatch
	}
	t.Run("delayed older close cannot override newer reopen", func(t *testing.T) {
		h, id, dispatch := setup(t, 841)
		h.ingest("reopened", reopened(841, epoch.Add(2*time.Hour)))
		h.ingest("late-close", prClosed(841, false, epoch.Add(time.Hour)))
		h.finish(dispatch, completed(1))
		if got := h.ticket(id); got.Lifecycle != model.LifecycleOpen {
			t.Fatalf("older close overrode newer reopen: lifecycle=%s outcome=%s", got.Lifecycle, got.Outcome)
		}
	})
	t.Run("delayed older merge cannot override newer reopen", func(t *testing.T) {
		h, id, dispatch := setup(t, 843)
		h.ingest("reopened", reopened(843, epoch.Add(2*time.Hour)))
		h.ingest("late-merge", prClosed(843, true, epoch.Add(time.Hour)))
		h.finish(dispatch, completed(1))
		if got := h.ticket(id); got.Lifecycle != model.LifecycleOpen {
			t.Fatalf("older merge overrode newer reopen: %+v", got)
		}
	})
	t.Run("close in the same second as a reopen does not end", func(t *testing.T) {
		h, id, dispatch := setup(t, 845)
		at := epoch.Add(2 * time.Hour)
		h.ingest("reopened", reopened(845, at))
		h.ingest("same-second-close", prClosed(845, false, at))
		h.finish(dispatch, completed(1))
		if got := h.ticket(id); got.Lifecycle != model.LifecycleOpen {
			t.Fatalf("tie ended the ticket: %+v", got)
		}
	})
	t.Run("newer close after reopen still ends", func(t *testing.T) {
		h, id, dispatch := setup(t, 847)
		h.ingest("closed", prClosed(847, false, epoch.Add(time.Hour)))
		h.ingest("reopened", reopened(847, epoch.Add(2*time.Hour)))
		h.ingest("closed-again", prClosed(847, false, epoch.Add(150*time.Minute)))
		h.finish(dispatch, completed(1))
		if got := h.ticket(id); got.Lifecycle != model.LifecycleEnded || got.Outcome != model.OutcomeClosed {
			t.Fatalf("newest close ignored: %+v", got)
		}
	})
	t.Run("snapshot older than a known fact is ignored", func(t *testing.T) {
		h, id, dispatch := setup(t, 849)
		h.ingest("reopened", reopened(849, epoch.Add(2*time.Hour)))
		old := model.PullRequest{Number: 849, Author: account, HeadRef: model.TicketBranch(id), HeadRepo: "acme/widgets", State: "closed", ClosedAt: epoch.Add(time.Hour), UpdatedAt: epoch.Add(time.Hour)}
		if _, err := h.store.Ingest(h.ctx, model.IntakeBatch{Resource: "pr:849", PRs: []model.PullRequest{old}, ReadAt: h.clock.Now()}); err != nil {
			t.Fatal(err)
		}
		h.finish(dispatch, completed(1))
		if got := h.ticket(id); got.Lifecycle != model.LifecycleOpen {
			t.Fatalf("older snapshot overrode newer reopen: %+v", got)
		}
	})
}

// Ledger arithmetic saturates instead of wrapping: no spend, grant or
// reservation can create budget.
func TestLedgerArithmeticCannotMintAllowance(t *testing.T) {
	l := model.Ledger{Allowance: model.Spend{MicroUSD: 10_000_000}, Spent: model.Spend{MicroUSD: math.MaxInt64}}
	settle(&l, "d", model.Spend{MicroUSD: math.MaxInt64, TotalTokens: math.MaxInt64}, false)
	if left := leftMicro(l); left != 0 || l.Spent.MicroUSD != math.MaxInt64 {
		t.Errorf("spent overflow reopened budget: spent=%d left=%d", l.Spent.MicroUSD, left)
	}
	granted := model.Ledger{Allowance: model.Spend{MicroUSD: 1, TotalTokens: 1}, Granted: model.Spend{MicroUSD: math.MaxInt64, TotalTokens: math.MaxInt64}}
	granted.Granted = addSpend(granted.Granted, model.Spend{MicroUSD: math.MaxInt64, TotalTokens: math.MaxInt64})
	if granted.Granted.MicroUSD != math.MaxInt64 || leftMicro(granted) != math.MaxInt64 {
		t.Errorf("repeated grant wrapped: granted=%d left=%d", granted.Granted.MicroUSD, leftMicro(granted))
	}
	if left, _ := leftTokens(granted); left != math.MaxInt64 {
		t.Errorf("token grant wrapped: left=%d", left)
	}
	reserving := model.Ledger{Allowance: model.Spend{MicroUSD: 10}, Reservations: map[model.DispatchID]model.Spend{"a": {MicroUSD: math.MaxInt64}, "b": {MicroUSD: math.MaxInt64}}}
	if left := leftMicro(reserving); left != 0 {
		t.Errorf("reservation overflow reopened budget: left=%d", left)
	}
	negative := model.Ledger{Allowance: model.Spend{MicroUSD: 10}}
	settle(&negative, "d", model.Spend{MicroUSD: -1_000_000}, false)
	if left := leftMicro(negative); left != 10 || negative.Spent.MicroUSD != 0 {
		t.Errorf("negative usage credited the ledger: spent=%d left=%d", negative.Spent.MicroUSD, left)
	}
	for _, usd := range []float64{1e300, 9.3e12} {
		if got := micro(usd); got != math.MaxInt64 {
			t.Errorf("micro(%g) = %d, want saturation", usd, got)
		}
	}
}
