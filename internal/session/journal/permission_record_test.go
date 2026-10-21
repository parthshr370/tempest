package journal

import (
	"encoding/json"
	"strings"
	"testing"

	types "go.harness.dev/harness/internal/engine/types"
)

func TestPermissionRecordIsNotExecutionIntent(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	run := Meta{RunID: store.NewRunID()}
	appendAll(t, store,
		RunStartedRecord{Meta: run, Model: "faux-1"},
		MessageEntry{Meta: run, Message: types.AssistantMessage{Content: []types.ContentBlock{types.NewToolCall("c", "bash", json.RawMessage(`{"command":"mkdir x"}`))}, StopReason: types.StopToolUse}},
		PermissionRecord{Meta: run, ToolCallID: "c", Tool: "bash", Decision: "allow", Source: "prompt", WaitMS: 42},
		RunFinishedRecord{Meta: run, StopReason: "aborted"},
	)
	closeStore(t, store)
	loaded, err := Load(testContext, store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Diagnostics) != 0 || len(loaded.Interrupted) != 0 {
		t.Fatalf("permission changed reconciliation: %+v", loaded)
	}
	if len(loaded.Messages) != 2 {
		t.Fatalf("messages=%#v", loaded.Messages)
	}
	result, ok := loaded.Messages[1].(types.ToolResultMessage)
	if !ok || len(result.Content) == 0 || !strings.Contains(result.Content[0].Text, "not executed") {
		t.Fatalf("result=%#v", loaded.Messages[1])
	}
	if syncsAfter(KindPermission) {
		t.Fatal("permission records must not fsync")
	}
}
