package tempest

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"go.harness.dev/harness/internal/agent"
	types "go.harness.dev/harness/internal/engine/types"
)

func TestMapEveryAgentEvent(t *testing.T) {
	assistant := types.AssistantMessage{Content: []types.ContentBlock{types.NewText("answer"), types.NewThinking("thought", "")}, StopReason: types.StopStop, Usage: Usage{Input: 10, CacheRead: 20, CacheWrite: 30}}
	cases := []struct {
		name  string
		event agent.AgentEvent
		want  []string
	}{
		{"agent_start", agent.AgentStart(nil), []string{"tempest/run"}},
		{"message_start", agent.MessageStart(assistant), nil},
		{"text_delta", agent.MessageUpdate(assistant, &types.StreamEvent{Type: types.EvTextDelta, Delta: "hi"}), []string{"agent_message_chunk"}},
		{"thought_delta", agent.MessageUpdate(assistant, &types.StreamEvent{Type: types.EvThinkingDelta, Delta: "why"}), []string{"agent_thought_chunk"}},
		{"tool_delta", agent.MessageUpdate(assistant, &types.StreamEvent{Type: types.EvToolCallDelta, Delta: "{}"}), nil},
		{"user_end", agent.MessageEnd(types.UserMessage{Content: types.StringContent("question")}), []string{"user_message_chunk"}},
		{"assistant_end", agent.MessageEnd(assistant), []string{"agent_message_chunk", "agent_thought_chunk", "tempest/message_end", "usage_update"}},
		{"tool_result_end", agent.MessageEnd(types.ToolResultMessage{}), nil},
		{"child_usage", agent.AgentEvent{Type: agent.EventUsage, Model: "child", RunResult: RunResult{Usage: Usage{Input: 5}}}, []string{"tempest/usage"}},
		{"permission", agent.AgentEvent{Type: agent.EventToolPermission, ToolCallID: "c", ToolName: "read", Permission: &agent.PermissionOutcome{Allow: true, Source: "mode", Wait: 3 * time.Millisecond}}, []string{"tool_call", "tool_call_update"}},
		{"prompted_permission", agent.AgentEvent{Type: agent.EventToolPermission, ToolCallID: "c", Permission: &agent.PermissionOutcome{Allow: true, Source: "prompt", Prompted: true}}, []string{"tool_call_update"}},
		{"tool_start", agent.ToolExecutionStart("c", "read", json.RawMessage(`{"path":"x"}`)), []string{"tool_call"}},
		{"tool_update", agent.ToolExecutionUpdate("c", "read", nil, agent.AgentToolResult{Content: []types.ContentBlock{types.NewText("partial")}}), []string{"tool_call_update"}},
		{"tool_end", agent.ToolExecutionEnd("c", "read", agent.AgentToolResult{Content: []types.ContentBlock{types.NewText("done")}}, false), []string{"tool_call_update"}},
		{"turn_start", agent.TurnStart(), []string{"tempest/turn"}},
		{"turn_end", agent.TurnEnd(assistant, nil), []string{"tempest/turn"}},
		{"agent_end", agent.AgentEnd(nil, RunResult{StopReason: agent.StopError, Err: errors.New("broken")}), []string{"tempest/run"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newMapper("s", types.Model{ID: "m", ContextWindow: 1234})
			var kinds []string
			for _, u := range m.mapEvent(tc.event) {
				kinds = append(kinds, u.Kind)
				if !json.Valid(u.Update) {
					t.Fatalf("invalid update: %s", u.Update)
				}
			}
			if !reflect.DeepEqual(kinds, tc.want) {
				t.Fatalf("updates = %v, want %v", kinds, tc.want)
			}
		})
	}
}

// Every update for a call must follow its tool_call, including the permission decision.
func TestToolCallPrecedesItsUpdates(t *testing.T) {
	args := json.RawMessage(`{"command":"rm -rf build"}`)
	done := agent.AgentToolResult{Content: []types.ContentBlock{types.NewText("done")}}
	for _, test := range []struct {
		name     string
		outcome  *agent.PermissionOutcome
		statuses []string
	}{
		{"allowed", &agent.PermissionOutcome{Allow: true, Source: "mode"}, []string{"tool_call:pending", "tool_call_update:pending", "tool_call_update:in_progress", "tool_call_update:completed"}},
		{"denied", &agent.PermissionOutcome{Source: "guard"}, []string{"tool_call:pending", "tool_call_update:failed", "tool_call_update:failed"}},
		{"ungated", nil, []string{"tool_call:in_progress", "tool_call_update:completed"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := newMapper("s", types.Model{ID: "m"})
			events := []agent.AgentEvent{agent.ToolExecutionStart("c", "bash", args), agent.ToolExecutionEnd("c", "bash", done, test.outcome != nil && !test.outcome.Allow)}
			if test.outcome != nil {
				events = append([]agent.AgentEvent{{Type: agent.EventToolPermission, ToolCallID: "c", ToolName: "bash", Args: args, Permission: test.outcome}}, events...)
			}
			var got []string
			for _, ev := range events {
				for _, u := range m.mapEvent(ev) {
					var p struct {
						ToolCallID string          `json:"toolCallId"`
						Status     string          `json:"status"`
						Title      string          `json:"title"`
						RawInput   json.RawMessage `json:"rawInput"`
					}
					if err := json.Unmarshal(u.Update, &p); err != nil || p.ToolCallID != "c" {
						t.Fatalf("update %s: %v", u.Update, err)
					}
					if u.Kind == "tool_call" && (p.Title != "$ rm -rf build" || string(p.RawInput) != string(args)) {
						t.Fatalf("announcement lacks call details: %s", u.Update)
					}
					got = append(got, u.Kind+":"+p.Status)
				}
			}
			if !reflect.DeepEqual(got, test.statuses) {
				t.Fatalf("updates = %v, want %v", got, test.statuses)
			}
		})
	}
}

func TestConsumedQueueUpdateCarriesOnlyTheID(t *testing.T) {
	m := newMapper("s", types.Model{ID: "m"})
	ev := agent.MessageEnd(types.UserMessage{Content: types.StringContent("follow up")})
	ev.QueueID = "queue_1"
	updates := m.mapEvent(ev)
	last := updates[len(updates)-1]
	if last.Kind != "tempest/queue" || string(last.Update) != `{"action":"consumed","item":{"id":"queue_1"}}` {
		t.Fatalf("consumed update = %s %s", last.Kind, last.Update)
	}
}
func TestUsageContextAndUnknownCost(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "priced", true: "unknown"}[unknown], func(t *testing.T) {
			m := newMapper("s", types.Model{ContextWindow: 65536})
			updates := m.mapEvent(agent.MessageEnd(types.AssistantMessage{Usage: Usage{Input: 10, Output: 99, CacheRead: 20, CacheWrite: 30, Cost: types.Cost{Unknown: unknown, Total: 0.25}}}))
			var p map[string]json.RawMessage
			_ = json.Unmarshal(updates[len(updates)-1].Update, &p)
			if string(p["used"]) != "60" || string(p["size"]) != "65536" {
				t.Fatalf("usage math = %s", updates[len(updates)-1].Update)
			}
			_, hasCost := p["cost"]
			if hasCost == unknown {
				t.Fatalf("cost presence = %v, unknown %v", hasCost, unknown)
			}
		})
	}
}
func TestToolKindAndTitle(t *testing.T) {
	cases := []struct {
		name, input, title string
		kind               ToolKind
	}{
		{"read", `{"path":"a.go"}`, "read: a.go", "read"}, {"write", `{"path":"a"}`, "write: a", "edit"}, {"edit", `{}`, "edit", "edit"}, {"hashline", `{"input":"[a.go#1A2B]\nPUT 1.=1:\n+x\n[b c.md#3C4D]\nREM\n"}`, "hashline: a.go, b c.md", "edit"}, {"bash", `{"command":"pwd"}`, "$ pwd", "execute"}, {"grep", `{"pattern":"x"}`, "grep: x", "search"}, {"find", `{"pattern":"*.go"}`, "find: *.go", "search"}, {"ls", `{"path":"."}`, "ls: .", "search"}, {"web_fetch", `{"query":"q"}`, "web_fetch: q", "fetch"}, {"web_search", `{}`, "web_search", "fetch"}, {"todo_write", `{}`, "todo_write", "think"}, {"task", `{}`, "task", "other"},
		{"read", `{"paths":["a.go:1-9","b.go"]}`, "read: a.go:1-9, b.go", "read"},
		{"integrate", `{"job_id":"job_1","accept_merged":true}`, "integrate: job_1", "edit"},
		{"write_stdin", `{"name":"repl","text":"print(6*7)\n"}`, `write_stdin repl "print(6*7)\n"`, "execute"},
		{"write_stdin", `{"job_id":"job_1","text":"\u0003"}`, `write_stdin job_1 "\x03"`, "execute"},
		{"write_stdin", `{"name":"repl","close_stdin":true}`, `write_stdin repl "" + close_stdin`, "execute"},
	}
	for _, tc := range cases {
		title, kind := toolDescription(tc.name, json.RawMessage(tc.input))
		if title != tc.title || kind != tc.kind {
			t.Fatalf("%s = %q %q, want %q %q", tc.name, title, kind, tc.title, tc.kind)
		}
	}
}

func TestHookPreparationStatusNeverRegresses(t *testing.T) {
	m := newMapper("s", types.Model{ID: "m"})
	preparing := agent.ToolExecutionStart("c", "bash", json.RawMessage(`{"command":"old"}`))
	preparing.Preparing = true
	events := []agent.AgentEvent{preparing, {Type: agent.EventToolPermission, ToolCallID: "c", ToolName: "bash", Permission: &agent.PermissionOutcome{Allow: true}}, agent.ToolExecutionStart("c", "bash", json.RawMessage(`{"command":"new"}`))}
	var statuses []string
	var last map[string]json.RawMessage
	for _, event := range events {
		for _, u := range m.mapEvent(event) {
			var body map[string]json.RawMessage
			_ = json.Unmarshal(u.Update, &body)
			var status string
			_ = json.Unmarshal(body["status"], &status)
			statuses = append(statuses, status)
			last = body
		}
	}
	if !reflect.DeepEqual(statuses, []string{"pending", "pending", "in_progress"}) || string(last["rawInput"]) != `{"command":"new"}` {
		t.Fatalf("statuses=%v last=%v", statuses, last)
	}
}
