package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/tempest"
)

func runTestSession(t *testing.T, f *faux.Faux, budget tempest.Budget) (*tempest.Session, error) {
	t.Helper()
	work := agent.AgentTool{
		Tool: types.Tool{Name: "work", Description: "work tool", Parameters: json.RawMessage(`{"type":"object"}`)},
		Execute: func(context.Context, string, json.RawMessage, agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			return agent.AgentToolResult{Content: []types.ContentBlock{types.NewText("worked")}}, nil
		},
	}
	s, err := tempest.Open(context.Background(), tempest.Options{
		SessionDir: t.TempDir(), Cwd: t.TempDir(),
		New:   &tempest.NewSession{Model: f.Model().ID, Budget: budget, Tools: []tempest.Tool{work}},
		Stack: &tempest.StackConfig{Model: f.Model(), StreamFn: f.StreamSimple},
	})
	if err == nil {
		t.Cleanup(func() {
			if err := s.Close(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	return s, err
}

func TestPublicRunCompletesWhenAssistantStopsCallingTools(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(
		faux.Respond(faux.Text("working"), faux.ToolCall("work", json.RawMessage(`{}`), "tc1")),
		faux.Respond(faux.Text("done")),
	)
	s, err := runTestSession(t, f, tempest.Budget{})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := s.Prompt(context.Background(), tempest.Prompt{Text: "start"})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.StopReason != agent.StopCompleted || outcome.Turns != 2 || s.LastAssistantText() != "done" {
		t.Fatalf("outcome = %+v, text = %q, want completed after 2 turns ending in done", outcome, s.LastAssistantText())
	}
}

func TestPublicRunAppliesBudget(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(
		faux.Respond(faux.Text("first"), faux.ToolCall("work", json.RawMessage(`{}`), "tc1")),
		faux.Respond(faux.Text("never")),
	)
	s, err := runTestSession(t, f, tempest.Budget{MaxTurns: 1})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := s.Prompt(context.Background(), tempest.Prompt{Text: "start"})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.StopReason != agent.StopMaxTurns || f.CallCount() != 1 || s.LastAssistantText() != "first" {
		t.Fatalf("outcome = %+v calls=%d text=%q, want max_turns after 1 call", outcome, f.CallCount(), s.LastAssistantText())
	}
}

func TestPublicRunReportsCanceledContextAsAborted(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("never")))
	s, err := runTestSession(t, f, tempest.Budget{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	outcome, err := s.Prompt(ctx, tempest.Prompt{Text: "start"})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.StopReason != agent.StopAborted || f.CallCount() != 0 {
		t.Fatalf("outcome = %+v calls=%d, want aborted without provider calls", outcome, f.CallCount())
	}
}

func TestPublicRunRefusesCostBudgetForUnknownPrice(t *testing.T) {
	f := faux.New(faux.Options{Models: []faux.ModelDef{{ID: "unpriced", Cost: types.ModelCost{Unknown: true}}}})
	_, err := runTestSession(t, f, tempest.Budget{MaxCostUSD: 1})
	if !errors.Is(err, agent.ErrUnknownPrice) || f.CallCount() != 0 {
		t.Fatalf("err = %v calls=%d, want ErrUnknownPrice before any model call", err, f.CallCount())
	}
}

func TestPublicPromptPreservesOrderedMediaRefs(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("done")))
	s, err := runTestSession(t, f, tempest.Budget{})
	if err != nil {
		t.Fatal(err)
	}
	prompt := tempest.Prompt{Text: "inspect these", Media: []tempest.Media{
		types.NewImageRef("session_local", "image-key", "image/png", "preview.png", 11),
		types.NewDocumentRef("session_local", "document-key", "application/pdf", "report.pdf", 22, 3),
	}}
	if _, err := s.Prompt(context.Background(), prompt); err != nil {
		t.Fatal(err)
	}
	history := s.Messages()
	if len(history) < 1 {
		t.Fatal("initial user message missing from history")
	}
	userMessage, ok := history[0].(types.UserMessage)
	if !ok {
		t.Fatalf("initial message type = %T, want types.UserMessage", history[0])
	}
	blocks := userMessage.Content.Blocks
	if len(blocks) != 3 || blocks[0].Type != types.BlockText || blocks[0].Text != prompt.Text {
		t.Fatalf("initial blocks = %#v, want text followed by two refs", blocks)
	}
	if image := blocks[1]; image.Type != types.BlockImageRef || image.RefKey != "image-key" || image.RefFilename != "preview.png" || image.RefSizeBytes != 11 {
		t.Fatalf("image block = %#v", image)
	}
	if doc := blocks[2]; doc.Type != types.BlockDocumentRef || doc.RefKey != "document-key" || doc.RefFilename != "report.pdf" || doc.RefSizeBytes != 22 || doc.RefPageCount != 3 {
		t.Fatalf("document block = %#v", doc)
	}
}
