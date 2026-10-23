package tempest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/editbatch"
	"go.harness.dev/harness/internal/provider/faux"
)

// crashBatch leaves session's edit journal holding a batch that wrote a.txt
// and died before deleting b.txt, as a process killed mid-hashline would.
func crashBatch(t *testing.T, s *Session, cwd string) {
	t.Helper()
	root, err := os.OpenRoot(cwd)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	changes := []editbatch.Change{{Path: "a.txt", Data: []byte("A2\n")}, {Path: "b.txt", Delete: true}}
	if err := editbatch.New(s.core.store.Path()+".d").ApplyInterrupted(root, "hashline call c1", changes, 1); err == nil {
		t.Fatal("interrupted batch reported success")
	}
}

func recoveryWorkspace(t *testing.T) string {
	t.Helper()
	cwd := t.TempDir()
	for name, content := range map[string]string{"a.txt": "A1\n", "b.txt": "B1\n"} {
		if err := os.WriteFile(filepath.Join(cwd, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return cwd
}

func fileText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func recoveryDiagnostic(snapshot Snapshot) string {
	for _, message := range snapshot.Diagnostics {
		if strings.Contains(message, "hashline call c1") {
			return message
		}
	}
	return ""
}

func TestResumeRestoresCrashedEditBatch(t *testing.T) {
	f := faux.New(faux.Options{})
	stack := &StackConfig{Model: f.Model(), StreamFn: f.StreamSimple}
	cwd, sessionDir := recoveryWorkspace(t), t.TempDir()
	first, err := Open(context.Background(), Options{SessionDir: sessionDir, Cwd: cwd, New: &NewSession{}, Stack: stack})
	if err != nil {
		t.Fatal(err)
	}
	crashBatch(t, first, cwd)
	path := first.core.store.Path()
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fileText(t, filepath.Join(cwd, "a.txt")) != "A2\n" {
		t.Fatal("the crash simulation did not write a.txt")
	}
	resumed, err := Open(context.Background(), Options{SessionDir: sessionDir, Resume: &ResumeSession{Path: path}, Stack: stack})
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close(context.Background())
	if fileText(t, filepath.Join(cwd, "a.txt")) != "A1\n" || fileText(t, filepath.Join(cwd, "b.txt")) != "B1\n" {
		t.Fatal("resume did not restore the pre-batch state")
	}
	if message := recoveryDiagnostic(resumed.Snapshot()); !strings.Contains(message, "restored to their pre-batch content: a.txt") {
		t.Fatalf("diagnostics = %q", resumed.Snapshot().Diagnostics)
	}
}

// TestNewSessionRecoversSiblingCrashAndReportsConflict covers a user who
// starts a fresh session after the crash instead of resuming, having edited
// one of the batch's files meanwhile.
func TestNewSessionRecoversSiblingCrashAndReportsConflict(t *testing.T) {
	f := faux.New(faux.Options{})
	stack := &StackConfig{Model: f.Model(), StreamFn: f.StreamSimple}
	cwd, sessionDir := recoveryWorkspace(t), t.TempDir()
	crashed, err := Open(context.Background(), Options{SessionDir: sessionDir, Cwd: cwd, New: &NewSession{}, Stack: stack})
	if err != nil {
		t.Fatal(err)
	}
	crashBatch(t, crashed, cwd)
	if err := crashed.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "a.txt"), []byte("user edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fresh, err := Open(context.Background(), Options{SessionDir: sessionDir, Cwd: cwd, New: &NewSession{}, Stack: stack})
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close(context.Background())
	if fileText(t, filepath.Join(cwd, "a.txt")) != "user edit\n" {
		t.Fatal("recovery clobbered an edit made after the crash")
	}
	message := recoveryDiagnostic(fresh.Snapshot())
	if !strings.Contains(message, "conflict, left as found: a.txt") || !strings.Contains(message, "pre-batch copies kept in") {
		t.Fatalf("diagnostics = %q", fresh.Snapshot().Diagnostics)
	}
	// b.txt was still at its pre-batch state: the crash came before its delete.
	if fileText(t, filepath.Join(cwd, "b.txt")) != "B1\n" {
		t.Fatal("b.txt changed")
	}
}
