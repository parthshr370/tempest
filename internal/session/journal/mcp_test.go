package journal

import (
	"encoding/json"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
)

func TestMCPReadIsNeverReplayed(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	run := Meta{RunID: store.NewRunID()}
	appendAll(t, store, RunStartedRecord{Meta: run, Model: "faux"}, MessageEntry{Meta: run, Message: types.AssistantMessage{Content: []types.ContentBlock{types.NewToolCall("call", "mcp__x__read", json.RawMessage(`{}`))}, StopReason: types.StopToolUse}}, ToolStartedRecord{Meta: run, ToolCallID: "call", Tool: "mcp__x__read"})
	closeStore(t, store)
	loaded, err := Load(testContext, store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Interrupted) != 1 {
		t.Fatalf("interrupted: %#v", loaded.Interrupted)
	}
	last := loaded.Messages[len(loaded.Messages)-1].(types.ToolResultMessage)
	if !last.IsError || !strings.Contains(last.Content[0].Text, "outcome unknown") || !strings.Contains(last.Content[0].Text, textNoRerun) || strings.Contains(last.Content[0].Text, "safe to run again") {
		t.Fatalf("unsafe retry advice: %#v", last)
	}
}
