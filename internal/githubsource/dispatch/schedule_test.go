package dispatch

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/githubsource/model"
	"go.harness.dev/harness/tempest"
)

// TestDispatchSlotsNPlusOneWithRealSessions fills both normal slots with
// blocked workers, then shows an authoritative follow-up of an idle worker
// running in the one human slot while a human-requested launch of a new
// Ticket waits for a normal slot.
func TestDispatchSlotsNPlusOneWithRealSessions(t *testing.T) {
	w := newWorld(t, nil)
	idle := w.mentionEvent(1, "first job")
	idleTicket := ticketOf(idle)
	w.ingest(idle)
	w.settled(model.NewDispatchID(idleTicket, 1))

	gates := map[int]chan struct{}{2: make(chan struct{}), 3: make(chan struct{})}
	var busy []model.TicketID
	for _, number := range []int{2, 3} {
		event := w.mentionEvent(number, fmt.Sprintf("job %d", number))
		w.worker.script(ticketOf(event), gated(gates[number]))
		w.ingest(event)
		busy = append(busy, ticketOf(event))
	}
	w.eventually("both normal slots running", func() bool {
		return len(w.worker.requests(busy[0])) == 1 && len(w.worker.requests(busy[1])) == 1
	})
	waiting := w.mentionEvent(4, "a new job")
	waitingTicket := ticketOf(waiting)
	w.ingest(waiting)

	humanGate := make(chan struct{})
	w.worker.script(idleTicket, gated(humanGate))
	w.ingest(w.commentEvent(1, "please also cover the edge case"))
	continuation := model.NewDispatchID(idleTicket, 2)
	w.eventually("the human continuation running", func() bool { return len(w.worker.requests(idleTicket)) == 2 })
	if dispatch := w.dispatchRecord(continuation); dispatch.Slot != model.SlotHuman || dispatch.State != model.DispatchSubmitted {
		t.Fatalf("continuation = %+v, want submitted in the human slot", dispatch)
	}
	if launch := w.dispatchRecord(model.NewDispatchID(waitingTicket, 1)); launch.State != model.DispatchQueued {
		t.Fatalf("new ticket launch took the human slot: %+v", launch)
	}
	if peak := w.worker.peakConcurrency(); peak != 3 {
		t.Fatalf("peak concurrent workers = %d, want N+1 = 3", peak)
	}

	close(humanGate)
	w.settled(continuation)
	w.wake()
	if launch := w.dispatchRecord(model.NewDispatchID(waitingTicket, 1)); launch.State != model.DispatchQueued {
		t.Fatalf("new ticket launched into the freed human slot: %+v", launch)
	}
	if requests := w.worker.requests(waitingTicket); len(requests) != 0 {
		t.Fatalf("waiting ticket's worker ran: %v", requests)
	}
	close(gates[2])
	launched := w.settled(model.NewDispatchID(waitingTicket, 1))
	if launched.Slot != model.SlotNormal || launched.Outcome == nil || launched.Outcome.StopReason != "completed" {
		t.Fatalf("waiting launch = %+v", launched)
	}
	close(gates[3])
	w.settled(model.NewDispatchID(busy[1], 1))
}

// TestDispatchQueuesAboveHostRunLimit runs two normal slots on a host that
// admits one run: the second submission is rejected at capacity, abandoned
// as transient with backoff, and runs in its existing session once the
// first run ends and the retry time passes.
func TestDispatchQueuesAboveHostRunLimit(t *testing.T) {
	w := newWorld(t, func(w *world) { w.limits = tempest.HostLimits{MaxConcurrentRuns: 1} })
	first := w.mentionEvent(1, "first")
	gate := make(chan struct{})
	w.worker.script(ticketOf(first), gated(gate))
	w.ingest(first)
	w.eventually("the first run", func() bool { return len(w.worker.requests(ticketOf(first))) == 1 })

	second := w.mentionEvent(2, "second")
	secondTicket := ticketOf(second)
	w.ingest(second)
	retried := model.NewDispatchID(secondTicket, 1)
	w.eventually("the rejected submission to requeue", func() bool {
		dispatch := w.dispatchRecord(retried)
		return dispatch.State == model.DispatchQueued && dispatch.Attempts == 1
	})
	dispatch := w.dispatchRecord(retried)
	if !dispatch.RetryAt.After(w.clock.Now()) || dispatch.Reservation.MicroUSD != 0 {
		t.Fatalf("requeued dispatch = %+v", dispatch)
	}
	close(gate)
	w.settled(model.NewDispatchID(ticketOf(first), 1))
	w.clock.Advance(time.Hour)
	w.wake()
	if finished := w.settled(retried); finished.Outcome == nil || finished.Outcome.StopReason != "completed" {
		t.Fatalf("retried dispatch = %+v", finished)
	}
	if sessions := w.sessionsIn(w.worktree(secondTicket)); len(sessions) != 1 {
		t.Fatalf("retry created another session: %+v", sessions)
	}
}

// TestDispatchSteersRunningWorkerAndContinuesRacedInput steers an Input
// into a running worker at its next checkpoint and records the consumption
// with a second receipt. An Input that lands after a later run's last
// checkpoint is continued under the same lease with BeginContinue, not left
// queued, and both runs' usage settles the dispatch.
func TestDispatchSteersRunningWorkerAndContinuesRacedInput(t *testing.T) {
	w := newWorld(t, nil)
	event := w.mentionEvent(3, "start work")
	ticket := ticketOf(event)
	gate := make(chan struct{})
	w.worker.script(ticket, gated(gate), text("saw the note"))
	w.ingest(event)
	w.eventually("the worker run", func() bool { return len(w.worker.requests(ticket)) == 1 })

	w.ingest(w.commentEvent(3, "STEERED-NOTE"))
	w.eventually("the steer to be accepted", func() bool { return w.detail(ticket).Ticket.DeliveredSeq == 2 })
	close(gate)
	launch := w.settled(model.NewDispatchID(ticket, 1))
	requests := w.worker.requests(ticket)
	if launch.ThroughInput != 2 || containsCount(requests, "STEERED-NOTE") != 1 || !strings.Contains(requests[len(requests)-1], "STEERED-NOTE") {
		t.Fatalf("steer not consumed by the run: dispatch %+v requests %v", launch, requests)
	}
	if detail := w.detail(ticket); detail.Ticket.HandledSeq != 2 || detail.Ticket.DispatchSeq != 1 {
		t.Fatalf("ticket after steered run = %+v", detail.Ticket)
	}

	var once sync.Once
	raced := func(point string) {
		if point != "run-ended" {
			return
		}
		once.Do(func() {
			w.ingest(w.commentEvent(3, "RACED-NOTE"))
			w.dispatcherNow().deliver(w.ctx)
		})
	}
	w.onPoint.Store(&raced)
	w.ingest(w.commentEvent(3, "wake up"))
	wake := w.settled(model.NewDispatchID(ticket, 2))
	requests = w.worker.requests(ticket)
	if containsCount(requests, "RACED-NOTE") != 1 || wake.ThroughInput != 4 || wake.Outcome == nil || wake.Outcome.StopReason != "completed" || !wake.Outcome.UsageKnown {
		t.Fatalf("raced input not continued: dispatch %+v requests %v", wake, requests)
	}
	detail := w.detail(ticket)
	if detail.Ticket.HandledSeq != 4 || detail.Ticket.DispatchSeq != 2 {
		t.Fatalf("raced input left for another dispatch: %+v", detail.Ticket)
	}
	session := w.session(detail.Ticket.SessionID)
	woken, err := session.Submission(w.ctx, wake.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	continued, err := session.Submission(w.ctx, model.DeliveryRequestID(ticket, 4, 1))
	if err != nil {
		t.Fatal(err)
	}
	if continued.RunID == woken.RunID {
		t.Fatalf("raced input consumed by the finished run %s", woken.RunID)
	}
	want := microUSD(woken.Result.Usage.Cost.Total) + microUSD(continued.Result.Usage.Cost.Total)
	if wake.Settled == nil || wake.Settled.MicroUSD != want {
		t.Fatalf("settled %+v, want both runs' %d micro-USD", wake.Settled, want)
	}
}

// TestDispatchQueueOverflowDeliversRestLater feeds a running worker more
// Inputs than its steering queue holds. The full queue stops delivery
// without dropping anything: every Input reaches the worker exactly once,
// by steer, continuation or the next wake.
func TestDispatchQueueOverflowDeliversRestLater(t *testing.T) {
	w := newWorld(t, func(w *world) {
		// Each consumed input is a model turn; the run cap must not end
		// the run before the queue is drained.
		w.config.Budgets.Run.MaxCostUSD, w.config.Budgets.TicketUSD = 50, 100
	})
	event := w.mentionEvent(5, "start")
	ticket := ticketOf(event)
	gate := make(chan struct{})
	w.worker.script(ticket, gated(gate))
	w.ingest(event)
	w.eventually("the worker run", func() bool { return len(w.worker.requests(ticket)) == 1 })

	const inputs = 40
	batch := model.IntakeBatch{Resource: "thread:5", ReadAt: w.clock.Now()}
	for i := range inputs {
		batch.Events = append(batch.Events, w.commentEvent(5, fmt.Sprintf("overflow-%02d", i)))
	}
	if _, err := w.store().Ingest(w.ctx, batch); err != nil {
		t.Fatal(err)
	}
	w.wake()
	w.eventually("the queue to fill", func() bool { return w.detail(ticket).Ticket.DeliveredSeq > 1 })
	if delivered := w.detail(ticket).Ticket.DeliveredSeq; delivered >= inputs+1 {
		t.Fatalf("delivered through %d: the queue accepted every input", delivered)
	}
	close(gate)
	w.eventually("every input handled", func() bool {
		detail := w.detail(ticket)
		live := slices.ContainsFunc(detail.Dispatches, func(dispatch model.DispatchIntent) bool {
			return dispatch.State != model.DispatchFinished && dispatch.State != model.DispatchCancelled
		})
		return detail.Ticket.HandledSeq == inputs+1 && !live
	})
	requests := w.worker.requests(ticket)
	for i := range inputs {
		if got := containsCount(requests, fmt.Sprintf("overflow-%02d", i)); got != 1 {
			t.Fatalf("overflow-%02d reached the worker %d times", i, got)
		}
	}
}

// TestDispatchStopCancelsRunAndOwnedJobs stops a Ticket whose worker has a
// background bash job and a blocked model call: the run is cancelled, the
// job is stopped, the dispatch settles aborted and its slot and reservation
// are released.
func TestDispatchStopCancelsRunAndOwnedJobs(t *testing.T) {
	w := newWorld(t, nil)
	event := w.mentionEvent(6, "long task")
	ticket := ticketOf(event)
	gate := make(chan struct{})
	w.worker.script(ticket,
		reply{content: []types.ContentBlock{toolCall("bash", "background-1", map[string]any{"command": "sleep 30", "async": true})}},
		gated(gate))
	w.ingest(event)
	w.eventually("the blocked second call", func() bool { return len(w.worker.requests(ticket)) == 2 })
	session := w.session(w.detail(ticket).Ticket.SessionID)
	if live := liveJobs(t, w, session); live == 0 {
		t.Fatal("background job not running before stop")
	}
	if err := w.store().Operator().Stop(w.ctx, model.OperatorRequest{ID: "stop-1", Target: string(ticket), Reason: "operator stop", At: w.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	w.wake()
	dispatch := w.settled(model.NewDispatchID(ticket, 1))
	if dispatch.State != model.DispatchFinished || dispatch.Outcome == nil || dispatch.Outcome.StopReason != "aborted" {
		t.Fatalf("stopped dispatch = %+v outcome %+v", dispatch, dispatch.Outcome)
	}
	if live := liveJobs(t, w, session); live != 0 {
		t.Fatalf("%d jobs outlived the released slot", live)
	}
	detail := w.detail(ticket)
	if !detail.Ticket.Stopped || len(detail.Ledger.Reservations) != 0 {
		t.Fatalf("ticket %+v ledger %+v", detail.Ticket, detail.Ledger)
	}
	status, err := w.store().Operator().Status(w.ctx)
	if err != nil || status.Occupancy.Normal != 0 || status.Occupancy.Human != 0 {
		t.Fatalf("occupancy after stop = %+v %v", status.Occupancy, err)
	}
	close(gate)
}

func liveJobs(t *testing.T, w *world, session *tempest.Session) int {
	t.Helper()
	page, err := session.Jobs(w.ctx, "", jobPageLimit)
	if err != nil {
		t.Fatal(err)
	}
	live := 0
	for _, job := range page.Jobs {
		if jobLive(job.State) {
			live++
		}
	}
	return live
}
