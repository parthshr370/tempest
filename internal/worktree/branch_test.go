package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// branchFixture is a bare remote with one commit on main and a clone of it.
type branchFixture struct {
	remote, clone, state string
}

func canonicalTemp(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func newBranchFixture(t *testing.T) branchFixture {
	t.Helper()
	root := canonicalTemp(t)
	f := branchFixture{remote: filepath.Join(root, "remote.git"), clone: filepath.Join(root, "clone"), state: filepath.Join(root, "state")}
	gitTest(t, root, "init", "--bare", "-b", "main", f.remote)
	seed := filepath.Join(root, "seed")
	gitTest(t, root, "clone", f.remote, seed)
	gitTest(t, seed, "checkout", "-b", "main")
	writeTest(t, filepath.Join(seed, "tracked"), []byte("base\n"))
	gitTest(t, seed, "add", ".")
	gitTest(t, seed, "commit", "-m", "base")
	gitTest(t, seed, "push", "origin", "main")
	gitTest(t, root, "clone", f.remote, f.clone)
	return f
}

func (f branchFixture) spec(ticket string) BranchSpec {
	return BranchSpec{Clone: f.clone, Path: filepath.Join(f.state, "worktrees", ticket), Branch: "agent/" + ticket, Base: "main"}
}

func branchContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func TestBranchEnsureCreatesAdoptsAndRecreates(t *testing.T) {
	ctx := branchContext(t)
	f := newBranchFixture(t)
	base := gitTest(t, f.remote, "rev-parse", "refs/heads/main")
	spec := f.spec("i7-abc123")
	created, err := EnsureBranch(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if !created.Created || created.BaseSHA != base || created.Path != spec.Path {
		t.Fatalf("created = %+v, want new checkout at base %s", created, base)
	}
	if head := gitTest(t, spec.Path, "symbolic-ref", "HEAD"); head != "refs/heads/agent/i7-abc123" {
		t.Fatalf("HEAD = %s", head)
	}
	if upstream, err := git(ctx, spec.Path, nil, "rev-parse", "--abbrev-ref", "@{upstream}"); err == nil {
		t.Fatalf("new branch tracks %s; it must not push implicitly", upstream)
	}
	if info, err := os.Stat(created.GitDir); err != nil || !info.IsDir() {
		t.Fatalf("git dir %q: %v", created.GitDir, err)
	}

	adopted, err := EnsureBranch(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if adopted.Created || adopted.BaseSHA != base || adopted.GitDir != created.GitDir {
		t.Fatalf("adopted = %+v, want the same checkout", adopted)
	}

	writeTest(t, filepath.Join(spec.Path, "work"), []byte("worker\n"))
	gitTest(t, spec.Path, "add", ".")
	gitTest(t, spec.Path, "commit", "-m", "worker")
	work := gitTest(t, spec.Path, "rev-parse", "HEAD")
	if err := os.RemoveAll(spec.Path); err != nil {
		t.Fatal(err)
	}
	recreated, err := EnsureBranch(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if !recreated.Created || recreated.BaseSHA != base {
		t.Fatalf("recreated = %+v", recreated)
	}
	if head := gitTest(t, spec.Path, "rev-parse", "HEAD"); head != work {
		t.Fatalf("recreated checkout lost the branch's commit: HEAD %s, want %s", head, work)
	}
}

func TestBranchEnsureRefusesForeignCheckouts(t *testing.T) {
	ctx := branchContext(t)
	f := newBranchFixture(t)
	spec := f.spec("i8-def456")
	writeTest(t, filepath.Join(spec.Path, "foreign"), []byte("not ours\n"))
	if _, err := EnsureBranch(ctx, spec); !errors.Is(err, ErrBranchMismatch) {
		t.Fatalf("unregistered directory adopted: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(spec.Path, "foreign")); err != nil || string(data) != "not ours\n" {
		t.Fatalf("foreign directory changed: %q %v", data, err)
	}

	other := f.spec("i9-0a0b0c")
	gitTest(t, f.clone, "worktree", "add", "-b", "elsewhere", other.Path, "HEAD")
	if _, err := EnsureBranch(ctx, other); !errors.Is(err, ErrBranchMismatch) {
		t.Fatalf("worktree on another branch adopted: %v", err)
	}

	linked := f.spec("i10-123456")
	if err := os.MkdirAll(filepath.Join(filepath.Dir(f.state), "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(filepath.Dir(f.state), "real"), filepath.Join(f.state, "linked")); err != nil {
		t.Fatal(err)
	}
	linked.Path = filepath.Join(f.state, "linked", "i10-123456")
	if _, err := EnsureBranch(ctx, linked); err == nil {
		t.Fatal("path below a symlink accepted")
	}
}

func TestBranchRemoveRefusesDirtyAndUnpushed(t *testing.T) {
	ctx := branchContext(t)
	f := newBranchFixture(t)
	spec := f.spec("i11-aaaaaa")
	if _, err := EnsureBranch(ctx, spec); err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(spec.Path, "change"), []byte("dirty\n"))
	if err := RemoveBranch(ctx, spec, false); !errors.Is(err, ErrDirty) {
		t.Fatalf("dirty checkout removed: %v", err)
	}
	gitTest(t, spec.Path, "add", ".")
	gitTest(t, spec.Path, "commit", "-m", "change")
	if err := RemoveBranch(ctx, spec, false); !errors.Is(err, ErrUnpushed) {
		t.Fatalf("unpushed branch removed: %v", err)
	}
	gitTest(t, spec.Path, "push", "origin", "agent/i11-aaaaaa")
	if err := RemoveBranch(ctx, spec, false); err != nil {
		t.Fatalf("pushed clean branch: %v", err)
	}
	if _, err := os.Stat(spec.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree still present: %v", err)
	}
	if exists, err := branchExists(ctx, f.clone, spec.Branch); err != nil || exists {
		t.Fatalf("local branch kept: %v %v", exists, err)
	}

	forced := f.spec("i12-bbbbbb")
	if _, err := EnsureBranch(ctx, forced); err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(forced.Path, "change"), []byte("dirty\n"))
	gitTest(t, forced.Path, "add", ".")
	gitTest(t, forced.Path, "commit", "-m", "change")
	writeTest(t, filepath.Join(forced.Path, "more"), []byte("dirty\n"))
	if err := RemoveBranch(ctx, forced, true); err != nil {
		t.Fatalf("operator force: %v", err)
	}
	if entries, err := listWorktrees(ctx, f.clone); err != nil || len(entries) != 1 {
		t.Fatalf("worktrees left: %+v %v", entries, err)
	}
}
