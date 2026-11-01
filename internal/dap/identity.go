package dap

import (
	"encoding/json"
	"errors"
	"os"
	"syscall"
)

// attachTarget is the process a local attach names, pinned before the adapter
// starts so cleanup never signals another process that later reuses its pid.
// On Linux the handle holds a pidfd when the kernel provides one, and the
// signal goes through it. Elsewhere, and on Linux without pidfds, the start
// identity is compared immediately before kill, which leaves a short window in
// which the pid could exit and be reused between the check and the signal.
type attachTarget struct {
	pid    int
	start  processStart
	handle *os.Process
}

// processStart is the kernel's record of when a process started. A process
// that reuses a pid reports a different one.
type processStart [2]int64

// pinAttachTarget pins the process named by an attach request's processId. It
// returns nil when the request names no pid or the platform can't establish the
// process's identity; that target then gets no SIGCONT at cleanup.
func pinAttachTarget(processID json.RawMessage) *attachTarget {
	var pid int
	if err := json.Unmarshal(processID, &pid); err != nil || pid <= 0 {
		return nil
	}
	handle, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	start, ok := processStartTime(pid)
	if !ok {
		_ = handle.Release() // A handle for a process we won't signal; nothing depends on closing it.
		return nil
	}
	return &attachTarget{pid: pid, start: start, handle: handle}
}

// resume sends SIGCONT if the pid still names the pinned process, then releases
// the handle. A target that has exited, or whose pid now names another
// process, is left alone.
func (target *attachTarget) resume() error {
	var signalErr error
	if start, ok := processStartTime(target.pid); ok && start == target.start {
		signalErr = target.handle.Signal(syscall.SIGCONT)
		if errors.Is(signalErr, os.ErrProcessDone) {
			signalErr = nil
		}
	}
	return errors.Join(signalErr, target.release())
}

// release drops the handle of a target that is never resumed.
func (target *attachTarget) release() error {
	if target == nil {
		return nil
	}
	return target.handle.Release()
}
