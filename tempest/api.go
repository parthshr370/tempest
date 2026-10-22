// Package tempest provides durable, embeddable coding-agent sessions.
package tempest

import (
	"context"
	"errors"
	"log/slog"

	"go.harness.dev/harness/internal/adapter"
	"go.harness.dev/harness/internal/agent"
	types "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/hub"
	"go.harness.dev/harness/internal/mcpconfig"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/session"
	"go.harness.dev/harness/internal/session/journal"
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

// StackConfig supplies an already resolved agent-stack recipe.
type StackConfig = session.StackConfig

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

// Prompt is a user message and optional images.
type Prompt struct {
	Text  string
	Media []Media
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
