package control

import (
	"errors"
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
	t.Run("snapshot in the same second as a reopen does not close", func(t *testing.T) {
		h, id, dispatch := setup(t, 851)
		at := epoch.Add(2 * time.Hour)
		h.ingest("reopened", reopened(851, at))
		h.ingestPRs("same-second-closed-snapshot", model.PullRequest{Number: 851, State: "closed", ClosedAt: at, UpdatedAt: at})
		h.finish(dispatch, completed(1))
		if got := h.ticket(id); got.Lifecycle != model.LifecycleOpen {
			t.Fatalf("same-second closed snapshot overrode the reopen: lifecycle=%s outcome=%s", got.Lifecycle, got.Outcome)
		}
	})
	t.Run("snapshot in the same second as a merge does not unmerge", func(t *testing.T) {
		h, id, dispatch := setup(t, 853)
		at := epoch.Add(2 * time.Hour)
		h.ingest("merged", prClosed(853, true, at))
		h.ingestPRs("same-second-open-snapshot", model.PullRequest{Number: 853, State: "open", HeadSHA: "head853", UpdatedAt: at})
		pr, _ := h.store.prs.get("853")
		if !pr.Merged || pr.State != "closed" || pr.MergeSHA != "merge853" || !pr.ClosedAt.Equal(at) {
			t.Fatalf("same-second open snapshot replaced the merge: %+v", *pr)
		}
		if pr.HeadSHA != "head853" {
			t.Fatalf("outranked snapshot dropped its metadata: head %q", pr.HeadSHA)
		}
		h.finish(dispatch, completed(1))
		if got := h.ticket(id); got.Lifecycle != model.LifecycleEnded || got.Outcome != model.OutcomeMerged {
			t.Fatalf("same-second open snapshot erased the merge: lifecycle=%s outcome=%s", got.Lifecycle, got.Outcome)
		}
	})
	t.Run("newer snapshot still replaces the state", func(t *testing.T) {
		h, id, dispatch := setup(t, 855)
		h.ingest("reopened", reopened(855, epoch.Add(time.Hour)))
		h.ingestPRs("newer-closed-snapshot", model.PullRequest{Number: 855, State: "closed", ClosedAt: epoch.Add(2 * time.Hour), UpdatedAt: epoch.Add(2 * time.Hour)})
		h.finish(dispatch, completed(1))
		if got := h.ticket(id); got.Lifecycle != model.LifecycleEnded || got.Outcome != model.OutcomeClosed {
			t.Fatalf("newer closed snapshot ignored: lifecycle=%s outcome=%s", got.Lifecycle, got.Outcome)
		}
	})
}

// Completion is decided once per intake batch, against the state after
// every event and snapshot of the batch is staged. An intermediate state
// must neither end a Ticket nor steer admission of later events.
func TestCompletionOncePerIntakeBatch(t *testing.T) {
	reopened := func(number int, at time.Time) model.Event {
		return model.Event{ID: model.PRReopenedID(number, at), Kind: model.EventPRReopened, Thread: model.ThreadRef{Number: number, IsPR: true}, At: at}
	}
	// quiescent is a Ticket with an open own PR, its dispatch finished and
	// every Input handled: only a PR fact can end it.
	quiescent := func(t *testing.T, number int) (*harness, model.TicketID) {
		h := newHarness(t)
		id, dispatch := h.launched(number-1, int64(number)*10)
		h.openPR(id, "pr", number)
		h.finish(dispatch, completed(1))
		h.at(3 * time.Hour)
		if got := h.ticket(id); got.Lifecycle != model.LifecycleOpen {
			t.Fatalf("setup ended the ticket: %+v", got)
		}
		return h, id
	}
	noise := func(n int) []model.Event {
		events := make([]model.Event, n)
		for i := range events {
			events[i] = comment(999, false, int64(99000+i), mallory, "noise", epoch.Add(time.Hour))
		}
		return events
	}
	t.Run("close then reopen events", func(t *testing.T) {
		h, id := quiescent(t, 961)
		events := []model.Event{prClosed(961, false, epoch.Add(time.Hour)), reopened(961, epoch.Add(2*time.Hour))}
		h.ingestBatch(model.IntakeBatch{Resource: "batch", Events: events, PRs: []model.PullRequest{{Number: 961, State: "open", UpdatedAt: epoch.Add(2 * time.Hour)}}})
		if got := h.ticket(id); got.Lifecycle != model.LifecycleOpen {
			t.Fatalf("intermediate close ended the ticket: lifecycle=%s outcome=%s", got.Lifecycle, got.Outcome)
		}
	})
	t.Run("close then newer open snapshot", func(t *testing.T) {
		h, id := quiescent(t, 963)
		h.ingestBatch(model.IntakeBatch{Resource: "batch", Events: []model.Event{prClosed(963, false, epoch.Add(time.Hour))}, PRs: []model.PullRequest{{Number: 963, State: "open", UpdatedAt: epoch.Add(2 * time.Hour)}}})
		if got := h.ticket(id); got.Lifecycle != model.LifecycleOpen {
			t.Fatalf("intermediate close ended the ticket: lifecycle=%s outcome=%s", got.Lifecycle, got.Outcome)
		}
	})
	t.Run("close at the end of the batch still ends", func(t *testing.T) {
		h, id := quiescent(t, 965)
		events := []model.Event{reopened(965, epoch.Add(time.Hour)), prClosed(965, true, epoch.Add(2*time.Hour))}
		h.ingestBatch(model.IntakeBatch{Resource: "batch", Events: events})
		if got := h.ticket(id); got.Lifecycle != model.LifecycleEnded || got.Outcome != model.OutcomeMerged {
			t.Fatalf("final merge did not end the ticket: lifecycle=%s outcome=%s", got.Lifecycle, got.Outcome)
		}
	})
	t.Run("machine wake after a merge in the same batch is cancelled by the end", func(t *testing.T) {
		h, id := quiescent(t, 975)
		events := []model.Event{prClosed(975, true, epoch.Add(time.Hour)), prCI(975, "head975", nil, epoch.Add(2*time.Hour))}
		h.ingestBatch(model.IntakeBatch{Resource: "batch", Events: events})
		if got := h.ticket(id); got.Lifecycle != model.LifecycleEnded || got.Outcome != model.OutcomeMerged {
			t.Fatalf("merge did not end the ticket: lifecycle=%s outcome=%s", got.Lifecycle, got.Outcome)
		}
		if got := h.admit(); len(got) != 0 {
			t.Fatalf("ended ticket kept a wake from the same batch: %+v", got)
		}
	})
	t.Run("first snapshot of two own PRs does not end", func(t *testing.T) {
		h := newHarness(t)
		id, dispatch := h.launched(966, 9670)
		h.openPR(id, "pr", 967)
		h.openPR(id, "pr2", 968)
		h.finish(dispatch, completed(1))
		h.at(3 * time.Hour)
		h.ingest("second-closed", prClosed(968, false, epoch.Add(time.Hour)))
		if got := h.ticket(id); got.Lifecycle != model.LifecycleOpen {
			t.Fatalf("one of two own PRs closed ended the ticket: %+v", got)
		}
		at := epoch.Add(2 * time.Hour)
		h.ingestPRs("both", model.PullRequest{Number: 967, State: "closed", ClosedAt: at, UpdatedAt: at}, model.PullRequest{Number: 968, State: "open", UpdatedAt: at})
		if got := h.ticket(id); got.Lifecycle != model.LifecycleOpen {
			t.Fatalf("first snapshot ended the ticket before the second reopened its PR: lifecycle=%s outcome=%s", got.Lifecycle, got.Outcome)
		}
	})
	t.Run("mention after a merge in the same batch is not lost", func(t *testing.T) {
		h, id := quiescent(t, 969)
		events := []model.Event{prClosed(969, true, epoch.Add(time.Hour)), mention(968, false, 9691, alice, "one more thing", epoch.Add(2*time.Hour))}
		result := h.ingestBatch(model.IntakeBatch{Resource: "batch", Events: events})
		if got := result.Decisions[1].Kind; got != model.DecisionInput {
			t.Fatalf("mention after the merge was decided against an intermediate end: %s %s", got, result.Decisions[1].Reason)
		}
		got := h.ticket(id)
		if got.Lifecycle != model.LifecycleOpen || got.HandledSeq == got.InputSeq {
			t.Fatalf("mention not delivered to the open ticket: %+v", got)
		}
		wake := h.runAll()
		if len(wake) != 1 {
			t.Fatalf("mention did not wake the ticket: %+v", wake)
		}
		h.report(id, "ready", model.ReportReady)
		h.finish(wake[0].Dispatch.ID, completed(1))
		if got := h.ticket(id); got.Lifecycle != model.LifecycleEnded || got.Outcome != model.OutcomeMerged {
			t.Fatalf("handled mention did not let the merge end the ticket: lifecycle=%s outcome=%s", got.Lifecycle, got.Outcome)
		}
	})
	t.Run("close and reopen in different parts of a split batch", func(t *testing.T) {
		h, id := quiescent(t, 971)
		events := append([]model.Event{prClosed(971, false, epoch.Add(time.Hour))}, noise(model.MaxBatchEvents-1)...)
		events = append(events, reopened(971, epoch.Add(2*time.Hour)))
		h.ingestBatch(model.IntakeBatch{Resource: "split", Events: events})
		if got := h.ticket(id); got.Lifecycle != model.LifecycleOpen {
			t.Fatalf("close in the first part ended the ticket: lifecycle=%s outcome=%s", got.Lifecycle, got.Outcome)
		}
	})
	t.Run("crash between parts settles on the reread", func(t *testing.T) {
		h, id := quiescent(t, 973)
		batch := model.IntakeBatch{Resource: "split", Events: append([]model.Event{prClosed(973, false, epoch.Add(time.Hour))}, noise(model.MaxBatchEvents)...), ReadAt: h.clock.Now()}
		h.store.store = &crashStore{Store: h.store.store, failAt: 2}
		if _, err := h.store.Ingest(h.ctx, batch); !errors.Is(err, errCrash) {
			t.Fatalf("expected crash at the second part: %v", err)
		}
		h.reopen()
		if got := h.ticket(id); got.Lifecycle != model.LifecycleOpen {
			t.Fatalf("first part decided completion alone: %+v", got)
		}
		h.ingestBatch(batch)
		if got := h.ticket(id); got.Lifecycle != model.LifecycleEnded || got.Outcome != model.OutcomeClosed {
			t.Fatalf("reread did not settle the committed close: lifecycle=%s outcome=%s", got.Lifecycle, got.Outcome)
		}
	})
}

func (h *harness) ingestBatch(batch model.IntakeBatch) model.IngestResult {
	h.t.Helper()
	if batch.ReadAt.IsZero() {
		batch.ReadAt = h.clock.Now()
	}
	result, err := h.store.Ingest(h.ctx, batch)
	if err != nil {
		h.t.Fatalf("ingest %s: %v", batch.Resource, err)
	}
	return result
}

func (h *harness) ingestPRs(resource string, prs ...model.PullRequest) model.IngestResult {
	h.t.Helper()
	return h.ingestBatch(model.IntakeBatch{Resource: resource, PRs: prs})
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
