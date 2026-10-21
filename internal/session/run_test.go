package session

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/document"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
)

func runTestAgent(f *faux.Faux) *agent.Agent {
	work := agent.AgentTool{
		Tool: types.Tool{Name: "work", Description: "work tool", Parameters: json.RawMessage(`{"type":"object"}`)},
		Execute: func(ctx context.Context, id string, params json.RawMessage, onUpdate agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			return agent.AgentToolResult{Content: []types.ContentBlock{types.NewText("worked")}}, nil
		},
	}
	return agent.NewAgent(agent.AgentOptions{
		InitialState: &agent.AgentState{SystemPrompt: "test", Model: f.Model(), Tools: []agent.AgentTool{work}},
		StreamFn:     f.StreamSimple,
	})
}

func TestRunCompletesWhenAssistantStopsCallingTools(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(
		faux.Respond(faux.Text("working"), faux.ToolCall("work", json.RawMessage(`{}`), "tc1")),
		faux.Respond(faux.Text("done")),
	)

	outcome, err := Run(context.Background(), runTestAgent(f), InitialPrompt{Text: "start"}, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.StopReason != agent.StopCompleted || outcome.Turns != 2 || outcome.LastAssistantText != "done" || outcome.Elapsed <= 0 {
		t.Fatalf("outcome = %+v, want completed after 2 turns ending in \"done\"", outcome)
	}
}

func TestRunAppliesBudget(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(
		faux.Respond(faux.Text("first"), faux.ToolCall("work", json.RawMessage(`{}`), "tc1")),
		faux.Respond(faux.Text("never")),
	)

	outcome, err := Run(context.Background(), runTestAgent(f), InitialPrompt{Text: "start"}, RunOptions{Budget: agent.Budget{MaxTurns: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.StopReason != agent.StopMaxTurns || f.CallCount() != 1 || outcome.LastAssistantText != "first" {
		t.Fatalf("outcome = %+v calls=%d, want max_turns after 1 call", outcome, f.CallCount())
	}
}

func TestRunReportsCanceledContextAsAborted(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("never")))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	outcome, err := Run(ctx, runTestAgent(f), InitialPrompt{Text: "start"}, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.StopReason != agent.StopAborted {
		t.Fatalf("outcome = %+v, want aborted", outcome)
	}
}

func TestRunRefusesCostBudgetForUnknownPrice(t *testing.T) {
	f := faux.New(faux.Options{Models: []faux.ModelDef{{ID: "unpriced", Cost: types.ModelCost{Unknown: true}}}})

	_, err := Run(context.Background(), runTestAgent(f), InitialPrompt{Text: "start"}, RunOptions{Budget: agent.Budget{MaxCostUSD: 1}})
	if !errors.Is(err, agent.ErrUnknownPrice) || f.CallCount() != 0 {
		t.Fatalf("err = %v calls=%d, want ErrUnknownPrice before any model call", err, f.CallCount())
	}
}

func TestRunInitialPromptPreservesOrderedMediaRefs(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("done")))
	agt := runTestAgent(f)
	prompt := InitialPrompt{
		Text: "inspect these",
		Media: []document.MediaRef{
			{Filename: "preview.png", MediaType: "image/png", SizeBytes: 11, Blob: document.BlobRef{Store: document.StoreSessionLocal, Key: "image-key"}},
			{Filename: "report.pdf", MediaType: "application/pdf", SizeBytes: 22, PageCount: 3, Blob: document.BlobRef{Store: document.StoreSessionLocal, Key: "document-key"}},
		},
	}

	if _, err := Run(context.Background(), agt, prompt, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	history := agt.State().Messages
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
	if documentBlock := blocks[2]; documentBlock.Type != types.BlockDocumentRef || documentBlock.RefKey != "document-key" || documentBlock.RefFilename != "report.pdf" || documentBlock.RefSizeBytes != 22 || documentBlock.RefPageCount != 3 {
		t.Fatalf("document block = %#v", documentBlock)
	}
}
