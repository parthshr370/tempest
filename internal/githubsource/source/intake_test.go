package source

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
)

// memIntake is an in-memory model.Intake with the control journal's source
// contract: known EventIDs are skipped, positions and cursors only move
// forward (a backward move fails the test), a stale notification flag is a
// no-op and a complete read clears the flag only if no newer notification
// arrived. Every applied batch is recorded.
type memIntake struct {
	t  *testing.T
	mu sync.Mutex

	cursors map[model.CursorKey]model.Cursor
	threads map[int]*model.Thread
	prs     map[int]model.PullRequest
	events  map[model.EventID]model.Event
	order   []model.EventID
	batches []model.IntakeBatch
	// changes counts Ingest calls that changed any stored state.
	changes int

	// subscribed threads are watched as if an open Ticket followed them.
	subscribed map[int]bool
	// hideFlagged keeps flagged-only threads out of Watched, to test the
	// notification step on its own.
	hideFlagged bool
	// fail, when set, rejects a batch before anything is applied.
	fail func(model.IntakeBatch) error
}

var _ model.Intake = (*memIntake)(nil)

func newMemIntake(t *testing.T) *memIntake {
	return &memIntake{
		t: t, cursors: map[model.CursorKey]model.Cursor{}, threads: map[int]*model.Thread{},
		prs: map[int]model.PullRequest{}, events: map[model.EventID]model.Event{}, subscribed: map[int]bool{},
	}
}

func (m *memIntake) subscribe(numbers ...int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, number := range numbers {
		m.subscribed[number] = true
		if _, ok := m.threads[number]; !ok {
			m.threads[number] = &model.Thread{Ref: model.ThreadRef{Number: number}}
		}
	}
}

func (m *memIntake) Cursor(_ context.Context, key model.CursorKey) (model.Cursor, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cursor, ok := m.cursors[key]
	return cursor, ok, nil
}

func (m *memIntake) Watched(_ context.Context, _ time.Time) ([]model.WatchedThread, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []model.WatchedThread
	for number, thread := range m.threads {
		if thread.State == stateGone || !(m.subscribed[number] || (thread.NeedsReconcile && !m.hideFlagged)) {
			continue
		}
		item := model.WatchedThread{Thread: *thread}
		if pr, ok := m.prs[number]; ok {
			item.PR = &pr
		}
		out = append(out, item)
	}
	slices.SortFunc(out, func(a, b model.WatchedThread) int { return a.Thread.Ref.Number - b.Thread.Ref.Number })
	return out, nil
}

func (m *memIntake) Ingest(_ context.Context, batch model.IntakeBatch) (model.IngestResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		if err := m.fail(batch); err != nil {
			return model.IngestResult{}, err
		}
	}
	if len(batch.Events) > model.MaxBatchEvents || bodyBytes(batch.Events) > model.MaxBatchBodyBytes && len(batch.Events) > 1 {
		m.t.Errorf("batch %s over limits: %d events, %d body bytes", batch.Resource, len(batch.Events), bodyBytes(batch.Events))
	}
	m.batches = append(m.batches, cloneBatch(m.t, batch))
	changed := false
	result := model.IngestResult{}
	for _, event := range batch.Events {
		if _, known := m.events[event.ID]; known {
			continue
		}
		m.events[event.ID] = event
		m.order = append(m.order, event.ID)
		result.NewEvents++
		changed = true
	}
	if batch.Cursor != nil {
		old := m.cursors[batch.Cursor.Key]
		if batch.Cursor.Since.Before(old.Since) {
			m.t.Errorf("cursor %s moved backward: %s -> %s", batch.Cursor.Key, old.Since, batch.Cursor.Since)
		}
		if !sameCursor(old, *batch.Cursor) {
			m.cursors[batch.Cursor.Key] = *batch.Cursor
			changed = true
		}
	}
	for _, update := range batch.Threads {
		changed = m.applyThread(update, batch.ReadAt) || changed
	}
	for _, pr := range batch.PRs {
		old, ok := m.prs[pr.Number]
		if ok && pr.UpdatedAt.Before(old.UpdatedAt) {
			continue
		}
		if !ok || string(mustJSON(old)) != string(mustJSON(pr)) {
			m.prs[pr.Number] = pr
			changed = true
		}
	}
	if changed {
		m.changes++
	}
	return result, nil
}

func (m *memIntake) applyThread(update model.ThreadUpdate, readAt time.Time) bool {
	thread, ok := m.threads[update.Ref.Number]
	if !ok {
		thread = &model.Thread{Ref: update.Ref}
		m.threads[update.Ref.Number] = thread
	}
	before, err := json.Marshal(thread)
	if err != nil {
		m.t.Fatal(err)
	}
	thread.Ref = update.Ref
	if update.Title != "" {
		thread.Title = update.Title
	}
	if update.State != "" {
		thread.State = update.State
	}
	if update.Author != nil {
		thread.Author = *update.Author
	}
	if !update.LastActivityAt.IsZero() {
		thread.LastActivityAt = update.LastActivityAt
	}
	for _, position := range []struct {
		name   string
		update *model.Position
		stored *model.Position
	}{{"comments", update.Comments, &thread.Comments}, {"reviewComments", update.ReviewComments, &thread.ReviewComments}, {"reviews", update.Reviews, &thread.Reviews}} {
		if position.update == nil {
			continue
		}
		if position.update.Since.Before(position.stored.Since) || position.update.LastID < position.stored.LastID {
			m.t.Errorf("thread %d %s moved backward: %+v -> %+v", update.Ref.Number, position.name, *position.stored, *position.update)
		}
		*position.stored = *position.update
	}
	if update.Reconcile != nil {
		switch {
		case *update.Reconcile && (update.NotifiedAt.IsZero() || update.NotifiedAt.After(thread.NotifiedAt)):
			thread.NeedsReconcile, thread.NotifiedAt = true, update.NotifiedAt
		case !*update.Reconcile && !thread.NotifiedAt.After(readAt):
			thread.NeedsReconcile = false
		}
	}
	after, err := json.Marshal(thread)
	if err != nil {
		m.t.Fatal(err)
	}
	return string(before) != string(after)
}

func cloneBatch(t *testing.T, batch model.IntakeBatch) model.IntakeBatch {
	raw, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	var clone model.IntakeBatch
	if err := json.Unmarshal(raw, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

// snapshot helpers for assertions.

func (m *memIntake) thread(number int) model.Thread {
	m.mu.Lock()
	defer m.mu.Unlock()
	thread, ok := m.threads[number]
	if !ok {
		m.t.Fatalf("thread %d never stored", number)
	}
	return *thread
}

func (m *memIntake) pr(number int) model.PullRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.prs[number]
}

func (m *memIntake) changeCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.changes
}

// eventsOfKind returns stored events of kind in arrival order.
func (m *memIntake) eventsOfKind(kind model.EventKind) []model.Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []model.Event
	for _, id := range m.order {
		if event := m.events[id]; event.Kind == kind {
			out = append(out, event)
		}
	}
	return out
}

func (m *memIntake) hasEvent(id model.EventID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.events[id]
	return ok
}

func (m *memIntake) eventCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.order)
}

func (m *memIntake) recorded() []model.IntakeBatch {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.batches)
}
