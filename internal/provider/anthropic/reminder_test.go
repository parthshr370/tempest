package anthropic

import (
	"context"
	"encoding/json"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
)

// A request-scope reminder at the tail is not in the next request, so the
// message cache breakpoint goes on the block before it: on the tool result,
// which the next request still sends. A conversation reminder (not request
// scoped) keeps the breakpoint, since later requests carry it.
func TestRequestScopedTailSitsAfterCacheBreakpoint(t *testing.T) {
	model := baseModel()
	reminder := types.UserMessage{Content: types.StringContent("<system-reminder>\nnudge\n</system-reminder>"), Reminder: "nudge"}
	messages := []types.Message{
		types.UserMessage{Content: types.StringContent("go")},
		types.AssistantMessage{API: model.API, Provider: model.Provider, Model: model.ID, Content: []types.ContentBlock{types.NewToolCall("tc1", "work", json.RawMessage(`{}`))}},
		types.ToolResultMessage{ToolCallID: "tc1", ToolName: "work", Content: []types.ContentBlock{types.NewText("ok")}},
		reminder,
	}
	breakpoints := func(scoped int) []string {
		t.Helper()
		params, err := buildParams(context.Background(), model, types.Context{Messages: messages, RequestScoped: scoped}, false, &Options{})
		if err != nil {
			t.Fatal(err)
		}
		var sent []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		raw, _ := json.Marshal(params["messages"])
		if err := json.Unmarshal(raw, &sent); err != nil {
			t.Fatal(err)
		}
		if len(sent) != 4 {
			t.Fatalf("messages = %s", raw)
		}
		var at []string
		for _, message := range sent {
			var blocks []struct {
				Type         string          `json:"type"`
				CacheControl json.RawMessage `json:"cache_control"`
			}
			if json.Unmarshal(message.Content, &blocks) != nil {
				continue // plain string content carries no breakpoint
			}
			for _, block := range blocks {
				if len(block.CacheControl) > 0 {
					at = append(at, block.Type)
				}
			}
		}
		return at
	}
	if got := breakpoints(0); len(got) != 1 || got[0] != "text" {
		t.Fatalf("conversation reminder breakpoints = %v, want on its text", got)
	}
	if got := breakpoints(1); len(got) != 1 || got[0] != "tool_result" {
		t.Fatalf("request-scope reminder breakpoints = %v, want on the tool result only", got)
	}
}
