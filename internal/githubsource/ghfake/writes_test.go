package ghfake

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/githubsource/model"
)

func (f repoFixture) write(method, path string, body any) (model.GHResponse, error) {
	f.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.client.Do(context.Background(), model.GHRequest{Method: method, Path: path, Body: raw})
}

func decodeInto[T any](t *testing.T, response model.GHResponse) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(response.Body, &out); err != nil {
		t.Fatalf("decode: %v: %s", err, response.Body)
	}
	return out
}

func ghStatus(err error) (int, bool) {
	var ghErr *model.GHError
	if errors.As(err, &ghErr) {
		return ghErr.Status, true
	}
	return 0, false
}

func TestWritesCommentAndLostAck(t *testing.T) {
	f := newRepoFixture(t)
	writes := f.repo.ServeWrites(f.server, "")
	number := f.repo.OpenIssue(alice, "bug", "body")

	response, err := f.write(http.MethodPost, "repos/o/r/issues/1/comments", map[string]string{"body": "first"})
	if err != nil || response.Status != http.StatusCreated {
		t.Fatalf("POST = %d %v", response.Status, err)
	}
	posted := decodeInto[Comment](t, response)
	if posted.User.ID != account.ID || posted.Body != "first" || posted.HTMLURL == "" {
		t.Fatalf("comment = %+v, want the account's", posted)
	}

	// A dropped connection after the write: gh prints no status line.
	writes.LoseAck(http.MethodPost, "/repos/o/r/issues/1/comments", 0, 1)
	_, err = f.write(http.MethodPost, "repos/o/r/issues/1/comments", map[string]string{"body": "second"})
	if status, ok := ghStatus(err); !ok || status != 0 {
		t.Fatalf("lost ack error = %v, want a GHError with no status", err)
	}
	// A gateway error after the write.
	writes.LoseAck(http.MethodPost, "/repos/o/r/issues/1/comments", http.StatusBadGateway, 1)
	_, err = f.write(http.MethodPost, "repos/o/r/issues/1/comments", map[string]string{"body": "third"})
	if status, _ := ghStatus(err); status != http.StatusBadGateway {
		t.Fatalf("gateway error = %v, want 502", err)
	}
	comments := f.repo.Comments(number)
	if len(comments) != 3 || comments[1].Body != "second" || comments[2].Body != "third" {
		t.Fatalf("comments = %+v, want every write applied", comments)
	}
	if _, err := f.write(http.MethodPost, "repos/o/r/issues/99/comments", map[string]string{"body": "x"}); err == nil {
		t.Fatal("comment on a missing issue succeeded")
	}
}

func TestWritesReviewReplies(t *testing.T) {
	f := newRepoFixture(t)
	f.repo.ServeWrites(f.server, "")
	number := f.repo.OpenPull(alice, PullSpec{Title: "feature", HeadRef: "feature", HeadSHA: "abc1234"})
	root := f.repo.ReviewComment(number, alice, ReviewCommentSpec{Body: "nit", Path: "a.go", Line: 2})
	response, err := f.write(http.MethodPost, "repos/o/r/pulls/1/comments/"+itoa(root)+"/replies", map[string]string{"body": "fixed"})
	if err != nil {
		t.Fatal(err)
	}
	reply := decodeInto[ReviewComment](t, response)
	// A reply to a reply joins the root's thread.
	response, err = f.write(http.MethodPost, "repos/o/r/pulls/1/comments/"+itoa(reply.ID)+"/replies", map[string]string{"body": "again"})
	if err != nil {
		t.Fatal(err)
	}
	nested := decodeInto[ReviewComment](t, response)
	if reply.InReplyToID != root || nested.InReplyToID != root || nested.Path != "a.go" || nested.User.ID != account.ID {
		t.Fatalf("replies = %+v %+v, want both in root %d's thread", reply, nested, root)
	}
	f.repo.DeleteReviewComment(root)
	_, err = f.write(http.MethodPost, "repos/o/r/pulls/1/comments/"+itoa(root)+"/replies", map[string]string{"body": "late"})
	wantStatus(t, "reply to a deleted comment", err, http.StatusNotFound)
	if got := len(f.repo.ReviewComments(number)); got != 2 {
		t.Fatalf("review comments = %d, want 2", got)
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+dir, "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// bareRepo returns a bare repository with main and feature branches.
func bareRepo(t *testing.T) (bare, mainSHA, featureSHA string) {
	t.Helper()
	dir := t.TempDir()
	bare, work := filepath.Join(dir, "remote.git"), filepath.Join(dir, "work")
	gitIn(t, dir, "init", "--quiet", "--bare", "-b", "main", bare)
	gitIn(t, dir, "init", "--quiet", "-b", "main", work)
	gitIn(t, work, "commit", "--quiet", "--allow-empty", "-m", "base")
	mainSHA = gitIn(t, work, "rev-parse", "HEAD")
	gitIn(t, work, "commit", "--quiet", "--allow-empty", "-m", "feature")
	featureSHA = gitIn(t, work, "rev-parse", "HEAD")
	gitIn(t, work, "push", "--quiet", bare, mainSHA+":refs/heads/main", featureSHA+":refs/heads/agent/i1-abc")
	return bare, mainSHA, featureSHA
}

func TestWritesPullsAndRefsFromBareRepo(t *testing.T) {
	f := newRepoFixture(t)
	bare, mainSHA, featureSHA := bareRepo(t)
	f.repo.ServeWrites(f.server, bare)

	var ref refReply
	f.get("repos/o/r/git/ref/heads/agent/i1-abc", nil, &ref)
	if ref.Ref != "refs/heads/agent/i1-abc" || ref.Object.SHA != featureSHA {
		t.Fatalf("ref = %+v, want %s", ref, featureSHA)
	}
	_, err := f.client.Do(context.Background(), model.GHRequest{Path: "repos/o/r/git/ref/heads/agent/missing"})
	wantStatus(t, "missing ref", err, http.StatusNotFound)

	request := map[string]any{"title": "Fix", "body": "body", "head": "o:agent/i1-abc", "base": "main", "draft": true}
	response, err := f.write(http.MethodPost, "repos/o/r/pulls", request)
	if err != nil || response.Status != http.StatusCreated {
		t.Fatalf("create = %d %v", response.Status, err)
	}
	created := decodeInto[Pull](t, response)
	if created.Head.SHA != featureSHA || created.Base.SHA != mainSHA || !created.Draft || created.User.ID != account.ID {
		t.Fatalf("pull = %+v", created)
	}
	// One open PR per head and base.
	_, err = f.write(http.MethodPost, "repos/o/r/pulls", request)
	wantStatus(t, "duplicate", err, http.StatusUnprocessableEntity)
	request["head"] = "agent/unpushed"
	_, err = f.write(http.MethodPost, "repos/o/r/pulls", request)
	wantStatus(t, "unpushed head", err, http.StatusUnprocessableEntity)

	other := f.repo.OpenPull(alice, PullSpec{Title: "other", HeadRef: "elsewhere", HeadSHA: "abc1234"})
	f.repo.Close(other)
	var listed []Pull
	f.get("repos/o/r/pulls", url.Values{"state": {"all"}, "head": {"o:agent/i1-abc"}, "base": {"main"}}, &listed)
	if len(listed) != 1 || listed[0].Number != created.Number {
		t.Fatalf("by head = %+v", listed)
	}
	f.get("repos/o/r/pulls", url.Values{"state": {"all"}, "per_page": {"1"}}, &listed)
	response = f.get("repos/o/r/pulls", url.Values{"state": {"all"}, "per_page": {"1"}}, &listed)
	if len(listed) != 1 || listed[0].Number != other || response.NextPath == "" {
		t.Fatalf("newest first page = %+v next %q", listed, response.NextPath)
	}
	f.get("repos/o/r/pulls", nil, &listed)
	if len(listed) != 1 || listed[0].Number != created.Number {
		t.Fatalf("open pulls = %+v", listed)
	}

	response, err = f.write(http.MethodPatch, "repos/o/r/pulls/"+itoa(int64(created.Number)), map[string]string{"title": "Fix it"})
	if err != nil || decodeInto[Pull](t, response).Title != "Fix it" {
		t.Fatalf("patch = %v", err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// wantStatus fails unless err is a GHError with status.
func wantStatus(t *testing.T, what string, err error, status int) {
	t.Helper()
	if got, ok := ghStatus(err); !ok || got != status {
		t.Fatalf("%s = %v, want HTTP %d", what, err, status)
	}
}
