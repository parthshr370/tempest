package envrpc

import (
	"syscall"
	"testing"
)

func TestSignalProcessGroupSkipsReapedDaemon(t *testing.T) {
	done := make(chan struct{})
	close(done)
	called := false
	err := signalProcessGroupUnlessDone(done, 123, syscall.SIGTERM, func(pid int, signal syscall.Signal) error {
		called = true
		if pid != -123 || signal != syscall.SIGTERM {
			t.Fatalf("signal target = (%d, %v), want (-123, SIGTERM)", pid, signal)
		}
		return nil
	})
	if err != nil || called {
		t.Fatalf("signalProcessGroupUnlessDone = %v, called=%v; exited process group must not be signaled", err, called)
	}
}
