package base

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/retry"
)

const (
	testIdle        = 40 * time.Millisecond
	testFirstOutput = 400 * time.Millisecond
)

func TestWatchdogAllowsLongerSilenceBeforeFirstOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watchdog := WatchStream(cancel, StreamTimeouts{FirstOutput: testFirstOutput, Idle: testIdle})
	defer watchdog.Stop()

	// Gaps several times the idle bound are fine while the model has produced nothing.
	time.Sleep(3 * testIdle)
	watchdog.Event()
	time.Sleep(3 * testIdle)
	if ctx.Err() != nil || watchdog.Stalled() {
		t.Fatal("watchdog fired before the first-output bound")
	}

	// Once output started, the idle bound applies.
	watchdog.Output()
	select {
	case <-ctx.Done():
	case <-time.After(testFirstOutput):
		t.Fatal("watchdog did not fire after output went silent")
	}
	var stall *retry.Error
	if err := watchdog.Err(nil, ctx.Err()); !errors.As(err, &stall) || stall.Code != retry.CodeStreamInterrupted || !stall.Retryable || !strings.Contains(stall.Message, testIdle.String()) {
		t.Fatalf("stall error = %#v", err)
	}
}

func TestWatchdogBoundsSilenceBeforeFirstOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := time.Now()
	watchdog := WatchStream(cancel, StreamTimeouts{FirstOutput: testFirstOutput, Idle: testIdle})
	defer watchdog.Stop()

	select {
	case <-ctx.Done():
	case <-time.After(2 * testFirstOutput):
		t.Fatal("watchdog did not fire")
	}
	if elapsed := time.Since(started); elapsed < testFirstOutput {
		t.Fatalf("fired after %s, before the first-output bound", elapsed)
	}
	if err := watchdog.Err(nil, ctx.Err()); !strings.Contains(err.Error(), testFirstOutput.String()) {
		t.Fatalf("stall error = %v", err)
	}
	// The caller's cancellation is never reported as a stall.
	if err := watchdog.Err(context.Canceled, ctx.Err()); !errors.Is(err, context.Canceled) {
		t.Fatalf("parent cancellation = %v", err)
	}
}

func TestStreamTimeoutsFromEnv(t *testing.T) {
	t.Setenv("HARNESS_STREAM_IDLE_SECONDS", "")
	t.Setenv("HARNESS_STREAM_FIRST_OUTPUT_SECONDS", "")
	if got := StreamTimeoutsFromEnv(); got != (StreamTimeouts{FirstOutput: 5 * time.Minute, Idle: 90 * time.Second}) {
		t.Fatalf("defaults = %+v", got)
	}
	t.Setenv("HARNESS_STREAM_IDLE_SECONDS", "600")
	if got := StreamTimeoutsFromEnv(); got != (StreamTimeouts{FirstOutput: 10 * time.Minute, Idle: 10 * time.Minute}) {
		t.Fatalf("first output shorter than idle = %+v", got)
	}
}
