package jobs

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/work"
)

// stdinUntil polls write_stdin until the output gathered so far contains want.
func stdinUntil(t *testing.T, manager *Manager, jobID string, request work.StdinRequest, want string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var gathered strings.Builder
	for {
		result, err := manager.WriteStdin(context.Background(), jobID, request)
		if err != nil {
			t.Fatalf("write_stdin %q: %v (output so far %q)", request.Text, err, gathered.String())
		}
		gathered.WriteString(result.Output)
		if strings.Contains(gathered.String(), want) {
			return gathered.String()
		}
		if time.Now().After(deadline) {
			t.Fatalf("output never contained %q: %q", want, gathered.String())
		}
		request = work.StdinRequest{Wait: time.Second}
	}
}

func TestPTYJobSeesTerminalAndRoundTrips(t *testing.T) {
	host := newHost(t, nil)
	command := `test -t 0 && test -t 1 && test -t 2 && echo tty-yes; stty size; read line; echo "got:$line"`
	job, err := host.manager.StartBash(invocation(1), work.BashSpec{Command: command, TimeoutSeconds: 60, PTY: true})
	if err != nil {
		t.Fatal(err)
	}
	stdinUntil(t, host.manager, job.ID, work.StdinRequest{Wait: 5 * time.Second}, "24 80")
	// The terminal echoes input, and its output discipline ends lines with CRLF.
	output := stdinUntil(t, host.manager, job.ID, work.StdinRequest{Text: "hello\n", Wait: 5 * time.Second}, "got:hello")
	if !strings.Contains(output, "hello\r\n") {
		t.Fatalf("pty output %q lacks the echoed CRLF line", output)
	}
	job = awaitJob(t, host.manager, job.ID)
	if job.State != "completed" || job.ExitCode == nil || *job.ExitCode != 0 {
		t.Fatal(job)
	}
	page, err := host.manager.Logs(context.Background(), job.ID, 0, maxLogPage)
	if err != nil || !strings.Contains(page.Text, "tty-yes") || !strings.Contains(page.Text, "got:hello") {
		t.Fatalf("logs %q err %v", page.Text, err)
	}
}

// TestDefaultJobStdinIsDevNull pins the background-process convention: a job
// started without stdin or pty reads /dev/null, so a command that reads input
// sees end of file at once instead of waiting out its timeout, and write_stdin
// refuses text with the fix while Ctrl-C, a signal, still reaches it.
func TestDefaultJobStdinIsDevNull(t *testing.T) {
	host := newHost(t, nil)
	started := time.Now()
	job, err := host.manager.StartBash(invocation(1), work.BashSpec{Command: "cat; echo got-eof", TimeoutSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	job = awaitJob(t, host.manager, job.ID)
	if elapsed := time.Since(started); job.State != "completed" || *job.ExitCode != 0 || elapsed > 10*time.Second {
		t.Fatalf("job %+v after %s", job, elapsed)
	}
	logs, err := host.manager.Logs(context.Background(), job.ID, 0, maxLogPage)
	if err != nil || logs.Text != "got-eof\n" {
		t.Fatalf("logs %q err %v", logs.Text, err)
	}

	running, err := host.manager.StartBash(invocation(2), work.BashSpec{Command: "echo ready; sleep 60", TimeoutSeconds: 120})
	if err != nil {
		t.Fatal(err)
	}
	result := stdinUntil(t, host.manager, running.ID, work.StdinRequest{Wait: 5 * time.Second}, "ready")
	if result != "ready\n" {
		t.Fatalf("poll = %q", result)
	}
	_, err = host.manager.WriteStdin(context.Background(), running.ID, work.StdinRequest{Text: "hello\n"})
	if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "/dev/null") || !strings.Contains(err.Error(), "stdin: true") || !strings.Contains(err.Error(), "pty: true") {
		t.Fatalf("write to a /dev/null job = %v", err)
	}
	// Its input is already at end of file, so close_stdin has nothing to do.
	closed, err := host.manager.WriteStdin(context.Background(), running.ID, work.StdinRequest{CloseStdin: true})
	if err != nil || !closed.StdinClosed {
		t.Fatalf("close_stdin = %+v, %v", closed, err)
	}
}

func TestPipeJobStdinRoundTripAndCloseStdin(t *testing.T) {
	host := newHost(t, nil)
	job, err := host.manager.StartBash(invocation(1), work.BashSpec{Command: "cat; echo cat-done", TimeoutSeconds: 60, Stdin: true})
	if err != nil {
		t.Fatal(err)
	}
	result, err := host.manager.WriteStdin(context.Background(), job.ID, work.StdinRequest{Text: "one\n", Wait: 5 * time.Second})
	if err != nil || result.Output != "one\n" || result.Offset != 0 || result.NextOffset != 4 {
		t.Fatalf("first write = %+v, %v", result, err)
	}
	result, err = host.manager.WriteStdin(context.Background(), job.ID, work.StdinRequest{Text: "two\n", CloseStdin: true, Wait: 5 * time.Second})
	if err != nil || !strings.HasPrefix(result.Output, "two\n") || result.Offset != 4 || !result.StdinClosed {
		t.Fatalf("closing write = %+v, %v", result, err)
	}
	job = awaitJob(t, host.manager, job.ID)
	if job.State != "completed" || *job.ExitCode != 0 {
		t.Fatal(job)
	}
	// The reader saw EOF, so the command after cat ran; a poll returns what is left.
	result, err = host.manager.WriteStdin(context.Background(), job.ID, work.StdinRequest{})
	if err != nil {
		t.Fatal(err)
	}
	logs, err := host.manager.Logs(context.Background(), job.ID, 0, maxLogPage)
	if err != nil || logs.Text != "one\ntwo\ncat-done\n" || result.NextOffset != int64(len(logs.Text)) {
		t.Fatalf("logs %q err %v, poll %+v", logs.Text, err, result)
	}
	if _, err = host.manager.WriteStdin(context.Background(), job.ID, work.StdinRequest{Text: "late\n"}); !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "exited") {
		t.Fatalf("write after exit = %v", err)
	}
}

func TestWriteStdinAfterCloseStdinIsRefused(t *testing.T) {
	host := newHost(t, nil)
	job, err := host.manager.StartBash(invocation(1), work.BashSpec{Command: "cat >/dev/null; sleep 60", TimeoutSeconds: 120, Stdin: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = host.manager.WriteStdin(context.Background(), job.ID, work.StdinRequest{CloseStdin: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = host.manager.WriteStdin(context.Background(), job.ID, work.StdinRequest{Text: "x"}); !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "close_stdin") {
		t.Fatalf("write after close_stdin = %v", err)
	}
}

// TestWriteStdinCtrlCInterrupts covers Ctrl-C on a pty and on a pipe job
// started without stdin: the pipe job's interrupt is a signal, not input.
func TestWriteStdinCtrlCInterrupts(t *testing.T) {
	for _, pty := range []bool{true, false} {
		t.Run(map[bool]string{true: "pty", false: "pipe"}[pty], func(t *testing.T) {
			host := newHost(t, nil)
			command := `trap 'echo interrupted; exit 7' INT; echo ready; while :; do sleep 0.1; done`
			job, err := host.manager.StartBash(invocation(1), work.BashSpec{Command: command, TimeoutSeconds: 60, PTY: pty})
			if err != nil {
				t.Fatal(err)
			}
			stdinUntil(t, host.manager, job.ID, work.StdinRequest{Wait: 5 * time.Second}, "ready")
			stdinUntil(t, host.manager, job.ID, work.StdinRequest{Text: "\u0003", Wait: 5 * time.Second}, "interrupted")
			job = awaitJob(t, host.manager, job.ID)
			if job.ExitCode == nil || *job.ExitCode != 7 {
				t.Fatal(job)
			}
		})
	}
}

func TestWriteStdinReturnsOnQuietOrWaitAndBoundsOutput(t *testing.T) {
	host := newHost(t, nil)
	quiet, err := host.manager.StartBash(invocation(1), work.BashSpec{Command: "echo hi; sleep 60", TimeoutSeconds: 120})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	result, err := host.manager.WriteStdin(context.Background(), quiet.ID, work.StdinRequest{Wait: 20 * time.Second})
	if err != nil || result.Output != "hi\n" {
		t.Fatalf("quiet poll = %+v, %v", result, err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("quiet output held the call for %s", elapsed)
	}
	// Silence with nothing new waits the whole bound: the program may be working.
	started = time.Now()
	result, err = host.manager.WriteStdin(context.Background(), quiet.ID, work.StdinRequest{Wait: 400 * time.Millisecond})
	if elapsed := time.Since(started); err != nil || result.Output != "" || elapsed < 400*time.Millisecond {
		t.Fatalf("silent poll = %+v, %v after %s", result, err, elapsed)
	}

	noisy, err := host.manager.StartBash(invocation(2), work.BashSpec{Command: "while :; do echo tick; sleep 0.05; done", TimeoutSeconds: 120})
	if err != nil {
		t.Fatal(err)
	}
	started = time.Now()
	result, err = host.manager.WriteStdin(context.Background(), noisy.ID, work.StdinRequest{Wait: 600 * time.Millisecond})
	elapsed := time.Since(started)
	if err != nil || !strings.Contains(result.Output, "tick") || elapsed < 600*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("never-quiet poll = %+v, %v after %s", result, err, elapsed)
	}

	large, err := host.manager.StartBash(invocation(3), work.BashSpec{Command: "head -c 50000 /dev/zero | tr '\\0' a; echo; sleep 60", TimeoutSeconds: 120})
	if err != nil {
		t.Fatal(err)
	}
	result = work.StdinResult{}
	for result.NextOffset < 50001 {
		if result, err = host.manager.WriteStdin(context.Background(), large.ID, work.StdinRequest{Wait: 5 * time.Second}); err != nil {
			t.Fatal(err)
		}
		if len(result.Output) > maxStdinOutputBytes {
			t.Fatalf("returned %d bytes", len(result.Output))
		}
	}
	if result.OmittedBytes == 0 || result.Offset != result.NextOffset-int64(len(result.Output)) {
		t.Fatalf("bounded poll = offset %d next %d omitted %d", result.Offset, result.NextOffset, result.OmittedBytes)
	}
}

func TestWriteStdinDrivesPythonREPL(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	host := newHost(t, nil)
	job, err := host.manager.StartBash(invocation(1), work.BashSpec{Command: "python3 -i -q", TimeoutSeconds: 60, PTY: true})
	if err != nil {
		t.Fatal(err)
	}
	stdinUntil(t, host.manager, job.ID, work.StdinRequest{Wait: 10 * time.Second}, ">>> ")
	stdinUntil(t, host.manager, job.ID, work.StdinRequest{Text: "print(6*7)\n", Wait: 10 * time.Second}, "42")
	if _, err = host.manager.WriteStdin(context.Background(), job.ID, work.StdinRequest{CloseStdin: true}); err != nil {
		t.Fatal(err)
	}
	job = awaitJob(t, host.manager, job.ID)
	if job.State != "completed" || *job.ExitCode != 0 {
		t.Fatal(job)
	}
}

func TestWriteStdinRefusesRecoveredAndNonBashJobs(t *testing.T) {
	host := newHost(t, nil)
	parent := host.manager.host
	jobID, _, digest := Identity(parent.SessionID, "old:0")
	recovery := work.Recovery{
		Job:            work.Job{ID: jobID, Kind: "bash", State: "running", RunID: "old", LogicalRunID: "old", Cwd: parent.Cwd, Cleanup: "none"},
		Invocation:     work.Invocation{RunID: "old", CallID: "old:0"},
		IdentityDigest: digest,
		Bash:           &work.BashSpec{Command: "python3 -i", TimeoutSeconds: 10, PTY: true},
	}
	manager, err := New(parent, Options{}, []work.Recovery{recovery})
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.WriteStdin(context.Background(), jobID, work.StdinRequest{Text: "print(1)\n"})
	if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "never reattaches") || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("recovered pty job = %v", err)
	}
	// A missing job is reported with what to do next: with no jobs, how to
	// start one that takes input; otherwise, the jobs that exist.
	if _, err = host.manager.WriteStdin(context.Background(), "job_missing", work.StdinRequest{}); !errors.Is(err, ErrJobNotFound) || !strings.Contains(err.Error(), "stdin:true") {
		t.Fatal(err)
	}
	if _, err = manager.WriteStdin(context.Background(), "job_missing", work.StdinRequest{}); !errors.Is(err, ErrJobNotFound) || !strings.Contains(err.Error(), jobID) {
		t.Fatal(err)
	}
	if _, err = host.manager.WriteStdin(context.Background(), jobID, work.StdinRequest{Wait: (work.MaxStdinWaitMS + 1) * time.Millisecond}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal(err)
	}
}
