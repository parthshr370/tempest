package mcpserver

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/tempest"
)

func TestMCPDurableRequestReturnsOriginalAnswer(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("original")), faux.Respond(faux.Text("must not run")))
	c := connect(t, options(t, f), "2025-11-25", map[string]any{}, nil)
	first, err := c.run(RunInput{Prompt: "hello", RequestID: "stable"}, nil)
	if err != nil || first.IsError || first.StructuredContent == nil {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	retry, err := c.run(RunInput{SessionID: first.StructuredContent.SessionID, Prompt: "hello", RequestID: "stable"}, nil)
	if err != nil || retry.IsError || retry.StructuredContent == nil || retry.StructuredContent.RunID != first.StructuredContent.RunID || retry.StructuredContent.Text != "original" {
		t.Fatalf("retry=%+v err=%v", retry, err)
	}
	conflict, err := c.run(RunInput{SessionID: first.StructuredContent.SessionID, Prompt: "changed", RequestID: "stable"}, nil)
	if err != nil || !conflict.IsError {
		t.Fatalf("changed input=%+v err=%v", conflict, err)
	}
}

func TestMCPBusySessionAllowsOnlyExistingRequestKeys(t *testing.T) {
	provider := faux.New(faux.Options{})
	provider.SetResponses(
		faux.Respond(faux.Text("seed answer")),
		faux.Respond(faux.ToolCall("hold", json.RawMessage(`{}`), "hold-call")),
		faux.Respond(faux.Text("active answer")),
	)
	opts := options(t, provider)
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	opts.DefaultNew.PermissionMode = "bypass"
	opts.DefaultNew.Tools = []tempest.Tool{{
		Tool: tempest.ToolDefinition{Name: "hold", Parameters: json.RawMessage(`{"type":"object"}`)},
		Execute: func(ctx context.Context, _ string, _ json.RawMessage, _ tempest.ToolUpdateCallback) (tempest.ToolResult, error) {
			close(started)
			select {
			case <-release:
				return tempest.ToolResult{}, nil
			case <-ctx.Done():
				return tempest.ToolResult{}, ctx.Err()
			}
		},
	}}
	client := connect(t, opts, "2025-11-25", map[string]any{}, nil)
	seed, err := client.run(RunInput{Prompt: "seed", RequestID: "seed"}, nil)
	if err != nil || seed.IsError || seed.StructuredContent == nil {
		t.Fatalf("seed=%+v %v", seed, err)
	}
	active := RunInput{SessionID: seed.StructuredContent.SessionID, Prompt: "active", RequestID: "active"}
	results := make(chan toolResult, 2)
	go func() {
		result, err := client.run(active, nil)
		if err != nil {
			t.Error(err)
		}
		results <- result
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("active submission did not start")
	}
	go func() {
		result, err := client.run(active, nil)
		if err != nil {
			t.Error(err)
		}
		results <- result
	}()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	var retry toolResult
	err = client.conn.Call(ctx, "tools/call", map[string]any{"name": "tempest_run", "arguments": RunInput{SessionID: active.SessionID, Prompt: "seed", RequestID: "seed"}}, &retry)
	if err != nil || retry.IsError || retry.StructuredContent == nil || retry.StructuredContent.RunID != seed.StructuredContent.RunID {
		t.Fatalf("terminal retry blocked by another request: %+v %v", retry, err)
	}
	var busy toolResult
	err = client.conn.Call(ctx, "tools/call", map[string]any{"name": "tempest_run", "arguments": RunInput{SessionID: active.SessionID, Prompt: "new", RequestID: "new"}}, &busy)
	if err != nil || !busy.IsError {
		t.Fatalf("new request did not fail immediately: %+v %v", busy, err)
	}
	releaseOnce.Do(func() { close(release) })
	first, duplicate := <-results, <-results
	if first.IsError || duplicate.IsError || first.StructuredContent == nil || duplicate.StructuredContent == nil || first.StructuredContent.RunID != duplicate.StructuredContent.RunID || first.StructuredContent.Text != "active answer" || duplicate.StructuredContent.Text != "active answer" {
		t.Fatalf("active retry duplicated or lost original work: first=%+v duplicate=%+v", first, duplicate)
	}
}
