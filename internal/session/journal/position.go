package journal

import (
	"context"
	"encoding/json"
	"strconv"

	"go.harness.dev/harness/tempest/durable"
)

// Position identifies the next append independently of file layout.
// LegacyOffset preserves recovery obligations from pre-v4 pending checkpoints.
type Position struct {
	Seq          uint64
	LegacyOffset int64
}

// Position captures the store's durable watermark.
func (s *Store) Position() Position { s.mu.Lock(); defer s.mu.Unlock(); return Position{Seq: s.seq} }

// ContainsAfter reports whether the append at a saved position matches expected.
// Later messages don't change that evidence, and reads never execute recovery.
func (s *Store) ContainsAfter(ctx context.Context, position Position, expected Entry) (bool, error) {
	equal := func(raw []byte) (bool, error) {
		entry, err := decodeEntry(raw)
		if err != nil {
			return false, err
		}
		a, err := MarshalEntry(entry.withMeta(Meta{}))
		if err != nil {
			return false, err
		}
		b, err := MarshalEntry(expected.withMeta(Meta{}))
		if err != nil {
			return false, err
		}
		ad, err := durable.Digest(json.RawMessage(a))
		if err != nil {
			return false, err
		}
		bd, err := durable.Digest(json.RawMessage(b))
		return ad == bd, err
	}
	if position.LegacyOffset > 0 {
		page, err := s.Read(ctx, durable.Query{Kind: durable.EntryKind, Namespace: "_tempest/legacy_offsets", ID: strconv.FormatInt(position.LegacyOffset, 10), Limit: 1})
		if err != nil {
			return false, err
		}
		if len(page.Records) > 0 {
			var mapped struct {
				Seq uint64 `json:"seq"`
			}
			if err = json.Unmarshal(page.Records[0].Value, &mapped); err != nil {
				return false, err
			}
			position.Seq = mapped.Seq - 1
		} else {
			// Migration changes every byte offset. No mapping means the old
			// append never committed, not an offset into the replacement file.
			return false, nil
		}
	}
	query := durable.Query{Kind: durable.EntryKind, Namespace: "_tempest/journal", History: true, Limit: 100}
	for {
		page, err := s.Read(ctx, query)
		if err != nil {
			return false, err
		}
		for _, record := range page.Records {
			if record.Seq > position.Seq {
				return equal(record.Value)
			}
		}
		if page.NextCursor == "" {
			return false, nil
		}
		query.Cursor = page.NextCursor
	}
}
