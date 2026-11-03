package outbox

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/githubsource/ghfake"
	"go.harness.dev/harness/internal/githubsource/model"
)

func issueThread(w *world) model.ThreadRef {
	return model.ThreadRef{Number: w.repo.OpenIssue(parth, "Fix the widget", "@tempest-bot please fix")}
}

func TestCommentPostedOnceWithMarker(t *testing.T) {
	w := newWorld(t)
	thread := issueThread(w)
	ticket := w.launch(thread)
	intent := w.enqueue(ticket, "call-1", model.EffectComment, model.EffectPayload{Body: "On it."})
	w.drain()

	got := w.intent(ticket, intent.ID)
	if got.State != model.EffectConfirmed || got.Result == nil || got.Result.Reconciled || got.Result.RemoteID == 0 || got.Result.URL == "" {
		t.Fatalf("intent = %+v, want confirmed with the posted comment", got)
	}
	posted := w.marked(thread.Number, intent.ID)
	if len(posted) != 1 || posted[0].User.ID != botUser.ID || !strings.HasPrefix(posted[0].Body, "On it.\n\n") || posted[0].ID != got.Result.RemoteID {
		t.Fatalf("comments = %+v, want one marked comment by the account", posted)
	}
	// A settled intent is not due again: nothing more is sent.
	w.drain()
	if n := len(w.requests(http.MethodPost, "/comments")); n != 1 {
		t.Fatalf("POSTs = %d, want 1", n)
	}
}

// §13 "no replay of unknown comments": GitHub applied the POST and the
// reply was lost. The outbox finds the marker and never posts again.
func TestCommentLostAckReconciledNotResent(t *testing.T) {
	for name, status := range map[string]int{"dropped connection": 0, "gateway error": http.StatusBadGateway} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			thread := issueThread(w)
			ticket := w.launch(thread)
			w.writes.LoseAck(http.MethodPost, "/repos/"+repoName+"/issues/1/comments", status, 1)
			intent := w.enqueue(ticket, "call-1", model.EffectComment, model.EffectPayload{Body: "Done."})
			w.drain()

			got := w.intent(ticket, intent.ID)
			if got.State != model.EffectConfirmed || got.Result == nil || !got.Result.Reconciled {
				t.Fatalf("intent = %+v, want confirmed by reconciliation", got)
			}
			if posted := w.marked(thread.Number, intent.ID); len(posted) != 1 || posted[0].ID != got.Result.RemoteID {
				t.Fatalf("comments = %+v, want exactly one", posted)
			}
			if n := len(w.requests(http.MethodPost, "/comments")); n != 1 {
				t.Fatalf("POSTs = %d, want 1", n)
			}
		})
	}
}

// §13 restart at every boundary: intent committed and attempting, then the
// process dies before the request, after the request, and after the
// settlement was made durable but not acknowledged.
func TestCommentRestartAtEveryBoundary(t *testing.T) {
	t.Run("after attempt before request", func(t *testing.T) {
		w := newWorld(t)
		thread := issueThread(w)
		ticket := w.launch(thread)
		intent := w.enqueue(ticket, "call-1", model.EffectComment, model.EffectPayload{Body: "hello"})
		w.outbox.hook = func(point string, _ model.OutboxIntent) error {
			if point == "attempted" {
				return errCrash
			}
			return nil
		}
		if err := w.publish(); !errors.Is(err, errCrash) {
			t.Fatalf("publish = %v, want the crash", err)
		}
		w.restart()
		// Nothing was sent, but a death after the request leaves the same
		// journal and the same listing. The listing may lag, so the first
		// read that does not find it settles nothing.
		if err := w.publish(); err != nil {
			t.Fatal(err)
		}
		if got := w.intent(ticket, intent.ID); got.State != model.EffectAttempting {
			t.Fatalf("after one read: %+v, want attempting", got)
		}
		// Still not found after the window: unknown, for an operator.
		w.reread()
		got := w.intent(ticket, intent.ID)
		if got.State != model.EffectUnknown || !strings.Contains(got.LastError, "operator") || !strings.Contains(got.LastError, fmt.Sprintf("not found in %d reads", absentReads)) {
			t.Fatalf("after reconcile: %+v, want unknown", got)
		}
		if elapsed := got.SettledAt.Sub(got.AttemptedAt); elapsed < absentWindow {
			t.Fatalf("settled unknown %s after the attempt, want at least %s", elapsed, absentWindow)
		}
		w.clock.Advance(24 * time.Hour)
		w.drain()
		if n := len(w.requests(http.MethodPost, "/comments")); n != 0 {
			t.Fatalf("POSTs = %d, want none: unknown is never resent", n)
		}
	})
	t.Run("after request before settle", func(t *testing.T) {
		w := newWorld(t)
		thread := issueThread(w)
		ticket := w.launch(thread)
		intent := w.enqueue(ticket, "call-1", model.EffectComment, model.EffectPayload{Body: "hello"})
		w.outbox.hook = func(point string, _ model.OutboxIntent) error {
			if point == "decided" {
				return errCrash
			}
			return nil
		}
		if err := w.publish(); !errors.Is(err, errCrash) {
			t.Fatalf("publish = %v, want the crash", err)
		}
		w.restart()
		w.drain()
		got := w.intent(ticket, intent.ID)
		if got.State != model.EffectConfirmed || !got.Result.Reconciled {
			t.Fatalf("intent = %+v, want confirmed by reconciliation", got)
		}
		if len(w.marked(thread.Number, intent.ID)) != 1 || len(w.requests(http.MethodPost, "/comments")) != 1 {
			t.Fatal("the comment was posted again")
		}
	})
	t.Run("settlement durable but unacknowledged", func(t *testing.T) {
		w := newWorld(t)
		thread := issueThread(w)
		ticket := w.launch(thread)
		intent := w.enqueue(ticket, "call-1", model.EffectComment, model.EffectPayload{Body: "hello"})
		w.crash.failOn(func(id string) bool { return strings.HasPrefix(id, "settle:") })
		if err := w.publish(); !errors.Is(err, errCrash) {
			t.Fatalf("publish = %v, want the crash", err)
		}
		w.restart()
		w.drain()
		if got := w.intent(ticket, intent.ID); got.State != model.EffectConfirmed {
			t.Fatalf("state = %s, want confirmed", got.State)
		}
		if len(w.requests(http.MethodPost, "/comments")) != 1 {
			t.Fatal("the comment was posted again")
		}
	})
}

// §13: a reply was possibly sent and its outcome cannot be read. The
// intent stays attempting while reads fail, then settles unknown; it is
// never posted again.
func TestCommentUnreadableOutcomeBecomesUnknown(t *testing.T) {
	w := newWorld(t)
	thread := issueThread(w)
	ticket := w.launch(thread)
	path := "/repos/" + repoName + "/issues/1/comments"
	w.writes.LoseAck(http.MethodPost, path, 0, 1)
	w.server.Fail(http.MethodGet, path, http.StatusInternalServerError, 100, nil)
	intent := w.enqueue(ticket, "call-1", model.EffectComment, model.EffectPayload{Body: "hello"})
	for read := 1; read <= maxReconcileReads; read++ {
		if err := w.publish(); err != nil {
			t.Fatal(err)
		}
		state := w.intent(ticket, intent.ID).State
		if read < maxReconcileReads && state != model.EffectAttempting {
			t.Fatalf("after read %d: state %s, want attempting", read, state)
		}
		w.clock.Advance(time.Hour)
	}
	got := w.intent(ticket, intent.ID)
	if got.State != model.EffectUnknown || !strings.Contains(got.LastError, "could not be read") {
		t.Fatalf("intent = %+v, want unknown", got)
	}
	w.clock.Advance(24 * time.Hour)
	w.drain()
	if n := len(w.requests(http.MethodPost, "/comments")); n != 1 {
		t.Fatalf("POSTs = %d, want the single lost one", n)
	}
	if len(w.repo.Comments(thread.Number)) != 1 {
		t.Fatal("the comment was posted again")
	}
}

// §13 marker search: it pages the whole target thread with no time bound,
// counts only the account's comments ending with the marker, and is not
// fooled by a human quoting the marker or by the marker on another thread.
func TestCommentReconcilePagesAndScopes(t *testing.T) {
	w := newWorld(t)
	thread := issueThread(w)
	other := w.repo.OpenIssue(alice, "Other", "unrelated")
	ticket := w.launch(thread)
	for range 150 {
		w.repo.Comment(thread.Number, alice, "noise")
	}
	intent := w.enqueue(ticket, "call-1", model.EffectComment, model.EffectPayload{Body: "hello"})
	marker := model.OutboxMarker(intent.ID)
	w.repo.Comment(thread.Number, alice, "quoting "+marker)
	w.repo.Comment(other, botUser, "wrong thread "+marker)

	// Crash after the attempt commit: nothing was sent, and the forged and
	// misplaced markers must not count as the write.
	w.outbox.hook = func(point string, _ model.OutboxIntent) error {
		if point == "attempted" {
			return errCrash
		}
		return nil
	}
	_ = w.publish()
	w.restart()
	w.reread()
	if got := w.intent(ticket, intent.ID); got.State != model.EffectUnknown {
		t.Fatalf("state = %s, want unknown: the forged markers are not the write", got.State)
	}
	pages := 0
	for _, request := range w.requests(http.MethodGet, "/issues/1/comments") {
		if request.Query.Get("page") == "2" {
			pages++
		}
		if request.Query.Has("since") {
			t.Fatalf("search bounded by the controller clock: %+v", request.Query)
		}
	}
	if pages == 0 {
		t.Fatal("the search did not read the second page")
	}

	// Now a real comment lands after 150 others; its reply is lost. The
	// search finds it on the second page.
	w.clock.Advance(time.Hour)
	w.writes.LoseAck(http.MethodPost, "/repos/"+repoName+"/issues/1/comments", 0, 1)
	intent = w.enqueue(ticket, "call-2", model.EffectComment, model.EffectPayload{Body: "hello"})
	w.drain()
	got := w.intent(ticket, intent.ID)
	posted := w.marked(thread.Number, intent.ID)
	var own []ghfake.Comment
	for _, comment := range posted {
		if comment.User.ID == botUser.ID {
			own = append(own, comment)
		}
	}
	if got.State != model.EffectConfirmed || !got.Result.Reconciled || len(own) != 1 || own[0].ID != got.Result.RemoteID {
		t.Fatalf("intent = %+v, own comments = %+v", got, own)
	}
}

func TestCommentRefusals(t *testing.T) {
	w := newWorld(t)
	thread := issueThread(w)
	ticket := w.launch(thread)
	path := "/repos/" + repoName + "/issues/1/comments"

	// A rate limit is a refusal for now: pending with GitHub's wait.
	w.server.Fail(http.MethodPost, path, http.StatusTooManyRequests, 1, http.Header{"Retry-After": {"120"}})
	limited := w.enqueue(ticket, "limited", model.EffectComment, model.EffectPayload{Body: "a"})
	if err := w.publish(); err != nil {
		t.Fatal(err)
	}
	got := w.intent(ticket, limited.ID)
	if got.State != model.EffectPending || got.RetryAt.Before(w.clock.Now().Add(2*time.Minute)) {
		t.Fatalf("intent = %+v, want pending until the rate limit passes", got)
	}
	w.clock.Advance(3 * time.Minute)
	w.drain()
	if got := w.intent(ticket, limited.ID); got.State != model.EffectConfirmed || got.Attempts != 2 {
		t.Fatalf("intent = %+v, want confirmed on the second attempt", got)
	}

	// A validation failure is final.
	w.server.Fail(http.MethodPost, path, http.StatusUnprocessableEntity, 1, nil)
	invalid := w.enqueue(ticket, "invalid", model.EffectComment, model.EffectPayload{Body: "b"})
	w.drain()
	if got := w.intent(ticket, invalid.ID); got.State != model.EffectFailed || got.LastError == "" {
		t.Fatalf("intent = %+v, want failed", got)
	}
}

func TestReviewReply(t *testing.T) {
	w := newWorld(t)
	number := w.repo.OpenPull(alice, ghfake.PullSpec{Title: "Feature", HeadRef: "feature", HeadSHA: "abc1234", BaseRef: "main"})
	thread := model.ThreadRef{Number: number, IsPR: true}
	root := w.repo.ReviewComment(number, alice, ghfake.ReviewCommentSpec{Body: "nit", Path: "a.go", Line: 3})
	gone := w.repo.ReviewComment(number, alice, ghfake.ReviewCommentSpec{Body: "old", Path: "b.go", Line: 1})
	reviewEvent := func(id int64) model.Event {
		return model.Event{ID: model.ReviewCommentID(id, start), Kind: model.EventReviewComment, Thread: thread, Actor: actor(alice), At: start, CommentID: id, Body: "x"}
	}
	ticket := w.launch(thread, reviewEvent(root), reviewEvent(gone))
	w.repo.DeleteReviewComment(gone)

	inThread := w.enqueue(ticket, "reply-1", model.EffectReviewReply, model.EffectPayload{ReplyTo: root, Body: "fixed"})
	fallback := w.enqueue(ticket, "reply-2", model.EffectReviewReply, model.EffectPayload{ReplyTo: gone, Body: "fixed too"})
	w.drain()

	got := w.intent(ticket, inThread.ID)
	replies := w.repo.ReviewComments(number)
	last := replies[len(replies)-1]
	if got.State != model.EffectConfirmed || last.InReplyToID != root || last.ID != got.Result.RemoteID || !strings.Contains(last.Body, model.OutboxMarker(inThread.ID)) {
		t.Fatalf("intent = %+v, last inline comment = %+v", got, last)
	}
	got = w.intent(ticket, fallback.ID)
	posted := w.marked(number, fallback.ID)
	if got.State != model.EffectConfirmed || len(posted) != 1 || posted[0].ID != got.Result.RemoteID || !strings.Contains(posted[0].Body, "fixed too") {
		t.Fatalf("fallback intent = %+v, conversation = %+v", got, posted)
	}

	// A lost reply in the review thread is found among inline comments.
	w.writes.LoseAck(http.MethodPost, fmt.Sprintf("/repos/%s/pulls/%d/comments/%d/replies", repoName, number, root), 0, 1)
	lost := w.enqueue(ticket, "reply-3", model.EffectReviewReply, model.EffectPayload{ReplyTo: root, Body: "again"})
	w.drain()
	if got := w.intent(ticket, lost.ID); got.State != model.EffectConfirmed || !got.Result.Reconciled {
		t.Fatalf("lost reply = %+v, want confirmed by reconciliation", got)
	}
	if n := len(w.requests(http.MethodPost, "/replies")); n != 3 {
		t.Fatalf("reply POSTs = %d, want 3 (one 404, two writes)", n)
	}
}
