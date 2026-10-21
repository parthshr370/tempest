package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/engine/validate"
)

// ExecutedToolCallBatch is the result of executing one assistant message's
// tool calls: the result messages (in source order) and whether the batch
// signaled turn termination.
type ExecutedToolCallBatch struct {
	Messages  []types.ToolResultMessage
	Terminate bool
}

// preparedToolCall is a tool call that passed validation and the BeforeToolCall
// gate and is ready to execute.
type preparedToolCall struct {
	toolCall   types.ContentBlock
	tool       AgentTool
	args       json.RawMessage
	permission *PermissionOutcome
}

// immediateOutcome is a result produced during prepare (validation error,
// blocked, aborted, unknown tool) that skips execution entirely.
type immediateOutcome struct {
	result     AgentToolResult
	isError    bool
	permission *PermissionOutcome
}

// executedOutcome is the raw result of Execute (before AfterToolCall merges).
type executedOutcome struct {
	result  AgentToolResult
	isError bool
}

// finalizedOutcome is the post-AfterToolCall result paired with its source
// tool call, ready to be emitted and turned into a ToolResultMessage.
type finalizedOutcome struct {
	toolCall types.ContentBlock
	result   AgentToolResult
	isError  bool
}

// executeToolCalls dispatches an assistant message's tool calls either
// sequentially or in parallel. The mode is sequential when the config forces it
// OR any involved tool declares ExecSequential (one sequential tool
// serializes the whole batch, since it can't be interleaved safely).
func executeToolCalls(ctx context.Context, currentContext AgentContext, assistantMessage types.AssistantMessage, config AgentLoopConfig, emit AgentEventSink) (ExecutedToolCallBatch, error) {
	toolCalls := make([]types.ContentBlock, 0, len(assistantMessage.Content))
	for _, content := range assistantMessage.Content {
		if content.Type == types.BlockToolCall {
			toolCalls = append(toolCalls, content)
		}
	}

	hasSequentialToolCall := false
	for _, toolCall := range toolCalls {
		for _, tool := range currentContext.Tools {
			if tool.Name == toolCall.Name && tool.ExecutionMode == ExecSequential {
				hasSequentialToolCall = true
				break
			}
		}
		if hasSequentialToolCall {
			break
		}
	}

	if config.ToolExecution == ExecSequential || hasSequentialToolCall {
		return executeToolCallsSequential(ctx, currentContext, assistantMessage, toolCalls, config, emit)
	}
	return executeToolCallsParallel(ctx, currentContext, assistantMessage, toolCalls, config, emit)
}

// executeToolCallsSequential runs tool calls one at a time in source order,
// emitting start/end per call. After cancellation the remaining calls still
// get an "Operation aborted" result without running, so every tool call in the
// assistant message has exactly one result.
func executeToolCallsSequential(ctx context.Context, currentContext AgentContext, assistantMessage types.AssistantMessage, toolCalls []types.ContentBlock, config AgentLoopConfig, emit AgentEventSink) (ExecutedToolCallBatch, error) {
	finalizedCalls := make([]finalizedOutcome, 0, len(toolCalls))
	messages := make([]types.ToolResultMessage, 0, len(toolCalls))

	for _, toolCall := range toolCalls {
		prepared, immediate := prepareToolCall(ctx, currentContext, assistantMessage, toolCall, config)
		if err := emitPreparedStart(ctx, toolCall, prepared, immediate, emit); err != nil {
			return ExecutedToolCallBatch{}, err
		}
		var finalized finalizedOutcome
		if immediate != nil {
			finalized = finalizedOutcome{toolCall: toolCall, result: immediate.result, isError: immediate.isError}
		} else {
			executed := executePreparedToolCall(ctx, prepared, emit)
			finalized = finalizeExecutedToolCall(ctx, currentContext, assistantMessage, prepared, executed, config)
		}

		if err := emitToolExecutionEnd(ctx, finalized, emit); err != nil {
			return ExecutedToolCallBatch{}, err
		}
		message := createToolResultMessage(finalized)
		if err := emitToolResultMessage(ctx, message, emit); err != nil {
			return ExecutedToolCallBatch{}, err
		}
		finalizedCalls = append(finalizedCalls, finalized)
		messages = append(messages, message)
	}

	return ExecutedToolCallBatch{Messages: messages, Terminate: shouldTerminateToolBatch(finalizedCalls)}, nil
}

// executeToolCallsParallel runs tool calls concurrently. Results are written
// into slots[i] (source-order indexing) so the emitted ToolResultMessages keep
// source order even though end-events fire in completion order.
//
// Two-phase: prepare runs sequentially (it does validation + the BeforeToolCall
// gate which callers may not expect to run concurrently), then the prepared
// calls run in parallel and are all joined before this returns. safeEmit
// serializes sink calls so the (single-threaded) event sink contract holds.
// Calls prepared after cancellation get an immediate "Operation aborted"
// result instead of running.
func executeToolCallsParallel(ctx context.Context, currentContext AgentContext, assistantMessage types.AssistantMessage, toolCalls []types.ContentBlock, config AgentLoopConfig, emit AgentEventSink) (ExecutedToolCallBatch, error) {
	// safeEmit: parallel goroutines all call emit; the sink (e.g. a stream
	// Push) is not guaranteed concurrency-safe, so serialize through a mutex.
	var emitMu sync.Mutex
	safeEmit := func(ctx context.Context, ev AgentEvent) error {
		emitMu.Lock()
		defer emitMu.Unlock()
		return emit(ctx, ev)
	}

	// slots[i] holds the result for toolCalls[i]; source-order indexing
	// preserves message order regardless of completion order.
	slots := make([]finalizedOutcome, len(toolCalls))
	preps := make([]*preparedToolCall, len(toolCalls))

	for i, toolCall := range toolCalls {
		prepared, immediate := prepareToolCall(ctx, currentContext, assistantMessage, toolCall, config)
		if err := emitPreparedStart(ctx, toolCall, prepared, immediate, safeEmit); err != nil {
			return ExecutedToolCallBatch{}, err
		}
		if immediate != nil {
			finalized := finalizedOutcome{toolCall: toolCall, result: immediate.result, isError: immediate.isError}
			slots[i] = finalized
			if err := emitToolExecutionEnd(ctx, finalized, safeEmit); err != nil {
				return ExecutedToolCallBatch{}, err
			}
			continue
		}
		preps[i] = prepared
	}

	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error
	setErr := func(err error) {
		if err == nil {
			return
		}
		errMu.Lock()
		defer errMu.Unlock()
		if firstErr == nil {
			firstErr = err
		}
	}

	for i := range toolCalls {
		if preps[i] == nil {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			executed := executePreparedToolCall(ctx, preps[i], safeEmit)
			finalized := finalizeExecutedToolCall(ctx, currentContext, assistantMessage, preps[i], executed, config)
			slots[i] = finalized
			setErr(emitToolExecutionEnd(ctx, finalized, safeEmit))
		}(i)
	}
	wg.Wait()

	if firstErr != nil {
		return ExecutedToolCallBatch{}, firstErr
	}

	finalizedCalls := make([]finalizedOutcome, 0, len(toolCalls))
	messages := make([]types.ToolResultMessage, 0, len(toolCalls))
	for i := range toolCalls {
		finalized := slots[i]
		message := createToolResultMessage(finalized)
		if err := emitToolResultMessage(ctx, message, safeEmit); err != nil {
			return ExecutedToolCallBatch{}, err
		}
		finalizedCalls = append(finalizedCalls, finalized)
		messages = append(messages, message)
	}

	return ExecutedToolCallBatch{Messages: messages, Terminate: shouldTerminateToolBatch(finalizedCalls)}, nil
}

// shouldTerminateToolBatch reports whether the batch should end the turn.
// The batch terminates only when it is non-empty and every finalized result
// sets Terminate (a single non-terminating result means another round).
func shouldTerminateToolBatch(finalizedCalls []finalizedOutcome) bool {
	if len(finalizedCalls) == 0 {
		return false
	}
	for _, finalized := range finalizedCalls {
		if !finalized.result.Terminate {
			return false
		}
	}
	return true
}

// prepareToolCallArguments applies the tool's PrepareArguments transform. The
// byte-equality check preserves the "unchanged arguments" branch (Go can't
// compare RawMessage by identity, but immutable JSON bytes compare cleanly).
func prepareToolCallArguments(tool AgentTool, toolCall types.ContentBlock) types.ContentBlock {
	if tool.PrepareArguments == nil {
		return toolCall
	}
	preparedArguments := tool.PrepareArguments(toolCall.Arguments)
	// Go cannot compare RawMessage identity with `===`; byte equality preserves
	// the same observable "unchanged arguments" branch for immutable JSON bytes.
	if bytes.Equal(preparedArguments, toolCall.Arguments) {
		return toolCall
	}
	prepared := toolCall
	prepared.Arguments = preparedArguments
	return prepared
}

// prepareToolCall validates and gates a tool call before execution. It returns
// either a preparedToolCall (ready to execute) or an immediateOutcome (the call
// failed before execution: unknown tool, invalid args, blocked, aborted).
// Panics from PrepareArguments/validation are recovered into an error result so
// a misbehaving hook can't kill the loop.
func prepareToolCall(ctx context.Context, currentContext AgentContext, assistantMessage types.AssistantMessage, toolCall types.ContentBlock, config AgentLoopConfig) (prepared *preparedToolCall, immediate *immediateOutcome) {
	var outcome *PermissionOutcome
	defer func() {
		if prepared != nil {
			prepared.permission = outcome
		}
		if immediate != nil {
			immediate.permission = outcome
		}
	}()
	// After an abort, skip validation and the BeforeToolCall gate (which may
	// prompt a user) for calls that were never going to run.
	if ctx.Err() != nil {
		return nil, &immediateOutcome{result: createErrorToolResult("Operation aborted"), isError: true}
	}
	var tool AgentTool
	found := false
	for _, candidate := range currentContext.Tools {
		if candidate.Name == toolCall.Name {
			tool = candidate
			found = true
			break
		}
	}
	if !found {
		return nil, &immediateOutcome{result: createErrorToolResult("Tool " + toolCall.Name + " not found"), isError: true}
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			prepared = nil
			immediate = &immediateOutcome{result: createErrorToolResult(recoveredMessage(recovered)), isError: true}
		}
	}()

	preparedCall := prepareToolCallArguments(tool, toolCall)
	if len(preparedCall.Arguments) == 0 {
		// Empty Arguments would fail validation; default to {} (the empty
		// object) so a tool with no parameters still runs.
		preparedCall.Arguments = json.RawMessage(`{}`)
	}
	validatedArgs, err := validate.ValidateToolArguments(tool.Tool, preparedCall)
	if err != nil {
		return nil, &immediateOutcome{result: createErrorToolResult(err.Error()), isError: true}
	}

	if config.BeforeToolCall != nil {
		beforeResult := config.BeforeToolCall(ctx, BeforeToolCallContext{
			AssistantMessage: assistantMessage,
			ToolCall:         toolCall,
			Args:             validatedArgs,
			Context:          currentContext,
		})
		if beforeResult != nil {
			outcome = beforeResult.Permission
		}
		if ctx.Err() != nil {
			return nil, &immediateOutcome{result: createErrorToolResult("Operation aborted"), isError: true}
		}
		if beforeResult != nil && beforeResult.Block {
			reason := beforeResult.Reason
			if reason == "" {
				reason = "Tool execution was blocked"
			}
			return nil, &immediateOutcome{result: createErrorToolResult(reason), isError: true}
		}
	}

	if ctx.Err() != nil {
		return nil, &immediateOutcome{result: createErrorToolResult("Operation aborted"), isError: true}
	}

	return &preparedToolCall{toolCall: toolCall, tool: tool, args: validatedArgs}, nil
}

func emitPreparedStart(ctx context.Context, call types.ContentBlock, prepared *preparedToolCall, immediate *immediateOutcome, emit AgentEventSink) error {
	var outcome *PermissionOutcome
	if prepared != nil {
		outcome = prepared.permission
	}
	if immediate != nil {
		outcome = immediate.permission
	}
	if outcome != nil {
		if err := emit(ctx, AgentEvent{Type: EventToolPermission, ToolCallID: call.ID, ToolName: call.Name, Args: call.Arguments, Permission: outcome}); err != nil {
			return err
		}
	}
	return emit(ctx, ToolExecutionStart(call.ID, call.Name, call.Arguments))
}

// executePreparedToolCall runs the tool's Execute and forwards partial updates
// via onUpdate. The acceptingUpdates flag + WaitGroup ensure any in-flight
// onUpdate goroutine settles before this function returns — otherwise a late
// partial could fire after tool_execution_end and confuse listeners (A9a fix).
// Panics from Execute are recovered into an error result.
func executePreparedToolCall(ctx context.Context, prepared *preparedToolCall, emit AgentEventSink) executedOutcome {
	slog.Debug("tool call start", "tool", prepared.toolCall.Name, "id", prepared.toolCall.ID)
	var mu sync.Mutex
	var wg sync.WaitGroup
	acceptingUpdates := true
	var updateErr error

	setAcceptingUpdates := func(accepting bool) {
		mu.Lock()
		defer mu.Unlock()
		acceptingUpdates = accepting
	}
	setUpdateErr := func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if updateErr == nil {
			updateErr = err
		}
	}
	getUpdateErr := func() error {
		mu.Lock()
		defer mu.Unlock()
		return updateErr
	}

	onUpdate := func(partialResult AgentToolResult) {
		mu.Lock()
		if !acceptingUpdates {
			// After Execute returned we stop accepting updates: a late partial
			// would race the tool_execution_end emit ordering.
			mu.Unlock()
			return
		}
		wg.Add(1)
		mu.Unlock()
		defer wg.Done()
		setUpdateErr(emit(ctx, ToolExecutionUpdate(prepared.toolCall.ID, prepared.toolCall.Name, prepared.toolCall.Arguments, partialResult)))
	}

	start := time.Now()
	var result AgentToolResult
	var executeErr error
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				executeErr = fmt.Errorf("%s", recoveredMessage(recovered))
			}
		}()
		result, executeErr = prepared.tool.Execute(ctx, prepared.toolCall.ID, prepared.args, onUpdate)
	}()
	// Stop accepting updates, THEN wait for any in-flight onUpdate goroutine.
	// The order matters: setting the flag first guarantees the wg only tracks
	// updates that actually started; waiting after drains them completely.
	setAcceptingUpdates(false)
	wg.Wait()
	durMs := time.Since(start).Milliseconds()

	if executeErr != nil {
		slog.Error("tool call failed", "tool", prepared.toolCall.Name, "id", prepared.toolCall.ID, "duration_ms", durMs, "error", redactTruncate(executeErr.Error(), 400))
		return executedOutcome{result: createErrorToolResult(executeErr.Error()), isError: true}
	}
	if err := getUpdateErr(); err != nil {
		slog.Error("tool call update failed", "tool", prepared.toolCall.Name, "id", prepared.toolCall.ID, "duration_ms", durMs, "error", redactTruncate(err.Error(), 400))
		return executedOutcome{result: createErrorToolResult(err.Error()), isError: true}
	}
	slog.Info("tool call ok", "tool", prepared.toolCall.Name, "id", prepared.toolCall.ID, "duration_ms", durMs, "result_bytes", contentBytes(result.Content))
	slog.Debug("tool call result", "tool", prepared.toolCall.Name, "id", prepared.toolCall.ID, "preview", logPreview(result.Content, 400))
	return executedOutcome{result: result, isError: false}
}

// finalizeExecutedToolCall applies the AfterToolCall hook's field-by-field
// override to the result. Nil pointer fields and nil Content mean "omitted;
// keep the original" (preserving the merge semantics). AfterToolCall panics
// are recovered into an error result so a bad hook can't kill the loop.
func finalizeExecutedToolCall(ctx context.Context, currentContext AgentContext, assistantMessage types.AssistantMessage, prepared *preparedToolCall, executed executedOutcome, config AgentLoopConfig) (finalized finalizedOutcome) {
	result := executed.result
	isError := executed.isError

	if config.AfterToolCall != nil {
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					result = createErrorToolResult(recoveredMessage(recovered))
					isError = true
				}
			}()
			afterResult := config.AfterToolCall(ctx, AfterToolCallContext{
				AssistantMessage: assistantMessage,
				ToolCall:         prepared.toolCall,
				Args:             prepared.args,
				Result:           result,
				IsError:          isError,
				Context:          currentContext,
			})
			if afterResult == nil {
				return
			}
			if afterResult.Content != nil {
				result.Content = afterResult.Content
			}
			if afterResult.HasDetails {
				result.Details = afterResult.Details
			}
			if afterResult.Terminate != nil {
				result.Terminate = *afterResult.Terminate
			}
			if afterResult.IsError != nil {
				isError = *afterResult.IsError
			}
		}()
	}

	return finalizedOutcome{toolCall: prepared.toolCall, result: result, isError: isError}
}

// createErrorToolResult builds a tool result whose content is the error text.
func createErrorToolResult(message string) AgentToolResult {
	return AgentToolResult{Content: []types.ContentBlock{types.NewText(message)}, Details: nil}
}

// emitToolExecutionEnd emits the tool_execution_end event for a finalized call.
func emitToolExecutionEnd(ctx context.Context, finalized finalizedOutcome, emit AgentEventSink) error {
	return emit(ctx, ToolExecutionEnd(finalized.toolCall.ID, finalized.toolCall.Name, finalized.result, finalized.isError))
}

// createToolResultMessage converts a finalized outcome into the ToolResultMessage
// that will be appended to the conversation.
func createToolResultMessage(finalized finalizedOutcome) types.ToolResultMessage {
	return types.ToolResultMessage{
		ToolCallID: finalized.toolCall.ID,
		ToolName:   finalized.toolCall.Name,
		Content:    finalized.result.Content,
		Details:    marshalDetails(finalized.result.Details),
		IsError:    finalized.isError,
		Timestamp:  time.Now().UnixMilli(),
	}
}

// emitToolResultMessage emits the message_start/message-end pair for a tool
// result (tool results are full messages in the conversation, not deltas).
func emitToolResultMessage(ctx context.Context, toolResultMessage types.ToolResultMessage, emit AgentEventSink) error {
	if err := emit(ctx, MessageStart(toolResultMessage)); err != nil {
		return err
	}
	return emit(ctx, MessageEnd(toolResultMessage))
}

// marshalDetails passes through a tool's raw JSON details for the wire.
// Nil details remain omitted from the message.
func marshalDetails(details json.RawMessage) json.RawMessage {
	if details == nil {
		return nil
	}
	return details
}

// recoveredMessage renders a recovered panic value into a string for an error
// result, handling error/string/other types.
func recoveredMessage(recovered any) string {
	switch value := recovered.(type) {
	case error:
		return value.Error()
	case string:
		return value
	default:
		return fmt.Sprint(value)
	}
}
