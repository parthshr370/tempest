package journal

import (
	"bytes"
	"encoding/json"

	ptypes "go.harness.dev/harness/internal/engine/types"
)

// ContextFileRecord marks a directory whose context file this session
// surfaced, so the session neither repeats it nor overspends the
// context_files byte cap while the conversation holds it. Dir and Path are
// workspace-relative slash paths. Withheld marks a file named instead of
// loaded because the cap was spent.
//
// It is journaled inside the MessageEntry of the tool result that delivered
// the file, never on its own: the record and the delivered text commit
// together or not at all.
type ContextFileRecord struct {
	Dir      string `json:"dir"`
	Path     string `json:"path"`
	Bytes    int    `json:"bytes"`
	Withheld bool   `json:"withheld,omitempty"`
}

// ContextDelivery names the tool result that delivered context files: the
// timestamp of the assistant turn that made the call, and the call's ID.
// A session counts the files as shown only while its conversation holds
// that result, so a rewind, reset, fork or compaction that drops it lets
// them surface again.
type ContextDelivery struct {
	Turn       int64
	ToolCallID string
	Files      []ContextFileRecord
}

// prefixDeliveries finds the deliveries among a fork prefix's journal
// records and names each by its place in the prefix transcript. Only the
// few records that carry context files are decoded.
func prefixDeliveries(records []json.RawMessage, messages []ptypes.Message) ([]ContextDelivery, error) {
	type result struct {
		id        string
		timestamp int64
	}
	carried := map[result][]ContextFileRecord{}
	for _, raw := range records {
		if !bytes.Contains(raw, []byte(`"context_files"`)) {
			continue
		}
		entry, err := decodeEntry(raw)
		if err != nil {
			return nil, err
		}
		if message, ok := entry.(MessageEntry); ok && len(message.ContextFiles) > 0 {
			if tool, ok := message.Message.(ptypes.ToolResultMessage); ok {
				carried[result{tool.ToolCallID, tool.Timestamp}] = message.ContextFiles
			}
		}
	}
	if len(carried) == 0 {
		return nil, nil
	}
	var deliveries []ContextDelivery
	var turn int64
	for _, message := range messages {
		switch m := message.(type) {
		case ptypes.AssistantMessage:
			turn = m.Timestamp
		case ptypes.ToolResultMessage:
			if files, ok := carried[result{m.ToolCallID, m.Timestamp}]; ok {
				deliveries = append(deliveries, ContextDelivery{Turn: turn, ToolCallID: m.ToolCallID, Files: files})
			}
		}
	}
	return deliveries, nil
}

// StageContextFiles holds the context files a tool call's result carries
// until the recorder commits that result; callID is the call's canonical
// invocation id, or its provider id outside durable admission.
func (s *Store) StageContextFiles(callID string, files []ContextFileRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stagedContext == nil {
		s.stagedContext = map[string][]ContextFileRecord{}
	}
	s.stagedContext[callID] = append(s.stagedContext[callID], files...)
}

// TakeContextFiles returns and forgets what StageContextFiles holds for
// callID, for the MessageEntry that commits its result.
func (s *Store) TakeContextFiles(callID string) []ContextFileRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	files := s.stagedContext[callID]
	delete(s.stagedContext, callID)
	return files
}
