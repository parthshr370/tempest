package checkpoint

import (
	"os"
	"path/filepath"
	"testing"

	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/toolio"
)

func TestCheckpointExcludesItsOwnJournalInsideWorkspace(t *testing.T) {
	cwd := t.TempDir()
	store, err := journal.Create(t.Context(), filepath.Join(cwd, "sessions"), cwd, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = os.WriteFile(filepath.Join(cwd, "file"), []byte("bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	manager, err := New(t.Context(), cwd, store, toolio.NewFileMutationQueue())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = manager.Create(t.Context(), "first", nil); err != nil {
		t.Fatal(err)
	}
	if _, err = manager.Create(t.Context(), "second", nil); err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range manager.state.Snapshots {
		if len(snapshot.Files) != 1 {
			t.Fatalf("session storage entered snapshot: %+v", snapshot.Files)
		}
	}
	if _, err = manager.Rewind(t.Context(), "", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err = journal.Load(t.Context(), store.Path()); err != nil {
		t.Fatal(err)
	}
}
