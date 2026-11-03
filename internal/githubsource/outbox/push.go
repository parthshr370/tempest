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
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
	"go.harness.dev/harness/internal/procgroup"
)

const (
	// pushTimeout bounds one git import or push.
	pushTimeout = 5 * time.Minute
	// maxGitOutput bounds what a git subprocess may print.
	maxGitOutput = 1 << 20
	gitWaitDelay = 2 * time.Second
	// bareConfig is the whole configuration of the outbox's repository. It
	// is rewritten before every git command, so nothing written into the
	// file between commands is ever read.
	bareConfig = "[core]\n\trepositoryformatversion = 0\n\tbare = true\n"
)

// keptVariables are the only controller variables git sees: the executable
// search path, scratch space, and the operator's proxy settings. Every GIT_*
// variable is dropped: they inject configuration (GIT_CONFIG_*), programs
// (GIT_SSH*, GIT_EXEC_PATH, GIT_ASKPASS) and object routing.
var keptVariables = []string{
	"PATH", "TMPDIR",
	"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy",
}

// gitRunner runs the outbox's git: credential-free imports of a Ticket's
// commit into an outbox-owned bare repository, and credential-bearing pushes
// from that repository to the configured GitHub repository.
//
// No git command the outbox runs reads a worker-writable configuration with
// the credential attached. The worker's repository is touched only by the
// import, as the source of a local fetch: git-upload-pack runs there with
// the worker's configuration but no credential, no lazy fetch and no
// network protocol. The push runs in the bare repository, whose
// configuration is rewritten first, with no system or global configuration,
// hooks and fsmonitor off, only HTTPS allowed, and `gh auth git-credential`
// under the account's GH_CONFIG_DIR as the only credential helper. Its
// destination is a URL built from Config.Repository, never a remote name.
//
// Same-UID workers can still edit files of the controller between two
// commands; only a separate OS account closes that.
type gitRunner struct {
	git string
	// dir is the outbox's bare repository; home is git's HOME.
	dir, home string
	env       []string
	// credential is the helper command line: the account's gh with its
	// configuration directory and the operator's HOME, which gh's keyring
	// lookup may need. Only the helper receives them.
	credential string
	// url is the push destination and protocol the protocol it needs.
	// Tests point both at a local repository.
	url, protocol string

	mu *sync.Mutex
}

func newGitRunner(config model.Config, owner, name string) (gitRunner, error) {
	path, err := exec.LookPath("git")
	if err != nil {
		return gitRunner{}, fmt.Errorf("outbox: find git: %w", err)
	}
	if path, err = filepath.Abs(path); err != nil {
		return gitRunner{}, fmt.Errorf("outbox: find git: %w", err)
	}
	if !filepath.IsAbs(config.StateDir) {
		return gitRunner{}, fmt.Errorf("outbox: stateDir %q is not absolute: it holds the outbox's push repository", config.StateDir)
	}
	root := filepath.Join(config.StateDir, "outbox")
	runner := gitRunner{
		git: path, dir: filepath.Join(root, "push.git"), home: filepath.Join(root, "home"),
		url: "https://github.com/" + owner + "/" + name + ".git", protocol: "https",
		mu: &sync.Mutex{},
	}
	runner.env = []string{
		"HOME=" + runner.home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_NO_LAZY_FETCH=1",
		"GIT_PROTOCOL_FROM_USER=0",
		"LC_ALL=C",
	}
	for _, key := range keptVariables {
		if value, ok := os.LookupEnv(key); ok {
			runner.env = append(runner.env, key+"="+value)
		}
	}
	account := config.Account
	assignments := []string{"GH_CONFIG_DIR=" + quote(account.GHConfigDir), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1"}
	if home, ok := os.LookupEnv("HOME"); ok {
		assignments = append(assignments, "HOME="+quote(home))
	}
	runner.credential = "!" + strings.Join(assignments, " ") + " " + quote(account.GHPath) + " auth git-credential"
	return runner, nil
}

// quote makes s one sh word.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// policy is the configuration every outbox git command runs with on top of
// the rewritten repository file. Command-line values win over any file.
func (g gitRunner) policy() []string {
	return []string{
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "core.fsmonitor=false",
		"-c", "core.sshCommand=false",
		"-c", "credential.helper=",
		"-c", "protocol.allow=never",
		"-c", "gc.auto=0",
		"-c", "maintenance.auto=false",
	}
}

// prepare creates the bare repository on first use and rewrites its
// configuration and alternates, which could otherwise be planted between
// commands.
func (g gitRunner) prepare(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := os.MkdirAll(g.home, 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(g.dir, "HEAD")); errors.Is(err, os.ErrNotExist) {
		if _, stderr, err := g.exec(ctx, append(g.policy(), "init", "--quiet", "--bare", "--template=", g.dir)...); err != nil {
			return fmt.Errorf("create %s: %s", g.dir, gitMessage(stderr, err))
		}
	} else if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(g.dir, "objects", "info", "alternates")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	staged := filepath.Join(g.dir, "config.outbox")
	if err := os.WriteFile(staged, []byte(bareConfig), 0o600); err != nil {
		return err
	}
	return os.Rename(staged, filepath.Join(g.dir, "config"))
}

// importCommit copies the exact commit and its history from a worktree into
// the bare repository under the branch's ref, running nothing with the
// credential.
func (g gitRunner) importCommit(ctx context.Context, worktree, sha, ref string) error {
	if err := g.prepare(ctx); err != nil {
		return fmt.Errorf("prepare the push repository: %w", err)
	}
	args := append(g.policy(),
		"-c", "protocol.file.allow=always",
		"-c", "uploadpack.allowAnySHA1InWant=true",
		"--git-dir", g.dir, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules", "--no-auto-maintenance",
		"--", worktree, "+"+sha+":"+ref)
	if _, stderr, err := g.exec(ctx, args...); err != nil {
		return errors.New(gitMessage(stderr, err))
	}
	return nil
}

// push updates ref on GitHub to sha under the lease, from the bare
// repository, as the account.
func (g gitRunner) push(ctx context.Context, sha, ref, lease string) (string, string, error) {
	if err := g.prepare(ctx); err != nil {
		return "", "", fmt.Errorf("prepare the push repository: %w", err)
	}
	args := append(g.policy(),
		"-c", "protocol."+g.protocol+".allow=always",
		"-c", "credential.helper="+g.credential,
		"--git-dir", g.dir, "push", "--porcelain", "--no-verify", "--force-with-lease="+ref+":"+lease,
		"--", g.url, sha+":"+ref)
	return g.exec(ctx, args...)
}

// exec runs git and returns stdout, stderr and the exit error.
func (g gitRunner) exec(ctx context.Context, args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, g.git, args...)
	command.Dir = filepath.Dir(g.dir)
	command.Env = g.env
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &capped{buffer: &stdout}, &capped{buffer: &stderr}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return procgroup.Signal(command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = gitWaitDelay
	err := command.Run()
	if command.Process != nil {
		// Nothing git started (upload-pack, credential helper) outlives
		// the call. The group is already gone after a clean exit.
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

// attemptPush imports the exact commit from the Ticket's worktree and
// pushes it to the Ticket's branch with the control-filled lease: GitHub
// must still hold the previous push (or no branch for the first). A push
// is conditional and repeatable, so a push proven not applied is retried.
// Success is what GitHub's REST API reports for the configured repository,
// not what git printed.
func (o *Outbox) attemptPush(ctx context.Context, intent model.OutboxIntent) step {
	payload := intent.Payload
	ticket, err := o.store.Ticket(ctx, intent.Ticket)
	if err != nil {
		return o.retry(intent, fmt.Errorf("read ticket: %w", err), 0)
	}
	if payload.Branch != model.TicketBranch(intent.Ticket) || payload.SHA == "" {
		return failed(fmt.Errorf("push of %q to %q is not the ticket's branch update", payload.SHA, payload.Branch))
	}
	if ticket.Worktree == "" {
		return failed(errors.New("the ticket has no worktree to push from"))
	}
	ref := "refs/heads/" + payload.Branch
	if err := o.git.importCommit(ctx, ticket.Worktree, payload.SHA, ref); err != nil {
		if ctx.Err() != nil {
			return unread(ctx.Err())
		}
		return failed(fmt.Errorf("commit %s could not be imported from the ticket worktree: %w", payload.SHA, err))
	}
	stdout, stderr, err := o.git.push(ctx, payload.SHA, ref, payload.LeaseSHA)
	if ctx.Err() != nil {
		return unread(ctx.Err())
	}
	result, summary := pushStatus(stdout, ref)
	if result == pushRefused {
		return failed(fmt.Errorf("remote refused the push: %s", summary))
	}
	decided := o.reconcilePush(ctx, intent)
	if decided.state == model.EffectConfirmed && result == pushDone {
		decided.result.Reconciled = false
	}
	if decided.state != model.EffectConfirmed && err != nil {
		decided.err = errors.Join(fmt.Errorf("git push: %s", gitMessage(stderr, err)), decided.err)
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
		return o.retry(intent, errors.New("reconciled: the branch was not updated"), 0)
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
	response, err := o.do(ctx, model.GHRequest{Method: http.MethodGet, Path: fmt.Sprintf("repos/%s/git/ref/heads/%s", o.repo, branch)})
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
