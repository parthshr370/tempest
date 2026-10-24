package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.harness.dev/harness/internal/work"
)

func TestBashAdmissionReleasesHostPermitAfterCancellation(t *testing.T) {
	host := newHost(t, nil)
	full := errors.New("host full")
	slots := make(chan struct{}, 1)
	released := make(chan struct{}, 1)
	host.manager.options.AdmitProcess = func(context.Context) (func(), error) {
		select {
		case slots <- struct{}{}:
			return func() { <-slots; released <- struct{}{} }, nil
		default:
			return nil, full
		}
	}
	job, err := host.manager.StartBash(invocation(1), work.BashSpec{Command: "sleep 30", TimeoutSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.manager.StartBash(invocation(2), work.BashSpec{Command: "true"}); !errors.Is(err, full) {
		t.Fatal(err)
	}
	duplicate, err := host.manager.StartBash(invocation(1), work.BashSpec{Command: "sleep 30", TimeoutSeconds: 60})
	if err != nil || duplicate.ID != job.ID {
		t.Fatal(duplicate, err)
	}
	if _, err := host.manager.Cancel(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}
	awaitJob(t, host.manager, job.ID)
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("cancelled process retained permit")
	}
	if len(slots) != 0 {
		t.Fatal("permit leaked")
	}
}
