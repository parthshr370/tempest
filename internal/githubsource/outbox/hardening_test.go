package outbox

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/githubsource/ghfake"
	"go.harness.dev/harness/internal/githubsource/model"
)

// script writes an executable that appends its name and arguments to
// capture, then runs tail (shell text) if any.
func script(t *testing.T, dir, name, capture, tail string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	text := "#!/bin/sh\nprintf '%s %s\\n' " + quote(name) + " \"$*\" >> " + quote(capture) + "\n" + tail + "\n"
	if err := os.WriteFile(path, []byte(text), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func crashAt(w *world, point string) {
	w.t.Helper()
	w.outbox.hook = func(at string, _ model.OutboxIntent) error {
		if at == point {
			return errCrash
		}
		return nil
	}
	if err := w.publish(); !errors.Is(err, errCrash) {
		w.t.Fatalf("publish = %v, want the crash at %s", err, point)
	}
}

// P1-1: the worker owns its repository's configuration and may own parts
// of the controller's environment. Nothing from either runs or redirects
// the push: not a remote's receive-pack or push URL, URL rewrites, hooks,
// fsmonitor, credential helpers, ssh commands or GIT_* variables.
func TestPushRunsNothingFromTheWorker(t *testing.T) {
	w := newWorld(t)
	ticket := w.launch(issueThread(w))
	worktree := w.worktree(ticket)
	sha := w.commit(worktree, "a.txt", "change\n")
	capture := filepath.Join(w.dir, "executed")
	tools := filepath.Join(w.dir, "worker-tools")
	hooks := filepath.Join(w.dir, "worker-hooks")
	for _, dir := range []string{tools, hooks} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, hook := range []string{"pre-push", "reference-transaction", "post-update", "pre-receive", "update", "fsmonitor-watchman"} {
		script(t, hooks, hook, capture, "exit 0")
	}
	other := filepath.Join(w.dir, "unintended.git")
	w.git(w.dir, "init", "--quiet", "--bare", other)
	include := filepath.Join(w.dir, "worker-include")
	if err := os.WriteFile(include, []byte("[core]\n\tsshCommand = "+script(t, tools, "ssh", capture, "exit 1")+"\n[uploadpack]\n\tpackObjectsHook = "+script(t, tools, "pack-hook", capture, "exit 1")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"remote.origin.pushurl":                other,
		"remote.origin.receivepack":            script(t, tools, "receive-pack", capture, "exit 1"),
		"remote.origin.uploadpack":             script(t, tools, "upload-pack", capture, "exit 1"),
		"url." + other + ".insteadOf":          w.bare,
		"url." + other + ".pushInsteadOf":      w.bare,
		"core.hooksPath":                       hooks,
		"core.fsmonitor":                       script(t, tools, "fsmonitor", capture, "exit 1"),
		"credential.helper":                    "!" + script(t, tools, "credential", capture, "exit 1"),
		"filter.worker.process":                script(t, tools, "filter", capture, "exit 1"),
		"core.alternateRefsCommand":            script(t, tools, "alternate-refs", capture, "exit 1"),
		"include.path":                         include,
		"protocol.ext.allow":                   "always",
		"uploadpack.allowFilter":               "true",
		"transfer.credentialsInUrl":            "allow",
		"core.askPass":                         script(t, tools, "askpass", capture, "exit 1"),
		"http." + w.bare + ".extraHeader":      "X-Worker: yes",
		"remote.origin.vcs":                    "worker",
		"extensions.worktreeConfig":            "false",
		"maintenance.auto":                     "true",
		"gc.auto":                              "1",
		"remote.origin.mirror":                 "true",
		"remote.origin.tagOpt":                 "--tags",
		"push.default":                         "matching",
		"push.recurseSubmodules":               "on-demand",
		"submodule.recurse":                    "true",
		"core.gitProxy":                        script(t, tools, "git-proxy", capture, "exit 1"),
		"diff.external":                        script(t, tools, "diff", capture, "exit 1"),
		"sequence.editor":                      script(t, tools, "editor", capture, "exit 1"),
		"gpg.program":                          script(t, tools, "gpg", capture, "exit 1"),
		"push.gpgSign":                         "true",
		"core.pager":                           script(t, tools, "pager", capture, "exit 1"),
		"credential.https://github.com.helper": "!" + script(t, tools, "credential-scoped", capture, "exit 1"),
	} {
		w.git(worktree, "config", key, value)
	}
	// The worker cannot set the controller's environment, but nothing
	// from it may route git either.
	execPath := filepath.Join(w.dir, "worker-exec")
	if err := os.MkdirAll(execPath, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, helper := range []string{"git-receive-pack", "git-upload-pack", "git-remote-https", "git-remote-http"} {
		script(t, execPath, helper, capture, "exit 1")
	}
	t.Setenv("GIT_EXEC_PATH", execPath)
	t.Setenv("GIT_SSH_COMMAND", script(t, tools, "ssh-env", capture, "exit 1"))
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.fsmonitor")
	t.Setenv("GIT_CONFIG_VALUE_0", script(t, tools, "fsmonitor-env", capture, "exit 1"))
	t.Setenv("GIT_DIR", other)
	w.restart()
	intent := w.push(ticket, "push-1", sha)
	w.drain()
	for _, key := range []string{"GIT_EXEC_PATH", "GIT_SSH_COMMAND", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "GIT_DIR"} {
		os.Unsetenv(key)
	}

	if ran, err := os.ReadFile(capture); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worker-controlled programs ran during the push: %s %v", ran, err)
	}
	got := w.intent(ticket, intent.ID)
	if got.State != model.EffectConfirmed || got.Result.SHA != sha || w.remoteHead(model.TicketBranch(ticket)) != sha {
		t.Fatalf("push = %+v, configured repository at %q, want confirmed at %s", got, w.remoteHead(model.TicketBranch(ticket)), sha)
	}
	if out := w.git(other, "for-each-ref"); out != "" {
		t.Fatalf("the worker's push URL received refs: %s", out)
	}
}

// P1-1: over HTTP the only credential git asks is the account's gh, run
// with the account's GH_CONFIG_DIR; the worker's helpers and headers are
// never used.
func TestPushCredentialIsTheAccountHelperOnly(t *testing.T) {
	w := newWorld(t)
	ticket := w.launch(issueThread(w))
	worktree := w.worktree(ticket)
	sha := w.commit(worktree, "a.txt", "change\n")
	capture := filepath.Join(w.dir, "executed")
	helper := script(t, w.dir, "account-gh", capture, "[ \"$3\" = get ] && printf 'username=synthetic-account\\npassword=SYNTHETIC-SECRET\\n'; printf 'dir=%s\\n' \"$GH_CONFIG_DIR\" >> "+quote(capture))
	var mu sync.Mutex
	var authorizations, headers []string
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, request *http.Request) {
		mu.Lock()
		authorizations = append(authorizations, request.Header.Get("Authorization"))
		headers = append(headers, request.Header.Get("X-Worker"))
		mu.Unlock()
		rw.Header().Set("WWW-Authenticate", `Basic realm="synthetic"`)
		rw.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	w.git(worktree, "config", "credential.helper", "!"+script(t, w.dir, "worker-credential", capture, "printf 'username=worker\\npassword=worker\\n'"))
	w.git(worktree, "config", "credential."+server.URL+".helper", "!"+script(t, w.dir, "worker-scoped", capture, "exit 0"))
	w.git(worktree, "config", "http."+server.URL+".extraHeader", "X-Worker: yes")
	w.config.Account.GHPath = helper
	w.restart()
	w.outbox.git.url, w.outbox.git.protocol = server.URL+"/acme/widgets.git", "http"

	intent := w.push(ticket, "push-1", sha)
	w.drain()

	ran, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ran), "worker-") {
		t.Fatalf("a worker credential helper ran:\n%s", ran)
	}
	if !strings.Contains(string(ran), "account-gh auth git-credential get") || !strings.Contains(string(ran), "dir="+w.config.Account.GHConfigDir+"\n") {
		t.Fatalf("account helper runs:\n%s\nwant gh auth git-credential under %s", ran, w.config.Account.GHConfigDir)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("synthetic-account:SYNTHETIC-SECRET"))
	mu.Lock()
	defer mu.Unlock()
	sawAccount := false
	for i, authorization := range authorizations {
		sawAccount = sawAccount || authorization == want
		if authorization != "" && authorization != want || headers[i] != "" {
			t.Fatalf("request %d: authorization %q, worker header %q", i, authorization, headers[i])
		}
	}
	if !sawAccount {
		t.Fatalf("authorizations = %q, want the account's", authorizations)
	}
	// Refused by the server and the branch absent on GitHub: not applied.
	if got := w.intent(ticket, intent.ID); got.State != model.EffectPending {
		t.Fatalf("push = %+v, want pending after a refused push", got)
	}
}

// P1-2: a comment whose request may have reached GitHub is found by its
// marker in the whole thread, or settles unknown. It is never posted twice,
// whatever the controller's clock, GitHub's indexing or a human's edit.
func TestAmbiguousCommentIsFoundOrUnknown(t *testing.T) {
	for _, tc := range []struct {
		name string
		want model.EffectState
	}{
		{"controller clock ahead", model.EffectConfirmed},
		{"delayed listing", model.EffectUnknown},
		{"marker edited away", model.EffectUnknown},
		{"marker kept by an edit", model.EffectConfirmed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			thread := issueThread(w)
			if tc.name == "controller clock ahead" {
				w.clock.Advance(3 * time.Hour)
			}
			ticket := w.launch(thread)
			if tc.name == "delayed listing" {
				w.repo.SetIndexDelay(time.Hour)
			}
			intent := w.enqueue(ticket, "call-1", model.EffectComment, model.EffectPayload{Body: "original"})
			crashAt(w, "decided")
			posted := w.repo.Comments(thread.Number)[0]
			switch tc.name {
			case "marker edited away":
				w.repo.EditComment(posted.ID, "human corrected text")
			case "marker kept by an edit":
				w.repo.EditComment(posted.ID, "human corrected text\n\n"+model.OutboxMarker(intent.ID))
			}
			w.restart()
			w.drain()
			got := w.intent(ticket, intent.ID)
			if got.State != tc.want {
				t.Fatalf("intent = %+v, want %s", got, tc.want)
			}
			w.clock.Advance(24 * time.Hour)
			w.repo.Advance(24 * time.Hour)
			w.drain()
			if comments, posts := len(w.repo.Comments(thread.Number)), len(w.requests(http.MethodPost, "/comments")); comments != 1 || posts != 1 {
				t.Fatalf("comments = %d, POSTs = %d, want the one write", comments, posts)
			}
		})
	}
}

// P1-2: a PR whose open may have reached GitHub and whose body a human
// rewrote and closed is not opened again; the operator is told about it.
func TestAmbiguousPROpenIsFoundOrUnknown(t *testing.T) {
	w := newWorld(t)
	ticket := w.launch(issueThread(w))
	sha := w.commit(w.worktree(ticket), "a.txt", "change\n")
	w.push(ticket, "push", sha)
	w.drain()
	intent := w.openPR(ticket, "open", sha)
	crashAt(w, "decided")
	first := w.repo.Pulls()[0]
	w.repo.EditIssue(first.Number, "Human rewritten PR body")
	w.repo.Close(first.Number)
	w.restart()
	w.drain()
	got := w.intent(ticket, intent.ID)
	if got.State != model.EffectUnknown || !strings.Contains(got.LastError, fmt.Sprintf("#%d (closed)", first.Number)) {
		t.Fatalf("intent = %+v, want unknown naming #%d", got, first.Number)
	}
	w.clock.Advance(24 * time.Hour)
	w.drain()
	if n := len(w.repo.Pulls()); n != 1 {
		t.Fatalf("PRs = %d, want 1", n)
	}
}

// A marker counts only as the last line of the account's comment in the
// intended review thread: a quoted marker or one in another thread of the
// same PR does not confirm a write that was never sent.
func TestMarkerScope(t *testing.T) {
	t.Run("quoted by the account", func(t *testing.T) {
		w := newWorld(t)
		thread := issueThread(w)
		ticket := w.launch(thread)
		intent := w.enqueue(ticket, "call-1", model.EffectComment, model.EffectPayload{Body: "intended"})
		w.repo.Comment(thread.Number, botUser, "Quoted diagnostic:\n```\n"+model.OutboxMarker(intent.ID)+"\n```")
		crashAt(w, "attempted")
		w.restart()
		w.drain()
		if got := w.intent(ticket, intent.ID); got.State != model.EffectUnknown {
			t.Fatalf("intent = %+v, want unknown", got)
		}
	})
	t.Run("another review thread", func(t *testing.T) {
		w := newWorld(t)
		number := w.repo.OpenPull(alice, ghfake.PullSpec{Title: "PR", HeadRef: "feature", HeadSHA: "abc1234", BaseRef: "main"})
		thread := model.ThreadRef{Number: number, IsPR: true}
		root := w.repo.ReviewComment(number, alice, ghfake.ReviewCommentSpec{Body: "root", Path: "a.go", Line: 1})
		event := model.Event{ID: model.ReviewCommentID(root, start), Kind: model.EventReviewComment, Thread: thread, Actor: actor(alice), At: start, CommentID: root}
		ticket := w.launch(thread, event)
		intent := w.enqueue(ticket, "call-1", model.EffectReviewReply, model.EffectPayload{ReplyTo: root, Body: "intended"})
		other := w.repo.ReviewComment(number, alice, ghfake.ReviewCommentSpec{Body: "other root", Path: "b.go", Line: 1})
		w.repo.ReviewComment(number, botUser, ghfake.ReviewCommentSpec{Body: marked("elsewhere", intent.ID), Path: "b.go", Line: 1, InReplyTo: other})
		crashAt(w, "attempted")
		w.restart()
		w.drain()
		if got := w.intent(ticket, intent.ID); got.State != model.EffectUnknown {
			t.Fatalf("intent = %+v, want unknown", got)
		}
	})
}

// GitHub's replies endpoint takes only a thread's root: an answer to a
// reply is posted to its root.
func TestReviewReplyToAReplyGoesToTheRoot(t *testing.T) {
	w := newWorld(t)
	number := w.repo.OpenPull(alice, ghfake.PullSpec{Title: "Feature", HeadRef: "feature", HeadSHA: "abc1234", BaseRef: "main"})
	thread := model.ThreadRef{Number: number, IsPR: true}
	root := w.repo.ReviewComment(number, alice, ghfake.ReviewCommentSpec{Body: "root", Path: "a.go", Line: 1})
	reply := w.repo.ReviewComment(number, alice, ghfake.ReviewCommentSpec{Body: "follow-up", Path: "a.go", Line: 1, InReplyTo: root})
	event := model.Event{ID: model.ReviewCommentID(reply, start), Kind: model.EventReviewComment, Thread: thread, Actor: actor(alice), At: start, CommentID: reply, InReplyTo: root}
	ticket := w.launch(thread, event)
	intent := w.enqueue(ticket, "reply", model.EffectReviewReply, model.EffectPayload{ReplyTo: reply, Body: "answer"})
	w.drain()
	got := w.intent(ticket, intent.ID)
	comments := w.repo.ReviewComments(number)
	last := comments[len(comments)-1]
	if got.State != model.EffectConfirmed || last.ID != got.Result.RemoteID || last.InReplyToID != root {
		t.Fatalf("intent = %+v, last inline comment = %+v, want a reply in root %d's thread", got, last, root)
	}
	if posts := w.requests(http.MethodPost, fmt.Sprintf("/comments/%d/replies", root)); len(posts) != 1 {
		t.Fatalf("replies to the root = %d, want 1", len(posts))
	}
}

// Retry-After holds the account: no Ticket sends anything until it passes,
// and an outcome read waits for it too.
func TestRateLimitHoldsTheAccount(t *testing.T) {
	t.Run("write", func(t *testing.T) {
		w := newWorld(t)
		first := w.launch(issueThread(w))
		limited := w.enqueue(first, "call-1", model.EffectComment, model.EffectPayload{Body: "one"})
		w.server.Fail(http.MethodPost, "/repos/"+repoName+"/issues/1/comments", http.StatusTooManyRequests, 1, http.Header{"Retry-After": {"120"}})
		if err := w.publish(); err != nil {
			t.Fatal(err)
		}
		second := w.launch(issueThread(w))
		waiting := w.enqueue(second, "call-1", model.EffectComment, model.EffectPayload{Body: "two"})
		w.clock.Advance(time.Minute)
		if err := w.publish(); err != nil {
			t.Fatal(err)
		}
		if n := len(w.requests(http.MethodPost, "/comments")); n != 1 {
			t.Fatalf("POSTs inside the account's wait = %d, want only the refused one", n)
		}
		if got := w.intent(second, waiting.ID); got.State != model.EffectPending || got.Attempts != 0 {
			t.Fatalf("second ticket = %+v, want untouched", got)
		}
		w.clock.Advance(time.Minute + time.Second)
		w.drain()
		for ticket, id := range map[model.TicketID]model.OutboxID{first: limited.ID, second: waiting.ID} {
			if got := w.intent(ticket, id); got.State != model.EffectConfirmed {
				t.Fatalf("intent = %+v, want confirmed after the wait", got)
			}
		}
	})
	t.Run("outcome read", func(t *testing.T) {
		w := newWorld(t)
		ticket := w.launch(issueThread(w))
		path := "/repos/" + repoName + "/issues/1/comments"
		w.writes.LoseAck(http.MethodPost, path, 0, 1)
		w.server.Fail(http.MethodGet, path, http.StatusTooManyRequests, 1, http.Header{"Retry-After": {"300"}})
		intent := w.enqueue(ticket, "call-1", model.EffectComment, model.EffectPayload{Body: "one"})
		if err := w.publish(); err != nil {
			t.Fatal(err)
		}
		reads := len(w.requests(http.MethodGet, path))
		w.clock.Advance(2 * time.Minute)
		if err := w.publish(); err != nil {
			t.Fatal(err)
		}
		if n := len(w.requests(http.MethodGet, path)); n != reads {
			t.Fatalf("reads inside the wait = %d, want %d", n, reads)
		}
		w.clock.Advance(4 * time.Minute)
		w.drain()
		if got := w.intent(ticket, intent.ID); got.State != model.EffectConfirmed || !got.Result.Reconciled {
			t.Fatalf("intent = %+v, want confirmed by the later read", got)
		}
	})
}

type blockingClient struct {
	model.GHClient
	path             string
	entered, release chan struct{}
	once             sync.Once
}

func (c *blockingClient) Do(ctx context.Context, request model.GHRequest) (model.GHResponse, error) {
	if request.Method == http.MethodPost && request.Path == c.path {
		c.once.Do(func() { close(c.entered) })
		select {
		case <-c.release:
		case <-ctx.Done():
			return model.GHResponse{}, ctx.Err()
		}
	}
	return c.GHClient.Do(ctx, request)
}

// A Ticket's slow write holds only that Ticket.
func TestTicketsPublishIndependently(t *testing.T) {
	w := newWorld(t)
	slow := w.launch(issueThread(w))
	fast := w.launch(issueThread(w))
	w.enqueue(slow, "call-1", model.EffectComment, model.EffectPayload{Body: "slow"})
	quick := w.enqueue(fast, "call-1", model.EffectComment, model.EffectPayload{Body: "fast"})
	client := &blockingClient{GHClient: w.outbox.client, path: fmt.Sprintf("repos/%s/issues/1/comments", repoName), entered: make(chan struct{}), release: make(chan struct{})}
	w.outbox.client = client
	done := make(chan error, 1)
	go func() { done <- w.publish() }()
	<-client.entered
	deadline := time.Now().Add(30 * time.Second)
	for w.intent(fast, quick.ID).State != model.EffectConfirmed {
		if time.Now().After(deadline) {
			close(client.release)
			t.Fatal("the second ticket waited for the first ticket's request")
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(client.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
