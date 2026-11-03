package ghfake

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"go.harness.dev/harness/internal/githubsource/gh"
	"go.harness.dev/harness/internal/githubsource/model"
)

var (
	repoStart = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	account   = User{Login: "acme-bot", ID: 9, Type: TypeUser}
	alice     = User{Login: "alice", ID: 11, Type: TypeUser}
)

type repoFixture struct {
	t      *testing.T
	server *Server
	repo   *Repo
	client *gh.Client
}

func newRepoFixture(t *testing.T) repoFixture {
	t.Helper()
	server := NewServer(t)
	repo := NewRepo(server, "o/r", account, repoStart)
	client := gh.New(gh.Options{Path: Executable(), ConfigDir: t.TempDir(), Env: server.Env(), Clock: repo})
	return repoFixture{t: t, server: server, repo: repo, client: client}
}

// get decodes one reply into out (nil for 304) and returns the response.
func (f repoFixture) get(path string, query url.Values, out any, conditional ...string) model.GHResponse {
	f.t.Helper()
	request := model.GHRequest{Path: path, Query: query}
	if len(conditional) == 2 {
		request.ETag, request.LastModified = conditional[0], conditional[1]
	}
	response, err := f.client.Do(context.Background(), request)
	if err != nil {
		f.t.Fatalf("GET %s: %v", path, err)
	}
	if out != nil && response.Status == http.StatusOK {
		if err := json.Unmarshal(response.Body, out); err != nil {
			f.t.Fatalf("decode %s: %v", path, err)
		}
	}
	return response
}

func ids[T sortable](rows []T) []int64 {
	out := make([]int64, len(rows))
	for i, row := range rows {
		out[i] = row.order().id
	}
	return out
}

func equalIDs(t *testing.T, what string, got, want []int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s = %v, want %v", what, got, want)
		}
	}
}

func TestRepoCommentsSinceOrderPagesAndIndexDelay(t *testing.T) {
	f := newRepoFixture(t)
	number := f.repo.OpenIssue(alice, "bug", "body")
	first := f.repo.Comment(number, alice, "one")
	f.repo.Advance(time.Minute)
	second := f.repo.Comment(number, alice, "two")
	f.repo.Advance(time.Minute)
	f.repo.EditComment(first, "one, edited")
	f.repo.SetIndexDelay(5 * time.Minute)
	late := f.repo.Comment(number, alice, "late")

	var page []Comment
	response := f.get("repos/o/r/issues/1/comments", url.Values{"per_page": {"1"}}, &page)
	equalIDs(t, "page 1", ids(page), []int64{first})
	if response.NextPath == "" {
		t.Fatal("page 1 has no next link")
	}
	f.get(response.NextPath, nil, &page)
	equalIDs(t, "page 2", ids(page), []int64{second})

	since := url.Values{"since": {repoStart.Add(90 * time.Second).Format(time.RFC3339)}}
	f.get("repos/o/r/issues/1/comments", since, &page)
	equalIDs(t, "since keeps ID order and filters by updated_at", ids(page), []int64{first})
	if page[0].Body != "one, edited" || !page[0].UpdatedAt.After(page[0].CreatedAt) {
		t.Fatalf("edited comment = %+v", page[0])
	}

	var issue Issue
	f.get("repos/o/r/issues/1", nil, &issue)
	if issue.Comments != 2 {
		t.Fatalf("visible comment count = %d", issue.Comments)
	}
	f.repo.Advance(5 * time.Minute)
	f.get("repos/o/r/issues/1/comments", nil, &page)
	equalIDs(t, "after index delay", ids(page), []int64{first, second, late})
	if page[2].CreatedAt.After(repoStart.Add(3 * time.Minute)) {
		t.Fatalf("late comment keeps its creation time, got %s", page[2].CreatedAt)
	}

	f.repo.DeleteComment(second)
	f.repo.EditComment(first, "one, edited twice")
	var all []Comment
	f.get("repos/o/r/issues/comments", url.Values{"sort": {"updated"}, "direction": {"asc"}}, &all)
	equalIDs(t, "repo stream sorted by updated", ids(all), []int64{late, first})
}

func TestRepoNotificationsConditionalAndNewestFirst(t *testing.T) {
	f := newRepoFixture(t)
	issue := f.repo.OpenIssue(alice, "issue", "")
	pull := f.repo.OpenPull(alice, PullSpec{Title: "pr", HeadRef: "feature", HeadSHA: "aaaaaaa"})
	f.repo.Notify(issue)
	f.repo.NotifyElsewhere("x/y", 3)
	f.repo.Advance(time.Minute)
	f.repo.Notify(pull)

	var rows []Notification
	response := f.get("notifications", url.Values{"all": {"true"}}, &rows)
	if len(rows) != 3 || rows[0].Subject.Type != "PullRequest" || rows[2].Subject.Type != "Issue" || response.PollInterval != time.Minute {
		t.Fatalf("notifications = %+v poll %s", rows, response.PollInterval)
	}
	if rows[0].Subject.URL != "https://api.github.com/repos/o/r/pulls/2" {
		t.Fatalf("pull subject url = %q", rows[0].Subject.URL)
	}
	again := f.get("notifications", url.Values{"all": {"true"}}, nil, "", response.LastModified)
	if again.Status != http.StatusNotModified {
		t.Fatalf("If-Modified-Since status = %d", again.Status)
	}
	f.repo.Advance(time.Minute)
	f.repo.Notify(issue)
	response = f.get("notifications", url.Values{"all": {"true"}, "since": {repoStart.Add(90 * time.Second).Format(time.RFC3339)}}, &rows)
	if len(rows) != 1 || rows[0].Subject.Type != "Issue" {
		t.Fatalf("since filter = %+v", rows)
	}
	if date, err := http.ParseTime(response.Header.Get("Date")); err != nil || !date.Equal(f.repo.Now()) {
		t.Fatalf("Date = %q, want the repository clock %s", response.Header.Get("Date"), f.repo.Now())
	}
	// before is exclusive: the pull's notification at one minute is out.
	f.get("notifications", url.Values{"all": {"true"}, "before": {repoStart.Add(time.Minute).Format(time.RFC3339)}}, &rows)
	if len(rows) != 1 || rows[0].Repository.FullName != "x/y" {
		t.Fatalf("before filter = %+v", rows)
	}
	for range 60 {
		f.repo.Notify(f.repo.OpenIssue(alice, "more", ""))
	}
	response = f.get("notifications", url.Values{"all": {"true"}, "per_page": {"100"}}, &rows)
	if len(rows) != 50 || response.NextPath == "" {
		t.Fatalf("notification pages hold at most 50 rows: %d, next %q", len(rows), response.NextPath)
	}
}

func TestRepoETagAndRateHeaders(t *testing.T) {
	f := newRepoFixture(t)
	reset := repoStart.Add(time.Hour)
	f.repo.SetRateLimit(100, 50, reset)
	f.repo.OpenIssue(alice, "issue", "")
	var rows []Issue
	response := f.get("repos/o/r/issues", url.Values{"state": {"all"}}, &rows)
	if len(rows) != 1 || response.ETag == "" || response.RateLimit.Remaining != 49 || !response.RateLimit.Reset.Equal(reset) {
		t.Fatalf("first = %+v", response)
	}
	cached := f.get("repos/o/r/issues", url.Values{"state": {"all"}}, nil, response.ETag, "")
	if cached.Status != http.StatusNotModified {
		t.Fatalf("If-None-Match status = %d", cached.Status)
	}
	after := f.get("repos/o/r/issues", url.Values{"state": {"all"}}, &rows)
	if after.RateLimit.Remaining != 48 {
		t.Fatalf("304 must not charge the budget: remaining %d", after.RateLimit.Remaining)
	}
}

func TestRepoPullReviewsChecksAndStatuses(t *testing.T) {
	f := newRepoFixture(t)
	number := f.repo.OpenPull(alice, PullSpec{Title: "pr", HeadRef: "agent/x", HeadSHA: "aaaaaaa"})
	approval := f.repo.Review(number, alice, ReviewApproved, "")
	f.repo.Push(number, "bbbbbbb")
	f.repo.SetCheckRun("bbbbbbb", "test", CheckInProgress, "")
	f.repo.SetCheckRun("bbbbbbb", "test", CheckCompleted, "failure")
	f.repo.SetCheckRun("bbbbbbb", "lint", CheckCompleted, "success")
	f.repo.SetStatus("bbbbbbb", "ci/legacy", StatusPending)
	f.repo.SetStatus("bbbbbbb", "ci/legacy", StatusSuccess)
	f.repo.Merge(number, "ccccccc")

	var pull Pull
	f.get("repos/o/r/pulls/1", nil, &pull)
	if !pull.Merged || pull.MergeCommitSHA == nil || *pull.MergeCommitSHA != "ccccccc" || pull.Head.SHA != "bbbbbbb" || pull.Head.Repo.FullName != "o/r" || pull.State != StateClosed {
		t.Fatalf("pull = %+v", pull)
	}
	var reviews []Review
	f.get("repos/o/r/pulls/1/reviews", nil, &reviews)
	if len(reviews) != 1 || reviews[0].ID != approval || reviews[0].CommitID != "aaaaaaa" {
		t.Fatalf("reviews = %+v", reviews)
	}
	var checks checkRunsReply
	f.get("repos/o/r/commits/bbbbbbb/check-runs", nil, &checks)
	if checks.TotalCount != 2 || checks.CheckRuns[0].Name != "test" || *checks.CheckRuns[0].Conclusion != "failure" {
		t.Fatalf("latest check runs = %+v", checks)
	}
	var combined combinedStatusReply
	f.get("repos/o/r/commits/bbbbbbb/status", nil, &combined)
	if combined.State != StatusSuccess || len(combined.Statuses) != 1 || combined.Statuses[0].Context != "ci/legacy" {
		t.Fatalf("combined = %+v", combined)
	}
	f.repo.SetBranch("main", "ddddddd")
	var commit commitReply
	f.get("repos/o/r/commits/main", nil, &commit)
	if commit.SHA != "ddddddd" {
		t.Fatalf("branch commit = %+v", commit)
	}
}
