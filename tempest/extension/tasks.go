package extension

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"go.harness.dev/harness/tempest/durable"
)

// Task is a committed phase snapshot. Effect keys should include ID and Phase,
// not Attempt: a restarted phase may repeat an outside effect.
type Task struct {
	ID            string          `json:"id"`
	SessionID     string          `json:"sessionId"`
	Definition    string          `json:"definition"`
	Version       uint32          `json:"version"`
	CodeRevision  string          `json:"codeRevision"`
	Input         json.RawMessage `json:"input"`
	Checkpoint    json.RawMessage `json:"checkpoint"`
	Phase         string          `json:"phase"`
	Status        string          `json:"status"`
	Owner         string          `json:"owner,omitempty"`
	RunID         string          `json:"runId,omitempty"`
	Background    bool            `json:"background,omitempty"`
	Revision      uint64          `json:"revision"`
	Attempt       uint64          `json:"attempt"`
	Crashes       uint32          `json:"crashes,omitempty"`
	WakeAt        *time.Time      `json:"wakeAt,omitempty"`
	Wait          []string        `json:"wait,omitempty"`
	FailFast      bool            `json:"failFast,omitempty"`
	AbortReason   string          `json:"abortReason,omitempty"`
	Reason        string          `json:"reason,omitempty"`
	Result        json.RawMessage `json:"result,omitempty"`
	CleanupFailed bool            `json:"cleanupFailed,omitempty"`
	Outcome       string          `json:"outcome,omitempty"`
	// Now is supplied for this invocation, not a persisted deadline.
	Now time.Time `json:"now,omitzero"`
}

// TaskSummary keeps list and watch frames independent of checkpoint size.
type TaskSummary struct {
	ID         string     `json:"id"`
	Definition string     `json:"definition"`
	Phase      string     `json:"phase"`
	Status     string     `json:"status"`
	Owner      string     `json:"owner,omitempty"`
	Background bool       `json:"background,omitempty"`
	Revision   uint64     `json:"revision"`
	Attempt    uint64     `json:"attempt"`
	WakeAt     *time.Time `json:"wakeAt,omitempty"`
	Reason     string     `json:"reason,omitempty"`
	Outcome    string     `json:"outcome,omitempty"`
}

// Summary omits payloads; Task retains the complete record for detail reads.
func (task Task) Summary() TaskSummary {
	const maxLabelBytes = 256
	const maxReasonBytes = 512
	clip := func(value string, size int) string {
		if len(value) <= size {
			return value
		}
		return strings.ToValidUTF8(value[:size], "") + "..."
	}
	return TaskSummary{
		ID: clip(task.ID, maxLabelBytes), Definition: clip(task.Definition, maxLabelBytes),
		Phase: clip(task.Phase, maxLabelBytes), Status: task.Status,
		Owner: clip(task.Owner, maxLabelBytes), Background: task.Background,
		Revision: task.Revision, Attempt: task.Attempt, WakeAt: task.WakeAt,
		Reason: clip(task.Reason, maxReasonBytes), Outcome: task.Outcome,
	}
}

// TaskRequest admits a deterministic child or root. Background is explicit and
// can only anchor a root; descendants inherit their owner's lifetime.
type TaskRequest struct {
	ID         string          `json:"id"`
	Definition string          `json:"definition"`
	Input      json.RawMessage `json:"input"`
	Owner      string          `json:"owner,omitempty"`
	RunID      string          `json:"runId,omitempty"`
	Background bool            `json:"background,omitempty"`
}

// TaskDecision joins a phase transition, application writes, children and outbox
// admissions in one commit. Kind is advance, sleep, wait or complete.
type TaskDecision struct {
	Kind        string           `json:"kind"`
	Phase       string           `json:"phase,omitempty"`
	Checkpoint  json.RawMessage  `json:"checkpoint,omitempty"`
	WakeAt      *time.Time       `json:"wakeAt,omitempty"`
	Wait        []string         `json:"wait,omitempty"`
	FailFast    bool             `json:"failFast,omitempty"`
	Result      json.RawMessage  `json:"result,omitempty"`
	Error       string           `json:"error,omitempty"`
	Commit      durable.Commit   `json:"commit"`
	Children    []TaskRequest    `json:"children,omitempty"`
	Memos       []TaskMemo       `json:"memos,omitempty"`
	Submissions []TaskSubmission `json:"submissions,omitempty"`
}

// TaskSubmission is a keyed conversational delivery, admitted with the decision.
type TaskSubmission struct {
	Key  string `json:"key"`
	Text string `json:"text"`
}

// TaskMemo inserts a first-write-wins value with the phase decision.
type TaskMemo struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

// TaskDefinition supplies restartable phases. Migrate is pure and transforms
// checkpoint JSON from a stored version to Version before any handler runs.
type TaskDefinition struct {
	Name        string                                                        `json:"name"`
	Version     uint32                                                        `json:"version"`
	InputSchema json.RawMessage                                               `json:"inputSchema"`
	StateSchema json.RawMessage                                               `json:"stateSchema"`
	Initial     json.RawMessage                                               `json:"initial"`
	Migrations  []uint32                                                      `json:"migrations,omitempty"`
	Run         func(context.Context, Task, Invocation) (TaskDecision, error) `json:"-"`
	Abort       func(context.Context, Task, Invocation) (TaskDecision, error) `json:"-"`
	Migrate     func(uint32, json.RawMessage) (json.RawMessage, error)        `json:"-"`
}
