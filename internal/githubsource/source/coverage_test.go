package source

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/githubsource/ghfake"
	"go.harness.dev/harness/internal/githubsource/model"
)

// These tests are the adversarial regressions of the G1 review
// (context/live/verify/g1-check.md, findings 1 to 5): each one lost or
// invented a request before the fix.

// skewedClock is a source clock that is not GitHub's.
type skewedClock struct{ now time.Time }

func (c skewedClock) Now() time.Time { return c.now }

// commentsRequests counts requests for one thread's comment listing.
func commentsRequests(requests []model.GHRequest, number int) int {
	count := 0
	for _, request := range requests {
		if strings.HasPrefix(request.Path, fmt.Sprintf("repos/o/r/issues/%d/comments", number)) {
			count++
		}
	}
	return count
}

// TestDeletionDuringWalkDoesNotSkipAnUnreadComment deletes an already read
// comment between two page reads of one walk. Offset pagination then moves
// the first unread comment back onto the page already read; the position
// must not advance past it.
func TestDeletionDuringWalkDoesNotSkipAnUnreadComment(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	number := e.repo.OpenIssue(alice, "t", "")
	var first, request int64
	var requestAt time.Time
	for i := range 201 {
		body := fmt.Sprintf("comment %d", i)
		if i == 100 {
			body = "@acme-bot please fix"
			requestAt = e.repo.Now()
		}
		id := e.repo.Comment(number, alice, body)
		switch i {
		case 0:
			first = id
		case 100:
			request = id
		}
		e.repo.Advance(time.Minute)
	}
	e.intake.subscribe(number)
	seen, deleted := 0, false
	e.client.fail = func(req model.GHRequest) error {
		if strings.HasPrefix(req.Path, fmt.Sprintf("repos/o/r/issues/%d/comments", number)) {
			seen++
			if seen == 2 {
				e.repo.DeleteComment(first)
				deleted = true
			}
		}
		return nil
	}
	for range 3 {
		e.pollClean()
		e.repo.Advance(time.Minute)
	}
	if !deleted {
		t.Fatal("the deletion hook never ran")
	}
	if !e.intake.hasEvent(model.CommentID(request, requestAt)) {
		t.Fatalf("surviving unread mention %d was skipped; position %+v", request, e.intake.thread(number).Comments)
	}
}

// TestEqualTimestampsBeyondThePageBudgetProgress lists more comments with
// one timestamp than a round may read: every round must move forward.
func TestEqualTimestampsBeyondThePageBudgetProgress(t *testing.T) {
	t.Parallel()
	const comments = 501
	e := newEnv(t, func(config *model.Config) { config.Poll.MaxPages = 1 })
	number := e.repo.OpenIssue(alice, "t", "")
	var last int64
	for range comments {
		last = e.repo.Comment(number, alice, "@acme-bot request")
	}
	e.intake.subscribe(number)
	for round := 0; len(e.intake.eventsOfKind(model.EventComment)) < comments && round < 12; round++ {
		read := len(e.intake.eventsOfKind(model.EventComment))
		e.pollClean()
		if len(e.intake.eventsOfKind(model.EventComment)) == read {
			t.Fatalf("round %d made no progress at %d comments; position %+v", round, read, e.intake.thread(number).Comments)
		}
		e.repo.Advance(time.Minute)
	}
	if got := len(e.intake.eventsOfKind(model.EventComment)); got != comments || !e.intake.hasEvent(model.CommentID(last, start)) {
		t.Fatalf("comments read = %d of %d, last present %t", got, comments, e.intake.hasEvent(model.CommentID(last, start)))
	}
}

// TestTruncatedNotificationsReachTheOldestThread keeps an unchanged inbox
// one row longer than a round's page budget.
func TestTruncatedNotificationsReachTheOldestThread(t *testing.T) {
	t.Parallel()
	const threads = 51
	e := newEnv(t, func(config *model.Config) { config.Poll.MaxPages = 1 })
	e.intake.hideFlagged = true
	for range threads {
		number := e.repo.OpenIssue(alice, "t", "")
		e.repo.Notify(number)
		e.repo.Advance(time.Second)
	}
	for range 3 {
		e.pollClean()
		e.repo.Advance(time.Minute)
	}
	for number := 1; number <= threads; number++ {
		if thread, ok := e.intake.threads[number]; !ok || !thread.NeedsReconcile {
			t.Fatalf("thread %d never flagged", number)
		}
	}
}

// TestTruncatedNotificationsBacklogBeyondOnePageOfProbes needs several
// backlog windows, one of them a single second holding more than a page.
func TestTruncatedNotificationsBacklogBeyondOnePageOfProbes(t *testing.T) {
	t.Parallel()
	const spread, burst = 140, 60
	e := newEnv(t, func(config *model.Config) { config.Poll.MaxPages = 1 })
	e.intake.hideFlagged = true
	for range spread {
		e.repo.Notify(e.repo.OpenIssue(alice, "t", ""))
		e.repo.Advance(7 * time.Second)
	}
	for range burst {
		e.repo.Notify(e.repo.OpenIssue(alice, "t", ""))
	}
	e.repo.Advance(time.Second)
	for range 60 {
		e.repo.Notify(e.repo.OpenIssue(alice, "t", ""))
		e.repo.Advance(time.Second)
	}
	// The first round reads all 260 rows in a handful of first pages; the
	// second repeats the unchanged URL and keeps its validator; the third
	// is one 304.
	for round, most := range []int{12, 12, 1} {
		before := len(e.client.requests())
		e.pollClean()
		if sent := len(e.client.requests()) - before; sent > most {
			t.Fatalf("round %d sent %d requests, want at most %d", round, sent, most)
		}
		e.repo.Advance(time.Minute)
	}
	for number := 1; number <= spread+burst+60; number++ {
		if thread, ok := e.intake.threads[number]; !ok || !thread.NeedsReconcile {
			t.Fatalf("thread %d never flagged", number)
		}
	}
}

// TestFirstSeenEditedCommentIsOnlyContext edits a comment into a stop
// command before the first poll. REST returns only the edited body, so no
// original comment event may carry it.
func TestFirstSeenEditedCommentIsOnlyContext(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	number := e.repo.OpenIssue(alice, "t", "")
	id := e.repo.Comment(number, alice, "ordinary context")
	e.repo.Advance(time.Minute)
	e.repo.EditComment(id, "@acme-bot stop")
	e.intake.subscribe(number)
	e.pollClean()
	if created := e.intake.eventsOfKind(model.EventComment); len(created) != 0 {
		t.Fatalf("a creation event was invented from edited text: %+v", created)
	}
	edits := e.intake.eventsOfKind(model.EventCommentEdited)
	if len(edits) != 1 || edits[0].ID != model.CommentID(id, start.Add(time.Minute)) || edits[0].Body != "@acme-bot stop" {
		t.Fatalf("edited version = %+v", edits)
	}
}

// TestSourceClockSkewLosesNothing runs the source with a local clock three
// hours ahead of and behind GitHub's. Cursors and stream baselines come
// from GitHub's clock, so neither direction excludes new activity.
func TestSourceClockSkewLosesNothing(t *testing.T) {
	t.Parallel()
	for _, skew := range []time.Duration{3 * time.Hour, -3 * time.Hour} {
		t.Run(skew.String(), func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, func(config *model.Config) { config.Observe.Comments = true })
			e.source.clock = skewedClock{start.Add(skew)}
			backlog := e.repo.Comment(e.repo.OpenIssue(alice, "older", ""), alice, "before the stream was enabled")
			e.repo.Advance(time.Minute)
			e.pollClean() // first start: notification and stream baselines
			e.repo.Advance(time.Minute)
			number := e.repo.OpenIssue(alice, "t", "")
			comment := e.repo.Comment(number, carol, "@acme-bot please fix")
			commentAt := e.repo.Now()
			e.repo.Notify(number)
			e.repo.Advance(time.Minute)
			for range 3 {
				e.pollClean()
			}
			if thread, ok := e.intake.threads[number]; !ok || thread.Title != "t" {
				t.Fatal("notification of the new thread was excluded")
			}
			if !e.intake.hasEvent(model.CommentID(comment, commentAt)) {
				t.Fatal("the new mention was excluded")
			}
			observed, imported := false, false
			for _, batch := range e.intake.recorded() {
				for _, event := range batch.Events {
					observed = observed || batch.Resource == string(model.CursorComments) && event.CommentID == comment
					imported = imported || event.CommentID == backlog
				}
			}
			if !observed {
				t.Fatal("the comments stream excluded activity after it was enabled")
			}
			if imported {
				t.Fatal("the comments stream imported activity from before it was enabled")
			}
		})
	}
}

// TestDenseLookbackWindowProgresses fills one lookback hour with more
// comments than the reread allowance of a round.
func TestDenseLookbackWindowProgresses(t *testing.T) {
	t.Parallel()
	// More than the 400 rows (4 * MaxPages pages) the old reread cap allowed.
	const comments = 450
	e := newEnv(t, func(config *model.Config) { config.Poll.MaxPages = 1 })
	number := e.repo.OpenIssue(alice, "t", "")
	e.intake.subscribe(number)
	for i := range comments {
		e.repo.Comment(number, alice, fmt.Sprintf("c%d", i))
		e.repo.Advance(time.Second)
	}
	last := e.repo.Comment(number, carol, "@acme-bot last")
	lastAt := e.repo.Now()
	for round := 0; !e.intake.hasEvent(model.CommentID(last, lastAt)) && round < 12; round++ {
		read := len(e.intake.eventsOfKind(model.EventComment))
		e.pollClean()
		if len(e.intake.eventsOfKind(model.EventComment)) == read {
			t.Fatalf("round %d made no progress at %d comments", round, read)
		}
		e.repo.Advance(time.Second)
	}
	if !e.intake.hasEvent(model.CommentID(last, lastAt)) {
		t.Fatalf("read %d of %d comments", len(e.intake.eventsOfKind(model.EventComment)), comments+1)
	}
}

// TestDeletedPRReviewCommentDoesNotHideTheNextOne covers the inline stream
// with the same deletion between pages.
func TestDeletedReviewCommentDuringWalkDoesNotSkip(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	number := e.repo.OpenPull(alice, ghfake.PullSpec{Title: "p", HeadRef: "f", HeadSHA: "aaaaaaa"})
	var first, target int64
	var targetAt time.Time
	for i := range 201 {
		id := e.repo.ReviewComment(number, carol, ghfake.ReviewCommentSpec{Body: fmt.Sprintf("n%d", i), Path: "a.go", Line: 1})
		switch i {
		case 0:
			first = id
		case 100:
			target, targetAt = id, e.repo.Now()
		}
		e.repo.Advance(time.Minute)
	}
	e.intake.subscribe(number)
	seen := 0
	e.client.fail = func(req model.GHRequest) error {
		if strings.HasPrefix(req.Path, fmt.Sprintf("repos/o/r/pulls/%d/comments", number)) {
			seen++
			if seen == 2 {
				e.repo.DeleteReviewComment(first)
			}
		}
		return nil
	}
	for range 3 {
		e.pollClean()
		e.repo.Advance(time.Minute)
	}
	if !e.intake.hasEvent(model.ReviewCommentID(target, targetAt)) {
		t.Fatalf("inline comment %d was skipped", target)
	}
}

// TestDeletionsDuringWalkLoseNothing deletes read comments between the
// first two requests of a walk: the last row read, one before it, and more
// rows than the next request overlaps. Every surviving comment is read.
func TestDeletionsDuringWalkLoseNothing(t *testing.T) {
	t.Parallel()
	for name, deleted := range map[string][]int{
		"last read":       {99},
		"one before":      {50},
		"beyond overlap":  {0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
		"the whole page":  rangeInts(0, 100),
		"read and unread": {98, 99, 100, 101},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, func(config *model.Config) { config.Poll.MaxPages = 1 })
			number := e.repo.OpenIssue(alice, "t", "")
			var ids []int64
			for i := range 250 {
				ids = append(ids, e.repo.Comment(number, alice, fmt.Sprintf("c%d", i)))
				e.repo.Advance(time.Second)
			}
			e.intake.subscribe(number)
			gone := map[int64]bool{}
			seen := 0
			e.client.fail = func(req model.GHRequest) error {
				if strings.HasPrefix(req.Path, fmt.Sprintf("repos/o/r/issues/%d/comments", number)) {
					if seen++; seen == 2 {
						for _, index := range deleted {
							e.repo.DeleteComment(ids[index])
							gone[ids[index]] = true
						}
					}
				}
				return nil
			}
			read := map[int64]bool{}
			for range 6 {
				e.pollClean()
				e.repo.Advance(time.Minute)
				for _, event := range e.intake.eventsOfKind(model.EventComment) {
					read[event.CommentID] = true
				}
				if allRead(ids, gone, read) {
					break
				}
			}
			for _, id := range ids {
				if !gone[id] && !read[id] {
					t.Fatalf("surviving comment %d was never read", id)
				}
			}
		})
	}
}

func allRead(ids []int64, gone, read map[int64]bool) bool {
	for _, id := range ids {
		if !gone[id] && !read[id] {
			return false
		}
	}
	return true
}

func rangeInts(from, to int) []int {
	var out []int
	for i := from; i < to; i++ {
		out = append(out, i)
	}
	return out
}

// TestOverlapWindowContainsTheLastRowAndReachesPastIt pins the window
// arithmetic: the reply contains offset end-1 and reaches some way past
// it (49 rows for 100-row pages, 21 for 50), so a walk both proves
// continuity and progresses.
func TestOverlapWindowContainsTheLastRowAndReachesPastIt(t *testing.T) {
	t.Parallel()
	for perPage, gain := range map[int]int{listPageSize: 49, notificationsPage: 21} {
		if page, size := overlapWindow(0, perPage); page != 1 || size != perPage {
			t.Fatalf("overlapWindow(0, %d) = %d, %d", perPage, page, size)
		}
		for end := 1; end <= 200000; end++ {
			page, size := overlapWindow(end, perPage)
			start := (page - 1) * size
			if size < 1 || size > perPage || start > end-1 || start+size-1 < end-1 || start+size-end < gain {
				t.Fatalf("overlapWindow(%d, %d) = page %d of %d: rows [%d, %d)", end, perPage, page, size, start, start+size)
			}
		}
	}
}

// TestObserveStreamTiesAndDeletionsLoseNothing lists a same-second burst
// longer than a page and deletes a read row between requests.
func TestObserveStreamTiesAndDeletionsLoseNothing(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(config *model.Config) { config.Observe.Comments = true })
	number := e.repo.OpenIssue(alice, "t", "")
	e.pollClean() // enables the stream
	e.repo.Advance(time.Minute)
	var ids []int64
	for i := range 250 {
		ids = append(ids, e.repo.Comment(number, alice, fmt.Sprintf("tie %d", i)))
	}
	for i := range 150 {
		e.repo.Advance(time.Second)
		ids = append(ids, e.repo.Comment(number, alice, fmt.Sprintf("spread %d", i)))
	}
	seen := 0
	e.client.fail = func(req model.GHRequest) error {
		if req.Path == "repos/o/r/issues/comments" {
			if seen++; seen == 4 {
				e.repo.DeleteComment(ids[0])
			}
		}
		return nil
	}
	e.repo.Advance(time.Second)
	e.pollClean()
	observed := map[int64]bool{}
	for _, batch := range e.intake.recorded() {
		if batch.Resource == string(model.CursorComments) {
			for _, event := range batch.Events {
				observed[event.CommentID] = true
			}
		}
	}
	for _, id := range ids[1:] {
		if !observed[id] {
			t.Fatalf("stream lost comment %d", id)
		}
	}
}
