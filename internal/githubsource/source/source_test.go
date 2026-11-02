package source

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/githubsource/gh"
	"go.harness.dev/harness/internal/githubsource/ghfake"
	"go.harness.dev/harness/internal/githubsource/model"
)

func TestMain(m *testing.M) {
	ghfake.MainIfFake()
	os.Exit(m.Run())
}

var (
	start   = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	botUser = ghfake.User{Login: "acme-bot", ID: 9, Type: ghfake.TypeUser}
	alice   = ghfake.User{Login: "alice", ID: 11, Type: ghfake.TypeUser}
	carol   = ghfake.User{Login: "carol", ID: 12, Type: ghfake.TypeUser}
)

// faultClient wraps the real gh client to fail chosen requests and count
// what was sent.
type faultClient struct {
	inner model.GHClient
	mu    sync.Mutex
	fail  func(model.GHRequest) error
	sent  []model.GHRequest
}

func (c *faultClient) Do(ctx context.Context, request model.GHRequest) (model.GHResponse, error) {
	c.mu.Lock()
	c.sent = append(c.sent, request)
	fail := c.fail
	c.mu.Unlock()
	if fail != nil {
		if err := fail(request); err != nil {
			return model.GHResponse{}, err
		}
	}
	return c.inner.Do(ctx, request)
}

func (c *faultClient) requests() []model.GHRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]model.GHRequest(nil), c.sent...)
}

// failOnce fails the first request matching match with err.
func (c *faultClient) failOnce(match func(model.GHRequest) bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	used := false
	c.fail = func(request model.GHRequest) error {
		if used || !match(request) {
			return nil
		}
		used = true
		return err
	}
}

type env struct {
	t      *testing.T
	server *ghfake.Server
	repo   *ghfake.Repo
	client *faultClient
	intake *memIntake
	source *Source
}

func newEnv(t *testing.T, configure func(*model.Config)) *env {
	t.Helper()
	server := ghfake.NewServer(t)
	repo := ghfake.NewRepo(server, "o/r", botUser, start)
	client := &faultClient{inner: gh.New(gh.Options{Path: ghfake.Executable(), ConfigDir: t.TempDir(), Env: server.Env(), Clock: repo})}
	intake := newMemIntake(t)
	config := model.Config{Poll: model.PollConfig{Lookback: model.Duration(time.Hour), MaxPages: 10}}
	if configure != nil {
		configure(&config)
	}
	source, err := New(Options{
		Client: client, Intake: intake, Clock: repo, Config: config,
		Repo:    model.RepoMeta{Repo: "o/r", DefaultBranch: "main"},
		Account: model.Actor{Login: botUser.Login, ID: botUser.ID, Type: botUser.Type},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, server: server, repo: repo, client: client, intake: intake, source: source}
}

func (e *env) poll() model.PollReport {
	e.t.Helper()
	report, err := e.source.Poll(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return report
}

// pollClean polls and fails the test on any resource error.
func (e *env) pollClean() model.PollReport {
	e.t.Helper()
	report := e.poll()
	if len(report.Errors) > 0 {
		e.t.Fatalf("round errors: %+v", report.Errors)
	}
	return report
}

func TestRereadCommitsNothingNewAndEditsAreNewVersions(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	number := e.repo.OpenIssue(alice, "Crash on save", "@acme-bot please fix")
	first := e.repo.Comment(number, alice, "more detail")
	e.repo.Advance(time.Minute)
	second := e.repo.Comment(number, carol, "me too")
	e.repo.Comment(number, botUser, "on it")
	e.intake.subscribe(number)

	report := e.pollClean()
	if report.NewEvents != 3 {
		t.Fatalf("first round new events = %d, want opened + 2 comments", report.NewEvents)
	}
	opened := e.intake.eventsOfKind(model.EventIssueOpened)
	if len(opened) != 1 || opened[0].ID != model.IssueOpenedID(number) || !opened[0].Mention || opened[0].Actor.ID != alice.ID || opened[0].Title != "Crash on save" {
		t.Fatalf("opened = %+v", opened)
	}
	comments := e.intake.eventsOfKind(model.EventComment)
	if len(comments) != 2 || comments[0].CommentID != first || comments[1].CommentID != second {
		t.Fatalf("comments = %+v (the account's own comment must not be stored)", comments)
	}
	thread := e.intake.thread(number)
	if !thread.Comments.Since.Equal(start.Add(time.Minute)) || thread.Title != "Crash on save" || thread.State != "open" {
		t.Fatalf("thread after first read = %+v", thread)
	}

	changes := e.intake.changeCount()
	e.repo.Advance(time.Minute)
	if report := e.pollClean(); report.NewEvents != 0 || e.intake.changeCount() != changes {
		t.Fatalf("reread: %d new events, %d changes", report.NewEvents, e.intake.changeCount()-changes)
	}

	e.repo.Advance(time.Minute)
	e.repo.EditComment(first, "more detail, edited")
	e.repo.DeleteComment(second)
	e.repo.EditIssue(number, "@acme-bot please fix, and also this")
	report = e.pollClean()
	edits := e.intake.eventsOfKind(model.EventCommentEdited)
	if report.NewEvents != 1 || len(edits) != 1 || edits[0].ID != model.CommentID(first, start.Add(3*time.Minute)) || edits[0].Body != "more detail, edited" {
		t.Fatalf("edit round: %d new, edits %+v", report.NewEvents, edits)
	}
	if !e.intake.hasEvent(model.CommentID(first, start)) {
		t.Fatal("original version was lost")
	}
	if opened := e.intake.eventsOfKind(model.EventIssueOpened); opened[0].Body != "@acme-bot please fix" {
		t.Fatalf("a body edit rewrote the opened event: %q", opened[0].Body)
	}
}

func TestCommentFirstSeenEditedKeepsItsCreationVersion(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	number := e.repo.OpenIssue(alice, "t", "")
	id := e.repo.Comment(number, alice, "@acme-bot fxi this")
	e.repo.Advance(20 * time.Second)
	e.repo.EditComment(id, "@acme-bot fix this")
	e.intake.subscribe(number)
	e.pollClean()
	created := e.intake.eventsOfKind(model.EventComment)
	current := e.intake.eventsOfKind(model.EventCommentEdited)
	if len(created) != 1 || created[0].ID != model.CommentID(id, start) || !created[0].Mention || created[0].Body != "@acme-bot fix this" {
		t.Fatalf("creation version = %+v", created)
	}
	if len(current) != 1 || current[0].ID != model.CommentID(id, start.Add(20*time.Second)) {
		t.Fatalf("edited version = %+v", current)
	}
}

func TestLateIndexedCommentWithinLookbackIsRead(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	number := e.repo.OpenIssue(alice, "t", "")
	e.intake.subscribe(number)
	e.repo.Advance(time.Minute)
	e.repo.SetIndexDelay(5 * time.Minute)
	late := e.repo.Comment(number, alice, "written first, indexed late")
	e.repo.SetIndexDelay(0)
	e.repo.Advance(time.Minute)
	e.repo.Comment(number, carol, "written second, indexed at once")

	e.pollClean()
	if e.intake.hasEvent(model.CommentID(late, start.Add(time.Minute))) {
		t.Fatal("hidden comment was read before it was indexed")
	}
	if since := e.intake.thread(number).Comments.Since; !since.Equal(start.Add(2 * time.Minute)) {
		t.Fatalf("position = %s", since)
	}
	e.repo.Advance(5 * time.Minute)
	e.pollClean()
	if !e.intake.hasEvent(model.CommentID(late, start.Add(time.Minute))) {
		t.Fatal("a comment older than the position but within lookback was lost")
	}
}

func TestNotificationsPaginateAndOnlyFlagThreads(t *testing.T) {
	t.Parallel()
	const threads = 120
	e := newEnv(t, nil)
	e.intake.hideFlagged = true
	for range threads {
		number := e.repo.OpenIssue(alice, "issue", "")
		e.repo.Notify(number)
		e.repo.Advance(time.Second)
	}
	pull := e.repo.OpenPull(alice, ghfake.PullSpec{Title: "pr", HeadRef: "f", HeadSHA: "abcdef1"})
	e.repo.Notify(pull)
	e.repo.NotifyElsewhere("x/y", 5)
	e.repo.Advance(time.Minute)

	e.pollClean()
	var pages int
	for _, request := range e.client.requests() {
		if strings.HasPrefix(request.Path, "notifications") {
			pages++
		}
		if strings.Contains(request.Path, "/issues/") {
			t.Fatalf("notification step read a thread: %s", request.Path)
		}
	}
	if pages != 3 {
		t.Fatalf("notification pages = %d, want 3", pages)
	}
	for number := 1; number <= threads+1; number++ {
		thread := e.intake.thread(number)
		if !thread.NeedsReconcile || thread.NotifiedAt.IsZero() || thread.Comments != (model.Position{}) {
			t.Fatalf("thread %d = %+v: a notification flags and never moves a position", number, thread)
		}
	}
	if ref := e.intake.thread(pull).Ref; !ref.IsPR {
		t.Fatalf("pull request flag ref = %+v", ref)
	}
	cursor, _, _ := e.intake.Cursor(context.Background(), model.CursorNotifications)
	if !cursor.Since.Equal(start.Add(threads*time.Second)) || cursor.LastModified == "" {
		t.Fatalf("cursor = %+v", cursor)
	}

	changes := e.intake.changeCount()
	e.repo.Advance(time.Minute)
	e.pollClean()
	last := e.client.requests()[len(e.client.requests())-1]
	if last.LastModified != cursor.LastModified || e.intake.changeCount() != changes {
		t.Fatalf("unchanged inbox: If-Modified-Since %q, %d changes", last.LastModified, e.intake.changeCount()-changes)
	}
}

// TestNotifiedOldThreadIsReadFromItsOwnPosition is the regression for the
// late-mention defect: a notification never supplies a lower bound, so an
// unseen thread is read from its creation and a known one from its own
// committed position, however old the comments are.
func TestNotifiedOldThreadIsReadFromItsOwnPosition(t *testing.T) {
	t.Parallel()
	const oldComments = 250
	e := newEnv(t, nil)
	old := e.repo.OpenIssue(alice, "old thread", "")
	for i := range oldComments {
		e.repo.Comment(old, alice, fmt.Sprintf("comment %d", i))
	}
	e.repo.Advance(48 * time.Hour)
	e.repo.Comment(old, alice, "@acme-bot can you look")
	e.repo.Notify(old)

	e.pollClean()
	if got := len(e.intake.eventsOfKind(model.EventComment)); got != oldComments+1 {
		t.Fatalf("comments read = %d, want %d", got, oldComments+1)
	}
	thread := e.intake.thread(old)
	if thread.NeedsReconcile || !thread.Comments.Since.Equal(start.Add(48*time.Hour)) {
		t.Fatalf("thread after complete read = %+v", thread)
	}

	// A known thread whose comment and notification are indexed late, after
	// the notification cursor moved past them on another thread.
	e.repo.Advance(time.Minute)
	e.repo.SetIndexDelay(10 * time.Minute)
	late := e.repo.Comment(old, carol, "@acme-bot one more thing")
	e.repo.Notify(old)
	e.repo.SetIndexDelay(0)
	e.repo.Advance(time.Minute)
	other := e.repo.OpenIssue(carol, "other", "")
	e.repo.Notify(other)
	e.repo.Advance(time.Minute)
	e.pollClean()
	cursor, _, _ := e.intake.Cursor(context.Background(), model.CursorNotifications)
	if cursor.Since.Before(start.Add(48*time.Hour + 2*time.Minute)) {
		t.Fatalf("cursor did not move past the late comment: %s", cursor.Since)
	}
	e.repo.Advance(10 * time.Minute)
	e.pollClean()
	if !e.intake.hasEvent(model.CommentID(late, start.Add(48*time.Hour+time.Minute))) {
		t.Fatal("late-indexed mention older than the notification cursor was lost")
	}
}

func TestPartialFailureKeepsPositionAndOtherThreadsAdvance(t *testing.T) {
	t.Parallel()
	const longThread = 150
	e := newEnv(t, nil)
	long := e.repo.OpenIssue(alice, "long", "")
	for i := range longThread {
		e.repo.Comment(long, alice, fmt.Sprintf("c%d", i))
		e.repo.Advance(time.Second)
	}
	short := e.repo.OpenIssue(carol, "short", "")
	e.repo.Comment(short, carol, "hello")
	e.intake.subscribe(long, short)
	e.client.failOnce(func(request model.GHRequest) bool {
		return strings.Contains(request.Path, fmt.Sprintf("issues/%d/comments", long)) && strings.Contains(request.Path, "page=2")
	}, &model.GHError{Status: http.StatusBadGateway, Message: "Bad Gateway"})

	report := e.poll()
	if len(report.Errors) != 1 || report.Errors[0].Resource != fmt.Sprintf("thread:%d", long) || report.Errors[0].Status != http.StatusBadGateway {
		t.Fatalf("errors = %+v", report.Errors)
	}
	// Page 1 ends at comment 100, created at second 99.
	if since := e.intake.thread(long).Comments.Since; !since.Equal(start.Add(99 * time.Second)) {
		t.Fatalf("failed thread position = %s, want page 1's end", since)
	}
	if got := len(e.intake.eventsOfKind(model.EventComment)); got != 101 {
		t.Fatalf("comments after partial round = %d, want page 1 plus the short thread", got)
	}
	if e.intake.thread(short).Comments.Since.IsZero() {
		t.Fatal("the healthy thread did not advance")
	}

	e.repo.Advance(time.Minute)
	e.pollClean()
	if got := len(e.intake.eventsOfKind(model.EventComment)); got != longThread+1 {
		t.Fatalf("comments after recovery = %d", got)
	}
	if since := e.intake.thread(long).Comments.Since; !since.Equal(start.Add((longThread - 1) * time.Second)) {
		t.Fatalf("recovered position = %s", since)
	}
}

func TestFailedIngestMovesNothing(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	number := e.repo.OpenIssue(alice, "t", "")
	e.repo.Comment(number, alice, "first")
	e.intake.subscribe(number)
	failed := false
	e.intake.fail = func(batch model.IntakeBatch) error {
		if batch.Resource == fmt.Sprintf("thread:%d:comments", number) && !failed {
			failed = true
			return errors.New("control store closed")
		}
		return nil
	}
	report := e.poll()
	if len(report.Errors) != 1 || !strings.Contains(report.Errors[0].Message, "control store closed") {
		t.Fatalf("errors = %+v", report.Errors)
	}
	if !e.intake.thread(number).Comments.Since.IsZero() || len(e.intake.eventsOfKind(model.EventComment)) != 0 {
		t.Fatal("a failed commit moved the position or stored events")
	}
	e.pollClean()
	if len(e.intake.eventsOfKind(model.EventComment)) != 1 {
		t.Fatal("the next round did not recover the comment")
	}
}

func TestPullReviewsAndCIPinnedToHead(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	number := e.repo.OpenPull(alice, ghfake.PullSpec{Title: "fix", Body: "closes #1", HeadRef: "agent/i1-abc", HeadSHA: "aaaaaaa"})
	e.repo.SetCheckRun("aaaaaaa", "test", ghfake.CheckCompleted, "failure")
	e.repo.SetStatus("aaaaaaa", "lint", ghfake.StatusSuccess)
	approval := e.repo.Review(number, carol, ghfake.ReviewApproved, "")
	inline := e.repo.ReviewComment(number, carol, ghfake.ReviewCommentSpec{Body: "nit", Path: "main.go", Line: 3})
	e.intake.subscribe(number)

	e.pollClean()
	reviews := e.intake.eventsOfKind(model.EventReview)
	if len(reviews) != 1 || reviews[0].ID != model.ReviewID(approval) || reviews[0].ReviewState != "APPROVED" || reviews[0].CommitSHA != "aaaaaaa" || reviews[0].Body != "" {
		t.Fatalf("reviews = %+v", reviews)
	}
	inlines := e.intake.eventsOfKind(model.EventReviewComment)
	if len(inlines) != 1 || inlines[0].CommentID != inline || inlines[0].Path != "main.go" || inlines[0].Line != 3 || inlines[0].CommitSHA != "aaaaaaa" {
		t.Fatalf("inline comments = %+v", inlines)
	}
	ci := e.intake.eventsOfKind(model.EventPRCI)
	if len(ci) != 1 || ci[0].CI.SHA != "aaaaaaa" || len(ci[0].CI.Failing) != 1 || ci[0].CI.Failing[0] != "test" || ci[0].CI.Passing != 1 {
		t.Fatalf("ci = %+v", ci)
	}
	if ci[0].ID != model.PRCIID(number, "aaaaaaa", ci[0].CI.Fingerprint) {
		t.Fatalf("ci id = %s", ci[0].ID)
	}
	pr := e.intake.pr(number)
	if pr.HeadSHA != "aaaaaaa" || pr.CI == nil || pr.CI.SHA != "aaaaaaa" || len(pr.Reviews) != 1 || pr.Reviews[0].CommitSHA != "aaaaaaa" || pr.HeadRef != "agent/i1-abc" || pr.HeadRepo != "o/r" {
		t.Fatalf("pr = %+v", pr)
	}
	if len(e.intake.eventsOfKind(model.EventPRHead)) != 0 {
		t.Fatal("first sighting of a head is not a head move")
	}

	e.repo.Advance(time.Minute)
	e.repo.Push(number, "bbbbbbb")
	e.repo.SetCheckRun("bbbbbbb", "test", ghfake.CheckInProgress, "")
	e.pollClean()
	heads := e.intake.eventsOfKind(model.EventPRHead)
	if len(heads) != 1 || heads[0].ID != model.PRHeadID(number, "bbbbbbb") {
		t.Fatalf("heads = %+v", heads)
	}
	if pr := e.intake.pr(number); pr.HeadSHA != "bbbbbbb" || pr.CI != nil {
		t.Fatalf("new head kept the old head's CI: %+v", pr.CI)
	}
	if len(e.intake.eventsOfKind(model.EventPRCI)) != 1 {
		t.Fatal("pending checks produced a CI event")
	}

	e.repo.Advance(time.Minute)
	e.repo.SetCheckRun("bbbbbbb", "test", ghfake.CheckCompleted, "success")
	e.repo.SetStatus("bbbbbbb", "lint", ghfake.StatusSuccess)
	e.repo.DismissReview(approval)
	e.pollClean()
	ci = e.intake.eventsOfKind(model.EventPRCI)
	if len(ci) != 2 || ci[1].CI.SHA != "bbbbbbb" || len(ci[1].CI.Failing) != 0 || ci[1].CI.Passing != 2 {
		t.Fatalf("green ci = %+v", ci)
	}
	if pr := e.intake.pr(number); pr.CI == nil || pr.CI.SHA != "bbbbbbb" || pr.Reviews[0].State != "DISMISSED" || pr.Reviews[0].CommitSHA != "aaaaaaa" {
		t.Fatalf("pr after green = %+v", pr)
	}
	if dismissed := e.intake.eventsOfKind(model.EventReviewDismissed); len(dismissed) != 1 || dismissed[0].ID != model.ReviewDismissedID(approval) {
		t.Fatalf("dismissed = %+v", dismissed)
	}

	e.repo.Advance(time.Minute)
	e.repo.Merge(number, "ccccccc")
	e.pollClean()
	merged := e.intake.eventsOfKind(model.EventPRMerged)
	if len(merged) != 1 || merged[0].ID != model.PRMergedID(number, "ccccccc") || !e.intake.pr(number).Merged {
		t.Fatalf("merged = %+v", merged)
	}
	changes := e.intake.changeCount()
	e.repo.Advance(time.Minute)
	e.pollClean()
	if e.intake.changeCount() != changes {
		t.Fatal("an unchanged merged PR committed again")
	}
}

func TestPullCloseAndReopenFacts(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	number := e.repo.OpenPull(alice, ghfake.PullSpec{Title: "p", HeadRef: "f", HeadSHA: "aaaaaaa"})
	e.intake.subscribe(number)
	e.pollClean()
	e.repo.Advance(time.Minute)
	e.repo.Close(number)
	e.pollClean()
	e.repo.Advance(time.Minute)
	e.repo.Reopen(number)
	e.pollClean()
	closed := e.intake.eventsOfKind(model.EventPRClosed)
	reopened := e.intake.eventsOfKind(model.EventPRReopened)
	if len(closed) != 1 || closed[0].ID != model.PRClosedID(number, start.Add(time.Minute)) {
		t.Fatalf("closed = %+v", closed)
	}
	if len(reopened) != 1 || reopened[0].ID != model.PRReopenedID(number, start.Add(2*time.Minute)) {
		t.Fatalf("reopened = %+v", reopened)
	}
}

func TestRateLimitStopsRoundAndBacksOff(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	number := e.repo.OpenIssue(alice, "t", "")
	e.intake.subscribe(number)
	e.server.Fail(http.MethodGet, "/notifications", http.StatusTooManyRequests, 1, http.Header{"Retry-After": {"120"}})
	report := e.poll()
	if len(report.Errors) != 1 || report.Errors[0].Status != http.StatusTooManyRequests || !report.NextPollAt.Equal(start.Add(120*time.Second)) {
		t.Fatalf("limited round = %+v", report)
	}
	if sent := len(e.client.requests()); sent != 1 {
		t.Fatalf("a limited round sent %d requests", sent)
	}
	if report := e.poll(); !report.NextPollAt.Equal(start.Add(120*time.Second)) || len(e.client.requests()) != 1 {
		t.Fatalf("poll inside the wait sent requests: %+v", report)
	}
	e.repo.Advance(121 * time.Second)
	e.repo.SetPollInterval(300)
	report = e.pollClean()
	if !report.NextPollAt.Equal(e.repo.Now().Add(300 * time.Second)) {
		t.Fatalf("X-Poll-Interval ignored: next %s", report.NextPollAt)
	}
	if e.intake.thread(number).Title != "t" {
		t.Fatal("thread not read after the wait")
	}
}

func TestLowRateBudgetReadsOnlyFlaggedThreads(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(config *model.Config) { config.Observe.Comments = true })
	quiet := e.repo.OpenIssue(alice, "quiet", "")
	flagged := e.repo.OpenIssue(alice, "flagged", "")
	e.intake.subscribe(quiet)
	e.repo.Advance(time.Minute)
	e.repo.Notify(flagged)
	e.repo.SetRateLimit(5000, 100, start.Add(time.Hour))
	e.pollClean()
	for _, request := range e.client.requests() {
		if strings.Contains(request.Path, fmt.Sprintf("issues/%d", quiet)) || strings.Contains(request.Path, "issues/comments") {
			t.Fatalf("low budget read %s", request.Path)
		}
	}
	if e.intake.thread(flagged).NeedsReconcile {
		t.Fatal("flagged thread was not read")
	}
}

func TestHealthAfterConsecutiveFailures(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.server.Fail(http.MethodGet, "/notifications", http.StatusBadGateway, 3, nil)
	for round := 1; round <= 3; round++ {
		e.poll()
		health := e.source.Health()
		if health.ConsecutiveFailures != round || health.Healthy != (round < 3) || len(health.Errors) != 1 {
			t.Fatalf("round %d health = %+v", round, health)
		}
		e.repo.Advance(time.Minute)
	}
	e.pollClean()
	if health := e.source.Health(); !health.Healthy || health.ConsecutiveFailures != 0 || !health.LastSuccessAt.Equal(e.repo.Now()) {
		t.Fatalf("recovered health = %+v", health)
	}
}

func TestNotFoundRetriesAndGoneStopsWatching(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	missing := e.repo.OpenIssue(alice, "private", "")
	gone := e.repo.OpenIssue(alice, "deleted", "")
	e.intake.subscribe(missing, gone)
	e.server.Fail(http.MethodGet, fmt.Sprintf("/repos/o/r/issues/%d", missing), http.StatusNotFound, 1, nil)
	e.server.Fail(http.MethodGet, fmt.Sprintf("/repos/o/r/issues/%d", gone), http.StatusGone, 1, nil)
	report := e.poll()
	if len(report.Errors) != 1 || report.Errors[0].Status != http.StatusNotFound {
		t.Fatalf("errors = %+v (410 is not a failure)", report.Errors)
	}
	if e.intake.thread(gone).State != stateGone {
		t.Fatal("410 did not mark the thread gone")
	}
	e.repo.Advance(time.Minute)
	e.pollClean()
	if e.intake.thread(missing).Title != "private" {
		t.Fatal("404 thread was not retried")
	}
	for _, request := range e.client.requests()[len(e.client.requests())/2:] {
		if strings.HasSuffix(request.Path, fmt.Sprintf("issues/%d", gone)) {
			t.Fatal("a gone thread was read again")
		}
	}
}

func TestObserveStreamsStartAtEnableAndUseETags(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(config *model.Config) {
		config.Observe = model.ObserveConfig{Issues: true, Comments: true}
	})
	backlog := e.repo.OpenIssue(alice, "backlog", "")
	e.repo.Comment(backlog, alice, "old comment")
	e.repo.Advance(time.Minute)

	e.pollClean()
	if count := e.intake.eventCount(); count != 0 {
		t.Fatalf("enabling streams imported %d backlog events", count)
	}
	cursor, found, _ := e.intake.Cursor(context.Background(), model.CursorComments)
	if !found || !cursor.EnabledAt.Equal(start.Add(time.Minute)) {
		t.Fatalf("comments cursor = %+v", cursor)
	}

	e.repo.Advance(time.Minute)
	fresh := e.repo.OpenIssue(carol, "new issue", "")
	comment := e.repo.Comment(backlog, carol, "new comment")
	e.pollClean()
	if !e.intake.hasEvent(model.IssueOpenedID(fresh)) || !e.intake.hasEvent(model.CommentID(comment, start.Add(2*time.Minute))) {
		t.Fatal("observed events missing")
	}
	if e.intake.hasEvent(model.IssueOpenedID(backlog)) {
		t.Fatal("an issue created before enable was imported")
	}
	e.repo.Advance(time.Minute)
	e.pollClean() // records the validators of the now-stable listings
	changes := e.intake.changeCount()
	e.repo.Advance(time.Minute)
	e.pollClean()
	if e.intake.changeCount() != changes {
		t.Fatal("unchanged streams committed again")
	}
	last := e.client.requests()
	sawETag := false
	for _, request := range last[len(last)-3:] {
		sawETag = sawETag || request.ETag != ""
	}
	if !sawETag {
		t.Fatal("stream rereads sent no If-None-Match")
	}
}

func TestBranchCIBaselineThenChange(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(config *model.Config) { config.Observe.CI = true })
	e.repo.SetBranch("main", "1111111")
	e.repo.SetCheckRun("1111111", "build", ghfake.CheckCompleted, "success")
	e.pollClean()
	cursor, _, _ := e.intake.Cursor(context.Background(), model.CursorBranchCI)
	if cursor.BranchSHA != "1111111" || cursor.Fingerprint == "" || e.intake.eventCount() != 0 {
		t.Fatalf("baseline cursor = %+v, %d events", cursor, e.intake.eventCount())
	}
	e.repo.Advance(time.Minute)
	e.repo.SetBranch("main", "2222222")
	e.repo.SetCheckRun("2222222", "build", ghfake.CheckCompleted, "success")
	e.pollClean()
	if e.intake.eventCount() != 0 {
		t.Fatal("an unchanged outcome on a new head emitted an event")
	}
	e.repo.Advance(time.Minute)
	e.repo.SetBranch("main", "3333333")
	e.repo.SetCheckRun("3333333", "build", ghfake.CheckCompleted, "failure")
	e.pollClean()
	events := e.intake.eventsOfKind(model.EventBranchCI)
	if len(events) != 1 || events[0].Branch != "main" || events[0].CommitSHA != "3333333" || events[0].ID != model.BranchCIID("main", "3333333", events[0].CI.Fingerprint) {
		t.Fatalf("branch ci = %+v", events)
	}
}

func TestBatchesSplitAtEventAndByteLimits(t *testing.T) {
	t.Parallel()
	const editedComments = 60
	const bigComments = 12
	const bigBody = 60000
	e := newEnv(t, nil)
	edited := e.repo.OpenIssue(alice, "edited", "")
	for range editedComments {
		id := e.repo.Comment(edited, alice, "x")
		e.repo.Advance(time.Second)
		e.repo.EditComment(id, "y")
	}
	big := e.repo.OpenIssue(alice, "big", "")
	for range bigComments {
		e.repo.Comment(big, alice, strings.Repeat("b", bigBody))
	}
	e.intake.subscribe(edited, big)
	e.pollClean()
	if got := len(e.intake.eventsOfKind(model.EventComment)) + len(e.intake.eventsOfKind(model.EventCommentEdited)); got != 2*editedComments+bigComments {
		t.Fatalf("events = %d", got)
	}
	var editedBatches, bigBatches int
	for _, batch := range e.intake.recorded() {
		switch batch.Resource {
		case fmt.Sprintf("thread:%d:comments", edited):
			editedBatches++
		case fmt.Sprintf("thread:%d:comments", big):
			bigBatches++
		}
	}
	if editedBatches != 2 || bigBatches != 2 {
		t.Fatalf("batches: edited %d (120 events), big %d (720000 bytes)", editedBatches, bigBatches)
	}
}

func TestRoundsAreDeterministic(t *testing.T) {
	t.Parallel()
	run := func() []model.IntakeBatch {
		e := newEnv(t, nil)
		issue := e.repo.OpenIssue(alice, "t", "@acme-bot hi")
		pull := e.repo.OpenPull(alice, ghfake.PullSpec{Title: "p", HeadRef: "f", HeadSHA: "aaaaaaa"})
		e.repo.Comment(issue, carol, "@acme-bot status")
		e.repo.Review(pull, carol, ghfake.ReviewChangesRequested, "please change")
		e.repo.SetCheckRun("aaaaaaa", "test", ghfake.CheckCompleted, "success")
		e.repo.Notify(issue)
		e.repo.Notify(pull)
		e.repo.Advance(time.Minute)
		e.pollClean()
		e.repo.Advance(time.Minute)
		e.repo.Push(pull, "bbbbbbb")
		e.pollClean()
		return e.intake.recorded()
	}
	first, second := run(), run()
	if len(first) != len(second) || len(first) == 0 {
		t.Fatalf("batch counts differ: %d vs %d", len(first), len(second))
	}
	for i := range first {
		a, b := string(mustMarshal(t, first[i])), string(mustMarshal(t, second[i]))
		if a != b {
			t.Fatalf("batch %d differs:\n%s\n%s", i, a, b)
		}
	}
	statusCommand := false
	for _, batch := range first {
		for _, event := range batch.Events {
			statusCommand = statusCommand || event.Command == model.CommandStatus
		}
	}
	if !statusCommand {
		t.Fatal("status command not parsed")
	}
}
