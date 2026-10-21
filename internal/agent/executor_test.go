package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/engine/types"
)

func executorRaw(s string) json.RawMessage { return json.RawMessage(s) }

func executorToolCall(id, name, args string) types.ContentBlock {
	return types.NewToolCall(id, name, executorRaw(args))
}

func executorTool(name, schema string, mode ToolExecutionMode, execute func(context.Context, string, json.RawMessage, AgentToolUpdateCallback) (AgentToolResult, error)) AgentTool {
	if schema == "" {
		schema = `{"type":"object"}`
	}
	return AgentTool{
		Tool: types.Tool{
			Name:        name,
			Description: name + " tool",
			Parameters:  executorRaw(schema),
		},
		ExecutionMode: mode,
		Execute:       execute,
	}
}

type recordingSink struct {
	mu     sync.Mutex
	events []AgentEvent
	endCh  chan string
}

func (r *recordingSink) sink(ctx context.Context, ev AgentEvent) error {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
	if ev.Type == EventToolExecutionEnd && r.endCh != nil {
		r.endCh <- ev.ToolCallID
	}
	return nil
}

func (r *recordingSink) snapshot() []AgentEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]AgentEvent(nil), r.events...)
}

func eventTypes(events []AgentEvent) []AgentEventType {
	out := make([]AgentEventType, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.Type)
	}
	return out
}

func assertEventTypes(t *testing.T, got []AgentEventType, want []AgentEventType) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("event types len = %d, want %d\ngot:  %#v\nwant: %#v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event type[%d] = %q, want %q\ngot:  %#v\nwant: %#v", i, got[i], want[i], got, want)
		}
	}
}

func assertStrings(t *testing.T, got []string, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("strings len = %d, want %d\ngot:  %#v\nwant: %#v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("strings[%d] = %q, want %q\ngot:  %#v\nwant: %#v", i, got[i], want[i], got, want)
		}
	}
}

func resultText(t *testing.T, blocks []types.ContentBlock) string {
	t.Helper()
	if len(blocks) != 1 || blocks[0].Type != types.BlockText {
		t.Fatalf("content = %#v, want one text block", blocks)
	}
	return blocks[0].Text
}

func waitString(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for string")
		return ""
	}
}

func waitBatch(t *testing.T, ch <-chan struct {
	batch ExecutedToolCallBatch
	err   error
}) (ExecutedToolCallBatch, error) {
	t.Helper()
	select {
	case result := <-ch:
		return result.batch, result.err
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for batch")
		return ExecutedToolCallBatch{}, nil
	}
}

func toolResultIDsFromMessages(messages []types.ToolResultMessage) []string {
	ids := make([]string, 0, len(messages))
	for _, message := range messages {
		ids = append(ids, message.ToolCallID)
	}
	return ids
}

func messageEventIDs(t *testing.T, events []AgentEvent) []string {
	t.Helper()
	ids := []string{}
	for _, ev := range events {
		if ev.Type != EventMessageStart && ev.Type != EventMessageEnd {
			continue
		}
		message, ok := ev.Message.(types.ToolResultMessage)
		if !ok {
			t.Fatalf("message event payload = %#v, want ToolResultMessage", ev.Message)
		}
		ids = append(ids, fmt.Sprintf("%s:%s", ev.Type, message.ToolCallID))
	}
	return ids
}

func toolStartIDs(events []AgentEvent) []string {
	ids := []string{}
	for _, ev := range events {
		if ev.Type == EventToolExecutionStart {
			ids = append(ids, ev.ToolCallID)
		}
	}
	return ids
}

func TestExecuteToolCallsSequentialOrderAndTerminate(t *testing.T) {
	var callMu sync.Mutex
	calls := []string{}
	makeExecute := func(label string, terminate bool) func(context.Context, string, json.RawMessage, AgentToolUpdateCallback) (AgentToolResult, error) {
		return func(ctx context.Context, id string, params json.RawMessage, onUpdate AgentToolUpdateCallback) (AgentToolResult, error) {
			callMu.Lock()
			calls = append(calls, id)
			callMu.Unlock()
			onUpdate(AgentToolResult{Content: []types.ContentBlock{types.NewText("loading " + label)}})
			return AgentToolResult{Content: []types.ContentBlock{types.NewText("done " + label)}, Terminate: terminate}, nil
		}
	}

	assistant := types.AssistantMessage{Content: []types.ContentBlock{
		types.NewText("before"),
		executorToolCall("tc1", "first", `{"value":"a"}`),
		executorToolCall("tc2", "second", `{"value":"b"}`),
	}}
	current := AgentContext{Tools: []AgentTool{
		executorTool("first", "", ExecParallel, makeExecute("first", true)),
		executorTool("second", "", ExecParallel, makeExecute("second", true)),
	}}
	recorder := &recordingSink{}

	batch, err := executeToolCalls(context.Background(), current, assistant, AgentLoopConfig{ToolExecution: ExecSequential}, recorder.sink)
	if err != nil {
		t.Fatalf("executeToolCalls error: %v", err)
	}
	if !batch.Terminate {
		t.Fatal("Terminate = false, want true when every result terminates")
	}
	assertStrings(t, toolResultIDsFromMessages(batch.Messages), []string{"tc1", "tc2"})
	callMu.Lock()
	assertStrings(t, calls, []string{"tc1", "tc2"})
	callMu.Unlock()

	assertEventTypes(t, eventTypes(recorder.snapshot()), []AgentEventType{
		EventToolExecutionStart, EventToolExecutionUpdate, EventToolExecutionEnd, EventMessageStart, EventMessageEnd,
		EventToolExecutionStart, EventToolExecutionUpdate, EventToolExecutionEnd, EventMessageStart, EventMessageEnd,
	})

	if shouldTerminateToolBatch([]finalizedOutcome{
		{result: AgentToolResult{Terminate: true}},
		{result: AgentToolResult{Terminate: false}},
	}) {
		t.Fatal("shouldTerminateToolBatch returned true when not every result terminates")
	}
	if shouldTerminateToolBatch(nil) {
		t.Fatal("shouldTerminateToolBatch returned true for an empty batch")
	}
}

func TestExecuteToolCallsUnknownToolImmediateError(t *testing.T) {
	executed := false
	current := AgentContext{Tools: []AgentTool{
		executorTool("other", "", ExecParallel, func(ctx context.Context, id string, params json.RawMessage, onUpdate AgentToolUpdateCallback) (AgentToolResult, error) {
			executed = true
			return AgentToolResult{}, nil
		}),
	}}
	assistant := types.AssistantMessage{Content: []types.ContentBlock{executorToolCall("tc1", "missing", `{}`)}}
	recorder := &recordingSink{}

	batch, err := executeToolCalls(context.Background(), current, assistant, AgentLoopConfig{ToolExecution: ExecSequential}, recorder.sink)
	if err != nil {
		t.Fatalf("executeToolCalls error: %v", err)
	}
	if executed {
		t.Fatal("unknown tool executed")
	}
	if len(batch.Messages) != 1 || !batch.Messages[0].IsError {
		t.Fatalf("messages = %#v, want one error result", batch.Messages)
	}
	if got := resultText(t, batch.Messages[0].Content); got != "Tool missing not found" {
		t.Fatalf("error text = %q", got)
	}
}

func TestExecuteToolCallsValidationFailureImmediateError(t *testing.T) {
	executed := false
	current := AgentContext{Tools: []AgentTool{
		executorTool("needs_name", `{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`, ExecParallel, func(ctx context.Context, id string, params json.RawMessage, onUpdate AgentToolUpdateCallback) (AgentToolResult, error) {
			executed = true
			return AgentToolResult{}, nil
		}),
	}}
	assistant := types.AssistantMessage{Content: []types.ContentBlock{executorToolCall("tc1", "needs_name", `{}`)}}
	recorder := &recordingSink{}

	batch, err := executeToolCalls(context.Background(), current, assistant, AgentLoopConfig{ToolExecution: ExecSequential}, recorder.sink)
	if err != nil {
		t.Fatalf("executeToolCalls error: %v", err)
	}
	if executed {
		t.Fatal("tool executed after validation failure")
	}
	if len(batch.Messages) != 1 || !batch.Messages[0].IsError {
		t.Fatalf("messages = %#v, want one error result", batch.Messages)
	}
	if got := resultText(t, batch.Messages[0].Content); !strings.Contains(got, `Validation failed for tool "needs_name"`) {
		t.Fatalf("validation error text = %q", got)
	}
}

func TestExecuteToolCallsBeforeBlockSkipsExecute(t *testing.T) {
	executed := false
	current := AgentContext{Tools: []AgentTool{
		executorTool("write", "", ExecParallel, func(ctx context.Context, id string, params json.RawMessage, onUpdate AgentToolUpdateCallback) (AgentToolResult, error) {
			executed = true
			return AgentToolResult{}, nil
		}),
	}}
	assistant := types.AssistantMessage{Content: []types.ContentBlock{executorToolCall("tc1", "write", `{}`)}}
	recorder := &recordingSink{}

	batch, err := executeToolCalls(context.Background(), current, assistant, AgentLoopConfig{
		ToolExecution: ExecSequential,
		BeforeToolCall: func(ctx context.Context, c BeforeToolCallContext) *BeforeToolCallResult {
			return &BeforeToolCallResult{Block: true, Reason: "blocked by test"}
		},
	}, recorder.sink)
	if err != nil {
		t.Fatalf("executeToolCalls error: %v", err)
	}
	if executed {
		t.Fatal("blocked tool executed")
	}
	if got := resultText(t, batch.Messages[0].Content); got != "blocked by test" {
		t.Fatalf("block reason = %q", got)
	}
}

func TestFinalizeExecutedToolCallAfterOverrideMergesFields(t *testing.T) {
	t.Run("override fields", func(t *testing.T) {
		isError := true
		terminate := true
		current := AgentContext{Tools: []AgentTool{
			executorTool("read", "", ExecParallel, func(ctx context.Context, id string, params json.RawMessage, onUpdate AgentToolUpdateCallback) (AgentToolResult, error) {
				return AgentToolResult{Content: []types.ContentBlock{types.NewText("before")}, Details: json.RawMessage(`{"before":true}`)}, nil
			}),
		}}
		assistant := types.AssistantMessage{Content: []types.ContentBlock{executorToolCall("tc1", "read", `{}`)}}
		recorder := &recordingSink{}

		batch, err := executeToolCalls(context.Background(), current, assistant, AgentLoopConfig{
			ToolExecution: ExecSequential,
			AfterToolCall: func(ctx context.Context, c AfterToolCallContext) *AfterToolCallResult {
				return &AfterToolCallResult{
					Content:    []types.ContentBlock{types.NewText("after")},
					Details:    json.RawMessage(`{"after":true}`),
					HasDetails: true,
					IsError:    &isError,
					Terminate:  &terminate,
				}
			},
		}, recorder.sink)
		if err != nil {
			t.Fatalf("executeToolCalls error: %v", err)
		}
		if !batch.Terminate || len(batch.Messages) != 1 || !batch.Messages[0].IsError {
			t.Fatalf("batch = %#v, want terminate + error override", batch)
		}
		if got := resultText(t, batch.Messages[0].Content); got != "after" {
			t.Fatalf("content = %q", got)
		}
		var details map[string]bool
		if err := json.Unmarshal(batch.Messages[0].Details, &details); err != nil {
			t.Fatalf("details unmarshal: %v", err)
		}
		if !details["after"] {
			t.Fatalf("details = %#v, want after:true", details)
		}
	})

	t.Run("omitted fields stay original", func(t *testing.T) {
		isError := false
		current := AgentContext{Tools: []AgentTool{
			executorTool("read", "", ExecParallel, func(ctx context.Context, id string, params json.RawMessage, onUpdate AgentToolUpdateCallback) (AgentToolResult, error) {
				return AgentToolResult{Content: []types.ContentBlock{types.NewText("original")}, Details: json.RawMessage(`{"original":true}`), Terminate: true}, nil
			}),
		}}
		assistant := types.AssistantMessage{Content: []types.ContentBlock{executorToolCall("tc1", "read", `{}`)}}
		recorder := &recordingSink{}

		batch, err := executeToolCalls(context.Background(), current, assistant, AgentLoopConfig{
			ToolExecution: ExecSequential,
			AfterToolCall: func(ctx context.Context, c AfterToolCallContext) *AfterToolCallResult {
				return &AfterToolCallResult{IsError: &isError}
			},
		}, recorder.sink)
		if err != nil {
			t.Fatalf("executeToolCalls error: %v", err)
		}
		if !batch.Terminate || batch.Messages[0].IsError {
			t.Fatalf("batch = %#v, want original terminate and explicit non-error", batch)
		}
		if got := resultText(t, batch.Messages[0].Content); got != "original" {
			t.Fatalf("content = %q", got)
		}
		var details map[string]bool
		if err := json.Unmarshal(batch.Messages[0].Details, &details); err != nil {
			t.Fatalf("details unmarshal: %v", err)
		}
		if !details["original"] {
			t.Fatalf("details = %#v, want original:true", details)
		}
	})
}

func TestExecutePreparedToolCallUpdateAndDropAfterReturn(t *testing.T) {
	var late AgentToolUpdateCallback
	current := AgentContext{Tools: []AgentTool{
		executorTool("work", "", ExecParallel, func(ctx context.Context, id string, params json.RawMessage, onUpdate AgentToolUpdateCallback) (AgentToolResult, error) {
			onUpdate(AgentToolResult{Content: []types.ContentBlock{types.NewText("partial")}})
			late = onUpdate
			return AgentToolResult{Content: []types.ContentBlock{types.NewText("done")}}, nil
		}),
	}}
	assistant := types.AssistantMessage{Content: []types.ContentBlock{executorToolCall("tc1", "work", `{}`)}}
	recorder := &recordingSink{}

	if _, err := executeToolCalls(context.Background(), current, assistant, AgentLoopConfig{ToolExecution: ExecSequential}, recorder.sink); err != nil {
		t.Fatalf("executeToolCalls error: %v", err)
	}
	before := recorder.snapshot()
	updates := 0
	for _, ev := range before {
		if ev.Type == EventToolExecutionUpdate {
			updates++
		}
	}
	if updates != 1 {
		t.Fatalf("updates before late callback = %d, want 1", updates)
	}
	if late == nil {
		t.Fatal("late callback was not captured")
	}
	late(AgentToolResult{Content: []types.ContentBlock{types.NewText("too late")}})
	if got, want := len(recorder.snapshot()), len(before); got != want {
		t.Fatalf("events after late callback = %d, want unchanged %d", got, want)
	}
}

func TestExecutePreparedToolCallWaitsForAcceptedAsyncUpdate(t *testing.T) {
	updateEntered := make(chan struct{})
	releaseUpdate := make(chan struct{})
	endSeen := make(chan struct{}, 1)
	var closeUpdateEntered sync.Once

	current := AgentContext{Tools: []AgentTool{
		executorTool("work", "", ExecParallel, func(ctx context.Context, id string, params json.RawMessage, onUpdate AgentToolUpdateCallback) (AgentToolResult, error) {
			go onUpdate(AgentToolResult{Content: []types.ContentBlock{types.NewText("async partial")}})
			<-updateEntered
			return AgentToolResult{Content: []types.ContentBlock{types.NewText("done")}}, nil
		}),
	}}
	assistant := types.AssistantMessage{Content: []types.ContentBlock{executorToolCall("tc1", "work", `{}`)}}
	sink := func(ctx context.Context, ev AgentEvent) error {
		if ev.Type == EventToolExecutionUpdate {
			closeUpdateEntered.Do(func() { close(updateEntered) })
			<-releaseUpdate
		}
		if ev.Type == EventToolExecutionEnd {
			endSeen <- struct{}{}
		}
		return nil
	}
	done := make(chan error, 1)

	go func() {
		_, err := executeToolCalls(context.Background(), current, assistant, AgentLoopConfig{ToolExecution: ExecSequential}, sink)
		done <- err
	}()
	waitRuntimeEvent(t, updateEntered)

	select {
	case <-endSeen:
		t.Fatal("tool_execution_end emitted before accepted async update settled")
	case <-time.After(25 * time.Millisecond):
	}

	close(releaseUpdate)
	waitRuntimeEvent(t, endSeen)
	if err := waitRuntimeErr(t, done); err != nil {
		t.Fatalf("executeToolCalls error: %v", err)
	}
}

func TestExecutePreparedToolCallPanicBecomesErrorAndLoopContinues(t *testing.T) {
	current := AgentContext{Tools: []AgentTool{
		executorTool("panic", "", ExecParallel, func(ctx context.Context, id string, params json.RawMessage, onUpdate AgentToolUpdateCallback) (AgentToolResult, error) {
			panic("boom")
		}),
		executorTool("next", "", ExecParallel, func(ctx context.Context, id string, params json.RawMessage, onUpdate AgentToolUpdateCallback) (AgentToolResult, error) {
			return AgentToolResult{Content: []types.ContentBlock{types.NewText("next done")}}, nil
		}),
	}}
	assistant := types.AssistantMessage{Content: []types.ContentBlock{
		executorToolCall("tc1", "panic", `{}`),
		executorToolCall("tc2", "next", `{}`),
	}}
	recorder := &recordingSink{}

	batch, err := executeToolCalls(context.Background(), current, assistant, AgentLoopConfig{ToolExecution: ExecSequential}, recorder.sink)
	if err != nil {
		t.Fatalf("executeToolCalls error: %v", err)
	}
	if len(batch.Messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(batch.Messages))
	}
	if !batch.Messages[0].IsError || resultText(t, batch.Messages[0].Content) != "boom" {
		t.Fatalf("first message = %#v, want panic error", batch.Messages[0])
	}
	if batch.Messages[1].IsError || resultText(t, batch.Messages[1].Content) != "next done" {
		t.Fatalf("second message = %#v, want normal continuation", batch.Messages[1])
	}
}

func TestExecuteToolCallsParallelEndCompletionOrderAndMessagesSourceOrder(t *testing.T) {
	started := make(chan string, 3)
	releases := map[string]chan struct{}{
		"tc1": make(chan struct{}),
		"tc2": make(chan struct{}),
		"tc3": make(chan struct{}),
	}
	current := AgentContext{Tools: []AgentTool{
		executorTool("work", "", ExecParallel, func(ctx context.Context, id string, params json.RawMessage, onUpdate AgentToolUpdateCallback) (AgentToolResult, error) {
			started <- id
			select {
			case <-releases[id]:
				return AgentToolResult{Content: []types.ContentBlock{types.NewText("done " + id)}}, nil
			case <-ctx.Done():
				return AgentToolResult{}, ctx.Err()
			}
		}),
	}}
	assistant := types.AssistantMessage{Content: []types.ContentBlock{
		executorToolCall("tc1", "work", `{}`),
		executorToolCall("tc2", "work", `{}`),
		executorToolCall("tc3", "work", `{}`),
	}}
	recorder := &recordingSink{endCh: make(chan string, 3)}
	done := make(chan struct {
		batch ExecutedToolCallBatch
		err   error
	}, 1)

	go func() {
		batch, err := executeToolCalls(context.Background(), current, assistant, AgentLoopConfig{ToolExecution: ExecParallel}, recorder.sink)
		done <- struct {
			batch ExecutedToolCallBatch
			err   error
		}{batch: batch, err: err}
	}()

	for i := 0; i < 3; i++ {
		waitString(t, started)
	}
	close(releases["tc2"])
	if got := waitString(t, recorder.endCh); got != "tc2" {
		t.Fatalf("first end = %q, want tc2", got)
	}
	close(releases["tc3"])
	if got := waitString(t, recorder.endCh); got != "tc3" {
		t.Fatalf("second end = %q, want tc3", got)
	}
	close(releases["tc1"])
	if got := waitString(t, recorder.endCh); got != "tc1" {
		t.Fatalf("third end = %q, want tc1", got)
	}

	batch, err := waitBatch(t, done)
	if err != nil {
		t.Fatalf("executeToolCalls error: %v", err)
	}
	assertStrings(t, toolResultIDsFromMessages(batch.Messages), []string{"tc1", "tc2", "tc3"})
	assertStrings(t, messageEventIDs(t, recorder.snapshot()), []string{
		"message_start:tc1", "message_end:tc1",
		"message_start:tc2", "message_end:tc2",
		"message_start:tc3", "message_end:tc3",
	})
}

func TestExecuteToolCallsParallelCancelGivesEveryCallAResultAndRunsOnlyReachedPending(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	executed := []string{}
	current := AgentContext{Tools: []AgentTool{
		executorTool("work", "", ExecParallel, func(ctx context.Context, id string, params json.RawMessage, onUpdate AgentToolUpdateCallback) (AgentToolResult, error) {
			mu.Lock()
			executed = append(executed, id)
			mu.Unlock()
			if err := ctx.Err(); err != nil {
				return AgentToolResult{}, err
			}
			return AgentToolResult{Content: []types.ContentBlock{types.NewText("done " + id)}}, nil
		}),
	}}
	assistant := types.AssistantMessage{Content: []types.ContentBlock{
		executorToolCall("tc1", "work", `{}`),
		executorToolCall("tc2", "work", `{}`),
		executorToolCall("tc3", "work", `{}`),
	}}
	recorder := &recordingSink{}

	batch, err := executeToolCalls(ctx, current, assistant, AgentLoopConfig{
		ToolExecution: ExecParallel,
		BeforeToolCall: func(ctx context.Context, c BeforeToolCallContext) *BeforeToolCallResult {
			if c.ToolCall.ID == "tc2" {
				cancel()
			}
			return nil
		},
	}, recorder.sink)
	if err != nil {
		t.Fatalf("executeToolCalls error: %v", err)
	}
	assertStrings(t, toolStartIDs(recorder.snapshot()), []string{"tc1", "tc2", "tc3"})
	mu.Lock()
	assertStrings(t, executed, []string{"tc1"})
	mu.Unlock()
	assertStrings(t, toolResultIDsFromMessages(batch.Messages), []string{"tc1", "tc2", "tc3"})
	if !batch.Messages[0].IsError || !strings.Contains(resultText(t, batch.Messages[0].Content), "context canceled") {
		t.Fatalf("first message = %#v, want canceled in-flight result", batch.Messages[0])
	}
	for _, skipped := range batch.Messages[1:] {
		if !skipped.IsError || resultText(t, skipped.Content) != "Operation aborted" {
			t.Fatalf("message %s = %#v, want aborted immediate result", skipped.ToolCallID, skipped)
		}
	}
}

func TestSequentialToolInParallelDefaultForcesSequentialBatch(t *testing.T) {
	var mu sync.Mutex
	active := 0
	maxActive := 0
	current := AgentContext{Tools: []AgentTool{
		executorTool("work", "", ExecSequential, func(ctx context.Context, id string, params json.RawMessage, onUpdate AgentToolUpdateCallback) (AgentToolResult, error) {
			mu.Lock()
			active++
			if active > maxActive {
				maxActive = active
			}
			mu.Unlock()
			time.Sleep(10 * time.Millisecond)
			mu.Lock()
			active--
			mu.Unlock()
			return AgentToolResult{Content: []types.ContentBlock{types.NewText("done " + id)}}, nil
		}),
	}}
	assistant := types.AssistantMessage{Content: []types.ContentBlock{
		executorToolCall("tc1", "work", `{}`),
		executorToolCall("tc2", "work", `{}`),
	}}
	recorder := &recordingSink{}

	batch, err := executeToolCalls(context.Background(), current, assistant, AgentLoopConfig{ToolExecution: ExecParallel}, recorder.sink)
	if err != nil {
		t.Fatalf("executeToolCalls error: %v", err)
	}
	if len(batch.Messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(batch.Messages))
	}
	mu.Lock()
	gotMaxActive := maxActive
	mu.Unlock()
	if gotMaxActive != 1 {
		t.Fatalf("max concurrent executions = %d, want sequential forced to 1", gotMaxActive)
	}
	assertEventTypes(t, eventTypes(recorder.snapshot()), []AgentEventType{
		EventToolExecutionStart, EventToolExecutionEnd, EventMessageStart, EventMessageEnd,
		EventToolExecutionStart, EventToolExecutionEnd, EventMessageStart, EventMessageEnd,
	})
}
