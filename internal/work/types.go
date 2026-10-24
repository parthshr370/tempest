// Package work holds the shared vocabulary for durable queues and background
// jobs: queue items, job records, run hooks and the controller seam between
// the agent loop, the job manager and the session layer.
package work

import (
	"context"
	"encoding/json"
	"time"

	"go.harness.dev/harness/internal/cloudcmd"
	types "go.harness.dev/harness/internal/engine/types"
)

type QueueKind string

const (
	Steer    QueueKind = "steer"
	FollowUp QueueKind = "follow_up"
)

// MessageKeyPrefix reserves queue idempotency keys for agent-to-agent
// messages; MailboxCap bounds one agent's unread messages.
// DefaultReadyTimeoutMS and MaxReadyTimeoutMS bound a service's readiness wait.
const (
	MessageKeyPrefix      = "msg:"
	MailboxCap            = 100
	DefaultReadyTimeoutMS = 30000
	MaxReadyTimeoutMS     = 300000
	// DefaultStdinWaitMS and MaxStdinWaitMS bound how long write_stdin
	// collects output when it does not go quiet first.
	DefaultStdinWaitMS = 2000
	MaxStdinWaitMS     = 30000
	// MaxJobPage is the largest page Controller.Jobs serves.
	MaxJobPage = 50
)

type QueueInput struct {
	Kind        QueueKind
	Text        string
	Key         string // optional session-scoped idempotency key
	SourceJobID string // internal only, never accepted from wire
	// SourceAgent marks an agent-to-agent message: "parent" or the sending
	// child's job id. Internal only, never accepted from wire.
	SourceAgent string
}
type QueueItem struct {
	ID          string    `json:"id"`
	Kind        QueueKind `json:"kind"`
	Text        string    `json:"text"`
	Key         string    `json:"key,omitempty"`
	SourceJobID string    `json:"sourceJobId,omitempty"`
	SourceAgent string    `json:"sourceAgent,omitempty"`
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
	Reopen(context.Context) error         // resume admission after completion feedback continues the run
	Finish(context.Context, string) error // stop reason; close admission, cancel/join
}

// Keyed context accessors InvocationFromContext(ctx) (Invocation, bool) and
// WithInvocation(ctx, Invocation) context.Context are implemented by A in
// internal/work/context.go (the only additional work file owned by A).

type TaskSpec struct {
	Description   string          `json:"description,omitempty"`
	Prompt        string          `json:"prompt"`
	SubagentType  string          `json:"subagent_type"`
	OutputSchema  json.RawMessage `json:"output_schema,omitempty"`
	PresetID      string          `json:"preset_id,omitempty"`
	PresetVersion uint32          `json:"preset_version,omitempty"`
}
type BashSpec struct {
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeout_seconds"` // normalized internal spec
	// Name, unique among the session's running jobs, addresses a service.
	Name string `json:"name,omitempty"`
	// ReadyLog and ReadyPort are the service's readiness conditions; every
	// condition given must hold before StartBash returns.
	ReadyLog       string `json:"ready_log,omitempty"`
	ReadyPort      int    `json:"ready_port,omitempty"`
	ReadyTimeoutMS int    `json:"ready_timeout_ms,omitempty"`
	// PTY runs the command on a pseudo-terminal: the process sees a
	// terminal on stdin, stdout and stderr, and its output is one stream.
	PTY bool `json:"pty,omitempty"`
	// Stdin gives the process a pipe write_stdin writes to. Without it, or
	// PTY, a background job reads /dev/null and sees end of file at once.
	Stdin bool `json:"stdin,omitempty"`
}

// JobArtifactsDir is the directory inside a session's data directory
// (<journal>.d) that holds <job id>/{output.log,result.txt,changes.patch}.
const JobArtifactsDir = "jobs"

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
	Reason         string          `json:"reason,omitempty"`     // bounded machine reason, no raw args
	Cleanup        string          `json:"cleanup,omitempty"`    // none | retained | removed | blocked
	Name           string          `json:"name,omitempty"`       // bash service name
	Readiness      string          `json:"readiness,omitempty"`  // waiting | ready | not_ready, for jobs with a readiness condition
	ReadyMatch     string          `json:"readyMatch,omitempty"` // the matched log line and/or "127.0.0.1:<port>"
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

// StdinRequest is one write_stdin call on a running bash job: Text is written
// first, then CloseStdin ends its input, then output is collected for up to
// Wait or until it goes quiet.
type StdinRequest struct {
	Text       string
	CloseStdin bool
	Wait       time.Duration
}

// StdinResult is the job output produced since the previous write_stdin read.
// Output starts at raw log Offset; OmittedBytes newer than that read but
// before Offset were left out to bound the result, and logs pages them.
type StdinResult struct {
	Job          Job
	Output       string
	Offset       int64
	NextOffset   int64
	OmittedBytes int64
	StdinClosed  bool
}

// ChildPatch is a finished general task's exported change: its verified
// patch artifact and the commit the patch is relative to.
type ChildPatch struct {
	Job Job
	// Repo is the parent repository root the child's worktree came from.
	Repo       string
	BaseCommit string
	Patch      Artifact
}
type Controller interface {
	StartTask(context.Context, TaskSpec) (Job, error)
	StartBash(context.Context, BashSpec) (Job, error)
	Jobs(context.Context, string, int) (JobPage, error)
	Wait(context.Context, []string, time.Duration) ([]Job, error)
	Cancel(context.Context, string) (Job, error)
	Logs(context.Context, string, int64, int) (LogPage, error)
	Cleanup(context.Context, string) (Job, error)
	// JobByName resolves a service name: the running job with that name,
	// else the most recently started one.
	JobByName(context.Context, string) (Job, error)
	// WriteStdin writes to a running bash job's input and returns the output
	// it produced since the previous WriteStdin on that job.
	WriteStdin(context.Context, string, StdinRequest) (StdinResult, error)
	// ChildPatch returns a settled task's patch for integration into the
	// parent workspace.
	ChildPatch(context.Context, string) (ChildPatch, error)
}
type Fact struct {
	Kind    string
	RunID   string
	Payload json.RawMessage
}
type Event struct {
	Kind            string // job | job_output | queue | compaction | message
	Job             *Job
	Queue           *QueueItem
	Message         *AgentMessage
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

// AgentMessage is one delivered agent-to-agent message as the wire reports it.
// From and To are "parent" or a child's job id.
type AgentMessage struct {
	ID       string `json:"id"`
	From     string `json:"from"`
	To       string `json:"to"`
	Body     string `json:"body"`
	Delivery string `json:"delivery"` // mailbox | reply (handed to the recipient's awaiting message call)
	QueueID  string `json:"queueId,omitempty"`
}
type Ceiling struct {
	Mode        string
	Tools       []string
	Overrides   map[string]string // allow | deny | prompt; no credentials
	Interactive map[string]string // launch-time session choices
	// CloudRules is the launch-time cloud context policy. Absent in records
	// written before M5, which means no grants (classification still applies).
	CloudRules []cloudcmd.Rule `json:"cloud_rules,omitempty"`
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
