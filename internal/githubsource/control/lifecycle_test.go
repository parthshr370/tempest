package control

import (
	"errors"
	"testing"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
)

func TestCompletionTable(t *testing.T) {
	ready := &model.Report{Status: model.ReportReady, Gen: 1}
	dropped := &model.Report{Status: model.ReportDropped, Gen: 1}
	staleReady := &model.Report{Status: model.ReportReady, Gen: 1}
	prs := map[int]model.PullRequest{
		5: {Number: 5, ClosedAt: epoch.Add(time.Hour)},
		6: {Number: 6, ClosedAt: epoch.Add(2 * time.Hour)},
		7: {Number: 7, ClosedAt: epoch.Add(3 * time.Hour)},
	}
	cases := []struct {
		name       string
		ticket     model.Ticket
		unfinished bool
		outcome    model.Outcome
		end        bool
	}{
		{name: "ready without links completes", ticket: model.Ticket{Gen: 1, LastReport: ready}, outcome: model.OutcomeCompleted, end: true},
		{name: "unfinished defers", ticket: model.Ticket{Gen: 1, LastReport: ready}, unfinished: true},
		{name: "stale report from older generation", ticket: model.Ticket{Gen: 2, LastReport: staleReady}},
		{name: "ready with open own PR waits", ticket: model.Ticket{Gen: 1, LastReport: ready, PRs: []model.PRLink{{Number: 5, Own: true}}}},
		{name: "own PR merged", ticket: model.Ticket{Gen: 1, PRs: []model.PRLink{{Number: 5, Own: true, Closed: true, Merged: true}}}, outcome: model.OutcomeMerged, end: true},
		{name: "one of two own PRs closed waits", ticket: model.Ticket{Gen: 1, PRs: []model.PRLink{{Number: 5, Own: true, Closed: true, Merged: true}, {Number: 6, Own: true}}}},
		{name: "latest close of own set decides closed", ticket: model.Ticket{Gen: 1, PRs: []model.PRLink{{Number: 5, Own: true, Closed: true, Merged: true}, {Number: 6, Own: true, Closed: true}}}, outcome: model.OutcomeClosed, end: true},
		{name: "latest close of own set decides merged", ticket: model.Ticket{Gen: 1, PRs: []model.PRLink{{Number: 6, Own: true, Closed: true}, {Number: 7, Own: true, Closed: true, Merged: true}}}, outcome: model.OutcomeMerged, end: true},
		{name: "own PR first: open followed PR ignored", ticket: model.Ticket{Gen: 1, PRs: []model.PRLink{{Number: 5}, {Number: 6, Own: true, Closed: true, Merged: true}}}, outcome: model.OutcomeMerged, end: true},
		{name: "followed only when no own", ticket: model.Ticket{Gen: 1, PRs: []model.PRLink{{Number: 5, Closed: true}}}, outcome: model.OutcomeClosed, end: true},
		{name: "dropped ends dropped", ticket: model.Ticket{Gen: 1, LastReport: dropped, PRs: []model.PRLink{{Number: 5, Own: true}}}, outcome: model.OutcomeDropped, end: true},
		{name: "dropped with closed set takes close outcome", ticket: model.Ticket{Gen: 1, LastReport: dropped, PRs: []model.PRLink{{Number: 5, Own: true, Closed: true, Merged: true}}}, outcome: model.OutcomeMerged, end: true},
		{name: "ended stays", ticket: model.Ticket{Gen: 1, Lifecycle: model.LifecycleEnded, LastReport: ready}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.ticket.Lifecycle == "" {
				tc.ticket.Lifecycle = model.LifecycleOpen
			}
			outcome, end := completion(tc.ticket, prs, tc.unfinished)
			if outcome != tc.outcome || end != tc.end {
				t.Fatalf("got %q %v, want %q %v", outcome, end, tc.outcome, tc.end)
			}
		})
	}
}

// launched creates a Ticket and runs its launch dispatch to submitted.
func (h *harness) launched(number int, commentID int64) (model.TicketID, model.DispatchID) {
	h.t.Helper()
	id := h.ingest("thread", mention(number, false, commentID, parth, "fix", h.clock.Now())).Decisions[0].Tickets[0]
	for _, a := range h.runAll() {
		if a.Dispatch.Ticket == id {
			return id, a.Dispatch.ID
		}
	}
	h.t.Fatalf("ticket %s not admitted", id)
	return "", ""
}

func TestReadyReportEndsAfterDispatchFinishes(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	id, dispatch := h.launched(20, 1)
	ticket := h.report(id, "r1", model.ReportReady)
	if ticket.Lifecycle != model.LifecycleOpen {
		t.Fatal("ended while the dispatch was live")
	}
	h.finish(dispatch, completed(300_000))
	ticket = h.ticket(id)
	if ticket.Lifecycle != model.LifecycleEnded || ticket.Outcome != model.OutcomeCompleted || ticket.EndedBy != "executor" {
		t.Fatalf("ticket: %+v", ticket)
	}
	// A repeated report call returns the recorded state without deciding
	// again.
	again := h.report(id, "r1", model.ReportReady)
	if again.Lifecycle != model.LifecycleEnded {
		t.Fatalf("replayed report: %+v", again)
	}
}

func TestCommentWhileFinishingDefersEnd(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	id, dispatch := h.launched(21, 1)
	h.report(id, "r1", model.ReportReady)
	// A human follow-up arrives before the run ends and is not delivered.
	h.at(time.Minute)
	h.ingest("thread", comment(21, false, 2, alice, "wait, also X", h.clock.Now()))
	h.finish(dispatch, completed(100_000))
	ticket := h.ticket(id)
	if ticket.Lifecycle != model.LifecycleOpen {
		t.Fatalf("ended with unhandled human input: %+v", ticket)
	}
	admitted := h.runAll()
	if len(admitted) != 1 || admitted[0].Dispatch.Kind != model.DispatchWake || admitted[0].Dispatch.ThroughInput != 2 {
		t.Fatalf("wake: %+v", admitted)
	}
	h.finish(admitted[0].Dispatch.ID, completed(100_000))
	if ticket := h.ticket(id); ticket.Lifecycle != model.LifecycleEnded || ticket.Outcome != model.OutcomeCompleted {
		t.Fatalf("deferred end not applied: %+v", ticket)
	}
}

func TestMultiPRCloseRaces(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	id, dispatch := h.launched(30, 1)
	h.openPR(id, "pr-a", 31)
	h.openPR(id, "pr-b", 32)
	h.report(id, "r1", model.ReportReady)
	h.finish(dispatch, completed(100_000))
	detail := h.detail(id)
	if detail.Ticket.Lifecycle != model.LifecycleOpen || len(detail.Ticket.PRs) != 2 || detail.Stage != model.StageReview {
		t.Fatalf("after open PRs: stage %s ticket %+v", detail.Stage, detail.Ticket)
	}
	h.at(time.Hour)
	h.ingest("pr:31", prClosed(31, true, h.clock.Now()))
	if ticket := h.ticket(id); ticket.Lifecycle != model.LifecycleOpen {
		t.Fatal("ended with an own PR still open")
	}
	// The second PR closes while a dispatch is live: the end is deferred.
	h.ingest("thread", comment(30, false, 2, parth, "one more thing", h.clock.Now()))
	admitted := h.runAll()
	h.at(2 * time.Hour)
	h.ingest("pr:32", prClosed(32, false, h.clock.Now()))
	if ticket := h.ticket(id); ticket.Lifecycle != model.LifecycleOpen {
		t.Fatal("ended while a dispatch was live")
	}
	// An authoritative input after the close keeps it open past this run.
	h.ingest("thread", comment(30, false, 3, alice, "and please answer this", h.clock.Now()))
	h.finish(admitted[0].Dispatch.ID, completed(100_000))
	if ticket := h.ticket(id); ticket.Lifecycle != model.LifecycleOpen {
		t.Fatal("ended with an unhandled authoritative input")
	}
	admitted = h.runAll()
	h.finish(admitted[0].Dispatch.ID, completed(100_000))
	ticket := h.ticket(id)
	if ticket.Lifecycle != model.LifecycleEnded || ticket.Outcome != model.OutcomeClosed || ticket.EndedBy != "source" {
		t.Fatalf("final outcome (latest close was #32 unmerged): %+v", ticket)
	}
}

func TestAutoBindAndPRThreadTicket(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	id, dispatch := h.launched(40, 1)
	// An open_pr whose outcome was never settled: the PR fact alone binds.
	pr := model.PullRequest{Number: 41, Author: account, HeadRef: model.TicketBranch(id), HeadRepo: "acme/widgets", HeadSHA: "h1", BaseRef: "main", State: "open", UpdatedAt: h.clock.Now()}
	if _, err := h.store.Ingest(h.ctx, model.IntakeBatch{Resource: "pr:41", PRs: []model.PullRequest{pr}, ReadAt: h.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	ticket := h.ticket(id)
	if len(ticket.PRs) != 1 || !ticket.PRs[0].Own || ticket.PRs[0].Number != 41 {
		t.Fatalf("auto bind: %+v", ticket.PRs)
	}
	// A foreign PR on the same branch name from a fork does not bind.
	fork := pr
	fork.Number, fork.HeadRepo = 42, "evil/widgets"
	if _, err := h.store.Ingest(h.ctx, model.IntakeBatch{Resource: "pr:42", PRs: []model.PullRequest{fork}, ReadAt: h.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	if ticket := h.ticket(id); len(ticket.PRs) != 1 {
		t.Fatalf("fork bound: %+v", ticket.PRs)
	}
	h.finish(dispatch, completed(1))

	// A request on a PR thread follows that PR (not own) and ends with it.
	h.at(time.Hour)
	result := h.ingest("pr:50", mention(50, true, 9, parth, "review this", h.clock.Now()))
	prTicket := result.Decisions[0].Tickets[0]
	if prTicket[0] != 'p' {
		t.Fatalf("pr ticket id %s", prTicket)
	}
	if ticket := h.ticket(prTicket); len(ticket.PRs) != 1 || ticket.PRs[0].Own {
		t.Fatalf("pr ticket link: %+v", ticket.PRs)
	}
	admitted := h.runAll()
	h.finish(admitted[0].Dispatch.ID, completed(1))
	h.ingest("pr:50", prClosed(50, true, h.clock.Now()))
	if ticket := h.ticket(prTicket); ticket.Outcome != model.OutcomeMerged {
		t.Fatalf("pr ticket: %+v", ticket)
	}
}

func TestReopenOnlyOnFreshMention(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	id, dispatch := h.launched(60, 1)
	h.report(id, "r1", model.ReportReady)
	h.finish(dispatch, completed(100_000))
	if h.ticket(id).Lifecycle != model.LifecycleEnded {
		t.Fatal("not ended")
	}
	h.at(time.Hour)
	result := h.ingest("thread", comment(60, false, 2, parth, "thanks!", h.clock.Now()))
	if result.Decisions[0].Kind != model.DecisionObserved || h.ticket(id).Lifecycle != model.LifecycleEnded {
		t.Fatalf("plain thanks reopened: %+v", result.Decisions[0])
	}
	result = h.ingest("thread", mention(60, false, 3, alice, "one more change", h.clock.Now()))
	if result.Decisions[0].Kind != model.DecisionReopen || result.Decisions[0].Tickets[0] != id {
		t.Fatalf("mention did not reopen: %+v", result.Decisions[0])
	}
	detail := h.detail(id)
	if detail.Ticket.Lifecycle != model.LifecycleOpen || detail.Ticket.Gen != 2 || detail.Assignments[1].Reason != model.ReasonReopen || detail.Assignments[0].State != model.AssignmentEnded {
		t.Fatalf("reopened: %+v %+v", detail.Ticket, detail.Assignments)
	}
	// The reopened Ticket has a session, so its launch is a human
	// continuation.
	admitted := h.admit()
	if len(admitted) != 1 || admitted[0].Dispatch.Class != model.SlotHuman || admitted[0].Dispatch.Gen != 2 {
		t.Fatalf("reopen dispatch: %+v", admitted)
	}
	// Ledger spend carries over a reopen.
	if ledger := h.ledger(model.TicketLedger(id)); ledger.Spent.MicroUSD != 100_000 {
		t.Fatalf("ledger reset: %+v", ledger)
	}
}

func TestRevokedMandateHeldAtAdmission(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	id := h.ingest("thread", mention(70, false, 1, alice, "fix", h.clock.Now())).Decisions[0].Tickets[0]
	// The operator removes alice from the file and restarts.
	h.config.Admission.Actors = []model.ActorID{{Login: parth.Login, ID: parth.ID}}
	h.reopen()
	if admitted := h.admit(); len(admitted) != 0 {
		t.Fatalf("revoked admitted: %+v", admitted)
	}
	if ticket := h.ticket(id); ticket.Hold != model.HoldRevoked {
		t.Fatalf("hold %q", ticket.Hold)
	}
	// An authoritative input from a listed actor does not release revoked.
	h.ingest("thread", comment(70, false, 2, parth, "go ahead", h.clock.Now()))
	if admitted := h.admit(); len(admitted) != 0 {
		t.Fatalf("revoked admitted after input: %+v", admitted)
	}
	// Operator retry releases it and re-authorizes the Ticket.
	if err := h.store.Operator().Retry(h.ctx, model.OperatorRequest{ID: "op-1", Target: string(id), Reason: "approved"}); err != nil {
		t.Fatal(err)
	}
	admitted := h.admit()
	if len(admitted) != 1 || admitted[0].Dispatch.Gen != 2 {
		t.Fatalf("after retry: %+v", admitted)
	}
	h.reopen()
	if ticket := h.ticket(id); ticket.Hold != model.HoldNone {
		t.Fatalf("hold after restart: %q", ticket.Hold)
	}
}

func TestPauseHoldsDispatchAndOutboxWhileIntakeContinues(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	operator := h.store.Operator()
	if err := operator.SetPaused(h.ctx, model.OperatorRequest{ID: "p1", Reason: "maintenance"}, true); err != nil {
		t.Fatal(err)
	}
	result := h.ingest("thread", mention(80, false, 1, parth, "fix", h.clock.Now()))
	if result.Decisions[0].Kind != model.DecisionRequest {
		t.Fatalf("intake paused: %+v", result)
	}
	if admitted := h.admit(); len(admitted) != 0 {
		t.Fatalf("admitted while paused: %+v", admitted)
	}
	h.ingest("thread", command(80, 2, parth, model.CommandStatus, h.clock.Now()))
	if due, _ := h.store.Due(h.ctx, h.clock.Now()); len(due) != 0 {
		t.Fatalf("due while paused: %+v", due)
	}
	h.reopen()
	status, err := h.store.Operator().Status(h.ctx)
	if err != nil || !status.Pause.Paused || status.Queued != 1 || status.OpenTickets != 1 {
		t.Fatalf("status: %+v %v", status, err)
	}
	if err := h.store.Operator().SetPaused(h.ctx, model.OperatorRequest{ID: "p2"}, false); err != nil {
		t.Fatal(err)
	}
	if admitted := h.admit(); len(admitted) != 1 {
		t.Fatalf("not admitted after unpause: %+v", admitted)
	}
	if due, _ := h.store.Due(h.ctx, h.clock.Now()); len(due) != 1 {
		t.Fatalf("not due after unpause: %+v", due)
	}
}

func TestOperatorStopAndIdempotentRequests(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	id, dispatch := h.launched(90, 1)
	operator := h.store.Operator()
	request := model.OperatorRequest{ID: "stop-1", Target: string(id), Reason: "wrong repo"}
	if err := operator.Stop(h.ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := operator.Stop(h.ctx, request); err != nil {
		t.Fatalf("repeated stop: %v", err)
	}
	if err := operator.Retry(h.ctx, model.OperatorRequest{ID: "retry-1", Target: string(id)}); !errors.Is(err, model.ErrStopped) {
		t.Fatalf("retry stopped: %v", err)
	}
	if err := operator.Stop(h.ctx, model.OperatorRequest{ID: "stop-2", Target: "nope"}); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("stop unknown: %v", err)
	}
	h.finish(dispatch, model.RunOutcome{StopReason: "aborted"})
	detail := h.detail(id)
	if detail.Ticket.Outcome != model.OutcomeStopped || detail.Ticket.EndedBy != "operator" || len(detail.Dispatches) != 1 {
		t.Fatalf("stopped: %+v", detail.Ticket)
	}
	if detail.Assignments[0].EndReason != "stopped" {
		t.Fatalf("assignment: %+v", detail.Assignments[0])
	}
}

func TestRunOutcomes(t *testing.T) {
	cases := []struct {
		name    string
		outcome model.RunOutcome
		check   func(t *testing.T, h *harness, id model.TicketID)
	}{
		{name: "provider transient error requeues with backoff", outcome: model.RunOutcome{StopReason: "error", Error: "503 service unavailable: overloaded", UsageKnown: true}, check: func(t *testing.T, h *harness, id model.TicketID) {
			next := h.dispatch(model.NewDispatchID(id, 2))
			if next.State != model.DispatchQueued || next.Kind != model.DispatchWake || next.Attempts != 1 || !next.RetryAt.Equal(h.clock.Now().Add(transientBackoff)) || next.Gen != 1 {
				t.Fatalf("retry dispatch: %+v", next)
			}
		}},
		{name: "terminal error holds failed", outcome: model.RunOutcome{StopReason: "error", Error: "invalid api key", UsageKnown: true}, check: func(t *testing.T, h *harness, id model.TicketID) {
			if ticket := h.ticket(id); ticket.Hold != model.HoldFailed || ticket.DispatchSeq != 1 {
				t.Fatalf("ticket: %+v", ticket)
			}
		}},
		{name: "loop detected holds failed", outcome: model.RunOutcome{StopReason: "loop_detected", UsageKnown: true}, check: func(t *testing.T, h *harness, id model.TicketID) {
			if ticket := h.ticket(id); ticket.Hold != model.HoldFailed {
				t.Fatalf("ticket: %+v", ticket)
			}
		}},
		{name: "unknown starts a recovery generation", outcome: model.RunOutcome{StopReason: "unknown"}, check: func(t *testing.T, h *harness, id model.TicketID) {
			detail := h.detail(id)
			if detail.Ticket.Gen != 2 || detail.Assignments[0].EndReason != "replaced" || detail.Assignments[1].Reason != model.ReasonRecovery {
				t.Fatalf("recovery: %+v", detail.Assignments)
			}
			next := detail.Dispatches[1]
			if next.Kind != model.DispatchRecovery || next.Gen != 2 || next.RequestID != model.LaunchRequestID(id, 2, 2) {
				t.Fatalf("recovery dispatch: %+v", next)
			}
		}},
		{name: "budget stop with money left is quiescent", outcome: model.RunOutcome{StopReason: "max_turns", Usage: model.Spend{MicroUSD: 500_000}, UsageKnown: true}, check: func(t *testing.T, h *harness, id model.TicketID) {
			if ticket := h.ticket(id); ticket.Hold != model.HoldNone || ticket.DispatchSeq != 1 {
				t.Fatalf("ticket: %+v", ticket)
			}
		}},
		{name: "budget stop that exhausts the ticket holds budget", outcome: model.RunOutcome{StopReason: "max_cost", Usage: model.Spend{MicroUSD: 9_900_000}, UsageKnown: true}, check: func(t *testing.T, h *harness, id model.TicketID) {
			if ticket := h.ticket(id); ticket.Hold != model.HoldBudget {
				t.Fatalf("ticket: %+v", ticket)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.at(0)
			id, dispatch := h.launched(100, 1)
			h.at(time.Minute)
			h.finish(dispatch, tc.outcome)
			tc.check(t, h, id)
			h.reopen()
			tc.check(t, h, id)
		})
	}
}

func TestTransientRetriesStopAfterThree(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	id, dispatch := h.launched(101, 1)
	transient := model.RunOutcome{StopReason: "error", Error: "connection reset by peer", UsageKnown: true}
	for round := range 4 {
		h.at(time.Duration(round+1) * time.Hour)
		h.finish(dispatch, transient)
		if round == 3 {
			break
		}
		if got := h.admit(); len(got) != 0 {
			t.Fatalf("round %d admitted before backoff: %+v", round, got)
		}
		h.clock.Advance(10 * time.Minute)
		admitted := h.runAll()
		if len(admitted) != 1 {
			t.Fatalf("round %d: %+v", round, admitted)
		}
		dispatch = admitted[0].Dispatch.ID
	}
	if ticket := h.ticket(id); ticket.Hold != model.HoldFailed || ticket.DispatchSeq != 4 {
		t.Fatalf("after 4 transient failures: %+v", ticket)
	}
}

func TestAbandon(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	id := h.ingest("thread", mention(110, false, 1, parth, "fix", h.clock.Now())).Decisions[0].Tickets[0]
	for attempt := range 4 {
		admitted := h.admit()
		if len(admitted) != 1 {
			t.Fatalf("attempt %d not admitted: %+v", attempt, admitted)
		}
		dispatch := admitted[0].Dispatch.ID
		failure := model.DispatchFailure{Reason: "host limit", Transient: true, RetryAt: h.clock.Now().Add(time.Minute), At: h.clock.Now()}
		if err := h.store.Abandon(h.ctx, dispatch, failure); err != nil {
			t.Fatal(err)
		}
		// Released reservations are not charged.
		if ledger := h.ledger(model.TicketLedger(id)); len(ledger.Reservations) != 0 || ledger.Spent.MicroUSD != 0 {
			t.Fatalf("ledger after abandon: %+v", ledger)
		}
		if attempt < 3 {
			if got := h.admit(); len(got) != 0 {
				t.Fatalf("admitted before RetryAt: %+v", got)
			}
			h.clock.Advance(time.Minute)
		}
	}
	ticket := h.ticket(id)
	if ticket.Hold != model.HoldFailed || h.dispatch(model.NewDispatchID(id, 1)).State != model.DispatchCancelled {
		t.Fatalf("after 4 transient abandons: %+v", ticket)
	}
	// A non-transient abandon (interrupted prepare) holds failed at once.
	other := h.ingest("thread", mention(111, false, 2, parth, "fix", h.clock.Now())).Decisions[0].Tickets[0]
	admitted := h.admit()
	if err := h.store.Abandon(h.ctx, admitted[0].Dispatch.ID, model.DispatchFailure{Reason: "prepare interrupted"}); err != nil {
		t.Fatal(err)
	}
	if ticket := h.ticket(other); ticket.Hold != model.HoldFailed || ticket.HoldReason != "dispatch failed: prepare interrupted" {
		t.Fatalf("non-transient: %+v", ticket)
	}
	if err := h.store.Abandon(h.ctx, admitted[0].Dispatch.ID, model.DispatchFailure{Reason: "prepare interrupted"}); err != nil {
		t.Fatalf("repeated abandon: %v", err)
	}
	// Retry starts a new generation and admits again.
	if err := h.store.Operator().Retry(h.ctx, model.OperatorRequest{ID: "r", Target: string(other)}); err != nil {
		t.Fatal(err)
	}
	if got := h.admit(); len(got) != 1 || got[0].Dispatch.Gen != 2 {
		t.Fatalf("after retry: %+v", got)
	}
}

func TestBlockedReportAndAnswer(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	id, dispatch := h.launched(120, 1)
	if _, err := h.store.Report(h.ctx, model.WorkerCall{SessionID: sessionOf(id), ToolCallID: "b1"}, model.ReportRequest{Status: model.ReportBlocked, Summary: "need input", Question: &model.Question{Text: "Which API version?"}}); err != nil {
		t.Fatal(err)
	}
	h.finish(dispatch, completed(1))
	detail := h.detail(id)
	if detail.Stage != model.StageAnswer || detail.Ticket.Hold != model.HoldBlocked {
		t.Fatalf("stage %s hold %s", detail.Stage, detail.Ticket.Hold)
	}
	// A bot comment does not wake a blocked Ticket.
	h.ingest("thread", comment(120, false, 2, rabbit, "ping", h.clock.Now()))
	if admitted := h.admit(); len(admitted) != 0 {
		t.Fatalf("bot woke blocked ticket: %+v", admitted)
	}
	h.ingest("thread", comment(120, false, 3, parth, "v2", h.clock.Now()))
	admitted := h.admit()
	if len(admitted) != 1 || admitted[0].Dispatch.Class != model.SlotHuman || admitted[0].Dispatch.ThroughInput != 3 {
		t.Fatalf("answer wake: %+v", admitted)
	}
}
