// Package hub reduces session events and isolates bounded subscribers.
package hub

import (
	"encoding/json"
	"errors"
	"time"

	"go.harness.dev/harness/internal/agent"
	types "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/work"
	"go.harness.dev/harness/tempest/durable"
)

// ErrStaleEpoch means a compaction replaced the requested transcript.
var ErrStaleEpoch = errors.New("stale history epoch")

// ToolKind is the ACP category used to display a tool call.
type ToolKind string

// Option is a selectable approval decision.
type Option struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// Ask carries the input and available decisions of a pending tool approval.
type Ask struct {
	SessionID       string          `json:"sessionId"`
	ToolCallID      string          `json:"toolCallId"`
	Tool            string          `json:"tool"`
	Title           string          `json:"title"`
	Kind            ToolKind        `json:"kind"`
	RawInput        json.RawMessage `json:"rawInput"`
	Reason          string          `json:"reason"`
	Options         []Option        `json:"options"`
	JobID           string          `json:"jobId,omitempty"`
	ChildSessionID  string          `json:"childSessionId,omitempty"`
	ChildToolCallID string          `json:"childToolCallId,omitempty"`
	Cwd             string          `json:"cwd,omitempty"`
}

// Update is one sequenced ACP or Tempest payload.
type Update struct {
	Seq       uint64          `json:"seq"`
	At        time.Time       `json:"at"`
	SessionID string          `json:"sessionId"`
	Update    json.RawMessage `json:"update"`
	Kind      string          `json:"kind"`
}

// RunState describes the session's currently admitted run.
type RunState struct {
	Status         string    `json:"status"`
	RunID          string    `json:"runId,omitempty"`
	LogicalRunID   string    `json:"logicalRunId,omitempty"`
	ContinuationOf string    `json:"continuationOf,omitempty"`
	StartedAt      time.Time `json:"startedAt,omitempty"`
	Turns          int       `json:"turns"`
}

// RunSummary describes the last durable run, including interrupted runs.
type RunSummary struct {
	RunID      string    `json:"runId"`
	StopReason string    `json:"stopReason"`
	Error      string    `json:"error,omitempty"`
	Finished   bool      `json:"finished"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
}

// StreamingMessage holds text not yet committed to transcript history.
type StreamingMessage struct {
	MessageID string `json:"messageId"`
	Text      string `json:"text"`
	Thought   string `json:"thought"`
}

// ToolState holds the latest status and content of a current-run call.
type ToolState struct {
	ToolCallID string          `json:"toolCallId"`
	Title      string          `json:"title"`
	Kind       ToolKind        `json:"kind"`
	Status     string          `json:"status"`
	RawInput   json.RawMessage `json:"rawInput,omitempty"`
	RawOutput  json.RawMessage `json:"rawOutput,omitempty"`
	Content    json.RawMessage `json:"content,omitempty"`
}

// UsageState separates cumulative spend from the most recent context window.
type UsageState struct {
	Total       types.Usage `json:"total"`
	LastCall    types.Usage `json:"lastCall"`
	ContextUsed int         `json:"contextUsed"`
	ContextSize int         `json:"contextSize"`
	CostUnknown bool        `json:"costUnknown"`
}

// Snapshot is a detached view captured atomically with subscriber registration.
type Snapshot struct {
	Seq               uint64                    `json:"seq"`
	SessionID         string                    `json:"sessionId"`
	Cwd               string                    `json:"cwd"`
	Model             string                    `json:"model"`
	PermissionMode    string                    `json:"permissionMode"`
	Budget            agent.Budget              `json:"budget"`
	Run               RunState                  `json:"run"`
	LastRun           *RunSummary               `json:"lastRun,omitempty"`
	HistoryEpoch      int                       `json:"historyEpoch"`
	HistoryLen        int                       `json:"historyLen"`
	History           []Update                  `json:"history"`
	Streaming         *StreamingMessage         `json:"streaming,omitempty"`
	Tools             []ToolState               `json:"tools"`
	PendingAsks       []Ask                     `json:"pendingAsks"`
	Usage             UsageState                `json:"usage"`
	Interrupted       []journal.InterruptedTool `json:"interrupted"`
	Diagnostics       []string                  `json:"diagnostics"`
	Queues            []work.QueueItem          `json:"queues"`
	Jobs              []work.Job                `json:"jobs"`
	JobsTruncated     bool                      `json:"jobsTruncated"`
	JobsCursor        string                    `json:"jobsCursor,omitempty"`
	DurableSeq        uint64                    `json:"durableSeq"`
	Documents         []durable.Record          `json:"documents"`
	Submissions       []durable.Record          `json:"submissions"`
	Tasks             []durable.Record          `json:"tasks"`
	DocumentsCursor   string                    `json:"documentsCursor,omitempty"`
	SubmissionsCursor string                    `json:"submissionsCursor,omitempty"`
}

// WireBudget is the ACP form of [agent.Budget], matching the session/new and
// tempest/set_budget parameters. Zero fields are unlimited.
type WireBudget struct {
	MaxTurns       int     `json:"maxTurns"`
	MaxCostUSD     float64 `json:"maxCostUsd"`
	MaxWallSeconds float64 `json:"maxWallSeconds"`
}

// NewWireBudget converts a budget to its wire form.
func NewWireBudget(b agent.Budget) WireBudget {
	return WireBudget{MaxTurns: b.MaxTurns, MaxCostUSD: b.MaxCostUSD, MaxWallSeconds: b.MaxWallTime.Seconds()}
}

// Budget converts the wire form back to a budget.
func (w WireBudget) Budget() agent.Budget {
	return agent.Budget{MaxTurns: w.MaxTurns, MaxCostUSD: w.MaxCostUSD, MaxWallTime: time.Duration(w.MaxWallSeconds * float64(time.Second))}
}

// snapshotJSON shadows Budget so the snapshot carries the camelCase wire form.
type snapshotJSON struct {
	*snapshotFields
	Budget WireBudget `json:"budget"`
}
type snapshotFields Snapshot

// MarshalJSON writes the budget in its wire form.
func (s Snapshot) MarshalJSON() ([]byte, error) {
	return json.Marshal(snapshotJSON{(*snapshotFields)(&s), NewWireBudget(s.Budget)})
}

// UnmarshalJSON reads the budget's wire form.
func (s *Snapshot) UnmarshalJSON(raw []byte) error {
	v := snapshotJSON{snapshotFields: (*snapshotFields)(s)}
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	s.Budget = v.Budget.Budget()
	return nil
}
