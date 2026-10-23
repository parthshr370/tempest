package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/work"
)

// StreamFn is the loop's provider entry. It never throws: failures are encoded
// in the returned stream as error/aborted terminal events.
type StreamFn func(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream

// ToolExecutionMode configures how tool calls from one assistant message run.
type ToolExecutionMode string

const (
	ExecSequential ToolExecutionMode = "sequential"
	ExecParallel   ToolExecutionMode = "parallel"
)

// QueueMode controls how many queued messages are injected at drain points.
type QueueMode string

const (
	QueueAll        QueueMode = "all"
	QueueOneAtATime QueueMode = "one-at-a-time"
)

// ThinkingLevel is the model reasoning level requested for future turns.
type ThinkingLevel string

const (
	ThinkingOff     ThinkingLevel = "off"
	ThinkingMinimal ThinkingLevel = "minimal"
	ThinkingLow     ThinkingLevel = "low"
	ThinkingMedium  ThinkingLevel = "medium"
	ThinkingHigh    ThinkingLevel = "high"
	ThinkingXHigh   ThinkingLevel = "xhigh"
)

// AgentToolResult is the final or partial result produced by a tool.
type AgentToolResult struct {
	Content []types.ContentBlock
	Details json.RawMessage
	// IsError preserves protocol-level failures without discarding their content.
	IsError   bool
	Terminate bool
}

// AgentToolUpdateCallback streams partial tool results while Execute runs.
type AgentToolUpdateCallback func(AgentToolResult)

// AgentTool is a runtime tool definition used by the agent loop.
type AgentTool struct {
	types.Tool
	Label            string
	PrepareArguments func(args json.RawMessage) json.RawMessage
	ExecutionMode    ToolExecutionMode
	Execute          func(ctx context.Context, toolCallID string, params json.RawMessage, onUpdate AgentToolUpdateCallback) (AgentToolResult, error)
}

// AgentContext is the context snapshot passed into the low-level agent loop.
type AgentContext struct {
	SystemPrompt string
	Messages     []types.Message
	Tools        []AgentTool
}

// AgentState is the public observable agent state. Runtime code copies slices on
// assignment; this type is only the plain snapshot shape.
type AgentState struct {
	SystemPrompt     string
	Model            types.Model
	ThinkingLevel    ThinkingLevel
	Tools            []AgentTool
	Messages         []types.Message
	IsStreaming      bool
	StreamingMessage types.Message
	PendingToolCalls map[string]struct{}
	ErrorMessage     string
}

// BeforeToolCallResult is returned by BeforeToolCall.
type BeforeToolCallResult struct {
	Block      bool
	Reason     string
	Permission *PermissionOutcome
}

// PermissionOutcome carries the gate's decision before the tool starts.
type PermissionOutcome struct {
	Allow    bool
	Reason   string
	Source   string
	Prompted bool
	Wait     time.Duration
	AskID    string
}

// AfterToolCallResult is a field-by-field override for a completed tool result.
// Nil pointer fields and nil Content mean "omitted; keep the original".
type AfterToolCallResult struct {
	Content    []types.ContentBlock
	Details    json.RawMessage
	HasDetails bool
	IsError    *bool
	Terminate  *bool
}

// BeforeToolCallContext is passed to BeforeToolCall after argument validation.
type BeforeToolCallContext struct {
	AssistantMessage types.AssistantMessage
	ToolCall         types.ContentBlock
	Args             any
	Context          AgentContext
}

// AfterToolCallContext is passed to AfterToolCall before result events are emitted.
type AfterToolCallContext struct {
	AssistantMessage types.AssistantMessage
	ToolCall         types.ContentBlock
	Args             any
	Result           AgentToolResult
	IsError          bool
	Context          AgentContext
}

// ShouldStopAfterTurnContext is passed to ShouldStopAfterTurn.
type ShouldStopAfterTurnContext struct {
	Message     types.AssistantMessage
	ToolResults []types.ToolResultMessage
	Context     AgentContext
	NewMessages []types.Message
}

// AgentLoopTurnUpdate replaces selected state before the next provider request.
type AgentLoopTurnUpdate struct {
	Context       *AgentContext
	Model         *types.Model
	ThinkingLevel ThinkingLevel
}

// AgentLoopConfig configures the low-level loop. Hooks return plain values; the
// loop edge recovers panics. The embedded [Budget] caps the run.
type AgentLoopConfig struct {
	types.SimpleStreamOptions
	Budget
	Model         types.Model
	ToolExecution ToolExecutionMode

	ConvertToLlm        func(messages []types.Message) []types.Message
	TransformContext    func(ctx context.Context, messages []types.Message) []types.Message
	GetAPIKey           func(provider string) string
	ShouldStopAfterTurn func(c ShouldStopAfterTurnContext) bool
	PrepareNextTurn     func(c ShouldStopAfterTurnContext) *AgentLoopTurnUpdate
	PrepareNextTurnE    func(context.Context, ShouldStopAfterTurnContext) (*AgentLoopTurnUpdate, error)
	QueueSource         work.QueueSource
	Work                work.RunHooks
	Resume              bool
	bindings            *runtimeBindings
	GetSteeringMessages func() []types.Message
	GetFollowUpMessages func() []types.Message
	BeforeToolCall      func(ctx context.Context, c BeforeToolCallContext) *BeforeToolCallResult
	AfterToolCall       func(ctx context.Context, c AfterToolCallContext) *AfterToolCallResult
}

// Budget caps one run. Zero fields are unlimited. The loop checks the budget
// at turn boundaries (after a turn's tools finish, before the next model
// call), so a single long turn can overrun MaxWallTime; Abort or a context
// deadline is the hard stop.
type Budget struct {
	// MaxTurns caps model calls in the run.
	MaxTurns int
	// MaxCostUSD caps model-call cost, including delegated child runs.
	MaxCostUSD float64
	// MaxWallTime caps elapsed time since the run started.
	MaxWallTime time.Duration
}

// ErrUnknownPrice is returned when a cost budget is set for a model whose
// price is unknown and whose route does not report billed cost: the budget
// could never trip, so the run must not start.
var ErrUnknownPrice = errors.New("model price is unknown")

// Validate rejects negative or non-finite limits and a cost budget that cannot
// be enforced for model. A route that reports billed cost is enforceable
// without a catalog price.
func (b Budget) Validate(model types.Model) error {
	if math.IsNaN(b.MaxCostUSD) || math.IsInf(b.MaxCostUSD, 0) {
		return fmt.Errorf("budget max cost must be finite: %g", b.MaxCostUSD)
	}
	if b.MaxTurns < 0 || b.MaxCostUSD < 0 || b.MaxWallTime < 0 {
		return fmt.Errorf("budget limits must not be negative (max turns %d, max cost %g, max wall time %s)", b.MaxTurns, b.MaxCostUSD, b.MaxWallTime)
	}
	if b.MaxCostUSD > 0 && model.Cost.Unknown && !model.ReportsBilledCost() {
		return fmt.Errorf("cost budget $%g cannot be enforced for %s/%s: %w", b.MaxCostUSD, model.Provider, model.ID, ErrUnknownPrice)
	}
	return nil
}

// StopReason says why a run ended.
type StopReason string

const (
	// StopCompleted: the model produced a turn with no tool calls and no
	// queued messages were waiting (or ShouldStopAfterTurn asked to stop).
	StopCompleted StopReason = "completed"
	// StopAborted: Abort was called or the caller's context was canceled.
	StopAborted StopReason = "aborted"
	// StopMaxTurns, StopMaxCost, StopMaxWallTime: a [Budget] limit tripped.
	StopMaxTurns    StopReason = "max_turns"
	StopMaxCost     StopReason = "max_cost"
	StopMaxWallTime StopReason = "max_wall_time"
	// StopError: the provider failed, a listener failed, or the loop
	// panicked. [RunResult.Err] carries the cause.
	StopError StopReason = "error"
)

// RunResult is how one run ended. Prompt and Continue return it, and the run's
// agent_end event carries it.
type RunResult struct {
	// RunID and ElapsedMS are populated by the durable Session wrapper.
	RunID      string
	ElapsedMS  int64
	StopReason StopReason
	// Err is set only when StopReason is StopError.
	Err error
	// Turns counts model calls made in this run.
	Turns int
	// Usage sums this run's model calls and delegated child calls. Cost.Unknown
	// is true when any call's price was unknown, so the total is a lower bound.
	Usage types.Usage
}

// AgentEventType is the discriminant for AgentEvent.
type AgentEventType string

const (
	EventAgentStart          AgentEventType = "agent_start"
	EventAgentEnd            AgentEventType = "agent_end"
	EventTurnStart           AgentEventType = "turn_start"
	EventTurnEnd             AgentEventType = "turn_end"
	EventMessageStart        AgentEventType = "message_start"
	EventMessageUpdate       AgentEventType = "message_update"
	EventMessageEnd          AgentEventType = "message_end"
	EventToolExecutionStart  AgentEventType = "tool_execution_start"
	EventToolPermission      AgentEventType = "tool_permission"
	EventToolExecutionUpdate AgentEventType = "tool_execution_update"
	EventToolExecutionEnd    AgentEventType = "tool_execution_end"
	// EventUsage reports a delegated model call without adding its transcript.
	EventUsage AgentEventType = "usage"
	EventWork  AgentEventType = "work"
)

// AgentEvent is a tagged struct over the event union. Only fields relevant to
// Type are populated, except Seq and At, which every event carries.
type AgentEvent struct {
	Type AgentEventType
	// Seq is assigned at emit time and strictly increases in delivery order:
	// per Agent across all its runs, or per stream for [AgentLoop].
	Seq uint64
	// At is the emit time.
	At                    time.Time
	Message               types.Message
	Messages              []types.Message
	ToolResults           []types.ToolResultMessage
	AssistantMessageEvent *types.StreamEvent
	ToolCallID            string
	ToolName              string
	Args                  any
	Result                any
	PartialResult         any
	IsError               bool
	Permission            *PermissionOutcome
	Work                  *work.Event
	QueueID               string
	UsageSource           string
	ChildSessionID        string
	UsageRunID            string
	Resume                bool
	// Model and Usage are set on usage events for delegated model calls.
	Model string
	// RunResult is set on agent_end; Usage is also set on usage events.
	RunResult
}

// AgentStart constructs an agent_start event. prompts are the run's prompt
// messages (nil when the run continues an existing transcript).
func AgentStart(prompts []types.Message) AgentEvent {
	return AgentEvent{Type: EventAgentStart, Messages: prompts}
}

// AgentEnd constructs an agent_end event carrying the run's new messages and
// how the run ended.
func AgentEnd(messages []types.Message, result RunResult) AgentEvent {
	return AgentEvent{Type: EventAgentEnd, Messages: messages, RunResult: result}
}

// TurnStart constructs a turn_start event.
func TurnStart() AgentEvent { return AgentEvent{Type: EventTurnStart} }

// TurnEnd constructs a turn_end event.
func TurnEnd(message types.Message, results []types.ToolResultMessage) AgentEvent {
	return AgentEvent{Type: EventTurnEnd, Message: message, ToolResults: results}
}

// MessageStart constructs a message_start event.
func MessageStart(message types.Message) AgentEvent {
	return AgentEvent{Type: EventMessageStart, Message: message}
}

// MessageUpdate constructs a message_update event.
func MessageUpdate(message types.Message, assistantEvent *types.StreamEvent) AgentEvent {
	return AgentEvent{Type: EventMessageUpdate, Message: message, AssistantMessageEvent: assistantEvent}
}

// MessageEnd constructs a message_end event.
func MessageEnd(message types.Message) AgentEvent {
	return AgentEvent{Type: EventMessageEnd, Message: message}
}

// ToolExecutionStart constructs a tool_execution_start event.
func ToolExecutionStart(id, name string, args any) AgentEvent {
	return AgentEvent{Type: EventToolExecutionStart, ToolCallID: id, ToolName: name, Args: args}
}

// ToolExecutionUpdate constructs a tool_execution_update event.
func ToolExecutionUpdate(id, name string, args, partial any) AgentEvent {
	return AgentEvent{Type: EventToolExecutionUpdate, ToolCallID: id, ToolName: name, Args: args, PartialResult: partial}
}

// ToolExecutionEnd constructs a tool_execution_end event.
func ToolExecutionEnd(id, name string, result any, isError bool) AgentEvent {
	return AgentEvent{Type: EventToolExecutionEnd, ToolCallID: id, ToolName: name, Result: result, IsError: isError}
}

// AgentEventSink consumes events in order. The loop calls it synchronously and
// waits for it to return before continuing, which is what lets a listener
// record tool_execution_start before the tool runs.
type AgentEventSink func(ctx context.Context, ev AgentEvent) error
