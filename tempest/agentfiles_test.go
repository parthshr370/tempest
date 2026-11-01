package tempest

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/agentfiles"
	"go.harness.dev/harness/internal/engine/stream"
	types "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/prompt"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/tempest/extension"
)

const fileAgentInstructions = "FILE_AGENT_INSTRUCTIONS: audit only the assigned input"

// recordedRequest is one provider request as the stream received it.
type recordedRequest struct {
	model     types.Model
	input     types.Context
	reasoning string
}

// providerRecorder wraps a faux stream and keeps every request it serves.
type providerRecorder struct {
	mu       sync.Mutex
	requests []recordedRequest
}

func (recorder *providerRecorder) wrap(provider *faux.Faux) agent.StreamFn {
	return func(ctx context.Context, model types.Model, input types.Context, options *types.SimpleStreamOptions) *stream.AssistantStream {
		request := recordedRequest{model: model, input: input}
		if options != nil {
			request.reasoning = options.Reasoning
		}
		recorder.mu.Lock()
		recorder.requests = append(recorder.requests, request)
		recorder.mu.Unlock()
		return provider.StreamSimple(ctx, model, input, options)
	}
}

func (recorder *providerRecorder) all() []recordedRequest {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return slices.Clone(recorder.requests)
}

func toolNames(tools []types.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return names
}

// TestFileAgentRunsThroughChildRunner delegates to an agent defined in
// .harness/agents with and without extension bundles, through each model
// role alias. The child sees only the tools both the parent and the file
// allow, runs on the aliased role's model with the file's reasoning, and the
// root's system prompt lists the agent exactly once on every request.
func TestFileAgentRunsThroughChildRunner(t *testing.T) {
	cases := []struct {
		name       string
		alias      string
		extensions bool
	}{
		{"smol without extensions", agentfiles.RoleSmol, false},
		{"plan without extensions", agentfiles.RolePlan, false},
		{"smol beside a clashing bundle preset", agentfiles.RoleSmol, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := faux.New(faux.Options{})
			child := faux.New(faux.Options{Models: []faux.ModelDef{{ID: "role-model", ContextWindow: 128000, MaxTokens: 16384}}})
			cwd := t.TempDir()
			if err := os.WriteFile(filepath.Join(cwd, "input.txt"), []byte("file agent input"), 0o600); err != nil {
				t.Fatal(err)
			}
			agentPath := filepath.Join(cwd, ".harness", "agents", "auditor.md")
			if err := os.MkdirAll(filepath.Dir(agentPath), 0o755); err != nil {
				t.Fatal(err)
			}
			definition := "---\nname: auditor\ndescription: Read-only file auditor\ntools: [read, grep, bash]\nmodel: " + tc.alias + "\nreasoning: high\n---\n" + fileAgentInstructions + "\n"
			if err := os.WriteFile(agentPath, []byte(definition), 0o600); err != nil {
				t.Fatal(err)
			}
			agents, diagnostics, err := agentfiles.Load(context.Background(), agentfiles.Options{ProjectDir: cwd})
			if err != nil || len(diagnostics) != 0 || len(agents) != 1 {
				t.Fatalf("load: %+v %+v %v", agents, diagnostics, err)
			}

			root.SetResponses(
				faux.Respond(faux.ToolCall("task", json.RawMessage(`{"prompt":"Read input.txt and audit it","subagent_type":"auditor"}`), "delegate")),
				faux.Respond(faux.Text("waiting")),
				faux.Respond(faux.Text("root complete")),
			)
			child.SetResponses(
				faux.Respond(faux.ToolCall("read", json.RawMessage(`{"path":"input.txt"}`), "child_read")),
				faux.Respond(faux.Text("audited")),
			)
			rootRequests, childRequests := &providerRecorder{}, &providerRecorder{}
			childStream := childRequests.wrap(child)
			stack := &StackConfig{
				Model: root.Model(), StreamFn: rootRequests.wrap(root), EnableTask: true,
				// The parent may use find; the file does not allow it, so the child must not see it.
				AllowedTools: []string{"task", "read", "grep", "find"},
				ContextFiles: []prompt.ContextFile{},
				AgentFiles:   agents,
			}
			switch tc.alias {
			case agentfiles.RoleSmol:
				stack.SmolModel, stack.SmolStreamFn = child.Model(), childStream
			case agentfiles.RolePlan:
				// A host that routed the plan role passes its stream.
				stack.PlanModel, stack.PlanStreamFn = child.Model(), childStream
			}
			var logs bytes.Buffer
			options := Options{SessionDir: t.TempDir(), Cwd: cwd, New: &NewSession{PermissionMode: permission.ModeBypass}, Stack: stack, Logger: slog.New(slog.NewTextHandler(&logs, nil))}
			if tc.extensions {
				registry := extension.NewRegistry()
				t.Cleanup(func() { _ = registry.Close(context.Background()) })
				if err := registry.Install(t.Context(), extension.Definition{ID: "presets", Revision: "v1", Presets: []extension.AgentPreset{{ID: "auditor", Version: 1, Description: "Bundle auditor", Instructions: "BUNDLE_INSTRUCTIONS", AllowedTools: []string{"read"}}}}); err != nil {
					t.Fatal(err)
				}
				options.Registry = registry
				options.Extensions = []extension.Selection{{ID: "presets", Required: true}}
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

			if tc.extensions {
				for _, want := range []string{"agent file name matches a bundle preset", agentPath, "bundle=presets"} {
					if !strings.Contains(logs.String(), want) {
						t.Errorf("clash warning lacks %q: %s", want, logs.String())
					}
				}
			}
			roots := rootRequests.all()
			if len(roots) == 0 {
				t.Fatal("no root requests")
			}
			for _, request := range roots {
				if request.input.SystemPrompt != roots[0].input.SystemPrompt {
					t.Error("root system prompt changed between requests")
				}
				if count := strings.Count(request.input.SystemPrompt, "<available_subagents>"); count != 1 {
					t.Errorf("root system prompt lists agents %d times, want once", count)
				}
				if !strings.Contains(request.input.SystemPrompt, "- auditor: Read-only file auditor") {
					t.Error("root system prompt lacks the file agent")
				}
				for _, tool := range request.input.Tools {
					if tool.Name == "task" && !strings.Contains(string(tool.Parameters), `"auditor"`) {
						t.Errorf("task schema does not offer the file agent: %s", tool.Parameters)
					}
				}
			}
			children := childRequests.all()
			if len(children) != 2 {
				t.Fatalf("child made %d provider requests, want two", len(children))
			}
			for _, request := range children {
				if request.model.ID != child.Model().ID || request.model.Provider != child.Model().Provider {
					t.Errorf("child model %+v, want the %s role model", request.model, tc.alias)
				}
				if request.reasoning != "high" {
					t.Errorf("child reasoning %q, want the file's high", request.reasoning)
				}
				if !strings.Contains(request.input.SystemPrompt, fileAgentInstructions) || strings.Contains(request.input.SystemPrompt, "BUNDLE_INSTRUCTIONS") {
					t.Error("child does not run the file agent's instructions")
				}
				if names := toolNames(request.input.Tools); !slices.Equal(names, []string{"grep", "read"}) {
					t.Errorf("child tools %v, want [grep read]", names)
				}
			}
			read, ok := toolResults(children[1].input.Messages)["child_read"]
			if !ok || read.IsError || len(read.Content) == 0 || !strings.Contains(read.Content[0].Text, "file agent input") {
				t.Fatalf("child did not execute read: %+v", read)
			}
			jobs, err := owner.Jobs(t.Context(), "", 10)
			if err != nil || len(jobs.Jobs) != 1 || jobs.Jobs[0].State != "completed" {
				t.Fatalf("durable child outcome: %+v %v", jobs, err)
			}
		})
	}
}
