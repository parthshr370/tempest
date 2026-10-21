package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
)

func loopConfig(f *faux.Faux) AgentLoopConfig {
	return AgentLoopConfig{
		Model: f.Model(),
		ConvertToLlm: func(messages []types.Message) []types.Message {
			return messages
		},
	}
}

func loopStreamFn(f *faux.Faux) StreamFn {
	return func(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream {
		return f.StreamSimple(ctx, model, c, opts)
	}
}

func collectLoop(t *testing.T, s interface {
	Events() <-chan AgentEvent
	Result(context.Context) ([]types.Message, error)
}) ([]AgentEvent, []types.Message) {
	t.Helper()
	events := []AgentEvent{}
	for event := range s.Events() {
		events = append(events, event)
	}
	messages, err := s.Result(context.Background())
	if err != nil {
		t.Fatalf("Result() error = %v", err)
	}
	return events, messages
}

func userMessage(text string) types.UserMessage {
	return types.UserMessage{Content: types.StringContent(text)}
}

func assistantMessageAt(t *testing.T, messages []types.Message, index int) types.AssistantMessage {
	t.Helper()
	if index >= len(messages) {
		t.Fatalf("messages len = %d, need index %d", len(messages), index)
	}
	message, ok := messages[index].(types.AssistantMessage)
	if !ok {
		t.Fatalf("message[%d] = %#v, want AssistantMessage", index, messages[index])
	}
	return message
}

func assistantText(t *testing.T, message types.AssistantMessage) string {
	t.Helper()
	if len(message.Content) != 1 || message.Content[0].Type != types.BlockText {
		t.Fatalf("assistant content = %#v, want one text block", message.Content)
	}
	return message.Content[0].Text
}

func countEvents(events []AgentEvent, typ AgentEventType) int {
	count := 0
	for _, event := range events {
		if event.Type == typ {
			count++
		}
	}
	return count
}

func TestAgentLoopPreCancelledEmitsDurableRunWithoutProviderCall(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("must not be requested")))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	events, _ := collectLoop(t, AgentLoop(ctx, []types.Message{userMessage("accepted")}, AgentContext{}, loopConfig(f), loopStreamFn(f)))
	if f.CallCount() != 0 {
		t.Fatalf("provider calls = %d, want zero", f.CallCount())
	}
	if countEvents(events, EventAgentStart) != 1 || countEvents(events, EventAgentEnd) != 1 {
		t.Fatalf("run brackets = %v", eventTypes(events))
	}
	end := events[len(events)-1]
	if end.StopReason != StopAborted || end.Turns != 0 {
		t.Fatalf("terminal result = %+v", end.RunResult)
	}
}

func TestAgentLoopSingleTurnNoTools(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("hi")))
	agentContext := AgentContext{SystemPrompt: "sys", Messages: []types.Message{userMessage("hello")}}

	events, messages := collectLoop(t, AgentLoop(context.Background(), nil, agentContext, loopConfig(f), loopStreamFn(f)))

	if len(events) < 7 {
		t.Fatalf("events len = %d, want at least 7: %#v", len(events), eventTypes(events))
	}
	if events[0].Type != EventAgentStart || events[1].Type != EventTurnStart {
		t.Fatalf("initial events = %#v", eventTypes(events[:2]))
	}
	assistantStart := 2
	if events[assistantStart].Type != EventMessageStart || events[assistantStart].Message.Role() != "assistant" {
		t.Fatalf("assistant start event = %#v", events[assistantStart])
	}
	if countEvents(events, EventMessageUpdate) == 0 {
		t.Fatalf("events = %#v, want message_update events", eventTypes(events))
	}
	if events[len(events)-3].Type != EventMessageEnd || events[len(events)-2].Type != EventTurnEnd || events[len(events)-1].Type != EventAgentEnd {
		t.Fatalf("terminal events = %#v", eventTypes(events[len(events)-3:]))
	}
	if len(messages) != 1 {
		t.Fatalf("result messages len = %d, want 1", len(messages))
	}
	if got := assistantText(t, assistantMessageAt(t, messages, 0)); got != "hi" {
		t.Fatalf("assistant text = %q, want hi", got)
	}
}

func TestAgentLoopOneToolRoundContinuesToSecondTurn(t *testing.T) {
	f := faux.New(faux.Options{})
	seenToolCount := make(chan int, 1)
	f.SetResponses(
		func(c types.Context, opts *types.StreamOptions, state faux.State, model types.Model) (types.AssistantMessage, error) {
			seenToolCount <- len(c.Tools)
			return types.AssistantMessage{
				Content:    []types.ContentBlock{faux.ToolCall("lookup", json.RawMessage(`{"query":"x"}`), "tc1")},
				StopReason: types.StopStop,
			}, nil
		},
		faux.Respond(faux.Text("final")),
	)
	tool := executorTool("lookup", `{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`, ExecParallel,
		func(ctx context.Context, id string, params json.RawMessage, onUpdate AgentToolUpdateCallback) (AgentToolResult, error) {
			return AgentToolResult{Content: []types.ContentBlock{types.NewText("result for " + id)}}, nil
		})
	agentContext := AgentContext{Messages: []types.Message{userMessage("start")}, Tools: []AgentTool{tool}}

	events, messages := collectLoop(t, AgentLoop(context.Background(), nil, agentContext, loopConfig(f), loopStreamFn(f)))

	if got := <-seenToolCount; got != 1 {
		t.Fatalf("provider saw %d tools, want 1", got)
	}
	if got := countEvents(events, EventTurnStart); got != 2 {
		t.Fatalf("turn_start count = %d, want 2; events %#v", got, eventTypes(events))
	}
	if got := countEvents(events, EventToolExecutionStart); got != 1 {
		t.Fatalf("tool_execution_start count = %d, want 1", got)
	}
	if got := countEvents(events, EventToolExecutionEnd); got != 1 {
		t.Fatalf("tool_execution_end count = %d, want 1", got)
	}
	if len(messages) != 3 {
		t.Fatalf("result messages len = %d, want assistant/toolResult/assistant", len(messages))
	}
	if messages[1].Role() != "toolResult" {
		t.Fatalf("message[1] role = %q, want toolResult", messages[1].Role())
	}
	if got := assistantText(t, assistantMessageAt(t, messages, 2)); got != "final" {
		t.Fatalf("final assistant text = %q", got)
	}
}

func TestAgentLoopErrorStopEndsImmediately(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.RespondMessage(types.AssistantMessage{
		Content:      []types.ContentBlock{types.NewText("bad")},
		StopReason:   types.StopError,
		ErrorMessage: "boom",
	}))
	agentContext := AgentContext{Messages: []types.Message{userMessage("start")}}

	events, messages := collectLoop(t, AgentLoop(context.Background(), nil, agentContext, loopConfig(f), loopStreamFn(f)))

	if got := countEvents(events, EventTurnEnd); got != 1 {
		t.Fatalf("turn_end count = %d, want 1", got)
	}
	if got := f.CallCount(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
	message := assistantMessageAt(t, messages, 0)
	if message.StopReason != types.StopError || message.ErrorMessage != "boom" {
		t.Fatalf("message = %+v, want stop error boom", message)
	}
}

func TestAgentLoopShouldStopAfterTurnSkipsFollowUp(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("first")), faux.Respond(faux.Text("unused")))
	followUpCalled := false
	config := loopConfig(f)
	config.ShouldStopAfterTurn = func(c ShouldStopAfterTurnContext) bool { return true }
	config.GetFollowUpMessages = func() []types.Message {
		followUpCalled = true
		return []types.Message{userMessage("again")}
	}
	agentContext := AgentContext{Messages: []types.Message{userMessage("start")}}

	_, messages := collectLoop(t, AgentLoop(context.Background(), nil, agentContext, config, loopStreamFn(f)))

	if followUpCalled {
		t.Fatal("follow-up hook called after shouldStopAfterTurn returned true")
	}
	if got := f.CallCount(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
	if len(messages) != 1 || assistantText(t, assistantMessageAt(t, messages, 0)) != "first" {
		t.Fatalf("messages = %#v, want first assistant only", messages)
	}
}

func TestAgentLoopPrepareNextTurnSwapsModel(t *testing.T) {
	f := faux.New(faux.Options{Models: []faux.ModelDef{{ID: "m1"}, {ID: "m2"}}})
	model2, ok := f.ModelByID("m2")
	if !ok {
		t.Fatal("missing faux model m2")
	}
	seenSecondModel := make(chan string, 1)
	seenSecondReasoning := make(chan string, 1)
	f.SetResponses(
		faux.Respond(faux.ToolCall("lookup", json.RawMessage(`{}`), "tc1")),
		func(c types.Context, opts *types.StreamOptions, state faux.State, model types.Model) (types.AssistantMessage, error) {
			seenSecondModel <- model.ID
			return types.AssistantMessage{Content: []types.ContentBlock{types.NewText("done on " + model.ID)}, StopReason: types.StopStop}, nil
		},
	)
	config := loopConfig(f)
	config.Reasoning = string(ThinkingHigh)
	config.PrepareNextTurn = func(c ShouldStopAfterTurnContext) *AgentLoopTurnUpdate {
		if len(c.ToolResults) == 0 {
			return nil
		}
		return &AgentLoopTurnUpdate{Model: &model2, ThinkingLevel: ThinkingOff}
	}
	agentContext := AgentContext{
		Messages: []types.Message{userMessage("start")},
		Tools: []AgentTool{executorTool("lookup", `{"type":"object"}`, ExecParallel,
			func(ctx context.Context, id string, params json.RawMessage, onUpdate AgentToolUpdateCallback) (AgentToolResult, error) {
				return AgentToolResult{Content: []types.ContentBlock{types.NewText("ok")}}, nil
			})},
	}

	streamFn := func(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream {
		if f.CallCount() == 1 {
			seenSecondReasoning <- opts.Reasoning
		}
		return f.StreamSimple(ctx, model, c, opts)
	}

	_, messages := collectLoop(t, AgentLoop(context.Background(), nil, agentContext, config, streamFn))

	if got := <-seenSecondModel; got != "m2" {
		t.Fatalf("second model = %q, want m2", got)
	}
	if got := <-seenSecondReasoning; got != "" {
		t.Fatalf("second reasoning = %q, want cleared", got)
	}
	if got := assistantText(t, assistantMessageAt(t, messages, 2)); got != "done on m2" {
		t.Fatalf("final text = %q, want done on m2", got)
	}
}

func TestAgentLoopSteeringMessagesInjectedBeforeAssistant(t *testing.T) {
	f := faux.New(faux.Options{})
	seenPrompt := make(chan string, 1)
	f.SetResponses(func(c types.Context, opts *types.StreamOptions, state faux.State, model types.Model) (types.AssistantMessage, error) {
		roles := make([]string, 0, len(c.Messages))
		for _, message := range c.Messages {
			roles = append(roles, message.Role())
		}
		seenPrompt <- strings.Join(roles, ",")
		return types.AssistantMessage{Content: []types.ContentBlock{types.NewText("after steering")}, StopReason: types.StopStop}, nil
	})
	called := false
	config := loopConfig(f)
	config.GetSteeringMessages = func() []types.Message {
		if called {
			return nil
		}
		called = true
		return []types.Message{userMessage("steer")}
	}
	agentContext := AgentContext{Messages: []types.Message{userMessage("start")}}

	events, messages := collectLoop(t, AgentLoop(context.Background(), nil, agentContext, config, loopStreamFn(f)))

	if got := <-seenPrompt; got != "user,user" {
		t.Fatalf("llm roles = %q, want user,user", got)
	}
	if len(events) < 5 || events[2].Type != EventMessageStart || events[2].Message.Role() != "user" || events[4].Type != EventMessageStart || events[4].Message.Role() != "assistant" {
		t.Fatalf("initial message events = %#v", eventTypes(events[:5]))
	}
	if len(messages) != 2 || messages[0].Role() != "user" || assistantText(t, assistantMessageAt(t, messages, 1)) != "after steering" {
		t.Fatalf("messages = %#v", messages)
	}
}

func TestAgentLoopFollowUpContinuesOneMoreTurn(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("first")), faux.Respond(faux.Text("second")))
	called := false
	config := loopConfig(f)
	config.GetFollowUpMessages = func() []types.Message {
		if called {
			return nil
		}
		called = true
		return []types.Message{userMessage("follow up")}
	}
	agentContext := AgentContext{Messages: []types.Message{userMessage("start")}}

	events, messages := collectLoop(t, AgentLoop(context.Background(), nil, agentContext, config, loopStreamFn(f)))

	if got := countEvents(events, EventTurnStart); got != 2 {
		t.Fatalf("turn_start count = %d, want 2", got)
	}
	if got := f.CallCount(); got != 2 {
		t.Fatalf("provider calls = %d, want 2", got)
	}
	if len(messages) != 3 || assistantText(t, assistantMessageAt(t, messages, 0)) != "first" || messages[1].Role() != "user" || assistantText(t, assistantMessageAt(t, messages, 2)) != "second" {
		t.Fatalf("messages = %#v", messages)
	}
}

func TestAgentLoopContinueGuards(t *testing.T) {
	f := faux.New(faux.Options{})
	config := loopConfig(f)
	if _, err := AgentLoopContinue(context.Background(), AgentContext{}, config, loopStreamFn(f)); err == nil || err.Error() != "Cannot continue: no messages in context" {
		t.Fatalf("empty continue err = %v", err)
	}
	assistant := types.AssistantMessage{Content: []types.ContentBlock{types.NewText("done")}}
	if _, err := AgentLoopContinue(context.Background(), AgentContext{Messages: []types.Message{assistant}}, config, loopStreamFn(f)); err == nil || err.Error() != "Cannot continue from message role: assistant" {
		t.Fatalf("assistant continue err = %v", err)
	}
}

func TestRunAgentLoopPropagatesMessageEndListenerErrorBeforeTools(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.ToolCall("mutate", json.RawMessage(`{}`), "tc1")))
	toolRan := false
	tool := executorTool("mutate", `{"type":"object"}`, ExecParallel,
		func(ctx context.Context, id string, params json.RawMessage, onUpdate AgentToolUpdateCallback) (AgentToolResult, error) {
			toolRan = true
			return AgentToolResult{Content: []types.ContentBlock{types.NewText("mutated")}}, nil
		})
	wantErr := errors.New("listener failed")
	emit := func(ctx context.Context, ev AgentEvent) error {
		if ev.Type == EventMessageEnd && ev.Message.Role() == "assistant" {
			return wantErr
		}
		return nil
	}

	_, result := runAgentLoop(context.Background(), []types.Message{userMessage("start")}, AgentContext{Tools: []AgentTool{tool}}, loopConfig(f), emit, loopStreamFn(f))

	if result.StopReason != StopError || !errors.Is(result.Err, wantErr) {
		t.Fatalf("runAgentLoop result = %+v, want StopError wrapping the listener failure", result)
	}
	if toolRan {
		t.Fatal("tool ran after message_end listener failed")
	}
}
