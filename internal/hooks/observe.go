package hooks

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"
)

// maxQueuedObservations bounds observation hooks waiting to run. Past it, a
// slow observer loses later observations; it never holds memory without bound
// or delays the caller.
const maxQueuedObservations = 256

// observationQueue runs observation-only hooks one at a time, in the order
// they were observed, on a goroutine owned by the runner. One worker keeps a
// request's PreModelRequest ahead of its PostModelRequest.
type observationQueue struct {
	mu      sync.Mutex
	pending chan Input
	closed  bool
	done    chan struct{}
	cancel  context.CancelFunc
}

// Observe queues an observation-only hook (PreModelRequest, PostModelRequest)
// and returns without waiting for it. The command gets the usual stdin input
// and its event's timeout; its output is logged and never interpreted, so it
// can't block, rewrite or stop anything. When the queue is full or the
// session has ended, the observation is dropped with a warning.
func (r *Runner) Observe(input Input) {
	if !r.Has(input.Event) {
		return
	}
	input = r.withSession(input)
	queue := &r.observations
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if queue.closed {
		slog.Warn("hook observation dropped: session ended", "event", input.Event)
		return
	}
	if queue.pending == nil {
		var ctx context.Context
		ctx, queue.cancel = context.WithCancel(context.Background())
		queue.pending = make(chan Input, maxQueuedObservations)
		queue.done = make(chan struct{})
		go r.drainObservations(ctx, queue.pending, queue.done)
	}
	select {
	case queue.pending <- input:
	default:
		slog.Warn("hook observation dropped: queue full", "event", input.Event, "limit", maxQueuedObservations)
	}
}

func (r *Runner) drainObservations(ctx context.Context, pending <-chan Input, done chan<- struct{}) {
	defer close(done)
	for input := range pending {
		if ctx.Err() != nil {
			slog.Warn("hook observation dropped: flush timed out", "event", input.Event)
			continue
		}
		r.observe(ctx, input)
	}
}

func (r *Runner) observe(ctx context.Context, input Input) {
	payload, err := json.Marshal(input)
	if err != nil {
		slog.Warn("hook observation not encoded", "event", input.Event, "error", err)
		return
	}
	for _, group := range r.groups[input.Event] {
		for _, command := range group.commands {
			if ctx.Err() != nil {
				return
			}
			report, err := r.executeAdmitted(ctx, command, input.Event, input.Cwd, payload)
			if err != nil {
				slog.Warn("hook observation not run", "event", input.Event, "command", command.Command, "error", err)
				continue
			}
			slog.Info("hook completed", "event", report.Event, "command", report.Command, "exit_code", report.ExitCode, "stdout", report.Stdout, "stderr", report.Stderr, "error", report.Error, "timed_out", report.TimedOut)
		}
	}
}

// FlushObservations stops accepting observations and waits for the queued
// ones to run. The wait is bounded by the longest observation hook timeout
// (or ctx); after that the running command is killed and the rest dropped.
func (r *Runner) FlushObservations(ctx context.Context) {
	if r == nil {
		return
	}
	queue := &r.observations
	queue.mu.Lock()
	if queue.pending != nil && !queue.closed {
		close(queue.pending)
	}
	queue.closed = true
	done, cancel := queue.done, queue.cancel
	queue.mu.Unlock()
	if done == nil {
		return
	}
	deadline := time.NewTimer(r.observationTimeout())
	defer deadline.Stop()
	select {
	case <-done:
		return
	case <-deadline.C:
	case <-ctx.Done():
	}
	cancel()
	<-done
}

// observationTimeout is the longest timeout any observation command has.
func (r *Runner) observationTimeout() time.Duration {
	longest := time.Duration(0)
	for _, event := range []string{"PreModelRequest", "PostModelRequest"} {
		for _, group := range r.groups[event] {
			for _, command := range group.commands {
				timeout := DefaultTimeout(event)
				if command.Timeout > 0 {
					timeout = max(time.Nanosecond, time.Duration(command.Timeout*float64(time.Second)))
				}
				longest = max(longest, timeout)
			}
		}
	}
	return longest
}
