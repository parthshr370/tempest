package base

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.harness.dev/harness/internal/retry"
)

// StreamTimeouts bound how long a provider stream may go without an SSE data
// event before the request is abandoned as stalled. Keep-alive comments do not
// count: they show that the connection is open, not that the model is
// producing, and a proxy keeps sending them for an upstream that has hung.
type StreamTimeouts struct {
	// FirstOutput applies until the model's first content (text, thinking or a
	// tool call). It covers provider queueing and, for models that do not
	// stream their reasoning, the whole silent reasoning phase.
	FirstOutput time.Duration
	// Idle applies once content has started. A generating model streams
	// tokens continuously, so a gap this long means the stream stalled.
	Idle time.Duration
}

const (
	defaultStreamIdle        = 90 * time.Second
	defaultStreamFirstOutput = 5 * time.Minute
)

// StreamTimeoutsFromEnv reads HARNESS_STREAM_IDLE_SECONDS (default 90) and
// HARNESS_STREAM_FIRST_OUTPUT_SECONDS (default 300). The first-output bound is
// never shorter than the idle bound.
func StreamTimeoutsFromEnv() StreamTimeouts {
	timeouts := StreamTimeouts{
		FirstOutput: envSeconds("HARNESS_STREAM_FIRST_OUTPUT_SECONDS", defaultStreamFirstOutput),
		Idle:        envSeconds("HARNESS_STREAM_IDLE_SECONDS", defaultStreamIdle),
	}
	timeouts.FirstOutput = max(timeouts.FirstOutput, timeouts.Idle)
	return timeouts
}

func envSeconds(name string, fallback time.Duration) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name))); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return fallback
}

// Watchdog cancels a provider request whose stream stays silent past its
// [StreamTimeouts] bound. Without it a stream that stops sending but never
// closes would hold the run forever.
type Watchdog struct {
	timeouts StreamTimeouts
	activity chan struct{}
	done     chan struct{}
	output   atomic.Bool
	stall    atomic.Pointer[retry.Error]
}

// WatchStream starts a watchdog that calls cancel when the stream goes silent.
// The clock starts now, so waiting for response headers counts as silence
// before the first output. Call Stop when the request ends.
func WatchStream(cancel context.CancelFunc, timeouts StreamTimeouts) *Watchdog {
	watchdog := &Watchdog{timeouts: timeouts, activity: make(chan struct{}, 1), done: make(chan struct{})}
	go watchdog.run(cancel)
	return watchdog
}

// Event records an SSE data event. It never blocks the reader: one pending
// signal is enough to restart the clock.
func (watchdog *Watchdog) Event() {
	select {
	case watchdog.activity <- struct{}{}:
	default:
	}
}

// Output records that the model produced content; from here on the shorter
// idle bound applies.
func (watchdog *Watchdog) Output() {
	if !watchdog.output.Swap(true) {
		watchdog.Event()
	}
}

// Stop ends the watchdog.
func (watchdog *Watchdog) Stop() { close(watchdog.done) }

// Stalled reports whether the watchdog cancelled the request.
func (watchdog *Watchdog) Stalled() bool { return watchdog.stall.Load() != nil }

// Err picks the error a provider reports for a failed request: the caller's
// cancellation as is, a stall as a retryable stream interruption, anything
// else unchanged. The watchdog cancels the request through its own context, so
// the transport error after a stall is a bare context cancellation that would
// otherwise read as the caller's.
func (watchdog *Watchdog) Err(parentErr, err error) error {
	if parentErr != nil {
		return parentErr
	}
	if stall := watchdog.stall.Load(); stall != nil {
		return stall
	}
	return err
}

func (watchdog *Watchdog) run(cancel context.CancelFunc) {
	timer := time.NewTimer(watchdog.bound())
	defer timer.Stop()
	for {
		select {
		case <-watchdog.activity:
			timer.Reset(watchdog.bound())
		case <-timer.C:
			message := fmt.Sprintf("stream idle for %s (no events)", watchdog.timeouts.Idle)
			if !watchdog.output.Load() {
				message = fmt.Sprintf("stream idle for %s before the first output (no events)", watchdog.timeouts.FirstOutput)
			}
			watchdog.stall.Store(&retry.Error{Code: retry.CodeStreamInterrupted, Message: message, Retryable: true})
			cancel()
			return
		case <-watchdog.done:
			return
		}
	}
}

func (watchdog *Watchdog) bound() time.Duration {
	if watchdog.output.Load() {
		return watchdog.timeouts.Idle
	}
	return watchdog.timeouts.FirstOutput
}
