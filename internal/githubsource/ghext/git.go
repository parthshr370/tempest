package ghext

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// gitTimeout bounds one read-only git command in the worktree.
const gitTimeout = 30 * time.Second

// routingVariables would point git at another repository than the
// worktree.
var routingVariables = []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES"}

// git runs read-only git commands in a Ticket worktree. It carries no
// credential: these commands never reach a remote.
type git struct{ env []string }

func newGit(extra []string) git {
	env := make([]string, 0, len(os.Environ())+len(extra)+1)
	for _, list := range [][]string{os.Environ(), extra} {
		for _, entry := range list {
			key, _, _ := strings.Cut(entry, "=")
			if !slices.Contains(routingVariables, key) {
				env = append(env, entry)
			}
		}
	}
	// A status read must not take the index lock the worker's own git
	// commands may need.
	return git{env: append(env, "GIT_OPTIONAL_LOCKS=0")}
}

func (g git) run(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.hooksPath=" + os.DevNull}, args...)...)
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
	dirty, err := g.run(ctx, worktree, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return "", err
	}
	if dirty != "" {
		return "", fmt.Errorf("tracked files have uncommitted changes; commit them first:\n%s", dirty)
	}
	return sha, nil
}
