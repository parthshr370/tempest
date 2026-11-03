package jobs

import (
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

// A `cmd &` child belongs to its job's command: it ends when the job's shell
// exits, instead of outliving the job unseen by wait and cancel while it
// holds the job's output open.
func TestBashJobEndsBackgroundChildrenWithItsShell(t *testing.T) {
	host := newHost(t, nil)
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	started := time.Now()
	job, err := host.manager.StartBash(invocation(1), work.BashSpec{Command: fmt.Sprintf("sleep 30 & printf '%%s' $! > %q; echo started", pidFile), TimeoutSeconds: 20})
	if err != nil {
		t.Fatal(err)
	}
	job = awaitJob(t, host.manager, job.ID)
	elapsed := time.Since(started)
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if job.State != "completed" || job.ExitCode == nil || *job.ExitCode != 0 || !strings.Contains(job.ResultText, "started") {
		t.Fatalf("job = %+v", job)
	}
	if elapsed >= pipeJoinDelay {
		t.Fatalf("job settled after %s: the background child held its output", elapsed)
	}
	assertDescendantStopped(t, pid)
}
