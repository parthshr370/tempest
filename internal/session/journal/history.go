package journal

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/tempest/durable"
)

// HistoryOptions binds a literal search to a stable archive cut.
type HistoryOptions struct {
	Query  string `json:"query"`
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// HistoryMatch retains the durable entry identity, not a compacted slice index.
type HistoryMatch struct {
	EntryID   string          `json:"entryId"`
	SessionID string          `json:"sessionId"`
	Entry     json.RawMessage `json:"entry,omitempty"`
	Excerpt   string          `json:"excerpt,omitempty"`
	Clipped   bool            `json:"clipped,omitempty"`
}

// HistoryPage pages audit history, including entries hidden by reset or rewind.
type HistoryPage struct {
	Matches    []HistoryMatch `json:"matches"`
	NextCursor string         `json:"nextCursor,omitempty"`
	Cut        uint64         `json:"cut"`
}

type historyCursor struct {
	Query   string      `json:"query"`
	Cut     uint64      `json:"cut"`
	Native  string      `json:"native,omitempty"`
	Prefix  *ArchiveRef `json:"prefix,omitempty"`
	Offset  int         `json:"offset,omitempty"`
	Started bool        `json:"started"`
	Session string      `json:"session"`
}

// SearchHistory reads one bounded archive page without injecting it into context.
func (s *Store) SearchHistory(ctx context.Context, options HistoryOptions) (HistoryPage, error) {
	if options.Limit < 0 || options.Limit > 100 {
		return HistoryPage{}, fmt.Errorf("history limit must be between 1 and 100")
	}
	if options.Limit == 0 {
		options.Limit = 20
	}
	cursor := historyCursor{Query: options.Query, Session: s.id}
	if options.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(options.Cursor)
		if err != nil || json.Unmarshal(raw, &cursor) != nil || cursor.Query != options.Query || cursor.Session != s.id || cursor.Offset < 0 {
			return HistoryPage{}, fmt.Errorf("invalid history cursor")
		}
	}
	q := durable.Query{Kind: durable.EntryKind, Namespace: "_tempest/journal", History: true, Limit: options.Limit, Cursor: cursor.Native}
	result := HistoryPage{}
	needle := strings.ToLower(options.Query)
	appendMatch := func(raw json.RawMessage, session string) error {
		entry, err := decodeEntry(raw)
		if err != nil {
			return err
		}
		if entry.meta().ID == "" {
			return nil
		}
		var text string
		switch value := entry.(type) {
		case MessageEntry:
			text = historyText(value.Message)
		case ContextHeadEntry:
			text = value.Note
		case CompactionEntry:
			text = value.Summary
		default:
			return nil
		}
		if index := strings.Index(strings.ToLower(text), needle); index >= 0 {
			const maximumEntryBytes = 2 << 10
			const maximumExcerptBytes = 512
			match := HistoryMatch{EntryID: entry.meta().ID, SessionID: session, Entry: raw}
			if len(raw) > maximumEntryBytes {
				match.Entry = nil
				match.Clipped = true
				// Center on the literal match rather than the serialized entry header.
				start := max(0, index-(maximumExcerptBytes-len(needle))/2)
				start = min(start, index)
				match.Excerpt = strings.ToValidUTF8(text[start:min(len(text), start+maximumExcerptBytes)], "")
			}
			result.Matches = append(result.Matches, match)
		}
		return nil
	}
	if cursor.Prefix != nil {
		if err := s.validateHistoryPrefix(ctx, *cursor.Prefix, cursor.Cut); err != nil {
			return result, err
		}
		segment, err := s.readArchive(ctx, *cursor.Prefix)
		if err != nil {
			return result, err
		}
		end := min(cursor.Offset+options.Limit, len(segment.Records))
		if cursor.Offset > len(segment.Records) {
			return result, fmt.Errorf("invalid history cursor offset")
		}
		for _, raw := range segment.Records[cursor.Offset:end] {
			if err := appendMatch(raw, s.id); err != nil {
				return result, err
			}
		}
		cursor.Offset = end
		if end == len(segment.Records) {
			cursor.Prefix = nil
			cursor.Offset = 0
		}
	} else {
		page, err := s.Read(ctx, q)
		if err != nil {
			return result, err
		}
		cursor.Cut = page.Seq
		for _, record := range page.Records {
			entry, err := decodeEntry(record.Value)
			if err != nil {
				return result, err
			}
			if head, ok := entry.(ContextHeadEntry); ok && head.Prefix != nil {
				cursor.Prefix = head.Prefix
			}
			if err := appendMatch(record.Value, s.id); err != nil {
				return result, err
			}
		}
		cursor.Native, cursor.Started = page.NextCursor, true
	}
	result.Cut = cursor.Cut
	if cursor.Prefix != nil || cursor.Native != "" || !cursor.Started {
		raw, _ := json.Marshal(cursor)
		result.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return result, nil
}

func (s *Store) validateHistoryPrefix(ctx context.Context, reference ArchiveRef, cut uint64) error {
	page, err := s.Read(ctx, durable.Query{Kind: durable.ChildKind, Namespace: "_tempest/forks", ID: "creation", AsOf: cut, Limit: 1})
	if err != nil {
		return err
	}
	if len(page.Records) != 1 {
		return fmt.Errorf("history cursor has no session-owned prefix")
	}
	entry, err := decodeEntry(page.Records[0].Value)
	if err != nil {
		return err
	}
	head, ok := entry.(ContextHeadEntry)
	if !ok || head.Prefix == nil || *head.Prefix != reference {
		return fmt.Errorf("history cursor references a foreign prefix")
	}
	return nil
}

func historyText(message types.Message) string {
	var blocks []types.ContentBlock
	switch value := message.(type) {
	case types.UserMessage:
		if value.Content.Blocks == nil {
			return value.Content.Text
		}
		blocks = value.Content.Blocks
	case types.AssistantMessage:
		blocks = value.Content
	case types.ToolResultMessage:
		blocks = value.Content
	}
	var parts []string
	for _, block := range blocks {
		switch block.Type {
		case types.BlockText:
			parts = append(parts, block.Text)
		case types.BlockThinking:
			parts = append(parts, block.Thinking)
		case types.BlockToolCall:
			parts = append(parts, block.Name)
			decoder := json.NewDecoder(bytes.NewReader(block.Arguments))
			for {
				token, err := decoder.Token()
				if err != nil {
					break
				}
				switch value := token.(type) {
				case string:
					parts = append(parts, value)
				case float64, bool:
					parts = append(parts, fmt.Sprint(value))
				}
			}
		}
	}
	return strings.Join(parts, "\n")
}
