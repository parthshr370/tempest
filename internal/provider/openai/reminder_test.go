package openai

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
)

// sentMessage is one wire message with its content parts' cache markers.
type sentMessage struct {
	raw         json.RawMessage
	role        string
	breakpoints int
}

// sentMessages decodes a request body's messages and counts the
// cache_control markers on each one's content parts.
func sentMessages(t *testing.T, params map[string]any) []sentMessage {
	t.Helper()
	body, err := json.Marshal(params["messages"])
	if err != nil {
		t.Fatal(err)
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(body, &raws); err != nil {
		t.Fatal(err)
	}
	out := make([]sentMessage, len(raws))
	for i, raw := range raws {
		var message struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(raw, &message); err != nil {
			t.Fatal(err)
		}
		out[i] = sentMessage{raw: raw, role: message.Role}
		var parts []struct {
			CacheControl json.RawMessage `json:"cache_control"`
		}
		if json.Unmarshal(message.Content, &parts) == nil {
			for _, part := range parts {
				if len(part.CacheControl) > 0 {
					out[i].breakpoints++
				}
			}
		}
	}
	return out
}

// Claude behind OpenRouter: a request-scope tail is not in the next request,
// so its cache breakpoint goes on the last message that request still sends,
// not (through top-level automatic caching) on the tail. The next request,
// with no tail, keeps automatic caching and sends the marked prefix unchanged.
func TestOpenRouterClaudeBreakpointSkipsRequestScopedTail(t *testing.T) {
	model := openRouterModel("anthropic/claude-sonnet-4.5")
	tool := func(id string) []types.Message {
		return []types.Message{
			types.AssistantMessage{Content: []types.ContentBlock{types.NewToolCall(id, "read", json.RawMessage(`{"path":"a.go"}`))}},
			types.ToolResultMessage{ToolCallID: id, ToolName: "read", Content: []types.ContentBlock{types.NewText("package a")}},
		}
	}
	reminder := types.UserMessage{Content: types.StringContent("<system-reminder>\nnudge\n</system-reminder>"), Reminder: "nudge"}
	history := append([]types.Message{types.UserMessage{Content: types.StringContent("fix the bug")}}, tool("call_a")...)

	first := buildParams(model, types.Context{SystemPrompt: "system", Messages: slices.Concat(history, []types.Message{reminder}), RequestScoped: 1}, sessionOptions("sess1"))
	if _, ok := first["cache_control"]; ok {
		t.Fatal("request with a request-scope tail kept top-level cache_control, which marks the tail")
	}
	sent := sentMessages(t, first)
	if len(sent) != 5 {
		t.Fatalf("sent %d messages, want system, user, assistant, tool, reminder", len(sent))
	}
	marked := -1
	for i, message := range sent {
		if message.breakpoints > 1 || message.breakpoints == 1 && marked >= 0 {
			t.Fatalf("more than one breakpoint: %+v", sent)
		}
		if message.breakpoints == 1 {
			marked = i
		}
	}
	if marked != 3 || sent[3].role != "tool" {
		t.Fatalf("breakpoint on message %d, want the tool result (3): %+v", marked, sent)
	}

	second := buildParams(model, types.Context{SystemPrompt: "system", Messages: slices.Concat(history, tool("call_b"))}, sessionOptions("sess1"))
	if _, ok := second["cache_control"]; !ok {
		t.Fatal("request without a tail lost top-level automatic caching")
	}
	next := sentMessages(t, second)
	for i := range marked {
		if !bytes.Equal(sent[i].raw, next[i].raw) {
			t.Fatalf("message %d before the breakpoint changed between requests:\n%s\n%s", i, sent[i].raw, next[i].raw)
		}
	}
	var before, after struct {
		Role       string `json:"role"`
		ToolCallID string `json:"tool_call_id"`
		Content    any    `json:"content"`
	}
	if json.Unmarshal(sent[marked].raw, &before) != nil || json.Unmarshal(next[marked].raw, &after) != nil {
		t.Fatal("decode marked message")
	}
	if before.Role != after.Role || before.ToolCallID != after.ToolCallID || contentText(before.Content) != contentText(after.Content) {
		t.Fatalf("marked message differs in the next request:\n%s\n%s", sent[marked].raw, next[marked].raw)
	}
}

// contentText joins a decoded wire content value's text, string or parts.
func contentText(content any) string {
	if text, ok := content.(string); ok {
		return text
	}
	parts, _ := content.([]any)
	var text string
	for _, part := range parts {
		if object, ok := part.(map[string]any); ok {
			value, _ := object["text"].(string)
			text += value
		}
	}
	return text
}
