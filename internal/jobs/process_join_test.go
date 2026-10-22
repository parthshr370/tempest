package jobs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.harness.dev/harness/internal/work"
)

// TestEscapedProcessHelper runs only in a child test binary with inherited pipes.
func TestEscapedProcessHelper(t *testing.T) {
	switch os.Getenv("TEMPEST_PIPE_HELPER") {
	case "launcher":
		descendant := exec.Command(os.Args[0], "-test.run=^TestEscapedProcessHelper$")
		descendant.Env = append(os.Environ(), "TEMPEST_PIPE_HELPER=escaped")
		descendant.Stdout = os.Stdout
		descendant.Stderr = os.Stderr
		descendant.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := descendant.Start(); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	case "escaped":
		if err := os.WriteFile(os.Getenv("TEMPEST_PIPE_PID"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
			os.Exit(2)
		}
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
}

func TestEscapedDescendantCannotHoldReadersBeyondWaitDelay(t *testing.T) {
	host := newHost(t, nil)
	pidFile := filepath.Join(t.TempDir(), "escaped.pid")
	command := fmt.Sprintf("TEMPEST_PIPE_HELPER=launcher TEMPEST_PIPE_PID=%q %q -test.run='^TestEscapedProcessHelper$'", pidFile, os.Args[0])
	started := time.Now()
	job, err := host.manager.StartBash(invocation(1), work.BashSpec{Command: command, TimeoutSeconds: 20})
	if err != nil {
		t.Fatal(err)
	}
	pidText := waitForFile(t, pidFile)
	pid, err := strconv.Atoi(strings.TrimSpace(pidText))
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	job = awaitJob(t, host.manager, job.ID)
	if time.Since(started) > 6*time.Second {
		t.Fatal("escaped pipe delayed owned readers beyond WaitDelay")
	}
	if job.ExitCode == nil || *job.ExitCode != 0 {
		t.Fatal(job)
	}
	if _, err = host.manager.Logs(context.Background(), job.ID, 0, 1); err != nil {
		t.Fatal(err)
	}
}

func TestBashAndTaskTotalQuota(t *testing.T) {
	host := newHost(t, func(ctx context.Context, _ work.TaskLaunch) (work.TaskResult, error) {
		<-ctx.Done()
		return work.TaskResult{}, ctx.Err()
	})
	for index := range maxActiveKind {
		if _, err := host.manager.StartTask(invocation(index), work.TaskSpec{Prompt: "work", SubagentType: "explore"}); err != nil {
			t.Fatal(err)
		}
		if _, err := host.manager.StartBash(invocation(index+maxActiveKind), work.BashSpec{Command: "sleep 300", TimeoutSeconds: 300}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := host.manager.StartBash(invocation(20), work.BashSpec{Command: "echo not-run", TimeoutSeconds: 10}); err != ErrJobLimit {
		t.Fatal(err)
	}
	if err := host.manager.Finish(context.Background(), "aborted"); err != nil {
		t.Fatal(err)
	}
	for _, job := range host.manager.Snapshot() {
		if job.State != "cancelled" {
			t.Fatal(job)
		}
	}
}

func assertDescendantStopped(t *testing.T, pid int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		command := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "stat=")
		status, err := command.Output()
		if err != nil || strings.HasPrefix(strings.TrimSpace(string(status)), "Z") {
			return
		}
		select {
		case <-deadline:
			t.Fatal("descendant still executing after group cancellation")
		case <-ticker.C:
		}
	}
}
