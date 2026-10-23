package worktree

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// calcSource is a file with two functions far enough apart that edits to
// each merge independently.
const calcSource = `package calc

func Add(a, b int) int {
	return a + b
}

// Spacer keeps the two functions in separate merge hunks.
func Spacer() {}

func Sub(a, b int) int {
	return a - b
}
`

// childPatch runs the real child flow: plan a worktree from the parent's
// current state, let edit change it, and export the patch.
func childPatch(t *testing.T, repo, jobID string, edit func(tree string)) (string, string) {
	t.Helper()
	ctx, journal := planTest(t, repo)
	workspace, err := Plan(ctx, repo, journal, jobID, "digest")
	if err != nil || workspace == nil {
		t.Fatal(workspace, err)
	}
	if err := Ensure(ctx, *workspace, jobID, "digest"); err != nil {
		t.Fatal(err)
	}
	edit(workspace.Path)
	patch, err := Export(ctx, *workspace, filepath.Join(filepath.Dir(workspace.Path), "changes.patch"))
	if err != nil {
		t.Fatal(err)
	}
	return workspace.BaseCommit, patch.Path
}

func calcRepo(t *testing.T) string {
	t.Helper()
	repo := newRepo(t)
	writeTest(t, filepath.Join(repo, "calc.go"), []byte(calcSource))
	gitTest(t, repo, "add", ".")
	gitTest(t, repo, "commit", "-m", "calc")
	return repo
}

// gitState captures what integrate must never change: the index file, HEAD,
// refs and the working-tree status.
func gitState(t *testing.T, repo string) string {
	t.Helper()
	return indexHash(t, repo) + "\n" + gitTest(t, repo, "rev-parse", "HEAD") + "\n" + gitTest(t, repo, "for-each-ref") + "\n" + gitTest(t, repo, "status", "--porcelain", "--untracked-files=all")
}

var integrateLabels = IntegrateLabels{Parent: "parent (live workspace)", Child: "child job_test"}

func TestIntegrateMergesIndependentEditsInOneFile(t *testing.T) {
	repo := calcRepo(t)
	base, patch := childPatch(t, repo, "job_merge", func(tree string) {
		writeTest(t, filepath.Join(tree, "calc.go"), []byte(strings.Replace(calcSource, "return a - b", "return a - b // child", 1)))
		writeTest(t, filepath.Join(tree, "notes", "child.txt"), []byte("child only\n"))
	})
	// The parent edits the other function after the child started, without
	// committing, and keeps an untracked file of its own.
	writeTest(t, filepath.Join(repo, "calc.go"), []byte(strings.Replace(calcSource, "return a + b", "return a + b // parent", 1)))
	writeTest(t, filepath.Join(repo, "scratch.txt"), []byte("parent untracked\n"))
	before := gitState(t, repo)
	integration, err := Integrate(context.Background(), repo, base, patch, integrateLabels)
	if err != nil {
		t.Fatal(err)
	}
	if after := gitState(t, repo); after != before {
		t.Fatalf("integrate changed the parent's git state:\nbefore %s\nafter %s", before, after)
	}
	statuses := map[string]string{}
	for _, file := range integration.Files {
		statuses[file.Path] = file.Status
	}
	if statuses["calc.go"] != IntegrationMerged || statuses["notes/child.txt"] != IntegrationClean || len(statuses) != 2 {
		t.Fatalf("files = %+v", integration.Files)
	}
	writes := map[string]FileWrite{}
	for _, write := range integration.Writes {
		writes[write.Path] = write
	}
	calc := string(writes["calc.go"].Data)
	if !strings.Contains(calc, "a + b // parent") || !strings.Contains(calc, "a - b // child") {
		t.Fatalf("merged calc.go = %s", calc)
	}
	if writes["calc.go"].Snapshot == "" || writes["notes/child.txt"].Snapshot != "" || len(writes) != 2 {
		t.Fatalf("writes = %+v", integration.Writes)
	}
	// The live file still hashes to the snapshot blob the write is checked
	// against; the child-only file did not exist there.
	ids, err := BlobIDs(context.Background(), repo, []string{"calc.go", "scratch.txt"})
	if err != nil || ids["calc.go"] != writes["calc.go"].Snapshot || ids["scratch.txt"] == "" {
		t.Fatalf("live blobs %v, snapshot %s, %v", ids, writes["calc.go"].Snapshot, err)
	}
}

func TestIntegrateReportsOverlappingEditsAsConflict(t *testing.T) {
	repo := calcRepo(t)
	base, patch := childPatch(t, repo, "job_conflict", func(tree string) {
		writeTest(t, filepath.Join(tree, "calc.go"), []byte(strings.Replace(calcSource, "return a + b", "return b + a", 1)))
	})
	writeTest(t, filepath.Join(repo, "calc.go"), []byte(strings.Replace(calcSource, "return a + b", "return a + b + 0", 1)))
	before := gitState(t, repo)
	integration, err := Integrate(context.Background(), repo, base, patch, integrateLabels)
	if err != nil {
		t.Fatal(err)
	}
	if gitState(t, repo) != before {
		t.Fatal("integrate changed the parent's git state")
	}
	if integration.Conflicts() != 1 || len(integration.Writes) != 0 {
		t.Fatalf("integration = %+v", integration)
	}
	report := integration.Files[0].Conflict
	for _, want := range []string{"<<<<<<< parent (live workspace)", "return a + b + 0", "||||||| base (the child's starting point)", "return b + a", ">>>>>>> child job_test"} {
		if !strings.Contains(report, want) {
			t.Fatalf("conflict report lacks %q:\n%s", want, report)
		}
	}
	if !strings.HasPrefix(integration.Files[0].Message, "CONFLICT") {
		t.Fatalf("message = %q", integration.Files[0].Message)
	}
}

func TestIntegrateNeedsGit(t *testing.T) {
	_, err := Integrate(context.Background(), t.TempDir(), "HEAD", filepath.Join(t.TempDir(), "changes.patch"), integrateLabels)
	if !errors.Is(err, ErrNotGit) {
		t.Fatalf("err = %v", err)
	}
}
