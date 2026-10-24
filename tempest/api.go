// Package tempest provides durable, embeddable coding-agent sessions.
package tempest

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"go.harness.dev/harness/internal/adapter"
	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/cloudcmd"
	types "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/hooks"
	"go.harness.dev/harness/internal/hub"
	"go.harness.dev/harness/internal/jobs"
	"go.harness.dev/harness/internal/lsp"
	"go.harness.dev/harness/internal/mcpconfig"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/session"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/work"
)

// Options selects a new or existing journal and its runtime configuration.
type Options struct {
	SessionDir string
	Cwd        string
	New        *NewSession
	Resume     *ResumeSession
	Provider   ProviderConfig
	Prompter   Prompter
	Logger     *slog.Logger
	// FauxScript selects a hermetic JSON response script instead of a network provider.
	FauxScript string
	// Stack optionally supplies resolved models, providers, discovery and tools.
	// It applies to both new and resumed sessions; Open owns its live permission hook.
	Stack *StackConfig
	// LSP replaces the host's language-server configuration; nil keeps the
	// resolved host default (Stack.LSP, or the environment and settings when
	// Stack is nil). Open resolves it once; when Enabled is true the root
	// session gets a manager that starts each server on its first query.
	LSP *LSPConfig
	// CloudRules replaces the host's exact kube-context rules; nil keeps the
	// host default and a non-nil empty slice selects no scoped rules.
	CloudRules []CloudRule
	// Hooks replaces operator hook settings. Project commands still require explicit trust.
	Hooks *HookSettings
}

// NewSession configures a new conversation.
type NewSession struct {
	Model          string
	SystemPrompt   string
	Tools          []Tool
	Budget         Budget
	PermissionMode PermissionMode
	// MCPServers replaces host defaults; a non-nil empty slice disables MCP.
	MCPServers []MCPServerConfig
}

// ResumeSession identifies an existing conversation; runtime settings are optional.
type ResumeSession struct {
	SessionID      string
	Path           string
	Model          string
	Budget         Budget
	PermissionMode PermissionMode
	// MCPServers is runtime-only; credentials aren't restored from the journal.
	MCPServers []MCPServerConfig
}

// ProviderConfig configures provider credentials and endpoint routing.
type ProviderConfig = adapter.RoutingConfig

// LSPConfig selects trusted language-server definitions for a session.
type LSPConfig = lsp.Config

// LSPServerConfig replaces one built-in language-server route.
type LSPServerConfig = lsp.ServerConfig

// CloudRule is one operator kube-context rule for kubectl and helm.
type CloudRule = cloudcmd.Rule

// StackConfig supplies an already resolved agent-stack recipe.
type StackConfig = session.StackConfig

// HookSettings configures command hooks and explicit project trust.
type HookSettings = hooks.Settings

// HookGroup matches lifecycle commands against Tempest's snake_case tool names.
type HookGroup = hooks.Group

// HookCommand is a shell command with an optional timeout in seconds.
type HookCommand = hooks.Command

// Tool is an embeddable tool implementation.
type Tool = agent.AgentTool

// MCPServerConfig is an operator-selected MCP endpoint.
type MCPServerConfig = mcpconfig.Server

// ErrMCPConfigConflict rejects changes to a live session's MCP endpoints.
var ErrMCPConfigConflict = errors.New("MCP configuration differs from live session")

// Message is one transcript message.
type Message = types.Message

// Media is an image block attached to a prompt.
type Media = types.ContentBlock

// ContentBlock is an ordered text, image or tool-call block.
type ContentBlock = types.ContentBlock

// ToolResult is a complete or partial tool response.
type ToolResult = agent.AgentToolResult

// ToolUpdateCallback publishes partial tool responses during execution.
type ToolUpdateCallback = agent.AgentToolUpdateCallback

// ToolDefinition is the model-visible name, description and JSON schema.
type ToolDefinition = types.Tool

// Prompt is a user message and optional images. OutputSchema, when set, is a
// JSON Schema the run's final answer must satisfy: the provider is asked for
// native structured output, the answer is validated locally with one
// correction turn, and a valid answer is returned in [RunResult].Output. A
// run whose answer never validates stops with "output_invalid".
type Prompt struct {
	Text         string
	Media        []Media
	OutputSchema json.RawMessage
}

// WatchOptions bounds the live queue; Approver is interpreted by the server.
type WatchOptions struct {
	Buffer   int
	Approver bool
	// Replay puts full history and the in-flight message's original chunks in
	// Snapshot.History, reserving that many extra live queue slots for delivery.
	// HistoryLen still counts only completed history. Don't append Streaming
	// again when reconstructing this replay snapshot.
	Replay bool
}

// Prompter resolves permission requests. Answer can also resolve a pending request;
// the first answer wins. With no Prompter, prompt decisions deny immediately.
// Approve must return when its context is cancelled, including when Answer wins.
type Prompter interface {
	Approve(context.Context, Ask) (Choice, error)
}

// Choice is a permission option; an empty choice cancels the request.
type Choice = permission.Choice

// PermissionMode determines the default tool policy.
type PermissionMode = permission.Mode

// ToolKind is the ACP category of a tool.
type ToolKind = hub.ToolKind

// Ask describes a pending approval request.
type Ask = hub.Ask

// Option is one selectable permission choice.
type Option = hub.Option

// Snapshot is an atomic reduced view. Live updates start after Seq.
type Snapshot = hub.Snapshot

// Update is an ACP or Tempest event. A final tempest/gap update signals overflow;
// queued updates preceding that marker may have been discarded. Re-watch then.
type Update = hub.Update

// RunState describes the current run.
type RunState = hub.RunState

// RunSummary describes the most recent durable run.
type RunSummary = hub.RunSummary

// StreamingMessage holds the currently streaming assistant message.
type StreamingMessage = hub.StreamingMessage

// ToolState describes a tool call in the current run.
type ToolState = hub.ToolState

// UsageState holds cumulative and latest-call usage.
type UsageState = hub.UsageState

// InterruptedTool is a tool with an unknown outcome after process death.
type InterruptedTool = journal.InterruptedTool

// RunResult is the terminal result of a run.
type RunResult = agent.RunResult

// QueuedInput is text retained until delivered or explicitly withdrawn.
type QueuedInput struct {
	Text string
	Key  string
}

// QueuedItem is a durable queue receipt.
type QueuedItem = work.QueueItem

// Job describes a supervised child or background command.
type Job = work.Job

// JobPage is a bounded listing of job summaries.
type JobPage = work.JobPage

// LogPage contains a page of raw output and byte cursors.
type LogPage = work.LogPage

// Artifact identifies a durable host-local result.
type Artifact = work.Artifact

// RunDetail is a read-only reconstruction of a durable run.
type RunDetail = journal.RunDetail

// StopReason is the exact reason the loop stopped.
type StopReason = agent.StopReason

// Budget bounds a run; zero fields are unlimited.
type Budget = agent.Budget

// Usage records tokens and cost, including unknown prices.
type Usage = types.Usage

var (
	// ErrRunActive means a session already owns a run.
	ErrRunActive = agent.ErrRunActive
	// ErrSessionLocked means another writer owns the journal.
	ErrSessionLocked = journal.ErrSessionLocked
	// ErrStaleEpoch means history changed since the caller's snapshot.
	ErrStaleEpoch = hub.ErrStaleEpoch
	// ErrNoPendingAsk means the request has already resolved or never existed.
	ErrNoPendingAsk = errors.New("no pending permission ask")
	// ErrClosed means the session has been closed.
	ErrClosed = errors.New("session closed")
	// ErrInvalidOutputSchema means a prompt's OutputSchema is not a usable
	// JSON Schema; no run starts.
	ErrInvalidOutputSchema = errors.New("invalid output schema")
	// ErrQueueFull means the durable input queue is at capacity.
	ErrQueueFull = session.ErrQueueFull
	// ErrQueueConflict means an idempotency key was reused with different input.
	ErrQueueConflict = session.ErrQueueConflict
	// ErrQueueNotFound means the queue id doesn't belong to this session.
	ErrQueueNotFound = session.ErrQueueNotFound
	// ErrQueueTooLate means delivery already claimed or consumed the item.
	ErrQueueTooLate = session.ErrQueueTooLate
	// ErrJobNotFound means the job doesn't belong to this session.
	ErrJobNotFound = jobs.ErrJobNotFound
	// ErrJobLimit means a supervision or delivery capacity is exhausted.
	ErrJobLimit = jobs.ErrJobLimit
	// ErrJobNotCleanupable means retained work can't safely be removed.
	ErrJobNotCleanupable = jobs.ErrJobNotCleanupable
	// ErrRecoveryRequired means Continue or explicit cancellation must settle children first.
	ErrRecoveryRequired = jobs.ErrRecoveryRequired
	// ErrInvalidQueueInput rejects malformed queue text or keys.
	ErrInvalidQueueInput = session.ErrInvalidQueueInput
	// ErrInvalidJobInput rejects malformed job ids, paging or wait bounds.
	ErrInvalidJobInput = jobs.ErrInvalidInput
)

// Session owns one journal, agent and reduced event stream.
type Session struct{ core *sessionCore }

// Open creates or resumes a session. Exactly one of New and Resume is required.
func Open(ctx context.Context, o Options) (*Session, error) { return open(ctx, o) }

// ID returns the durable session identifier.
func (s *Session) ID() string { return s.core.id }

// Begin registers a prompt before starting its loop. Cancel can see the run as
// soon as Begin returns; an earlier Cancel while idle doesn't affect it.
func (s *Session) Begin(ctx context.Context, p Prompt) (*Run, error) { return s.begin(ctx, &p) }

// BeginContinue registers a run without adding a user message.
func (s *Session) BeginContinue(ctx context.Context) (*Run, error) { return s.begin(ctx, nil) }

// Prompt waits for a newly admitted user message to finish.
func (s *Session) Prompt(ctx context.Context, p Prompt) (RunResult, error) {
	run, err := s.Begin(ctx, p)
	if err != nil {
		return RunResult{}, err
	}
	return run.Wait()
}

// Continue resumes the current transcript without adding a user message.
func (s *Session) Continue(ctx context.Context) (RunResult, error) {
	run, err := s.BeginContinue(ctx)
	if err != nil {
		return RunResult{}, err
	}
	return run.Wait()
}

// Cancel idempotently cancels the session-owned run context.
func (s *Session) Cancel() { s.cancel() }

// Watch atomically captures a snapshot and starts bounded live delivery.
func (s *Session) Watch(ctx context.Context, o WatchOptions) (Snapshot, <-chan Update, func()) {
	return s.watch(ctx, o)
}

// Snapshot returns a detached copy of the current reduced view.
func (s *Session) Snapshot() Snapshot { return s.core.hub.Snapshot() }

// History pages transcript updates in the specified history epoch.
func (s *Session) History(epoch, offset, limit int) ([]Update, error) {
	return s.core.hub.History(epoch, offset, limit)
}

// Answer resolves a pending approval; the first answer wins.
func (s *Session) Answer(id string, c Choice) error { return s.answer(id, c) }

// SetBudget validates and installs the budget for future run boundaries.
func (s *Session) SetBudget(b Budget) error { return s.setBudget(b) }

// SetPermissionMode changes the mode used by subsequent permission checks.
func (s *Session) SetPermissionMode(m PermissionMode) error { return s.setPermissionMode(m) }

// Close prevents admission, cancels and joins the run, then closes the journal.
func (s *Session) Close(ctx context.Context) error { return s.close(ctx) }

// Steer retains input for the next safe checkpoint; idle input waits for a run.
func (s *Session) Steer(ctx context.Context, in QueuedInput) (QueuedItem, error) {
	return s.enqueue(ctx, work.Steer, in)
}

// FollowUp retains input for quiescence; it never starts a run by itself.
func (s *Session) FollowUp(ctx context.Context, in QueuedInput) (QueuedItem, error) {
	return s.enqueue(ctx, work.FollowUp, in)
}

// CancelQueued explicitly withdraws a pending durable input.
func (s *Session) CancelQueued(ctx context.Context, id string) error {
	if err := s.checkOpen(); err != nil {
		return err
	}
	return s.core.queues.Cancel(ctx, id)
}

// Jobs returns bounded summaries with a stable id cursor.
func (s *Session) Jobs(ctx context.Context, cursor string, limit int) (JobPage, error) {
	if err := s.checkOpen(); err != nil {
		return JobPage{}, err
	}
	return s.core.jobs.Jobs(ctx, cursor, limit)
}

// WaitJobs waits for any requested job; cancelling the wait doesn't cancel jobs.
func (s *Session) WaitJobs(ctx context.Context, ids []string, timeout time.Duration) ([]Job, error) {
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	return s.core.jobs.Wait(ctx, ids, timeout)
}

// CancelJob durably requests cancellation of one session-owned job.
func (s *Session) CancelJob(ctx context.Context, id string) (Job, error) {
	if err := s.checkOpen(); err != nil {
		return Job{}, err
	}
	return s.core.jobs.Cancel(ctx, id)
}

// JobLogs pages the bounded output of a background command.
func (s *Session) JobLogs(ctx context.Context, id string, offset int64, limit int) (LogPage, error) {
	if err := s.checkOpen(); err != nil {
		return LogPage{}, err
	}
	return s.core.jobs.Logs(ctx, id, offset, limit)
}

// CleanupJob removes only safely exported, owned retained work.
func (s *Session) CleanupJob(ctx context.Context, id string) (Job, error) {
	if err := s.checkOpen(); err != nil {
		return Job{}, err
	}
	return s.core.jobs.Cleanup(ctx, id)
}

// Messages returns a copied transcript slice for reporting. Treat its message
// contents as read-only; Snapshot and History provide fully detached wire views.
func (s *Session) Messages() []Message { return s.core.agent.State().Messages }

// LastAssistantText returns the last nonempty assistant text in the latest run.
func (s *Session) LastAssistantText() string {
	s.core.mu.Lock()
	defer s.core.mu.Unlock()
	return s.core.lastText
}

// PermissionChoices returns the four standard ACP approval options.
func PermissionChoices() []Option {
	return []Option{{OptionID: "allow_once", Name: "Allow once", Kind: "allow_once"}, {OptionID: "allow_always", Name: "Allow always", Kind: "allow_always"}, {OptionID: "reject_once", Name: "Reject once", Kind: "reject_once"}, {OptionID: "reject_always", Name: "Reject always", Kind: "reject_always"}}
}
