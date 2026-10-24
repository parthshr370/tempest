package agent

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"time"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/hooks"
	"go.harness.dev/harness/internal/outputschema"
	"go.harness.dev/harness/internal/work"
)

// PendingMessageQueue is the queued steering/follow-up buffer. The Agent mutex
// guards all calls.
//
// Drain returns a copy of the queued messages so the caller owns the slice
// independently of future Enqueue/Clear calls. QueueAll drains the whole queue;
// one-at-a-time hands out only the head, preserving single-message pacing.
type PendingMessageQueue struct {
	messages []types.Message
	Mode     QueueMode
}

// Enqueue appends a message to the queue.
func (q *PendingMessageQueue) Enqueue(m types.Message) {
	q.messages = append(q.messages, m)
}

// HasItems reports whether the queue is non-empty.
func (q *PendingMessageQueue) HasItems() bool {
	return len(q.messages) > 0
}

// Drain removes messages from the head. QueueAll drains everything and resets;
// one-at-a-time returns only the first message and shifts the rest forward.
// Copies the slice on drain so the returned messages are independent of future
// mutations — critical when the loop holds a reference across turns.
func (q *PendingMessageQueue) Drain() []types.Message {
	if q.Mode == QueueAll {
		drained := copyAgentMessages(q.messages)
		q.messages = nil
		return drained
	}

	if len(q.messages) == 0 {
		return nil
	}
	first := q.messages[0]
	if first == nil {
		return nil
	}
	q.messages = append([]types.Message(nil), q.messages[1:]...)
	return []types.Message{first}
}

// Clear empties the queue without returning anything.
func (q *PendingMessageQueue) Clear() {
	q.messages = nil
}

// MutableAgentState is AgentState with runtime-owned fields mutable under Agent.mu.
type MutableAgentState struct {
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

// AgentOptions supplies the runtime configuration with provider-transport fields deferred.
type AgentOptions struct {
	// InitialState seeds the agent's state; nil gives defaults.
	InitialState *AgentState
	// ConvertToLlm filters/reshapes messages before the provider call.
	// When nil, defaults to passing through user/assistant/toolResult roles.
	ConvertToLlm func([]types.Message) []types.Message
	// TransformContext is called before every provider request (e.g. for compaction).
	TransformContext func(ctx context.Context, msgs []types.Message) []types.Message
	// StreamFn is the provider entry point. Required for Prompt/Continue.
	StreamFn StreamFn
	// GetApiKey resolves a provider-specific API key; overrides the ambient key.
	GetApiKey      func(provider string) string
	BeforeToolCall func(ctx context.Context, c BeforeToolCallContext) *BeforeToolCallResult
	AfterToolCall  func(ctx context.Context, c AfterToolCallContext) *AfterToolCallResult
	Hooks          *hooks.Runner
	Child          bool
	// PrepareNextTurn is called after each turn for a stateless config swap.
	PrepareNextTurn func() *AgentLoopTurnUpdate
	// PrepareNextTurnWithContext is the stateful variant — preferred when set;
	// it receives turn context and can inspect the last message + tool results.
	PrepareNextTurnWithContext func(c ShouldStopAfterTurnContext) *AgentLoopTurnUpdate
	PrepareNextTurnE           func(context.Context, ShouldStopAfterTurnContext) (*AgentLoopTurnUpdate, error)
	QueueSource                work.QueueSource
	Work                       work.RunHooks
	// SteeringMode controls how queued steering messages are drained:
	// QueueAll drains everything, QueueOneAtATime hands out one per turn.
	SteeringMode QueueMode
	// FollowUpMode controls follow-up message drain behavior. Defaults to
	// QueueOneAtATime.
	FollowUpMode  QueueMode
	SessionID     string
	ToolExecution ToolExecutionMode
	// Budget caps each run; see [Agent.SetBudget].
	Budget Budget
}

// ErrRunActive is returned by Prompt and Continue while a run is in progress.
// Queue input with Steer or FollowUp, or wait with WaitForIdle.
var ErrRunActive = errors.New("agent: a run is already active")

// activeRun represents one in-flight agent run. done closes when the run
// settles (finishRun); cancel aborts it (Abort). Both always set or always nil
// under Agent.mu.
type activeRun struct {
	done   chan struct{}
	cancel context.CancelFunc
}

// agentListener is a subscriber to agent events. id is a monotonic identifier
// used to remove the listener later (Go closures aren't reliably comparable,
// so a funcval key would be unsafe).
type agentListener struct {
	id uint64
	fn func(ctx context.Context, ev AgentEvent) error
}

// Agent is the stateful wrapper around the low-level loop. It implements the
// public runtime API: Prompt (start a run), Continue (resume from a
// transcript), Abort (cancel the in-flight run), Reset (clear conversation),
// and State (snapshot public observables).
//
// Concurrency: mu guards all mutable state (state, queues, activeRun,
// listeners, budget). A second Prompt/Continue while a run is active returns
// [ErrRunActive]. emitMu serializes event delivery: it is held across
// stamping Seq/At and the listener fan-out, so listeners never run
// concurrently and see Seq in delivery order. mu is released before the
// fan-out, so a listener may call State and the other mu-guarded methods.
// A listener must not start a run (Prompt/Continue return ErrRunActive).
type Agent struct {
	mu            sync.Mutex
	state         MutableAgentState
	listeners     []agentListener
	nextListener  uint64
	steeringQueue PendingMessageQueue
	followUpQueue PendingMessageQueue
	activeRun     *activeRun
	budget        Budget
	bindings      *runtimeBindings
	ended         bool
	outputSchema  *outputschema.Schema

	emitMu sync.Mutex
	// seq is the last Seq handed out; guarded by emitMu.
	seq uint64

	ConvertToLlm               func([]types.Message) []types.Message
	TransformContext           func(ctx context.Context, msgs []types.Message) []types.Message
	StreamFn                   StreamFn
	GetApiKey                  func(provider string) string
	BeforeToolCall             func(ctx context.Context, c BeforeToolCallContext) *BeforeToolCallResult
	AfterToolCall              func(ctx context.Context, c AfterToolCallContext) *AfterToolCallResult
	Hooks                      *hooks.Runner
	Child                      bool
	PrepareNextTurn            func() *AgentLoopTurnUpdate
	PrepareNextTurnWithContext func(c ShouldStopAfterTurnContext) *AgentLoopTurnUpdate
	PrepareNextTurnE           func(context.Context, ShouldStopAfterTurnContext) (*AgentLoopTurnUpdate, error)
	QueueSource                work.QueueSource
	Work                       work.RunHooks
	SessionID                  string
	ToolExecution              ToolExecutionMode
}

var defaultModel = types.Model{
	ID:       "unknown",
	Name:     "unknown",
	API:      "unknown",
	Provider: "unknown",
	BaseURL:  "",
	Input:    []string{},
}

// defaultConvertToLlm is the fallback ConvertToLlm: passes through user,
// assistant, and toolResult messages; drops system and nil entries.
func defaultConvertToLlm(messages []types.Message) []types.Message {
	converted := make([]types.Message, 0, len(messages))
	for _, message := range messages {
		if message == nil {
			continue
		}
		switch message.Role() {
		case "user", "assistant", "toolResult":
			converted = append(converted, message)
		}
	}
	return converted
}

// createMutableAgentState seeds the internal state from an optional public
// snapshot. Slices are copied into fresh backing arrays so the Agent's state
// is never backed by caller-owned memory.
func createMutableAgentState(initialState *AgentState) MutableAgentState {
	state := MutableAgentState{
		Model:            defaultModel,
		ThinkingLevel:    ThinkingOff,
		PendingToolCalls: map[string]struct{}{},
	}
	if initialState == nil {
		return state
	}
	state.SystemPrompt = initialState.SystemPrompt
	if !isZeroModel(initialState.Model) {
		state.Model = initialState.Model
	}
	if initialState.ThinkingLevel != "" {
		state.ThinkingLevel = initialState.ThinkingLevel
	}
	state.Tools = copyAgentTools(initialState.Tools)
	state.Messages = copyAgentMessages(initialState.Messages)
	return state
}

// NewAgent creates an Agent with the given options, filling defaults for nil
// callbacks and empty modes. The Agent is idle on return; call Prompt or
// Continue to start a turn.
func NewAgent(opts AgentOptions) *Agent {
	convertToLlm := opts.ConvertToLlm
	if convertToLlm == nil {
		convertToLlm = defaultConvertToLlm
	}
	steeringMode := opts.SteeringMode
	if steeringMode == "" {
		steeringMode = QueueOneAtATime
	}
	followUpMode := opts.FollowUpMode
	if followUpMode == "" {
		followUpMode = QueueOneAtATime
	}
	toolExecution := opts.ToolExecution
	if toolExecution == "" {
		toolExecution = ExecParallel
	}

	return &Agent{
		state:                      createMutableAgentState(opts.InitialState),
		ConvertToLlm:               convertToLlm,
		TransformContext:           opts.TransformContext,
		StreamFn:                   opts.StreamFn,
		GetApiKey:                  opts.GetApiKey,
		BeforeToolCall:             opts.BeforeToolCall,
		AfterToolCall:              opts.AfterToolCall,
		Hooks:                      opts.Hooks,
		Child:                      opts.Child,
		PrepareNextTurn:            opts.PrepareNextTurn,
		PrepareNextTurnWithContext: opts.PrepareNextTurnWithContext,
		PrepareNextTurnE:           opts.PrepareNextTurnE,
		QueueSource:                opts.QueueSource,
		Work:                       opts.Work,
		bindings:                   &runtimeBindings{},
		steeringQueue:              PendingMessageQueue{Mode: steeringMode},
		followUpQueue:              PendingMessageQueue{Mode: followUpMode},
		SessionID:                  opts.SessionID,
		ToolExecution:              toolExecution,
		budget:                     opts.Budget,
	}
}

// Subscribe registers a listener that receives every AgentEvent in order.
// It returns an unsubscribe function. Listener errors stop fan-out immediately
// (subsequent listeners are NOT called) and are surfaced to the run.
//
// Subscribe uses a monotonically assigned identifier because Go closures aren't
// reliably comparable (captureless literals share one funcval), so every
// Subscribe gets a fresh identifier with no dedup. Two independently-created
// listeners both fire.
func (a *Agent) Subscribe(listener func(ctx context.Context, ev AgentEvent) error) (unsubscribe func()) {
	// Go closures aren't reliably comparable (captureless literals share one
	// funcval), so every Subscribe registers a distinct listener under a fresh
	// identifier — no dedup and no unsafe funcval-pointer key.
	a.mu.Lock()
	a.nextListener++
	id := a.nextListener
	a.listeners = append(a.listeners, agentListener{id: id, fn: listener})
	a.mu.Unlock()

	return func() { a.removeListener(id) }
}

// SetSteeringMode sets the steering queue drain mode.
func (a *Agent) SetSteeringMode(mode QueueMode) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steeringQueue.Mode = mode
}

// SteeringMode returns the steering queue drain mode.
func (a *Agent) SteeringMode() QueueMode {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.steeringQueue.Mode
}

// SetFollowUpMode sets the follow-up queue drain mode.
func (a *Agent) SetFollowUpMode(mode QueueMode) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.followUpQueue.Mode = mode
}

// FollowUpMode returns the follow-up queue drain mode.
func (a *Agent) FollowUpMode() QueueMode {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.followUpQueue.Mode
}

// Steer enqueues a steering message for the current (or next) run.
func (a *Agent) Steer(m types.Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steeringQueue.Enqueue(m)
}

// FollowUp enqueues a follow-up message — consumed between outer-loop
// iterations rather than inner ones.
func (a *Agent) FollowUp(m types.Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.followUpQueue.Enqueue(m)
}

// ClearSteeringQueue empties the steering queue.
func (a *Agent) ClearSteeringQueue() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steeringQueue.Clear()
}

// ClearFollowUpQueue empties the follow-up queue.
func (a *Agent) ClearFollowUpQueue() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.followUpQueue.Clear()
}

// ClearAllQueues empties both queues.
func (a *Agent) ClearAllQueues() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steeringQueue.Clear()
	a.followUpQueue.Clear()
}

// HasQueuedMessages reports whether either queue has pending messages.
func (a *Agent) HasQueuedMessages() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.QueueSource != nil {
		pending, _ := a.QueueSource.Pending()
		return pending
	}
	return a.steeringQueue.HasItems() || a.followUpQueue.HasItems()
}

// Abort cancels the in-flight run's context. The run's Prompt/Continue
// returns only after the loop has joined everything the run started (provider
// stream, tool goroutines) and emitted agent_end with [StopAborted]; tools
// that ignore their context delay that return.
func (a *Agent) Abort() {
	a.mu.Lock()
	active := a.activeRun
	a.mu.Unlock()
	if active != nil {
		active.cancel()
	}
}

// SetBudget sets the budget for runs started after the call. The zero Budget
// is unlimited.
func (a *Agent) SetBudget(budget Budget) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.budget = budget
}

// WaitForIdle blocks until the current run finishes (or returns immediately
// if idle).
func (a *Agent) WaitForIdle() {
	a.mu.Lock()
	var done chan struct{}
	if a.activeRun != nil {
		done = a.activeRun.done
	}
	a.mu.Unlock()
	if done != nil {
		<-done
	}
}

// Reset clears the conversation state, queues, streaming flags, and error.
// It does NOT abort an in-flight run — call [Agent.Abort] + [Agent.WaitForIdle]
// before Reset if a turn is in progress.
func (a *Agent) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Messages = nil
	a.state.IsStreaming = false
	a.state.StreamingMessage = nil
	a.state.PendingToolCalls = map[string]struct{}{}
	a.state.ErrorMessage = ""
	a.steeringQueue.Clear()
	a.followUpQueue.Clear()
}

// State returns a snapshot of the public agent state. Slices are copied so
// the caller owns the returned values independently.
func (a *Agent) State() AgentState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return AgentState{
		SystemPrompt:     a.state.SystemPrompt,
		Model:            a.state.Model,
		ThinkingLevel:    a.state.ThinkingLevel,
		Tools:            copyAgentTools(a.state.Tools),
		Messages:         copyAgentMessages(a.state.Messages),
		IsStreaming:      a.state.IsStreaming,
		StreamingMessage: a.state.StreamingMessage,
		PendingToolCalls: copyStringSet(a.state.PendingToolCalls),
		ErrorMessage:     a.state.ErrorMessage,
	}
}

// SetModel updates the model for the next turn.
func (a *Agent) SetModel(model types.Model) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Model = model
}

// SetTools replaces the active tool set.
func (a *Agent) SetTools(tools []AgentTool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Tools = copyAgentTools(tools)
}

// SetMessages replaces the conversation history.
func (a *Agent) SetMessages(messages []types.Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Messages = copyAgentMessages(messages)
}

// SetSystemPrompt replaces the system prompt.
func (a *Agent) SetSystemPrompt(systemPrompt string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.SystemPrompt = systemPrompt
}

// SetThinkingLevel sets the reasoning level for future turns.
func (a *Agent) SetThinkingLevel(level ThinkingLevel) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.ThinkingLevel = level
}

// SetOutputSchema sets the final-answer contract for future runs; nil
// returns them to free text. See [AgentLoopConfig.OutputSchema].
func (a *Agent) SetOutputSchema(schema *outputschema.Schema) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.outputSchema = schema
}

// Prompt runs one run with messages as the prompt and blocks until it ends.
// The error is non-nil only when no run started: [ErrRunActive], a nil
// StreamFn, or a budget that cannot be enforced ([Budget.Validate]). How a
// started run ended, including [StopError], is in the [RunResult].
func (a *Agent) Prompt(ctx context.Context, messages ...types.Message) (RunResult, error) {
	a.mu.Lock()
	if err := a.startableLocked(); err != nil {
		a.mu.Unlock()
		return RunResult{}, err
	}
	return a.runLocked(ctx, false, func(runCtx context.Context, snapshot AgentContext, config AgentLoopConfig, streamFn StreamFn) RunResult {
		_, result := runAgentLoop(runCtx, messages, snapshot, config, a.processEvents, streamFn)
		return result
	})
}

// PromptText is a convenience wrapper that wraps a text string in a user
// message and calls [Agent.Prompt]. Optional image blocks are appended to
// the content.
func (a *Agent) PromptText(ctx context.Context, text string, images ...types.ContentBlock) (RunResult, error) {
	content := []types.ContentBlock{types.NewText(text)}
	if len(images) > 0 {
		content = append(content, images...)
	}
	return a.Prompt(ctx, types.UserMessage{Content: types.BlockContent(content...), Timestamp: time.Now().UnixMilli()})
}

// Continue runs one run from the current transcript. When the last message is
// from the assistant, queued steering (else follow-up) messages or recovered
// job completions become the run's prompt. With neither input nor recoverable
// work, that's an error. The error contract matches [Agent.Prompt].
func (a *Agent) Continue(ctx context.Context) (RunResult, error) {
	a.mu.Lock()
	if err := a.startableLocked(); err != nil {
		a.mu.Unlock()
		return RunResult{}, err
	}
	pendingWork := a.Work != nil && a.Work.Pending()
	if a.QueueSource != nil || pendingWork {
		pending := false
		if a.QueueSource != nil {
			pending, _ = a.QueueSource.Pending()
		}
		if !pending && !pendingWork && (len(a.state.Messages) == 0 || a.state.Messages[len(a.state.Messages)-1].Role() == "assistant") {
			a.mu.Unlock()
			return RunResult{}, errors.New("Cannot continue without pending input")
		}
		return a.runLocked(ctx, false, func(runCtx context.Context, snapshot AgentContext, config AgentLoopConfig, streamFn StreamFn) RunResult {
			config.Resume = true
			_, result := runAgentLoopContinue(runCtx, snapshot, config, a.processEvents, streamFn)
			return result
		})
	}
	if len(a.state.Messages) == 0 {
		a.mu.Unlock()
		return RunResult{}, errors.New("No messages to continue from")
	}
	if a.state.Messages[len(a.state.Messages)-1].Role() != "assistant" {
		// Durable continuation provenance is carried independently of the transcript.
		return a.runLocked(ctx, false, func(runCtx context.Context, snapshot AgentContext, config AgentLoopConfig, streamFn StreamFn) RunResult {
			config.Resume = true
			_, result := runAgentLoopContinue(runCtx, snapshot, config, a.processEvents, streamFn)
			return result
		})
	}

	// Steering drained here must not be polled again as the run's first
	// steering check, hence skipInitialSteeringPoll.
	queued, skipInitialSteeringPoll := a.steeringQueue.Drain(), true
	if len(queued) == 0 {
		queued, skipInitialSteeringPoll = a.followUpQueue.Drain(), false
	}
	if len(queued) == 0 {
		a.mu.Unlock()
		return RunResult{}, errors.New("Cannot continue from message role: assistant")
	}
	return a.runLocked(ctx, skipInitialSteeringPoll, func(runCtx context.Context, snapshot AgentContext, config AgentLoopConfig, streamFn StreamFn) RunResult {
		config.Resume = true
		_, result := runAgentLoop(runCtx, queued, snapshot, config, a.processEvents, streamFn)
		return result
	})
}

// startableLocked reports why a run cannot start now, or nil. Caller holds mu.
func (a *Agent) startableLocked() error {
	if a.activeRun != nil {
		return ErrRunActive
	}
	if a.StreamFn == nil {
		return errors.New("Agent streamFn is nil")
	}
	return a.budget.Validate(a.state.Model)
}

// runBody runs the loop for one run under the claimed run slot.
type runBody func(runCtx context.Context, snapshot AgentContext, config AgentLoopConfig, streamFn StreamFn) RunResult

// runLocked claims the run slot, releases mu, and runs body to completion.
// Caller holds mu and has checked startableLocked. The run context derives
// from ctx so Abort cancels the run without canceling the caller's context.
func (a *Agent) runLocked(ctx context.Context, skipInitialSteeringPoll bool, body runBody) (RunResult, error) {
	snapshot := AgentContext{
		SystemPrompt: a.state.SystemPrompt,
		Messages:     copyAgentMessages(a.state.Messages),
		Tools:        copyAgentTools(a.state.Tools),
	}
	config := a.loopConfigLocked(skipInitialSteeringPoll)
	streamFn := a.StreamFn
	runCtx, cancel := context.WithCancel(ctx)
	a.activeRun = &activeRun{done: make(chan struct{}), cancel: cancel}
	a.state.IsStreaming = true
	a.ended = false
	a.state.StreamingMessage = nil
	a.state.ErrorMessage = ""
	a.mu.Unlock()

	defer a.finishRun()
	return body(runCtx, snapshot, config, streamFn), nil
}

// loopConfigLocked builds the [AgentLoopConfig] for one run from the current
// state, with closures that drain the steering/follow-up queues on each turn.
// Caller holds mu.
//
// skipInitialSteeringPoll suppresses the first GetSteeringMessages call. The
// suppression is a one-shot bool captured by the closure; it flips to false
// after the first poll.
func (a *Agent) loopConfigLocked(skipInitialSteeringPoll bool) AgentLoopConfig {
	convertToLlm := a.ConvertToLlm
	if convertToLlm == nil {
		convertToLlm = defaultConvertToLlm
	}
	toolExecution := a.ToolExecution
	if toolExecution == "" {
		toolExecution = ExecParallel
	}

	config := AgentLoopConfig{
		Budget:           a.budget,
		Model:            a.state.Model,
		ToolExecution:    toolExecution,
		BeforeToolCall:   a.BeforeToolCall,
		AfterToolCall:    a.AfterToolCall,
		Hooks:            a.Hooks,
		Child:            a.Child,
		ConvertToLlm:     convertToLlm,
		TransformContext: a.TransformContext,
		GetAPIKey:        a.GetApiKey,
		QueueSource:      a.QueueSource,
		Work:             a.Work,
		PrepareNextTurnE: a.PrepareNextTurnE,
		bindings:         a.bindings,
		OutputSchema:     a.outputSchema,
	}
	if thinkingLevel := a.state.ThinkingLevel; thinkingLevel != "" && thinkingLevel != ThinkingOff {
		config.Reasoning = string(thinkingLevel)
	}
	config.SessionID = a.SessionID
	prepareNextTurn := a.PrepareNextTurn
	prepareNextTurnWithContext := a.PrepareNextTurnWithContext
	if prepareNextTurnWithContext != nil || prepareNextTurn != nil {
		config.PrepareNextTurn = func(c ShouldStopAfterTurnContext) *AgentLoopTurnUpdate {
			if prepareNextTurnWithContext != nil {
				return prepareNextTurnWithContext(c)
			}
			return prepareNextTurn()
		}
	}
	config.GetSteeringMessages = func() []types.Message {
		a.mu.Lock()
		defer a.mu.Unlock()
		if skipInitialSteeringPoll {
			skipInitialSteeringPoll = false
			return nil
		}
		return a.steeringQueue.Drain()
	}
	config.GetFollowUpMessages = func() []types.Message {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.followUpQueue.Drain()
	}
	return config
}

// finishRun marks the run as settled. IMPORTANT: close(done) happens before
// cancel() — WaitForIdle reads done under mu then unlocks, so a racing
// WaitForIdle must see the closed channel before cancel. The loop has already
// joined everything the run started, so cancel() only releases the context.
func (a *Agent) finishRun() {
	a.mu.Lock()
	active := a.activeRun
	a.state.IsStreaming = false
	a.state.StreamingMessage = nil
	a.state.PendingToolCalls = map[string]struct{}{}
	if active != nil {
		close(active.done)
		a.activeRun = nil
	}
	a.mu.Unlock()
	if active != nil {
		active.cancel()
	}
}

// processEvents is the single event sink wired into the loop config. Under
// emitMu it stamps Seq/At, mutates the streaming/pending state under mu, then
// fans out to listeners with mu released.
//
// State mutations:
//   - message_start/update: set StreamingMessage so State() shows it live
//   - message_end: commit the message to history, clear StreamingMessage
//   - tool_execution_start/end: track PendingToolCalls (in-flight tool set)
//   - turn_end: capture error message from assistant messages
//   - agent_end: clear StreamingMessage; record a run error not already
//     captured from an assistant message
func (a *Agent) processEvents(ctx context.Context, ev AgentEvent) error {
	a.emitMu.Lock()
	defer a.emitMu.Unlock()

	a.mu.Lock()
	if a.activeRun == nil || a.ended {
		a.mu.Unlock()
		return ErrNoActiveRun
	}
	a.seq++
	ev.Seq = a.seq
	ev.At = time.Now()
	switch ev.Type {
	case EventMessageStart, EventMessageUpdate:
		a.state.StreamingMessage = ev.Message
	case EventMessageEnd:
		a.state.StreamingMessage = nil
		a.state.Messages = append(copyAgentMessages(a.state.Messages), ev.Message)
	case EventToolExecutionStart:
		pendingToolCalls := copyStringSet(a.state.PendingToolCalls)
		pendingToolCalls[ev.ToolCallID] = struct{}{}
		a.state.PendingToolCalls = pendingToolCalls
	case EventToolExecutionEnd:
		pendingToolCalls := copyStringSet(a.state.PendingToolCalls)
		delete(pendingToolCalls, ev.ToolCallID)
		a.state.PendingToolCalls = pendingToolCalls
	case EventTurnEnd:
		if errorMessage, ok := assistantErrorMessage(ev.Message); ok {
			a.state.ErrorMessage = errorMessage
		}
	case EventAgentEnd:
		a.ended = true
		a.state.StreamingMessage = nil
		if ev.Err != nil && a.state.ErrorMessage == "" {
			a.state.ErrorMessage = ev.Err.Error()
		}
	}
	listeners := append([]agentListener(nil), a.listeners...)
	a.mu.Unlock()

	for _, listener := range listeners {
		if err := listener.fn(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}

// assistantErrorMessage extracts an error message from an assistant message.
// It handles direct [types.AssistantMessage], pointer, and structs that
// implement the role interface with a reflect fallback.
func assistantErrorMessage(message types.Message) (string, bool) {
	if message == nil || message.Role() != "assistant" {
		return "", false
	}
	switch value := message.(type) {
	case types.AssistantMessage:
		if value.ErrorMessage != "" {
			return value.ErrorMessage, true
		}
	case *types.AssistantMessage:
		if value != nil && value.ErrorMessage != "" {
			return value.ErrorMessage, true
		}
	}
	reflected := reflect.ValueOf(message)
	if reflected.Kind() == reflect.Pointer {
		if reflected.IsNil() {
			return "", false
		}
		reflected = reflected.Elem()
	}
	if reflected.Kind() == reflect.Struct {
		field := reflected.FieldByName("ErrorMessage")
		if field.IsValid() && field.Kind() == reflect.String && field.String() != "" {
			return field.String(), true
		}
	}
	return "", false
}

// removeListener unsubscribes a listener by its monotonic id.
func (a *Agent) removeListener(id uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, current := range a.listeners {
		if current.id == id {
			a.listeners = append(a.listeners[:i], a.listeners[i+1:]...)
			return
		}
	}
}

// isZeroModel reports whether m is the empty value (all fields zero/nil).
func isZeroModel(model types.Model) bool {
	return model.ID == "" && model.Name == "" && model.API == "" && model.Provider == "" && model.BaseURL == "" && !model.Reasoning && len(model.ThinkingLevelMap) == 0 && len(model.Input) == 0 && model.Cost == (types.ModelCost{}) && model.ContextWindow == 0 && model.MaxTokens == 0 && len(model.Headers) == 0 && model.Compat == nil
}

// copyAgentTools returns a shallow copy of tools (the backing array is fresh,
// the elements are shared).
func copyAgentTools(tools []AgentTool) []AgentTool {
	if tools == nil {
		return nil
	}
	return append([]AgentTool(nil), tools...)
}

// copyAgentMessages returns a shallow copy of messages.
func copyAgentMessages(messages []types.Message) []types.Message {
	if messages == nil {
		return nil
	}
	return append([]types.Message(nil), messages...)
}

// copyStringSet returns a shallow copy of a mapset.
func copyStringSet(in map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for key := range in {
		out[key] = struct{}{}
	}
	return out
}
