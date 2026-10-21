package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
)

func TestConstantsMatchExecutionStrings(t *testing.T) {
	if ExecSequential != "sequential" || ExecParallel != "parallel" {
		t.Fatalf("tool execution constants drifted: %q %q", ExecSequential, ExecParallel)
	}
	if QueueAll != "all" || QueueOneAtATime != "one-at-a-time" {
		t.Fatalf("queue constants drifted: %q %q", QueueAll, QueueOneAtATime)
	}

	thinking := map[ThinkingLevel]string{
		ThinkingOff:     "off",
		ThinkingMinimal: "minimal",
		ThinkingLow:     "low",
		ThinkingMedium:  "medium",
		ThinkingHigh:    "high",
		ThinkingXHigh:   "xhigh",
	}
	for got, want := range thinking {
		if string(got) != want {
			t.Fatalf("thinking constant = %q, want %q", got, want)
		}
	}

	events := map[AgentEventType]string{
		EventAgentStart:          "agent_start",
		EventAgentEnd:            "agent_end",
		EventTurnStart:           "turn_start",
		EventTurnEnd:             "turn_end",
		EventMessageStart:        "message_start",
		EventMessageUpdate:       "message_update",
		EventMessageEnd:          "message_end",
		EventToolExecutionStart:  "tool_execution_start",
		EventToolExecutionUpdate: "tool_execution_update",
		EventToolExecutionEnd:    "tool_execution_end",
	}
	for got, want := range events {
		if string(got) != want {
			t.Fatalf("event constant = %q, want %q", got, want)
		}
	}
}

func TestEngineMessagesSatisfyEngineMessages(t *testing.T) {
	var _ types.Message = types.UserMessage{}
	var _ types.Message = types.AssistantMessage{}
	var _ types.Message = types.ToolResultMessage{}
}

func TestAgentEventConstructors(t *testing.T) {
	msg := types.AssistantMessage{Content: []types.ContentBlock{types.NewText("hello")}}
	toolResults := []types.ToolResultMessage{{ToolCallID: "tc1", ToolName: "read"}}
	streamEvent := &types.StreamEvent{Type: types.EvTextDelta, Delta: "h"}
	args := map[string]any{"path": "README.md"}
	partial := AgentToolResult{Content: []types.ContentBlock{types.NewText("partial")}}
	result := AgentToolResult{Content: []types.ContentBlock{types.NewText("done")}, Details: json.RawMessage(`"ok"`), Terminate: true}
	msgs := []types.Message{msg}

	tests := []struct {
		name  string
		ev    AgentEvent
		want  AgentEventType
		check func(t *testing.T, ev AgentEvent)
	}{
		{"agent_start", AgentStart(msgs), EventAgentStart, func(t *testing.T, ev AgentEvent) {
			if !reflect.DeepEqual(ev.Messages, msgs) {
				t.Fatalf("prompts = %#v, want %#v", ev.Messages, msgs)
			}
		}},
		{"agent_end", AgentEnd(msgs, RunResult{StopReason: StopMaxTurns, Turns: 2}), EventAgentEnd, func(t *testing.T, ev AgentEvent) {
			if !reflect.DeepEqual(ev.Messages, msgs) || ev.StopReason != StopMaxTurns || ev.Turns != 2 {
				t.Fatalf("agent_end fields = %#v", ev)
			}
		}},
		{"turn_start", TurnStart(), EventTurnStart, func(t *testing.T, ev AgentEvent) {}},
		{"turn_end", TurnEnd(msg, toolResults), EventTurnEnd, func(t *testing.T, ev AgentEvent) {
			if ev.Message.Role() != "assistant" || !reflect.DeepEqual(ev.ToolResults, toolResults) {
				t.Fatalf("turn_end fields = %#v", ev)
			}
		}},
		{"message_start", MessageStart(msg), EventMessageStart, func(t *testing.T, ev AgentEvent) {
			if ev.Message.Role() != "assistant" {
				t.Fatalf("message role = %q", ev.Message.Role())
			}
		}},
		{"message_update", MessageUpdate(msg, streamEvent), EventMessageUpdate, func(t *testing.T, ev AgentEvent) {
			if ev.AssistantMessageEvent != streamEvent || ev.Message.Role() != "assistant" {
				t.Fatalf("message_update fields = %#v", ev)
			}
		}},
		{"message_end", MessageEnd(msg), EventMessageEnd, func(t *testing.T, ev AgentEvent) {
			if ev.Message.Role() != "assistant" {
				t.Fatalf("message role = %q", ev.Message.Role())
			}
		}},
		{"tool_execution_start", ToolExecutionStart("tc1", "read", args), EventToolExecutionStart, func(t *testing.T, ev AgentEvent) {
			if ev.ToolCallID != "tc1" || ev.ToolName != "read" || !reflect.DeepEqual(ev.Args, args) {
				t.Fatalf("tool_execution_start fields = %#v", ev)
			}
		}},
		{"tool_execution_update", ToolExecutionUpdate("tc1", "read", args, partial), EventToolExecutionUpdate, func(t *testing.T, ev AgentEvent) {
			if ev.ToolCallID != "tc1" || ev.ToolName != "read" || !reflect.DeepEqual(ev.Args, args) || !reflect.DeepEqual(ev.PartialResult, partial) {
				t.Fatalf("tool_execution_update fields = %#v", ev)
			}
		}},
		{"tool_execution_end", ToolExecutionEnd("tc1", "read", result, true), EventToolExecutionEnd, func(t *testing.T, ev AgentEvent) {
			if ev.ToolCallID != "tc1" || ev.ToolName != "read" || !reflect.DeepEqual(ev.Result, result) || !ev.IsError {
				t.Fatalf("tool_execution_end fields = %#v", ev)
			}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.ev.Type != tt.want {
				t.Fatalf("type = %q, want %q", tt.ev.Type, tt.want)
			}
			tt.check(t, tt.ev)
		})
	}
}

func TestAgentToolExecuteShape(t *testing.T) {
	params := json.RawMessage(`{"path":"README.md"}`)
	var updates []AgentToolResult

	tool := AgentTool{
		Tool: types.Tool{
			Name:        "read",
			Description: "Read a file",
			Parameters:  json.RawMessage(`{"type":"object"}`),
		},
		Label:         "Read",
		ExecutionMode: ExecParallel,
		Execute: func(ctx context.Context, toolCallID string, got json.RawMessage, onUpdate AgentToolUpdateCallback) (AgentToolResult, error) {
			if toolCallID != "tc1" {
				t.Fatalf("toolCallID = %q", toolCallID)
			}
			if string(got) != string(params) {
				t.Fatalf("params = %s, want %s", got, params)
			}
			onUpdate(AgentToolResult{Content: []types.ContentBlock{types.NewText("loading")}})
			return AgentToolResult{Content: []types.ContentBlock{types.NewText("done")}, Details: json.RawMessage(`{"ok":true}`)}, nil
		},
	}

	got, err := tool.Execute(context.Background(), "tc1", params, func(r AgentToolResult) { updates = append(updates, r) })
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || updates[0].Content[0].Text != "loading" {
		t.Fatalf("updates = %#v", updates)
	}
	if got.Content[0].Text != "done" || got.Details == nil {
		t.Fatalf("result = %#v", got)
	}
}

func TestAgentLoopConfigOptionalHooksAreNil(t *testing.T) {
	input := []types.Message{types.UserMessage{Content: types.StringContent("hi")}}
	cfg := AgentLoopConfig{
		Model: types.Model{ID: "m1", API: "faux", Provider: "faux"},
		ConvertToLlm: func(messages []types.Message) []types.Message {
			return messages
		},
	}

	if cfg.TransformContext != nil || cfg.GetAPIKey != nil || cfg.ShouldStopAfterTurn != nil || cfg.PrepareNextTurn != nil || cfg.GetSteeringMessages != nil || cfg.GetFollowUpMessages != nil || cfg.BeforeToolCall != nil || cfg.AfterToolCall != nil {
		t.Fatalf("optional hooks should default to nil: %#v", cfg)
	}
	converted := cfg.ConvertToLlm(input)
	if len(converted) != 1 || converted[0].Role() != "user" {
		t.Fatalf("converted = %#v", converted)
	}
}

func TestAfterToolCallResultPointerFields(t *testing.T) {
	omitted := AfterToolCallResult{}
	if omitted.IsError != nil || omitted.Terminate != nil || omitted.Content != nil || omitted.HasDetails {
		t.Fatalf("zero value should mean all fields omitted: %#v", omitted)
	}

	setFalse := false
	setTrue := true
	set := AfterToolCallResult{
		Content:    []types.ContentBlock{},
		Details:    nil,
		HasDetails: true,
		IsError:    &setFalse,
		Terminate:  &setTrue,
	}
	if set.IsError == nil || *set.IsError || set.Terminate == nil || !*set.Terminate {
		t.Fatalf("pointer fields did not preserve explicit false/true: %#v", set)
	}
	if set.Content == nil || !set.HasDetails {
		t.Fatalf("content/details presence not preserved: %#v", set)
	}
}
