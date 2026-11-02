package control

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
	"go.harness.dev/harness/tempest/durable"
)

// crashStep is one caller action against the control store: one method
// call, which a restarted caller re-issues when it did not see the result.
type crashStep struct {
	name string
	run  func(ctx context.Context, s *Store, now time.Time) error
}

func crashScenario() []crashStep {
	a := model.NewTicketID(model.ThreadRef{Number: 500}, model.CommentID(1, epoch))
	b := model.NewTicketID(model.ThreadRef{Number: 501}, model.CommentID(2, epoch))
	reconcile := true
	position := model.Position{Since: epoch, LastID: 1}
	ingest := func(resource string, events ...model.Event) func(context.Context, *Store, time.Time) error {
		return func(ctx context.Context, s *Store, now time.Time) error {
			_, err := s.Ingest(ctx, model.IntakeBatch{Resource: resource, Events: events, ReadAt: now})
			return err
		}
	}
	admit := func(ctx context.Context, s *Store, now time.Time) error {
		_, err := s.AdmitNext(ctx, now)
		return err
	}
	preparing := func(id model.DispatchID) func(context.Context, *Store, time.Time) error {
		return func(ctx context.Context, s *Store, now time.Time) error { return s.Preparing(ctx, id, now) }
	}
	prepared := func(id model.TicketID) func(context.Context, *Store, time.Time) error {
		return func(ctx context.Context, s *Store, now time.Time) error {
			return s.Prepared(ctx, id, "/wt/"+string(id), "base")
		}
	}
	bind := func(id model.TicketID) func(context.Context, *Store, time.Time) error {
		return func(ctx context.Context, s *Store, now time.Time) error {
			return s.Bind(ctx, model.SessionBinding{SessionID: sessionOf(id), Ticket: id, BoundAt: now})
		}
	}
	submitted := func(id model.TicketID, seq uint64, gen uint32) func(context.Context, *Store, time.Time) error {
		return func(ctx context.Context, s *Store, now time.Time) error {
			dispatch := model.NewDispatchID(id, seq)
			return s.Submitted(ctx, dispatch, model.SubmissionReceipt{RequestID: model.LaunchRequestID(id, seq, gen), SubmissionID: "sub-" + string(dispatch), At: now})
		}
	}
	finished := func(id model.TicketID, seq uint64, outcome model.RunOutcome) func(context.Context, *Store, time.Time) error {
		return func(ctx context.Context, s *Store, now time.Time) error {
			return s.Finished(ctx, model.NewDispatchID(id, seq), outcome, now)
		}
	}
	enqueue := func(id model.TicketID, call string, request model.EffectRequest) func(context.Context, *Store, time.Time) error {
		return func(ctx context.Context, s *Store, now time.Time) error {
			_, err := s.Enqueue(ctx, model.WorkerCall{SessionID: sessionOf(id), ToolCallID: call, At: now}, request)
			return err
		}
	}
	gen1 := func(id model.TicketID, call string) model.OutboxID { return model.NewOutboxID(id, 1, call) }
	attempt := func(id model.OutboxID) func(context.Context, *Store, time.Time) error {
		return func(ctx context.Context, s *Store, now time.Time) error {
			_, err := s.Attempt(ctx, id, now)
			return err
		}
	}
	settle := func(id model.OutboxID, settlement model.EffectSettlement) func(context.Context, *Store, time.Time) error {
		return func(ctx context.Context, s *Store, now time.Time) error {
			settlement.SettledAt = now
			_, err := s.Settle(ctx, id, settlement)
			return err
		}
	}
	operator := func(call func(ctx context.Context, o *Operator, now time.Time) error) func(context.Context, *Store, time.Time) error {
		return func(ctx context.Context, s *Store, now time.Time) error { return call(ctx, s.Operator(), now) }
	}
	return []crashStep{
		{"request a", ingest("thread:500", mention(500, false, 1, parth, "fix the parser", epoch))},
		{"request b and a stray denial", ingest("thread:501", mention(501, false, 2, alice, "fix the lexer", epoch), mention(501, false, 3, mallory, "and delete main", epoch))},
		{"notification", func(ctx context.Context, s *Store, now time.Time) error {
			_, err := s.Ingest(ctx, model.IntakeBatch{Resource: "notifications", Threads: []model.ThreadUpdate{{Ref: model.ThreadRef{Number: 500}, Reconcile: &reconcile, NotifiedAt: now}}, Cursor: &model.Cursor{Key: model.CursorNotifications, Since: now}, ReadAt: now})
			return err
		}},
		{"admit a and b", admit},
		{"a preparing", preparing(model.NewDispatchID(a, 1))},
		{"a prepared", prepared(a)},
		{"a bind", bind(a)},
		{"a submitted", submitted(a, 1, 1)},
		{"b preparing", preparing(model.NewDispatchID(b, 1))},
		{"b abandon transient", func(ctx context.Context, s *Store, now time.Time) error {
			return s.Abandon(ctx, model.NewDispatchID(b, 1), model.DispatchFailure{Reason: "host limit", Transient: true, RetryAt: now.Add(time.Minute), At: now})
		}},
		{"follow-up on a with thread position", func(ctx context.Context, s *Store, now time.Time) error {
			_, err := s.Ingest(ctx, model.IntakeBatch{Resource: "thread:500", Events: []model.Event{comment(500, false, 4, alice, "also the docs", now)}, Threads: []model.ThreadUpdate{{Ref: model.ThreadRef{Number: 500}, Comments: &position}}, ReadAt: now})
			return err
		}},
		{"a delivered", func(ctx context.Context, s *Store, now time.Time) error { return s.Delivered(ctx, a, 1, 2) }},
		{"a progress report", func(ctx context.Context, s *Store, now time.Time) error {
			_, err := s.Report(ctx, model.WorkerCall{SessionID: sessionOf(a), ToolCallID: "rep-1", At: now}, model.ReportRequest{Status: model.ReportProgress, Summary: "working"})
			return err
		}},
		{"a push", enqueue(a, "push-1", model.EffectRequest{Kind: model.EffectPush, Payload: model.EffectPayload{SHA: "c1"}})},
		{"a open_pr", enqueue(a, "pr-1", model.EffectRequest{Kind: model.EffectOpenPR, Payload: model.EffectPayload{Title: "Fix parser", Body: "Fixes #500"}})},
		{"a push attempt", attempt(gen1(a, "push-1"))},
		{"a push confirmed", settle(gen1(a, "push-1"), model.EffectSettlement{State: model.EffectConfirmed, Result: &model.EffectReply{SHA: "c1"}})},
		{"a open_pr attempt", attempt(gen1(a, "pr-1"))},
		{"a open_pr confirmed binds", settle(gen1(a, "pr-1"), model.EffectSettlement{State: model.EffectConfirmed, Result: &model.EffectReply{RemoteID: 502}})},
		{"a comment", enqueue(a, "reply-1", model.EffectRequest{Kind: model.EffectComment, Payload: model.EffectPayload{Body: "Opened #502"}})},
		{"a comment attempt", attempt(gen1(a, "reply-1"))},
		{"a comment unknown", settle(gen1(a, "reply-1"), model.EffectSettlement{State: model.EffectUnknown, Error: "listing failed"})},
		{"operator resolves", operator(func(ctx context.Context, o *Operator, now time.Time) error {
			return o.ResolveEffect(ctx, model.OperatorRequest{ID: "resolve-1", Target: string(gen1(a, "reply-1")), Reason: "seen on GitHub", At: now}, model.EffectConfirmed)
		})},
		{"a ready", func(ctx context.Context, s *Store, now time.Time) error {
			_, err := s.Report(ctx, model.WorkerCall{SessionID: sessionOf(a), ToolCallID: "rep-2", At: now}, model.ReportRequest{Status: model.ReportReady, Summary: "PR open"})
			return err
		}},
		{"a finished", finished(a, 1, completed(1_250_000))},
		{"admit b again", admit},
		{"b preparing again", preparing(model.NewDispatchID(b, 1))},
		{"b prepared", prepared(b)},
		{"b bind", bind(b)},
		{"b submitted", submitted(b, 1, 1)},
		{"b interrupted", finished(b, 1, model.RunOutcome{StopReason: "unknown"})},
		{"admit b recovery", admit},
		{"b recovery submitted", submitted(b, 2, 2)},
		{"b budget stop", finished(b, 2, model.RunOutcome{StopReason: "max_turns", Usage: model.Spend{MicroUSD: 1_900_000, TotalTokens: 12_000}, UsageKnown: true})},
		{"ci and review on a's pr", ingest("pr:502",
			model.Event{ID: model.PRHeadID(502, "c1"), Kind: model.EventPRHead, Thread: model.ThreadRef{Number: 502, IsPR: true}, CommitSHA: "c1"},
			prCI(502, "c1", nil, epoch),
			model.Event{ID: model.ReviewID(31), Kind: model.EventReview, Thread: model.ThreadRef{Number: 502, IsPR: true}, Actor: rabbit, ReviewState: "COMMENTED", CommitSHA: "c1", CommentID: 31})},
		{"a pr merged", ingest("pr:502", prClosed(502, true, epoch.Add(time.Hour)))},
		{"grant repo", operator(func(ctx context.Context, o *Operator, now time.Time) error {
			return o.Grant(ctx, model.OperatorRequest{ID: "grant-1", At: now}, model.Spend{MicroUSD: 5_000_000})
		})},
		{"pause", operator(func(ctx context.Context, o *Operator, now time.Time) error {
			return o.SetPaused(ctx, model.OperatorRequest{ID: "pause-1", Reason: "deploy", At: now}, true)
		})},
		{"status on b", ingest("thread:501", command(501, 5, parth, model.CommandStatus, epoch.Add(2*time.Hour)))},
		{"unpause", operator(func(ctx context.Context, o *Operator, now time.Time) error {
			return o.SetPaused(ctx, model.OperatorRequest{ID: "pause-2", At: now}, false)
		})},
		{"stop b", ingest("thread:501", command(501, 6, alice, model.CommandStop, epoch.Add(3*time.Hour)))},
		{"reopen a", ingest("thread:500", mention(500, false, 7, parth, "please also update the changelog", epoch.Add(4*time.Hour)))},
		{"admit a reopen", admit},
		{"a reopen submitted", submitted(a, 3, 2)},
		{"a shutdown abort", finished(a, 3, model.RunOutcome{StopReason: "aborted", Usage: model.Spend{MicroUSD: 400_000}, UsageKnown: true})},
		{"operator retry a", operator(func(ctx context.Context, o *Operator, now time.Time) error {
			return o.Retry(ctx, model.OperatorRequest{ID: "retry-1", Target: string(a), At: now})
		})},
		{"admit a retry", admit},
	}
}

// crashMode says how a commit fails and how the caller recovers.
type crashMode struct {
	// durable makes the failed commit durable with its acknowledgment lost.
	durable bool
	// restart reopens the journal file as a new process would; otherwise
	// the same Store is called again, relying on its reload after an
	// ambiguous commit failure.
	restart bool
}

// runCrashScenario runs the scenario against a fresh journal, failing the
// failAt-th commit (0: never), recovering per mode, and re-issuing the
// failed step.
func runCrashScenario(t *testing.T, failAt int, mode crashMode) (map[string]string, int) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "control.jsonl")
	clock := &fakeClock{now: epoch}
	open := func(wrap bool) (*Store, *crashStore, error) {
		log, err := durable.OpenFile(ctx, path)
		if err != nil {
			t.Fatalf("open journal: %v", err)
		}
		var store durable.Store = log
		var crash *crashStore
		if wrap {
			crash = &crashStore{Store: log, failAt: failAt, durable: mode.durable}
			store = crash
		}
		s, err := Open(ctx, Options{Store: store, Config: testConfig(), Repo: testRepo(), Clock: clock})
		if err != nil {
			log.Close(ctx)
		}
		return s, crash, err
	}
	crashed := false
	store, crash, err := open(true)
	restart := func() {
		crashed = true
		if store != nil && !mode.restart {
			return
		}
		if store != nil {
			store.Close(ctx)
		}
		var openErr error
		store, _, openErr = open(false)
		if openErr != nil {
			t.Fatalf("reopen after crash: %v", openErr)
		}
	}
	if errors.Is(err, errCrash) {
		restart()
	} else if err != nil {
		t.Fatalf("open: %v", err)
	}
	for index, step := range crashScenario() {
		now := epoch.Add(time.Duration(index) * time.Minute)
		clock.Set(now)
		err := step.run(ctx, store, now)
		if errors.Is(err, errCrash) && !crashed {
			restart()
			err = step.run(ctx, store, now)
		}
		if err != nil {
			t.Fatalf("step %d %q (crash at %d, %+v): %v", index, step.name, failAt, mode, err)
		}
	}
	defer store.Close(ctx)
	if failAt > 0 && !crashed {
		t.Fatalf("commit %d never happened", failAt)
	}
	return journal(t, store.store), crash.commits
}

// TestRestartAtEveryCommitBoundary fails each commit of a scenario that
// exercises every transition: before the write with a process restart,
// after it is durable with the acknowledgment lost and a restart, and after
// it is durable with the same process retrying. The failed step is
// re-issued each time. The final journal must equal the uninterrupted
// run's.
func TestRestartAtEveryCommitBoundary(t *testing.T) {
	want, commits := runCrashScenario(t, 0, crashMode{})
	if commits < 40 {
		t.Fatalf("scenario made only %d commits", commits)
	}
	checkScenarioOutcome(t, want)
	modes := []crashMode{{restart: true}, {durable: true, restart: true}, {durable: true}}
	for failAt := 1; failAt <= commits; failAt++ {
		for _, mode := range modes {
			got, _ := runCrashScenario(t, failAt, mode)
			if diff := diffJournals(want, got); diff != "" {
				t.Fatalf("crash at commit %d (%+v) diverged:\n%s", failAt, mode, diff)
			}
		}
	}
}

// checkScenarioOutcome pins the uninterrupted run's end state so the
// comparison is against a correct baseline, not just a repeatable one.
func checkScenarioOutcome(t *testing.T, records map[string]string) {
	t.Helper()
	a := model.NewTicketID(model.ThreadRef{Number: 500}, model.CommentID(1, epoch))
	b := model.NewTicketID(model.ThreadRef{Number: 501}, model.CommentID(2, epoch))
	var ticketA, ticketB model.Ticket
	decode(t, records, model.NamespaceTicket+"/"+string(a), &ticketA)
	decode(t, records, model.NamespaceTicket+"/"+string(b), &ticketB)
	if ticketA.Lifecycle != model.LifecycleOpen || ticketA.Gen != 4 || len(ticketA.PRs) != 1 || !ticketA.PRs[0].Merged {
		t.Fatalf("ticket a: %+v", ticketA)
	}
	if !ticketB.Stopped || ticketB.Outcome != model.OutcomeStopped || ticketB.Gen != 2 {
		t.Fatalf("ticket b: %+v", ticketB)
	}
	var ledgerA, ledgerB model.Ledger
	decode(t, records, model.NamespaceLedger+"/"+string(model.TicketLedger(a)), &ledgerA)
	decode(t, records, model.NamespaceLedger+"/"+string(model.TicketLedger(b)), &ledgerB)
	if ledgerA.Spent.MicroUSD != 1_650_000 || len(ledgerA.Reservations) != 1 {
		t.Fatalf("ledger a: %+v", ledgerA)
	}
	// b: the interrupted run charged its whole 2.0 reservation, then 1.9.
	if ledgerB.Spent.MicroUSD != 3_900_000 || !ledgerB.Estimated || len(ledgerB.Reservations) != 0 {
		t.Fatalf("ledger b: %+v", ledgerB)
	}
	var status model.OutboxIntent
	decode(t, records, model.NamespaceOutbox+"/"+string(model.NewOutboxID(b, 0, string(model.CommentID(5, epoch.Add(2*time.Hour))))), &status)
	if status.State != model.EffectCancelled {
		t.Fatalf("status reply for stopped b: %+v", status)
	}
	if _, ok := records[model.NamespaceDecision+"/"+string(model.CommentID(3, epoch))]; !ok {
		t.Fatal("denied mention has no decision")
	}
}

func decode(t *testing.T, records map[string]string, key string, into any) {
	t.Helper()
	raw, ok := records[key]
	if !ok {
		t.Fatalf("record %s missing", key)
	}
	if err := json.Unmarshal([]byte(raw), into); err != nil {
		t.Fatalf("decode %s: %v", key, err)
	}
}
