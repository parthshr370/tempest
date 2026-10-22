package mcpserver

import (
	"fmt"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
)

func TestMCPResumeAfterConnectionRestartUsesJournalWorkspace(t *testing.T) {
	f := faux.New(faux.Options{})
	f.AppendResponses(faux.Respond(faux.Text("old answer")), func(c types.Context, _ *types.StreamOptions, _ faux.State, _ types.Model) (types.AssistantMessage, error) {
		if len(c.Messages) != 3 {
			return types.AssistantMessage{}, fmt.Errorf("resumed transcript has %d messages, want 3", len(c.Messages))
		}
		return types.AssistantMessage{Content: []types.ContentBlock{types.NewText("new answer")}}, nil
	})
	opts := options(t, f)
	firstClient := connect(t, opts, "2025-11-25", map[string]any{}, nil)
	first, err := firstClient.run(RunInput{Prompt: "first"}, nil)
	if err != nil || first.IsError || first.StructuredContent == nil {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	firstClient.stop(t)
	opts.Cwd = t.TempDir()
	secondClient := connect(t, opts, "2025-06-18", map[string]any{}, nil)
	second, err := secondClient.run(RunInput{Prompt: "second", SessionID: first.StructuredContent.SessionID}, nil)
	if err != nil || second.IsError || second.StructuredContent == nil || second.StructuredContent.Text != "new answer" || second.StructuredContent.SessionID != first.StructuredContent.SessionID {
		t.Fatalf("resume=%+v err=%v", second, err)
	}
}
