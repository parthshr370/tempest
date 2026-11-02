package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Branch checkout errors. Callers treat ErrBranchMismatch as permanent: the
// path or branch belongs to something other than the expected checkout and
// is never adopted or repaired automatically.
var (
	ErrBranchMismatch = errors.New("worktree: path is not the expected branch checkout")
	ErrDirty          = errors.New("worktree: branch checkout has uncommitted changes")
	ErrUnpushed       = errors.New("worktree: branch has commits not on the remote")
)

// BranchSpec names a tracked author-branch worktree of a clone: Branch
// checked out at Path, started from Remote/Base.
type BranchSpec struct {
	// Clone is the absolute path of the clone that owns the worktree.
	Clone string
	// Path is the absolute worktree path. It is unique to the branch.
	Path   string
	Branch string
	// Remote defaults to "origin".
	Remote string
	// Base is the remote branch a new Branch starts from.
	Base string
	// Env is added to git's environment, for example credentials for fetch.
	Env []string
}

// BranchCheckout is an existing or new branch worktree.
type BranchCheckout struct {
	Path   string
	Branch string
	// BaseSHA is the commit the branch started from: the merge base of the
	// branch and Remote/Base.
	BaseSHA string
	// GitDir is the worktree's private administrative directory inside the
	// clone's common git directory.
	GitDir string
	// Created is true when this call created the worktree.
	Created bool
}

// worktreeEntry is one record of `git worktree list --porcelain`.
type worktreeEntry struct {
	path   string
	branch string
}

func (spec BranchSpec) remote() string {
	if spec.Remote == "" {
		return "origin"
	}
	return spec.Remote
}

func (spec BranchSpec) validate(ctx context.Context) error {
	if filepath.Clean(spec.Path) != spec.Path {
		return fmt.Errorf("worktree: path %q is not clean", spec.Path)
	}
	// git registers worktrees by resolved path; a symlinked parent would
	// make the registration never match the path callers compare.
	if parent, err := filepath.EvalSymlinks(filepath.Dir(spec.Path)); err == nil && parent != filepath.Dir(spec.Path) {
		return fmt.Errorf("worktree: path %q has a symlinked parent (%s)", spec.Path, parent)
	}
	if !filepath.IsAbs(spec.Clone) || !filepath.IsAbs(spec.Path) {
		return fmt.Errorf("worktree: clone and path must be absolute")
	}
	if spec.Branch == "" || spec.Base == "" {
		return fmt.Errorf("worktree: branch and base are required")
	}
	for _, name := range []string{spec.Branch, spec.Base, spec.remote()} {
		if strings.HasPrefix(name, "-") {
			return fmt.Errorf("worktree: invalid name %q", name)
		}
	}
	if _, err := git(ctx, spec.Clone, nil, "check-ref-format", "--branch", spec.Branch); err != nil {
		return fmt.Errorf("worktree: invalid branch %q: %w", spec.Branch, err)
	}
	return nil
}

// listWorktrees parses the clone's worktree registrations.
func listWorktrees(ctx context.Context, clone string) ([]worktreeEntry, error) {
	listing, err := git(ctx, clone, nil, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	var entries []worktreeEntry
	for _, field := range strings.Split(listing, "\x00") {
		switch {
		case strings.HasPrefix(field, "worktree "):
			entries = append(entries, worktreeEntry{path: strings.TrimPrefix(field, "worktree ")})
		case strings.HasPrefix(field, "branch ") && len(entries) > 0:
			entries[len(entries)-1].branch = strings.TrimPrefix(field, "branch ")
		}
	}
	return entries, nil
}

func registration(ctx context.Context, spec BranchSpec) (worktreeEntry, bool, error) {
	entries, err := listWorktrees(ctx, spec.Clone)
	if err != nil {
		return worktreeEntry{}, false, err
	}
	for _, entry := range entries {
		if entry.path == spec.Path {
			return entry, true, nil
		}
	}
	return worktreeEntry{}, false, nil
}

func commonDir(ctx context.Context, dir string) (string, error) {
	common, err := git(ctx, dir, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(common)
}

func branchExists(ctx context.Context, clone, branch string) (bool, error) {
	cmd := command(ctx, clone, nil, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exit interface{ ExitCode() int }
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git show-ref: %w", err)
}

// verifyCheckout proves an existing directory is the clone's registered
// worktree of spec.Branch at spec.Path.
func verifyCheckout(ctx context.Context, spec BranchSpec, common string) (string, error) {
	info, err := os.Lstat(spec.Path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: %s is not a directory", ErrBranchMismatch, spec.Path)
	}
	entry, registered, err := registration(ctx, spec)
	if err != nil {
		return "", err
	}
	if !registered {
		return "", fmt.Errorf("%w: %s is not registered in %s", ErrBranchMismatch, spec.Path, spec.Clone)
	}
	if entry.branch != "refs/heads/"+spec.Branch {
		return "", fmt.Errorf("%w: %s has %q checked out, want %s", ErrBranchMismatch, spec.Path, entry.branch, spec.Branch)
	}
	own, err := commonDir(ctx, spec.Path)
	if err != nil || own != common {
		return "", fmt.Errorf("%w: %s belongs to another repository", ErrBranchMismatch, spec.Path)
	}
	head, err := git(ctx, spec.Path, nil, "symbolic-ref", "-q", "HEAD")
	if err != nil || head != "refs/heads/"+spec.Branch {
		return "", fmt.Errorf("%w: %s HEAD is %q, want %s", ErrBranchMismatch, spec.Path, head, spec.Branch)
	}
	return git(ctx, spec.Path, nil, "rev-parse", "--absolute-git-dir")
}

// baseOf is the merge base of the branch and the remote-tracking base, or
// empty when the tracking ref is unknown.
func baseOf(ctx context.Context, spec BranchSpec) string {
	base, err := git(ctx, spec.Clone, nil, "merge-base", "refs/heads/"+spec.Branch, "refs/remotes/"+spec.remote()+"/"+spec.Base)
	if err != nil {
		return ""
	}
	return base
}

// EnsureBranch adopts or creates the worktree of spec.Branch at spec.Path.
// An existing directory is adopted only when the clone has it registered at
// that path with that branch checked out; anything else at the path is
// ErrBranchMismatch and is left untouched. A registration whose directory
// is gone is pruned. A new worktree checks out the existing local branch, or
// creates the branch from a fresh fetch of Remote/Base without upstream
// tracking, so nothing pushes it implicitly.
func EnsureBranch(ctx context.Context, spec BranchSpec) (BranchCheckout, error) {
	if err := spec.validate(ctx); err != nil {
		return BranchCheckout{}, err
	}
	common, err := commonDir(ctx, spec.Clone)
	if err != nil {
		return BranchCheckout{}, err
	}
	checkout := BranchCheckout{Path: spec.Path, Branch: spec.Branch}
	if _, err := os.Lstat(spec.Path); err == nil {
		gitDir, err := verifyCheckout(ctx, spec, common)
		if err != nil {
			return BranchCheckout{}, err
		}
		checkout.GitDir, checkout.BaseSHA = gitDir, baseOf(ctx, spec)
		return checkout, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return BranchCheckout{}, err
	}
	if _, registered, err := registration(ctx, spec); err != nil {
		return BranchCheckout{}, err
	} else if registered {
		if _, err := git(ctx, spec.Clone, nil, "worktree", "remove", "--force", spec.Path); err != nil {
			return BranchCheckout{}, err
		}
	}
	remote := spec.remote()
	tracking := "refs/remotes/" + remote + "/" + spec.Base
	if _, err := git(ctx, spec.Clone, spec.Env, "fetch", "--no-tags", "--", remote, "+refs/heads/"+spec.Base+":"+tracking); err != nil {
		return BranchCheckout{}, err
	}
	baseSHA, err := git(ctx, spec.Clone, nil, "rev-parse", "--verify", tracking+"^{commit}")
	if err != nil {
		return BranchCheckout{}, err
	}
	if err := os.MkdirAll(filepath.Dir(spec.Path), 0o700); err != nil {
		return BranchCheckout{}, err
	}
	exists, err := branchExists(ctx, spec.Clone, spec.Branch)
	if err != nil {
		return BranchCheckout{}, err
	}
	if exists {
		if _, err := git(ctx, spec.Clone, nil, "worktree", "add", "--", spec.Path, spec.Branch); err != nil {
			return BranchCheckout{}, err
		}
		baseSHA = baseOf(ctx, spec)
	} else if _, err := git(ctx, spec.Clone, nil, "worktree", "add", "--no-track", "-b", spec.Branch, "--", spec.Path, baseSHA); err != nil {
		return BranchCheckout{}, err
	}
	gitDir, err := verifyCheckout(ctx, spec, common)
	if err != nil {
		return BranchCheckout{}, err
	}
	checkout.GitDir, checkout.BaseSHA, checkout.Created = gitDir, baseSHA, true
	return checkout, nil
}

// unpushed counts commits of the branch reachable from no remote-tracking
// ref of the spec's remote.
func unpushed(ctx context.Context, spec BranchSpec) (int, error) {
	count, err := git(ctx, spec.Clone, nil, "rev-list", "--count", "refs/heads/"+spec.Branch, "--not", "--remotes="+spec.remote())
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(count)
}

// RemoveBranch removes the branch worktree at spec.Path and deletes the
// local branch. Without force it refuses a checkout with uncommitted or
// untracked changes (ErrDirty) and a branch with commits that no
// remote-tracking ref of the remote contains (ErrUnpushed); force is the
// operator's decision to discard them. A path that is not the registered
// checkout of the branch is ErrBranchMismatch even with force.
func RemoveBranch(ctx context.Context, spec BranchSpec, force bool) error {
	if err := spec.validate(ctx); err != nil {
		return err
	}
	common, err := commonDir(ctx, spec.Clone)
	if err != nil {
		return err
	}
	present := false
	if _, err := os.Lstat(spec.Path); err == nil {
		if _, err := verifyCheckout(ctx, spec, common); err != nil {
			return err
		}
		present = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	exists, err := branchExists(ctx, spec.Clone, spec.Branch)
	if err != nil {
		return err
	}
	if !force {
		if present {
			dirty, err := hasStatus(ctx, spec.Path, false)
			if err != nil {
				return err
			}
			if dirty {
				return fmt.Errorf("%w: %s", ErrDirty, spec.Path)
			}
		}
		if exists {
			count, err := unpushed(ctx, spec)
			if err != nil {
				return err
			}
			if count > 0 {
				return fmt.Errorf("%w: %s has %d", ErrUnpushed, spec.Branch, count)
			}
		}
	}
	if _, registered, err := registration(ctx, spec); err != nil {
		return err
	} else if registered {
		if _, err := git(ctx, spec.Clone, nil, "worktree", "remove", "--force", spec.Path); err != nil {
			return err
		}
	}
	if exists {
		if _, err := git(ctx, spec.Clone, nil, "branch", "-D", "--", spec.Branch); err != nil {
			return err
		}
	}
	return nil
}
