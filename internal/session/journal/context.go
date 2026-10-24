package journal

import (
	"context"
	"encoding/json"
	"fmt"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/tempest/durable"
)

// KindContextHead changes active context without deleting audit records.
const KindContextHead EntryType = "context_head"

// ContextHeadEntry starts a new context epoch. A fork pins its inherited archive.
type ContextHeadEntry struct {
	Meta
	Reason        string      `json:"reason"`
	Note          string      `json:"note,omitempty"`
	Prefix        *ArchiveRef `json:"prefix,omitempty"`
	SubmissionKey string      `json:"submission_key,omitempty"`
}

func (ContextHeadEntry) kind() EntryType         { return KindContextHead }
func (e ContextHeadEntry) meta() Meta            { return e.Meta }
func (e ContextHeadEntry) withMeta(m Meta) Entry { e.Meta = m; return e }

// ContextEpoch identifies resets and rewinds, but ignores ordinary tail growth.
func (s *Store) ContextEpoch(ctx context.Context) (string, error) {
	page, err := s.Read(ctx, durable.Query{Kind: durable.DocumentKind, Namespace: "_tempest/context", ID: "head", Limit: 1})
	if err != nil {
		return "", err
	}
	if len(page.Records) == 0 {
		return "initial", nil
	}
	var epoch string
	if err := json.Unmarshal(page.Records[0].Value, &epoch); err != nil {
		return "", err
	}
	return epoch, nil
}

func (s *Store) contextChanges(ctx context.Context, raw []byte, meta Meta) ([]durable.Change, error) {
	var wire wireEntry
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, err
	}
	if wire.Type == KindCompaction && wire.CompactionTask != "" {
		value, _ := json.Marshal(meta.ID)
		return []durable.Change{{SessionID: s.id, Kind: durable.DocumentKind, Namespace: "_tempest/compaction", ID: wire.CompactionTask, Version: 1, Value: value}}, nil
	}
	if wire.Type != KindContextHead && wire.Type != KindRewind {
		return nil, nil
	}
	value, _ := json.Marshal(meta.ID)
	changes := []durable.Change{{SessionID: s.id, Kind: durable.DocumentKind, Namespace: "_tempest/context", ID: "head", Version: 1, Value: value}}
	entry, err := decodeEntry(raw)
	if err != nil {
		return nil, err
	}
	if rewind, ok := entry.(RewindEntry); ok {
		for _, change := range rewind.Documents {
			if change.Kind != durable.DocumentKind || change.SessionID != s.id || change.Namespace == "" || change.Namespace[0] == '_' {
				return nil, fmt.Errorf("invalid rewind document")
			}
			changes = append(changes, change)
		}
		return changes, nil
	}
	head := entry.(ContextHeadEntry)
	if head.Reason == "reset" || head.Reason == "handoff" {
		definitions, err := s.documentDefinitions(ctx, 0)
		if err != nil {
			return nil, err
		}
		for _, definition := range definitions {
			if definition.Reset != durable.ResetInitial {
				continue
			}
			records, err := s.readAll(ctx, durable.Query{SessionID: s.id, Kind: durable.DocumentKind, Namespace: definition.Namespace})
			if err != nil {
				return nil, err
			}
			if len(records) == 0 {
				records = []durable.Record{{Change: durable.Change{ID: "default"}}}
			}
			for _, record := range records {
				changes = append(changes, durable.Change{SessionID: s.id, Kind: durable.DocumentKind, Namespace: definition.Namespace, ID: record.ID, Version: definition.Version, Value: definition.Initial})
			}
		}
	}
	if head.SubmissionKey != "" {
		page, err := s.durable.Read(ctx, durable.Query{SessionID: s.id, Kind: durable.SubmissionKind, Namespace: "_tempest/submissions", ID: head.SubmissionKey, Limit: 1})
		if err != nil {
			return nil, err
		}
		if len(page.Records) != 1 {
			return nil, fmt.Errorf("context submission missing")
		}
		var submission durable.Submission
		if err := json.Unmarshal(page.Records[0].Value, &submission); err != nil {
			return nil, err
		}
		if submission.State != "accepted" {
			return nil, fmt.Errorf("context submission already settled")
		}
		submission.State, submission.AnswerEntryID = "completed", meta.ID
		payload, _ := json.Marshal(submission)
		changes = append(changes, durable.Change{SessionID: s.id, Kind: durable.SubmissionKind, Namespace: "_tempest/submissions", ID: head.SubmissionKey, Version: 1, Value: payload})
	}
	return changes, nil
}

func (s *Store) readAll(ctx context.Context, query durable.Query) ([]durable.Record, error) {
	query.Limit = 1000
	var records []durable.Record
	for {
		page, err := s.durable.Read(ctx, query)
		if err != nil {
			return nil, err
		}
		records = append(records, page.Records...)
		if page.NextCursor == "" {
			return records, nil
		}
		query.Cursor = page.NextCursor
	}
}
func (s *Store) documentDefinitions(ctx context.Context, cut uint64) ([]durable.DocumentDefinition, error) {
	records, err := s.readAll(ctx, durable.Query{SessionID: s.id, Kind: durable.DocumentKind, Namespace: "_tempest/definitions", AsOf: cut})
	if err != nil {
		return nil, err
	}
	var definitions []durable.DocumentDefinition
	for _, record := range records {
		var definition durable.DocumentDefinition
		if err := json.Unmarshal(record.Value, &definition); err != nil {
			return nil, err
		}
		definitions = append(definitions, definition)
	}
	return definitions, nil
}

// RewindDocuments projects only rewindable application values at a checkpoint.
// Grants, submissions and accounting don't participate in this projection.
func (s *Store) RewindDocuments(ctx context.Context, cut uint64) ([]durable.Change, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cut == 0 {
		return nil, nil
	}
	definitions, err := s.documentDefinitions(ctx, cut)
	if err != nil {
		return nil, err
	}
	var changes []durable.Change
	for _, definition := range definitions {
		if definition.History != durable.HistoryRewindable {
			continue
		}
		old, err := s.readAll(ctx, durable.Query{SessionID: s.id, Kind: durable.DocumentKind, Namespace: definition.Namespace, AsOf: cut})
		if err != nil {
			return nil, err
		}
		current, err := s.readAll(ctx, durable.Query{SessionID: s.id, Kind: durable.DocumentKind, Namespace: definition.Namespace})
		if err != nil {
			return nil, err
		}
		seen := make(map[string]bool)
		for _, record := range old {
			change := record.Change
			change.SessionID = s.id
			changes = append(changes, change)
			seen[record.ID] = true
		}
		for _, record := range current {
			if !seen[record.ID] {
				changes = append(changes, durable.Change{SessionID: s.id, Kind: durable.DocumentKind, Namespace: definition.Namespace, ID: record.ID, Version: definition.Version, Delete: true})
			}
		}
	}
	return changes, nil
}

// ApplyPendingContext runs only at a settled tool boundary or while idle.
func (s *Store) ApplyPendingContext(ctx context.Context) (bool, error) {
	q := durable.Query{Kind: durable.SubmissionKind, Namespace: "_tempest/submissions", Active: true, Limit: 100}
	changed := false
	for {
		page, err := s.Read(ctx, q)
		if err != nil {
			return changed, err
		}
		for _, record := range page.Records {
			var submission durable.Submission
			if err := json.Unmarshal(record.Value, &submission); err != nil {
				return changed, err
			}
			if submission.Placement != "reset" || submission.State != "accepted" {
				continue
			}
			var note string
			if err := json.Unmarshal(submission.Payload, &note); err != nil {
				return changed, err
			}
			if err := s.Append(ctx, ContextHeadEntry{Reason: "reset", Note: note, SubmissionKey: record.ID}); err != nil {
				return changed, err
			}
			changed = true
		}
		if page.NextCursor == "" {
			break
		}
		q.Cursor = page.NextCursor
	}
	return changed, nil
}

func rawMessages(messages []types.Message) ([]json.RawMessage, error) {
	raw := make([]json.RawMessage, 0, len(messages))
	for _, message := range messages {
		value, err := json.Marshal(message)
		if err != nil {
			return nil, err
		}
		raw = append(raw, value)
	}
	return raw, nil
}

// AppendCompactionPlacement commits context replacement and its task receipt.
func (s *Store) AppendCompactionPlacement(ctx context.Context, entry CompactionEntry, taskID string) error {
	entry.TaskID = taskID
	return s.Append(ctx, entry)
}

// AppendToolBatch publishes all results and an optional handoff in one v4 envelope.
func (s *Store) AppendToolBatch(ctx context.Context, results []MessageEntry, head *ContextHeadEntry) error {
	s.mu.Lock()
	committed := false
	defer func() {
		observer := s.onCommit
		s.mu.Unlock()
		if committed && observer != nil {
			observer()
		}
	}()
	if s.closed {
		return durable.ErrClosed
	}
	if s.failed != nil {
		return s.failed
	}
	count := len(results)
	if head != nil {
		count++
	}
	if count == 0 {
		return nil
	}
	if s.durable == nil {
		s.initDurable()
	}
	batch := durable.Commit{ID: fmt.Sprintf("tool-batch:%d", s.seq+1), Changes: make([]durable.Change, 0, count+1)}
	if s.external {
		batch.ID = s.id + ":" + batch.ID
	}
	parent := s.lastID
	for index := range count {
		var entry Entry
		if index < len(results) {
			entry = results[index]
		} else {
			entry = *head
		}
		meta := entry.meta()
		meta.ID, meta.ParentID, meta.Timestamp = s.newID(), parent, s.clock()
		// The enclosing commit orders this batch; individual entries have no sequence.
		meta.Seq = 0
		raw, err := MarshalEntry(entry.withMeta(meta))
		if err != nil {
			return err
		}
		batch.Changes = append(batch.Changes, durable.Change{SessionID: s.id, Kind: durable.EntryKind, Namespace: "_tempest/journal", ID: meta.ID, Version: 1, Value: raw})
		extra, err := s.contextChanges(ctx, raw, meta)
		if err != nil {
			return err
		}
		batch.Changes = append(batch.Changes, extra...)
		parent = meta.ID
	}
	receipt, err := s.durable.Commit(ctx, batch)
	if err == nil {
		s.seq, s.lastID = receipt.Seq, parent
		committed = true
	}
	return err
}
