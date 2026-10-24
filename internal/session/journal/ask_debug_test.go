package journal

import (
	"encoding/json"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
)

func TestAskDebugInterruptedCallsSettleUnknown(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	run := Meta{RunID: store.NewRunID()}
	calls := types.AssistantMessage{Content: []types.ContentBlock{types.NewToolCall("question", "ask", json.RawMessage(`{"questions":[]}`)), types.NewToolCall("adapter", "debug", json.RawMessage(`{"action":"continue"}`))}, StopReason: types.StopToolUse}
	appendAll(t, store, RunStartedRecord{Meta: run, Model: "faux"}, MessageEntry{Meta: run, Message: userMessage("inspect")}, MessageEntry{Meta: run, Message: calls}, ToolStartedRecord{Meta: run, ToolCallID: "question", Tool: "ask"}, ToolStartedRecord{Meta: run, ToolCallID: "adapter", Tool: "debug"})
	closeStore(t, store)
	loaded, err := Load(testContext, store.Path())
	if err != nil {
		t.Fatal(err)
	}
	results := map[string]string{}
	for _, message := range loaded.Messages {
		if result, ok := message.(types.ToolResultMessage); ok {
			if !result.IsError {
				t.Fatal("interrupted tool was marked successful")
			}
			results[result.ToolName] = result.Content[0].Text
		}
	}
	for name, text := range results {
		if !strings.Contains(text, "outcome unknown") || !strings.Contains(text, textNoRerun) {
			t.Fatalf("%s unsafe result: %s", name, text)
		}
	}
	if !strings.Contains(results["ask"], "isn't asked again automatically") || !strings.Contains(results["debug"], "gone after resume") {
		t.Fatalf("missing recovery semantics: %+v", results)
	}
}
