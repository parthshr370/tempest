package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/retry"
)

// scriptedProvider answers each call with the next attempt's events and
// records what every call was sent.
type scriptedProvider struct {
	mu       sync.Mutex
	attempts []func(ctx context.Context, s *stream.AssistantStream)
	contexts []types.Context
	options  []types.SimpleStreamOptions
}

func (provider *scriptedProvider) Stream(ctx context.Context, model types.Model, request types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream {
	provider.mu.Lock()
	attempt := provider.attempts[len(provider.contexts)]
	provider.contexts = append(provider.contexts, request)
	provider.options = append(provider.options, *opts)
	provider.mu.Unlock()
	events := stream.NewAssistantStream(model.API, model.Provider, model.ID)
	go attempt(ctx, events)
	return events
}

func (provider *scriptedProvider) calls() int {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return len(provider.contexts)
}

// stalledAttempt streams some thinking and the start of a tool call, then
// fails the way a provider reports its idle watchdog firing.
func stalledAttempt(_ context.Context, events *stream.AssistantStream) {
	events.Push(types.StreamEvent{Type: types.EvStart})
	events.Push(types.StreamEvent{Type: types.EvThinkingStart, ContentIndex: 0})
	events.Push(types.StreamEvent{Type: types.EvThinkingDelta, ContentIndex: 0, Delta: "Let's look"})
	events.Push(types.StreamEvent{Type: types.EvToolCallStart, ContentIndex: 1, ToolCall: &types.ContentBlock{Type: types.BlockToolCall, ID: "c1", Name: "write"}})
	events.Push(types.StreamEvent{Type: types.EvToolCallDelta, ContentIndex: 1, Delta: `{"pa`})
	events.Push(types.StreamEvent{Type: types.EvError, Reason: types.StopError, Err: &types.AssistantMessage{
		Content:      []types.ContentBlock{{Type: types.BlockThinking, Thinking: "Let's look"}, {Type: types.BlockToolCall, ID: "c1", Name: "write", Arguments: json.RawMessage(`{"pa`)}},
		StopReason:   types.StopError,
		ErrorCode:    string(retry.CodeStreamInterrupted),
		ErrorMessage: "stream idle for 1m30s (no events)",
		Usage:        types.Usage{Input: 100, TotalTokens: 100},
	}})
}

func answeredAttempt(_ context.Context, events *stream.AssistantStream) {
	events.Push(types.StreamEvent{Type: types.EvStart})
	events.Push(types.StreamEvent{Type: types.EvTextStart, ContentIndex: 0})
	events.Push(types.StreamEvent{Type: types.EvTextDelta, ContentIndex: 0, Delta: "done"})
	events.Push(types.StreamEvent{Type: types.EvDone, Reason: types.StopStop, Message: &types.AssistantMessage{
		Content:    []types.ContentBlock{types.NewText("done")},
		StopReason: types.StopStop,
		Usage:      types.Usage{Input: 100, Output: 5, TotalTokens: 105},
	}})
}

type instantSleeper struct{}

func (instantSleeper) Sleep(context.Context, time.Duration) error { return nil }

func retryingStream(t *testing.T, provider *scriptedProvider, maxAttempts int) StreamFn {
	t.Helper()
	cfg := retry.DefaultConfig()
	cfg.MaxAttempts = maxAttempts
	cfg.Sleeper = instantSleeper{}
	retrier, err := retry.New(provider, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return retrier.Stream
}

func retryLoopConfig() AgentLoopConfig {
	config := AgentLoopConfig{
		Model:        types.Model{API: "test", Provider: "test", ID: "model"},
		ConvertToLlm: func(messages []types.Message) []types.Message { return messages },
	}
	config.SimpleStreamOptions.SessionID = "session-1"
	return config
}

func writeTool(runs *int) AgentTool {
	return AgentTool{
		Tool: types.Tool{Name: "write", Description: "write tool", Parameters: json.RawMessage(`{"type":"object"}`)},
		Execute: func(context.Context, string, json.RawMessage, AgentToolUpdateCallback) (AgentToolResult, error) {
			*runs++
			return AgentToolResult{Content: []types.ContentBlock{types.NewText("written")}}, nil
		},
	}
}

func TestStalledAttemptIsDiscardedAndRequestResent(t *testing.T) {
	provider := &scriptedProvider{attempts: []func(context.Context, *stream.AssistantStream){stalledAttempt, answeredAttempt}}
	toolRuns := 0
	var events []AgentEvent
	emit := func(_ context.Context, ev AgentEvent) error {
		events = append(events, ev)
		return nil
	}

	messages, result := runAgentLoop(context.Background(), []types.Message{userMessage("fix it")}, AgentContext{Tools: []AgentTool{writeTool(&toolRuns)}}, retryLoopConfig(), emit, retryingStream(t, provider, 11))

	if result.StopReason != StopCompleted || result.Turns != 1 || provider.calls() != 2 {
		t.Fatalf("result = %+v, calls = %d", result, provider.calls())
	}
	if toolRuns != 0 {
		t.Fatalf("a tool call from the discarded attempt ran %d times", toolRuns)
	}
	if !reflect.DeepEqual(provider.contexts[0], provider.contexts[1]) || provider.options[0].SessionID != provider.options[1].SessionID {
		t.Fatalf("re-sent request differs:\n%+v\n%+v", provider.contexts[0], provider.contexts[1])
	}
	if len(messages) != 3 {
		t.Fatalf("messages = %+v", messages)
	}
	discarded := assistantMessageAt(t, messages, 1)
	if discarded.StopReason != types.StopError || discarded.ErrorCode != string(retry.CodeStreamInterrupted) || discarded.Content[0].Thinking != "Let's look" {
		t.Fatalf("discarded attempt = %+v", discarded)
	}
	if final := assistantMessageAt(t, messages, 2); assistantText(t, final) != "done" {
		t.Fatalf("final message = %+v", final)
	}
	if result.Usage.Input != 200 || result.Usage.TotalTokens != 205 {
		t.Fatalf("usage = %+v, want both attempts counted once", result.Usage)
	}
	// Listeners see the discarded attempt end before the next attempt starts.
	var assistantLifecycle []string
	for _, ev := range events {
		if (ev.Type == EventMessageStart || ev.Type == EventMessageEnd) && ev.Message.Role() == "assistant" {
			assistantLifecycle = append(assistantLifecycle, string(ev.Type)+":"+string(ev.Message.(types.AssistantMessage).StopReason))
		}
	}
	if got := strings.Join(assistantLifecycle, " "); got != "message_start: message_end:error message_start: message_end:stop" {
		t.Fatalf("assistant lifecycle = %s", got)
	}
}

func TestStallOnEveryAttemptFailsAfterTheCap(t *testing.T) {
	provider := &scriptedProvider{attempts: []func(context.Context, *stream.AssistantStream){stalledAttempt, stalledAttempt, stalledAttempt}}
	toolRuns := 0

	messages, result := runAgentLoop(context.Background(), []types.Message{userMessage("fix it")}, AgentContext{Tools: []AgentTool{writeTool(&toolRuns)}}, retryLoopConfig(), func(context.Context, AgentEvent) error { return nil }, retryingStream(t, provider, 3))

	if result.StopReason != StopError || provider.calls() != 3 || toolRuns != 0 {
		t.Fatalf("result = %+v, calls = %d, tool runs = %d", result, provider.calls(), toolRuns)
	}
	if result.Err == nil || !strings.Contains(result.Err.Error(), "3 attempts") || !strings.Contains(result.Err.Error(), "stream idle") {
		t.Fatalf("err = %v", result.Err)
	}
	if len(messages) != 4 || result.Usage.Input != 300 {
		t.Fatalf("messages = %d, usage = %+v; want every attempt recorded and counted once", len(messages), result.Usage)
	}
}

func TestCancelDuringAttemptIsNotRetried(t *testing.T) {
	waitForCancel := func(ctx context.Context, events *stream.AssistantStream) {
		events.Push(types.StreamEvent{Type: types.EvStart})
		events.Push(types.StreamEvent{Type: types.EvThinkingDelta, ContentIndex: 0, Delta: "thinking"})
		<-ctx.Done()
		events.Push(types.StreamEvent{Type: types.EvError, Reason: types.StopAborted, Err: &types.AssistantMessage{StopReason: types.StopAborted, ErrorMessage: ctx.Err().Error()}})
	}
	provider := &scriptedProvider{attempts: []func(context.Context, *stream.AssistantStream){waitForCancel, answeredAttempt}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	emit := func(_ context.Context, ev AgentEvent) error {
		if ev.Type == EventMessageUpdate {
			cancel()
		}
		return nil
	}

	_, result := runAgentLoop(ctx, []types.Message{userMessage("fix it")}, AgentContext{}, retryLoopConfig(), emit, retryingStream(t, provider, 11))

	if result.StopReason != StopAborted || provider.calls() != 1 {
		t.Fatalf("result = %+v, calls = %d", result, provider.calls())
	}
}
