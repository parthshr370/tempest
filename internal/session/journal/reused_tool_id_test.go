package journal

import (
	"encoding/json"
	"strings"
	"testing"

	ptypes "go.harness.dev/harness/internal/engine/types"
)

// An OpenAI-compatible endpoint (or a faux script) can reuse a tool-call id in
// a later turn of the same run. If the process dies while the second call
// runs, its tool_started is dropped as a "duplicate" and the first call's
// tool_finished is applied to it, so the model is told "result lost: the tool
// succeeded" for a call whose outcome is unknown, and Interrupted is empty.
// Reproduced on the real binary with context/stress/dupid.sh.
func TestReusedToolCallIDInLaterTurnIsOutcomeUnknown(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	run := Meta{RunID: store.NewRunID()}
	call := func(command string) ptypes.AssistantMessage {
		return ptypes.AssistantMessage{
			Content:    []ptypes.ContentBlock{ptypes.NewToolCall("dup", "bash", json.RawMessage(`{"command":"`+command+`"}`))},
			StopReason: ptypes.StopToolUse,
		}
	}
	appendAll(t, store,
		RunStartedRecord{Meta: run, Model: "faux-1", Provider: "faux"},
		MessageEntry{Meta: run, Message: userMessage("go")},
		MessageEntry{Meta: run, Message: call("echo first")},
		ToolStartedRecord{Meta: run, ToolCallID: "dup", Tool: "bash"},
		ToolFinishedRecord{Meta: run, ToolCallID: "dup", OK: true},
		MessageEntry{Meta: run, Message: ptypes.ToolResultMessage{ToolCallID: "dup", ToolName: "bash", Content: []ptypes.ContentBlock{ptypes.NewText("ok")}}},
		MessageEntry{Meta: run, Message: call("sleep 2 && echo second")},
		ToolStartedRecord{Meta: run, ToolCallID: "dup", Tool: "bash"},
	)
	closeStore(t, store)

	loaded, err := Load(testContext, store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Interrupted) != 1 {
		t.Errorf("Interrupted = %+v, want the second dup call", loaded.Interrupted)
	}
	last, ok := loaded.Messages[len(loaded.Messages)-1].(ptypes.ToolResultMessage)
	if !ok {
		t.Fatalf("last message = %#v, want a synthesized tool result", loaded.Messages[len(loaded.Messages)-1])
	}
	text := last.Content[0].Text
	if !strings.HasPrefix(text, "outcome unknown") {
		t.Errorf("second call's synthesized result = %q, want outcome unknown", text)
	}
}

func TestReusedToolCallIDOccurrencesKeepDistinctOutcomes(t *testing.T) {
	for _, test := range []struct {
		name      string
		sameTurn  bool
		hasResult bool
	}{
		{name: "one turn with real first result", sameTurn: true, hasResult: true},
		{name: "one turn with lost first result", sameTurn: true},
		{name: "later turn with lost first result"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newTestStore(t, t.TempDir())
			run := Meta{RunID: store.NewRunID()}
			first := ptypes.NewToolCall("dup", "bash", json.RawMessage(`{"command":"first"}`))
			second := ptypes.NewToolCall("dup", "bash", json.RawMessage(`{"command":"second"}`))
			assistant := ptypes.AssistantMessage{Content: []ptypes.ContentBlock{first}, StopReason: ptypes.StopToolUse}
			if test.sameTurn {
				assistant.Content = append(assistant.Content, second)
			}
			appendAll(t, store,
				RunStartedRecord{Meta: run, Model: "m"},
				MessageEntry{Meta: run, Message: assistant},
				ToolStartedRecord{Meta: run, ToolCallID: "dup", Tool: "bash", Args: first.Arguments},
			)
			if test.sameTurn {
				appendAll(t, store, ToolStartedRecord{Meta: run, ToolCallID: "dup", Tool: "bash", Args: second.Arguments})
			}
			appendAll(t, store, ToolFinishedRecord{Meta: run, ToolCallID: "dup", OK: true})
			if test.hasResult {
				appendAll(t, store, MessageEntry{Meta: run, Message: ptypes.ToolResultMessage{
					ToolCallID: "dup", ToolName: "bash", Content: []ptypes.ContentBlock{ptypes.NewText("first result")},
				}})
			}
			if !test.sameTurn {
				appendAll(t, store,
					MessageEntry{Meta: run, Message: ptypes.AssistantMessage{Content: []ptypes.ContentBlock{second}, StopReason: ptypes.StopToolUse}},
					ToolStartedRecord{Meta: run, ToolCallID: "dup", Tool: "bash", Args: second.Arguments},
				)
			}
			closeStore(t, store)
			loaded, err := Load(testContext, store.Path())
			if err != nil {
				t.Fatal(err)
			}
			if len(loaded.Interrupted) != 1 || string(loaded.Interrupted[0].Args) != string(second.Arguments) {
				t.Fatalf("interrupted = %#v, want only the second occurrence", loaded.Interrupted)
			}
			var results []ptypes.ToolResultMessage
			for _, message := range loaded.Messages {
				if result, ok := message.(ptypes.ToolResultMessage); ok {
					results = append(results, result)
				}
			}
			if len(results) != 2 {
				t.Fatalf("results = %#v, want two occurrences", results)
			}
			wantFirst := "result lost: the tool succeeded"
			if test.hasResult {
				wantFirst = "first result"
			}
			if !strings.HasPrefix(results[0].Content[0].Text, wantFirst) {
				t.Fatalf("first result = %#v, want %q", results[0], wantFirst)
			}
			if !results[1].IsError || !strings.HasPrefix(results[1].Content[0].Text, "outcome unknown") {
				t.Fatalf("second result = %#v, want outcome unknown", results[1])
			}
		})
	}
}
