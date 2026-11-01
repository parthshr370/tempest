package dap

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// signalRecorder is a process that appends the name of each SIGCONT or SIGUSR1
// it receives to a file, so a test can see which signals reached it.
type signalRecorder struct {
	command  *exec.Cmd
	received string
}

func startSignalRecorder(t *testing.T) *signalRecorder {
	t.Helper()
	directory := t.TempDir()
	received := filepath.Join(directory, "received")
	ready := filepath.Join(directory, "ready")
	script := `trap 'echo cont >> "$1"' CONT; trap 'echo usr1 >> "$1"' USR1; : > "$2"; while :; do sleep 0.05; done`
	command := exec.Command("/bin/sh", "-c", script, "recorder", received, ready)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	waitDAPCondition(t, "signal recorder didn't install its traps", func() bool {
		_, err := os.Stat(ready)
		return err == nil
	})
	return &signalRecorder{command: command, received: received}
}

func (recorder *signalRecorder) signals() string {
	content, _ := os.ReadFile(recorder.received)
	return string(content)
}

// requireNoSIGCONT fails if recorder got a SIGCONT. Signals pend in the
// kernel as kill returns, and the shell runs every pending trap together, so
// a SIGCONT sent before this sentinel is recorded no later than it.
func (recorder *signalRecorder) requireNoSIGCONT(t *testing.T) {
	t.Helper()
	if err := recorder.command.Process.Signal(syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	waitDAPCondition(t, "recorder didn't record the sentinel signal", func() bool {
		return strings.Contains(recorder.signals(), "usr1")
	})
	if strings.Contains(recorder.signals(), "cont") {
		t.Fatalf("SIGCONT reached the recorder: %q", recorder.signals())
	}
}

// Cleanup continues an attach target only once the adapter confirmed the
// attach: an adapter that dies before answering, or refuses the attach, never
// stopped the target, which may be a process its user stopped on purpose.
// The adapter's exit observer runs cleanup concurrently with Start, and the
// target is pinned before that observer exists.
func TestDAPAttachContinuesOnlyAConfirmedTarget(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	adapters := map[string]Adapter{
		"exiting": {Command: "/bin/sh", Args: []string{"-c", "exit 3"}},
		"fake":    {Command: executable, Args: []string{"-test.run=^TestDAPAdapterProcess$", "--", "dap-helper", filepath.Join(t.TempDir(), "sequence")}},
	}
	for _, testCase := range []struct {
		name, adapter string
		arguments     map[string]json.RawMessage
		confirmed     bool
	}{
		{"adapter exits before answering", "exiting", nil, false},
		{"adapter refuses the attach", "fake", map[string]json.RawMessage{"fail": json.RawMessage(`true`)}, false},
		{"adapter confirms the attach", "fake", nil, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := startSignalRecorder(t)
			directory := t.TempDir()
			owner, err := New(directory, directory, adapters, nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			arguments := map[string]json.RawMessage{"processId": json.RawMessage(strconv.Itoa(recorder.command.Process.Pid))}
			maps.Copy(arguments, testCase.arguments)
			_, startErr := owner.Start(ctx, "attach", testCase.adapter, "", arguments)
			if (startErr == nil) != testCase.confirmed {
				t.Fatalf("attach error = %v", startErr)
			}
			if err := owner.Close(); err != nil {
				t.Fatal(err)
			}
			if !testCase.confirmed {
				recorder.requireNoSIGCONT(t)
				return
			}
			waitDAPCondition(t, "a confirmed attach target didn't get SIGCONT after its adapter exited", func() bool {
				return strings.Contains(recorder.signals(), "cont")
			})
		})
	}
}

// A reused pid names a different process; the attach target pinned at attach
// time must not signal it.
func TestDAPAttachTargetSkipsChangedIdentity(t *testing.T) {
	recorder := startSignalRecorder(t)
	processID := json.RawMessage(strconv.Itoa(recorder.command.Process.Pid))
	reused := pinAttachTarget(processID)
	if reused == nil {
		t.Fatal("couldn't pin the identity of a live process")
	}
	// Model pid reuse: the pid now belongs to a process that started at
	// another time.
	reused.start[0]++
	if err := reused.resume(); err != nil {
		t.Fatal(err)
	}
	recorder.requireNoSIGCONT(t)

	same := pinAttachTarget(processID)
	if same == nil {
		t.Fatal("couldn't pin the identity of a live process")
	}
	if err := same.resume(); err != nil {
		t.Fatal(err)
	}
	waitDAPCondition(t, "SIGCONT didn't reach the pinned process", func() bool {
		return strings.Contains(recorder.signals(), "cont")
	})
}

func TestDAPAttachTargetToleratesExitedProcess(t *testing.T) {
	command := exec.Command("/bin/sh", "-c", "exec sleep 30")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	target := pinAttachTarget(json.RawMessage(strconv.Itoa(command.Process.Pid)))
	if target == nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal("couldn't pin the identity of a live process")
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	if err := target.resume(); err != nil {
		t.Fatalf("resuming a reaped attach target: %v", err)
	}
}
