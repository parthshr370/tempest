package source

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
)

// These tests are the regressions of the fourth G1 review
// (context/live/verify/g1-check.md, "Check 4"): a 304 for a cached first
// page handed back the page's old pagination, so a full last page that
// gained a next page kept reporting the listing ended there.

// TestRevalidatedFullPageFindsItsNewNextPage: 100 comments fill one page
// exactly; a 101st leaves that page unchanged, so its ETag answers 304,
// but the listing now has a second page, which the reply must name.
func TestRevalidatedFullPageFindsItsNewNextPage(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.source.client = directClient{base: e.server.URL()}
	number := e.repo.OpenIssue(alice, "t", "")
	for range listPageSize {
		e.repo.Comment(number, alice, "context")
	}
	request := model.GHRequest{Path: fmt.Sprintf("repos/o/r/issues/%d/comments", number), Query: url.Values{"per_page": {"100"}}}
	old, err := e.source.get(context.Background(), e.source.newRound(), request)
	if err != nil {
		t.Fatal(err)
	}
	if old.NextPath != "" {
		t.Fatal("fixture: 100 comments did not end on one full page")
	}
	e.repo.Comment(number, alice, "@acme-bot new request")
	got, err := e.source.get(context.Background(), e.source.newRound(), request)
	if err != nil {
		t.Fatal(err)
	}
	if got.NextPath == "" {
		t.Fatalf("the reply hid the new second page: status %d, Link %q", got.Status, got.Header.Get("Link"))
	}
}

// TestRevalidatedReplyTakesThe304Pagination: a 304 hands back the stored
// page with the next page its own Link header names, keeps the stored one
// when it carries no Link header, and a full page without a next page is
// not kept, so no later 304 can call it the whole listing.
func TestRevalidatedReplyTakesThe304Pagination(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	full := []byte("[{}")
	for range listPageSize - 1 {
		full = append(full, ",{}"...)
	}
	full = append(full, ']')
	// etag is the page's current ETag, link its next page and sentETag
	// the validator of the last request.
	var etag, link, sentETag string
	e.source.client = scripted(func(request model.GHRequest) model.GHResponse {
		sentETag = request.ETag
		header := http.Header{"Date": {start.Format(http.TimeFormat)}}
		if link != "" {
			header.Set("Link", "<https://api.github.com/"+link+">; rel=\"next\"")
		}
		if request.ETag == etag {
			return model.GHResponse{Status: http.StatusNotModified, Header: header, NextPath: link}
		}
		return model.GHResponse{Status: http.StatusOK, Header: header, ETag: etag, Body: full, NextPath: link}
	})
	get := func() model.GHResponse {
		t.Helper()
		response, err := e.source.get(context.Background(), e.source.newRound(), model.GHRequest{Path: "probe", Query: url.Values{"per_page": {"100"}}})
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	etag, link = "one", "probe?after=a&per_page=100"
	get()
	link = "probe?after=b&per_page=100"
	if got := get(); sentETag != "one" || got.NextPath != link {
		t.Fatalf("304 with a Link (validator %q): next %q, want %q", sentETag, got.NextPath, link)
	}
	link = ""
	if got := get(); sentETag != "one" || got.NextPath != "probe?after=b&per_page=100" {
		t.Fatalf("304 without a Link (validator %q): next %q, want the stored one", sentETag, got.NextPath)
	}
	etag = "two"
	if got := get(); got.Status != http.StatusOK || got.NextPath != "" {
		t.Fatalf("changed page: %+v", got)
	}
	if got := get(); sentETag != "" || got.Status != http.StatusOK {
		t.Fatalf("a full page without a next page was revalidated with %q", sentETag)
	}
}

// TestObserveCacheCannotHideNewTail: an observe stream's cursor sits at a
// second holding exactly 100 comments, all on its first page. A 101st,
// a request, joins that second; the first page stays the same. Neither the
// cursor's ETag nor the in-memory reply may keep the stream from reading
// the new tail in the next rounds.
func TestObserveCacheCannotHideNewTail(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		// memoryOnly clears the cursor's ETag every round, leaving only
		// the in-memory revalidation.
		memoryOnly bool
	}{{"cursor ETag", false}, {"memory reply", true}} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, func(config *model.Config) { config.Observe.Comments = true })
			e.source.client = directClient{base: e.server.URL()}
			number := e.repo.OpenIssue(alice, "t", "")
			e.repo.Advance(time.Second)
			at := e.repo.Now()
			for range listPageSize {
				e.repo.Comment(number, alice, "context")
			}
			e.intake.cursors[model.CursorComments] = model.Cursor{Key: model.CursorComments, Since: at, EnabledAt: start}
			if err := e.source.pollCommentsStream(context.Background(), e.source.newRound()); err != nil {
				t.Fatal(err)
			}
			id := e.repo.Comment(number, alice, "@acme-bot new request")
			for index := range 70 {
				if test.memoryOnly {
					cursor := e.intake.cursors[model.CursorComments]
					cursor.ETag = ""
					e.intake.cursors[model.CursorComments] = cursor
				}
				e.repo.Advance(time.Minute)
				if err := e.source.pollCommentsStream(context.Background(), e.source.newRound()); err != nil {
					t.Fatal(err)
				}
				if e.intake.hasEvent(model.CommentID(id, at)) {
					if index > 0 {
						t.Fatalf("the new request was read only in round %d", index)
					}
					return
				}
			}
			t.Fatalf("the new request was not read in 70 rounds; cursor %s", e.intake.cursors[model.CursorComments].Since)
		})
	}
}
