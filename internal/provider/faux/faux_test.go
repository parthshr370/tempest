package faux

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
)

func collectEvents(s interface {
	Events() <-chan types.StreamEvent
}) []types.StreamEvent {
	events := []types.StreamEvent{}
	for event := range s.Events() {
		events = append(events, event)
	}
	return events
}

func assertTerminalResult(t *testing.T, s interface {
	Result(context.Context) (*types.AssistantMessage, error)
}, want types.StopReason) *types.AssistantMessage {
	t.Helper()
	message, err := s.Result(context.Background())
	if err != nil {
		t.Fatalf("Result() error = %v", err)
	}
	if message.StopReason != want {
		t.Fatalf("StopReason = %q, want %q", message.StopReason, want)
	}
	return message
}

func TestTextResponseStreamsDeltasAndDone(t *testing.T) {
	f := New(Options{})
	f.SetResponses(Respond(Text("Hello world")))

	s := f.Stream(context.Background(), f.Model(), types.Context{}, nil)
	events := collectEvents(s)

	if len(events) < 5 {
		t.Fatalf("events length = %d, want at least 5", len(events))
	}
	if events[0].Type != types.EvStart || events[1].Type != types.EvTextStart {
		t.Fatalf("initial event types = %q, %q; want start, text_start", events[0].Type, events[1].Type)
	}
	var delta strings.Builder
	for _, event := range events[2 : len(events)-2] {
		if event.Type != types.EvTextDelta {
			t.Fatalf("middle event type = %q, want text_delta", event.Type)
		}
		delta.WriteString(event.Delta)
	}
	if delta.String() != "Hello world" {
		t.Fatalf("deltas = %q, want Hello world", delta.String())
	}
	if end := events[len(events)-2]; end.Type != types.EvTextEnd || end.Content != "Hello world" {
		t.Fatalf("text end = %+v, want content Hello world", end)
	}
	if done := events[len(events)-1]; done.Type != types.EvDone || done.Reason != types.StopStop {
		t.Fatalf("terminal event = %+v, want done/stop", done)
	}

	message := assertTerminalResult(t, s, types.StopStop)
	if got := message.Content[0].Text; got != "Hello world" {
		t.Fatalf("Result content = %q, want Hello world", got)
	}
	if got := s.Final().Content[0].Text; got != "Hello world" {
		t.Fatalf("Final content = %q, want Hello world", got)
	}
}

func TestToolCallResponseStreamsArguments(t *testing.T) {
	f := New(Options{})
	f.SetResponses(Respond(ToolCall("get", json.RawMessage(`{"x":1}`), "t1")))

	s := f.Stream(context.Background(), f.Model(), types.Context{}, nil)
	events := collectEvents(s)

	if events[0].Type != types.EvStart || events[1].Type != types.EvToolCallStart {
		t.Fatalf("initial event types = %q, %q; want start, toolcall_start", events[0].Type, events[1].Type)
	}
	if start := events[1].ToolCall; start == nil || start.ID != "t1" || start.Name != "get" {
		t.Fatalf("toolcall_start = %+v, want id t1 name get", start)
	}
	var delta strings.Builder
	for _, event := range events[2 : len(events)-2] {
		if event.Type != types.EvToolCallDelta {
			t.Fatalf("middle event type = %q, want toolcall_delta", event.Type)
		}
		delta.WriteString(event.Delta)
	}
	if delta.String() != `{"x":1}` {
		t.Fatalf("argument deltas = %q, want {\"x\":1}", delta.String())
	}
	end := events[len(events)-2]
	if end.Type != types.EvToolCallEnd || end.ToolCall == nil || string(end.ToolCall.Arguments) != `{"x":1}` {
		t.Fatalf("toolcall_end = %+v, want full arguments", end)
	}

	message := assertTerminalResult(t, s, types.StopStop)
	if got := message.Content[0]; got.Type != types.BlockToolCall || got.ID != "t1" || got.Name != "get" || string(got.Arguments) != `{"x":1}` {
		t.Fatalf("Result tool call = %+v", got)
	}
}

func TestThinkingResponseStreamsDeltas(t *testing.T) {
	f := New(Options{})
	f.SetResponses(Respond(Thinking("careful thought")))

	s := f.Stream(context.Background(), f.Model(), types.Context{}, nil)
	events := collectEvents(s)

	if events[0].Type != types.EvStart || events[1].Type != types.EvThinkingStart {
		t.Fatalf("initial event types = %q, %q; want start, thinking_start", events[0].Type, events[1].Type)
	}
	var delta strings.Builder
	for _, event := range events[2 : len(events)-2] {
		if event.Type != types.EvThinkingDelta {
			t.Fatalf("middle event type = %q, want thinking_delta", event.Type)
		}
		delta.WriteString(event.Delta)
	}
	if delta.String() != "careful thought" {
		t.Fatalf("thinking deltas = %q, want careful thought", delta.String())
	}
	if end := events[len(events)-2]; end.Type != types.EvThinkingEnd || end.Content != "careful thought" {
		t.Fatalf("thinking_end = %+v", end)
	}
}

func TestEmptyQueueYieldsError(t *testing.T) {
	f := New(Options{})
	s := f.Stream(context.Background(), f.Model(), types.Context{}, nil)
	events := collectEvents(s)

	if len(events) != 1 || events[0].Type != types.EvError {
		t.Fatalf("events = %+v, want one error event", events)
	}
	if events[0].Err == nil || events[0].Err.ErrorMessage != "No more faux responses queued" {
		t.Fatalf("error message = %+v", events[0].Err)
	}
	if got := f.CallCount(); got != 1 {
		t.Fatalf("CallCount = %d, want 1", got)
	}
	message := assertTerminalResult(t, s, types.StopError)
	if message.ErrorMessage != "No more faux responses queued" {
		t.Fatalf("Result error = %q", message.ErrorMessage)
	}
}

func TestFactoryReceivesContextStateAndModel(t *testing.T) {
	f := New(Options{})
	seen := make(chan string, 1)
	f.SetResponses(func(c types.Context, opts *types.StreamOptions, state State, model types.Model) (types.AssistantMessage, error) {
		seen <- c.SystemPrompt + ":" + model.ID + ":" + string(rune('0'+state.CallCount))
		return types.AssistantMessage{Content: []types.ContentBlock{Text("factory")}, StopReason: types.StopStop}, nil
	})

	s := f.Stream(context.Background(), f.Model(), types.Context{SystemPrompt: "sys"}, nil)
	collectEvents(s)
	assertTerminalResult(t, s, types.StopStop)

	if got := <-seen; got != "sys:faux-1:1" {
		t.Fatalf("factory saw %q, want sys:faux-1:1", got)
	}
}

func TestAbortBeforeStreamStartYieldsAbortedError(t *testing.T) {
	f := New(Options{})
	f.SetResponses(Respond(Text("unseen")))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s := f.Stream(ctx, f.Model(), types.Context{}, nil)
	events := collectEvents(s)

	if len(events) != 1 || events[0].Type != types.EvError || events[0].Reason != types.StopAborted {
		t.Fatalf("events = %+v, want one aborted error", events)
	}
	message := assertTerminalResult(t, s, types.StopAborted)
	if message.ErrorMessage != "Request was aborted" {
		t.Fatalf("abort error = %q", message.ErrorMessage)
	}
}

func TestUsageCacheReadForSharedPromptPrefix(t *testing.T) {
	f := New(Options{})
	f.SetResponses(Respond(Text("one")), Respond(Text("two")))
	opts := &types.StreamOptions{SessionID: "session-1"}
	shared := strings.Repeat("shared prompt ", 8)

	first := f.Stream(context.Background(), f.Model(), types.Context{SystemPrompt: shared + "one"}, opts)
	collectEvents(first)
	firstMessage := assertTerminalResult(t, first, types.StopStop)
	if firstMessage.Usage.CacheWrite == 0 {
		t.Fatalf("first cacheWrite = 0, want prompt cached")
	}

	second := f.Stream(context.Background(), f.Model(), types.Context{SystemPrompt: shared + "two"}, opts)
	collectEvents(second)
	secondMessage := assertTerminalResult(t, second, types.StopStop)
	if secondMessage.Usage.CacheRead == 0 {
		t.Fatalf("second cacheRead = 0, want shared prefix cache read; usage %+v", secondMessage.Usage)
	}
}

func TestConcurrentStreamsAreRaceCleanAndConsumeDistinctResponses(t *testing.T) {
	f := New(Options{})
	texts := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	steps := make([]ResponseStep, 0, len(texts))
	for _, text := range texts {
		steps = append(steps, Respond(Text(text)))
	}
	f.SetResponses(steps...)

	results := make(chan string, len(texts))
	var wg sync.WaitGroup
	for range texts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := f.Stream(context.Background(), f.Model(), types.Context{}, nil)
			collectEvents(s)
			message, err := s.Result(context.Background())
			if err != nil {
				t.Errorf("Result() error = %v", err)
				return
			}
			results <- message.Content[0].Text
		}()
	}
	wg.Wait()
	close(results)

	seen := map[string]bool{}
	for result := range results {
		seen[result] = true
	}
	for _, text := range texts {
		if !seen[text] {
			t.Fatalf("missing response %q from results %#v", text, seen)
		}
	}
	if got := f.CallCount(); got != len(texts) {
		t.Fatalf("CallCount = %d, want %d", got, len(texts))
	}
	if got := f.PendingCount(); got != 0 {
		t.Fatalf("PendingCount = %d, want 0", got)
	}
}

func TestUsageCostsFollowModelPrice(t *testing.T) {
	for _, price := range []types.ModelCost{{Input: 2, Output: 10}, {Unknown: true}} {
		provider := New(Options{})
		provider.SetResponses(Respond(Text("a response with several tokens")))
		model := provider.Model()
		model.Cost = price
		streamed := provider.Stream(context.Background(), model, types.Context{Messages: []types.Message{types.UserMessage{Content: types.StringContent("hello")}}}, nil)
		message := assertTerminalResult(t, streamed, types.StopStop)
		if message.Usage.Cost.Unknown != price.Unknown {
			t.Fatalf("unknown price lost: %+v", message.Usage)
		}
		if !price.Unknown && message.Usage.Cost.Total <= 0 {
			t.Fatalf("priced model reported free usage: %+v", message.Usage)
		}
	}
}
