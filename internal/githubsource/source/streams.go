package source

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
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
// top. Every request is a first page, which a deletion cannot shift, but
// for one second holding more than a page; LastID keeps the progress
// through such a second (see second).
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
		resume := int64(0)
		if base.Equal(stored.Since) {
			resume = stored.LastID
		}
		covered, complete, backlogErr = read.backlog(ctx, base, rows[len(rows)-1].UpdatedAt.Add(time.Second), resume)
	}
	// Coverage settles one lookback behind GitHub's clock. A complete
	// listing keeps its URL, and so its validator, unless moving the
	// cursor drops listed rows: for one page, once its newest row settled;
	// for more, as far as rows settled. An incomplete one moves the cursor
	// up as far as it read, into a dense second only once it settled.
	settled := date.Add(-s.lookback)
	cursor := stored
	cursor.Key, cursor.Since, cursor.LastID, cursor.LastModified = model.CursorNotifications, base, 0, ""
	switch target := earlier(settled, newest.Add(time.Second)); {
	case !complete:
		cursor.Since = later(base, earlier(settled, covered))
		if read.partialAt.Equal(covered) && cursor.Since.Equal(covered) && !covered.Add(time.Second).After(settled) {
			// The second at covered was read in part, in ID order.
			cursor.LastID = read.partial
		}
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
	s.stall.clear(cursor.Since)
	switch {
	case backlogErr != nil:
		return backlogErr
	case read.unverified != nil:
		return read.unverified
	case !complete && !covered.After(base) && !read.advanced && !read.deferred:
		return fmt.Errorf("notifications: a round's requests ended at %s without progress; the cursor stays", covered.Format(time.RFC3339))
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
	// deferred reports that the backlog stopped because polling's share
	// of the rate window was spent.
	deferred bool
	// partialAt is the second where the backlog stopped when it walked
	// that second in ID order, and partial the lowest ID it read there.
	partialAt time.Time
	partial   int64
	// advanced reports that a second was read past where it started.
	advanced bool
	// unverified explains a second the round could not prove read.
	unverified error
}

// errRoundBound ends a listing at the round's request bound.
var errRoundBound = errors.New("round request bound reached")

// bounded reports that the backlog may send no more requests this round:
// it reached its request bound, or polling's share of the rate window is
// spent. The rows below the head are required but not urgent, so past the
// share they wait for budget instead of spending what the outbox and the
// tools are left; the head still flags the newest rows every round, and
// the round reports the deferral.
func (n *notificationRead) bounded() bool {
	if n.requests >= n.limit {
		return true
	}
	if n.rd.windowLeft() <= 0 {
		n.deferBacklog()
		return true
	}
	return false
}

func (n *notificationRead) deferBacklog() {
	n.deferred = true
	n.rd.deferWork("notification backlog deferred: polling's share of the rate window is spent; the cursor stays and the newest page is still read")
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
// drops to just past its oldest row, and the next window is half as wide.
// When that whole page is one second, to drops below it and the second is
// read once the windows reach it, so coverage stays contiguous and a
// round that stops inside it can resume there. A one-second window that
// does not fit is read as a second. resume, when set, is where the last
// round stopped inside the second from. It returns from, the end of the
// contiguous coverage from the start, and whether coverage reached to. A
// round spends at most MaxPages plus the search bound on it.
func (n *notificationRead) backlog(ctx context.Context, from, to time.Time, resume int64) (time.Time, bool, error) {
	n.limit = 1 + n.s.maxPages + scanSeekProbes
	n.requests = 1
	if resume > 0 {
		if done, err := n.second(ctx, from, resume); !done || err != nil {
			return from, false, err
		}
		from = from.Add(time.Second)
	}
	// dense are full seconds found at the top of windows, newest first;
	// nothing lies between them.
	var dense []time.Time
	for {
		width := to.Sub(from)
		for from.Before(to) {
			if n.bounded() {
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
				if done, err := n.second(ctx, from, 0); !done || err != nil {
					return from, false, err
				}
				from, width = until, 2*width
				continue
			case until.Equal(to) && len(rows) > 0:
				// Rows of the window newer than its first page's oldest are
				// all on that page.
				oldest := rows[len(rows)-1].UpdatedAt
				to = later(from.Add(time.Second), oldest.Add(time.Second))
				if rows[0].UpdatedAt.Equal(oldest) && oldest.After(from) {
					dense, to = append(dense, oldest), oldest
				}
			}
			width = max(time.Second, (earlier(until, to).Sub(from) / 2).Truncate(time.Second))
		}
		if len(dense) == 0 {
			return from, true, nil
		}
		at := dense[len(dense)-1]
		dense = dense[:len(dense)-1]
		if done, err := n.second(ctx, at, 0); !done || err != nil {
			return from, false, err
		}
		from, to = at.Add(time.Second), at.Add(time.Second)
		if len(dense) > 0 {
			to = dense[len(dense)-1]
		}
	}
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

// second flags every row of one second, which no narrower window splits,
// by page number. An update moves a notification out of a past second and
// shifts the rest back, so no page offset is trusted: the second is a
// ranked walk in descending ID order (GitHub's observed tie order, checked
// at every join), from the row with ID after when it is set. A round that
// stops in it leaves partial, the lowest ID read, for the next round to go
// on from. A second the walk cannot prove is read whole instead, with as
// many requests as polling has left in the rate window; one larger than
// that stalls (see notificationStall). done is false when the round's
// bound or a stall stopped it.
func (n *notificationRead) second(ctx context.Context, at time.Time, after int64) (bool, error) {
	if stall := n.s.stall; stall.at.Equal(at) && n.s.clock.Now().Before(stall.retryAt) {
		n.unverified = stall.err()
		return false, nil
	}
	query := notificationQuery(at, at.Add(time.Second))
	fetch := func(ctx context.Context, page, perPage int) (listing, error) {
		if n.bounded() {
			return listing{}, errRoundBound
		}
		query := maps.Clone(query)
		query.Set("per_page", strconv.Itoa(perPage))
		if page > 1 {
			query.Set("page", strconv.Itoa(page))
		}
		response, err := n.s.get(ctx, n.rd, model.GHRequest{Path: "notifications", Query: query})
		n.requests++
		if err != nil {
			return listing{}, err
		}
		rows, err := decode[[]ghNotification](response, "notifications")
		if err != nil {
			return listing{}, err
		}
		entries := make([]entry, len(rows))
		for i, row := range rows {
			n.flag(row)
			// An ID that does not parse ranks as zero, which the walk
			// reports as out of order.
			entries[i].id, _ = strconv.ParseInt(row.ID, 10, 64)
		}
		return listing{start: (page - 1) * perPage, rows: entries, next: response.NextPath != ""}, nil
	}
	walk := ranked{fetch: fetch, rank: func(row entry) int64 { return -row.id }, perPage: notificationsPage, last: math.MinInt64}
	if after > 0 {
		walk.last, walk.anchored = -after, true
	}
	first := walk.last
	// stop records how far the walk got when something ended it early.
	stop := func(err error) (bool, error) {
		if walk.anchored {
			n.partialAt, n.partial = at, -walk.last
		}
		n.advanced = n.advanced || walk.last > first
		if errors.Is(err, errRoundBound) {
			err = nil
		}
		return false, err
	}
	for !walk.complete {
		if walk.probes >= scanSeekProbes {
			return stop(nil)
		}
		if _, err := walk.next(ctx); err != nil {
			return stop(err)
		}
		if walk.broken {
			return n.secondWhole(ctx, at, fetch)
		}
	}
	n.s.stall.clear(at.Add(time.Second))
	return true, nil
}

// secondWhole reads the second at whole, with every request polling has
// left in the rate window if that is more than the round's bound. A second
// larger than that stalls: retrying it at once would spend as much again.
// With the window's share already spent it is deferred instead.
func (n *notificationRead) secondWhole(ctx context.Context, at time.Time, fetch fetcher) (bool, error) {
	if n.rd.windowLeft() <= 0 {
		n.deferBacklog()
		return false, nil
	}
	n.limit = max(n.limit, n.requests+n.rd.windowLeft())
	done, err := readWhole(ctx, fetch, notificationsPage, n.limit, func([]entry) {})
	switch {
	case errors.Is(err, errRoundBound), err == nil && !done:
		n.s.stall.note(at, n.s.clock.Now(), n.s.interval)
		n.unverified = n.s.stall.err()
		return false, nil
	case err != nil:
		return false, err
	}
	n.s.stall.clear(at.Add(time.Second))
	return true, nil
}

// notificationStall is a notification second that the walk could not
// prove and that held more rows than polling had requests left to read it
// whole. Reading it again soon costs as much and fails the same way, so
// the backlog retries it only after a wait that doubles from the poll
// interval up to the rate window, and reports it every round meanwhile.
// The cursor stays below it, and the head still flags the newest rows.
type notificationStall struct {
	at      time.Time
	wait    time.Duration
	retryAt time.Time
}

func (s *notificationStall) note(at, now time.Time, interval time.Duration) {
	wait := interval
	if s.at.Equal(at) {
		wait = min(2*s.wait, rateWindow)
	}
	*s = notificationStall{at: at, wait: wait, retryAt: now.Add(wait)}
}

// clear ends a stall once coverage reaches covered, past its second: the
// second was read, or every row left it.
func (s *notificationStall) clear(covered time.Time) {
	if !s.at.IsZero() && covered.After(s.at) {
		*s = notificationStall{}
	}
}

func (s *notificationStall) err() error {
	return fmt.Errorf("notifications: the second %s cannot be proven in ID order and holds more rows than polling may read whole in this rate window; the cursor stays below it and the read is retried after %s",
		s.at.Format(time.RFC3339), s.retryAt.Format(time.RFC3339))
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
				item := entry{updated: row.UpdatedAt, id: row.ID}
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
			item := entry{updated: row.UpdatedAt, id: row.ID}
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

// position is a place in an observe stream's updated order: every row
// updated before at, and with id set the rows updated in the second at
// that GitHub lists up to the one with that ID (in ascending ID order).
type position struct {
	at time.Time
	id int64
}

func (p position) compare(o position) int {
	if order := p.at.Compare(o.at); order != 0 {
		return order
	}
	return cmp.Compare(p.id, o.id)
}

// pollStream reads one observe stream in updated order. The first enabled
// round only records EnabledAt, from GitHub's clock: nothing older is
// imported, and local skew cannot move the start.
//
// The cursor is settled coverage: it moves only over rows read at least a
// lookback after their update, so a row indexed late cannot appear below
// it. Rows past it were read already, up to the stream's frontier, which
// the Source keeps in memory only: a restart loses nothing but rereads.
// A round reads MaxPages fresh pages from the frontier, then rereads from
// the cursor only as far as rows have settled, which moves the cursor:
// the reread window is what settled since the cursor, one round's activity
// when the stream keeps up and more after a round that could not reread,
// at most rereadPageFactor*MaxPages pages. A late row past that line is
// read when the cursor reaches it, at most a lookback after its update.
// Observe streams are context, read last and only while the round's
// request allowance lasts, so they lag under load instead of spending the
// budget of notifications and watched threads. Nothing is skipped: a
// reread cut short starts again at the cursor next round.
//
// Each request is the first page from a since, so a deletion cannot shift
// an unread row onto a page already read. Only a second holding more than
// a page is paged by number, as a ranked walk in ascending ID order, and
// the cursor's LastID keeps the progress through it; a second the walk
// cannot prove is read whole. The ETag is kept only while the cursor, and
// so the first request's URL, is unchanged and its reply proved itself the
// whole listing: no next page and shorter than a page, since a full page
// keeps its ETag while a next page appears.
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
	read := &streamRead{s: s, rd: rd, spec: spec, stored: stored, cursor: stored, cover: position{at: stored.Since, id: stored.LastID}}
	read.front = s.frontiers[spec.key]
	if read.front.compare(read.cover) < 0 {
		read.front = read.cover
	}
	query := read.query(read.cover.at)
	first, err := s.get(ctx, rd, model.GHRequest{Path: spec.path, Query: query, ETag: stored.ETag})
	if err != nil {
		return err
	}
	if first.Status == http.StatusNotModified {
		if first.NextPath == "" {
			return nil
		}
		// The page is unchanged but no longer the whole listing.
		if first, err = s.get(ctx, rd, model.GHRequest{Path: spec.path, Query: query}); err != nil {
			return err
		}
	}
	if read.front.compare(read.cover) > 0 {
		err = read.walk(ctx, &streamWalk{r: read, pos: read.front}, false)
	}
	if err == nil {
		err = read.walk(ctx, &streamWalk{r: read, pos: read.cover, first: &first}, true)
	}
	s.frontiers[spec.key] = read.front
	if err != nil {
		return err
	}
	if first.NextPath == "" && !fullPage(first.Body, query) && sameCursor(read.cursor, stored) && first.ETag != stored.ETag {
		// The first reply was the whole listing and its URL is unchanged.
		cursor := read.cursor
		cursor.ETag = first.ETag
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
	// cursor is the committed cursor, cover its position; front is the
	// frontier.
	cursor        model.Cursor
	cover, front  position
	fresh, reread int
}

func (r *streamRead) query(since time.Time) url.Values {
	query := listQuery(since, true)
	for key, values := range r.spec.extra {
		query[key] = values
	}
	return query
}

// walk reads w until the listing ends, the round's page bounds or its
// request allowance: pages past the frontier are fresh and bounded by
// MaxPages, pages below it rereads with their own bound, so rereading from
// the cursor still runs in a round whose fresh pages are spent. A walk
// from the cursor moves the cursor as long as every row it passed had
// settled when read, and then up to where rows settle; below the frontier
// it stops there, since the rows up to the frontier were read already.
func (r *streamRead) walk(ctx context.Context, w *streamWalk, fromCover bool) error {
	settling := fromCover
	for !w.done {
		if w.pos.compare(r.front) < 0 && r.reread >= rereadPageFactor*r.s.maxPages || w.pos.compare(r.front) >= 0 && r.fresh >= r.s.maxPages {
			return nil
		}
		if w.first == nil && r.rd.spare() <= 0 {
			return nil
		}
		w.searched = false
		rows, err := w.next(ctx)
		if err != nil {
			return err
		}
		if w.pos.compare(r.front) > 0 {
			// A step that found the walk's place in a second ends with a
			// part of a page; its requests count against the search bound.
			if !w.searched {
				r.fresh++
			}
			r.front = w.pos
		} else {
			r.reread++
		}
		cover := r.cover
		if settling {
			line := position{at: w.date.Add(-r.s.lookback)}
			if settling = w.pos.compare(line) <= 0; settling {
				cover = w.pos
			} else if line.compare(cover) > 0 {
				cover = line
			}
		}
		if err := r.commit(ctx, rows, cover); err != nil {
			return err
		}
		if fromCover && !settling && w.pos.compare(r.front) < 0 {
			return nil
		}
	}
	return nil
}

// commit stores the events of rows in bounded runs, the last with the
// cursor at cover when that moved it.
func (r *streamRead) commit(ctx context.Context, rows []entry, cover position) error {
	next := r.cursor
	if cover.compare(r.cover) > 0 {
		next.Since, next.LastID, next.ETag = cover.at, cover.id, ""
	}
	runs := split(rows)
	if len(runs) == 0 {
		runs = [][]entry{nil}
	}
	for i, run := range runs {
		batch := model.IntakeBatch{Resource: string(r.spec.key), Events: eventsOf(run)}
		if i == len(runs)-1 && !sameCursor(next, r.cursor) {
			batch.Cursor = &next
		}
		if len(batch.Events) == 0 && batch.Cursor == nil {
			continue
		}
		if err := r.s.ingest(ctx, r.rd, batch); err != nil {
			return err
		}
	}
	r.cursor, r.cover = next, furthest(r.cover, cover)
	return nil
}

// furthest returns the later of two positions.
func furthest(a, b position) position {
	if b.compare(a) > 0 {
		return b
	}
	return a
}

// streamWalk reads an observe stream in updated order from a position, a
// step at a time.
type streamWalk struct {
	r   *streamRead
	pos position
	// first is a reply already read for the first page from pos.at.
	first *model.GHResponse
	// bucket walks the second pos.at by ID when it holds more than a page;
	// tail is what its newest reply listed past that second.
	bucket   *ranked
	tail     []entry
	tailNext bool
	// searched reports that the last step searched for its place.
	searched bool
	// date is the Date of the newest reply.
	date time.Time
	done bool
}

// next reads one step and returns its rows; pos is then where the rows
// read reach.
func (w *streamWalk) next(ctx context.Context) ([]entry, error) {
	if w.bucket != nil {
		return w.bucketNext(ctx)
	}
	response, rows, err := w.page(ctx, 1, listPageSize)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		w.done = true
		return nil, nil
	}
	last := rows[len(rows)-1].updated
	if response.NextPath == "" {
		w.done = true
		w.pos = furthest(w.pos, position{at: last})
		return rows, nil
	}
	if !slices.ContainsFunc(rows, func(row entry) bool { return !row.updated.Equal(w.pos.at) }) {
		// One second holds more than a page: walk it by ID.
		w.first = &response
		w.bucket = &ranked{fetch: w.bucketPage, rank: entryID, perPage: listPageSize, last: w.pos.id, anchored: w.pos.id > 0}
		return w.bucketNext(ctx)
	}
	if !last.After(w.pos.at) {
		return nil, fmt.Errorf("%s: listing out of updated order at %s", w.r.spec.path, w.pos.at.Format(time.RFC3339))
	}
	w.pos = position{at: last}
	return rows, nil
}

// bucketNext reads one step of the second pos.at.
func (w *streamWalk) bucketNext(ctx context.Context) ([]entry, error) {
	bucket, at := w.bucket, w.pos.at.Format(time.RFC3339)
	if bucket.probes >= scanSeekProbes {
		return nil, fmt.Errorf("%s: lost its place among the rows updated in the second %s; the cursor stays", w.r.spec.path, at)
	}
	w.searched = !bucket.located
	fresh, err := bucket.next(ctx)
	if err != nil {
		return nil, err
	}
	if bucket.broken {
		return w.bucketWhole(ctx)
	}
	w.pos.id = bucket.last
	if !bucket.complete {
		return fresh, nil
	}
	// The second ended in this reply; what follows it there is contiguous.
	w.bucket = nil
	if len(w.tail) > 0 {
		w.pos = position{at: w.tail[len(w.tail)-1].updated}
	}
	w.done = !w.tailNext
	return append(fresh, w.tail...), nil
}

// bucketWhole reads the second pos.at whole when the walk cannot prove it
// in ID order from the row read last, within the requests a scan reads
// whole with. With every row of the second read, the position moves past
// it.
func (w *streamWalk) bucketWhole(ctx context.Context) ([]entry, error) {
	var rows []entry
	done, err := readWhole(ctx, w.bucketPage, listPageSize, w.r.s.maxPages+scanSeekProbes, func(fresh []entry) { rows = append(rows, fresh...) })
	if err != nil {
		return nil, err
	}
	if !done {
		return nil, fmt.Errorf("%s: the rows updated in the second %s cannot be proven in ID order and outnumber what a round reads whole; the cursor stays", w.r.spec.path, w.pos.at.Format(time.RFC3339))
	}
	w.bucket = nil
	w.pos = position{at: w.pos.at.Add(time.Second)}
	if len(w.tail) > 0 {
		w.pos = position{at: w.tail[len(w.tail)-1].updated}
	}
	w.done = !w.tailNext
	return append(rows, w.tail...), nil
}

// bucketPage lists one page from pos.at and returns its rows of that
// second; a row past it ends the second.
func (w *streamWalk) bucketPage(ctx context.Context, page, perPage int) (listing, error) {
	response, rows, err := w.page(ctx, page, perPage)
	if err != nil {
		return listing{}, err
	}
	cut := 0
	for cut < len(rows) && rows[cut].updated.Equal(w.pos.at) {
		cut++
	}
	w.tail, w.tailNext = rows[cut:], response.NextPath != ""
	return listing{start: (page - 1) * perPage, rows: rows[:cut], next: w.tailNext && cut == len(rows)}, nil
}

// page lists one page of rows updated at or after pos.at.
func (w *streamWalk) page(ctx context.Context, page, perPage int) (model.GHResponse, []entry, error) {
	var response model.GHResponse
	if w.first != nil && page == 1 && perPage == listPageSize {
		response, w.first = *w.first, nil
	} else {
		query := w.r.query(w.pos.at)
		query.Set("per_page", strconv.Itoa(perPage))
		if page > 1 {
			query.Set("page", strconv.Itoa(page))
		}
		var err error
		if response, err = w.r.s.get(ctx, w.r.rd, model.GHRequest{Path: w.r.spec.path, Query: query}); err != nil {
			return response, nil, err
		}
	}
	date, err := serverDate(response)
	if err != nil {
		return response, nil, err
	}
	w.date = date
	rows, err := w.r.spec.entries(response.Body, w.r.stored.EnabledAt, w.r.s.clock.Now())
	return response, rows, err
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
