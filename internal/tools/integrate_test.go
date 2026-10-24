package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/editbatch"
	"go.harness.dev/harness/internal/subagent"
	"go.harness.dev/harness/internal/work"
	"go.harness.dev/harness/internal/worktree"
)

// patchController serves one child patch to the integrate tool.
type patchController struct {
	work.Controller
	patch work.ChildPatch
}

func (c patchController) ChildPatch(context.Context, string) (work.ChildPatch, error) {
	return c.patch, nil
}

const integrateCalc = `package calc

func Add(a, b int) int {
	return a + b
}

// Spacer keeps the two functions in separate merge hunks.
func Spacer() {}

func Sub(a, b int) int {
	return a - b
}
`

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=t@example.test", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// integrateRepo is a repository holding calc.go, symlink-resolved so its
// path matches what git reports as the top level.
func integrateRepo(t *testing.T) string {
	t.Helper()
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "init")
	writeFile(t, filepath.Join(repo, "calc.go"), integrateCalc)
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-m", "base")
	return repo
}

// childJob runs the real child flow (plan, worktree, edit, export) and
// returns the ChildPatch the jobs manager would hand out.
func childJob(t *testing.T, repo string, edit func(tree string)) work.ChildPatch {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	journal := filepath.Join(t.TempDir(), "parent.jsonl")
	workspace, err := worktree.Plan(ctx, repo, journal, "job_child", "digest")
	if err != nil || workspace == nil {
		t.Fatal(workspace, err)
	}
	if err := worktree.Ensure(ctx, *workspace, "job_child", "digest"); err != nil {
		t.Fatal(err)
	}
	edit(workspace.Path)
	patch, err := worktree.Export(ctx, *workspace, filepath.Join(filepath.Dir(workspace.Path), "changes.patch"))
	if err != nil {
		t.Fatal(err)
	}
	return work.ChildPatch{Job: work.Job{ID: "job_child", Kind: "task", State: "completed"}, Repo: workspace.Repo, BaseCommit: workspace.BaseCommit, Patch: patch}
}

// userGitState is what integrate must never change: the index, HEAD and refs.
func userGitState(t *testing.T, repo string) string {
	t.Helper()
	index, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(index)
	return hex.EncodeToString(sum[:]) + " " + gitRun(t, repo, "rev-parse", "HEAD") + " " + gitRun(t, repo, "for-each-ref")
}

func integrateTool(t *testing.T, repo string, patch work.ChildPatch, sessionDir string) func(args map[string]any) (string, integrateDetails, error) {
	t.Helper()
	tool, err := NewTool(IntegrateTool, repo, withHashlineStore(ToolsOptions{Work: patchController{patch: patch}, TaskTool: subagent.NewTool(patchController{}), SessionDataDir: sessionDir}))
	if err != nil {
		t.Fatal(err)
	}
	return func(args map[string]any) (string, integrateDetails, error) {
		result, err := executeTool(t, tool, args)
		var details integrateDetails
		if err == nil {
			if decodeErr := json.Unmarshal(result.Details, &details); decodeErr != nil {
				t.Fatal(decodeErr)
			}
		}
		return resultText(result), details, err
	}
}

func readString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestIntegrateToolMergesIndependentEditsAfterAccept(t *testing.T) {
	repo := integrateRepo(t)
	// The child starts from a dirty parent: its base is a snapshot commit.
	writeFile(t, filepath.Join(repo, "draft.txt"), "parent draft\n")
	patch := childJob(t, repo, func(tree string) {
		writeFile(t, filepath.Join(tree, "calc.go"), strings.Replace(integrateCalc, "return a - b", "return a - b // child", 1))
		writeFile(t, filepath.Join(tree, "child.txt"), "from the child\n")
	})
	writeFile(t, filepath.Join(repo, "calc.go"), strings.Replace(integrateCalc, "return a + b", "return a + b // parent", 1))
	before := userGitState(t, repo)
	sessionDir := filepath.Join(t.TempDir(), "s.jsonl.d")
	run := integrateTool(t, repo, patch, sessionDir)

	text, details, err := run(map[string]any{"job_id": "job_child"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "merged   calc.go") || !strings.Contains(text, "clean    child.txt") || !strings.Contains(text, "accept_merged: true") || len(details.Written) != 0 {
		t.Fatalf("report = %q, details %+v", text, details)
	}
	if strings.Contains(readString(t, filepath.Join(repo, "calc.go")), "// child") {
		t.Fatal("a merged file was written without accept_merged")
	}

	text, details, err = run(map[string]any{"job_id": "job_child", "accept_merged": true})
	if err != nil {
		t.Fatal(err)
	}
	calc := readString(t, filepath.Join(repo, "calc.go"))
	if !strings.Contains(calc, "a + b // parent") || !strings.Contains(calc, "a - b // child") {
		t.Fatalf("calc.go = %s", calc)
	}
	if readString(t, filepath.Join(repo, "child.txt")) != "from the child\n" || readString(t, filepath.Join(repo, "draft.txt")) != "parent draft\n" {
		t.Fatal("child file missing or parent draft changed")
	}
	if len(details.Written) != 2 || !strings.Contains(text, "Wrote 2 file(s).") || !strings.Contains(text, "[calc.go#") {
		t.Fatalf("result = %q, details %+v", text, details)
	}
	if userGitState(t, repo) != before {
		t.Fatal("integrate changed the user's index, HEAD or refs")
	}
	if entries, _ := os.ReadDir(editbatch.Dir(sessionDir)); len(entries) != 0 {
		t.Fatalf("committed integration left journal entries %v", entries)
	}
}

func TestIntegrateToolConflictWritesNothing(t *testing.T) {
	repo := integrateRepo(t)
	patch := childJob(t, repo, func(tree string) {
		writeFile(t, filepath.Join(tree, "calc.go"), strings.Replace(integrateCalc, "return a + b", "return b + a", 1))
		writeFile(t, filepath.Join(tree, "child.txt"), "from the child\n")
	})
	parentCalc := strings.Replace(integrateCalc, "return a + b", "return a + b + 0", 1)
	writeFile(t, filepath.Join(repo, "calc.go"), parentCalc)
	before := userGitState(t, repo)
	text, details, err := integrateTool(t, repo, patch, "")(map[string]any{"job_id": "job_child", "accept_merged": true})
	if err != nil {
		t.Fatal(err)
	}
	if details.Conflicts != 1 || len(details.Written) != 0 || !strings.Contains(text, "conflict calc.go") || !strings.Contains(text, "<<<<<<< parent (live workspace)") || !strings.Contains(text, ">>>>>>> child job_child") {
		t.Fatalf("report = %q, details %+v", text, details)
	}
	if readString(t, filepath.Join(repo, "calc.go")) != parentCalc {
		t.Fatal("conflicted file was written")
	}
	if _, err := os.Stat(filepath.Join(repo, "child.txt")); !os.IsNotExist(err) {
		t.Fatalf("a clean file was written despite the conflict: %v", err)
	}
	if userGitState(t, repo) != before {
		t.Fatal("integrate changed the user's index, HEAD or refs")
	}
}

func TestIntegrateRefusesWhenLiveFileChangedAfterSnapshot(t *testing.T) {
	repo := integrateRepo(t)
	patch := childJob(t, repo, func(tree string) {
		writeFile(t, filepath.Join(tree, "calc.go"), strings.Replace(integrateCalc, "return a - b", "return a - b // child", 1))
	})
	integration, err := worktree.Integrate(context.Background(), repo, patch.BaseCommit, patch.Patch.Path, worktree.IntegrateLabels{Parent: liveParentLabel, Child: "child job_child"})
	if err != nil {
		t.Fatal(err)
	}
	// Another writer edits calc.go between the snapshot and the write.
	drifted := integrateCalc + "\n// edited meanwhile\n"
	writeFile(t, filepath.Join(repo, "calc.go"), drifted)
	root, err := os.OpenRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	run := integrationRun{cwd: repo, root: root, input: integrateInput{JobID: "job_child"}, integration: integration}
	_, _, err = run.finish(context.Background())
	if err == nil || !strings.Contains(err.Error(), "calc.go (modified)") || !strings.Contains(err.Error(), "Nothing was written") {
		t.Fatalf("err = %v", err)
	}
	if readString(t, filepath.Join(repo, "calc.go")) != drifted {
		t.Fatal("the drifted live file was overwritten")
	}
}

func TestIntegrateExplainsNonGitWorkspace(t *testing.T) {
	repo := integrateRepo(t)
	patch := childJob(t, repo, func(tree string) {
		writeFile(t, filepath.Join(tree, "calc.go"), integrateCalc+"// child\n")
	})
	plain := t.TempDir()
	_, _, err := integrateTool(t, plain, patch, "")(map[string]any{"job_id": "job_child"})
	if err == nil || !strings.Contains(err.Error(), "integration needs a git repository") || !strings.Contains(err.Error(), patch.Patch.Path) {
		t.Fatalf("err = %v", err)
	}
}

// integrateSchema is the integrate tool's advertised argument schema.
const integrateSchema = `{"type":"object","properties":{"job_id":{"type":"string","description":"the finished general task's job id"},"accept_merged":{"type":"boolean","description":"write files git merged from both sides' changes; default false reports them without writing"}},"required":["job_id"]}`

func TestIntegrateRegistersOnlyWhereTasksRun(t *testing.T) {
	controller := patchController{}
	options := ToolsOptions{Work: controller, TaskTool: subagent.NewTool(controller)}
	tool, err := NewTool(IntegrateTool, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	assertSchemaParity(t, string(IntegrateTool), tool.Parameters, integrateSchema)
	without := map[string]ToolsOptions{
		"no job controller": {TaskTool: options.TaskTool},
		"no task tool":      {Work: controller},
		"child session":     {Work: controller, TaskTool: options.TaskTool, Child: true},
	}
	for name, candidate := range without {
		if _, err := NewTool(IntegrateTool, t.TempDir(), candidate); err == nil {
			t.Fatalf("integrate built with %s", name)
		}
	}
}

// TestHashlineMultiFileWriteFailureRollsBack makes the second file of a
// two-section patch unwritable: the first file must come back unchanged.
func TestHashlineMultiFileWriteFailureRollsBack(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "a1\n")
	writeFile(t, filepath.Join(dir, "locked", "b.txt"), "b1\n")
	sessionDir := filepath.Join(t.TempDir(), "s.jsonl.d")
	all := AllTools(dir, ToolsOptions{SessionDataDir: sessionDir})
	tagA := tagOf(t, readThrough(t, all, "a.txt"), "a.txt")
	tagB := tagOf(t, readThrough(t, all, "locked/b.txt"), "locked/b.txt")
	if err := os.Chmod(filepath.Join(dir, "locked"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(dir, "locked"), 0o755) })
	_, err := runHashline(t, all[HashlineTool], fmt.Sprintf("[a.txt#%s]\nPUT 1.=1:\n+A1\n[locked/b.txt#%s]\nPUT 1.=1:\n+B1\n", tagA, tagB))
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("err = %v", err)
	}
	if readString(t, filepath.Join(dir, "a.txt")) != "a1\n" {
		t.Fatal("a.txt kept the write of a failed batch")
	}
	if entries, _ := os.ReadDir(editbatch.Dir(sessionDir)); len(entries) != 0 {
		t.Fatalf("rolled-back batch left %v", entries)
	}
}

// TestRecoverEditBatchesRestoresCrashedHashlineBatch replays a hashline
// batch killed between its writes and recovers it as a session open does.
func TestRecoverEditBatchesRestoresCrashedHashlineBatch(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "a1\n")
	writeFile(t, filepath.Join(dir, "b.txt"), "b1\n")
	sessionDir := filepath.Join(t.TempDir(), "s.jsonl.d")
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	changes := []editbatch.Change{{Path: "a.txt", Data: []byte("A1\n")}, {Path: "b.txt", Delete: true}}
	if err := editbatch.New(sessionDir).ApplyInterrupted(root, "hashline call c9", changes, 1); err == nil {
		t.Fatal("interrupted batch reported success")
	}
	outcomes, err := RecoverEditBatches([]string{sessionDir}, nil)
	if err != nil || len(outcomes) != 1 || len(outcomes[0].Restored) != 1 {
		t.Fatalf("recover = %+v, %v", outcomes, err)
	}
	if readString(t, filepath.Join(dir, "a.txt")) != "a1\n" || readString(t, filepath.Join(dir, "b.txt")) != "b1\n" {
		t.Fatal("pre-batch state not restored")
	}
}

func TestIntegrateHookSettingsRequireApproval(t *testing.T) {
	repo := integrateRepo(t)
	patch := childJob(t, repo, func(tree string) {
		if err := os.Mkdir(filepath.Join(tree, ".harness"), 0700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(tree, ".harness", "settings.local.json"), `{"hooks":{}}`)
	})
	unapproved, err := NewTool(IntegrateTool, repo, withHashlineStore(ToolsOptions{Work: patchController{patch: patch}, TaskTool: subagent.NewTool(patchController{})}))
	if err != nil {
		t.Fatal(err)
	}
	args := json.RawMessage(`{"job_id":"job_child"}`)
	if _, err := unapproved.Execute(context.Background(), "call", args, nil); err == nil || !strings.Contains(err.Error(), "hook settings") {
		t.Fatalf("settings patch bypassed approval: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".harness", "settings.local.json")); !os.IsNotExist(err) {
		t.Fatalf("unapproved patch written: %v", err)
	}
	calls := 0
	tool, err := NewTool(IntegrateTool, repo, withHashlineStore(ToolsOptions{Work: patchController{patch: patch}, TaskTool: subagent.NewTool(patchController{}), ApproveHookSettings: func(_ context.Context, callID string, paths []string) error {
		calls++
		if callID != "approved" {
			t.Errorf("approval lost tool call identity: %q", callID)
		}
		if len(paths) != 1 || !strings.HasSuffix(paths[0], ".harness/settings.local.json") {
			t.Errorf("computed targets=%v", paths)
		}
		return nil
	}}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(context.Background(), "approved", args, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || readString(t, filepath.Join(repo, ".harness", "settings.local.json")) != `{"hooks":{}}` {
		t.Fatal("approved integration failed")
	}
}
