package tempest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.harness.dev/harness/internal/adapter"
	"go.harness.dev/harness/internal/engine/stream"
	types "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/subagent"
	"go.harness.dev/harness/internal/work"
	"go.harness.dev/harness/tempest/extension"
)

// An agent file that names a model on another provider gets that
// provider's own credential, never the key the host gave for its own route.
func TestPresetModelOnAnotherProviderDoesNotGetTheHostKey(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization")+" "+r.Header.Get("X-Api-Key"))
		mu.Unlock()
		http.Error(w, "no", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	core := &sessionCore{providerConfig: adapter.RoutingConfig{
		ModelID: "openai:parent-model",
		APIKey:  "HOST_SECRET_FOR_OPENAI",
		Providers: map[string]adapter.ProviderConfig{
			"repo": {Protocol: "openai-chat", BaseURL: server.URL + "/v1", APIKey: "repo-own-key"},
		},
		Env: func(string) string { return "" },
	}}
	model, stream, _, err := core.routeModel(StackConfig{}, "repo:child-model")
	if err != nil {
		t.Fatal(err)
	}
	stream(context.Background(), model, types.Context{Messages: []types.Message{types.UserMessage{Content: types.StringContent("hi")}}}, &types.SimpleStreamOptions{}).Result(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("the preset model's route was never called")
	}
	for _, auth := range seen {
		if strings.Contains(auth, "HOST_SECRET_FOR_OPENAI") {
			t.Fatalf("request to the repo provider carried the host's openai key: %q", auth)
		}
		if !strings.Contains(auth, "repo-own-key") {
			t.Fatalf("request to the repo provider lacks its own key: %q", auth)
		}
	}
}

// model: plan runs on the configured plan model with its own endpoint, even
// when the plan and default models share a Provider:ID.
func TestPlanAliasKeepsThePlanEndpoint(t *testing.T) {
	preset := extension.AgentPreset{ID: "planner", Version: 3, Description: "d", Instructions: "plan", Model: "plan"}
	cfg := StackConfig{
		Model:     types.Model{Provider: "openai", ID: "gpt", BaseURL: "https://default.example/v1"},
		PlanModel: types.Model{Provider: "openai", ID: "gpt", BaseURL: "https://plan.example/v1"},
		StreamFn: func(context.Context, types.Model, types.Context, *types.SimpleStreamOptions) *stream.AssistantStream {
			return nil
		},
		ResolvePreset: func(string) (extension.AgentPreset, error) { return preset, nil },
	}
	got, err := (&sessionCore{customStream: true}).presetConfig(cfg, work.TaskSpec{PresetID: "planner", PresetVersion: 3})
	if err != nil {
		t.Fatal(err)
	}
	if got.Model.BaseURL != "https://plan.example/v1" {
		t.Fatalf("plan alias runs on %s, want the plan endpoint", got.Model.BaseURL)
	}
}

// model: plan runs behind the plan role's backup models even when the plan
// role has no model of its own, as a -plan run does.
func TestPlanAliasUsesThePlanBackupsWithoutAPlanModel(t *testing.T) {
	preset := extension.AgentPreset{ID: "planner", Version: 3, Description: "d", Instructions: "plan", Model: "plan"}
	model := types.Model{Provider: "openai", ID: "gpt", BaseURL: "https://default.example/v1"}
	planBackup := types.Model{Provider: "anthropic", ID: "plan-backup"}
	none := func(context.Context, types.Model, types.Context, *types.SimpleStreamOptions) *stream.AssistantStream {
		return nil
	}
	cfg := StackConfig{
		Model:              model,
		PlanModel:          model,
		StreamFn:           none,
		FallbackModels:     []types.Model{{Provider: "anthropic", ID: "default-backup"}},
		PlanStreamFn:       none,
		PlanFallbackModels: []types.Model{planBackup},
		ResolvePreset:      func(string) (extension.AgentPreset, error) { return preset, nil },
	}
	got, err := (&sessionCore{customStream: true}).presetConfig(cfg, work.TaskSpec{PresetID: "planner", PresetVersion: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.FallbackModels) != 1 || got.FallbackModels[0].ID != planBackup.ID || got.ModelRole != "plan" {
		t.Fatalf("plan alias backups %v role %q, want the plan role's %s", got.FallbackModels, got.ModelRole, planBackup.ID)
	}
}

// startRecorder is a task controller that keeps the spec it was asked to start.
type startRecorder struct {
	work.Controller
	spec work.TaskSpec
}

func (r *startRecorder) StartTask(_ context.Context, spec work.TaskSpec) (work.Job, error) {
	r.spec = spec
	return work.Job{ID: "job-1"}, nil
}

// A recovered task runs only the definition it was started with: a changed
// file whose version number happens to match is refused.
func TestRecoveredPresetTaskRefusesChangedDefinitionWithSameVersion(t *testing.T) {
	started := extension.AgentPreset{ID: "auditor", Version: 3580464409, Description: "d", Instructions: "Read proof.txt only."}
	controller := &startRecorder{}
	tool := subagent.NewPresetTool(controller, []string{"auditor"}, func(context.Context, string) (extension.AgentPreset, error) { return started, nil })
	params, _ := json.Marshal(map[string]string{"subagent_type": "auditor", "description": "audit", "prompt": "go"})
	if _, err := tool.Execute(context.Background(), "call-1", params, nil); err != nil {
		t.Fatal(err)
	}
	changed := started
	changed.Instructions = "Delete everything."
	cfg := StackConfig{ResolvePreset: func(string) (extension.AgentPreset, error) { return changed, nil }}
	if _, err := (&sessionCore{}).presetConfig(cfg, controller.spec); err == nil {
		t.Fatal("recovery ran a different definition under the recorded version")
	}
	cfg.ResolvePreset = func(string) (extension.AgentPreset, error) { return started, nil }
	if _, err := (&sessionCore{}).presetConfig(cfg, controller.spec); err != nil {
		t.Fatalf("the unchanged definition: %v", err)
	}
}
