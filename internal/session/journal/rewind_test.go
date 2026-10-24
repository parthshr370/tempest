package journal

import (
	"encoding/json"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
)

func TestRewindAbandonsInterruptedToolIntents(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	run := Meta{RunID: store.NewRunID()}
	appendAll(t, store,
		RunStartedRecord{Meta: run, Model: "faux"},
		MessageEntry{Meta: run, Message: types.AssistantMessage{Content: []types.ContentBlock{types.NewToolCall("abandoned", "bash", json.RawMessage(`{"command":"touch file"}`))}, StopReason: types.StopToolUse}},
		ToolStartedRecord{Meta: run, ToolCallID: "abandoned", Tool: "bash"},
	)
	entry, err := NewRewindEntry("checkpoint", "findings", []types.Message{types.UserMessage{Content: types.StringContent("retained boundary")}})
	if err != nil {
		t.Fatal(err)
	}
	appendAll(t, store, entry)
	loaded, err := Load(t.Context(), store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Interrupted) != 0 {
		t.Fatalf("discarded intent blocks child resume: %+v", loaded.Interrupted)
	}
	if len(loaded.Messages) != 1 {
		t.Fatalf("discarded call leaked into transcript: %+v", loaded.Messages)
	}
}
