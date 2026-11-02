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
	"go.harness.dev/harness/internal/hooks"
	"go.harness.dev/harness/internal/outputschema"
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
	Content           []types.ContentBlock
	Details           json.RawMessage
	StructuredContent json.RawMessage
	Usage             json.RawMessage
	Control           string
	// IsError preserves protocol-level failures without discarding their content.
	IsError   bool
	Terminate bool
	// Paths are the workspace files and directories this call read or listed
	// successfully. Nested context discovery consumes them; they are never
	// sent to the model or journaled with the result.
	Paths []PathEvent
}

// PathEvent is a file tool's normalized "path read or listed" fact: an
// absolute host path, whether it is a listed directory, and whether the
// call showed the whole file (not a range, a cut page or an outline).
type PathEvent struct {
	Path  string
	Dir   bool
	Whole bool
}

// AgentToolUpdateCallback streams partial tool results while Execute runs.
type AgentToolUpdateCallback func(AgentToolResult)

// AgentTool is a runtime tool definition used by the agent loop.
type AgentTool struct {
	types.Tool
	Definition       ToolDefinition
	Label            string
	OutputSchema     json.RawMessage
	PrepareArguments func(args json.RawMessage) json.RawMessage
	ExecutionMode    ToolExecutionMode
	Execute          func(ctx context.Context, toolCallID string, params json.RawMessage, onUpdate AgentToolUpdateCallback) (AgentToolResult, error)
	// Polls marks a tool whose job is to observe changing state, such as
	// waiting for a job or reading its new output: the same call returning
	// the same result is what polling looks like, so the loop guard never
	// reports it while it succeeds (see loopguard.go). A failure is not
	// polling: both guards still count it.
	Polls bool
	// ReadsWorkspace reports whether a call with these arguments only reads
	// the workspace: it changes nothing, and its result changes only when the
	// workspace does. The loop guard never takes such a call as progress, and
	// reports one returning the same result sooner than a call that may see
	// state outside the workspace. Nil, the default for commands, MCP and
	// extension tools, means every call may change things and see such state.
	ReadsWorkspace func(arguments json.RawMessage) bool
	// Deferrable marks a tool whose schema the loop may hold back behind
	// tool_search when the tool set is large (see [ToolSearch]). MCP and
	// extension tools set it; built-in tools do not.
	Deferrable bool
	// unloaded marks a deferred tool in a request's tool view that the
	// model has not loaded: it is not sent to the provider and a call to it
	// fails (see toolsearch.go).
	unloaded bool
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
	Definition       ToolDefinition
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
	Handoff     *string
	Overflow    bool
	// Model is the model the run is on: its primary, or the backup it
	// switched to and keeps until it ends. Work between turns, such as
	// compaction, goes to it.
	Model types.Model
}

// AgentLoopTurnUpdate replaces selected state before the next provider request.
type AgentLoopTurnUpdate struct {
	Context       *AgentContext
	Model         *types.Model
	ThinkingLevel ThinkingLevel
	Stop          bool
}

// AgentLoopConfig configures the low-level loop. Hooks return plain values; the
// loop edge recovers panics. The embedded [Budget] caps the run.
type AgentLoopConfig struct {
	types.SimpleStreamOptions
	Budget
	Model types.Model
	// FallbackModels are the backup models behind the stream function, in
	// chain order. The loop uses them to keep a recorded switch on resume.
	FallbackModels []types.Model
	// ModelRole names the model role the loop runs on (default, plan or
	// smol) for model-request hooks; empty means default.
	ModelRole     string
	ToolExecution ToolExecutionMode
	Hooks         *hooks.Runner
	Child         bool
	// Trace starts trace spans for runs; see [AgentOptions.Trace].
	Trace      bool
	Extensions ExtensionRuntime

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
	// OutputSchema, when set, is the contract for the run's final answer:
	// the provider is asked for native structured output, and the loop
	// validates the final text, granting one correction turn before the run
	// ends with [StopOutputInvalid].
	OutputSchema *outputschema.Schema
	// Reminders are added at the tail of model requests on their cadence
	// and conditions; see [Reminder].
	Reminders []Reminder
	// LoopThresholds configures the loop guard; nil means
	// [DefaultLoopThresholds].
	LoopThresholds *LoopThresholds
	// ToolSearch configures deferred tool loading; nil means the zero
	// [ToolSearch], automatic deferral at the default threshold.
	ToolSearch *ToolSearch
}

// Budget caps one run. Zero fields are unlimited. The loop checks the budget
// at turn boundaries (after a turn's tools finish, before the next model
// call), so a single long turn can overrun MaxWallTime; Abort or a context
// deadline is the hard stop.
//
// Cost and token caps are checked before each model request against the
// exact usage already reported, so the last admitted request can overrun its
// cap by its own size. They count delegated child runs and compaction
// requests. The tool-call cap is checked before a response's tool calls run
// and never overrun; see [StopMaxToolCalls].
type Budget struct {
	// MaxTurns caps model calls in the run.
	MaxTurns int
	// MaxCostUSD caps model-call cost, including delegated child runs.
	MaxCostUSD float64
	// MaxWallTime caps elapsed time since the run started.
	MaxWallTime time.Duration
	// MaxInputTokens caps the prompt tokens of the run's model calls:
	// uncached input plus cache reads and cache writes.
	MaxInputTokens int
	// MaxOutputTokens caps the output tokens of the run's model calls,
	// reasoning included.
	MaxOutputTokens int
	// MaxTotalTokens caps input plus output tokens as counted above.
	MaxTotalTokens int
	// MaxToolCalls caps the tool calls the run admits for execution.
	MaxToolCalls int
}

// ErrUnknownPrice is returned when a cost budget is set for a model whose
// price is unknown and whose route does not report billed cost: the budget
// could never trip, so the run must not start.
var ErrUnknownPrice = errors.New("model price is unknown")

// ErrUnknownUsage ends a run with a token budget when a completed model
// response reports no tokens: an unmeasured call is not a free one.
var ErrUnknownUsage = errors.New("model response reported no token usage")

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
	if b.MaxInputTokens < 0 || b.MaxOutputTokens < 0 || b.MaxTotalTokens < 0 || b.MaxToolCalls < 0 {
		return fmt.Errorf("budget limits must not be negative (max input tokens %d, max output tokens %d, max total tokens %d, max tool calls %d)", b.MaxInputTokens, b.MaxOutputTokens, b.MaxTotalTokens, b.MaxToolCalls)
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
	// StopMaxTurns, StopMaxCost, StopMaxWallTime, StopMaxInputTokens,
	// StopMaxOutputTokens, StopMaxTotalTokens: a [Budget] limit tripped.
	StopMaxTurns        StopReason = "max_turns"
	StopMaxCost         StopReason = "max_cost"
	StopMaxWallTime     StopReason = "max_wall_time"
	StopMaxInputTokens  StopReason = "max_input_tokens"
	StopMaxOutputTokens StopReason = "max_output_tokens"
	StopMaxTotalTokens  StopReason = "max_total_tokens"
	// StopMaxToolCalls: a response asked for more tool calls than
	// [Budget.MaxToolCalls] had left. None of them ran; each got an error
	// result, so the transcript stays valid for Continue.
	StopMaxToolCalls StopReason = "max_tool_calls"
	// StopError: the provider failed, a listener failed, or the loop
	// panicked. [RunResult.Err] carries the cause.
	StopError StopReason = "error"
	// StopOutputInvalid: the final answer still failed the run's output
	// schema after the correction turn. [RunResult.Err] carries why.
	StopOutputInvalid StopReason = "output_invalid"
	// StopRepeatedFailure: the model sent the same tool call, with the same
	// arguments, until it had failed with the same error RepeatedFailureLimit
	// times in a row. [RunResult.Err] names the call and its error.
	StopRepeatedFailure StopReason = "repeated_failure"
	// StopLoopDetected: the loop guard found the same loop again after it
	// had told the model about it (see loopguard.go). [RunResult.Err] is a
	// *[LoopError] naming the loop.
	StopLoopDetected StopReason = "loop_detected"
)

// RunResult is how one run ended. Prompt and Continue return it, and the run's
// agent_end event carries it.
type RunResult struct {
	// RunID and ElapsedMS are populated by the durable Session wrapper.
	RunID      string
	ElapsedMS  int64
	StopReason StopReason
	// Err is set when StopReason is StopError, StopOutputInvalid,
	// StopRepeatedFailure or StopLoopDetected.
	Err error
	// Turns counts model calls made in this run.
	Turns int
	// Usage sums this run's model calls and delegated child calls. Cost.Unknown
	// is true when any call's price was unknown, so the total is a lower bound.
	Usage types.Usage
	// Output is the final answer validated against the run's output schema,
	// in compact JSON. It is nil when the run had no schema or the answer
	// never validated.
	Output json.RawMessage
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
	// EventUsage reports a usage receipt without adding its transcript: a
	// delegated or compaction model call, or a response's admitted tool calls.
	EventUsage AgentEventType = "usage"
	EventWork  AgentEventType = "work"
	EventHook  AgentEventType = "hook"
	// EventModelFallback reports that the run switched to a backup model
	// because the model it was using is unavailable. The run stays on the
	// backup until it ends. Fallback describes the switch; the backup is
	// called once every listener has returned.
	EventModelFallback AgentEventType = "model_fallback"
	// EventModelRequest is emitted just before a model request is sent;
	// Request describes it. The message events of its response follow.
	EventModelRequest AgentEventType = "model_request"
	// EventModelRetry reports a failed attempt that is sent again: Message is
	// the discarded attempt (its error and any usage), Attempt its 1-based
	// ordinal on its model and FailedAt when the stream saw it fail. Without
	// Fallback the same model is due the request RetryDelay after FailedAt.
	// With Fallback the model was unavailable and the request goes to
	// Fallback.To, whose model_fallback event follows. When the attempt had
	// opened a message, that message's message_end follows this event. A
	// model_attempt event follows when the next attempt is sent.
	EventModelRetry AgentEventType = "model_retry"
	// EventModelAttempt reports that the request was sent again after a
	// model_retry: Attempt is the attempt's 1-based ordinal on its model and
	// SentAt when it was sent. A backoff or switch cancelled first reports
	// none.
	EventModelAttempt AgentEventType = "model_attempt"
	// EventLoopGuard reports that the loop guard found a loop: Loop says
	// which and whether the model was nudged or the run stops. It precedes
	// the message that carries the notice.
	EventLoopGuard AgentEventType = "loop_guard"
)

// ModelRequest describes one logical model request: the model, the request
// settings and the context sent. Messages and SystemPrompt are read-only.
type ModelRequest struct {
	Model        types.Model
	SystemPrompt string
	Messages     []types.Message
	MaxTokens    int
	Temperature  *float64
	Reasoning    string
}

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
	Preparing             bool
	Result                any
	PartialResult         any
	IsError               bool
	Permission            *PermissionOutcome
	Work                  *work.Event
	Definition            ToolDefinition
	Hook                  *hooks.Report
	Fallback              *types.ModelFallback
	Request               *ModelRequest
	Loop                  *LoopDetection
	Attempt               int
	RetryDelay            time.Duration
	FailedAt              time.Time
	SentAt                time.Time
	QueueID               string
	UsageSource           string
	ChildSessionID        string
	UsageRunID            string
	Resume                bool
	// Reminders are the request-scope reminders sent with the request
	// whose final message_end this is, or the conversation-scope reminder
	// whose message this is.
	Reminders []ReminderFire
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
