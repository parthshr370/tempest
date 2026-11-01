package adapter

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
)

// GetApiKey names only a provider, so its answer belongs at one credential
// scope per provider for the whole run: the primary's scope for the
// primary's provider, and for any other provider its configured default scope
// (where its route goes without an inline @endpoint). Every other scope of a
// provider authenticates with its own route's key, whatever order the chain
// reaches the scopes in, on the switching turn, on later turns and after a
// resume onto the recorded backup.
func TestGetAPIKeyBelongsToOneScopePerProvider(t *testing.T) {
	const (
		openAIToolCall    = "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"note\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n"
		openAIDone        = "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n"
		anthropicToolCall = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":1}}}\n\n" +
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"note\",\"input\":{}}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\n" +
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":1}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		anthropicDone = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_2\",\"usage\":{\"input_tokens\":1}}}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	)
	note := agent.AgentTool{
		Tool: types.Tool{Name: "note", Description: "records a note", Parameters: json.RawMessage(`{"type":"object"}`)},
		Execute: func(context.Context, string, json.RawMessage, agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			return agent.AgentToolResult{Content: []types.ContentBlock{types.NewText("noted")}}, nil
		},
	}
	callback := map[string]string{"anthropic": "anthropic-callback-key", "openai": "openai-callback-key"}
	// Endpoints p (the primary) and z speak Anthropic; x and y speak OpenAI.
	// Backup specs name them as {p}, {x}, {y} and {z}; the last backup answers.
	for _, tc := range []struct {
		name string
		// openAIBaseURL and openAIEnv configure OpenAI's default endpoint.
		openAIBaseURL, openAIEnv string
		backups                  []string
		// answers is the endpoint of the last backup, the one that answers.
		answers string
		// want is the key each endpoint the run reaches must receive.
		want                     map[string]string
		wantLookups, wantResumed []string
	}{
		{
			name:        "secondary provider endpoints, neither the default",
			backups:     []string{"openai:first@{x}", "openai:last@{y}"},
			answers:     "y",
			want:        map[string]string{"p": "anthropic-callback-key", "x": "openai-own-key", "y": "openai-own-key"},
			wantLookups: []string{"anthropic"},
		},
		{
			name:          "default scope reached first",
			openAIBaseURL: "{x}",
			backups:       []string{"openai:first", "openai:last@{y}"},
			answers:       "y",
			want:          map[string]string{"p": "anthropic-callback-key", "x": "openai-callback-key", "y": "openai-own-key"},
			wantLookups:   []string{"anthropic", "openai"},
		},
		{
			name:        "default scope reached last",
			openAIEnv:   "{y}",
			backups:     []string{"openai:first@{x}", "openai:last"},
			answers:     "y",
			want:        map[string]string{"p": "anthropic-callback-key", "x": "openai-own-key", "y": "openai-callback-key"},
			wantLookups: []string{"anthropic", "openai", "openai"},
			wantResumed: []string{"openai"},
		},
		{
			name:          "back to the primary's provider at another scope",
			openAIBaseURL: "{x}",
			backups:       []string{"openai:first", "anthropic:other@{z}"},
			answers:       "z",
			want:          map[string]string{"p": "anthropic-callback-key", "x": "openai-callback-key", "z": "anthropic-own-key"},
			wantLookups:   []string{"anthropic", "openai"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorders, urls := map[string]*authRecorder{}, map[string]string{}
			for _, name := range []string{"p", "x", "y", "z"} {
				recorder := &authRecorder{status: http.StatusServiceUnavailable}
				if name == tc.answers {
					recorder.status = 0
					recorder.replies = []string{openAIToolCall, openAIDone, openAIDone}
					if name == "z" {
						recorder.replies = []string{anthropicToolCall, anthropicDone, anthropicDone}
					}
				}
				server := httptest.NewServer(recorder)
				defer server.Close()
				recorders[name], urls[name] = recorder, server.URL
			}
			expand := func(spec string) string {
				for name, url := range urls {
					spec = strings.ReplaceAll(spec, "{"+name+"}", url)
				}
				return spec
			}
			backups := make([]string, len(tc.backups))
			for i, spec := range tc.backups {
				backups[i] = expand(spec)
			}
			env := map[string]string{"ANTHROPIC_API_KEY": "anthropic-own-key", "OPENAI_API_KEY": "openai-own-key", "OPENAI_BASE_URL": expand(tc.openAIEnv)}
			router, err := NewProviderRouter(RoutingConfig{
				ModelID:        "anthropic:primary@" + urls["p"],
				OpenAIBaseURL:  expand(tc.openAIBaseURL),
				FallbackModels: backups,
				Env:            func(name string) string { return env[name] },
			})
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var lookups []string
			run := func(resume *agent.FallbackChoice) {
				t.Helper()
				mu.Lock()
				lookups = nil
				mu.Unlock()
				a := agent.NewAgent(agent.AgentOptions{
					InitialState:   &agent.AgentState{SystemPrompt: "test", Model: router.Model, Tools: []agent.AgentTool{note}},
					StreamFn:       router.StreamFn,
					FallbackModels: router.Fallbacks,
					GetApiKey: func(provider string) string {
						mu.Lock()
						defer mu.Unlock()
						lookups = append(lookups, provider)
						return callback[provider]
					},
				})
				if resume != nil {
					a.Subscribe(func(_ context.Context, event agent.AgentEvent) error {
						if event.Type == agent.EventAgentStart {
							a.SetRunState(agent.RunState{Fallback: resume})
						}
						return nil
					})
				}
				result, err := a.PromptText(context.Background(), "hello")
				if err != nil || result.StopReason != agent.StopCompleted {
					t.Fatalf("run = %+v, %v", result, err)
				}
			}
			checkRequests := func(phase string, counts map[string]int, wantLookups []string) {
				t.Helper()
				for name, recorder := range recorders {
					requests := recorder.recorded(t)
					if len(requests) != counts[name] {
						t.Fatalf("%s: endpoint %s got %d requests, want %d", phase, name, len(requests), counts[name])
					}
					for i, got := range requests {
						key := strings.TrimPrefix(got.authorization, "Bearer ")
						if name == "p" || name == "z" {
							key = got.apiKey
						}
						if key != tc.want[name] {
							t.Fatalf("%s: endpoint %s request %d authenticated with %q, want %q", phase, name, i, key, tc.want[name])
						}
					}
				}
				mu.Lock()
				defer mu.Unlock()
				if !slices.Equal(lookups, wantLookups) {
					t.Fatalf("%s: GetApiKey lookups = %v, want %v", phase, lookups, wantLookups)
				}
			}
			// The switching turn and a later turn on the backup that answers.
			run(nil)
			counts := map[string]int{}
			for name := range tc.want {
				counts[name] = 1
			}
			counts[tc.answers] = 2
			checkRequests("switching run", counts, tc.wantLookups)
			// A run resumed onto the recorded backup starts there.
			spec, _, _ := strings.Cut(tc.backups[len(tc.backups)-1], "@")
			run(&agent.FallbackChoice{Index: len(tc.backups), Model: spec})
			counts[tc.answers] = 3
			checkRequests("resumed run", counts, tc.wantResumed)
		})
	}
}

// A credential scope compares the scheme and host as DNS does, ignoring
// case, and keeps the path exact. On Azure the resource is the scope: a
// deployment or /openai/v1 path selects a model within it, while any other
// path may separate tenants.
func TestRoutingForModelKeepsAzureResourceKeyAcrossEquivalentEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name, configured, endpoint string
		wantKey                    bool
	}{
		{"root trailing slash", "", "https://resource.example///", true},
		{"v1 trailing slash", "", "https://resource.example/openai/v1/", true},
		{"deployment trailing slash", "", "https://resource.example/openai/deployments/backup/", true},
		{"hostname case", "", "https://RESOURCE.example", true},
		// Only a lowercase @http(s):// starts an inline endpoint, so the
		// scheme's case is varied on the configured endpoint.
		{"scheme case", "HTTPS://resource.example", "https://resource.example/openai/v1", true},
		{"custom tenant path", "", "https://resource.example/tenant", false},
		{"custom tenant path case", "", "https://resource.example/Tenant", false},
		{"custom deployment subpath", "", "https://resource.example/openai/deployments/backup/extra", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := RoutingConfig{ModelID: "azure:primary", BaseURL: cmp.Or(tc.configured, "https://resource.example"), APIKey: "resource-key", Env: func(string) string { return "" }}
			derived := config.ForModel("azure:backup@" + tc.endpoint)
			router, err := NewProviderRouter(derived)
			if err != nil {
				t.Fatal(err)
			}
			// The row must reach the endpoint it names, not the configured
			// one with the endpoint left in the model ID.
			if router.Model.ID != "backup" || !strings.HasPrefix(router.Model.BaseURL, strings.TrimRight(tc.endpoint, "/")) {
				t.Fatalf("routed model %q at %q, want backup at %s", router.Model.ID, router.Model.BaseURL, tc.endpoint)
			}
			if (derived.APIKey != "") != tc.wantKey {
				t.Fatalf("kept resource key = %v, want %v", derived.APIKey != "", tc.wantKey)
			}
		})
	}
}

// Endpoint parameters belong in a provider's query settings, never in an
// endpoint URL: a route appends model paths to the endpoint, which would land
// inside the query. Every source of an endpoint is held to that.
func TestEndpointsWithQueryOrFragmentAreRejected(t *testing.T) {
	env := func(string) string { return "" }
	for _, tc := range []struct {
		name string
		cfg  RoutingConfig
	}{
		{"SDK base URL query", RoutingConfig{ModelID: "azure:deployment", BaseURL: "https://resource.example?tenant=primary", APIKey: "key"}},
		{"SDK base URL fragment", RoutingConfig{ModelID: "openai:gpt-test", BaseURL: "https://api.example/v1#tenant", APIKey: "key"}},
		{"OpenAI base URL query", RoutingConfig{ModelID: "openai:gpt-test", OpenAIBaseURL: "https://api.example/v1?tenant=primary", APIKey: "key"}},
		{"Anthropic base URL query", RoutingConfig{ModelID: "anthropic:claude-test", AnthropicBaseURL: "https://api.example?tenant=primary", APIKey: "key"}},
		{"inline v1 query", RoutingConfig{ModelID: "azure:backup@https://resource.example/openai/v1?tenant=other", APIKey: "key"}},
		{"inline deployment query", RoutingConfig{ModelID: "azure:backup@https://resource.example/openai/deployments/backup?tenant=other", APIKey: "key"}},
		{"provider definition query", RoutingConfig{ModelID: "azure:backup", Providers: map[string]ProviderConfig{"azure": {BaseURL: "https://resource.example/openai/v1?tenant=other"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg.Env = env
			router, err := NewProviderRouter(tc.cfg)
			if err == nil {
				t.Fatalf("accepted endpoint %s", router.Model.BaseURL)
			}
			if !strings.Contains(err.Error(), "query") {
				t.Fatalf("error does not name the query or fragment: %v", err)
			}
		})
	}
}
