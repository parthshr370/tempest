package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/work"
	"go.harness.dev/harness/internal/worktree"
)

func TestChildPatchServesOnlySettledIsolatedPatches(t *testing.T) {
	host := newHost(t, func(context.Context, work.TaskLaunch) (work.TaskResult, error) {
		return work.TaskResult{Text: "done"}, nil
	})
	patch, err := worktree.WriteArtifact(filepath.Join(t.TempDir(), "changes.patch"), strings.NewReader("diff --git a/x b/x\n"), worktree.MaxPatchBytes)
	if err != nil {
		t.Fatal(err)
	}
	workspace := &work.Workspace{Repo: "/repo", BaseCommit: "base"}
	host.manager.mu.Lock()
	host.manager.entries["finished"] = &entry{recovery: work.Recovery{Job: work.Job{ID: "finished", Kind: "task", State: "completed", Patch: &patch}, Workspace: workspace}, joined: true}
	host.manager.entries["running"] = &entry{recovery: work.Recovery{Job: work.Job{ID: "running", Kind: "task", State: "running"}, Workspace: workspace}, live: true}
	host.manager.entries["shared"] = &entry{recovery: work.Recovery{Job: work.Job{ID: "shared", Kind: "task", State: "completed", Isolation: "shared"}}, joined: true}
	host.manager.entries["shell"] = &entry{recovery: work.Recovery{Job: work.Job{ID: "shell", Kind: "bash", State: "completed"}}, joined: true}
	host.manager.mu.Unlock()

	got, err := host.manager.ChildPatch(context.Background(), "finished")
	if err != nil || got.Patch != patch || got.Repo != "/repo" || got.BaseCommit != "base" {
		t.Fatalf("ChildPatch = %+v, %v", got, err)
	}
	for jobID, want := range map[string]string{"running": "still running", "shared": "shared workspace", "shell": "bash job"} {
		if _, err := host.manager.ChildPatch(context.Background(), jobID); !errors.Is(err, ErrNoPatch) || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: err = %v", jobID, err)
		}
	}
	if _, err := host.manager.ChildPatch(context.Background(), "missing"); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("missing: err = %v", err)
	}
	// A patch changed on disk after export is refused, not merged.
	if err := os.WriteFile(patch.Path, []byte("tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := host.manager.ChildPatch(context.Background(), "finished"); err == nil || !strings.Contains(err.Error(), "failed verification") {
		t.Fatalf("tampered: err = %v", err)
	}
}
