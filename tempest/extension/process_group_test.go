package extension_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.harness.dev/harness/tempest/extension"
)

// descendantDeadline bounds each wait for the host to end the fixture's
// process group; the fixture's child otherwise lives far longer.
const descendantDeadline = 10 * time.Second

// processPoll is how often waitGone checks whether the child still exists.
const processPoll = 10 * time.Millisecond

// A subprocess extension's process group ends with it: a SIGTERM-ignoring
// child left in the group dies whether the root exits on shutdown or exits
// on its own first, and a child holding the root's stderr cannot keep the
// process admission slot.
func TestProcessExitEndsSameGroupDescendants(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	fixture, err := filepath.Abs("testdata/descendant.py")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"shutdown", "exit"} {
		t.Run(mode, func(t *testing.T) {
			pidPath := filepath.Join(t.TempDir(), "child.pid")
			t.Cleanup(func() { killLeftover(t, pidPath) })
			released := make(chan struct{})
			admit := func(context.Context) (func(), error) { return sync.OnceFunc(func() { close(released) }), nil }
			registry := extension.NewRegistry()
			options := extension.ProcessOptions{ID: "descendant", Argv: []string{python, fixture, mode, pidPath}, Timeout: descendantDeadline, AdmitProcess: admit}
			if err := registry.InstallProcess(context.Background(), options); err != nil {
				t.Fatal(err)
			}
			if mode == "shutdown" {
				if err := registry.Close(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-released:
			case <-time.After(descendantDeadline):
				t.Fatal("root exited but its process slot was never released")
			}
			child, err := childPID(pidPath)
			if err != nil {
				t.Fatal(err)
			}
			waitGone(t, child)
			if err := registry.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func childPID(path string) (int, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(string(encoded))
}

// killLeftover kills the fixture's child if it was started, so a failing
// run never leaves it behind.
func killLeftover(t *testing.T, path string) {
	pid, err := childPID(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Errorf("read child pid: %v", err)
		return
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Errorf("kill leftover child %d: %v", pid, err)
	}
}

// waitGone waits until pid no longer exists. The orphaned child is reaped by
// init, so it disappears shortly after the kill.
func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(descendantDeadline)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("same-group child %d outlived its extension", pid)
		}
		time.Sleep(processPoll)
	}
}
