package control

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"go.harness.dev/harness/internal/githubsource/model"
	"go.harness.dev/harness/tempest/durable"
)

// staged is one record a transaction writes, already encoded. The decoded
// committed value is rebuilt from these bytes, so the in-memory projection
// after a commit is exactly what a reload from the journal produces.
type staged struct {
	kind      durable.Kind
	namespace string
	id        string
	value     json.RawMessage
	revision  uint64
}

// table is the transaction surface of one namespace. Changes are staged in
// the table while the store's mutex is held and either applied after a
// durable commit or dropped.
type table interface {
	stage() ([]staged, error)
	apply(changes []staged) error
	rollback()
}

// docTable holds the committed documents of one namespace and the
// transaction's uncommitted edits.
type docTable[V any] struct {
	namespace string
	rows      map[string]*docRow[V]
	dirty     map[string]*V
}

type docRow[V any] struct {
	value    V
	revision uint64
}

func newDocTable[V any](namespace string) *docTable[V] {
	return &docTable[V]{namespace: namespace, rows: make(map[string]*docRow[V]), dirty: make(map[string]*V)}
}

// get returns the value visible to the transaction. Callers must not modify
// it; use edit.
func (t *docTable[V]) get(id string) (*V, bool) {
	if value, ok := t.dirty[id]; ok {
		return value, true
	}
	if row, ok := t.rows[id]; ok {
		return &row.value, true
	}
	return nil, false
}

// edit returns a mutable copy staged for commit, or nil when absent.
func (t *docTable[V]) edit(id string) *V {
	if value, ok := t.dirty[id]; ok {
		return value
	}
	row, ok := t.rows[id]
	if !ok {
		return nil
	}
	value := clone(row.value)
	t.dirty[id] = &value
	return &value
}

// put stages a whole value, creating the document when absent.
func (t *docTable[V]) put(id string, value V) *V {
	t.dirty[id] = &value
	return &value
}

// ids lists committed and staged IDs in sorted order.
func (t *docTable[V]) ids() []string {
	ids := make([]string, 0, len(t.rows)+len(t.dirty))
	for id := range t.rows {
		ids = append(ids, id)
	}
	for id := range t.dirty {
		if _, ok := t.rows[id]; !ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func (t *docTable[V]) stage() ([]staged, error) {
	ids := make([]string, 0, len(t.dirty))
	for id := range t.dirty {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]staged, 0, len(ids))
	for _, id := range ids {
		raw, err := json.Marshal(t.dirty[id])
		if err != nil {
			return nil, fmt.Errorf("encode %s/%s: %w", t.namespace, id, err)
		}
		var revision uint64
		if row, ok := t.rows[id]; ok {
			revision = row.revision
			old, err := json.Marshal(row.value)
			if err != nil {
				return nil, fmt.Errorf("encode %s/%s: %w", t.namespace, id, err)
			}
			if bytes.Equal(old, raw) {
				continue
			}
		}
		out = append(out, staged{kind: durable.DocumentKind, namespace: t.namespace, id: id, value: raw, revision: revision})
	}
	return out, nil
}

func (t *docTable[V]) apply(changes []staged) error {
	for _, change := range changes {
		var value V
		if err := json.Unmarshal(change.value, &value); err != nil {
			return fmt.Errorf("decode %s/%s: %w", t.namespace, change.id, err)
		}
		t.rows[change.id] = &docRow[V]{value: value, revision: change.revision + 1}
	}
	t.dirty = make(map[string]*V)
	return nil
}

func (t *docTable[V]) rollback() { t.dirty = make(map[string]*V) }

// load adopts one committed record during a reload.
func (t *docTable[V]) load(record durable.Record) error {
	var value V
	if err := json.Unmarshal(record.Value, &value); err != nil {
		return fmt.Errorf("decode %s/%s: %w", t.namespace, record.ID, err)
	}
	t.rows[record.ID] = &docRow[V]{value: value, revision: record.Revision}
	return nil
}

// entryTable holds immutable entries in commit order. Staged entries keep
// insertion order, which is also the order a reload sees them in.
type entryTable[V any] struct {
	namespace string
	rows      map[string]V
	order     []string
	dirty     map[string]V
	pending   []string
	// keep false keeps only the order and IDs in memory (event bodies stay
	// in the journal and are read back by ID).
	keep bool
}

func newEntryTable[V any](namespace string, keep bool) *entryTable[V] {
	return &entryTable[V]{namespace: namespace, rows: make(map[string]V), dirty: make(map[string]V), keep: keep}
}

func (t *entryTable[V]) has(id string) bool {
	if _, ok := t.dirty[id]; ok {
		return true
	}
	_, ok := t.rows[id]
	return ok
}

// get returns a committed or staged entry. Tables that do not keep values
// return the zero value for committed entries.
func (t *entryTable[V]) get(id string) (V, bool) {
	if value, ok := t.dirty[id]; ok {
		return value, true
	}
	value, ok := t.rows[id]
	return value, ok
}

func (t *entryTable[V]) add(id string, value V) {
	if t.has(id) {
		return
	}
	t.dirty[id] = value
	t.pending = append(t.pending, id)
}

// all lists entry IDs in commit order, staged ones last.
func (t *entryTable[V]) all() []string {
	if len(t.pending) == 0 {
		return t.order
	}
	ids := make([]string, 0, len(t.order)+len(t.pending))
	return append(append(ids, t.order...), t.pending...)
}

func (t *entryTable[V]) stage() ([]staged, error) {
	out := make([]staged, 0, len(t.pending))
	for _, id := range t.pending {
		raw, err := json.Marshal(t.dirty[id])
		if err != nil {
			return nil, fmt.Errorf("encode %s/%s: %w", t.namespace, id, err)
		}
		out = append(out, staged{kind: durable.EntryKind, namespace: t.namespace, id: id, value: raw})
	}
	return out, nil
}

func (t *entryTable[V]) apply(changes []staged) error {
	for _, change := range changes {
		var value V
		if t.keep {
			if err := json.Unmarshal(change.value, &value); err != nil {
				return fmt.Errorf("decode %s/%s: %w", t.namespace, change.id, err)
			}
		}
		t.rows[change.id] = value
		t.order = append(t.order, change.id)
	}
	t.rollback()
	return nil
}

func (t *entryTable[V]) rollback() {
	t.dirty = make(map[string]V)
	t.pending = nil
}

func (t *entryTable[V]) load(record durable.Record) error {
	var value V
	if t.keep {
		if err := json.Unmarshal(record.Value, &value); err != nil {
			return fmt.Errorf("decode %s/%s: %w", t.namespace, record.ID, err)
		}
	}
	if _, ok := t.rows[record.ID]; !ok {
		t.order = append(t.order, record.ID)
	}
	t.rows[record.ID] = value
	return nil
}

// clone deep-copies a record through its JSON form, the same form the
// journal stores.
func clone[V any](value V) V {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("control: encode %T: %v", value, err))
	}
	var out V
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(fmt.Sprintf("control: decode %T: %v", value, err))
	}
	return out
}

func change(s staged) durable.Change {
	return durable.Change{SessionID: model.ControlSession, Kind: s.kind, Namespace: s.namespace, ID: s.id, Version: 1, Value: s.value}
}
