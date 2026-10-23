package editbatch

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// workspace builds a root with the given files and a session data dir.
func workspace(t *testing.T, files map[string]string) (*os.Root, string, string) {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root, dir, filepath.Join(t.TempDir(), "session.jsonl.d")
}

func readOrMissing(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "<missing>"
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func batchDirs(t *testing.T, sessionDir string) []string {
	t.Helper()
	items, err := os.ReadDir(Dir(sessionDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, item := range items {
		names = append(names, item.Name())
	}
	return names
}

// mixedBatch updates a.go, creates new/dir/c.go and deletes b.go.
func mixedBatch() []Change {
	return []Change{
		{Path: "a.go", Data: []byte("A2\n")},
		{Path: filepath.Join("new", "dir", "c.go"), Data: []byte("C\n")},
		{Path: "b.go", Delete: true},
	}
}

func TestApplyCommitsAndLeavesNoRecord(t *testing.T) {
	root, dir, sessionDir := workspace(t, map[string]string{"a.go": "A1\n", "b.go": "B1\n"})
	if err := os.Chmod(filepath.Join(dir, "a.go"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := New(sessionDir).Apply(root, "test", mixedBatch()); err != nil {
		t.Fatal(err)
	}
	if got := readOrMissing(t, filepath.Join(dir, "a.go")); got != "A2\n" {
		t.Fatalf("a.go = %q", got)
	}
	if info, err := os.Stat(filepath.Join(dir, "a.go")); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("a.go mode = %v, %v; the rewrite must keep it", info.Mode(), err)
	}
	if got := readOrMissing(t, filepath.Join(dir, "new", "dir", "c.go")); got != "C\n" {
		t.Fatalf("c.go = %q", got)
	}
	if got := readOrMissing(t, filepath.Join(dir, "b.go")); got != "<missing>" {
		t.Fatalf("b.go = %q", got)
	}
	if names := batchDirs(t, sessionDir); len(names) != 0 {
		t.Fatalf("committed batch left journal entries %v", names)
	}
}

// TestCrashBetweenWritesThenRecoverRestoresPreState interrupts the batch
// after each write in turn, as a killed process would, and checks that
// recovery returns every path to its pre-batch content, mode and existence.
func TestCrashBetweenWritesThenRecoverRestoresPreState(t *testing.T) {
	for completed := 0; completed <= len(mixedBatch()); completed++ {
		root, dir, sessionDir := workspace(t, map[string]string{"a.go": "A1\n", "b.go": "B1\n"})
		if err := os.Chmod(filepath.Join(dir, "b.go"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := New(sessionDir).ApplyInterrupted(root, "hashline call c1", mixedBatch(), completed)
		if !errors.Is(err, ErrInterrupted) {
			t.Fatalf("completed=%d: err = %v", completed, err)
		}
		if names := batchDirs(t, sessionDir); len(names) != 1 {
			t.Fatalf("completed=%d: crashed batch must leave its record, got %v", completed, names)
		}
		outcomes, err := Recover(sessionDir, nil)
		if err != nil || len(outcomes) != 1 {
			t.Fatalf("completed=%d: recover = %v, %v", completed, outcomes, err)
		}
		outcome := outcomes[0]
		if outcome.Err != nil || len(outcome.Conflicts) != 0 || len(outcome.Restored) != completed {
			t.Fatalf("completed=%d: outcome = %+v", completed, outcome)
		}
		if !strings.Contains(outcome.String(), "hashline call c1") || !strings.Contains(outcome.String(), "interrupted") {
			t.Fatalf("diagnostic = %q", outcome.String())
		}
		if got := readOrMissing(t, filepath.Join(dir, "a.go")); got != "A1\n" {
			t.Fatalf("completed=%d: a.go = %q", completed, got)
		}
		if got := readOrMissing(t, filepath.Join(dir, "b.go")); got != "B1\n" {
			t.Fatalf("completed=%d: b.go = %q", completed, got)
		}
		if info, err := os.Stat(filepath.Join(dir, "b.go")); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("completed=%d: b.go mode = %v, %v", completed, info, err)
		}
		if _, err := os.Lstat(filepath.Join(dir, "new")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("completed=%d: directories the batch created must go: %v", completed, err)
		}
		if names := batchDirs(t, sessionDir); len(names) != 0 {
			t.Fatalf("completed=%d: recovered batch left %v", completed, names)
		}
	}
}

func TestRecoverReportsConflictForFileChangedAfterCrash(t *testing.T) {
	root, dir, sessionDir := workspace(t, map[string]string{"a.go": "A1\n", "b.go": "B1\n"})
	changes := []Change{{Path: "a.go", Data: []byte("A2\n")}, {Path: "b.go", Data: []byte("B2\n")}}
	if err := New(sessionDir).ApplyInterrupted(root, "test", changes, 2); !errors.Is(err, ErrInterrupted) {
		t.Fatal(err)
	}
	// The user edits a.go after the crash; recovery must not clobber it.
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("user edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outcomes, err := Recover(sessionDir, nil)
	if err != nil || len(outcomes) != 1 {
		t.Fatalf("recover = %v, %v", outcomes, err)
	}
	outcome := outcomes[0]
	if len(outcome.Conflicts) != 1 || outcome.Conflicts[0].Path != "a.go" || len(outcome.Restored) != 1 || outcome.Restored[0] != "b.go" {
		t.Fatalf("outcome = %+v", outcome)
	}
	if got := readOrMissing(t, filepath.Join(dir, "a.go")); got != "user edit\n" {
		t.Fatalf("conflicting file was overwritten: %q", got)
	}
	if got := readOrMissing(t, filepath.Join(dir, "b.go")); got != "B1\n" {
		t.Fatalf("b.go = %q", got)
	}
	if outcome.Preserved == "" || !strings.Contains(outcome.String(), "conflict, left as found: a.go") {
		t.Fatalf("diagnostic = %q", outcome.String())
	}
	copyOfA := filepath.Join(outcome.Preserved, "prior", "a.go")
	if got := readOrMissing(t, copyOfA); got != "A1\n" {
		t.Fatalf("preserved pre-batch copy = %q", got)
	}
	// The preserved batch is no longer hot: a second recovery does nothing.
	again, err := Recover(sessionDir, nil)
	if err != nil || len(again) != 0 {
		t.Fatalf("second recover = %v, %v", again, err)
	}
}

func TestRecoverSkipsLiveBatch(t *testing.T) {
	root, dir, sessionDir := workspace(t, map[string]string{"a.go": "A1\n", "b.go": "B1\n"})
	changes := []Change{{Path: "a.go", Data: []byte("A2\n")}, {Path: "b.go", Data: []byte("B2\n")}}
	live, err := New(sessionDir).begin(root, "test", changes)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyChange(root, changes[0], live.record.Entries[0].Staged); err != nil {
		t.Fatal(err)
	}
	outcomes, err := Recover(sessionDir, nil)
	if err != nil || len(outcomes) != 0 {
		t.Fatalf("recovery touched a live batch: %v, %v", outcomes, err)
	}
	if got := readOrMissing(t, filepath.Join(dir, "a.go")); got != "A2\n" {
		t.Fatalf("a.go = %q", got)
	}
	if err := applyChange(root, changes[1], live.record.Entries[1].Staged); err != nil {
		t.Fatal(err)
	}
	if err := live.commit(root); err != nil {
		t.Fatal(err)
	}
	if names := batchDirs(t, sessionDir); len(names) != 0 {
		t.Fatalf("journal = %v", names)
	}
}

// readOnlyBatch returns a batch whose second write fails after the first
// landed: its directory refuses the staged file.
func readOnlyBatch(t *testing.T, dir string) []Change {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	locked := filepath.Join(dir, "locked")
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	return []Change{{Path: "a.go", Data: []byte("A2\n")}, {Path: filepath.Join("locked", "f"), Data: []byte("F2\n")}}
}

func TestFailedWriteRollsBackInProcess(t *testing.T) {
	root, dir, sessionDir := workspace(t, map[string]string{"a.go": "A1\n", "locked/f": "F1\n"})
	err := New(sessionDir).Apply(root, "test", readOnlyBatch(t, dir))
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("err = %v", err)
	}
	if got := readOrMissing(t, filepath.Join(dir, "a.go")); got != "A1\n" {
		t.Fatalf("a.go = %q", got)
	}
	if names := batchDirs(t, sessionDir); len(names) != 0 {
		t.Fatalf("rolled-back batch left %v", names)
	}
}

func TestJournalLessBatchStillRollsBack(t *testing.T) {
	root, dir, _ := workspace(t, map[string]string{"a.go": "A1\n", "locked/f": "F1\n"})
	if err := New("").Apply(root, "test", readOnlyBatch(t, dir)); err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("err = %v", err)
	}
	if got := readOrMissing(t, filepath.Join(dir, "a.go")); got != "A1\n" {
		t.Fatalf("a.go = %q", got)
	}
}

// TestRecoverRemovesStagedFileOfKilledWrite covers a kill after a write
// created its staged file but before the rename published it.
func TestRecoverRemovesStagedFileOfKilledWrite(t *testing.T) {
	root, dir, sessionDir := workspace(t, map[string]string{"a.go": "A1\n", "b.go": "B1\n"})
	changes := []Change{{Path: "a.go", Data: []byte("A2\n")}, {Path: "b.go", Data: []byte("B2\n")}}
	live, err := New(sessionDir).begin(root, "test", changes)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyChange(root, changes[0], live.record.Entries[0].Staged); err != nil {
		t.Fatal(err)
	}
	staged := live.record.Entries[1].Staged
	if err := os.WriteFile(filepath.Join(dir, staged), []byte("B"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := live.abandon(); !errors.Is(err, ErrInterrupted) {
		t.Fatal(err)
	}
	outcomes, err := Recover(sessionDir, nil)
	if err != nil || len(outcomes) != 1 || outcomes[0].Err != nil {
		t.Fatalf("recover = %+v, %v", outcomes, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, staged)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged file of the killed write survived recovery: %v", err)
	}
	if readOrMissing(t, filepath.Join(dir, "a.go")) != "A1\n" || readOrMissing(t, filepath.Join(dir, "b.go")) != "B1\n" {
		t.Fatal("pre-batch state not restored")
	}
}

func TestRecoverRemovesUnpublishedBatch(t *testing.T) {
	_, _, sessionDir := workspace(t, nil)
	// A crash while copies were being written leaves no record.json.
	stale := filepath.Join(Dir(sessionDir), "20260101T000000Z-00")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "prior-0"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	outcomes, err := Recover(sessionDir, nil)
	if err != nil || len(outcomes) != 0 {
		t.Fatalf("recover = %v, %v", outcomes, err)
	}
	if _, err := os.Lstat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unpublished batch kept: %v", err)
	}
}

func TestWriteFileIsAtomicAndRefusesSymlinks(t *testing.T) {
	root, dir, _ := workspace(t, map[string]string{"a.txt": "old"})
	if err := WriteFile(root, "a.txt", []byte("new"), 0); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || readOrMissing(t, filepath.Join(dir, "a.txt")) != "new" {
		t.Fatalf("staged files left or content wrong: %v", entries)
	}
	if err := os.Symlink("a.txt", filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := WriteFile(root, "link", []byte("x"), 0); err == nil {
		t.Fatal("replacing a symlink must be refused")
	}
	if err := WriteFile(root, "exec.sh", []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(dir, "exec.sh")); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("exec.sh mode = %v, %v", info, err)
	}
}
