package ghext

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// gitTimeout bounds one read-only git command in the worktree.
const gitTimeout = 30 * time.Second

// git runs read-only git commands in a Ticket worktree. It carries no
// credential, and it runs in the controller, outside the worker's sandbox
// and permission checks, so no command may execute anything the worker's
// repository configures. Only plumbing that never refreshes the index,
// filters content or contacts a remote is used: `git status` would run the
// worker's fsmonitor and clean filters.
type git struct{ env []string }

// newGit builds the environment from the controller's and extra, without
// any GIT_* variable: they route git to another repository, inject
// configuration or name programs to run.
func newGit(extra []string) git {
	env := make([]string, 0, len(os.Environ())+len(extra)+2)
	for _, list := range [][]string{os.Environ(), extra} {
		for _, entry := range list {
			if !strings.HasPrefix(entry, "GIT_") {
				env = append(env, entry)
			}
		}
	}
	// A read must not take the index lock the worker's own git commands
	// may need, nor fetch missing objects through the worker's remotes.
	return git{env: append(env, "GIT_OPTIONAL_LOCKS=0", "GIT_NO_LAZY_FETCH=1")}
}

func (g git) run(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	policy := []string{"-C", dir, "-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false", "-c", "protocol.allow=never"}
	command := exec.CommandContext(ctx, "git", append(policy, args...)...)
	command.Env = g.env
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", args[0], message)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// committedHead returns the worktree's HEAD commit after checking that it
// is on branch and that no tracked file has uncommitted changes, which a
// push would silently leave behind.
func (g git) committedHead(ctx context.Context, worktree, branch string) (string, error) {
	ref, err := g.run(ctx, worktree, "symbolic-ref", "--quiet", "HEAD")
	if err != nil || ref != "refs/heads/"+branch {
		return "", fmt.Errorf("the worktree is not on branch %s; check it out (HEAD is %q)", branch, ref)
	}
	sha, err := g.run(ctx, worktree, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", fmt.Errorf("branch %s has no commit yet: %w", branch, err)
	}
	dirty, err := g.changed(ctx, worktree)
	if err != nil {
		return "", err
	}
	if len(dirty) > 0 {
		return "", fmt.Errorf("tracked files have uncommitted changes; commit them first:\n%s", strings.Join(dirty, "\n"))
	}
	return sha, nil
}

// changed lists tracked paths that differ from HEAD: staged changes, and
// worktree files that differ from the index. diff-files reports every file
// whose stat data is stale without reading it; each such file's blob ID and
// mode are then computed here from its raw bytes and compared with the
// index, so a file that was only touched is clean. A file with a filter or
// line-ending conversion and stale stat data reads as changed until the
// worker's own git refreshes the index.
func (g git) changed(ctx context.Context, worktree string) ([]string, error) {
	staged, err := g.run(ctx, worktree, "diff-index", "--cached", "--name-only", "-z", "--ignore-submodules=all", "HEAD", "--")
	if err != nil {
		return nil, err
	}
	dirty := split(staged)
	stale, err := g.run(ctx, worktree, "diff-files", "--name-only", "-z", "--ignore-submodules=all", "--")
	if err != nil || stale == "" {
		return dirty, err
	}
	entries, err := g.run(ctx, worktree, "ls-files", "--stage", "-z")
	if err != nil {
		return nil, err
	}
	type entry struct{ mode, object string }
	indexed := make(map[string]entry)
	for _, line := range split(entries) {
		// "<mode> <object> <stage>\t<path>"
		meta, path, found := strings.Cut(line, "\t")
		if fields := strings.Fields(meta); found && len(fields) == 3 {
			indexed[path] = entry{mode: fields[0], object: fields[1]}
		}
	}
	for _, path := range split(stale) {
		want := indexed[path]
		mode, object, err := blob(filepath.Join(worktree, path), len(want.object))
		if err != nil || mode != want.mode || object != want.object {
			dirty = append(dirty, path)
		}
	}
	return dirty, nil
}

// blob returns the git mode and blob ID of a worktree file as git would
// store it without filters, in the object format whose IDs have idLength
// hex digits.
func blob(path string, idLength int) (string, string, error) {
	var hasher hash.Hash
	switch idLength {
	case sha1.Size * 2:
		hasher = sha1.New()
	case sha256.Size * 2:
		hasher = sha256.New()
	default:
		return "", "", fmt.Errorf("unknown object ID length %d", idLength)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", "", err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return "", "", err
		}
		fmt.Fprintf(hasher, "blob %d\x00%s", len(target), target)
		return "120000", hex.EncodeToString(hasher.Sum(nil)), nil
	case !info.Mode().IsRegular():
		return "", "", fmt.Errorf("%s is not a file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer file.Close()
	fmt.Fprintf(hasher, "blob %d\x00", info.Size())
	if _, err := io.Copy(hasher, file); err != nil {
		return "", "", err
	}
	mode := "100644"
	if info.Mode()&0o111 != 0 {
		mode = "100755"
	}
	return mode, hex.EncodeToString(hasher.Sum(nil)), nil
}

// split separates NUL-terminated git output.
func split(output string) []string {
	var out []string
	for item := range strings.SplitSeq(output, "\x00") {
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}
