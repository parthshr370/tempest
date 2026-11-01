package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
)

const testWorkspaceListing = "<workspace_files source=\"git ls-files\" paths=\"1\">\nFiles in the working directory.\na.go\n</workspace_files>"

// The listing rides at the head of the first user message of every request,
// never in the system prompt or the stored transcript, and its bytes are the
// same in every request of the run.
func TestWorkspaceListingLeadsFirstUserMessageAndLeavesSystemPromptAlone(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "a.go"), []byte("package a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := faux.New(faux.Options{})
	var mu sync.Mutex
	var requests []ptypes.Context
	record := func(reply faux.ResponseStep) faux.ResponseStep {
		return func(c ptypes.Context, options *ptypes.StreamOptions, state faux.State, model ptypes.Model) (ptypes.AssistantMessage, error) {
			mu.Lock()
			requests = append(requests, c)
			mu.Unlock()
			return reply(c, options, state, model)
		}
	}
	provider.SetResponses(
		record(faux.Respond(faux.ToolCall("read", json.RawMessage(`{"path":"a.go"}`), "call-1"))),
		record(faux.Respond(faux.Text("done"))),
	)
	baseline, err := BuildAgentStack(StackConfig{Cwd: cwd, Model: provider.Model(), StreamFn: provider.StreamSimple})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := baseline.Close(); err != nil {
			t.Error(err)
		}
	})
	stack, err := BuildAgentStack(StackConfig{Cwd: cwd, Model: provider.Model(), StreamFn: provider.StreamSimple, WorkspaceListing: testWorkspaceListing})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := stack.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := stack.Agent.PromptText(context.Background(), "fix a.go"); err != nil {
		t.Fatal(err)
	}

	if len(requests) != 2 {
		t.Fatalf("requests = %d", len(requests))
	}
	var firstMessage []byte
	for index, request := range requests {
		if request.SystemPrompt != baseline.Agent.State().SystemPrompt || strings.Contains(request.SystemPrompt, "workspace_files") {
			t.Fatalf("request %d system prompt differs from a stack without a listing", index)
		}
		user, ok := request.Messages[0].(ptypes.UserMessage)
		if !ok || len(user.Content.Blocks) != 2 || user.Content.Blocks[0].Text != testWorkspaceListing || user.Content.Blocks[1].Text != "fix a.go" {
			t.Fatalf("request %d first message = %#v", index, request.Messages[0])
		}
		for _, later := range request.Messages[1:] {
			raw, err := json.Marshal(later)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "workspace_files") {
				t.Fatalf("request %d repeats the listing in %s", index, raw)
			}
		}
		raw, err := json.Marshal(request.Messages[0])
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			firstMessage = raw
		} else if string(raw) != string(firstMessage) {
			t.Fatalf("first message changed between requests:\n%s\n%s", firstMessage, raw)
		}
	}
	stored, err := json.Marshal(stack.Agent.State().Messages[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), "workspace_files") {
		t.Fatalf("transcript stores the listing: %s", stored)
	}
}

func TestWorkspaceListingTransformKeepsStringContentAndSkipsEmptyListing(t *testing.T) {
	if transform := withWorkspaceListing(""); transform != nil {
		t.Fatal("an empty listing installed a transform")
	}
	transform := withWorkspaceListing(testWorkspaceListing)
	original := []ptypes.Message{
		ptypes.AssistantMessage{Content: []ptypes.ContentBlock{ptypes.NewText("earlier")}},
		&ptypes.UserMessage{Content: ptypes.StringContent("task")},
		ptypes.UserMessage{Content: ptypes.StringContent("second")},
	}
	placed := transform(context.Background(), original)
	user, ok := placed[1].(ptypes.UserMessage)
	if !ok || len(user.Content.Blocks) != 2 || user.Content.Blocks[0].Text != testWorkspaceListing || user.Content.Blocks[1].Text != "task" {
		t.Fatalf("placed = %#v", placed[1])
	}
	if second := placed[2].(ptypes.UserMessage); second.Content.Text != "second" || second.Content.IsBlocks() {
		t.Fatalf("second user message changed: %#v", second)
	}
	if pointer := original[1].(*ptypes.UserMessage); pointer.Content.IsBlocks() {
		t.Fatal("transform mutated the caller's message")
	}
}
