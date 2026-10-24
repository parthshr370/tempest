package tempest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/stream"
	types "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/extension"
)

func TestNamedPresetRunsThroughChildRunner(t *testing.T) {
	for _, backend := range []string{"jsonl", "memory"} {
		t.Run(backend, func(t *testing.T) {
			root := faux.New(faux.Options{})
			child := faux.New(faux.Options{Models: []faux.ModelDef{{ID: "preset-model", ContextWindow: 128000, MaxTokens: 16384}}})
			registry := extension.NewRegistry()
			t.Cleanup(func() { _ = registry.Close(context.Background()) })
			const instructions = "PRESET_INSTRUCTIONS: audit only the assigned input"
			if err := registry.Install(t.Context(), extension.Definition{ID: "presets", Revision: "v1", Presets: []extension.AgentPreset{{ID: "auditor", Version: 1, Description: "Read-only auditor", Instructions: instructions, Model: child.Model().ID, AllowedTools: []string{"read", "grep", "bash"}}}}); err != nil {
				t.Fatal(err)
			}
			cwd := t.TempDir()
			if err := os.WriteFile(filepath.Join(cwd, "input.txt"), []byte("preset child input"), 0600); err != nil {
				t.Fatal(err)
			}
			root.SetResponses(
				faux.Respond(faux.ToolCall("task", json.RawMessage(`{"prompt":"Read input.txt and audit it","subagent_type":"presets/auditor"}`), "delegate")),
				faux.Respond(faux.Text("waiting")),
				faux.Respond(faux.Text("root complete")),
			)
			child.SetResponses(
				faux.Respond(faux.ToolCall("read", json.RawMessage(`{"path":"input.txt"}`), "child_read")),
				faux.Respond(faux.Text("audited")),
			)
			type request struct {
				model types.Model
				input types.Context
			}
			requests := make(chan request, 4)
			childStream := func(ctx context.Context, model types.Model, input types.Context, options *types.SimpleStreamOptions) *stream.AssistantStream {
				requests <- request{model, input}
				return child.StreamSimple(ctx, model, input, options)
			}
			options := Options{SessionDir: t.TempDir(), Cwd: cwd, New: &NewSession{PermissionMode: permission.ModeBypass}, Registry: registry, Extensions: []extension.Selection{{ID: "presets", Required: true}}, Stack: &StackConfig{Model: root.Model(), StreamFn: root.StreamSimple, SmolModel: child.Model(), SmolStreamFn: childStream, EnableTask: true, AllowedTools: []string{"task", "read", "grep", "find"}}}
			if backend == "memory" {
				options.Store = durable.NewMemory()
				t.Cleanup(func() { _ = options.Store.Close(context.Background()) })
			}
			owner, err := Open(t.Context(), options)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = owner.Close(context.Background()) })
			result, err := owner.Prompt(t.Context(), Prompt{Text: "Delegate to the auditor"})
			if err != nil || result.StopReason != agent.StopCompleted {
				t.Fatalf("root result: %+v %v", result, err)
			}
			if len(requests) != 2 {
				t.Fatalf("real child made %d provider requests, want two", len(requests))
			}
			first, second := <-requests, <-requests
			for _, got := range []request{first, second} {
				if got.model.ID != child.Model().ID || got.model.Provider != child.Model().Provider {
					t.Errorf("child model: %+v", got.model)
				}
				if !strings.Contains(got.input.SystemPrompt, instructions) || !strings.Contains(got.input.SystemPrompt, "You are a delegated child.") {
					t.Error("real child provider request lacks preset instructions or child identity")
				}
				var names []string
				for _, tool := range got.input.Tools {
					names = append(names, tool.Name)
				}
				slices.Sort(names)
				if !slices.Equal(names, []string{"grep", "read"}) {
					t.Errorf("child tool ceiling: %v, want [grep read]", names)
				}
			}
			read, ok := toolResults(second.input.Messages)["child_read"]
			if !ok || read.IsError || len(read.Content) == 0 || !strings.Contains(read.Content[0].Text, "preset child input") {
				t.Fatalf("real child did not execute read: %+v", read)
			}
			jobs, err := owner.Jobs(t.Context(), "", 10)
			if err != nil || len(jobs.Jobs) != 1 || jobs.Jobs[0].State != "completed" || jobs.Jobs[0].ChildSessionID == "" {
				t.Fatalf("durable child outcome: %+v %v", jobs, err)
			}
		})
	}
}
