package compaction

import (
	"encoding/json"
	"slices"
	"strconv"
	"testing"

	"go.harness.dev/harness/internal/agent"
	ptypes "go.harness.dev/harness/internal/engine/types"
)

// The conversation ApplyCompaction rebuilds shows the same loaded tools, in
// the same order, as the one it replaces, whether the summary stands alone
// or merges into the first kept user message, and whether or not the
// conversation was compacted before.
func TestApplyCompactionKeepsLoadedTools(t *testing.T) {
	search := func(id string, loaded ...string) []ptypes.Message {
		details, err := json.Marshal(map[string][]string{"loaded": loaded})
		if err != nil {
			t.Fatal(err)
		}
		return []ptypes.Message{
			ptypes.AssistantMessage{Content: []ptypes.ContentBlock{ptypes.NewToolCall(id, agent.ToolSearchName, json.RawMessage(`{}`))}},
			ptypes.ToolResultMessage{ToolCallID: id, ToolName: agent.ToolSearchName, Content: []ptypes.ContentBlock{ptypes.NewText("loaded")}, Details: details},
		}
	}
	call := func(id, name string) []ptypes.Message {
		return []ptypes.Message{
			ptypes.AssistantMessage{Content: []ptypes.ContentBlock{ptypes.NewToolCall(id, name, json.RawMessage(`{}`))}},
			ptypes.ToolResultMessage{ToolCallID: id, ToolName: name, Content: []ptypes.ContentBlock{ptypes.NewText("ran")}},
		}
	}
	earlier := slices.Concat(
		[]ptypes.Message{ptypes.UserMessage{Content: ptypes.StringContent("earlier summary"), LoadedTools: []string{agent.ToolSearchName, "mcp__s__delta"}}},
		search("s1", "mcp__s__beta", "mcp__s__gamma"),
		search("s2", "mcp__s__alpha"),
	)
	for _, tc := range []struct {
		name string
		kept []ptypes.Message
	}{
		{"summary merged into the kept user", slices.Concat([]ptypes.Message{userMsg("next")}, call("c1", "mcp__s__alpha"), call("c2", "mcp__s__beta"))},
		{"summary before a kept assistant", slices.Concat(call("c1", "mcp__s__alpha"), call("c2", "mcp__s__beta"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			messages := slices.Concat(earlier, tc.kept)
			entries := make([]SessionEntry, 0, len(messages))
			for i, message := range messages {
				entries = append(entries, msgEntry(strconv.Itoa(i+1), message))
			}
			prep := &CompactionPreparation{FirstKeptEntryID: strconv.Itoa(len(earlier) + 1), MessagesToSummarize: earlier}
			rebuilt := ApplyCompaction(entries, prep, "SUM")
			want := []string{agent.ToolSearchName, "mcp__s__delta", "mcp__s__beta", "mcp__s__gamma", "mcp__s__alpha"}
			if got := agent.LoadedTools(messages); !slices.Equal(got, want) {
				t.Fatalf("fixture loaded tools = %v, want %v", got, want)
			}
			if got := agent.LoadedTools(rebuilt); !slices.Equal(got, want) {
				t.Fatalf("loaded tools after compaction = %v, want %v", got, want)
			}
			raw, err := json.Marshal(rebuilt[0])
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := ptypes.UnmarshalMessage(raw)
			if err != nil {
				t.Fatal(err)
			}
			if got := agent.LoadedTools([]ptypes.Message{decoded}); !slices.Equal(got, want) {
				t.Fatalf("journaled summary loads %v, want %v", got, want)
			}
		})
	}
}
