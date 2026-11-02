package outbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
	"go.harness.dev/harness/internal/procgroup"
)

const (
	// pushTimeout bounds one git push or object check.
	pushTimeout = 5 * time.Minute
	// maxGitOutput bounds what a git subprocess may print.
	maxGitOutput = 1 << 20
	gitWaitDelay = 2 * time.Second
)

// droppedVariables never reach git: token variables would override the
// account's GH_CONFIG_DIR, and git routing variables would point git at
// another repository than the Ticket's worktree.
var droppedVariables = []string{
	"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN",
	"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_TERMINAL_PROMPT", "GIT_ASKPASS", "SSH_ASKPASS",
}

// gitRunner runs git as the account: the only credential git can use is
// `gh auth git-credential` under the account's GH_CONFIG_DIR. The
// environment is the controller's, never a worker session's.
type gitRunner struct {
	env        []string
	credential string
	remote     string
}

func newGitRunner(account model.AccountConfig, remote string, extra []string) gitRunner {
	env := make([]string, 0, len(os.Environ())+len(extra)+4)
	for _, list := range [][]string{os.Environ(), extra} {
		for _, entry := range list {
			key, _, _ := strings.Cut(entry, "=")
			if strings.HasPrefix(key, "GH_") || slices.Contains(droppedVariables, key) {
				continue
			}
			env = append(env, entry)
		}
	}
	env = append(env,
		"GH_CONFIG_DIR="+account.GHConfigDir,
		"GH_PROMPT_DISABLED=1",
		"GH_NO_UPDATE_NOTIFIER=1",
		"GIT_TERMINAL_PROMPT=0",
	)
	return gitRunner{
		env:        env,
		credential: "!'" + strings.ReplaceAll(account.GHPath, "'", `'\''`) + "' auth git-credential",
		remote:     remote,
	}
}

// run executes git in dir with hooks disabled and the account credential
// as the only helper. It returns stdout, stderr and the exit error.
func (g gitRunner) run(ctx context.Context, dir string, args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()
	full := append([]string{"-C", dir, "-c", "core.hooksPath=" + os.DevNull, "-c", "credential.helper=", "-c", "credential.helper=" + g.credential}, args...)
	command := exec.CommandContext(ctx, "git", full...)
	command.Env = g.env
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &capped{buffer: &stdout}, &capped{buffer: &stderr}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return procgroup.Signal(command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = gitWaitDelay
	err := command.Run()
	if command.Process != nil {
		// Nothing git started (ssh, credential helper) outlives the call.
		_ = procgroup.Signal(command.Process.Pid, syscall.SIGKILL)
	}
	if err != nil && ctx.Err() != nil {
		err = fmt.Errorf("%w: %w", err, ctx.Err())
	}
	return stdout.String(), stderr.String(), err
}

type capped struct{ buffer *bytes.Buffer }

func (c *capped) Write(p []byte) (int, error) {
	if room := maxGitOutput - c.buffer.Len(); room > 0 {
		c.buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// pushResult is what git push reported for the Ticket's ref.
type pushResult int

const (
	// pushUnreported: no status line for the ref (connection, auth or
	// local failure); the remote may or may not have the update.
	pushUnreported pushResult = iota
	pushDone
	// pushStale: the remote ref did not match the lease.
	pushStale
	// pushRefused: the remote refused the update (hook, protection).
	pushRefused
)

// pushStatus reads `git push --porcelain` output: one
// "<flag>\t<from>:<to>\t<summary>" line per ref.
func pushStatus(stdout, ref string) (pushResult, string) {
	for line := range strings.SplitSeq(stdout, "\n") {
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) < 3 || !strings.HasSuffix(fields[1], ":"+ref) {
			continue
		}
		summary := strings.TrimSpace(fields[2])
		switch strings.TrimSpace(fields[0]) {
		case "", "+", "*", "=":
			return pushDone, summary
		case "!":
			if strings.Contains(summary, "stale info") {
				return pushStale, summary
			}
			if strings.HasPrefix(summary, "[remote rejected]") || strings.HasPrefix(summary, "[rejected]") {
				return pushRefused, summary
			}
		}
		return pushUnreported, summary
	}
	return pushUnreported, ""
}

// attemptPush updates the Ticket's branch on the remote to the exact
// commit, from the Ticket's worktree, with the control-filled lease: the
// remote must still hold the previous push (or no branch for the first).
func (o *Outbox) attemptPush(ctx context.Context, intent model.OutboxIntent) step {
	payload := intent.Payload
	ticket, err := o.store.Ticket(ctx, intent.Ticket)
	if err != nil {
		return o.absent(intent, fmt.Errorf("read ticket: %w", err), 0)
	}
	if payload.Branch != model.TicketBranch(intent.Ticket) || payload.SHA == "" {
		return failed(fmt.Errorf("push of %q to %q is not the ticket's branch update", payload.SHA, payload.Branch))
	}
	if ticket.Worktree == "" {
		return failed(errors.New("the ticket has no worktree to push from"))
	}
	if _, stderr, err := o.git.run(ctx, ticket.Worktree, "cat-file", "-e", payload.SHA+"^{commit}"); err != nil {
		if ctx.Err() != nil {
			return unread(ctx.Err())
		}
		return failed(fmt.Errorf("commit %s is not in the ticket worktree: %s", payload.SHA, gitMessage(stderr, err)))
	}
	ref := "refs/heads/" + payload.Branch
	stdout, stderr, err := o.git.run(ctx, ticket.Worktree, "push", "--porcelain", "--no-verify",
		"--force-with-lease="+ref+":"+payload.LeaseSHA, o.git.remote, payload.SHA+":"+ref)
	if ctx.Err() != nil {
		return unread(ctx.Err())
	}
	result, summary := pushStatus(stdout, ref)
	switch {
	case err == nil && result == pushDone:
		return confirmed(&model.EffectReply{SHA: payload.SHA})
	case result == pushRefused:
		return failed(fmt.Errorf("remote refused the push: %s", summary))
	}
	// Stale lease or an unreported result: what the branch holds decides.
	decided := o.reconcilePush(ctx, intent)
	if decided.state != model.EffectConfirmed && err != nil {
		decided.err = fmt.Errorf("git push: %s; %w", gitMessage(stderr, err), decided.err)
	}
	return decided
}

// reconcilePush reads the branch on GitHub. At the pushed SHA it is
// confirmed; at the lease (or absent for a first push) the push is proven
// not applied; anywhere else someone else moved it, which only an operator
// can sort out.
func (o *Outbox) reconcilePush(ctx context.Context, intent model.OutboxIntent) step {
	payload := intent.Payload
	sha, exists, err := o.branchHead(ctx, payload.Branch)
	switch {
	case err != nil:
		return unread(err)
	case exists && sha == payload.SHA:
		return confirmed(&model.EffectReply{SHA: sha, Reconciled: true})
	case !exists && payload.LeaseSHA == "", exists && sha == payload.LeaseSHA:
		return o.absent(intent, errors.New("reconciled: the branch was not updated"), 0)
	case !exists:
		return unknown(fmt.Errorf("branch %s was deleted on GitHub; the push expected it at %s", payload.Branch, payload.LeaseSHA))
	}
	lease := payload.LeaseSHA
	if lease == "" {
		lease = "no branch"
	}
	return unknown(fmt.Errorf("branch %s is at %s on GitHub, neither the pushed %s nor the lease %s: it was moved outside the outbox", payload.Branch, sha, payload.SHA, lease))
}

// branchHead reads a branch's commit from GitHub's git refs.
func (o *Outbox) branchHead(ctx context.Context, branch string) (string, bool, error) {
	response, err := o.client.Do(ctx, model.GHRequest{Method: http.MethodGet, Path: fmt.Sprintf("repos/%s/git/ref/heads/%s", o.repo, branch)})
	if status(err) == http.StatusNotFound {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read branch %s: %w", branch, err)
	}
	var ref struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(response.Body, &ref); err != nil || ref.Object.SHA == "" {
		return "", false, fmt.Errorf("read branch %s: unreadable reply", branch)
	}
	if ref.Ref != "refs/heads/"+branch {
		// GitHub answers a prefix with an array of refs, never a near miss
		// as the exact one.
		return "", false, fmt.Errorf("read branch %s: GitHub returned %s", branch, ref.Ref)
	}
	return ref.Object.SHA, true, nil
}

func gitMessage(stderr string, err error) string {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
		return last
	}
	return err.Error()
}
