package dispatch

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/githubsource/model"
)

const reportTool = "ext__" + model.ExtensionID + "__" + model.ToolReport

func launchText(ticket model.TicketID) string {
	return "GitHub ticket " + string(ticket) + " in acme/widgets"
}

// TestDispatchLaunchRunsWorkerOnTicketBranch runs one Ticket end to end with
// a real Host session: the worktree is a real checkout of agent/<ticket>,
// the identity document is in the session before the first provider request
// (the test extension's section renders the Ticket from it), the run budget
// is the reservation, and the worker's ready report plus the completed run
// end the Ticket with the run's real usage settled.
func TestDispatchLaunchRunsWorkerOnTicketBranch(t *testing.T) {
	w := newWorld(t, func(w *world) { w.config.Budgets.TicketUSD = 1.5 })
	event := w.mentionEvent(7, "please fix the widget")
	ticket := ticketOf(event)
	gate := make(chan struct{})
	w.worker.script(ticket, gated(gate, toolCall(reportTool, "report-1", map[string]string{"status": "ready", "summary": "fixed"})), text("all done"))
	w.ingest(event)

	launch := model.NewDispatchID(ticket, 1)
	w.eventually("the worker's first provider request", func() bool { return len(w.worker.requests(ticket)) == 1 })
	if stray := w.worker.requests(""); len(stray) != 0 {
		t.Fatalf("a provider request ran before the identity document: %v", stray)
	}
	if first := w.worker.requests(ticket)[0]; !strings.Contains(first, launchText(ticket)) || !strings.Contains(first, "please fix the widget") {
		t.Fatalf("launch prompt = %s", first)
	}
	detail := w.detail(ticket)
	if detail.Ticket.SessionID == "" || detail.Ticket.Worktree != w.worktree(ticket) || !detail.Ticket.Prepared {
		t.Fatalf("ticket = %+v", detail.Ticket)
	}
	if head := gitRun(t, w.worktree(ticket), "symbolic-ref", "HEAD"); head != "refs/heads/"+model.TicketBranch(ticket) {
		t.Fatalf("worktree HEAD = %s", head)
	}
	if base := gitRun(t, w.remote, "rev-parse", "main"); detail.Ticket.BaseSHA != base {
		t.Fatalf("base = %s, want %s", detail.Ticket.BaseSHA, base)
	}
	sessions := w.sessionsIn(w.worktree(ticket))
	if len(sessions) != 1 || sessions[0].SessionID != detail.Ticket.SessionID {
		t.Fatalf("sessions in worktree = %+v, bound %s", sessions, detail.Ticket.SessionID)
	}
	session := w.session(detail.Ticket.SessionID)
	record, granted, err := readDocument(w.ctx, session)
	if err != nil || !granted {
		t.Fatalf("worker document: %v granted=%v", err, granted)
	}
	var document model.WorkerTicket
	if err := json.Unmarshal(record.Value, &document); err != nil {
		t.Fatal(err)
	}
	if document.Ticket != ticket || document.Branch != model.TicketBranch(ticket) || document.Worktree != w.worktree(ticket) || document.Mandate.Actor.ID != parth.ID || !strings.Contains(document.Request, "please fix the widget") {
		t.Fatalf("worker document = %+v", document)
	}
	reserved := w.dispatchRecord(launch).Reservation
	budget := session.Snapshot().Budget
	if reserved.MicroUSD != 1_500_000 || budget.MaxCostUSD != 1.5 || budget.MaxTurns != 60 || budget.MaxToolCalls != 300 {
		t.Fatalf("session budget %+v, reservation %+v", budget, reserved)
	}

	close(gate)
	dispatch := w.settled(launch)
	if dispatch.State != model.DispatchFinished || dispatch.Outcome == nil || dispatch.Outcome.StopReason != "completed" || !dispatch.Outcome.UsageKnown {
		t.Fatalf("dispatch = %+v", dispatch)
	}
	// Settling closed the idle worker; its receipts are read by reopening.
	session = w.session(detail.Ticket.SessionID)
	submission, err := session.Submission(w.ctx, dispatch.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if want := microUSD(submission.Result.Usage.Cost.Total); want == 0 || dispatch.Settled == nil || dispatch.Settled.MicroUSD != want {
		t.Fatalf("settled %+v, run cost %d micro-USD", dispatch.Settled, want)
	}
	detail = w.detail(ticket)
	if detail.Ticket.Lifecycle != model.LifecycleEnded || detail.Ticket.Outcome != model.OutcomeCompleted {
		t.Fatalf("ticket after ready report = %+v", detail.Ticket)
	}
	if detail.Ledger.Spent.MicroUSD != dispatch.Settled.MicroUSD || len(detail.Ledger.Reservations) != 0 {
		t.Fatalf("ticket ledger = %+v", detail.Ledger)
	}
}

// TestDispatchRestartAtEveryStep kills the process at each dispatcher step
// and restarts it. Before the submission exists the restarted dispatcher
// adopts the worktree and session and launches exactly once; once the
// worker run may have started, its receipt (never a second submission)
// proves the run unknown, the whole reservation is charged and a recovery
// generation runs in the same session.
func TestDispatchRestartAtEveryStep(t *testing.T) {
	for _, point := range []string{"admitted", "worktree", "created", "document", "submit", "running"} {
		t.Run(point, func(t *testing.T) {
			w := newWorld(t, nil)
			event := w.mentionEvent(9, "repair the gears")
			ticket := ticketOf(event)
			// Submit returns with the launch run started, and nothing
			// orders its provider reply after the crash point: a reply
			// that lands first completes the run, and its receipt then
			// rightly settles the launch completed. The first reply is
			// held until the process is dead. The run may also die
			// before it asks, leaving the reply to the recovery, so the
			// gate opens once the restarted process is up.
			gate := make(chan struct{})
			if point == "submit" || point == "running" {
				w.worker.script(ticket, gated(gate))
			}
			w.crashAt.Store(point)
			w.ingest(event)
			w.eventually("the crash at "+point, w.crashed.Load)
			w.restart()
			close(gate)

			launch := w.settled(model.NewDispatchID(ticket, 1))
			sessions := w.sessionsIn(w.worktree(ticket))
			detail := w.detail(ticket)
			if len(sessions) != 1 || sessions[0].SessionID != detail.Ticket.SessionID {
				t.Fatalf("sessions in worktree = %+v, bound %q", sessions, detail.Ticket.SessionID)
			}
			requests := w.worker.requests(ticket)
			launches := containsCount(requests, launchText(ticket))
			switch point {
			case "submit", "running":
				if launches > 1 || launch.Outcome == nil || launch.Outcome.StopReason != stopUnknown {
					t.Fatalf("interrupted launch: %d launch requests, dispatch %+v", launches, launch)
				}
				if launch.Settled == nil || launch.Settled.MicroUSD != launch.Reservation.MicroUSD || !detail.Ledger.Estimated {
					t.Fatalf("unknown run settled %+v of reservation %+v, ledger %+v", launch.Settled, launch.Reservation, detail.Ledger)
				}
				recovery := w.settled(model.NewDispatchID(ticket, 2))
				if recovery.Kind != model.DispatchRecovery || recovery.Gen != 2 || recovery.Outcome == nil || recovery.Outcome.StopReason != "completed" {
					t.Fatalf("recovery dispatch = %+v", recovery)
				}
				if got := containsCount(w.worker.requests(ticket), "Restart notice for GitHub ticket "+string(ticket)); got != 1 {
					t.Fatalf("recovery prompt sent %d times", got)
				}
			default:
				if launches != 1 || launch.Outcome == nil || launch.Outcome.StopReason != "completed" || launch.Gen != 1 {
					t.Fatalf("launch after restart: %d launch requests, dispatch %+v", launches, launch)
				}
			}
			assertDocument(t, w, detail.Ticket.SessionID, ticket)
		})
	}
}

func assertDocument(t *testing.T, w *world, sessionID string, ticket model.TicketID) {
	t.Helper()
	owner, granted, err := documentOwner(w.ctx, w.session(sessionID))
	if err != nil || !granted || owner != ticket {
		t.Fatalf("session %s document owner %q granted=%v err=%v", sessionID, owner, granted, err)
	}
}

// TestDispatchAdoptsOldestOrphanAndRetiresTheRest prepares the crash window
// of repeated failed creates: two never-run worker sessions without a
// document in the Ticket's worktree. The dispatcher adopts the oldest,
// retires the other and creates none.
func TestDispatchAdoptsOldestOrphanAndRetiresTheRest(t *testing.T) {
	w := newWorld(t, nil)
	event := w.mentionEvent(11, "tidy the docs")
	ticket := ticketOf(event)
	dispatcher := w.dispatcherNow()
	if _, err := ensureCheckout(w.ctx, dispatcher.branchSpec(ticket)); err != nil {
		t.Fatal(err)
	}
	first, err := dispatcher.createSession(w.ctx, w.worktree(ticket))
	if err != nil {
		t.Fatal(err)
	}
	second, err := dispatcher.createSession(w.ctx, w.worktree(ticket))
	if err != nil {
		t.Fatal(err)
	}
	w.ingest(event)
	w.settled(model.NewDispatchID(ticket, 1))
	detail := w.detail(ticket)
	if detail.Ticket.SessionID != first.ID() {
		t.Fatalf("bound %s, want the oldest orphan %s (other %s)", detail.Ticket.SessionID, first.ID(), second.ID())
	}
	if sessions := w.sessionsIn(w.worktree(ticket)); len(sessions) != 2 {
		t.Fatalf("a worker session was created beside the orphans: %+v", sessions)
	}
	owner, granted, err := documentOwner(w.ctx, w.session(second.ID()))
	if err != nil || !granted || owner != "" {
		t.Fatalf("retired orphan document owner %q granted=%v err=%v", owner, granted, err)
	}
	if last := w.session(second.ID()).Snapshot().LastRun; last != nil {
		t.Fatalf("retired orphan ran: %+v", last)
	}
	assertDocument(t, w, first.ID(), ticket)
}

// TestDispatchRecoveryRequiredSteersUnderSameRequest kills the process
// while the worker's delegated child runs. After restart the launch is
// unknown and the worker session needs child recovery, so a fresh prompt
// is refused with ErrRecoveryRequired; the recovery dispatch's prompt goes
// in as a steer under its own request ID, BeginContinue recovers the child
// and consumes it, and the dispatch settles from that receipt.
func TestDispatchRecoveryRequiredSteersUnderSameRequest(t *testing.T) {
	w := newWorld(t, func(w *world) { w.enableTask = true })
	event := w.mentionEvent(13, "investigate with a helper")
	ticket := ticketOf(event)
	childGate := make(chan struct{})
	defer close(childGate)
	w.worker.script(childScript, gated(childGate))
	w.worker.script(ticket, reply{content: []types.ContentBlock{toolCall("task", "task-1", map[string]string{"prompt": "inspect the gears", "subagent_type": "explore"})}})
	w.ingest(event)
	w.eventually("the delegated child running", func() bool { return len(w.worker.requests(childScript)) == 1 })
	w.crash()

	var sawRecovering atomic.Bool
	inspect := func(point string) {
		if point != "admitted" {
			return
		}
		bound, err := w.store().Ticket(w.ctx, ticket)
		if err != nil || bound.SessionID == "" {
			return
		}
		session, err := w.currentHost().Open(w.ctx, bound.SessionID)
		if err != nil {
			return
		}
		for _, job := range session.Snapshot().Jobs {
			if job.State == "recovering" {
				sawRecovering.Store(true)
			}
		}
	}
	w.onPoint.Store(&inspect)
	w.restart()

	launch := w.settled(model.NewDispatchID(ticket, 1))
	if launch.Outcome == nil || launch.Outcome.StopReason != stopUnknown {
		t.Fatalf("interrupted launch = %+v", launch)
	}
	recovery := w.settled(model.NewDispatchID(ticket, 2))
	if recovery.Kind != model.DispatchRecovery || recovery.Outcome == nil || recovery.Outcome.StopReason != "completed" {
		t.Fatalf("recovery dispatch = %+v", recovery)
	}
	if !sawRecovering.Load() {
		t.Fatal("the restarted worker session never needed child recovery")
	}
	session := w.session(w.detail(ticket).Ticket.SessionID)
	receipt, err := session.Submission(w.ctx, recovery.RequestID)
	if err != nil || receipt.ID != recovery.SubmissionID || receipt.State != submissionDone {
		t.Fatalf("recovery receipt %+v err %v, dispatch submission %s", receipt, err, recovery.SubmissionID)
	}
	if got := containsCount(w.worker.requests(ticket), "Restart notice for GitHub ticket "+string(ticket)); got != 1 {
		t.Fatalf("recovery prompt reached the worker %d times", got)
	}
}
