package tempest

import (
	"encoding/json"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/session/journal"
)

// TestToolCallLimitRefusesBatchAndKeepsJournalValid: through the durable
// SDK, a response whose calls exceed the tool-call cap runs none of them,
// every call is answered in the journal, and the run's usage counts only
// the calls that ran. The next run, under a higher cap, continues normally.
func TestToolCallLimitRefusesBatchAndKeepsJournalValid(t *testing.T) {
	ctx := t.Context()
	provider := faux.New(faux.Options{})
	owner := openFaux(t, provider, nil)
	if err := owner.SetBudget(Budget{MaxToolCalls: 2}); err != nil {
		t.Fatal(err)
	}
	read := func(id string) types.ContentBlock {
		return faux.ToolCall("read", json.RawMessage(`{"path":"missing-`+id+`"}`), id)
	}
	provider.SetResponses(faux.Respond(read("one")), faux.Respond(read("two"), read("three")), faux.Respond(faux.Text("never")))
	result, err := owner.Prompt(ctx, Prompt{Text: "read them"})
	if err != nil || result.StopReason != agent.StopMaxToolCalls || provider.CallCount() != 2 || result.Usage.ToolCalls != 1 {
		t.Fatalf("result = %+v err = %v calls = %d, want max_tool_calls after one call ran", result, err, provider.CallCount())
	}
	loaded, err := journal.Load(ctx, owner.core.store.Path())
	if err != nil || loaded.LastRun == nil || loaded.LastRun.StopReason != string(agent.StopMaxToolCalls) {
		t.Fatalf("journal last run = %+v err = %v", loaded.LastRun, err)
	}
	if usage, _, _ := loaded.LogicalUsage(loaded.LastRun.LogicalRunID); usage.ToolCalls != 1 {
		t.Fatalf("journaled tool calls = %d, want 1", usage.ToolCalls)
	}
	answered := map[string]string{}
	for _, message := range loaded.Messages {
		if result, ok := message.(types.ToolResultMessage); ok {
			answered[result.ToolCallID] = result.Content[0].Text
		}
	}
	for _, id := range []string{"two", "three"} {
		if !strings.HasPrefix(answered[id], "Not run: this response asked for 2 tool calls") {
			t.Fatalf("refused call %s answered %q", id, answered[id])
		}
	}
	if len(answered) != 3 {
		t.Fatalf("answered calls = %v, want all three", answered)
	}

	if err := owner.SetBudget(Budget{MaxToolCalls: 2}); err != nil {
		t.Fatal(err)
	}
	provider.SetResponses(faux.Respond(read("four"), read("five")), faux.Respond(faux.Text("done")))
	result, err = owner.Prompt(ctx, Prompt{Text: "read two more"})
	if err != nil || result.StopReason != agent.StopCompleted || result.Usage.ToolCalls != 2 {
		t.Fatalf("next run = %+v err = %v, want a fresh allowance per run", result, err)
	}
}
