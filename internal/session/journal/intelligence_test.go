package journal

import (
	"encoding/json"
	"strings"
	"testing"

	ptypes "go.harness.dev/harness/internal/engine/types"
)

// TestRenamePlanRecoveryNeverAppliesRemainingEdits models a process killed
// after lsp_rename returned its plan and the first planned file was edited:
// the second edit started but recorded no result, and a manifest_diff and an
// lsp_definition were in flight. Reduction must not run or apply anything;
// it words each missing result so the model decides what to redo.
func TestRenamePlanRecoveryNeverAppliesRemainingEdits(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	run := Meta{RunID: store.NewRunID()}
	plan := ptypes.AssistantMessage{Content: []ptypes.ContentBlock{ptypes.NewToolCall("plan", "lsp_rename", json.RawMessage(`{"path":"a.go","line":1,"column":6,"new_name":"Renamed"}`))}, StopReason: ptypes.StopToolUse}
	apply := ptypes.AssistantMessage{Content: []ptypes.ContentBlock{
		ptypes.NewToolCall("first", "edit", json.RawMessage(`{"path":"a.go","edits":[]}`)),
		ptypes.NewToolCall("second", "edit", json.RawMessage(`{"path":"b.go","edits":[]}`)),
		ptypes.NewToolCall("diff", "manifest_diff", json.RawMessage(`{"before":"old","after":"new"}`)),
		ptypes.NewToolCall("definition", "lsp_definition", json.RawMessage(`{"path":"a.go","line":1,"column":6}`)),
	}, StopReason: ptypes.StopToolUse}
	appendAll(t, store,
		RunStartedRecord{Meta: run, Model: "m"},
		MessageEntry{Meta: run, Message: userMessage("rename")},
		MessageEntry{Meta: run, Message: plan},
		ToolStartedRecord{Meta: run, ToolCallID: "plan", Tool: "lsp_rename"},
		ToolFinishedRecord{Meta: run, ToolCallID: "plan", OK: true},
		MessageEntry{Meta: run, Message: ptypes.ToolResultMessage{ToolCallID: "plan", ToolName: "lsp_rename", Content: []ptypes.ContentBlock{ptypes.NewText(`{"applied":false}`)}}},
		MessageEntry{Meta: run, Message: apply},
		ToolStartedRecord{Meta: run, ToolCallID: "first", Tool: "edit"},
		ToolFinishedRecord{Meta: run, ToolCallID: "first", OK: true},
		MessageEntry{Meta: run, Message: ptypes.ToolResultMessage{ToolCallID: "first", ToolName: "edit", Content: []ptypes.ContentBlock{ptypes.NewText("Successfully replaced 1 block(s) in a.go.")}}},
		ToolStartedRecord{Meta: run, ToolCallID: "second", Tool: "edit"},
		ToolStartedRecord{Meta: run, ToolCallID: "diff", Tool: "manifest_diff"},
		ToolStartedRecord{Meta: run, ToolCallID: "definition", Tool: "lsp_definition"},
	)
	closeStore(t, store)
	loaded, err := Load(testContext, store.Path())
	if err != nil {
		t.Fatal(err)
	}
	results := map[string]ptypes.ToolResultMessage{}
	for _, message := range loaded.Messages {
		if result, ok := message.(ptypes.ToolResultMessage); ok {
			results[result.ToolCallID] = result
		}
	}
	if text := results["first"].Content[0].Text; !strings.HasPrefix(text, "Successfully replaced") || results["first"].IsError {
		t.Fatalf("recorded edit result changed: %#v", results["first"])
	}
	for _, id := range []string{"second", "definition"} {
		text := results[id].Content[0].Text
		if !results[id].IsError || !strings.Contains(text, "outcome unknown") || !strings.Contains(text, textNoRerun) || strings.Contains(text, textSafeToRerun) {
			t.Fatalf("%s must be an unknown outcome that is never re-run: %q", id, text)
		}
	}
	if text := results["diff"].Content[0].Text; !strings.Contains(text, "outcome unknown") || !strings.Contains(text, textNoRerun) {
		t.Fatalf("old manifest_diff intent must remain Never: %q", text)
	}
	interrupted := map[string]bool{}
	for _, item := range loaded.Interrupted {
		interrupted[item.ToolCallID] = true
	}
	if len(interrupted) != 3 || !interrupted["second"] || !interrupted["diff"] || !interrupted["definition"] {
		t.Fatalf("interrupted = %#v", loaded.Interrupted)
	}
}
