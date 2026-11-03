package githubsource_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/githubsource"
	"go.harness.dev/harness/internal/githubsource/model"
	"go.harness.dev/harness/tempest"
	"go.harness.dev/harness/tempest/durable"
)

// A worker's shell and its background jobs do not inherit the serve
// process's GitHub or git credential selectors; other variables pass.
func TestGitHubWorkerShellDropsCredentialEnvironment(t *testing.T) {
	t.Setenv("GH_CONFIG_DIR", "/synthetic-account-config")
	t.Setenv("GH_TOKEN", "synthetic-token")
	t.Setenv("GITHUB_TOKEN", "synthetic-token")
	t.Setenv("GIT_ASKPASS", "/synthetic-askpass")
	t.Setenv("SSH_AUTH_SOCK", "/synthetic-agent.sock")
	t.Setenv("WORKER_PROBE_KEEP", "kept")
	w := newWorld(t, nil)
	issue := w.repo.OpenIssue(parth, "environment", "")
	probe := `env | grep -E '^(GH_|GITHUB_|GIT_ASKPASS|SSH_AUTH_SOCK|WORKER_PROBE_KEEP)' | sort > `
	w.workers.script(issue,
		bash(probe+"shell-env"),
		tool("bash", map[string]any{"command": probe + "job-env.tmp && mv job-env.tmp job-env", "async": true}),
		text("waiting"))
	w.mention(issue, parth, "start")
	w.poll()
	w.eventually("ticket", func() bool { return w.ticketOn(issue) != "" })
	id := w.ticketOn(issue)
	w.settled(id)
	worktree := w.detail(id).Ticket.Worktree
	for _, name := range []string{"shell-env", "job-env"} {
		path := filepath.Join(worktree, name)
		w.eventually(name, func() bool { _, err := os.Stat(path); return err == nil })
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(body)); got != "WORKER_PROBE_KEEP=kept" {
			t.Fatalf("%s: worker environment is\n%s\nwant only WORKER_PROBE_KEEP=kept", name, got)
		}
	}
}

// A client that loads a worker session can read it, but cannot run it
// outside a dispatch, before or after the Ticket stops.
func TestGitHubWorkerSessionRefusesDirectRuns(t *testing.T) {
	w := newWorld(t, nil)
	issue := w.repo.OpenIssue(parth, "direct run", "")
	w.workers.script(issue, text("waiting"))
	w.mention(issue, parth, "start")
	w.poll()
	w.eventually("ticket", func() bool { return w.ticketOn(issue) != "" })
	id := w.ticketOn(issue)
	w.settled(id)
	before := w.detail(id)
	session, err := w.serve.Resume(w.ctx, before.Ticket.Worktree, before.Ticket.SessionID, nil)
	if err != nil {
		t.Fatal(err)
	}
	direct := func(stage string) {
		t.Helper()
		refused := func(what string, err error) {
			t.Helper()
			if !errors.Is(err, githubsource.ErrWorkerSession) {
				t.Fatalf("%s: direct %s of a worker session: %v, want ErrWorkerSession", stage, what, err)
			}
		}
		_, err := session.Submit(w.ctx, tempest.Input{RequestID: "direct-" + stage, Prompt: tempest.Prompt{Text: "run"}})
		refused("submit", err)
		_, err = session.BeginContinue(w.ctx)
		refused("continue", err)
		_, err = session.FollowUp(w.ctx, tempest.QueuedInput{Key: "direct-follow-" + stage, Text: "run"})
		refused("follow-up", err)
		_, err = session.CreateTask(w.ctx, tempest.TaskRequest{ID: "direct-task-" + stage})
		refused("task", err)
		refused("rewind", session.Rewind(w.ctx, tempest.RewindOptions{Checkpoint: "any"}))
		refused("permission mode change", session.Authorize(w.ctx, "set_permission_mode"))
		if _, err := session.Read(w.ctx, durable.Query{Kind: durable.DocumentKind, Namespace: model.WorkerNamespace, Limit: 10}); err != nil {
			t.Fatalf("%s: read of a worker session: %v", stage, err)
		}
	}
	direct("open")
	if err := w.service().Control().Stop(w.ctx, model.OperatorRequest{ID: "stop", Target: string(id)}); err != nil {
		t.Fatal(err)
	}
	direct("stopped")
	after := w.detail(id)
	if len(after.Dispatches) != len(before.Dispatches) || after.Ledger.Spent != before.Ledger.Spent {
		t.Fatalf("dispatches %d -> %d, spent %+v -> %+v", len(before.Dispatches), len(after.Dispatches), before.Ledger.Spent, after.Ledger.Spent)
	}
	if got := len(w.workers.requests(issue)); got != 1 {
		t.Fatalf("worker provider requests = %d, want the one dispatch", got)
	}
}

// A client that loaded a worker during one dispatch gets the live session
// of the next dispatch, not the one the dispatcher closed.
func TestGitHubLoadedWorkerFollowsDispatches(t *testing.T) {
	w := newWorld(t, nil)
	issue := w.repo.OpenIssue(parth, "loaded worker", "")
	gate := make(chan struct{})
	w.workers.script(issue, gated(gate, text("waiting")))
	w.mention(issue, parth, "start")
	w.poll()
	w.eventually("first run", func() bool { return w.workers.running(issue) == 1 })
	id := w.ticketOn(issue)
	detail := w.detail(id)
	loaded, err := w.serve.Resume(w.ctx, detail.Ticket.Worktree, detail.Ticket.SessionID, nil)
	if err != nil {
		t.Fatal(err)
	}
	close(gate)
	w.settled(id)
	next := make(chan struct{})
	defer close(next)
	w.workers.script(issue, gated(next, text("next")))
	w.mention(issue, parth, "continue")
	w.poll()
	w.eventually("second run", func() bool { return w.workers.running(issue) == 1 })
	again, err := w.serve.Resume(w.ctx, detail.Ticket.Worktree, detail.Ticket.SessionID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if again == loaded {
		t.Fatal("resume returned the session the dispatcher closed")
	}
	if status := again.Snapshot().Run.Status; status != "running" {
		t.Fatalf("resumed worker run status = %q, want the live dispatch running", status)
	}
	held, release, err := w.serve.Acquire(detail.Ticket.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if held != again {
		t.Fatal("acquire returned a different session than resume")
	}
}
