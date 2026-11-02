package source

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
)

// pollNotifications lists the account's notifications and flags matching
// Issue and PR threads for a full reread. The flags and the cursor commit
// together; the cursor is a listing position only and never bounds what a
// thread read returns. Nothing is marked read on GitHub.
//
// The cursor's Since is settled coverage on GitHub's clock: every row
// updated before it was listed by a read at least one lookback later, so
// no late-indexed row can still appear below it. Each round lists from
// Since newest first. When that head does not fit the round, the rows
// below it are listed oldest first in time windows (since, before) that
// each fit one page, and Since moves up to the end of the windows read, so
// a long inbox is worked through across rounds instead of reread from the
// top. Every request is a first page, which a deletion cannot shift.
func (s *Source) pollNotifications(ctx context.Context, rd *round) error {
	stored, found, err := s.intake.Cursor(ctx, model.CursorNotifications)
	if err != nil {
		return fmt.Errorf("read notifications cursor: %w", err)
	}
	read := &notificationRead{s: s, rd: rd, flags: map[int]model.ThreadUpdate{}}
	base := stored.Since
	head := model.GHRequest{Path: "notifications", Query: notificationQuery(base, time.Time{})}
	if !base.IsZero() {
		// Last-Modified validates only the exact URL it came from.
		head.LastModified = stored.LastModified
	}
	response, err := s.get(ctx, rd, head)
	if err != nil {
		return err
	}
	rd.pollInterval = response.PollInterval
	if response.Status == http.StatusNotModified {
		return nil
	}
	date, err := serverDate(response)
	if err != nil {
		return err
	}
	if base.IsZero() {
		// First start: notifications from one lookback before GitHub's
		// clock count. The first listing has no since and stops at that
		// start, so the local clock never sets the horizon.
		base = date.Add(-s.lookback).Truncate(time.Second)
	}
	rows, err := decode[[]ghNotification](response, "notifications")
	if err != nil {
		return err
	}
	var newest time.Time
	for _, row := range rows {
		if !row.UpdatedAt.Before(base) {
			read.flag(row)
			newest = later(newest, row.UpdatedAt)
		}
	}
	// The head holds every row newer than its oldest. It covers everything
	// from base when it ends or reaches below base.
	headComplete := response.NextPath == "" || len(rows) > 0 && rows[len(rows)-1].UpdatedAt.Before(base)
	complete, covered := headComplete, base
	var backlogErr error
	if !headComplete {
		covered, complete, backlogErr = read.backlog(ctx, base, rows[len(rows)-1].UpdatedAt.Add(time.Second))
	}
	// Coverage settles one lookback behind GitHub's clock. A complete
	// listing keeps its URL, and so its validator, unless moving the
	// cursor drops listed rows: for one page, once its newest row settled;
	// for more, as far as rows settled. An incomplete one moves the cursor
	// up as far as it read.
	settled := date.Add(-s.lookback)
	cursor := stored
	cursor.Key, cursor.Since, cursor.LastModified = model.CursorNotifications, base, ""
	switch target := earlier(settled, newest.Add(time.Second)); {
	case !complete:
		cursor.Since = later(base, earlier(settled, covered))
	case newest.IsZero():
	case headComplete && !newest.Add(time.Second).After(settled), !headComplete && read.oldest.Before(target):
		cursor.Since = later(base, target)
	}
	if complete && !stored.Since.IsZero() && cursor.Since.Equal(stored.Since) {
		// Last-Modified covers the whole inbox: a 304 for the same URL
		// means nothing below the head changed either.
		cursor.LastModified = response.LastModified
	}
	if err := read.commit(ctx, cursor, found, stored); err != nil {
		return err
	}
	if backlogErr != nil {
		return backlogErr
	}
	if !complete && !covered.After(base) {
		return fmt.Errorf("notifications: one second holds more rows than a round may read; the cursor stays at %s", base.Format(time.RFC3339))
	}
	return nil
}

// notificationRead collects one round's thread flags.
type notificationRead struct {
	s     *Source
	rd    *round
	flags map[int]model.ThreadUpdate
	// oldest is the oldest row listed at or after the cursor.
	oldest time.Time
	// requests counts the round's listing requests, the head included;
	// the backlog stops at limit.
	requests, limit int
}

// flag marks the repository's Issue or PR thread of row, keeping the
// newest notification per thread.
func (n *notificationRead) flag(row ghNotification) {
	if n.oldest.IsZero() || row.UpdatedAt.Before(n.oldest) {
		n.oldest = row.UpdatedAt
	}
	if !strings.EqualFold(row.Repository.FullName, n.s.repo) || (row.Subject.Type != "Issue" && row.Subject.Type != "PullRequest") {
		return
	}
	number, ok := n.s.subjectNumber(row.Subject.URL)
	if !ok {
		return
	}
	if known, seen := n.flags[number]; seen && !row.UpdatedAt.After(known.NotifiedAt) {
		return
	}
	reconcile := true
	n.flags[number] = model.ThreadUpdate{
		Ref:       model.ThreadRef{Number: number, IsPR: row.Subject.Type == "PullRequest"},
		Reconcile: &reconcile, NotifiedAt: row.UpdatedAt,
	}
}

// backlog lists rows updated in [from, to) in windows (since, before)
// read by their first page. A window that fits one page covers itself and
// moves from up; the next one is twice as wide. A window that does not fit
// still covers the newest rows of its first page: when it reaches to, to
// drops to its oldest row (below it, when that whole page is one second,
// which is then read by page number), and the next window is half as
// wide. It returns from, the end of the contiguous coverage from the
// start, and whether coverage reached to. A round spends at most MaxPages
// plus the halving bound on it.
func (n *notificationRead) backlog(ctx context.Context, from, to time.Time) (time.Time, bool, error) {
	n.limit = 1 + n.s.maxPages + scanSeekProbes
	n.requests = 1
	width := to.Sub(from)
	for from.Before(to) {
		if n.requests >= n.limit {
			return from, false, nil
		}
		until := from.Add(width)
		if until.After(to) {
			until = to
		}
		rows, next, err := n.list(ctx, from, until)
		if err != nil {
			return from, false, err
		}
		switch {
		case !next:
			from, width = until, 2*width
			continue
		case until.Sub(from) <= time.Second:
			// One second holds more than a page.
			if done, err := n.second(ctx, from); !done || err != nil {
				return from, false, err
			}
			from, width = until, 2*width
			continue
		case until.Equal(to) && len(rows) > 0:
			// Rows of the window newer than its first page's oldest are all
			// on that page.
			oldest := rows[len(rows)-1].UpdatedAt
			to = later(from.Add(time.Second), oldest.Add(time.Second))
			if rows[0].UpdatedAt.Equal(oldest) && oldest.After(from) {
				// A full page in one second at the top: read that second.
				if done, err := n.second(ctx, oldest); !done || err != nil {
					return from, false, err
				}
				to = oldest
			}
		}
		width = max(time.Second, (earlier(until, to).Sub(from) / 2).Truncate(time.Second))
	}
	return from, true, nil
}

// list flags the first page of rows updated in [since, before) and
// reports whether more rows follow.
func (n *notificationRead) list(ctx context.Context, since, before time.Time) ([]ghNotification, bool, error) {
	response, err := n.s.get(ctx, n.rd, model.GHRequest{Path: "notifications", Query: notificationQuery(since, before)})
	n.requests++
	if err != nil {
		return nil, false, err
	}
	rows, err := decode[[]ghNotification](response, "notifications")
	if err != nil {
		return nil, false, err
	}
	for _, row := range rows {
		n.flag(row)
	}
	return rows, response.NextPath != "", nil
}

// second flags every row of one second by page number: no narrower window
// exists. done is false when the round's bound stopped it.
func (n *notificationRead) second(ctx context.Context, at time.Time) (bool, error) {
	query := notificationQuery(at, at.Add(time.Second))
	for page := 1; ; page++ {
		if n.requests >= n.limit {
			return false, nil
		}
		query.Set("page", strconv.Itoa(page))
		response, err := n.s.get(ctx, n.rd, model.GHRequest{Path: "notifications", Query: query})
		n.requests++
		if err != nil {
			return false, err
		}
		rows, err := decode[[]ghNotification](response, "notifications")
		if err != nil {
			return false, err
		}
		for _, row := range rows {
			n.flag(row)
		}
		if response.NextPath == "" {
			return true, nil
		}
	}
}

// commit stores the flags and, with the last of them, the cursor.
func (n *notificationRead) commit(ctx context.Context, cursor model.Cursor, found bool, stored model.Cursor) error {
	updates := make([]model.ThreadUpdate, 0, len(n.flags))
	for _, update := range n.flags {
		updates = append(updates, update)
	}
	slices.SortFunc(updates, func(a, b model.ThreadUpdate) int { return a.Ref.Number - b.Ref.Number })
	if len(updates) == 0 && found && sameCursor(cursor, stored) {
		return nil
	}
	for start := 0; start == 0 || start < len(updates); start += model.MaxBatchEvents {
		end := min(start+model.MaxBatchEvents, len(updates))
		batch := model.IntakeBatch{Resource: "notifications", Threads: updates[start:end]}
		if end == len(updates) {
			batch.Cursor = &cursor
		}
		if err := n.s.ingest(ctx, n.rd, batch); err != nil {
			return err
		}
	}
	return nil
}

// notificationQuery lists the whole inbox, read or not, updated at or
// after since and, when set, before before.
func notificationQuery(since, before time.Time) url.Values {
	query := url.Values{"all": {"true"}, "per_page": {strconv.Itoa(notificationsPage)}}
	if !since.IsZero() {
		query.Set("since", since.UTC().Format(time.RFC3339))
	}
	if !before.IsZero() {
		query.Set("before", before.UTC().Format(time.RFC3339))
	}
	return query
}

// streamSpec describes one repository-wide observe stream.
type streamSpec struct {
	key  model.CursorKey
	path string
	// extra query parameters beside the oldest-first defaults.
	extra url.Values
	// entries maps one decoded page to entries; enabledAt filters items.
	entries func(page []byte, enabledAt, observedAt time.Time) ([]entry, error)
}

func (s *Source) pollIssuesStream(ctx context.Context, rd *round) error {
	if !s.observe.Issues && !s.observe.Pulls {
		return nil
	}
	return s.pollStream(ctx, rd, streamSpec{
		key: model.CursorIssues, path: s.repoPath("issues"), extra: url.Values{"state": {"all"}},
		entries: func(page []byte, enabledAt, observedAt time.Time) ([]entry, error) {
			rows, err := decodeBytes[[]ghIssue](page, "issues")
			if err != nil {
				return nil, err
			}
			entries := make([]entry, 0, len(rows))
			for _, row := range rows {
				item := entry{updated: row.UpdatedAt}
				wanted := s.observe.Issues
				if row.PullRequest != nil {
					wanted = s.observe.Pulls
				}
				if event, ok := s.openedEvent(row, observedAt); ok && wanted && !row.CreatedAt.Before(enabledAt) {
					item.events = []model.Event{event}
				}
				entries = append(entries, item)
			}
			return entries, nil
		},
	})
}

func (s *Source) pollCommentsStream(ctx context.Context, rd *round) error {
	if !s.observe.Comments {
		return nil
	}
	return s.pollStream(ctx, rd, streamSpec{key: model.CursorComments, path: s.repoPath("issues/comments"), entries: s.streamComments(false)})
}

func (s *Source) pollReviewCommentsStream(ctx context.Context, rd *round) error {
	if !s.observe.Comments {
		return nil
	}
	return s.pollStream(ctx, rd, streamSpec{key: model.CursorReviewComments, path: s.repoPath("pulls/comments"), entries: s.streamComments(true)})
}

// streamComments maps a repository-wide comment page to entries. The
// thread comes from the comment's issue or pull request API URL.
func (s *Source) streamComments(inline bool) func([]byte, time.Time, time.Time) ([]entry, error) {
	return func(page []byte, enabledAt, observedAt time.Time) ([]entry, error) {
		rows, err := decodeBytes[[]ghComment](page, "comments")
		if err != nil {
			return nil, err
		}
		entries := make([]entry, 0, len(rows))
		for _, row := range rows {
			item := entry{updated: row.UpdatedAt}
			apiURL, isPR := row.IssueURL, strings.Contains(row.HTMLURL, "/pull/")
			if inline {
				apiURL, isPR = row.PullRequestURL, true
			}
			number, ok := s.subjectNumber(apiURL)
			if ok && !row.UpdatedAt.Before(enabledAt) {
				events := s.commentEvents(row, model.ThreadRef{Number: number, IsPR: isPR}, inline, observedAt)
				// A version older than the enable time is backlog.
				item.events = slices.DeleteFunc(events, func(event model.Event) bool { return event.At.Before(enabledAt) })
			}
			entries = append(entries, item)
		}
		return entries, nil
	}
}

// pollStream reads one observe stream in updated order from its cursor.
// The first enabled round only records EnabledAt, from GitHub's clock:
// nothing older is imported, and local skew cannot move the start.
//
// Each request is the first page from a since: after a page the next
// request starts at the page's last updated_at, so a deletion cannot shift
// an unread row onto a page already read. Only when one timestamp holds
// more than a page does the read page through that timestamp by number.
// Each page commits with the cursor at its last updated_at; rereads start
// a lookback before it. Pages that only reread the lookback are bounded,
// and past that bound the round skips to the cursor, so a dense lookback
// cannot starve progress. The ETag is kept only while the cursor, and so
// the request URL, is unchanged.
//
// model.Cursor has no ID to resume within one timestamp, so a timestamp
// with more rows than one round reads is not resumable; see the report.
func (s *Source) pollStream(ctx context.Context, rd *round, spec streamSpec) error {
	stored, found, err := s.intake.Cursor(ctx, spec.key)
	if err != nil {
		return fmt.Errorf("read %s cursor: %w", spec.key, err)
	}
	resource := string(spec.key)
	if !found || stored.EnabledAt.IsZero() {
		response, err := s.get(ctx, rd, model.GHRequest{Path: spec.path, Query: url.Values{"per_page": {"1"}}})
		if err != nil {
			return err
		}
		date, err := serverDate(response)
		if err != nil {
			return err
		}
		baseline := model.Cursor{Key: spec.key, EnabledAt: date, Since: date}
		return s.ingest(ctx, rd, model.IntakeBatch{Resource: resource, Cursor: &baseline})
	}
	read := &streamRead{s: s, rd: rd, spec: spec, stored: stored, cursor: stored}
	since := later(s.rereadFrom(stored.Since), stored.EnabledAt)
	for first := true; ; first = false {
		query := listQuery(since, true)
		for key, values := range spec.extra {
			query[key] = values
		}
		request := model.GHRequest{Path: spec.path, Query: query}
		if first {
			request.ETag = stored.ETag
		}
		response, err := s.get(ctx, rd, request)
		if err != nil {
			return err
		}
		if response.Status == http.StatusNotModified {
			return nil
		}
		if first {
			read.etag = response.ETag
		}
		last, err := read.page(ctx, response)
		if err != nil {
			return err
		}
		for next := response.NextPath; next != "" && !last.After(since); next = response.NextPath {
			// The whole page has the timestamp since: page through it.
			if read.exhausted() {
				return nil
			}
			if response, err = s.get(ctx, rd, model.GHRequest{Path: next}); err != nil {
				return err
			}
			if last, err = read.page(ctx, response); err != nil {
				return err
			}
		}
		if response.NextPath == "" {
			break
		}
		since = last
		if read.rereads >= rereadPageFactor*s.maxPages && since.Before(stored.Since) {
			// The lookback alone outgrew the round: skip to the cursor.
			since = stored.Since
		}
		if read.exhausted() {
			return nil
		}
	}
	if read.pages == 1 && read.cursor.Since.Equal(stored.Since) && read.etag != read.cursor.ETag {
		// A one-page read that moved nothing: remember its validator.
		cursor := read.cursor
		cursor.ETag = read.etag
		return s.ingest(ctx, rd, model.IntakeBatch{Resource: resource, Cursor: &cursor})
	}
	return nil
}

// streamRead is one round of an observe stream.
type streamRead struct {
	s      *Source
	rd     *round
	spec   streamSpec
	stored model.Cursor
	cursor model.Cursor
	etag   string
	// pages counts pages read; rereads those with nothing past the cursor.
	pages, fresh, rereads int
}

// exhausted reports the round's page bound: MaxPages pages past the cursor
// and the lookback reread allowance.
func (r *streamRead) exhausted() bool {
	return r.fresh >= r.s.maxPages || r.pages >= (rereadPageFactor+1)*r.s.maxPages
}

// page commits one listed page in bounded runs and returns its last
// updated_at.
func (r *streamRead) page(ctx context.Context, response model.GHResponse) (time.Time, error) {
	entries, err := r.spec.entries(response.Body, r.stored.EnabledAt, r.s.clock.Now())
	if err != nil {
		return time.Time{}, err
	}
	r.pages++
	if len(entries) == 0 {
		return time.Time{}, nil
	}
	last := entries[len(entries)-1].updated
	if last.After(r.stored.Since) {
		r.fresh++
	} else {
		r.rereads++
	}
	for _, run := range split(entries) {
		next := r.cursor
		next.Since = later(r.cursor.Since, run[len(run)-1].updated)
		events := eventsOf(run)
		if len(events) == 0 && sameCursor(next, r.cursor) {
			continue
		}
		next.ETag = ""
		if err := r.s.ingest(ctx, r.rd, model.IntakeBatch{Resource: string(r.spec.key), Events: events, Cursor: &next}); err != nil {
			return time.Time{}, err
		}
		r.cursor = next
	}
	return last, nil
}

// pollBranchCI watches the default branch head's settled CI. The first
// settled state is the baseline; later rounds emit only when the settled
// outcome fingerprint changes, pinned to the commit it describes.
func (s *Source) pollBranchCI(ctx context.Context, rd *round) error {
	if !s.observe.CI {
		return nil
	}
	if s.defaultBranch == "" {
		return errors.New("default branch unknown")
	}
	stored, _, err := s.intake.Cursor(ctx, model.CursorBranchCI)
	if err != nil {
		return fmt.Errorf("read %s cursor: %w", model.CursorBranchCI, err)
	}
	response, err := s.get(ctx, rd, model.GHRequest{Path: s.repoPath("branches/%s", url.PathEscape(s.defaultBranch))})
	if err != nil {
		return err
	}
	head, err := decode[ghBranchHead](response, "branch")
	if err != nil {
		return err
	}
	sha := head.Commit.SHA
	if sha == "" {
		return nil
	}
	ci, settled, err := s.readCI(ctx, rd, sha)
	if err != nil || !settled {
		return err
	}
	cursor := stored
	cursor.Key, cursor.BranchSHA, cursor.Fingerprint = model.CursorBranchCI, sha, ci.Fingerprint
	if sha == stored.BranchSHA && ci.Fingerprint == stored.Fingerprint {
		return nil
	}
	batch := model.IntakeBatch{Resource: string(model.CursorBranchCI), Cursor: &cursor}
	if stored.Fingerprint != "" && stored.Fingerprint != ci.Fingerprint {
		event := ciEvent(model.BranchCIID(s.defaultBranch, sha, ci.Fingerprint), model.EventBranchCI, model.ThreadRef{}, ci,
			fmt.Sprintf("https://github.com/%s/commit/%s", s.repo, sha), s.clock.Now())
		event.Branch = s.defaultBranch
		batch.Events = []model.Event{event}
	}
	return s.ingest(ctx, rd, batch)
}

// readCI reads check runs and commit statuses of sha. settled is false
// while anything is pending, when nothing reports, or when a listing was
// cut by MaxPages.
func (s *Source) readCI(ctx context.Context, rd *round, sha string) (model.CISnapshot, bool, error) {
	var outcomes []ciOutcome
	checks := model.GHRequest{Path: s.repoPath("commits/%s/check-runs", sha), Query: url.Values{"filter": {"latest"}, "per_page": {strconv.Itoa(listPageSize)}}}
	complete, err := s.walk(ctx, rd, checks, func(response model.GHResponse) error {
		page, err := decode[ghCheckRuns](response, "check runs")
		if err != nil {
			return err
		}
		for _, run := range page.CheckRuns {
			outcome := ciOutcome{name: run.Name, pending: run.Status != "completed" || run.Conclusion == nil}
			if run.Conclusion != nil {
				outcome.outcome = *run.Conclusion
			}
			outcomes = append(outcomes, outcome)
		}
		return nil
	})
	if err != nil || !complete {
		return model.CISnapshot{}, false, err
	}
	statuses := model.GHRequest{Path: s.repoPath("commits/%s/status", sha), Query: url.Values{"per_page": {strconv.Itoa(listPageSize)}}}
	complete, err = s.walk(ctx, rd, statuses, func(response model.GHResponse) error {
		page, err := decode[ghCombinedStatus](response, "combined status")
		if err != nil {
			return err
		}
		for _, status := range page.Statuses {
			outcomes = append(outcomes, ciOutcome{name: status.Context, outcome: status.State, pending: status.State == "pending"})
		}
		return nil
	})
	if err != nil || !complete {
		return model.CISnapshot{}, false, err
	}
	ci, settled := snapshot(sha, outcomes)
	return ci, settled, nil
}
