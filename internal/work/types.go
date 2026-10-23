// Package work holds the shared vocabulary for durable queues and background
// jobs: queue items, job records, run hooks and the controller seam between
// the agent loop, the job manager and the session layer.
package work

import (
	"context"
	"encoding/json"
	"time"

	types "go.harness.dev/harness/internal/engine/types"
)

type QueueKind string

const (
	Steer    QueueKind = "steer"
	FollowUp QueueKind = "follow_up"
)

type QueueInput struct {
	Kind        QueueKind
	Text        string
	Key         string // optional session-scoped idempotency key
	SourceJobID string // internal only, never accepted from wire
}
type QueueItem struct {
	ID          string    `json:"id"`
	Kind        QueueKind `json:"kind"`
	Text        string    `json:"text"`
	Key         string    `json:"key,omitempty"`
	SourceJobID string    `json:"sourceJobId,omitempty"`
	EnqueuedAt  time.Time `json:"enqueuedAt"`
}
type QueuedMessage struct {
	ID      string
	Message types.Message
}
type QueueSource interface {
	Take(context.Context, QueueKind) ([]QueuedMessage, error)
	Pending() (bool, <-chan struct{}) // predicate + change channel under one lock
}

type Invocation struct {
	RunID      string
	CallID     string // canonical occurrence id, not the raw provider id
	ToolCallID string // raw provider id, for correlation only
}
type RunInfo struct {
	SessionID    string
	RunID        string
	LogicalRunID string
	Resume       bool
}
type RunHooks interface {
	Pending() bool // recoverable jobs or completions not yet enqueued
	Start(context.Context, RunInfo) error
	Idle(context.Context) (bool, error)   // true: poll queues again; false: no owned work
	Finish(context.Context, string) error // stop reason; close admission, cancel/join
}

// Keyed context accessors InvocationFromContext(ctx) (Invocation, bool) and
// WithInvocation(ctx, Invocation) context.Context are implemented by A in
// internal/work/context.go (the only additional work file owned by A).

type TaskSpec struct {
	Description  string          `json:"description,omitempty"`
	Prompt       string          `json:"prompt"`
	SubagentType string          `json:"subagent_type"`
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
}
type BashSpec struct {
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeout_seconds"` // normalized internal spec
}
type Artifact struct {
	Path   string `json:"path"` // private host-local path
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}
type Job struct {
	ID             string          `json:"id"`
	Kind           string          `json:"kind"`  // task | bash
	State          string          `json:"state"` // starting | running | recovering | completed | failed | cancelled | unknown
	RunID          string          `json:"runId"` // original durable owner
	LogicalRunID   string          `json:"logicalRunId"`
	ToolCallID     string          `json:"toolCallId"`
	ChildSessionID string          `json:"childSessionId,omitempty"`
	Description    string          `json:"description,omitempty"`
	StartedAt      time.Time       `json:"startedAt"`
	FinishedAt     time.Time       `json:"finishedAt,omitzero"`
	Cwd            string          `json:"cwd,omitempty"`
	Isolation      string          `json:"isolation,omitempty"` // worktree | shared (general); empty for explore/bash
	BaseCommit     string          `json:"baseCommit,omitempty"`
	ResultText     string          `json:"resultText,omitempty"` // <= 4 KiB preview
	Result         *Artifact       `json:"result,omitempty"`
	Patch          *Artifact       `json:"patch,omitempty"`
	Output         *Artifact       `json:"output,omitempty"`
	Value          json.RawMessage `json:"value,omitempty"`         // typed yield <= 64 KiB
	ValueInResult  bool            `json:"valueInResult,omitempty"` // inline Value omitted to bound response
	Usage          types.Usage     `json:"usage"`                   // this job; do not add to global total again
	ExitCode       *int            `json:"exitCode,omitempty"`
	Reason         string          `json:"reason,omitempty"`  // bounded machine reason, no raw args
	Cleanup        string          `json:"cleanup,omitempty"` // none | retained | removed | blocked
}
type JobPage struct {
	Jobs       []Job  `json:"jobs"`
	NextCursor string `json:"nextCursor,omitempty"`
}
type LogPage struct {
	Text       string `json:"text"`
	Offset     int64  `json:"offset"`
	NextOffset int64  `json:"nextOffset"`
	EOF        bool   `json:"eof"`
	Truncated  bool   `json:"truncated"`
}
type Controller interface {
	StartTask(context.Context, TaskSpec) (Job, error)
	StartBash(context.Context, BashSpec) (Job, error)
	Jobs(context.Context, string, int) (JobPage, error)
	Wait(context.Context, []string, time.Duration) ([]Job, error)
	Cancel(context.Context, string) (Job, error)
	Logs(context.Context, string, int64, int) (LogPage, error)
	Cleanup(context.Context, string) (Job, error)
}
type Fact struct {
	Kind    string
	RunID   string
	Payload json.RawMessage
}
type Event struct {
	Kind            string // job | job_output | queue | compaction
	Job             *Job
	Queue           *QueueItem
	Action          string
	Bytes           int64
	Reason          string
	RunID           string
	CompactionID    string
	SourceHash      string
	ChildSessionID  string
	ChildToolCallID string
	ChildTitle      string
}
type Ceiling struct {
	Mode        string
	Tools       []string
	Overrides   map[string]string // allow | deny | prompt; no credentials
	Interactive map[string]string // launch-time session choices
}
type TaskEnvironment struct {
	Ceiling  Ceiling
	Model    string // resolved model identity, fixed for recovery
	Provider string
}
type Workspace struct {
	Repo        string
	CommonDir   string
	BaseCommit  string
	RelativeCwd string
	Path        string
	Marker      string
}
type Recovery struct {
	Job             Job
	Invocation      Invocation
	IdentityDigest  string
	Task            *TaskSpec
	Bash            *BashSpec
	Environment     *TaskEnvironment
	Workspace       *Workspace
	ChildJournal    string
	CancelRequested bool
	DeliveryPending bool
}
type TaskLaunch struct {
	Job         Job
	Spec        TaskSpec
	Environment TaskEnvironment
	JournalPath string
	Resume      bool
}
type TaskResult struct {
	Text       string
	Value      json.RawMessage
	Usage      types.Usage
	StopReason string
	Error      string
}
type TaskRunner func(context.Context, TaskLaunch) (TaskResult, error)
type Host struct {
	SessionID   string
	JournalPath string
	Cwd         string
	Queues      QueueSource
	Append      func(context.Context, Fact) error // A's strict schema adapter, sync before return
	Enqueue     func(context.Context, QueueInput) (QueueItem, error)
	Publish     func(context.Context, Event) error // mapped through root Agent, no raw network I/O
	PrepareTask func(context.Context, TaskSpec) (TaskEnvironment, error)
	RunTask     TaskRunner
}
