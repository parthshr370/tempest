package compaction

import (
	"encoding/json"
	"strings"
	"testing"

	ptypes "go.harness.dev/harness/internal/engine/types"
)

func TestFileOpsAndFormatting(t *testing.T) {
	fileOps := CreateFileOps()
	ExtractFileOpsFromMessage(ptypes.AssistantMessage{Content: []ptypes.ContentBlock{
		ptypes.NewToolCall("r", "read", raw(`{"path":"a.txt"}`)),
		ptypes.NewToolCall("w", "write", raw(`{"path":"b.txt"}`)),
		ptypes.NewToolCall("e", "edit", raw(`{"path":"a.txt"}`)),
	}}, fileOps)
	readFiles, modifiedFiles := ComputeFileLists(fileOps)
	if strings.Join(readFiles, ",") != "" || strings.Join(modifiedFiles, ",") != "a.txt,b.txt" {
		t.Fatalf("read=%v modified=%v", readFiles, modifiedFiles)
	}
	formatted := FormatFileOperations([]string{"read.md"}, modifiedFiles)
	for _, want := range []string{"<read-files>", "read.md", "<modified-files>", "a.txt", "b.txt"} {
		if !strings.Contains(formatted, want) {
			t.Fatalf("formatted missing %q: %s", want, formatted)
		}
	}
}

func TestSerializeConversationAndEstimate(t *testing.T) {
	messages := []ptypes.Message{
		ptypes.UserMessage{Content: ptypes.StringContent("hello")},
		ptypes.AssistantMessage{Content: []ptypes.ContentBlock{ptypes.NewText("hi"), ptypes.NewToolCall("x", "read", raw(`{"path":"a","limit":2}`))}},
		ptypes.ToolResultMessage{Content: []ptypes.ContentBlock{ptypes.NewText(strings.Repeat("a", ToolResultMaxChars+4))}},
	}
	serialized := SerializeConversation(messages)
	for _, want := range []string{"[User]: hello", "[Assistant]: hi", "read(path=\"a\", limit=2)", "[... 4 more characters truncated]"} {
		if !strings.Contains(serialized, want) {
			t.Fatalf("serialized missing %q:\n%s", want, serialized)
		}
	}
	if got := EstimateTokens(messages[0]); got != 2 {
		t.Fatalf("EstimateTokens user = %d", got)
	}
}

func TestEstimateContextTokensUsesLastAssistantUsage(t *testing.T) {
	usage := ptypes.Usage{Input: 10, Output: 5, TotalTokens: 20}
	messages := []ptypes.Message{
		ptypes.UserMessage{Content: ptypes.StringContent("old")},
		ptypes.AssistantMessage{Content: []ptypes.ContentBlock{ptypes.NewText("done")}, Usage: usage, StopReason: ptypes.StopStop},
		ptypes.UserMessage{Content: ptypes.StringContent("next")},
	}
	estimate := EstimateContextTokens(messages)
	if estimate.Tokens != 21 || estimate.UsageTokens != 20 || estimate.TrailingTokens != 1 || estimate.LastUsageIndex == nil || *estimate.LastUsageIndex != 1 {
		t.Fatalf("estimate = %+v", estimate)
	}
	if !ShouldCompact(90, 100, CompactionSettings{Enabled: true, ReserveTokens: 20}) {
		t.Fatal("should compact")
	}
}

func TestFindCutPointAndPrepareCompaction(t *testing.T) {
	entries := []SessionEntry{
		{ID: "1", Type: "message", Message: ptypes.UserMessage{Content: ptypes.StringContent(strings.Repeat("u", 40))}},
		{ID: "2", Type: "message", Message: ptypes.AssistantMessage{Content: []ptypes.ContentBlock{ptypes.NewText(strings.Repeat("a", 40))}}},
		{ID: "3", Type: "message", Message: ptypes.UserMessage{Content: ptypes.StringContent("recent")}},
	}
	cut := FindCutPoint(entries, 0, len(entries), 2)
	if cut.FirstKeptEntryIndex != 2 || cut.TurnStartIndex != -1 || cut.IsSplitTurn {
		t.Fatalf("cut = %+v", cut)
	}
	prep := PrepareCompaction(entries, CompactionSettings{Enabled: true, ReserveTokens: 100, KeepRecentTokens: 2})
	if prep == nil || prep.FirstKeptEntryID != "3" || len(prep.MessagesToSummarize) != 2 {
		t.Fatalf("prep = %+v", prep)
	}
}

func TestSummaryEntriesUseWrappedMessages(t *testing.T) {
	branch := getMessageFromEntry(SessionEntry{Type: "branch_summary", Summary: "branch", Timestamp: 1}).(ptypes.UserMessage)
	if got := branch.Content.Text; got != branchSummaryPrefix+"branch"+branchSummarySuffix {
		t.Fatalf("branch summary = %q", got)
	}
	compact := getMessageFromEntry(SessionEntry{Type: "compaction", Summary: "compact", Timestamp: 2}).(ptypes.UserMessage)
	if got := compact.Content.Text; got != compactionSummaryPrefix+"compact"+compactionSummarySuffix {
		t.Fatalf("compaction summary = %q", got)
	}
	if EstimateTokens(compact) != (len(compactionSummaryPrefix+"compact"+compactionSummarySuffix)+3)/4 {
		t.Fatalf("estimate mismatch for wrapped compaction summary")
	}
}

func TestBuildSessionContextSkipsPriorCompactedEntries(t *testing.T) {
	entries := []SessionEntry{
		{ID: "old", Type: "message", Message: ptypes.UserMessage{Content: ptypes.StringContent(strings.Repeat("o", 400))}},
		{ID: "keep", Type: "message", Message: ptypes.UserMessage{Content: ptypes.StringContent("keep")}},
		{ID: "c1", Type: "compaction", Summary: "summary", FirstKeptEntryID: "keep"},
		{ID: "new", Type: "message", Message: ptypes.UserMessage{Content: ptypes.StringContent("new")}},
	}
	contextEntries := buildContextEntries(entries)
	if len(contextEntries) != 3 || contextEntries[0].ID != "c1" || contextEntries[1].ID != "keep" || contextEntries[2].ID != "new" {
		t.Fatalf("context entries = %+v", contextEntries)
	}
	prep := PrepareCompaction(entries, CompactionSettings{Enabled: true, ReserveTokens: 100, KeepRecentTokens: 1})
	if prep == nil {
		t.Fatal("expected compaction prep")
	}
	inflated := EstimateContextTokens([]ptypes.Message{
		entries[0].Message,
		entries[1].Message,
		getMessageFromEntry(entries[2]),
		entries[3].Message,
	}).Tokens
	if prep.TokensBefore >= inflated {
		t.Fatalf("tokensBefore still inflated: got %d inflated %d", prep.TokensBefore, inflated)
	}
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }
