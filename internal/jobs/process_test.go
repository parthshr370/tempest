package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.harness.dev/harness/internal/work"
)

func waitForFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if content, err := os.ReadFile(path); err == nil && len(content) > 0 {
			return string(content)
		}
		select {
		case <-deadline.C:
			t.Fatal("file barrier never reached: ", path)
			return ""
		case <-ticker.C:
		}
	}
}
func TestBashAsyncLogsWaitCancelJoinsGroup(t *testing.T) {
	host := newHost(t, nil)
	pidFile := filepath.Join(t.TempDir(), "descendant.pid")
	command := fmt.Sprintf("printf 'one'; printf '\\377two' >&2; sleep 300 & echo $! > %q; wait", pidFile)
	job, err := host.manager.StartBash(invocation(1), work.BashSpec{Command: command, TimeoutSeconds: 300})
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "starting" {
		t.Fatal(job)
	}
	pidText := waitForFile(t, pidFile)
	descendant, err := strconv.Atoi(strings.TrimSpace(pidText))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		page, logErr := host.manager.Logs(context.Background(), job.ID, 0, 2)
		if logErr != nil {
			t.Fatal(logErr)
		}
		if page.NextOffset == 2 && !page.EOF {
			break
		}
		select {
		case <-deadline:
			t.Fatal("output never became available")
		case <-ticker.C:
		}
	}
	states, err := host.manager.Wait(context.Background(), []string{job.ID}, time.Millisecond)
	if err != nil || terminal(states[0].State) {
		t.Fatal(states, err)
	}
	if _, err = host.manager.Cancel(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	job = awaitJob(t, host.manager, job.ID)
	if job.State != "cancelled" || job.ExitCode == nil || job.Output == nil {
		t.Fatal(job)
	}
	// The known descendant belongs to this test. Clean up even if an assertion exposes a kill regression.
	defer syscall.Kill(descendant, syscall.SIGKILL)
	assertDescendantStopped(t, descendant)
	if job.Output.Bytes != 7 {
		t.Fatal(job.Output)
	}
	var rawOffset int64
	for {
		part, err := host.manager.Logs(context.Background(), job.ID, rawOffset, 2)
		if err != nil {
			t.Fatal(err)
		}
		if part.NextOffset < rawOffset || part.NextOffset > 7 {
			t.Fatal(part)
		}
		rawOffset = part.NextOffset
		if part.EOF {
			break
		}
	}
	if rawOffset != 7 {
		t.Fatal(rawOffset)
	}
}

// TestBashJobOutputKeepsWriteOrderAndTailError covers what the model reads
// from a failing background command: logs interleave stdout and stderr as the
// command wrote them, and result_text keeps the final error, within the
// journal's preview cap, even when the output is larger than the preview.
func TestBashJobOutputKeepsWriteOrderAndTailError(t *testing.T) {
	host := newHost(t, nil)
	command := "for i in $(seq 1 1000); do echo out$i; echo err$i >&2; done; echo 'fatal: tail error' >&2; exit 3"
	job, err := host.manager.StartBash(invocation(1), work.BashSpec{Command: command, TimeoutSeconds: 30})
	if err != nil {
		t.Fatal(err)
	}
	job = awaitJob(t, host.manager, job.ID)
	if job.State != "failed" || job.ExitCode == nil || *job.ExitCode != 3 {
		t.Fatal(job)
	}
	if !strings.HasSuffix(job.ResultText, "err1000\nfatal: tail error\n") || len(job.ResultText) > maxPreviewBytes {
		t.Fatalf("result_text lost the final error or exceeds %d bytes (%d): %q", maxPreviewBytes, len(job.ResultText), job.ResultText)
	}
	var want strings.Builder
	for i := 1; i <= 1000; i++ {
		fmt.Fprintf(&want, "out%d\nerr%d\n", i, i)
	}
	want.WriteString("fatal: tail error\n")
	page, err := host.manager.Logs(context.Background(), job.ID, 0, maxLogPage)
	if err != nil || !page.EOF {
		t.Fatal(page, err)
	}
	if page.Text != want.String() {
		t.Fatalf("logs reordered stdout and stderr:\n%.400s", page.Text)
	}
}
func TestBashHardOutputLimitAndReadersJoin(t *testing.T) {
	host := newHost(t, nil)
	job, err := host.manager.StartBash(invocation(1), work.BashSpec{Command: "/usr/bin/yes x", TimeoutSeconds: 30})
	if err != nil {
		t.Fatal(err)
	}
	job = awaitJob(t, host.manager, job.ID)
	if job.State != "failed" || job.Reason != "output_limit" || job.Output == nil || job.Output.Bytes != maxLogBytes {
		t.Fatal(job)
	}
	page, err := host.manager.Logs(context.Background(), job.ID, 0, 16)
	if err != nil || !page.Truncated {
		t.Fatal(page, err)
	}
}
func TestBashTimeoutAndParentFinishJoin(t *testing.T) {
	host := newHost(t, nil)
	job, err := host.manager.StartBash(invocation(1), work.BashSpec{Command: "sleep 300", TimeoutSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	job = awaitJob(t, host.manager, job.ID)
	if job.State != "failed" || job.Reason != "timeout" {
		t.Fatal(job)
	}
	host.consume()
	job, err = host.manager.StartBash(invocation(2), work.BashSpec{Command: "sleep 300", TimeoutSeconds: 300})
	if err != nil {
		t.Fatal(err)
	}
	if err = host.manager.Finish(context.Background(), "aborted"); err != nil {
		t.Fatal(err)
	}
	job = awaitJob(t, host.manager, job.ID)
	if job.State != "cancelled" {
		t.Fatal(job)
	}
}
func TestBashOutputPathFailureNeverStarts(t *testing.T) {
	host := newHost(t, nil)
	jobID, _, _ := Identity("parent", "message:1")
	path := host.manager.jobPath(jobID, "output.log")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "foreign"), path); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "started")
	job, err := host.manager.StartBash(invocation(1), work.BashSpec{Command: fmt.Sprintf("touch %q", marker), TimeoutSeconds: 30})
	if err != nil {
		t.Fatal(err)
	}
	job = awaitJob(t, host.manager, job.ID)
	if job.State != "failed" || job.Reason != "output_io" {
		t.Fatal(job)
	}
	if _, err = os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("process started despite bad output", err)
	}
}
