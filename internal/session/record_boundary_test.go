package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/tempest/durable"
)

func TestCompletedToolResultIsDurableBeforeBatchEnds(t *testing.T) {
	store := queueStore(t)
	agt := agent.NewAgent(agent.AgentOptions{})
	r := recorder{agt: agt, store: store, toolStarts: map[string]time.Time{}, sources: map[string]types.Usage{}}
	if err := r.record(t.Context(), agent.AgentStart(nil)); err != nil {
		t.Fatal(err)
	}
	assistant := types.AssistantMessage{Content: []types.ContentBlock{types.NewToolCall("done", "read", json.RawMessage(`{}`)), types.NewToolCall("held", "read", json.RawMessage(`{}`))}, StopReason: types.StopToolUse}
	if err := r.record(t.Context(), agent.MessageEnd(assistant)); err != nil {
		t.Fatal(err)
	}
	result := types.ToolResultMessage{ToolCallID: "done", ToolName: "read", Content: []types.ContentBlock{types.NewText("durable completed output")}}
	if err := r.record(t.Context(), agent.MessageEnd(result)); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range loaded.Messages {
		if tool, ok := message.(types.ToolResultMessage); ok && tool.ToolCallID == "done" && len(tool.Content) > 0 && tool.Content[0].Text == "durable completed output" {
			found = true
		}
	}
	if !found {
		t.Fatal("completed tool result lost before turn_end")
	}
}

func TestRequestFramesDeduplicatePromptAndSanitizeCredentials(t *testing.T) {
	store := queueStore(t)
	provider := faux.New(faux.Options{})
	provider.SetResponses(faux.Respond(faux.Text("first")), faux.Respond(faux.Text("second")))
	streamFn := recordRequestFrames(store, provider.StreamSimple)
	c := types.Context{SystemPrompt: strings.Repeat("system ", 1000), Tools: []types.Tool{{Name: "test", Parameters: json.RawMessage(`{"type":"object"}`)}}}
	for range 2 {
		out := streamFn(t.Context(), provider.Model(), c, &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{APIKey: "private-api-key", Headers: map[string]string{"Authorization": "private-auth"}}})
		for range out.Events() {
		}
		if _, err := out.Result(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	prompts, err := store.Read(t.Context(), durable.Query{Kind: durable.EntryKind, Namespace: "_tempest/request-prompts", Limit: 100})
	if err != nil || len(prompts.Records) != 1 {
		t.Fatal("prompt payload repeated", prompts, err)
	}
	frames, err := store.Read(t.Context(), durable.Query{Kind: durable.EntryKind, Namespace: "_tempest/requests", Limit: 100})
	if err != nil || len(frames.Records) != 2 {
		t.Fatal(frames, err)
	}
	for _, frame := range frames.Records {
		var value struct {
			PromptDigest string `json:"promptDigest"`
		}
		if err := json.Unmarshal(frame.Value, &value); err != nil || value.PromptDigest != prompts.Records[0].ID {
			t.Fatal("frame reference lost", value, err)
		}
		if strings.Contains(string(frame.Value), "private-") || strings.Contains(string(frame.Value), c.SystemPrompt) {
			t.Fatal("frame repeated prompt or leaked credentials")
		}
	}
}
