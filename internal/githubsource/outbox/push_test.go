package outbox

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
)

func (w *world) push(ticket model.TicketID, callID, sha string) model.OutboxIntent {
	w.t.Helper()
	return w.enqueue(ticket, callID, model.EffectPush, model.EffectPayload{SHA: sha})
}

// §13 push lease and head checks: pushes publish the exact commit under a
// lease of the previous push; a branch moved by someone else is unknown
// and never overwritten.
func TestPushLeaseAndForeignMove(t *testing.T) {
	w := newWorld(t)
	ticket := w.launch(issueThread(w))
	branch := model.TicketBranch(ticket)
	worktree := w.worktree(ticket)

	first := w.commit(worktree, "a.txt", "one\n")
	intent := w.push(ticket, "push-1", first)
	if intent.Payload.LeaseSHA != "" || intent.Payload.Branch != branch {
		t.Fatalf("first push payload = %+v, want the ticket branch with no lease", intent.Payload)
	}
	w.drain()
	if got := w.intent(ticket, intent.ID); got.State != model.EffectConfirmed || got.Result.SHA != first || w.remoteHead(branch) != first {
		t.Fatalf("first push = %+v, remote %s", got, w.remoteHead(branch))
	}

	// A rewrite is published too: the lease, not fast-forward, guards it.
	w.git(worktree, "commit", "--quiet", "--amend", "-m", "reworded")
	second := w.git(worktree, "rev-parse", "HEAD")
	intent = w.push(ticket, "push-2", second)
	if intent.Payload.LeaseSHA != first {
		t.Fatalf("lease = %q, want the previous push %s", intent.Payload.LeaseSHA, first)
	}
	w.drain()
	if got := w.intent(ticket, intent.ID); got.State != model.EffectConfirmed || w.remoteHead(branch) != second {
		t.Fatalf("second push = %+v, remote %s", got, w.remoteHead(branch))
	}

	// Someone else moves the branch. The next push's lease no longer
	// holds: the outbox reports unknown and leaves their commit in place.
	foreign := w.commit(w.clone, "foreign.txt", "theirs\n")
	w.git(w.clone, "push", "--quiet", "--force", "origin", foreign+":refs/heads/"+branch)
	third := w.commit(worktree, "b.txt", "two\n")
	intent = w.push(ticket, "push-3", third)
	w.drain()
	got := w.intent(ticket, intent.ID)
	if got.State != model.EffectUnknown || !strings.Contains(got.LastError, "moved outside the outbox") {
		t.Fatalf("push over a foreign move = %+v, want unknown", got)
	}
	if w.remoteHead(branch) != foreign {
		t.Fatalf("remote = %s, want the foreign commit untouched", w.remoteHead(branch))
	}
}

// §13 restart between attempt, push and settlement: the branch on GitHub
// decides; nothing is pushed blindly.
func TestPushRestartBoundaries(t *testing.T) {
	for _, point := range []string{"attempted", "decided"} {
		t.Run(point, func(t *testing.T) {
			w := newWorld(t)
			ticket := w.launch(issueThread(w))
			branch := model.TicketBranch(ticket)
			sha := w.commit(w.worktree(ticket), "a.txt", "one\n")
			intent := w.push(ticket, "push-1", sha)
			w.outbox.hook = func(at string, _ model.OutboxIntent) error {
				if at == point {
					return errCrash
				}
				return nil
			}
			if err := w.publish(); !errors.Is(err, errCrash) {
				t.Fatalf("publish = %v, want the crash", err)
			}
			w.restart()
			if err := w.publish(); err != nil {
				t.Fatal(err)
			}
			got := w.intent(ticket, intent.ID)
			switch point {
			case "attempted":
				// Not pushed: the branch is still absent, which proves it.
				if got.State != model.EffectPending || w.remoteHead(branch) != "" {
					t.Fatalf("after reconcile = %+v, remote %q", got, w.remoteHead(branch))
				}
				w.clock.Advance(time.Hour)
				w.drain()
				if got := w.intent(ticket, intent.ID); got.State != model.EffectConfirmed || got.Result.Reconciled {
					t.Fatalf("retry = %+v, want a confirmed push", got)
				}
			case "decided":
				if got.State != model.EffectConfirmed || !got.Result.Reconciled || got.Result.SHA != sha {
					t.Fatalf("after reconcile = %+v, want confirmed at %s", got, sha)
				}
			}
			if w.remoteHead(branch) != sha {
				t.Fatalf("remote = %s, want %s", w.remoteHead(branch), sha)
			}
		})
	}
}

func TestPushRefusesUnknownCommit(t *testing.T) {
	w := newWorld(t)
	ticket := w.launch(issueThread(w))
	intent := w.push(ticket, "push-1", strings.Repeat("ab", 20))
	w.drain()
	if got := w.intent(ticket, intent.ID); got.State != model.EffectFailed || !strings.Contains(got.LastError, "not in the ticket worktree") {
		t.Fatalf("intent = %+v, want failed", got)
	}
}

func (w *world) openPR(ticket model.TicketID, callID, sha string) model.OutboxIntent {
	w.t.Helper()
	return w.enqueue(ticket, callID, model.EffectOpenPR, model.EffectPayload{SHA: sha, Title: "Fix the widget", Body: "Fixes #1."})
}

func TestOpenPRBindsAfterPush(t *testing.T) {
	w := newWorld(t)
	ticket := w.launch(issueThread(w))
	sha := w.commit(w.worktree(ticket), "a.txt", "one\n")

	// Without the push first, no pull request is opened.
	early := w.openPR(ticket, "early", sha)
	w.drain()
	if got := w.intent(ticket, early.ID); got.State != model.EffectFailed || !strings.Contains(got.LastError, "not on GitHub") {
		t.Fatalf("open_pr before push = %+v, want failed", got)
	}

	w.push(ticket, "push-1", sha)
	intent := w.openPR(ticket, "open-1", sha)
	if intent.Payload.Base != "main" {
		t.Fatalf("base = %q, want the configured base", intent.Payload.Base)
	}
	w.drain()
	got := w.intent(ticket, intent.ID)
	pulls := w.repo.Pulls()
	if got.State != model.EffectConfirmed || len(pulls) != 1 || int64(pulls[0].Number) != got.Result.RemoteID || pulls[0].Head.SHA != sha {
		t.Fatalf("intent = %+v, pulls = %+v", got, pulls)
	}
	if !strings.Contains(pulls[0].Body, model.OutboxMarker(intent.ID)) || pulls[0].User.ID != botUser.ID || pulls[0].Head.Ref != model.TicketBranch(ticket) {
		t.Fatalf("pull = %+v, want the account's marked PR from the ticket branch", pulls[0])
	}
	prs, err := w.store.Ticket(w.ctx, ticket)
	if err != nil || len(prs.PRs) != 1 || !prs.PRs[0].Own || prs.PRs[0].Number != pulls[0].Number {
		t.Fatalf("ticket PRs = %+v %v, want the PR bound as own", prs.PRs, err)
	}
}

// §13 open_pr reconciliation: a lost reply is found by marker; a 422 for
// an already open PR of the branch confirms that PR. One PR either way.
func TestOpenPRReconcile(t *testing.T) {
	w := newWorld(t)
	ticket := w.launch(issueThread(w))
	sha := w.commit(w.worktree(ticket), "a.txt", "one\n")
	w.push(ticket, "push-1", sha)
	w.drain()

	w.writes.LoseAck(http.MethodPost, "/repos/"+repoName+"/pulls", 0, 1)
	lost := w.openPR(ticket, "open-1", sha)
	w.drain()
	got := w.intent(ticket, lost.ID)
	if got.State != model.EffectConfirmed || !got.Result.Reconciled || len(w.repo.Pulls()) != 1 {
		t.Fatalf("lost open_pr = %+v, pulls %d", got, len(w.repo.Pulls()))
	}

	// A second open_pr for the same branch meets GitHub's one-open-PR rule.
	again := w.openPR(ticket, "open-2", sha)
	w.drain()
	if got := w.intent(ticket, again.ID); got.State != model.EffectConfirmed || got.Result.RemoteID != int64(w.repo.Pulls()[0].Number) {
		t.Fatalf("second open_pr = %+v, want the existing PR", got)
	}
	if n := len(w.repo.Pulls()); n != 1 {
		t.Fatalf("pulls = %d, want 1", n)
	}

	// Crash after the request: the next process finds it by marker.
	w.repo.Close(w.repo.Pulls()[0].Number)
	next := w.commit(w.worktree(ticket), "b.txt", "two\n")
	w.push(ticket, "push-2", next)
	w.drain()
	third := w.openPR(ticket, "open-3", next)
	w.outbox.hook = func(at string, intent model.OutboxIntent) error {
		if at == "decided" && intent.ID == third.ID {
			return errCrash
		}
		return nil
	}
	if err := w.publish(); !errors.Is(err, errCrash) {
		t.Fatalf("publish = %v, want the crash", err)
	}
	w.restart()
	w.drain()
	got = w.intent(ticket, third.ID)
	pulls := w.repo.Pulls()
	if got.State != model.EffectConfirmed || !got.Result.Reconciled || len(pulls) != 2 || got.Result.RemoteID != int64(pulls[1].Number) {
		t.Fatalf("open_pr after restart = %+v, pulls %+v", got, pulls)
	}
}

func TestUpdatePR(t *testing.T) {
	w := newWorld(t)
	ticket := w.launch(issueThread(w))
	sha := w.commit(w.worktree(ticket), "a.txt", "one\n")
	w.push(ticket, "push-1", sha)
	opened := w.openPR(ticket, "open-1", sha)
	w.drain()
	number := int(w.intent(ticket, opened.ID).Result.RemoteID)

	// The edit lands and its reply is lost: reading the PR confirms it.
	w.writes.LoseAck(http.MethodPatch, fmt.Sprintf("/repos/%s/pulls/%d", repoName, number), 0, 1)
	update := w.enqueue(ticket, "update-1", model.EffectUpdatePR, model.EffectPayload{Number: number, Title: "Fix the widget properly"})
	w.drain()
	got := w.intent(ticket, update.ID)
	if got.State != model.EffectConfirmed || !got.Result.Reconciled || w.repo.Pulls()[0].Title != "Fix the widget properly" {
		t.Fatalf("update = %+v, pull %+v", got, w.repo.Pulls()[0])
	}
}
