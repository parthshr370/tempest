package source

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
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
// The source therefore reads each stream as a scan. A scan lists the rows
// of one window in ID order and proves its own coverage: every request
// after the first overlaps the highest ID read so far (a reply whose first
// row is at or below it continues the listing without a gap, because the
// listing is in ID order and one reply is one snapshot). A reply that
// starts above it means rows moved back; the scan steps back instead of
// trusting the offset. Page sizes vary so that consecutive requests
// overlap at all.
//
// Position records the scan:
//
//   - LastID is the highest ID the unfinished scan has read; zero means no
//     scan is under way.
//   - Since is GitHub's Date when that scan started, or for a finished
//     scan one second past it. A new scan of a windowed stream lists rows
//     updated at or after Since minus the lookback, so everything written,
//     edited or late-indexed since the last scan began is in it.
//
// An unfinished scan resumes next round over the unfiltered listing (its
// rows above LastID are a superset of what remained), found by a doubling
// then halving search for LastID. It finishes in a later round, so
// MaxPages bounds the pages per round without starving a dense or equal
// timestamp stream. Positions compare as (Since, LastID).

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

// scanSeekProbes bounds requests that read no new row in one round: the
// resume search (two per doubling of the listing's page count) and the
// steps back after rows moved.
const scanSeekProbes = 64

// listing is one reply of a scan: rows from offset start, in ID order.
type listing struct {
	start int
	rows  []entry
	next  bool
}

type scanner struct {
	s    *Source
	rd   *round
	spec scanSpec
	// query lists the scanned window, without page parameters.
	query url.Values
	// started is the Date of the scan's first reply; zero until it is read
	// in the round that started the scan.
	started time.Time
	// last is the highest ID read by this scan; end is the offset just
	// past it in the newest reply that proved it.
	last int64
	end  int
	// active reports a listed row updated at or after the old position.
	active   bool
	complete bool
	pages    int
	probes   int
	// held is the newest run, committed with the position at the end.
	held []entry
}

// scan reads one thread stream from its position and commits what it read
// and the new position. It reports whether the scan finished this round.
func (s *Source) scan(ctx context.Context, rd *round, spec scanSpec) (bool, error) {
	sc := &scanner{s: s, rd: rd, spec: spec, query: url.Values{}, last: spec.old.LastID}
	err := sc.run(ctx)
	if commitErr := sc.commit(ctx); err == nil {
		err = commitErr
	}
	return sc.complete && err == nil, err
}

func (sc *scanner) run(ctx context.Context) error {
	if sc.last == 0 {
		// A new scan: the window since the last scan began, minus the
		// lookback, or the whole stream when it was never read.
		if sc.spec.windowed && !sc.spec.old.Since.IsZero() {
			sc.query.Set("since", sc.spec.old.Since.Add(-sc.s.lookback).UTC().Format(time.RFC3339))
		}
		got, err := sc.fetch(ctx, 1, listPageSize)
		if err != nil {
			return err
		}
		sc.accept(got)
	} else if err := sc.seek(ctx); err != nil {
		return err
	}
	for !sc.complete && sc.pages < sc.s.maxPages && sc.probes < scanSeekProbes {
		page, perPage := overlapWindow(sc.end)
		got, err := sc.fetch(ctx, page, perPage)
		if err != nil {
			return err
		}
		if got.start > 0 && (len(got.rows) == 0 || got.rows[0].id > sc.last) {
			// Rows moved back past the reply's start: everything read so far
			// now lies before it. Step back rather than skip what moved.
			sc.end = got.start
			sc.probes++
			continue
		}
		sc.accept(got)
	}
	return nil
}

// seek finds where an unfinished scan stopped: pages double until one
// reaches past LastID, then halve between the last page wholly at or
// below it and the first page wholly above it.
func (sc *scanner) seek(ctx context.Context) error {
	low, high := 0, 0
	for page := 1; sc.probes < scanSeekProbes; sc.probes++ {
		got, err := sc.fetch(ctx, page, listPageSize)
		if err != nil {
			return err
		}
		switch {
		case page == 1 || len(got.rows) > 0 && got.rows[0].id <= sc.last:
			// The reply continues the listing from at or below LastID.
			if len(got.rows) == 0 || got.rows[len(got.rows)-1].id > sc.last || !got.next {
				sc.accept(got)
				return nil
			}
			low = page
		default:
			high = page
		}
		switch {
		case high == 0:
			page *= 2
		case high-low > 1:
			page = (low + high) / 2
		default:
			// LastID is at the end of page low: continue from there.
			sc.end = low * listPageSize
			return nil
		}
	}
	return nil
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

// accept takes a reply that continues the listing: rows above last are
// new to this scan.
func (sc *scanner) accept(got listing) {
	var fresh []entry
	for _, row := range got.rows {
		if row.id > sc.last {
			fresh = append(fresh, row)
		}
	}
	for _, row := range got.rows {
		sc.last = max(sc.last, row.id)
	}
	sc.end = got.start + len(got.rows)
	sc.complete = !got.next
	if len(fresh) == 0 {
		if !sc.complete {
			sc.probes++
		}
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
// finished keeps its position, and so its request URL.
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
	case !sc.complete && sc.last > old.LastID && !started.IsZero():
		position = &model.Position{Since: started, LastID: sc.last}
	}
	if len(sc.held) == 0 && position == nil && sc.spec.pr.pending() == nil {
		return nil
	}
	return sc.ingest(ctx, sc.held, position)
}

func (sc *scanner) ingest(ctx context.Context, run []entry, position *model.Position) error {
	batch := model.IntakeBatch{Resource: sc.spec.resource, Events: eventsOf(run), PRs: sc.spec.pr.pending()}
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

// overlapWindow picks the page and page size whose reply contains offset
// end-1, the last row already read, and reaches furthest past it. One page
// size alone cannot overlap: its pages tile the listing.
func overlapWindow(end int) (page, perPage int) {
	if end <= 0 {
		return 1, listPageSize
	}
	gain := -1
	for size := listPageSize; size > 0; size-- {
		if reach := size - 1 - (end-1)%size; reach > gain {
			gain, page, perPage = reach, (end-1)/size+1, size
		}
	}
	return page, perPage
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
