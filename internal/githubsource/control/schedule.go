package control

import (
	"sort"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
)

// candidate is a queued dispatch eligible for a slot.
type candidate struct {
	Dispatch model.DispatchID
	Class    model.SlotClass
	// HasSession is true for a continuation of an existing worker; only
	// those may use the human slot.
	HasSession bool
	QueuedAt   time.Time
}

// placement is a candidate given a slot.
type placement struct {
	Dispatch model.DispatchID
	Slot     model.SlotClass
}

// humanSlots is the one extra slot reserved for human continuations.
const humanSlots = 1

// orderCandidates sorts human class first, then by queue time, then by ID.
func orderCandidates(candidates []candidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if (a.Class == model.SlotHuman) != (b.Class == model.SlotHuman) {
			return a.Class == model.SlotHuman
		}
		if !a.QueuedAt.Equal(b.QueuedAt) {
			return a.QueuedAt.Before(b.QueuedAt)
		}
		return a.Dispatch < b.Dispatch
	})
}

// slotFor picks the slot a candidate may occupy now: a free normal slot,
// else the human slot for a human-class continuation, else none (it waits;
// nothing is preempted).
func slotFor(c candidate, occupancy model.Occupancy) (model.SlotClass, bool) {
	if occupancy.Normal < occupancy.MaxNormal {
		return model.SlotNormal, true
	}
	if c.Class == model.SlotHuman && c.HasSession && occupancy.Human < occupancy.MaxHuman {
		return model.SlotHuman, true
	}
	return "", false
}

// schedule places candidates in order. reserve is asked only for a
// candidate that has a slot; when it refuses (no budget) the slot stays
// free for the next candidate.
func schedule(candidates []candidate, occupancy model.Occupancy, reserve func(candidate) bool) []placement {
	orderCandidates(candidates)
	var placements []placement
	for _, c := range candidates {
		slot, ok := slotFor(c, occupancy)
		if !ok || !reserve(c) {
			continue
		}
		if slot == model.SlotHuman {
			occupancy.Human++
		} else {
			occupancy.Normal++
		}
		placements = append(placements, placement{Dispatch: c.Dispatch, Slot: slot})
	}
	return placements
}
