package checkpoint

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/editbatch"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/toolio"
)

func testManager(t *testing.T) (*Manager, *journal.Store) {
	t.Helper()
	store, err := journal.Create(t.Context(), t.TempDir(), t.TempDir(), journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	loaded, err := journal.Load(t.Context(), store.Path())
	if err != nil {
		t.Fatal(err)
	}
	manager, err := New(t.Context(), loaded.Header.Cwd, store, toolio.NewFileMutationQueue())
	if err != nil {
		t.Fatal(err)
	}
	return manager, store
}

func TestCheckpointSkipsSymlinksAndLargeFiles(t *testing.T) {
	manager, _ := testManager(t)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(manager.cwd, "link")); err != nil {
		t.Fatal(err)
	}
	large, err := os.Create(filepath.Join(manager.cwd, "large"))
	if err != nil {
		t.Fatal(err)
	}
	if err := large.Truncate(maxFile + 1); err != nil {
		t.Fatal(err)
	}
	if err := large.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := manager.Create(t.Context(), "partial protection", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Skipped) != 2 || info.Skipped["link"] == "" || info.Skipped["large"] == "" {
		t.Fatalf("missing capture notes: %+v", info)
	}
	if err := os.WriteFile(filepath.Join(manager.cwd, "large"), []byte("now small but still unprotected at target"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Rewind(t.Context(), info.ID, "", true); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(outside)
	if err != nil || string(content) != "untouched" {
		t.Fatalf("symlink target=%q err=%v", content, err)
	}
	content, err = os.ReadFile(filepath.Join(manager.cwd, "large"))
	if err != nil || !strings.Contains(string(content), "still unprotected") {
		t.Fatalf("skipped path lost: %q %v", content, err)
	}
}

func TestCheckpointInterruptedRewindSettlesWithoutReplaying(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "rolled_back", true: "files_committed"}[committed], func(t *testing.T) {
			ctx := context.Background()
			manager, store := testManager(t)
			path := filepath.Join(manager.cwd, "a")
			if err := os.WriteFile(path, []byte("before"), 0755); err != nil {
				t.Fatal(err)
			}
			original := []types.Message{types.UserMessage{Content: types.StringContent("saved boundary")}}
			info, err := manager.Create(ctx, "save", original)
			if err != nil {
				t.Fatal(err)
			}
			target := manager.state.Snapshots[0]
			if err = os.WriteFile(path, []byte("agent changed"), 0600); err != nil {
				t.Fatal(err)
			}
			os.Chmod(path, 0600)
			if err = os.WriteFile(filepath.Join(manager.cwd, "new"), []byte("agent new"), 0644); err != nil {
				t.Fatal(err)
			}
			before, err := manager.scan(ctx, target.Files)
			if err != nil {
				t.Fatal(err)
			}
			entry, err := journal.NewRewindEntry(info.ID, "", original)
			if err != nil {
				t.Fatal(err)
			}
			if err = persist(manager, "pending.json", pending{Entry: entry, Before: before, After: target.Files}); err != nil {
				t.Fatal(err)
			}
			manager.state.Expected = before
			if err = persist(manager, "state.json", manager.state); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(manager.cwd)
			if err != nil {
				t.Fatal(err)
			}
			changes := []editbatch.Change{{Path: "a", Data: []byte("before"), Perm: 0755}, {Path: "new", Delete: true}}
			if committed {
				err = editbatch.New(manager.data).Apply(root, "rewind test", changes)
			} else {
				err = editbatch.New(manager.data).ApplyInterrupted(root, "rewind test", changes, 1)
			}
			root.Close()
			if committed && err != nil {
				t.Fatal(err)
			}
			if outcomes, err := editbatch.Recover(manager.data, manager.queue); err != nil {
				t.Fatal(err)
			} else {
				for _, outcome := range outcomes {
					if outcome.Err != nil || len(outcome.Conflicts) > 0 {
						t.Fatal(outcome.String())
					}
				}
			}
			recovered, err := New(ctx, manager.cwd, store, manager.queue)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := journal.Load(ctx, store.Path())
			if err != nil {
				t.Fatal(err)
			}
			if committed {
				if len(loaded.Messages) != 1 || !recovered.didRecover {
					t.Fatalf("completed rewind not recovered: %+v", loaded.Messages)
				}
			} else {
				if len(loaded.Messages) != 0 {
					t.Fatalf("rolled-back rewind forked transcript: %+v", loaded.Messages)
				}
				content, _ := os.ReadFile(path)
				if string(content) != "agent changed" {
					t.Fatalf("interrupted batch not rolled back: %q", content)
				}
			}
			first, err := os.ReadFile(store.Path())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = New(ctx, manager.cwd, store, manager.queue); err != nil {
				t.Fatal(err)
			}
			second, err := os.ReadFile(store.Path())
			if err != nil {
				t.Fatal(err)
			}
			if string(first) != string(second) {
				t.Fatal("reopening replayed rewind")
			}
		})
	}
}

func TestCheckpointInterruptedDeferredRequestIsNotReplayed(t *testing.T) {
	manager, store := testManager(t)
	path := filepath.Join(manager.cwd, "file")
	if err := os.WriteFile(path, []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(t.Context(), "before", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("still changed"), 0644); err != nil {
		t.Fatal(err)
	}
	manager.state.RewindRequest = "interrupted-request"
	if err := persist(manager, "state.json", manager.state); err != nil {
		t.Fatal(err)
	}
	recovered, err := New(t.Context(), manager.cwd, store, manager.queue)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.state.RewindRequest != "" || !recovered.didRecover {
		t.Fatal("request was not settled")
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "still changed" {
		t.Fatalf("deferred side effect replayed: %q %v", content, err)
	}
	loaded, err := journal.Load(t.Context(), store.Path())
	if err != nil || len(loaded.Messages) != 1 {
		t.Fatalf("interruption notice missing: %+v %v", loaded, err)
	}
	before, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = New(t.Context(), manager.cwd, store, manager.queue); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("interruption notice duplicated")
	}
}

func TestCheckpointRewindGuardCannotBeForced(t *testing.T) {
	manager, _ := testManager(t)
	if _, err := manager.Create(t.Context(), "saved", nil); err != nil {
		t.Fatal(err)
	}
	manager.Bind(Runtime{
		Messages: func() []types.Message { return nil },
		Guard:    func(context.Context) error { return os.ErrPermission },
	})
	_, err := manager.Tool("rewind").Execute(t.Context(), "rewind", []byte(`{"report":"findings"}`), nil)
	if err != os.ErrPermission {
		t.Fatalf("force bypassed runtime safety guard: %v", err)
	}
	if manager.state.RewindRequest != "" {
		t.Fatal("refused rewind left a durable request")
	}
}
