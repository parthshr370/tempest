// Package journal persists a session as append-only JSONL: conversation
// entries plus run records, reduced back into a resumable state on load.
// docs/design/03-durable-core.md describes the format and its invariants.
package journal

import (
	"encoding/json"
	"fmt"
	"time"

	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/work"
)

// JournalVersion is the on-disk format version this package writes. Version 1
// files (entries only, no seq or records) still load.
const JournalVersion = 3

// LaneMain is the only lane in M1. Every line carries it so adding lanes later
// does not change the format.
const LaneMain = "main"

// Header is the first line of a session journal.
type Header struct {
	Type      string    `json:"type"`
	Version   int       `json:"version"`
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Cwd       string    `json:"cwd"`
	Title     string    `json:"title,omitempty"`
}

// Meta is the envelope every line after the header carries. ID and ParentID
// form the linear entry chain and are empty on records.
type Meta struct {
	ID        string
	ParentID  string
	Timestamp time.Time
	// Seq orders every line in the file; zero on version 1 lines.
	Seq  uint64
	Lane string
	// RunID names the run that wrote the line; empty outside a run.
	RunID string
	// EventSeq is the AgentEvent.Seq that caused the write; zero when the
	// write did not come from an agent event.
	EventSeq uint64
}

// Entry is one journal line: a conversation entry or a run record.
type Entry interface {
	kind() EntryType
	meta() Meta
	withMeta(Meta) Entry
}

// EntryType identifies a line's kind.
type EntryType string

const (
	// KindMessage identifies an entry that adds a transcript message.
	KindMessage EntryType = "message"
	// KindModelChange identifies an entry that changes the active model.
	KindModelChange EntryType = "model_change"
	// KindCompaction identifies an entry that summarizes older messages.
	KindCompaction EntryType = "compaction"

	// KindRunStarted records that a run began, before its first model call.
	KindRunStarted EntryType = "run_started"
	// KindToolStarted records intent to run a tool, before it executes.
	KindToolStarted EntryType = "tool_started"
	// KindToolFinished records that a tool returned.
	KindToolFinished EntryType = "tool_finished"
	// KindUsage records one model call's token usage and cost.
	KindUsage EntryType = "usage"
	// KindRunFinished records how a run ended.
	KindRunFinished EntryType = "run_finished"
	// KindPermission records a decision, not an execution intent.
	KindPermission EntryType = "permission"
)

// isRecord reports whether kind is a run record rather than a conversation
// entry. Records stay out of the parent chain.
func isRecord(kind EntryType) bool {
	switch kind {
	case KindRunStarted, KindToolStarted, KindToolFinished, KindUsage, KindRunFinished, KindPermission,
		KindQueueEnqueued, KindQueueCancelled, KindJobStarted, KindJobRunning, KindJobAttached,
		KindJobCancelRequested, KindJobFinished, KindJobCleaned, KindRunStopRequested,
		KindCompactionCallStarted, KindCompactionCallFinished:
		return true
	}
	return false
}

// syncsAfter is the flush policy: these records are fsynced before the append
// returns, so the run start, every tool intent, and the run outcome survive a
// power loss. Everything else reaches the OS on write and is fsynced at the
// next sync point or Close.
func syncsAfter(kind EntryType) bool {
	return kind != KindModelChange && kind != KindPermission && kind != KindToolFinished && kind != KindJobRunning
}

// MessageEntry adds a transcript message.
type MessageEntry struct {
	Meta
	Message         ptypes.Message
	QueueID         string
	Metered         bool
	CallOccurrences []string
}

func (MessageEntry) kind() EntryType            { return KindMessage }
func (e MessageEntry) meta() Meta               { return e.Meta }
func (e MessageEntry) withMeta(meta Meta) Entry { e.Meta = meta; return e }

// ModelChangeEntry changes the active model and optional role.
type ModelChangeEntry struct {
	Meta
	Model string
	Role  string
}

func (ModelChangeEntry) kind() EntryType            { return KindModelChange }
func (e ModelChangeEntry) meta() Meta               { return e.Meta }
func (e ModelChangeEntry) withMeta(meta Meta) Entry { e.Meta = meta; return e }

// CompactionEntry records a summary and the earliest message retained after it.
type CompactionEntry struct {
	Meta
	Summary      string
	FirstKeptID  string
	TokensBefore int
	SourceHash   string
	CompactionID string
	Snapshot     *work.Artifact
}

func (CompactionEntry) kind() EntryType            { return KindCompaction }
func (e CompactionEntry) meta() Meta               { return e.Meta }
func (e CompactionEntry) withMeta(meta Meta) Entry { e.Meta = meta; return e }

// Record payloads below marshal to the line's "payload" object; Meta is the
// envelope and stays out of it.

// RunStartedRecord opens a run.
type RunStartedRecord struct {
	Meta           `json:"-"`
	Model          string `json:"model"`
	Provider       string `json:"provider,omitempty"`
	PromptSummary  string `json:"prompt_summary,omitempty"`
	LogicalRunID   string `json:"logical_run_id,omitempty"`
	ContinuationOf string `json:"continuation_of,omitempty"`
}

func (RunStartedRecord) kind() EntryType            { return KindRunStarted }
func (e RunStartedRecord) meta() Meta               { return e.Meta }
func (e RunStartedRecord) withMeta(meta Meta) Entry { e.Meta = meta; return e }

// ToolStartedRecord is the intent record for one tool call. It is written
// before the tool executes; without a matching ToolFinishedRecord the call's
// outcome is unknown.
type ToolStartedRecord struct {
	Meta       `json:"-"`
	ToolCallID string          `json:"tool_call_id"`
	Tool       string          `json:"tool"`
	Args       json.RawMessage `json:"args,omitempty"`
	CallID     string          `json:"call_id,omitempty"`
}

func (ToolStartedRecord) kind() EntryType            { return KindToolStarted }
func (e ToolStartedRecord) meta() Meta               { return e.Meta }
func (e ToolStartedRecord) withMeta(meta Meta) Entry { e.Meta = meta; return e }

// ToolFinishedRecord marks a tool call as returned. The result text lives in
// the tool-result message entry, not here.
type ToolFinishedRecord struct {
	Meta       `json:"-"`
	ToolCallID string `json:"tool_call_id"`
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	CallID     string `json:"call_id,omitempty"`
}

func (ToolFinishedRecord) kind() EntryType            { return KindToolFinished }
func (e ToolFinishedRecord) meta() Meta               { return e.Meta }
func (e ToolFinishedRecord) withMeta(meta Meta) Entry { e.Meta = meta; return e }

// UsageRecord is one model call's usage, kept apart from the message so the
// session total survives a crash that loses run_finished.
type UsageRecord struct {
	Meta           `json:"-"`
	Model          string       `json:"model"`
	Usage          ptypes.Usage `json:"usage"`
	SourceRef      string       `json:"source_ref,omitempty"`
	MessageID      string       `json:"message_id,omitempty"`
	ChildSessionID string       `json:"child_session_id,omitempty"`
}

func (UsageRecord) kind() EntryType            { return KindUsage }
func (e UsageRecord) meta() Meta               { return e.Meta }
func (e UsageRecord) withMeta(meta Meta) Entry { e.Meta = meta; return e }

// RunFinishedRecord closes a run.
type RunFinishedRecord struct {
	Meta         `json:"-"`
	StopReason   string       `json:"stop_reason"`
	Turns        int          `json:"turns"`
	UsageTotal   ptypes.Usage `json:"usage_total"`
	Error        string       `json:"error,omitempty"`
	ElapsedMS    int64        `json:"elapsed_ms,omitempty"`
	LogicalCalls int          `json:"logical_calls,omitempty"`
}

func (RunFinishedRecord) kind() EntryType            { return KindRunFinished }
func (e RunFinishedRecord) meta() Meta               { return e.Meta }
func (e RunFinishedRecord) withMeta(meta Meta) Entry { e.Meta = meta; return e }

// PermissionRecord records the gate's decision before the tool's intent.
type PermissionRecord struct {
	Meta       `json:"-"`
	ToolCallID string `json:"tool_call_id"`
	Tool       string `json:"tool"`
	Decision   string `json:"decision"`
	Source     string `json:"source"`
	WaitMS     int64  `json:"wait_ms"`
	AskID      string `json:"ask_id,omitempty"`
}

func (PermissionRecord) kind() EntryType            { return KindPermission }
func (e PermissionRecord) meta() Meta               { return e.Meta }
func (e PermissionRecord) withMeta(meta Meta) Entry { e.Meta = meta; return e }

// wireEntry is the one JSONL envelope for entries and records. Message bodies
// and record payloads stay raw until the kind says how to decode them.
type wireEntry struct {
	Type            EntryType       `json:"type"`
	Seq             uint64          `json:"seq,omitempty"`
	ID              string          `json:"id,omitempty"`
	ParentID        string          `json:"parent_id,omitempty"`
	Timestamp       time.Time       `json:"timestamp"`
	Lane            string          `json:"lane,omitempty"`
	RunID           string          `json:"run_id,omitempty"`
	EventSeq        uint64          `json:"event_seq,omitempty"`
	Message         json.RawMessage `json:"message,omitempty"`
	Model           string          `json:"model,omitempty"`
	Role            string          `json:"role,omitempty"`
	Summary         string          `json:"summary,omitempty"`
	FirstKeptID     string          `json:"first_kept_id,omitempty"`
	TokensBefore    int             `json:"tokens_before,omitempty"`
	Payload         json.RawMessage `json:"payload,omitempty"`
	QueueID         string          `json:"queue_id,omitempty"`
	Metered         bool            `json:"metered,omitempty"`
	CallOccurrences []string        `json:"call_occurrences,omitempty"`
	SourceHash      string          `json:"source_hash,omitempty"`
	CompactionID    string          `json:"compaction_id,omitempty"`
	Snapshot        *work.Artifact  `json:"snapshot,omitempty"`
}

// unknownEntryError lets [Load] retain a forward-compatible journal tail as a
// diagnostic instead of discarding the whole transcript.
type unknownEntryError struct {
	typeName EntryType
	id       string
	seq      uint64
}

func (e unknownEntryError) Error() string {
	return fmt.Sprintf("unknown journal entry type %q", e.typeName)
}

// MarshalEntry encodes a typed entry or record for one journal line.
func MarshalEntry(entry Entry) ([]byte, error) {
	if entry == nil {
		return nil, fmt.Errorf("marshal journal entry: nil entry")
	}
	meta := entry.meta()
	wire := wireEntry{
		Type:      entry.kind(),
		Seq:       meta.Seq,
		ID:        meta.ID,
		ParentID:  meta.ParentID,
		Timestamp: meta.Timestamp,
		Lane:      meta.Lane,
		RunID:     meta.RunID,
		EventSeq:  meta.EventSeq,
	}
	switch e := entry.(type) {
	case MessageEntry:
		if e.Message == nil {
			return nil, fmt.Errorf("marshal message entry: nil message")
		}
		message, err := json.Marshal(e.Message)
		if err != nil {
			return nil, fmt.Errorf("marshal message entry: %w", err)
		}
		wire.Message = message
		wire.QueueID, wire.Metered, wire.CallOccurrences = e.QueueID, e.Metered, e.CallOccurrences
	case ModelChangeEntry:
		wire.Model, wire.Role = e.Model, e.Role
	case CompactionEntry:
		wire.Summary, wire.FirstKeptID, wire.TokensBefore = e.Summary, e.FirstKeptID, e.TokensBefore
		wire.SourceHash, wire.CompactionID, wire.Snapshot = e.SourceHash, e.CompactionID, e.Snapshot
	case WorkRecord:
		if err := validateWorkRecord(e); err != nil {
			return nil, err
		}
		wire.Payload = e.Payload
	case RunStartedRecord, ToolStartedRecord, ToolFinishedRecord, UsageRecord, RunFinishedRecord, PermissionRecord:
		payload, err := json.Marshal(e)
		if err != nil {
			return nil, fmt.Errorf("marshal %s record: %w", wire.Type, err)
		}
		wire.Payload = payload
	default:
		return nil, fmt.Errorf("marshal journal entry: unsupported entry type %T", entry)
	}
	return json.Marshal(wire)
}

// decodeEntry validates the shared envelope before choosing the concrete
// entry or record, leaving unknown kinds recoverable to the loader.
func decodeEntry(raw []byte) (Entry, error) {
	var wire wireEntry
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("decode journal entry: %w", err)
	}
	if wire.Type == "" {
		return nil, fmt.Errorf("decode journal entry: missing type")
	}
	if wire.Timestamp.IsZero() {
		return nil, fmt.Errorf("decode journal entry: missing timestamp")
	}
	meta := Meta{
		ID:        wire.ID,
		ParentID:  wire.ParentID,
		Timestamp: wire.Timestamp,
		Seq:       wire.Seq,
		Lane:      wire.Lane,
		RunID:     wire.RunID,
		EventSeq:  wire.EventSeq,
	}
	if isRecord(wire.Type) {
		return decodeRecord(wire.Type, meta, wire.Payload)
	}
	// Only known entry kinds must carry an id; a newer writer's unknown kind
	// may be an id-less record and is left for the loader to skip.
	if wire.ID == "" && (wire.Type == KindMessage || wire.Type == KindModelChange || wire.Type == KindCompaction) {
		return nil, fmt.Errorf("decode journal entry: missing id")
	}
	switch wire.Type {
	case KindMessage:
		message, err := ptypes.UnmarshalMessage(wire.Message)
		if err != nil {
			return nil, fmt.Errorf("decode journal message: %w", err)
		}
		return MessageEntry{Meta: meta, Message: message, QueueID: wire.QueueID, Metered: wire.Metered, CallOccurrences: wire.CallOccurrences}, nil
	case KindModelChange:
		return ModelChangeEntry{Meta: meta, Model: wire.Model, Role: wire.Role}, nil
	case KindCompaction:
		return CompactionEntry{
			Meta:         meta,
			Summary:      wire.Summary,
			FirstKeptID:  wire.FirstKeptID,
			TokensBefore: wire.TokensBefore,
			SourceHash:   wire.SourceHash,
			CompactionID: wire.CompactionID,
			Snapshot:     wire.Snapshot,
		}, nil
	default:
		return nil, unknownEntryError{typeName: wire.Type, id: wire.ID, seq: wire.Seq}
	}
}

// decodeRecord requires the fields reduction keys on, so a record that cannot
// be attributed to a run or tool call is treated as a malformed line.
func decodeRecord(kind EntryType, meta Meta, payload json.RawMessage) (Entry, error) {
	if isWorkKind(kind) {
		record := WorkRecord{Meta: meta, Type: kind, Payload: payload}
		return record, validateWorkRecord(record)
	}
	if meta.RunID == "" {
		return nil, fmt.Errorf("decode %s record: missing run_id", kind)
	}
	if len(payload) == 0 {
		return nil, fmt.Errorf("decode %s record: missing payload", kind)
	}
	var record Entry
	var err error
	switch kind {
	case KindPermission:
		var r PermissionRecord
		err = json.Unmarshal(payload, &r)
		if err == nil && (r.ToolCallID == "" || (r.Decision != "allow" && r.Decision != "deny")) {
			err = fmt.Errorf("missing tool_call_id or invalid decision")
		}
		record = r
	case KindRunStarted:
		var r RunStartedRecord
		err = json.Unmarshal(payload, &r)
		record = r
	case KindToolStarted:
		var r ToolStartedRecord
		err = json.Unmarshal(payload, &r)
		if err == nil && r.ToolCallID == "" {
			err = fmt.Errorf("missing tool_call_id")
		}
		record = r
	case KindToolFinished:
		var r ToolFinishedRecord
		err = json.Unmarshal(payload, &r)
		if err == nil && r.ToolCallID == "" {
			err = fmt.Errorf("missing tool_call_id")
		}
		record = r
	case KindUsage:
		var r UsageRecord
		err = json.Unmarshal(payload, &r)
		record = r
	case KindRunFinished:
		var r RunFinishedRecord
		err = json.Unmarshal(payload, &r)
		record = r
	}
	if err != nil {
		return nil, fmt.Errorf("decode %s record: %w", kind, err)
	}
	return record.withMeta(meta), nil
}
