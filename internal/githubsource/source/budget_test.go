package source

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/githubsource/gh"
	"go.harness.dev/harness/internal/githubsource/ghfake"
	"go.harness.dev/harness/internal/githubsource/model"
)

// directClient sends requests straight to the fake server instead of
// through one gh subprocess per call, so an hour of rounds runs in
// seconds. It maps a reply the way the gh client does for what polling
// reads: status, validators, the next link, the poll interval and the
// rate headers; the fake's routes, ETags and rate charging are unchanged.
type directClient struct{ base string }

func (c directClient) Do(ctx context.Context, request model.GHRequest) (model.GHResponse, error) {
	endpoint, err := gh.Endpoint(request.Path, request.Query)
	if err != nil {
		return model.GHResponse{}, err
	}
	outgoing, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/"+endpoint, nil)
	if err != nil {
		return model.GHResponse{}, err
	}
	if request.ETag != "" {
		outgoing.Header.Set("If-None-Match", request.ETag)
	}
	if request.LastModified != "" {
		outgoing.Header.Set("If-Modified-Since", request.LastModified)
	}
	reply, err := http.DefaultClient.Do(outgoing)
	if err != nil {
		return model.GHResponse{}, &model.GHError{Message: err.Error()}
	}
	defer reply.Body.Close()
	body, err := io.ReadAll(reply.Body)
	if err != nil {
		return model.GHResponse{}, &model.GHError{Message: err.Error()}
	}
	header := reply.Header
	response := model.GHResponse{
		Status: reply.StatusCode, Header: header, ETag: header.Get("ETag"), LastModified: header.Get("Last-Modified"),
		NextPath: gh.NextPath(header.Get("Link")),
	}
	if reply.StatusCode != http.StatusNotModified {
		response.Body = body
	}
	if seconds, err := strconv.Atoi(header.Get("X-Poll-Interval")); err == nil {
		response.PollInterval = time.Duration(seconds) * time.Second
	}
	limit, limitErr := strconv.Atoi(header.Get("X-RateLimit-Limit"))
	remaining, remainingErr := strconv.Atoi(header.Get("X-RateLimit-Remaining"))
	reset, resetErr := strconv.ParseInt(header.Get("X-RateLimit-Reset"), 10, 64)
	if limitErr == nil && remainingErr == nil && resetErr == nil {
		response.RateLimit = model.RateLimit{Limit: limit, Remaining: remaining, Reset: time.Unix(reset, 0).UTC()}
	}
	if reply.StatusCode >= http.StatusBadRequest {
		return response, &model.GHError{Status: reply.StatusCode, Message: string(body)}
	}
	return response, nil
}

// These tests are the regressions of the third G1 review
// (context/live/verify/g1-check.md, "Check 3"): a resumed ranked walk that
// searched past the join after its saved row and declared unread rows
// covered, and request costs that could exceed GitHub's hourly limit.

// reviewSpec scans a scripted listing of bare IDs as reviews, saving each
// position it commits.
func reviewSpec(saved *model.Position) scanSpec {
	return scanSpec{
		resource: "probe", path: "probe",
		entries: func(raw []byte, _ time.Time) ([]entry, error) {
			var values []int64
			if err := json.Unmarshal(raw, &values); err != nil {
				return nil, err
			}
			out := make([]entry, 0, len(values))
			for _, id := range values {
				out = append(out, entry{id: id, events: []model.Event{{ID: model.EventID(fmt.Sprintf("probe:%d", id)), Kind: model.EventReview}}})
			}
			return out, nil
		},
		position: func(_ *model.IntakeBatch, position model.Position) { *saved = position },
	}
}

// TestScanResumeChecksTheJoinAfterItsLastRow: reviews are listed
// chronologically, here [1..99, 1000, 100..299]. A round of one page
// stops at 1000; every page is in ID order on its own, so only the join
// after 1000 shows the listing is not. The resumed scan must read that
// join and fall back to reading the listing whole, not seek past it and
// call 100..299 read.
func TestScanResumeChecksTheJoinAfterItsLastRow(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(config *model.Config) { config.Poll.MaxPages = 1 })
	var ids []int64
	for id := int64(1); id <= 99; id++ {
		ids = append(ids, id)
	}
	ids = append(ids, 1000)
	for id := int64(100); id <= 299; id++ {
		ids = append(ids, id)
	}
	e.source.client = scripted(func(request model.GHRequest) model.GHResponse { return pageOf(request, ids, listPageSize, start) })
	var saved model.Position
	spec := reviewSpec(&saved)
	if done, err := e.source.scan(context.Background(), e.source.newRound(), spec); err != nil || done {
		t.Fatalf("first round: done %t, err %v", done, err)
	}
	if saved.LastID != 1000 {
		t.Fatalf("first round saved %+v, want LastID 1000", saved)
	}
	spec.old = saved
	if done, err := e.source.scan(context.Background(), e.source.newRound(), spec); err != nil || !done {
		t.Fatalf("resumed round: done %t, err %v", done, err)
	}
	for _, id := range ids {
		if !e.intake.hasEvent(model.EventID(fmt.Sprintf("probe:%d", id))) {
			t.Fatalf("review %d never read; saved %+v", id, saved)
		}
	}
}

// TestNotificationResumeChecksTheJoinAfterItsLastRow: 3,301 notifications
// share one settled second. The first 2,622 descend from ID 9999 to 7378;
// the rest descend too but start above them. A round of the default bound
// stops inside the first run, so only the join after the saved row shows
// the inversion. No round may move the cursor past that second before
// every thread in it is flagged, and the resumed round must read the
// second whole instead.
func TestNotificationResumeChecksTheJoinAfterItsLastRow(t *testing.T) {
	t.Parallel()
	const threads, firstRun = 3301, 2622
	e := newEnv(t, func(config *model.Config) { config.Poll.MaxPages = 1 })
	e.intake.cursors[model.CursorNotifications] = model.Cursor{Key: model.CursorNotifications, Since: start}
	at, now := start.Add(time.Second), start.Add(2*time.Hour)
	var rows []inboxRow
	for number := 1; number <= threads; number++ {
		id := int64(10000 - number)
		if number > firstRun {
			id = int64(20000 - number)
		}
		rows = append(rows, inboxIssue(id, number, at))
	}
	requests := 0
	e.source.client = scripted(func(request model.GHRequest) model.GHResponse {
		requests++
		return inbox(request, rows, now)
	})
	flagged := func() int {
		count := 0
		for number := 1; number <= threads; number++ {
			if _, ok := e.intake.threads[number]; ok {
				count++
			}
		}
		return count
	}
	for index := range 3 {
		before := requests
		err := e.source.pollNotifications(context.Background(), e.source.newRound())
		cursor := e.intake.cursors[model.CursorNotifications]
		t.Logf("round %d: %d requests, %d flagged, cursor %+v, err %v", index, requests-before, flagged(), cursor, err)
		if err != nil {
			t.Fatal(err)
		}
		if cursor.Since.After(at) && flagged() < threads {
			t.Fatalf("round %d moved the cursor past %s with %d of %d threads flagged", index, at, flagged(), threads)
		}
	}
	if got := flagged(); got != threads {
		t.Fatalf("%d of %d threads flagged", got, threads)
	}
}

// TestUnprovenNotificationSecondBacksOff: a second out of ID order that
// holds more rows than polling may read whole fails closed, then waits
// instead of spending the same requests every round: rounds inside the
// wait read only up to that second and report it, the wait doubles, and
// the retry after budget returns reads it whole.
func TestUnprovenNotificationSecondBacksOff(t *testing.T) {
	t.Parallel()
	const threads = 3301
	e := newEnv(t, func(config *model.Config) { config.Poll.MaxPages = 1 })
	e.intake.cursors[model.CursorNotifications] = model.Cursor{Key: model.CursorNotifications, Since: start}
	at, now := start.Add(time.Second), start.Add(2*time.Hour)
	var rows []inboxRow
	for number := 1; number <= threads; number++ {
		rows = append(rows, inboxIssue(int64(number), number, at))
	}
	requests := 0
	e.source.client = scripted(func(request model.GHRequest) model.GHResponse {
		requests++
		return inbox(request, rows, now)
	})
	// Another client of the account used polling's whole share.
	spent := model.RateLimit{Limit: 5000, Remaining: 3000, Reset: start.Add(time.Hour)}
	steps := []struct {
		advance  time.Duration
		rate     model.RateLimit
		retry    bool
		complete bool
	}{
		{0, spent, true, false},
		{0, spent, false, false},
		{61 * time.Second, spent, true, false},
		{61 * time.Second, spent, false, false},
		{61 * time.Second, model.RateLimit{Limit: 5000, Remaining: 5000, Reset: start.Add(time.Hour)}, true, true},
	}
	for index, step := range steps {
		e.repo.Advance(step.advance)
		e.source.health.RateLimit = step.rate
		before := requests
		err := e.source.pollNotifications(context.Background(), e.source.newRound())
		sent, cursor := requests-before, e.intake.cursors[model.CursorNotifications]
		t.Logf("round %d: %d requests, %d flagged, cursor %+v, err %v", index, sent, len(e.intake.threads), cursor, err)
		switch {
		case step.complete:
			if err != nil || !cursor.Since.After(at) || len(e.intake.threads) != threads {
				t.Fatalf("round %d: the retry with budget did not read the second whole", index)
			}
		case err == nil || !strings.Contains(err.Error(), "retried after") || cursor.Since.After(at) || cursor.LastID != 0:
			t.Fatalf("round %d: the unproven second did not fail closed", index)
		case step.retry && sent < threads/notificationsPage:
			t.Fatalf("round %d: the retry sent %d requests, too few to have read the second", index, sent)
		case !step.retry && sent > 2:
			t.Fatalf("round %d: inside the wait the round sent %d requests", index, sent)
		}
	}
}

// TestObserveUnprovenSecondIsReadWhole: 250 comments share one second,
// listed [1..99, 1000, 100..249]. A round of one page stops at 1000; the
// next must find the join after it out of order and read the second
// whole, not seek past it, and no round may report the stream failed.
func TestObserveUnprovenSecondIsReadWhole(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(config *model.Config) {
		config.Poll.MaxPages = 1
		config.Observe.Comments = true
	})
	at := start.Add(time.Second)
	e.intake.cursors[model.CursorComments] = model.Cursor{Key: model.CursorComments, Since: at, EnabledAt: start}
	var rows []ghComment
	for id := int64(1); id <= 99; id++ {
		rows = append(rows, commentRow(id, at))
	}
	rows = append(rows, commentRow(1000, at))
	for id := int64(100); id <= 249; id++ {
		rows = append(rows, commentRow(id, at))
	}
	all := func(ghComment) bool { return true }
	e.source.client = scripted(func(request model.GHRequest) model.GHResponse {
		return comments(request, rows, all, at.Add(time.Minute))
	})
	for range 3 {
		if err := e.source.pollCommentsStream(context.Background(), e.source.newRound()); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range rows {
		if !e.intake.hasEvent(model.CommentID(row.ID, at)) {
			t.Fatalf("comment %d never read", row.ID)
		}
	}
}

// TestRoundBudgetFollowsRateHeaders: a round may spend polling's share of
// the window's limit, less what the window used before it, spread over the
// rounds left until the reset.
func TestRoundBudgetFollowsRateHeaders(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	now := e.repo.Now()
	cases := []struct {
		name              string
		rate              model.RateLimit
		window, allowance int
	}{
		{"unreported", model.RateLimit{}, 2000, 33},
		{"fresh window", model.RateLimit{Limit: 5000, Remaining: 5000, Reset: now.Add(time.Hour)}, 2000, 33},
		{"half spent", model.RateLimit{Limit: 5000, Remaining: 4000, Reset: now.Add(30 * time.Minute)}, 1000, 33},
		{"near reset", model.RateLimit{Limit: 5000, Remaining: 4500, Reset: now.Add(90 * time.Second)}, 1500, 750},
		{"share used elsewhere", model.RateLimit{Limit: 5000, Remaining: 2500, Reset: now.Add(time.Hour)}, 0, 0},
		{"window ended", model.RateLimit{Limit: 5000, Remaining: 10, Reset: now.Add(-time.Minute)}, 2000, 33},
	}
	for _, test := range cases {
		e.source.health.RateLimit = test.rate
		rd := e.source.newRound()
		if rd.window != test.window || rd.allowance != test.allowance {
			t.Errorf("%s: window %d allowance %d, want %d and %d", test.name, rd.window, rd.allowance, test.window, test.allowance)
		}
	}
}

// TestBusyRepositoryStaysWithinPollingShare runs one hour of default
// polling, every observe stream on, against a busy fake repository: four
// watched PRs and two watched issues with steady comments, reviews, CI and
// notifications, and repository-wide comments on twenty more threads.
// GitHub must charge less than half of the 5,000 requests of the hour,
// and every comment on a watched thread must still be read.
func TestBusyRepositoryStaysWithinPollingShare(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(config *model.Config) {
		config.Observe = model.ObserveConfig{Issues: true, Pulls: true, Comments: true, CI: true}
	})
	const (
		limit   = 5000
		rounds  = 60
		quietAt = 55
	)
	e.source.client = directClient{base: e.server.URL()}
	e.repo.SetRateLimit(limit, limit, start.Add(time.Hour+time.Minute))
	e.repo.SetBranch("main", "main-0")
	var watched, others []int
	for index := range 4 {
		watched = append(watched, e.repo.OpenPull(alice, ghfake.PullSpec{Title: fmt.Sprintf("pr %d", index), HeadRef: fmt.Sprintf("b%d", index), HeadSHA: fmt.Sprintf("head-%d-0", index), BaseRef: "main"}))
	}
	for index := range 2 {
		watched = append(watched, e.repo.OpenIssue(alice, fmt.Sprintf("ticket %d", index), "@acme-bot please fix"))
	}
	for index := range 20 {
		others = append(others, e.repo.OpenIssue(carol, fmt.Sprintf("other %d", index), ""))
	}
	e.intake.subscribe(watched...)
	type posted struct {
		id      int64
		at      time.Time
		watched bool
	}
	var posts []posted
	for minute := range rounds {
		if minute < quietAt {
			// Eight repository comments a minute, one on a watched thread,
			// which notifies the account; an inline comment, a review or
			// a CI change on a watched PR in turn.
			for index := range 7 {
				id := e.repo.Comment(others[(minute*7+index)%len(others)], carol, fmt.Sprintf("note %d.%d", minute, index))
				posts = append(posts, posted{id, e.repo.Now(), false})
			}
			number := watched[minute%len(watched)]
			posts = append(posts, posted{e.repo.Comment(number, carol, fmt.Sprintf("progress %d", minute)), e.repo.Now(), true})
			e.repo.Notify(number)
			pull, sha := watched[minute%4], fmt.Sprintf("head-%d-0", minute%4)
			switch minute % 3 {
			case 0:
				e.repo.ReviewComment(pull, carol, ghfake.ReviewCommentSpec{Body: fmt.Sprintf("nit %d", minute), Path: "main.go", Line: 1})
			case 1:
				e.repo.Review(pull, carol, "COMMENTED", fmt.Sprintf("review %d", minute))
			default:
				e.repo.SetCheckRun(sha, "test", "completed", "success")
			}
		}
		report := e.poll()
		for _, failure := range report.Errors {
			t.Errorf("minute %d: %s: %s", minute, failure.Resource, failure.Message)
		}
		e.repo.Advance(time.Minute)
	}
	charged := limit - e.source.Health().RateLimit.Remaining
	t.Logf("charged %d requests in %d rounds (%.0f%% of %d an hour); %d events, %d observed comments", charged, rounds, float64(charged)*100/limit, limit, e.intake.eventCount(), len(e.intake.eventsOfKind(model.EventComment)))
	if charged*2 >= limit {
		t.Fatalf("an hour of polling charged %d of %d requests", charged, limit)
	}
	// A stream imports nothing from before its first round, which the
	// budget may defer past the first minute's comments.
	enabledAt := e.intake.cursors[model.CursorComments].EnabledAt
	for _, comment := range posts {
		if (comment.watched || !comment.at.Before(enabledAt)) && !e.intake.hasEvent(model.CommentID(comment.id, comment.at)) {
			t.Errorf("comment %d (watched %t) never read", comment.id, comment.watched)
		}
	}
}

// TestUnchangedWatchedThreadCostsNothing: once a watched PR's positions
// stop moving, a round that finds nothing changed is answered with 304s,
// which GitHub does not charge, through the real gh client; a new comment
// is still read the next round.
func TestUnchangedWatchedThreadCostsNothing(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	number := e.repo.OpenPull(alice, ghfake.PullSpec{Title: "p", HeadRef: "b", HeadSHA: "h1", BaseRef: "main"})
	e.repo.Comment(number, carol, "first")
	e.repo.SetCheckRun("h1", "test", "completed", "success")
	e.intake.subscribe(number)
	e.repo.SetRateLimit(5000, 5000, start.Add(time.Hour))
	charged := func() int {
		e.repo.Advance(time.Minute)
		before := e.source.Health().RateLimit.Remaining
		if before == 0 {
			before = 5000
		}
		e.pollClean()
		return before - e.source.Health().RateLimit.Remaining
	}
	for range 2 {
		charged()
	}
	if spent := charged(); spent != 0 {
		t.Fatalf("an unchanged round charged %d requests", spent)
	}
	id := e.repo.Comment(number, carol, "second")
	at := e.repo.Now()
	if spent := charged(); spent == 0 || !e.intake.hasEvent(model.CommentID(id, at)) {
		t.Fatalf("a new comment was not read: %d charged", spent)
	}
}
