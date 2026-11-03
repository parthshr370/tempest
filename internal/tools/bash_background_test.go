package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A foreground command that starts a child with & succeeds with its output
// when its shell exits 0, and the child ends with the command instead of
// outliving it unseen by jobs and cancel.
func TestBashBackgroundChildEndsWithTheCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups require Unix")
	}
	cwd := t.TempDir()
	tool := newBashTool(cwd, ToolsOptions{})
	started := time.Now()
	result, err := tool.Execute(context.Background(), "bash", json.RawMessage(`{"command":"sleep 30 & printf '%s' $! > child.pid; echo started"}`), nil)
	elapsed := time.Since(started)
	data, readErr := os.ReadFile(filepath.Join(cwd, "child.pid"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	pid, readErr := strconv.Atoi(strings.TrimSpace(string(data)))
	if readErr != nil {
		t.Fatal(readErr)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if err != nil {
		t.Fatalf("exit 0 reported as failure: %v", err)
	}
	text := resultText(result)
	if !strings.Contains(text, "started") || !strings.Contains(text, strayProcessesKilled) {
		t.Fatalf("result = %q", text)
	}
	if elapsed >= bashPipeJoinDelay {
		t.Fatalf("command returned after %s: the background child held its output", elapsed)
	}
	assertProcessStopped(t, pid)
}

// A command that leaves nothing running gets no background note.
func TestBashWithoutBackgroundChildHasNoNote(t *testing.T) {
	tool := newBashTool(t.TempDir(), ToolsOptions{})
	result, err := tool.Execute(context.Background(), "bash", json.RawMessage(`{"command":"echo done"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if text := resultText(result); strings.Contains(text, strayProcessesKilled) {
		t.Fatalf("result = %q", text)
	}
}

// assertProcessStopped waits for pid to stop executing; a zombie its new
// parent has not reaped yet counts as stopped.
func assertProcessStopped(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		status, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "stat=").Output()
		if err != nil || strings.HasPrefix(strings.TrimSpace(string(status)), "Z") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("background child %d still running after the command ended", pid)
		}
		time.Sleep(time.Millisecond)
	}
}
