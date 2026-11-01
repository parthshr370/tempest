package adapter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/retry"
)

// backupEndpoint is an OpenAI-compatible server that records each request's
// body and authorization header and answers with a short completion.
type backupEndpoint struct {
	mu      sync.Mutex
	bodies  []string
	bearers []string
}

func (e *backupEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	e.mu.Lock()
	e.bodies = append(e.bodies, string(body))
	e.bearers = append(e.bearers, r.Header.Get("Authorization"))
	e.mu.Unlock()
	io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"from backup\"},\"finish_reason\":\"stop\"}]}\n\n")
}

// signedHistory holds an Anthropic turn whose thinking block carries a
// provider-bound signature that no other provider may receive.
func signedHistory() types.Context {
	return types.Context{Messages: []types.Message{
		types.UserMessage{Content: types.StringContent("first question")},
		types.AssistantMessage{API: AnthropicMessagesAPI, Provider: "anthropic", Model: "claude-primary", StopReason: types.StopStop, Content: []types.ContentBlock{
			{Type: types.BlockThinking, Thinking: "visible reasoning", ThinkingSignature: "anthropic-signature-secret"},
			types.NewText("first answer"),
		}},
		types.UserMessage{Content: types.StringContent("second question")},
	}}
}

func unavailablePrimary(t *testing.T) (*httptest.Server, func() int) {
	t.Helper()
	calls := 0
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	}))
	t.Cleanup(server.Close)
	return server, func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
}

func streamWithFallback(t *testing.T, router ProviderRouter) (*types.AssistantMessage, []types.ModelFallback) {
	t.Helper()
	response := router.StreamFn(context.Background(), router.Model, signedHistory(), nil)
	var switches []types.ModelFallback
	for event := range response.Events() {
		if event.Type == types.EvFallback && event.Fallback != nil {
			switches = append(switches, *event.Fallback)
		}
	}
	final, err := response.Result(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return final, switches
}

func checkBackupRequest(t *testing.T, backup *backupEndpoint, bearer string) {
	t.Helper()
	backup.mu.Lock()
	defer backup.mu.Unlock()
	if len(backup.bodies) != 1 {
		t.Fatalf("backup requests = %d", len(backup.bodies))
	}
	body := backup.bodies[0]
	if strings.Contains(body, "anthropic-signature-secret") {
		t.Fatalf("provider-bound signature crossed providers: %s", body)
	}
	if !strings.Contains(body, "visible reasoning") || !strings.Contains(body, "gpt-backup") {
		t.Fatalf("backup request lost history or model: %s", body)
	}
	if backup.bearers[0] != "Bearer "+bearer {
		t.Fatalf("backup authorization = %q", backup.bearers[0])
	}
}

// A role whose primary is unavailable answers from its backup on another
// provider: the backup gets its own credential and the history without the
// primary's provider-bound thinking signature.
func TestRoleFallbackCrossesProvidersWithItsOwnCredential(t *testing.T) {
	primary, primaryCalls := unavailablePrimary(t)
	backup := &backupEndpoint{}
	backupServer := httptest.NewServer(backup)
	defer backupServer.Close()
	secrets := map[string]string{"ANTHROPIC_API_KEY": "anthropic-key", "OPENAI_API_KEY": "openai-key"}
	primarySpec := "anthropic:claude-primary@" + primary.URL
	roles, err := ResolveRoles(RoleRoutingConfig{
		DefaultModel: primarySpec,
		// Repeating the primary adds nothing: it already failed this request.
		DefaultFallbacks: []string{primarySpec, "openai:gpt-backup@" + backupServer.URL},
		SecretLookup:     func(name string) (string, bool) { value, ok := secrets[name]; return value, ok },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(roles.Default.Fallbacks) != 1 || roles.Default.Fallbacks[0].ID != "gpt-backup" || len(roles.Plan.Fallbacks) != 0 {
		t.Fatalf("fallback chains: default=%+v plan=%+v", roles.Default.Fallbacks, roles.Plan.Fallbacks)
	}
	final, switches := streamWithFallback(t, roles.Default)
	if final.StopReason != types.StopStop || final.Provider != "openai" || final.Model != "gpt-backup" {
		t.Fatalf("final = %+v", final)
	}
	if primaryCalls() != 1 || len(switches) != 1 || switches[0].Reason != string(retry.CodeServerFailure) || switches[0].Index != 1 {
		t.Fatalf("primary calls = %d, switches = %+v", primaryCalls(), switches)
	}
	checkBackupRequest(t, backup, "openai-key")
}

// The SDK route derives a backup on another provider without the primary's
// endpoint or key; the backup's credential comes from the environment.
func TestRouterFallbackDropsPrimaryEndpointAndKey(t *testing.T) {
	primary, _ := unavailablePrimary(t)
	backup := &backupEndpoint{}
	backupServer := httptest.NewServer(backup)
	defer backupServer.Close()
	router, err := NewProviderRouter(RoutingConfig{
		ModelID:        "anthropic:claude-primary",
		BaseURL:        primary.URL,
		APIKey:         "anthropic-key",
		FallbackModels: []string{"openai:gpt-backup@" + backupServer.URL},
		Env: func(name string) string {
			if name == "OPENAI_API_KEY" {
				return "openai-env-key"
			}
			return ""
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	final, switches := streamWithFallback(t, router)
	if final.Provider != "openai" || len(switches) != 1 {
		t.Fatalf("final = %+v switches = %+v", final, switches)
	}
	checkBackupRequest(t, backup, "openai-env-key")
	if _, err := NewProviderRouter(RoutingConfig{ModelID: "anthropic:claude-primary", FallbackModels: []string{"nope:model"}, Env: func(string) string { return "" }}); err == nil || !strings.Contains(err.Error(), "nope:model") {
		t.Fatalf("unroutable backup error = %v", err)
	}
}

// OpenAI's own endpoint takes function tools for reasoning models only on the
// Responses API; compatible endpoints keep Chat Completions.
func TestOpenAIPresetRoutesReasoningModelsToResponses(t *testing.T) {
	var nonReasoning map[string]ProviderConfig
	if err := json.Unmarshal([]byte(`{"openai":{"models":[{"id":"gpt-6-luna","reasoning":false}]}}`), &nonReasoning); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		routing   RoutingConfig
		api       string
		reasoning bool
	}{
		{"model the catalog does not list", RoutingConfig{ModelID: "openai:gpt-6-luna"}, "openai-responses", true},
		{"catalog reasoning model", RoutingConfig{ModelID: "openai:gpt-5.6"}, "openai-responses", true},
		{"catalog model without reasoning", RoutingConfig{ModelID: "openai:gpt-4.1"}, OpenAICompletionsAPI, false},
		{"settings say the model does not reason", RoutingConfig{ModelID: "openai:gpt-6-luna", Providers: nonReasoning}, OpenAICompletionsAPI, false},
		{"compatible endpoint in the spec", RoutingConfig{ModelID: "openai:gpt-6-luna@https://compatible.example/v1"}, OpenAICompletionsAPI, false},
		{"compatible endpoint setting", RoutingConfig{ModelID: "openai:gpt-6-luna", OpenAIBaseURL: "https://compatible.example/v1"}, OpenAICompletionsAPI, false},
		{"explicit protocol", RoutingConfig{ModelID: "openai:gpt-6-luna", Providers: map[string]ProviderConfig{"openai": {Protocol: "openai-chat"}}}, OpenAICompletionsAPI, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			routed, err := resolveRoute(tc.routing, func(string) string { return "" })
			if err != nil {
				t.Fatal(err)
			}
			if routed.API != tc.api || routed.Model.API != tc.api || routed.Model.Provider != "openai" {
				t.Fatalf("route = %s / %+v", routed.API, routed.Model)
			}
			if tc.api == "openai-responses" && !routed.Model.Reasoning {
				t.Fatalf("Responses route lost reasoning: %+v", routed.Model)
			}
			if tc.api == OpenAICompletionsAPI && routed.Model.Reasoning != tc.reasoning {
				t.Fatalf("reasoning = %v", routed.Model.Reasoning)
			}
		})
	}
	router, err := NewProviderRouter(RoutingConfig{ModelID: "openai:gpt-6-luna", APIKey: "key", Env: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := router.Registry.Resolve(router.Model.API); !ok || router.Model.BaseURL != "https://api.openai.com/v1" {
		t.Fatalf("router = %+v", router.Model)
	}
}
