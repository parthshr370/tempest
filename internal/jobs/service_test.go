package jobs

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/work"
)

// killBound is far below the sleeps the services below would otherwise run for.
const killBound = 10 * time.Second

func TestServiceReadyByLogReleasesTheRunAndCancelsByName(t *testing.T) {
	host := newHost(t, nil)
	started := time.Now()
	job, err := host.manager.StartBash(invocation(1), work.BashSpec{Command: "echo booting; sleep 0.3; echo 'listening on 4123'; sleep 60", TimeoutSeconds: 120, Name: "web", ReadyLog: `listening on \d+`})
	if err != nil {
		t.Fatal(err)
	}
	if job.Readiness != "ready" || job.ReadyMatch != "listening on 4123" || job.Name != "web" {
		t.Fatalf("ready job=%+v", job)
	}
	if elapsed := time.Since(started); elapsed < 300*time.Millisecond {
		t.Fatalf("returned after %v, before the ready line", elapsed)
	}
	// A ready service doesn't hold the run open.
	idleCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if again, err := host.manager.Idle(idleCtx); err != nil || again {
		t.Fatalf("idle with only a ready service=%v %v", again, err)
	}
	named, err := host.manager.JobByName(context.Background(), "web")
	if err != nil || named.ID != job.ID {
		t.Fatalf("by name=%+v %v", named, err)
	}
	if _, err = host.manager.Cancel(context.Background(), named.ID); err != nil {
		t.Fatal(err)
	}
	if final := awaitJob(t, host.manager, job.ID); final.State != "cancelled" || final.Readiness != "ready" {
		t.Fatalf("cancelled service=%+v", final)
	}
	if _, err = host.manager.JobByName(context.Background(), "nope"); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("unknown name=%v", err)
	}
}

func TestServiceReadyByPortWaitsForTheListener(t *testing.T) {
	host := newHost(t, nil)
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	if err = probe.Close(); err != nil {
		t.Fatal(err)
	}
	const listenAfter = 400 * time.Millisecond
	listening := make(chan net.Listener, 1)
	go func() {
		time.Sleep(listenAfter)
		listener, listenErr := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if listenErr != nil {
			t.Error(listenErr)
			listening <- nil
			return
		}
		listening <- listener
	}()
	started := time.Now()
	job, err := host.manager.StartBash(invocation(1), work.BashSpec{Command: "sleep 60", TimeoutSeconds: 120, ReadyPort: port, ReadyTimeoutMS: 10000})
	if listener := <-listening; listener != nil {
		defer listener.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < listenAfter {
		t.Fatalf("ready after %v, before the port listened", elapsed)
	}
	if job.Readiness != "ready" || job.ReadyMatch != "127.0.0.1:"+strconv.Itoa(port) {
		t.Fatalf("ready job=%+v", job)
	}
}

func TestServiceReadinessTimeoutKillsTheProcess(t *testing.T) {
	host := newHost(t, nil)
	started := time.Now()
	_, err := host.manager.StartBash(invocation(1), work.BashSpec{Command: "echo still-starting; sleep 60", TimeoutSeconds: 120, Name: "slow", ReadyLog: "never printed", ReadyTimeoutMS: 300})
	if err == nil || !strings.Contains(err.Error(), "not ready within 300 ms; the process was killed") || !strings.Contains(err.Error(), "still-starting") {
		t.Fatalf("timeout error=%v", err)
	}
	if elapsed := time.Since(started); elapsed > killBound {
		t.Fatalf("readiness failure took %v; the process was not killed", elapsed)
	}
	job, err := host.manager.JobByName(context.Background(), "slow")
	if err != nil {
		t.Fatal(err)
	}
	if final := awaitJob(t, host.manager, job.ID); final.State != "failed" || final.Reason != "ready_timeout" || final.Readiness != "not_ready" {
		t.Fatalf("timed-out service=%+v", final)
	}
	// The failed service released its name.
	if _, err = host.manager.StartBash(invocation(2), work.BashSpec{Command: "sleep 60", TimeoutSeconds: 120, Name: "slow"}); err != nil {
		t.Fatal(err)
	}
}

func TestServiceExitBeforeReadyReportsTheOutputTail(t *testing.T) {
	host := newHost(t, nil)
	_, err := host.manager.StartBash(invocation(1), work.BashSpec{Command: "echo 'address already in use' >&2; exit 3", TimeoutSeconds: 30, ReadyLog: "listening"})
	if err == nil || !strings.Contains(err.Error(), "exited with code 3 before it was ready") || !strings.Contains(err.Error(), "address already in use") {
		t.Fatalf("exit error=%v", err)
	}
}

func TestServiceNameIsUniqueAmongRunningJobsAndRestartable(t *testing.T) {
	host := newHost(t, nil)
	first, err := host.manager.StartBash(invocation(1), work.BashSpec{Command: "sleep 60", TimeoutSeconds: 120, Name: "db"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = host.manager.StartBash(invocation(2), work.BashSpec{Command: "sleep 60", TimeoutSeconds: 120, Name: "db"})
	if !errors.Is(err, ErrNameInUse) || !strings.Contains(err.Error(), first.ID) || !strings.Contains(err.Error(), `cancel {"name":"db"}`) {
		t.Fatalf("duplicate name=%v", err)
	}
	// Restart: cancel, then start the same name again without waiting for the join.
	if _, err = host.manager.Cancel(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	second, err := host.manager.StartBash(invocation(3), work.BashSpec{Command: "sleep 60", TimeoutSeconds: 120, Name: "db"})
	if err != nil {
		t.Fatal(err)
	}
	if named, err := host.manager.JobByName(context.Background(), "db"); err != nil || named.ID != second.ID {
		t.Fatalf("name resolves to %+v %v, want the restarted job", named, err)
	}
	awaitJob(t, host.manager, first.ID)
	// Once nothing runs under the name, it resolves to the latest job.
	if _, err = host.manager.Cancel(context.Background(), second.ID); err != nil {
		t.Fatal(err)
	}
	awaitJob(t, host.manager, second.ID)
	if named, err := host.manager.JobByName(context.Background(), "db"); err != nil || named.ID != second.ID {
		t.Fatalf("finished name resolves to %+v %v", named, err)
	}
}

func TestServiceSpecValidation(t *testing.T) {
	host := newHost(t, nil)
	for _, spec := range []work.BashSpec{
		{Command: "true", ReadyLog: "("},
		{Command: "true", ReadyPort: 70000},
		{Command: "true", ReadyLog: "x", ReadyTimeoutMS: work.MaxReadyTimeoutMS + 1},
		{Command: "true", ReadyTimeoutMS: 100},
		{Command: "true", Name: "bad name"},
		{Command: "true", Name: strings.Repeat("a", maxNameBytes+1)},
	} {
		if _, err := host.manager.StartBash(invocation(1), spec); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("spec %+v: %v", spec, err)
		}
	}
}
