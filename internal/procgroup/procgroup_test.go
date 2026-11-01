package procgroup

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func startGroup(t *testing.T, name string, args ...string) (*exec.Cmd, io.ReadCloser) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	return cmd, stdout
}

func TestSignalLiveGroup(t *testing.T) {
	cmd, _ := startGroup(t, "sleep", "30")
	if err := Signal(cmd.Process.Pid, 0); err != nil {
		t.Fatalf("probe of a live group = %v", err)
	}
	if err := Signal(cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill of a live group = %v", err)
	}
}

// An exited leader that nobody has reaped yet is the only member of its group.
// Darwin refuses to signal that group with EPERM; Signal must report it done.
func TestSignalUnreapedGroup(t *testing.T) {
	cmd, stdout := startGroup(t, "true")
	if _, err := io.Copy(io.Discard, stdout); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	if runtime.GOOS == "darwin" {
		// EOF arrives while the child is still exiting; wait until the kernel
		// counts no live member, which is the state that broke cleanup.
		deadline := time.Now().Add(5 * time.Second)
		for !errors.Is(syscall.Kill(-pgid, 0), syscall.EPERM) {
			if time.Now().After(deadline) {
				t.Fatal("unreaped group never reached Darwin's EPERM state")
			}
			time.Sleep(time.Millisecond)
		}
	}
	for _, sig := range []syscall.Signal{0, syscall.SIGKILL} {
		if err := Signal(pgid, sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Fatalf("signal %d to an unreaped group = %v", sig, err)
		}
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := Signal(pgid, syscall.SIGKILL); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("kill of a reaped group = %v, want os.ErrProcessDone", err)
	}
}

// A live group this user may not signal keeps its EPERM.
func TestSignalForeignGroupKeepsPermissionError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root may signal every group")
	}
	output, err := exec.Command("ps", "-A", "-o", "pid=,pgid=,uid=").Output()
	if err != nil {
		t.Skip(err)
	}
	for line := range bytes.Lines(output) {
		fields := strings.Fields(string(line))
		if len(fields) != 3 || fields[0] != fields[1] || fields[2] != "0" {
			continue
		}
		pgid, err := strconv.Atoi(fields[0])
		if err != nil || pgid <= 1 || !errors.Is(syscall.Kill(-pgid, 0), syscall.EPERM) {
			continue
		}
		if err := Signal(pgid, 0); !errors.Is(err, syscall.EPERM) {
			t.Fatalf("probe of root's group %d = %v, want EPERM", pgid, err)
		}
		return
	}
	t.Skip("no root-owned process group found")
}

func TestSignalRejectsSpecialGroups(t *testing.T) {
	for _, pgid := range []int{-5, 0, 1} {
		if err := Signal(pgid, 0); !errors.Is(err, syscall.EINVAL) {
			t.Fatalf("Signal(%d) = %v, want EINVAL", pgid, err)
		}
	}
}
