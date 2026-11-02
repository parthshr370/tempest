package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"go.harness.dev/harness/internal/githubsource/ghfake"
	"go.harness.dev/harness/internal/githubsource/model"
)

// These tests are the adversarial regressions of the second G1 review
// (context/live/verify/g1-check.md, "Check 2"): each one lost a surviving
// row, stalled, or took a snapshot time from the local clock before the
// fix. They drive the source through a scripted client so a reply can be
// changed between two requests of one round.

// scripted answers requests from a function, splitting a NextPath query
// back into Query like the gh client does.
type scripted func(model.GHRequest) model.GHResponse

func (f scripted) Do(_ context.Context, request model.GHRequest) (model.GHResponse, error) {
	if parsed, err := url.Parse(request.Path); err == nil && parsed.RawQuery != "" {
		request.Path, request.Query = parsed.Path, parsed.Query()
	}
	return f(request), nil
}

// pageOf serves rows[lo:hi] for the request's page and per_page, with a
// next link when rows remain, at GitHub time date.
func pageOf[T any](request model.GHRequest, rows []T, defaultPerPage int, date time.Time) model.GHResponse {
	size, page := defaultPerPage, 1
	if n, _ := strconv.Atoi(request.Query.Get("per_page")); n > 0 {
		size = n
	}
	if n, _ := strconv.Atoi(request.Query.Get("page")); n > 0 {
		page = n
	}
	lo := min((page-1)*size, len(rows))
	hi := min(lo+size, len(rows))
	body, _ := json.Marshal(rows[lo:hi])
	response := model.GHResponse{Status: http.StatusOK, Body: body, Header: http.Header{"Date": {date.UTC().Format(http.TimeFormat)}}}
	if hi < len(rows) {
		query := url.Values{}
		for key, values := range request.Query {
			query[key] = values
		}
		query.Set("page", strconv.Itoa(page+1))
		query.Set("per_page", strconv.Itoa(size))
		response.NextPath = request.Path + "?" + query.Encode()
	}
	return response
}

// inboxRow is a notification as GitHub serves it.
type inboxRow struct {
	ID         string    `json:"id"`
	UpdatedAt  time.Time `json:"updated_at"`
	Subject    any       `json:"subject"`
	Repository any       `json:"repository"`
}

func inboxIssue(id int64, number int, at time.Time) inboxRow {
	return inboxRow{
		ID: strconv.FormatInt(id, 10), UpdatedAt: at,
		Subject:    map[string]string{"type": "Issue", "url": fmt.Sprintf("https://api.github.com/repos/o/r/issues/%d", number)},
		Repository: map[string]string{"full_name": "o/r"},
	}
}

// inbox serves rows in their slice order filtered by since and before.
func inbox(request model.GHRequest, rows []inboxRow, date time.Time) model.GHResponse {
	since, _ := time.Parse(time.RFC3339, request.Query.Get("since"))
	before, _ := time.Parse(time.RFC3339, request.Query.Get("before"))
	var visible []inboxRow
	for _, row := range rows {
		if !row.UpdatedAt.Before(since) && (before.IsZero() || row.UpdatedAt.Before(before)) {
			visible = append(visible, row)
		}
	}
	return pageOf(request, visible, notificationsPage, date)
}

// TestNotificationLeavingADenseSecondShiftsNothingPast: 101 settled
// notifications share one second; after the first page of that second is
// read, its first thread is updated and leaves the second. The rest shift
// back one place, so page two by number starts one row late. Every
// surviving thread must still be flagged, whether GitHub lists the second
// in descending ID order (walked and verified) or in another order (read
// whole).
func TestNotificationLeavingADenseSecondShiftsNothingPast(t *testing.T) {
	t.Parallel()
	for _, descending := range []bool{true, false} {
		t.Run(fmt.Sprintf("descending=%t", descending), func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, func(config *model.Config) { config.Poll.MaxPages = 1 })
			at, now := start.Add(time.Second), start.Add(2*time.Hour)
			e.intake.cursors[model.CursorNotifications] = model.Cursor{Key: model.CursorNotifications, Since: start}
			var rows []inboxRow
			for number := 1; number <= 101; number++ {
				id := int64(number)
				if descending {
					id = int64(1000 - number)
				}
				rows = append(rows, inboxIssue(id, number, at))
			}
			moved := false
			e.source.client = scripted(func(request model.GHRequest) model.GHResponse {
				if !moved && request.Query.Get("before") != "" && request.Query.Get("page") == "2" {
					rows[0].UpdatedAt, moved = now, true
				}
				return inbox(request, rows, now)
			})
			for range 3 {
				if err := e.source.pollNotifications(context.Background(), &round{}); err != nil {
					t.Fatal(err)
				}
			}
			if !moved {
				t.Fatal("the update never ran")
			}
			for number := 1; number <= 101; number++ {
				if _, ok := e.intake.threads[number]; !ok {
					t.Fatalf("thread %d never flagged; cursor %+v", number, e.intake.cursors[model.CursorNotifications])
				}
			}
		})
	}
}

// TestNotificationSecondLargerThanARoundResumes: one settled second holds
// more notifications than a round may request. Each round must go on from
// where the last stopped instead of rereading the second from its first
// page, and an inbox read to its end must cost one request.
func TestNotificationSecondLargerThanARoundResumes(t *testing.T) {
	t.Parallel()
	const threads = 3301
	e := newEnv(t, func(config *model.Config) { config.Poll.MaxPages = 1 })
	at, now := start.Add(time.Second), start.Add(2*time.Hour)
	e.intake.cursors[model.CursorNotifications] = model.Cursor{Key: model.CursorNotifications, Since: start}
	var rows []inboxRow
	for number := 1; number <= threads; number++ {
		rows = append(rows, inboxIssue(int64(10000-number), number, at))
	}
	requests := 0
	e.source.client = scripted(func(request model.GHRequest) model.GHResponse {
		requests++
		return inbox(request, rows, now)
	})
	bound := 1 + 1 + scanSeekProbes
	for index := range 4 {
		before := requests
		err := e.source.pollNotifications(context.Background(), &round{})
		sent := requests - before
		t.Logf("round %d: %d requests, cursor %+v, err %v", index, sent, e.intake.cursors[model.CursorNotifications], err)
		if err != nil {
			t.Fatal(err)
		}
		if sent > bound {
			t.Fatalf("round %d sent %d requests, bound %d", index, sent, bound)
		}
		if index == 3 && sent != 1 {
			t.Fatalf("a read inbox cost %d requests", sent)
		}
	}
	for number := 1; number <= threads; number++ {
		if _, ok := e.intake.threads[number]; !ok {
			t.Fatalf("thread %d never flagged", number)
		}
	}
}

// commentRow is an issue comment as GitHub serves it.
func commentRow(id int64, at time.Time) ghComment {
	return ghComment{
		ID: id, CreatedAt: at, UpdatedAt: at, Body: "@acme-bot request",
		IssueURL: "https://api.github.com/repos/o/r/issues/1", User: ghUser{ID: alice.ID, Login: alice.Login},
	}
}

// comments serves visible rows updated at or after since, in slice order.
func comments(request model.GHRequest, rows []ghComment, visible func(ghComment) bool, date time.Time) model.GHResponse {
	since, _ := time.Parse(time.RFC3339, request.Query.Get("since"))
	var listed []ghComment
	for _, row := range rows {
		if !row.UpdatedAt.Before(since) && visible(row) {
			listed = append(listed, row)
		}
	}
	return pageOf(request, listed, listPageSize, date)
}

// TestObserveSecondLargerThanARoundResumes: 501 comments share the
// cursor's second, and GitHub's clock stands still, so none settles.
// Successive rounds must reach the last one instead of rereading the same
// pages of that second, and the cursor must not move over unsettled rows.
func TestObserveSecondLargerThanARoundResumes(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(config *model.Config) {
		config.Poll.MaxPages = 1
		config.Observe.Comments = true
	})
	at := start.Add(time.Second)
	stored := model.Cursor{Key: model.CursorComments, Since: at, EnabledAt: start}
	e.intake.cursors[model.CursorComments] = stored
	var rows []ghComment
	for id := int64(1); id <= 501; id++ {
		rows = append(rows, commentRow(id, at))
	}
	all := func(ghComment) bool { return true }
	e.source.client = scripted(func(request model.GHRequest) model.GHResponse {
		return comments(request, rows, all, at.Add(time.Minute))
	})
	// Each round reads one fresh page past where the last stopped; the
	// overlap that proves each step costs a row a page.
	for range 7 {
		if err := e.source.pollCommentsStream(context.Background(), &round{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range rows {
		if !e.intake.hasEvent(model.CommentID(row.ID, at)) {
			t.Fatalf("comment %d never read; %d of 501", row.ID, len(e.intake.eventsOfKind(model.EventComment)))
		}
	}
	if cursor := e.intake.cursors[model.CursorComments]; !cursor.Since.Equal(stored.Since) || cursor.LastID != 0 {
		t.Fatalf("cursor moved over unsettled rows: %+v", cursor)
	}
}

// TestObserveLateRowBehindTheFrontierIsRead: 501 comments a second apart
// are read once; then one of them, indexed late, appears behind what was
// read, beyond the rows a round rereads. It must still be read, and the
// cursor must never pass it unread.
func TestObserveLateRowBehindTheFrontierIsRead(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(config *model.Config) {
		config.Poll.MaxPages = 1
		config.Observe.Comments = true
	})
	e.intake.cursors[model.CursorComments] = model.Cursor{Key: model.CursorComments, Since: start, EnabledAt: start}
	var rows []ghComment
	for id := int64(1); id <= 501; id++ {
		rows = append(rows, commentRow(id, start.Add(time.Duration(id)*time.Second)))
	}
	late := rows[449]
	now, indexed := start.Add(10*time.Minute), false
	e.source.client = scripted(func(request model.GHRequest) model.GHResponse {
		return comments(request, rows, func(row ghComment) bool { return indexed || row.ID != late.ID }, now)
	})
	seen := func() bool { return e.intake.hasEvent(model.CommentID(late.ID, late.CreatedAt)) }
	for index := 0; index < 12 && !seen(); index++ {
		if err := e.source.pollCommentsStream(context.Background(), &round{}); err != nil {
			t.Fatal(err)
		}
		if index == 5 {
			// Everything else was read: the late row appears now.
			if !e.intake.hasEvent(model.CommentID(501, rows[500].CreatedAt)) {
				t.Fatal("the stream did not reach its end before the late row appeared")
			}
			indexed = true
		}
		if cursor := e.intake.cursors[model.CursorComments]; cursor.Since.After(late.UpdatedAt) && !seen() {
			t.Fatalf("round %d: cursor %s passed the unread late row", index, cursor.Since)
		}
		now = now.Add(10 * time.Minute)
	}
	if !seen() {
		t.Fatal("the late row behind the frontier was never read")
	}
}

// TestScanOutOfIDOrderReadsWhole: GitHub lists reviews chronologically,
// not by ID. A lower ID listed after a higher one on a later page must be
// read, not skipped as already passed.
func TestScanOutOfIDOrderReadsWhole(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	var ids []int64
	for id := int64(1); id <= 99; id++ {
		ids = append(ids, id)
	}
	ids = append(ids, 200, 100)
	e.source.client = scripted(func(request model.GHRequest) model.GHResponse { return pageOf(request, ids, listPageSize, start) })
	spec := scanSpec{
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
		position: func(*model.IntakeBatch, model.Position) {},
	}
	if done, err := e.source.scan(context.Background(), &round{}, spec); err != nil || !done {
		t.Fatalf("done %t, err %v", done, err)
	}
	for _, id := range ids {
		if !e.intake.hasEvent(model.EventID(fmt.Sprintf("probe:%d", id))) {
			t.Fatalf("review %d skipped", id)
		}
	}
}

// TestPRSnapshotsAreReadAtGitHubsClock: control orders PR snapshots by
// the batch's ReadAt, so every batch carrying one must be read at the
// pulls reply's Date, whichever way the local clock is off.
func TestPRSnapshotsAreReadAtGitHubsClock(t *testing.T) {
	t.Parallel()
	for _, skew := range []time.Duration{-3 * time.Hour, 3 * time.Hour} {
		t.Run(skew.String(), func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, nil)
			e.source.clock = skewedClock{start.Add(skew)}
			number := e.repo.OpenPull(alice, ghfake.PullSpec{Title: "p", HeadRef: "feature", HeadSHA: "aaaa"})
			e.repo.Review(number, carol, "APPROVED", "")
			e.repo.SetCheckRun("aaaa", "build", "completed", "success")
			e.intake.subscribe(number)
			e.repo.Advance(time.Minute)
			e.pollClean()
			e.repo.Advance(time.Minute)
			e.repo.Push(number, "bbbb")
			e.repo.SetCheckRun("bbbb", "build", "completed", "success")
			e.pollClean()
			var carried, atFirst, atSecond int
			for _, batch := range e.intake.recorded() {
				if len(batch.PRs) == 0 {
					continue
				}
				carried++
				switch {
				case batch.ReadAt.Equal(start.Add(time.Minute)):
					atFirst++
				case batch.ReadAt.Equal(start.Add(2 * time.Minute)):
					atSecond++
				default:
					t.Fatalf("batch %s carries a PR snapshot read at %s, not GitHub's clock", batch.Resource, batch.ReadAt)
				}
			}
			if atFirst == 0 || atSecond == 0 {
				t.Fatalf("PR snapshots: %d carried, %d in the first round, %d in the second", carried, atFirst, atSecond)
			}
		})
	}
}

// TestObserveCursorSettlesUnderSteadyLoad: a stream that gets most of a
// page of new comments every round still moves its cursor as rows settle,
// so a restart rereads one lookback, not the whole history, and every
// comment is read.
func TestObserveCursorSettlesUnderSteadyLoad(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(config *model.Config) {
		config.Poll.MaxPages = 1
		config.Observe.Comments = true
	})
	e.intake.cursors[model.CursorComments] = model.Cursor{Key: model.CursorComments, Since: start, EnabledAt: start}
	var rows []ghComment
	now, id := start, int64(0)
	all := func(ghComment) bool { return true }
	e.source.client = scripted(func(request model.GHRequest) model.GHResponse { return comments(request, rows, all, now) })
	for range 12 {
		for range 80 {
			id++
			rows = append(rows, commentRow(id, now))
			now = now.Add(5 * time.Second)
		}
		now = now.Add(10 * time.Minute)
		if err := e.source.pollCommentsStream(context.Background(), &round{}); err != nil {
			t.Fatal(err)
		}
	}
	cursor := e.intake.cursors[model.CursorComments]
	if settled := now.Add(-time.Hour); cursor.Since.Before(settled.Add(-20 * time.Minute)) {
		t.Fatalf("cursor %s lags GitHub's settled line %s", cursor.Since, settled)
	}
	for _, row := range rows {
		if !e.intake.hasEvent(model.CommentID(row.ID, row.CreatedAt)) {
			t.Fatalf("comment %d never read", row.ID)
		}
	}
}
