package journal

import (
	"context"
	"encoding/json"

	"go.harness.dev/harness/tempest/durable"
)

// ClaimOverflow consumes the one recovery retry belonging to the latest input.
// The receipt survives compaction, continuation and process restart.
func (s *Store) ClaimOverflow(ctx context.Context) (bool, error) {
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
		return false, durable.ErrClosed
	}
	if s.failed != nil {
		return false, s.failed
	}
	query := durable.Query{SessionID: s.id, Kind: durable.EntryKind, Namespace: "_tempest/journal", Reverse: true, Limit: 100}
	for {
		page, err := s.durable.Read(ctx, query)
		if err != nil {
			return false, err
		}
		for _, record := range page.Records {
			entry, err := decodeEntry(record.Value)
			if err != nil {
				return false, err
			}
			message, ok := entry.(MessageEntry)
			if !ok || message.Message.Role() != "user" {
				continue
			}
			prior, err := s.durable.Read(ctx, durable.Query{SessionID: s.id, Kind: durable.DocumentKind, Namespace: "_tempest/overflow", ID: message.ID, Limit: 1})
			if err != nil {
				return false, err
			}
			if len(prior.Records) > 0 {
				return false, nil
			}
			value, err := json.Marshal(message.ID)
			if err != nil {
				return false, err
			}
			decision := "overflow:" + message.ID
			if s.external {
				decision = s.id + ":" + decision
			}
			receipt, err := s.durable.Commit(ctx, durable.Commit{ID: decision, Expected: []durable.RevisionCheck{{SessionID: s.id, Kind: durable.DocumentKind, Namespace: "_tempest/overflow", ID: message.ID}}, Changes: []durable.Change{{SessionID: s.id, Kind: durable.DocumentKind, Namespace: "_tempest/overflow", ID: message.ID, Version: 1, Value: value}}})
			if err != nil {
				return false, err
			}
			s.seq = max(s.seq, receipt.Seq)
			committed = true
			return true, nil
		}
		if page.NextCursor == "" {
			return false, nil
		}
		query.Cursor = page.NextCursor
	}
}
