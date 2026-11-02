package source

import (
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
)

// entry is one listed GitHub item: the events it yields (none for the
// account's own items, two for a comment first seen edited) and the read
// position that is safe once it and everything before it is committed.
type entry struct {
	events []model.Event
	// mark is the stream's safe Since through this item.
	mark time.Time
	// updated is the item's updated_at, for the end-of-walk position.
	updated time.Time
	// id is the item's GitHub ID, for streams positioned by ID.
	id int64
}

func bodyBytes(events []model.Event) int {
	total := 0
	for _, event := range events {
		total += len(event.Body)
	}
	return total
}

// split cuts entries into runs that each fit one intake batch: at most
// model.MaxBatchEvents events and model.MaxBatchBodyBytes of bodies. An
// entry is never divided, and one that alone exceeds the body bound
// travels by itself. Positions are taken from a run's last entry, which is
// safe because a reread deduplicates by EventID.
func split(entries []entry) [][]entry {
	var runs [][]entry
	start, events, bytes := 0, 0, 0
	for i, item := range entries {
		count, size := len(item.events), bodyBytes(item.events)
		if i > start && (events+count > model.MaxBatchEvents || bytes+size > model.MaxBatchBodyBytes) {
			runs = append(runs, entries[start:i])
			start, events, bytes = i, 0, 0
		}
		events += count
		bytes += size
	}
	if start < len(entries) {
		runs = append(runs, entries[start:])
	}
	return runs
}

func eventsOf(entries []entry) []model.Event {
	var events []model.Event
	for _, item := range entries {
		events = append(events, item.events...)
	}
	return events
}

// later returns the later of two times; positions only move forward.
func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
