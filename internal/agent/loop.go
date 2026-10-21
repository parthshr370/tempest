// Package agent implements the owned agent loop. It runs turns (prompt →
// assistant stream → tool calls → results → next turn) and surfaces every step
// as an [AgentEvent]. See loop.go for the turn loop, executor.go for tool
// dispatch, and runtime.go for the stateful [Agent] wrapper.
//
// # Runs
//
// A run is one [Agent.Prompt] or [Agent.Continue]. The loop calls the model,
// executes the tool calls in its reply, feeds the results back, and repeats.
// The run completes ([StopCompleted]) when the model replies with no tool
// calls and no steering or follow-up message is queued. It also ends on
// Abort or context cancellation ([StopAborted]), a [Budget] limit checked at
// turn boundaries ([StopMaxTurns], [StopMaxCost], [StopMaxWallTime]), or a
// failure ([StopError]). A run that starts emits agent_start first and exactly
// one agent_end last; agent_end carries the [RunResult].
//
// # Goroutine ownership
//
// Context cancellation is a cooperative signal, not a join, so every
// goroutine a run starts is also joined before the run moves past the step
// that started it. Nothing a run started can therefore emit after agent_end.
//
//   - The run. [Agent.Prompt] and [Agent.Continue] run the loop on the
//     caller's goroutine; Abort cancels the run context derived from the
//     caller's. [AgentLoop] and [AgentLoopContinue] start one goroutine that
//     runs the loop and ends the returned stream after agent_end; the caller
//     joins it by draining Events or awaiting Result.
//   - The provider stream. StreamFn starts the provider's producer. It gets a
//     context derived from the run's, so Abort cancels it, and
//     streamAssistantResponse drains the stream to its terminal event before
//     returning, cancelling it first when a listener failure cuts the turn
//     short. The result is the turn's final assistant message.
//   - Parallel tools. executeToolCallsParallel starts one goroutine per
//     prepared call with the run context, each writing its result into a slot
//     indexed by source order, and waits for all of them before returning.
//     A tool that ignores its context blocks that wait: the loop waits rather
//     than leak a goroutine that could still touch the run. Sequential tools
//     run on the loop goroutine.
//   - Tool progress. A tool may call onUpdate from goroutines it starts.
//     executePreparedToolCall stops accepting updates when Execute returns and
//     waits for in-flight updates, so no update outlives tool_execution_end.
//
// Event delivery is serialized: [Agent] holds one emit lock across stamping
// Seq/At and the listener fan-out, so listeners are never called concurrently
// and see Seq in strictly increasing order.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
)

// createAgentStream builds the event stream for one agent run. The stream's
// terminal predicate is EventAgentEnd (so agent_end both closes Events() and
// resolves Result with the final message list); any other event flows through.
func createAgentStream() *stream.Stream[AgentEvent, []types.Message] {
	return stream.New[AgentEvent, []types.Message](64,
		func(ev AgentEvent) bool { return ev.Type == EventAgentEnd },
		func(ev AgentEvent) []types.Message {
			if ev.Type == EventAgentEnd {
				return ev.Messages
			}
			return nil
		},
	)
}

// streamSink returns a sink that stamps Seq/At and pushes onto s. The mutex
// keeps stamping and pushing atomic so Seq order matches delivery order when
// parallel tools emit.
func streamSink(s *stream.Stream[AgentEvent, []types.Message]) AgentEventSink {
	var mu sync.Mutex
	var seq uint64
	return func(ctx context.Context, ev AgentEvent) error {
		mu.Lock()
		defer mu.Unlock()
		seq++
		ev.Seq = seq
		ev.At = time.Now()
		s.Push(ev)
		return nil
	}
}

// AgentLoop runs a fresh agent run from prompts, returning a stream of events
// that terminates with an agent_end carrying the new messages. The loop body
// runs on a goroutine and the caller ranges over Events() / awaits Result;
// the run always emits agent_end, which ends the stream.
func AgentLoop(ctx context.Context, prompts []types.Message, agentContext AgentContext, config AgentLoopConfig, streamFn StreamFn) *stream.Stream[AgentEvent, []types.Message] {
	s := createAgentStream()
	go runAgentLoop(ctx, prompts, agentContext, config, streamSink(s), streamFn)
	return s
}

// AgentLoopContinue resumes a run from an existing transcript (the last message
// must be a user/toolResult — never an assistant message, since the loop would
// immediately try to stream another assistant response from nothing). Returns
// an error up front when the resume precondition is violated.
func AgentLoopContinue(ctx context.Context, agentContext AgentContext, config AgentLoopConfig, streamFn StreamFn) (*stream.Stream[AgentEvent, []types.Message], error) {
	if err := checkContinuable(agentContext.Messages); err != nil {
		return nil, err
	}
	s := createAgentStream()
	go runAgentLoopContinue(ctx, agentContext, config, streamSink(s), streamFn)
	return s, nil
}

// checkContinuable rejects transcripts a continuation cannot resume from.
func checkContinuable(messages []types.Message) error {
	if len(messages) == 0 {
		return errors.New("Cannot continue: no messages in context")
	}
	if messages[len(messages)-1].Role() == "assistant" {
		return errors.New("Cannot continue from message role: assistant")
	}
	return nil
}

// runAgentLoop is the synchronous core behind [AgentLoop] and [Agent.Prompt]:
// one run that starts by appending prompts to the transcript.
func runAgentLoop(ctx context.Context, prompts []types.Message, agentContext AgentContext, config AgentLoopConfig, emit AgentEventSink, streamFn StreamFn) ([]types.Message, RunResult) {
	ctx, run := newLoopRun(ctx, config, emit, streamFn)
	return run.execute(ctx, prompts, func() error {
		current := agentContext
		current.Messages = append([]types.Message{}, agentContext.Messages...)
		if err := emit(ctx, TurnStart()); err != nil {
			return err
		}
		if err := run.appendMessages(ctx, &current, prompts); err != nil {
			return err
		}
		return run.turns(ctx, current)
	})
}

// runAgentLoopContinue is the synchronous core behind [AgentLoopContinue] and
// [Agent.Continue]: one run over the existing transcript. Callers ensure the
// transcript is non-empty and does not end with an assistant message.
func runAgentLoopContinue(ctx context.Context, agentContext AgentContext, config AgentLoopConfig, emit AgentEventSink, streamFn StreamFn) ([]types.Message, RunResult) {
	ctx, run := newLoopRun(ctx, config, emit, streamFn)
	return run.execute(ctx, nil, func() error {
		if err := emit(ctx, TurnStart()); err != nil {
			return err
		}
		return run.turns(ctx, agentContext)
	})
}

// loopRun is the state of one run inside the loop.
type loopRun struct {
	config   AgentLoopConfig
	emit     AgentEventSink
	streamFn StreamFn
	started  time.Time
	// messages are the messages this run added to the transcript.
	messages   []types.Message
	result     RunResult
	accounting *runAccounting
	root       bool
}

// execute emits agent_start, runs body, then emits the run's single agent_end.
// body records how the run ended in run.result.StopReason or returns the
// error that ended it. A panic in body is converted to StopError (the loop
// edge is the one place hooks and providers can take the run down), so
// agent_end is still emitted.
func (run *loopRun) execute(ctx context.Context, prompts []types.Message, body func() error) ([]types.Message, RunResult) {
	err := run.emit(ctx, AgentStart(prompts))
	if err == nil {
		err = runRecoveringPanic(body)
	}
	err = errors.Join(err, run.accounting.failure())
	switch {
	case err == nil:
	case ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)):
		run.result.StopReason = StopAborted
	default:
		run.result.StopReason = StopError
		run.result.Err = err
	}
	if run.root {
		run.result.Usage = run.accounting.total()
	}
	// agent_end must reach listeners after an abort too, so it is delivered
	// under a context that is not canceled.
	if err := run.emit(context.WithoutCancel(ctx), AgentEnd(run.messages, run.result)); err != nil {
		// The run's outcome is already published, but a failed agent_end
		// listener (e.g. the journal) must not report success to the caller.
		run.result.StopReason = StopError
		run.result.Err = errors.Join(run.result.Err, fmt.Errorf("agent_end listener: %w", err))
	}
	return run.messages, run.result
}

// runRecoveringPanic runs body and converts a panic into an error carrying the
// panic value.
func runRecoveringPanic(body func() error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("agent loop panic: %s", recoveredMessage(recovered))
		}
	}()
	return body()
}

// appendMessages emits start/end for messages and adds them to the transcript.
func (run *loopRun) appendMessages(ctx context.Context, current *AgentContext, messages []types.Message) error {
	for _, message := range messages {
		if err := run.emit(ctx, MessageStart(message)); err != nil {
			return err
		}
		if err := run.emit(ctx, MessageEnd(message)); err != nil {
			return err
		}
		current.Messages = append(current.Messages, message)
		run.messages = append(run.messages, message)
	}
	return nil
}

// boundaryStop decides at a turn boundary whether the run must end before the
// next model call. Cancellation wins over budgets because it is the caller's
// explicit intent.
func (run *loopRun) boundaryStop(ctx context.Context) (StopReason, error) {
	if ctx.Err() != nil {
		return StopAborted, nil
	}
	budget := run.config.Budget
	if budget.MaxTurns > 0 && run.result.Turns >= budget.MaxTurns {
		return StopMaxTurns, nil
	}
	if stop, err := run.accounting.costStop(); stop != "" || err != nil {
		return stop, err
	}
	if budget.MaxCostUSD > 0 {
		// PrepareNextTurn can switch to a model whose price is unknown; the
		// budget can no longer be enforced, which is a failure, not free.
		if run.result.Usage.Cost.Unknown {
			return "", fmt.Errorf("cost budget $%g cannot be enforced: %w", budget.MaxCostUSD, ErrUnknownPrice)
		}
		if run.result.Usage.Cost.Total >= budget.MaxCostUSD {
			return StopMaxCost, nil
		}
	}
	if budget.MaxWallTime > 0 && time.Since(run.started) >= budget.MaxWallTime {
		return StopMaxWallTime, nil
	}
	return "", nil
}

// turns is the core outer/inner turn loop.
//
// Outer loop: turns grouped by tool-batch termination; an inner turn streams an
// assistant response, executes its tool calls, appends results, then either
// continues (more tool calls expected) or hands control to follow-up messages.
// Steering messages are drained at the end of each inner iteration; follow-ups
// at the bottom of each outer iteration. PrepareNextTurn can swap the context/
// model/thinking between turns; ShouldStopAfterTurn can short-circuit the run.
//
// Every turn after the first starts at a boundary where cancellation and the
// budget are checked. Messages already drained from the queues are appended
// to the transcript even when the boundary ends the run, so accepted input is
// never dropped; Continue picks them up.
//
// An error/aborted stop reason on the assistant message ends the run without
// executing tools, so a truncated stream doesn't run half-formed tool calls.
func (run *loopRun) turns(ctx context.Context, current AgentContext) error {
	config := &run.config
	firstTurn := true
	var pendingMessages []types.Message
	if config.GetSteeringMessages != nil {
		pendingMessages = config.GetSteeringMessages()
	}

	for {
		hasMoreToolCalls := true

		for hasMoreToolCalls || len(pendingMessages) > 0 {
			if !firstTurn {
				stop, err := run.boundaryStop(ctx)
				if stop != "" || err != nil {
					if appendErr := run.appendMessages(ctx, &current, pendingMessages); appendErr != nil {
						return appendErr
					}
					run.result.StopReason = stop
					return err
				}
				if err := run.emit(ctx, TurnStart()); err != nil {
					return err
				}
			}
			firstTurn = false
			if err := config.Budget.Validate(config.Model); err != nil {
				return err
			}

			if err := run.appendMessages(ctx, &current, pendingMessages); err != nil {
				return err
			}
			pendingMessages = nil
			release, stop, err := run.accounting.admit(ctx, config.Model)
			if stop != "" || err != nil {
				release()
				run.result.StopReason = stop
				return err
			}
			run.result.Turns++
			slog.Info("agent turn", "turn", run.result.Turns)

			message, err := func() (types.AssistantMessage, error) {
				defer release()
				message, err := streamAssistantResponse(ctx, &current, *config, run.emit, run.streamFn)
				return message, errors.Join(err, run.accountUsage(ctx, message))
			}()
			if err != nil {
				return err
			}
			run.messages = append(run.messages, message)

			if message.StopReason == types.StopError || message.StopReason == types.StopAborted {
				if err := run.emit(ctx, TurnEnd(message, []types.ToolResultMessage{})); err != nil {
					return err
				}
				if message.StopReason == types.StopAborted {
					run.result.StopReason = StopAborted
					return nil
				}
				return providerError(message)
			}

			toolResults := []types.ToolResultMessage{}
			hasMoreToolCalls = false
			if hasToolCalls(message) {
				executedToolBatch, err := executeToolCalls(ctx, current, message, *config, run.emit)
				if err != nil {
					return err
				}
				toolResults = executedToolBatch.Messages
				// A batch terminates the turn only when EVERY result sets
				// Terminate — a single non-terminating result means the model
				// expects another round.
				hasMoreToolCalls = !executedToolBatch.Terminate
				for _, result := range toolResults {
					current.Messages = append(current.Messages, result)
					run.messages = append(run.messages, result)
				}
			}

			if err := run.emit(ctx, TurnEnd(message, toolResults)); err != nil {
				return err
			}

			turnContext := ShouldStopAfterTurnContext{
				Message:     message,
				ToolResults: toolResults,
				Context:     current,
				NewMessages: run.messages,
			}
			if config.PrepareNextTurn != nil {
				if update := config.PrepareNextTurn(turnContext); update != nil {
					applyTurnUpdate(config, &current, update)
					turnContext.Context = current
				}
			}

			if config.ShouldStopAfterTurn != nil && config.ShouldStopAfterTurn(turnContext) {
				run.result.StopReason = StopCompleted
				return nil
			}

			if config.GetSteeringMessages != nil {
				pendingMessages = config.GetSteeringMessages()
			}
		}

		if config.GetFollowUpMessages != nil {
			pendingMessages = config.GetFollowUpMessages()
		}
		if len(pendingMessages) == 0 {
			run.result.StopReason = StopCompleted
			return nil
		}
	}
}

// applyTurnUpdate applies a PrepareNextTurn update to the next turn's state.
func applyTurnUpdate(config *AgentLoopConfig, current *AgentContext, update *AgentLoopTurnUpdate) {
	if update.Context != nil {
		*current = *update.Context
	}
	if update.Model != nil {
		config.Model = *update.Model
	}
	// ThinkingOff explicitly clears reasoning (not the same as leaving it
	// unset, which would preserve the prior level).
	if update.ThinkingLevel == ThinkingOff {
		config.Reasoning = ""
	} else if update.ThinkingLevel != "" {
		config.Reasoning = string(update.ThinkingLevel)
	}
}

// hasToolCalls reports whether the assistant message requests any tool call.
func hasToolCalls(message types.AssistantMessage) bool {
	for _, content := range message.Content {
		if content.Type == types.BlockToolCall {
			return true
		}
	}
	return false
}

// providerError is the run error for an assistant message that ended in
// StopError.
func providerError(message types.AssistantMessage) error {
	if message.ErrorMessage == "" {
		return fmt.Errorf("provider %s/%s failed without an error message", message.Provider, message.Model)
	}
	return fmt.Errorf("provider %s/%s: %s", message.Provider, message.Model, message.ErrorMessage)
}

// snapshotAssistant returns a copy of m whose Content slice (and each tool-call's
// Arguments bytes) is independent of the source — used to decouple an emitted
// streaming partial from the AssistantBuilder's live, still-mutating buffer.
func snapshotAssistant(m types.AssistantMessage) types.AssistantMessage {
	if len(m.Content) == 0 {
		return m
	}
	blocks := make([]types.ContentBlock, len(m.Content))
	for i, b := range m.Content {
		if len(b.Arguments) > 0 {
			// Copy the Arguments backing array: the builder keeps appending
			// tool-call JSON deltas to its own slice on this goroutine, while
			// the emitted partial flows to listeners on OTHER goroutines.
			// Without this copy the shared backing array is a data race
			// (A10 verifier finding).
			b.Arguments = append(b.Arguments[:0:0], b.Arguments...)
		}
		blocks[i] = b
	}
	m.Content = blocks
	return m
}

// streamAssistantResponse drives one provider stream: transforms the context,
// resolves the API key, calls streamFn, folds each delta into a partial
// [AssistantMessage], and emits message_start/update/end events. The partial
// is appended to the context messages on start and overwritten on each update
// so a downstream turn sees the live, growing message.
//
// The provider stream is always drained to its end before this returns. When
// a listener failure cuts the turn short, the stream is cancelled first so the
// provider's producer stops instead of running on unobserved.
func streamAssistantResponse(ctx context.Context, cctx *AgentContext, config AgentLoopConfig, emit AgentEventSink, streamFn StreamFn) (types.AssistantMessage, error) {
	messages := cctx.Messages
	if config.TransformContext != nil {
		messages = config.TransformContext(ctx, messages)
	}

	llmMessages := config.ConvertToLlm(messages)
	tools := make([]types.Tool, 0, len(cctx.Tools))
	for _, tool := range cctx.Tools {
		tools = append(tools, tool.Tool)
	}
	llmContext := types.Context{
		SystemPrompt: cctx.SystemPrompt,
		Messages:     llmMessages,
		Tools:        tools,
	}

	resolvedAPIKey := config.APIKey
	if config.GetAPIKey != nil {
		if key := config.GetAPIKey(config.Model.Provider); key != "" {
			resolvedAPIKey = key
		}
	}

	options := config.SimpleStreamOptions
	options.APIKey = resolvedAPIKey
	streamCtx, cancelStream := context.WithCancel(ctx)
	response := streamFn(streamCtx, config.Model, llmContext, &options)
	defer func() {
		cancelStream()
		for range response.Events() {
		}
	}()

	builder := types.NewAssistantBuilder(config.Model.API, config.Model.Provider, config.Model.ID)
	addedPartial := false
	logEnd := func(m types.AssistantMessage) {
		if m.StopReason == types.StopError {
			slog.Error("assistant stream error", "model", m.Model, "provider", m.Provider, "errorMessage", redactTruncate(m.ErrorMessage, 400), "errorCode", m.ErrorCode)
			return
		}
		slog.Info("assistant stream end", "model", m.Model, "provider", m.Provider, "stopReason", string(m.StopReason), "inputTokens", m.Usage.Input, "outputTokens", m.Usage.Output, "cacheReadTokens", m.Usage.CacheRead)
		slog.Debug("assistant text", "model", m.Model, "preview", logPreview(m.Content, 400))
	}

	for event := range response.Events() {
		builder.Fold(event)
		// Snapshot: builder.Message() shares the builder's live Content buffer, which
		// Fold keeps mutating on this goroutine. Emitted partials flow to Agent.State()
		// / listeners on OTHER goroutines, so hand out an independent copy to avoid a
		// data race on the shared backing array (A10 verifier finding).
		partialMessage := snapshotAssistant(builder.Message())

		switch event.Type {
		case types.EvStart:
			slog.Debug("assistant stream start", "model", config.Model.ID, "provider", config.Model.Provider)
			cctx.Messages = append(cctx.Messages, partialMessage)
			addedPartial = true
			if err := emit(ctx, MessageStart(partialMessage)); err != nil {
				return partialMessage, err
			}

		case types.EvTextStart, types.EvTextDelta, types.EvTextEnd,
			types.EvThinkingStart, types.EvThinkingDelta, types.EvThinkingEnd,
			types.EvToolCallStart, types.EvToolCallDelta, types.EvToolCallEnd:
			if addedPartial {
				// Overwrite the slot we added on EvStart so the context's
				// last message is always the current partial.
				cctx.Messages[len(cctx.Messages)-1] = partialMessage
				eventCopy := event
				if err := emit(ctx, MessageUpdate(partialMessage, &eventCopy)); err != nil {
					return partialMessage, err
				}
			}

		case types.EvDone, types.EvError:
			// Use background ctx for Result: we want the terminal message even
			// if runCtx was canceled (e.g. by /cancel) — the stream's own
			// settlement still resolves. Reading Result under a canceled ctx
			// would race the canceled run and lose the terminal metadata.
			finalMessage := assistantResult(context.Background(), response, config)
			if addedPartial {
				cctx.Messages[len(cctx.Messages)-1] = finalMessage
			} else {
				cctx.Messages = append(cctx.Messages, finalMessage)
			}
			if !addedPartial {
				// Stream produced no EvStart (some providers skip it): emit a
				// start now so listeners see the full start→end pairing.
				if err := emit(ctx, MessageStart(finalMessage)); err != nil {
					return finalMessage, err
				}
			}
			logEnd(finalMessage)
			if err := emit(ctx, MessageEnd(finalMessage)); err != nil {
				return finalMessage, err
			}
			return finalMessage, nil
		}
	}

	// Stream ended without an explicit EvDone/EvError (e.g. context cancel
	// mid-fold). Synthesize the final message from Result.
	finalMessage := assistantResult(ctx, response, config)
	if addedPartial {
		cctx.Messages[len(cctx.Messages)-1] = finalMessage
	} else {
		cctx.Messages = append(cctx.Messages, finalMessage)
		if err := emit(ctx, MessageStart(finalMessage)); err != nil {
			return finalMessage, err
		}
	}
	logEnd(finalMessage)
	if err := emit(ctx, MessageEnd(finalMessage)); err != nil {
		return finalMessage, err
	}
	return finalMessage, nil
}

// assistantResult extracts the final AssistantMessage from a completed stream.
// On error/abort it synthesizes a message with the right StopReason so the
// loop's truncated-abort branch fires and the run ends cleanly.
func assistantResult(ctx context.Context, response *stream.AssistantStream, config AgentLoopConfig) types.AssistantMessage {
	finalMessage, err := response.Result(ctx)
	if err == nil && finalMessage != nil {
		return *finalMessage
	}
	message := types.AssistantMessage{
		API:          config.Model.API,
		Provider:     config.Model.Provider,
		Model:        config.Model.ID,
		StopReason:   types.StopError,
		ErrorMessage: "assistant stream ended without a final message",
		Usage:        types.Usage{Cost: types.Cost{Unknown: config.Model.Cost.Unknown}},
	}
	if err != nil {
		message.ErrorMessage = err.Error()
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// Distinguish "user canceled" from "stream broke": the loop treats
			// StopAborted as a non-error termination path.
			message.StopReason = types.StopAborted
		}
	}
	return message
}
