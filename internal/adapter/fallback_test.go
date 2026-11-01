package adapter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.harness.dev/harness/internal/agent"
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

// authRecorder records the credentials each request carried and answers with
// status (an error body) or, for zero, the next of replies (Chat Completions
// stream bodies), then a short text stream.
type authRecorder struct {
	status   int
	replies  []string
	mu       sync.Mutex
	requests []recordedAuth
}

type recordedAuth struct {
	authorization, apiKey, query, body string
}

func (a *authRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	a.mu.Lock()
	a.requests = append(a.requests, recordedAuth{authorization: r.Header.Get("Authorization"), apiKey: r.Header.Get("X-Api-Key"), query: r.URL.RawQuery, body: string(body)})
	reply := "data: {\"choices\":[{\"delta\":{\"content\":\"from backup\"},\"finish_reason\":\"stop\"}]}\n\n"
	if len(a.replies) > 0 {
		reply, a.replies = a.replies[0], a.replies[1:]
	}
	a.mu.Unlock()
	if a.status != 0 {
		w.WriteHeader(a.status)
		io.WriteString(w, `{"error":{"type":"server_error","message":"unavailable"}}`)
		return
	}
	io.WriteString(w, reply)
}

func (a *authRecorder) recorded(t *testing.T) []recordedAuth {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.requests)
}

// A per-request credential is bound to the route it was issued for. A backup
// on another provider, or on the same provider at another endpoint, never
// receives the caller's key, authorization header or auth query parameter; it
// authenticates with its own credential. The primary still gets them.
func TestPerRequestCredentialsNeverReachAnotherRoute(t *testing.T) {
	const requestKey, requestToken, requestQuery = "primary-request-key", "primary-request-token", "primary-request-query"
	for _, tc := range []struct {
		name, primary, backup string
		primaryAuth           func(recordedAuth) string
	}{
		{"backup on another provider", "anthropic:claude-primary", "openai:gpt-backup", func(r recordedAuth) string { return r.apiKey }},
		{"backup on the same provider at another endpoint", "openai:gpt-primary", "openai:gpt-backup", func(r recordedAuth) string { return r.authorization }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			primary := &authRecorder{status: http.StatusServiceUnavailable}
			primaryServer := httptest.NewServer(primary)
			defer primaryServer.Close()
			backup := &authRecorder{}
			backupServer := httptest.NewServer(backup)
			defer backupServer.Close()
			router, err := NewProviderRouter(RoutingConfig{
				ModelID:        tc.primary + "@" + primaryServer.URL,
				FallbackModels: []string{tc.backup + "@" + backupServer.URL},
				Env: func(name string) string {
					return map[string]string{"OPENAI_API_KEY": "backup-own-key", "ANTHROPIC_API_KEY": "anthropic-env-key"}[name]
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, opts := range []types.StreamOptions{
				{APIKey: requestKey},
				{Headers: map[string]string{"Authorization": "Bearer " + requestToken}, Query: map[string]string{"key": requestQuery}},
			} {
				primary.requests, backup.requests = nil, nil
				response := router.StreamFn(context.Background(), router.Model, signedHistory(), &types.SimpleStreamOptions{StreamOptions: opts})
				final, err := response.Result(context.Background())
				if err != nil || final.Model != "gpt-backup" {
					t.Fatalf("final = %+v, %v", final, err)
				}
				primaryRequests, backupRequests := primary.recorded(t), backup.recorded(t)
				if len(primaryRequests) == 0 || len(backupRequests) != 1 {
					t.Fatalf("primary requests = %d, backup requests = %d", len(primaryRequests), len(backupRequests))
				}
				if opts.APIKey != "" && !strings.Contains(tc.primaryAuth(primaryRequests[0]), requestKey) {
					t.Fatalf("primary lost its per-request key: %+v", primaryRequests[0])
				}
				got := backupRequests[0]
				if got.authorization != "Bearer backup-own-key" {
					t.Fatalf("backup authorization = %q, want its own key", got.authorization)
				}
				for _, secret := range []string{requestKey, requestToken, requestQuery} {
					if strings.Contains(got.authorization+got.apiKey+got.query+got.body, secret) {
						t.Fatalf("backup received the primary's per-request credential %q: %+v", secret, got)
					}
				}
			}
		})
	}
}

// The agent loop resolves its per-request key for the model's own provider; a
// backup on another provider answers with its own key and never receives it.
func TestAgentRequestKeyStaysWithItsProvider(t *testing.T) {
	primary := &authRecorder{status: http.StatusServiceUnavailable}
	primaryServer := httptest.NewServer(primary)
	defer primaryServer.Close()
	backup := &authRecorder{}
	backupServer := httptest.NewServer(backup)
	defer backupServer.Close()
	router, err := NewProviderRouter(RoutingConfig{
		ModelID:        "anthropic:claude-primary@" + primaryServer.URL,
		FallbackModels: []string{"openai:gpt-backup@" + backupServer.URL},
		Env: func(name string) string {
			return map[string]string{"OPENAI_API_KEY": "backup-own-key", "ANTHROPIC_API_KEY": "anthropic-env-key"}[name]
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	a := agent.NewAgent(agent.AgentOptions{
		InitialState:   &agent.AgentState{SystemPrompt: "test", Model: router.Model},
		StreamFn:       router.StreamFn,
		FallbackModels: router.Fallbacks,
		GetApiKey: func(provider string) string {
			if provider == "anthropic" {
				return "primary-request-key"
			}
			return "wrong-provider-key"
		},
	})
	result, err := a.PromptText(context.Background(), "hello")
	if err != nil || result.StopReason != agent.StopCompleted {
		t.Fatalf("run = %+v, %v", result, err)
	}
	primaryRequests, backupRequests := primary.recorded(t), backup.recorded(t)
	if len(primaryRequests) != 1 || primaryRequests[0].apiKey != "primary-request-key" {
		t.Fatalf("primary requests = %+v", primaryRequests)
	}
	if len(backupRequests) != 1 || backupRequests[0].authorization != "Bearer backup-own-key" || strings.Contains(backupRequests[0].body, "primary-request-key") {
		t.Fatalf("backup requests = %+v", backupRequests)
	}
}

// Credentials a loop caller set on its stream options were given for the
// primary. After the run switched, every later turn on the backup still
// authenticates with the backup's own key, not only the turn that switched.
func TestLoopCredentialsStayWithThePrimaryOnLaterBackupTurns(t *testing.T) {
	primary := &authRecorder{status: http.StatusServiceUnavailable}
	primaryServer := httptest.NewServer(primary)
	defer primaryServer.Close()
	backup := &authRecorder{replies: []string{"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"note\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n"}}
	backupServer := httptest.NewServer(backup)
	defer backupServer.Close()
	router, err := NewProviderRouter(RoutingConfig{
		ModelID:        "anthropic:claude-primary@" + primaryServer.URL,
		FallbackModels: []string{"openai:gpt-backup@" + backupServer.URL},
		Env: func(name string) string {
			return map[string]string{"OPENAI_API_KEY": "backup-own-key", "ANTHROPIC_API_KEY": "anthropic-env-key"}[name]
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	note := agent.AgentTool{
		Tool: types.Tool{Name: "note", Description: "records a note", Parameters: json.RawMessage(`{"type":"object"}`)},
		Execute: func(context.Context, string, json.RawMessage, agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			return agent.AgentToolResult{Content: []types.ContentBlock{types.NewText("noted")}}, nil
		},
	}
	config := agent.AgentLoopConfig{
		Model:          router.Model,
		FallbackModels: router.Fallbacks,
		ConvertToLlm:   func(messages []types.Message) []types.Message { return messages },
		ToolExecution:  agent.ExecSequential,
	}
	config.APIKey = "primary-loop-key"
	config.Headers = map[string]string{"Authorization": "Bearer primary-loop-token"}
	run := agent.AgentLoop(context.Background(), []types.Message{types.UserMessage{Content: types.StringContent("hello")}}, agent.AgentContext{SystemPrompt: "test", Tools: []agent.AgentTool{note}}, config, router.StreamFn)
	for range run.Events() {
	}
	if _, err := run.Result(context.Background()); err != nil {
		t.Fatal(err)
	}
	backupRequests := backup.recorded(t)
	if len(primary.recorded(t)) != 1 || len(backupRequests) != 2 {
		t.Fatalf("primary requests = %d, backup requests = %d", len(primary.recorded(t)), len(backupRequests))
	}
	for i, got := range backupRequests {
		if got.authorization != "Bearer backup-own-key" || strings.Contains(got.body, "primary-loop") {
			t.Fatalf("backup request %d carried the primary's credentials: %+v", i, got)
		}
	}
}

// ForModel derives another model's routing without the endpoint and
// credentials given for cfg's own model when the model is on another provider
// or names another endpoint.
func TestRoutingForModelDropsCredentialsBoundToAnotherRoute(t *testing.T) {
	cfg := RoutingConfig{ModelID: "anthropic:claude-primary", BaseURL: "https://primary.example", APIKey: "primary-key", AuthToken: "primary-token", FallbackModels: []string{"openai:gpt-backup"}}
	for _, tc := range []struct {
		spec                string
		baseURL, key, token string
	}{
		{"anthropic:claude-other", cfg.BaseURL, cfg.APIKey, cfg.AuthToken},
		{"claude-other", cfg.BaseURL, cfg.APIKey, cfg.AuthToken},
		{"openai:gpt-other", "", "", ""},
		{"groq:llama-other", "", "", ""},
		{"anthropic:claude-other@https://elsewhere.example", cfg.BaseURL, "", ""},
	} {
		derived := cfg.ForModel(tc.spec)
		if derived.ModelID != tc.spec || derived.BaseURL != tc.baseURL || derived.APIKey != tc.key || derived.AuthToken != tc.token {
			t.Errorf("ForModel(%q) = base %q key %q token %q", tc.spec, derived.BaseURL, derived.APIKey, derived.AuthToken)
		}
		if len(derived.FallbackModels) != 1 {
			t.Errorf("ForModel(%q) changed the fallback chain: %v", tc.spec, derived.FallbackModels)
		}
	}
}

// OpenAI's own endpoint takes function tools for reasoning models only on the
// Responses API; compatible endpoints keep Chat Completions. Routing an
// unknown model to Responses never claims it reasons: only the catalog or
// settings grant reasoning support.
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
		{"catalog reasoning model", RoutingConfig{ModelID: "openai:gpt-6-luna"}, "openai-responses", true},
		{"older catalog reasoning model", RoutingConfig{ModelID: "openai:gpt-5.6"}, "openai-responses", true},
		{"uncataloged snapshot of a non-reasoning model", RoutingConfig{ModelID: "openai:gpt-4.1-2025-04-14"}, "openai-responses", false},
		{"uncataloged fine-tune", RoutingConfig{ModelID: "openai:ft:gpt-4.1-mini-2025-04-14:acme::abc123"}, "openai-responses", false},
		{"catalog model without reasoning", RoutingConfig{ModelID: "openai:gpt-4.1"}, OpenAICompletionsAPI, false},
		{"settings say the model does not reason", RoutingConfig{ModelID: "openai:gpt-6-luna", Providers: nonReasoning}, OpenAICompletionsAPI, false},
		{"compatible endpoint in the spec", RoutingConfig{ModelID: "openai:gpt-6-luna@https://compatible.example/v1"}, OpenAICompletionsAPI, true},
		{"compatible endpoint setting", RoutingConfig{ModelID: "openai:gpt-6-luna", OpenAIBaseURL: "https://compatible.example/v1"}, OpenAICompletionsAPI, true},
		{"explicit protocol", RoutingConfig{ModelID: "openai:gpt-6-luna", Providers: map[string]ProviderConfig{"openai": {Protocol: "openai-chat"}}}, OpenAICompletionsAPI, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			routed, err := resolveRoute(tc.routing, func(string) string { return "" })
			if err != nil {
				t.Fatal(err)
			}
			if routed.API != tc.api || routed.Model.API != tc.api || routed.Model.Provider != "openai" {
				t.Fatalf("route = %s / %+v", routed.API, routed.Model)
			}
			if routed.Model.Reasoning != tc.reasoning {
				t.Fatalf("reasoning = %v, want %v", routed.Model.Reasoning, tc.reasoning)
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

// The gpt-6 family is in the catalog, so a run on it reports what it cost
// instead of an unknown price, on either OpenAI route.
func TestGPT6LunaRoutesCarryCatalogPrice(t *testing.T) {
	want := types.ModelCost{Input: 0.10, Output: 0.50, CacheRead: 0.01, CacheWrite: 0.125}
	for _, spec := range []string{"openai:gpt-6-luna", "openai-responses:gpt-6-luna"} {
		routed, err := resolveRoute(RoutingConfig{ModelID: spec}, func(string) string { return "" })
		if err != nil {
			t.Fatal(err)
		}
		if routed.Model.Cost != want || routed.Model.ContextWindow != 1050000 || !routed.Model.Reasoning {
			t.Fatalf("%s: cost %+v context %d reasoning %v", spec, routed.Model.Cost, routed.Model.ContextWindow, routed.Model.Reasoning)
		}
	}
}
