package source

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
)

// A thread stream (conversation comments, inline comments, reviews) is an
// ID-ordered listing paginated by page number, optionally filtered by
// since on updated_at. GitHub offers no seek past an ID, and a deletion
// moves every later row one offset back, so a page number never proves
// what the previous page left unread.
//
// The source therefore reads each stream as a scan, a ranked walk (below)
// with the row ID as rank, and commits what it read with its position:
//
//   - LastID is the highest ID the unfinished scan has read; zero means no
//     scan is under way.
//   - Since is GitHub's Date when that scan started, or for a finished
//     scan one second past it. A new scan of a windowed stream lists rows
//     updated at or after Since minus the lookback, so everything written,
//     edited or late-indexed since the last scan began is in it.
//
// An unfinished scan resumes next round over the unfiltered listing (its
// rows above LastID are a superset of what remained). It finishes in a
// later round, so MaxPages bounds the pages per round without starving a
// dense or equal timestamp stream. Positions compare as (Since, LastID).
//
// GitHub documents ascending ID order for conversation and unsorted inline
// comments, but lists reviews only "in chronological order". A review
// listing the walk cannot prove in ID order is read whole instead.

// scanSpec describes one thread stream.
type scanSpec struct {
	resource string
	path     string
	// windowed streams take since; reviews list everything.
	windowed bool
	old      model.Position
	entries  func(raw []byte, observedAt time.Time) ([]entry, error)
	position func(batch *model.IntakeBatch, position model.Position)
	// pr, when set, rides along in every batch that changes it.
	pr *prTracker
}

// scanSeekProbes bounds requests that read no new row in one ranked walk:
// the search for the row it resumes after (two per doubling of the
// listing's page count) and the steps back after rows moved.
const scanSeekProbes = 64

// listing is one reply of a paged listing: rows from offset start.
type listing struct {
	start int
	rows  []entry
	next  bool
}

// fetcher reads one page of a listing.
type fetcher func(ctx context.Context, page, perPage int) (listing, error)

// ranked walks a listing that GitHub pages by number and sorts by a rank
// unique to each row (an ID, or a negated ID for a descending listing),
// from just past a row already read. Its coverage rests on one invariant,
// checked at every join: a reply continues the walk only when it lists the
// row the walk read last, and the walk takes only the rows after that row
// in that reply, whose ranks must strictly ascend from it. Rows only leave
// these listings or join at their end, so every row before that one was
// read already, and one reply is one snapshot, so no row lies unread
// between it and the rows after it. Page sizes vary so that consecutive
// requests overlap at all.
//
// A reply without that row proves nothing. When all its rows rank above
// the row, rows moved back and the walk steps back to look for it;
// otherwise the row left the listing or the listing is not in rank order,
// and the walk is broken. A walk that resumes from a row an earlier round
// read first searches for the page holding it, doubling then halving by
// rank. The search only decides where to look: nothing counts as read
// until a reply lists that row, and a search that does not find it breaks
// the walk. A broken walk proves nothing past its last join; its reader
// falls back to reading the listing whole.
type ranked struct {
	fetch fetcher
	rank  func(entry) int64
	// perPage is the route's largest page.
	perPage int
	// last is the rank of the row read last, which anchored reports. An
	// unanchored walk starts at the listing's first row. end is the offset
	// just past last in the newest reply that listed it, once located.
	last     int64
	anchored bool
	end      int
	located  bool
	// probes counts requests that read nothing new.
	probes   int
	complete bool
	// broken reports a reply out of rank order or one that does not list
	// the row read last where it must.
	broken bool
}

// next reads one step of the walk, its search for its place counting as
// one, and returns the rows it proved new.
func (w *ranked) next(ctx context.Context) ([]entry, error) {
	if !w.located {
		if w.anchored {
			return w.locate(ctx)
		}
		w.located = true
	}
	page, perPage := overlapWindow(w.end, w.perPage)
	got, err := w.get(ctx, page, perPage)
	if err != nil || w.broken {
		return nil, err
	}
	if !w.anchored {
		return w.accept(got, 0), nil
	}
	if at := w.find(got); at >= 0 {
		return w.accept(got, at+1), nil
	}
	if got.start > 0 && (len(got.rows) == 0 || w.rank(got.rows[0]) > w.last) {
		// Rows moved back past the reply's start: everything read so far
		// now lies before it. Step back rather than skip what moved.
		w.end = got.start
		w.probes++
		return nil, nil
	}
	w.broken = true
	return nil, nil
}

// locate searches for the page that lists the row read last: pages double
// until one reaches past it, then halve between the last page wholly below
// it and the first page wholly above it. A page that spans its rank
// without it, or two adjacent pages that bracket it, mean it is not listed
// where the order puts it: the walk is broken.
func (w *ranked) locate(ctx context.Context) ([]entry, error) {
	low, high := 0, 0
	for page := 1; w.probes < scanSeekProbes; w.probes++ {
		got, err := w.get(ctx, page, w.perPage)
		if err != nil || w.broken {
			return nil, err
		}
		if at := w.find(got); at >= 0 {
			w.located = true
			return w.accept(got, at+1), nil
		}
		switch {
		case page > 1 && (len(got.rows) == 0 || w.rank(got.rows[0]) > w.last):
			high = page
		case got.next && len(got.rows) > 0 && w.rank(got.rows[len(got.rows)-1]) < w.last:
			low = page
		default:
			w.broken = true
			return nil, nil
		}
		switch {
		case high == 0:
			page *= 2
		case high-low > 1:
			page = (low + high) / 2
		default:
			w.broken = true
			return nil, nil
		}
	}
	return nil, nil
}

// get fetches one page and checks its sort.
func (w *ranked) get(ctx context.Context, page, perPage int) (listing, error) {
	got, err := w.fetch(ctx, page, perPage)
	if err != nil {
		return got, err
	}
	for i := 1; i < len(got.rows); i++ {
		if w.rank(got.rows[i]) <= w.rank(got.rows[i-1]) {
			w.broken = true
		}
	}
	return got, nil
}

// find returns the index in got of the row read last, or -1.
func (w *ranked) find(got listing) int {
	return slices.IndexFunc(got.rows, func(row entry) bool { return w.rank(row) == w.last })
}

// accept takes the rows of a reply that continues the walk from index
// from on: the rows after the row read last, or a first page whole.
func (w *ranked) accept(got listing, from int) []entry {
	fresh := got.rows[from:len(got.rows):len(got.rows)]
	if len(fresh) > 0 {
		w.last, w.anchored = w.rank(fresh[len(fresh)-1]), true
	}
	w.end = got.start + len(got.rows)
	w.complete = !got.next
	if len(fresh) == 0 && !w.complete {
		w.probes++
	}
	return fresh
}

// readWhole reads a listing that a ranked walk could not prove from its
// first page, passing every row to take, within budget requests. Each
// reply after the first overlaps the last row of the one before at its
// offset, and that row's ID still being there proves that no row before it
// left in between, so nothing moved onto a page already read; a reply that
// moved starts the listing again. Rows only leave the listings this reads
// (a review, notification or comment updated out of a second, or deleted);
// none enter below their end. It reports whether it reached the end.
func readWhole(ctx context.Context, fetch fetcher, perPage, budget int, take func([]entry)) (bool, error) {
	end, mark := 0, int64(0)
	for range budget {
		page, size := overlapWindow(end, perPage)
		got, err := fetch(ctx, page, size)
		if err != nil {
			return false, err
		}
		skip := 0
		if end > 0 {
			skip = end - got.start
			if skip > len(got.rows) || got.rows[skip-1].id != mark {
				end = 0
				continue
			}
		}
		take(got.rows[skip:])
		if len(got.rows) > 0 {
			end, mark = got.start+len(got.rows), got.rows[len(got.rows)-1].id
		}
		if !got.next {
			return true, nil
		}
	}
	return false, nil
}

func entryID(row entry) int64 { return row.id }

type scanner struct {
	s    *Source
	rd   *round
	spec scanSpec
	// query lists the scanned window, without page parameters.
	query url.Values
	// started is the Date of the scan's first reply; zero until it is read
	// in the round that started the scan.
	started time.Time
	walk    ranked
	// active reports a listed row updated at or after the old position.
	active   bool
	complete bool
	// unproven is set once the walk could not prove the listing in ID order
	// from the row read last, and the listing was read whole.
	unproven bool
	pages    int
	// held is the newest run, committed with the position at the end.
	held []entry
}

// scan reads one thread stream from its position and commits what it read
// and the new position. It reports whether the scan finished this round.
func (s *Source) scan(ctx context.Context, rd *round, spec scanSpec) (bool, error) {
	sc := &scanner{s: s, rd: rd, spec: spec, query: url.Values{}}
	sc.walk = ranked{fetch: sc.fetch, rank: entryID, perPage: listPageSize, last: spec.old.LastID, anchored: spec.old.LastID > 0}
	err := sc.run(ctx)
	if commitErr := sc.commit(ctx); err == nil {
		err = commitErr
	}
	return sc.complete && err == nil, err
}

func (sc *scanner) run(ctx context.Context) error {
	if sc.spec.old.LastID == 0 && sc.spec.windowed && !sc.spec.old.Since.IsZero() {
		// A new scan: the window since the last scan began, minus the
		// lookback, or the whole stream when it was never read.
		sc.query.Set("since", sc.spec.old.Since.Add(-sc.s.lookback).UTC().Format(time.RFC3339))
	}
	walk := &sc.walk
	for !walk.complete && sc.pages < sc.s.maxPages && walk.probes < scanSeekProbes {
		fresh, err := walk.next(ctx)
		if err != nil {
			return err
		}
		if walk.broken {
			return sc.readWhole(ctx)
		}
		sc.take(fresh)
	}
	sc.complete = walk.complete
	return nil
}

// readWhole reads a listing the walk could not prove in full, so a lower
// ID listed after a higher one, or after the row a resumed scan stopped
// at, is not skipped. Only a read that reaches the end moves the position;
// a longer listing keeps it and fails the stream, so the round reports it
// instead of skipping.
func (sc *scanner) readWhole(ctx context.Context) error {
	sc.unproven = true
	complete, err := readWhole(ctx, sc.fetch, listPageSize, sc.s.maxPages+scanSeekProbes, sc.take)
	if err != nil {
		return err
	}
	if sc.complete = complete; !complete {
		return fmt.Errorf("%s cannot be proven in ID order and is longer than a round reads whole; its position stays", sc.spec.path)
	}
	return nil
}

// take records rows new to the scan.
func (sc *scanner) take(fresh []entry) {
	if len(fresh) == 0 {
		return
	}
	sc.pages++
	for _, row := range fresh {
		sc.active = sc.active || !row.updated.Before(sc.spec.old.Since)
		if row.apply != nil {
			row.apply()
		}
	}
	sc.held = append(sc.held, fresh...)
}

// fetch reads one page of the scanned window.
func (sc *scanner) fetch(ctx context.Context, page, perPage int) (listing, error) {
	query := maps.Clone(sc.query)
	query.Set("per_page", strconv.Itoa(perPage))
	if page > 1 {
		query.Set("page", strconv.Itoa(page))
	}
	response, err := sc.s.get(ctx, sc.rd, model.GHRequest{Path: sc.spec.path, Query: query})
	if err != nil {
		return listing{}, err
	}
	if sc.started.IsZero() && sc.spec.old.LastID == 0 {
		date, err := serverDate(response)
		if err != nil {
			return listing{}, err
		}
		sc.started = date
	}
	rows, err := sc.spec.entries(response.Body, sc.s.clock.Now())
	if err != nil {
		return listing{}, err
	}
	return listing{start: (page - 1) * perPage, rows: rows, next: response.NextPath != ""}, nil
}

// flush commits every held run but the newest.
func (sc *scanner) flush(ctx context.Context) error {
	runs := split(sc.held)
	for len(runs) > 1 {
		if err := sc.ingest(ctx, runs[0], nil); err != nil {
			return err
		}
		runs = runs[1:]
	}
	if len(runs) == 1 {
		sc.held = runs[0]
	}
	return nil
}

// commit writes what the scan read and, with the newest run, its new
// position: one second past its start when it finished and there is
// something to move (a resumed scan, or new activity in a window), its
// start and LastID when it stopped short. A scan that read nothing new and
// finished keeps its position, and so its request URL. A listing read
// whole has no LastID worth keeping: unfinished, it keeps its position.
func (sc *scanner) commit(ctx context.Context) error {
	if err := sc.flush(ctx); err != nil {
		return err
	}
	old := sc.spec.old
	started := old.Since
	if old.LastID == 0 {
		started = later(old.Since, sc.started)
	}
	var position *model.Position
	switch {
	case sc.complete && (old.LastID > 0 || sc.spec.windowed && sc.active):
		position = &model.Position{Since: started.Add(time.Second)}
	case !sc.complete && !sc.unproven && sc.walk.last > old.LastID && !started.IsZero():
		position = &model.Position{Since: started, LastID: sc.walk.last}
	}
	if len(sc.held) == 0 && position == nil && sc.spec.pr.pending() == nil {
		return nil
	}
	return sc.ingest(ctx, sc.held, position)
}

func (sc *scanner) ingest(ctx context.Context, run []entry, position *model.Position) error {
	batch := model.IntakeBatch{Resource: sc.spec.resource, Events: eventsOf(run)}
	sc.spec.pr.attach(&batch)
	if position != nil {
		sc.spec.position(&batch, *position)
	}
	if len(batch.Events) == 0 && len(batch.PRs) == 0 && position == nil {
		return nil
	}
	if err := sc.s.ingest(ctx, sc.rd, batch); err != nil {
		return err
	}
	sc.spec.pr.markCommitted()
	sc.held = nil
	return nil
}

// overlapWindow picks the page and page size, at most perPage, whose
// reply contains offset end-1, the last row already read, and reaches
// furthest past it. One page size alone cannot overlap: its pages tile
// the listing.
func overlapWindow(end, perPage int) (page, size int) {
	if end <= 0 {
		return 1, perPage
	}
	gain := -1
	for candidate := perPage; candidate > 0; candidate-- {
		if reach := candidate - 1 - (end-1)%candidate; reach > gain {
			gain, page, size = reach, (end-1)/candidate+1, candidate
		}
	}
	return page, size
}

// errNoDate is a reply without GitHub's Date header, which positions are
// taken from.
var errNoDate = errors.New("reply has no Date header")

// serverDate is GitHub's clock at a reply. Read positions are taken from
// it, never from the local clock, so local skew cannot exclude activity.
func serverDate(response model.GHResponse) (time.Time, error) {
	text := response.Header.Get("Date")
	if text == "" {
		return time.Time{}, errNoDate
	}
	date, err := http.ParseTime(text)
	if err != nil {
		return time.Time{}, fmt.Errorf("reply Date %q: %w", text, err)
	}
	return date.UTC(), nil
}
