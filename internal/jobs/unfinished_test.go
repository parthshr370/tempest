package jobs

import (
	"context"
	"testing"

	"go.harness.dev/harness/internal/work"
)

// TestUnfinishedCountsNonTerminalJobs backs the tempest.jobs gauge: a
// running job counts, and a cancelled one no longer does.
func TestUnfinishedCountsNonTerminalJobs(t *testing.T) {
	host := newHost(t, nil)
	if got := host.manager.Unfinished(); got != 0 {
		t.Fatalf("empty manager: Unfinished = %d", got)
	}
	if _, err := host.manager.StartBash(invocation(1), work.BashSpec{Command: "sleep 300", TimeoutSeconds: 300}); err != nil {
		t.Fatal(err)
	}
	if got := host.manager.Unfinished(); got != 1 {
		t.Fatalf("running job: Unfinished = %d, want 1", got)
	}
	if err := host.manager.Finish(context.Background(), "aborted"); err != nil {
		t.Fatal(err)
	}
	if got := host.manager.Unfinished(); got != 0 {
		t.Fatalf("after cancel: Unfinished = %d, want 0", got)
	}
}
