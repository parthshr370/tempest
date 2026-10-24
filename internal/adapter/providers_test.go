package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
)

func TestProviderPresetsWireShape(t *testing.T) {
	// Endpoints and credentials follow each vendor's API documentation, linked
	// in guide/providers.md. Local servers deliberately send no dummy key.
	for name, preset := range BuiltinProviders() {
		if preset.Protocol != "openai-chat" && preset.Protocol != "openai-responses" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				wantPath := "/v1/chat/completions"
				if preset.Protocol == "openai-responses" {
					wantPath = "/v1/responses"
				}
				if name == "azure" {
					wantPath = "/v1/openai/deployments/test-model/chat/completions"
				}
				if r.Method != http.MethodPost || r.URL.Path != wantPath {
					t.Errorf("request = %s %s", r.Method, r.URL)
				}
				if preset.NoAuth != nil && *preset.NoAuth {
					if r.Header.Get("Authorization") != "" {
						t.Error("keyless server received authorization")
					}
				} else if name == "azure" {
					if r.Header.Get("api-key") != "fixture-key" || r.Header.Get("Authorization") != "" || r.URL.Query().Get("api-version") != "2025-04-01-preview" {
						t.Errorf("Azure auth/version = %v %s", r.Header, r.URL)
					}
				} else if r.Header.Get("Authorization") != "Bearer fixture-key" {
					t.Errorf("authorization = %q", r.Header.Get("Authorization"))
				}
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if string(body["model"]) != `"test-model"` || string(body["stream"]) != "true" {
					t.Errorf("request body = %s", body)
				}
				if preset.Protocol == "openai-chat" {
					field := "max_tokens"
					if name == "openai" || name == "openrouter" || name == "azure" {
						field = "max_completion_tokens"
					}
					if string(body[field]) != "64" {
						t.Errorf("%s = %s", field, body[field])
					}
					if name != "openrouter" && body["prompt_cache_key"] != nil {
						t.Error("unsupported cache field")
					}
					if (name == "mistral" || name == "cerebras") && body["stream_options"] != nil {
						t.Error("preset received unsupported stream_options")
					}
					io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				} else {
					if body["input"] == nil || string(body["max_output_tokens"]) != "64" {
						t.Errorf("Responses body = %s", body)
					}
					io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-test\"}}\n\n")
				}
			}))
			defer server.Close()
			router, err := NewProviderRouter(RoutingConfig{ModelID: name + ":test-model", MaxTokens: 64, Providers: map[string]ProviderConfig{name: {BaseURL: server.URL + "/v1"}}, Env: func(key string) string {
				if key == preset.APIKeyEnv && key != "" {
					return "fixture-key"
				}
				return ""
			}})
			if err != nil {
				t.Fatal(err)
			}
			result, err := router.StreamFn(context.Background(), router.Model, types.Context{Messages: []types.Message{types.UserMessage{Content: types.StringContent("hi")}}}, &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{SessionID: "fixture-session"}}).Result(context.Background())
			if err != nil || result.StopReason != types.StopStop || calls.Load() != 1 {
				t.Fatalf("calls=%d result=%+v err=%v", calls.Load(), result, err)
			}
		})
	}
}

func TestCustomProviderAuthAndMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Token") != "secret env-key" || r.Header.Get("X-Tenant") != "example" || r.Header.Get("Authorization") != "" || r.URL.Query().Get("version") != "custom" {
			t.Errorf("request = %v %s", r.Header, r.URL)
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if string(body["max_tokens"]) != "123" || body["reasoning_effort"] != nil || !strings.Contains(string(body["reasoning"]), "high") {
			t.Errorf("compatibility = %s", body)
		}
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
	}))
	defer server.Close()
	providers := map[string]ProviderConfig{"gateway": {Protocol: "openai-chat", BaseURL: server.URL, APIKeyEnv: "GATEWAY_TOKEN", APIKey: "settings-key", AuthHeader: "X-Token", AuthPrefix: new("secret "), Headers: map[string]string{"X-Tenant": "example"}, Query: map[string]string{"version": "custom"}, Compatibility: &types.OpenAICompat{MaxTokensField: "max_tokens", ReasoningFormat: "openrouter"}, Models: []types.Model{{ID: "Mixed/Model:variant", ContextWindow: 16384, MaxTokens: 123, Reasoning: true, Cost: types.ModelCost{Input: 1, Output: 2}}}}}
	roles, err := ResolveRoles(RoleRoutingConfig{DefaultModel: "gateway:Mixed/Model:variant", MaxTokens: 32000, Providers: providers, SecretLookup: func(name string) (string, bool) { return "env-key", name == "GATEWAY_TOKEN" }})
	if err != nil {
		t.Fatal(err)
	}
	if roles.Default.Model.MaxTokens != 123 || roles.Default.Model.ContextWindow != 16384 || roles.Default.Model.Cost.Input != 1 {
		t.Fatalf("metadata = %+v", roles.Default.Model)
	}
	result, err := roles.Default.StreamFn(context.Background(), roles.Default.Model, types.Context{Messages: []types.Message{types.UserMessage{Content: types.StringContent("hi")}}}, &types.SimpleStreamOptions{Reasoning: "high"}).Result(context.Background())
	if err != nil || result.StopReason != types.StopStop {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	encoded, err := json.Marshal(roles.Default.Model)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "env-key") || strings.Contains(string(encoded), "settings-key") {
		t.Fatal("model contains credentials")
	}
}

func TestProviderMissingKeyNamesEnvironment(t *testing.T) {
	router, err := NewProviderRouter(RoutingConfig{ModelID: "groq:test", Env: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	result, err := router.StreamFn(context.Background(), router.Model, types.Context{}, nil).Result(context.Background())
	if err != nil || !strings.Contains(result.ErrorMessage, "GROQ_API_KEY") {
		t.Fatalf("missing key result=%+v err=%v", result, err)
	}
}

func TestProviderRegistryRejectsInvalidDefinitions(t *testing.T) {
	for _, provider := range []ProviderConfig{{Protocol: "unknown", BaseURL: "https://example.com"}, {Protocol: "openai-chat", BaseURL: "https://user:secret@example.com"}, {Protocol: "openai-chat", BaseURL: "https://example.com?key=secret"}, {Protocol: "openai-chat", BaseURL: "https://example.com", Headers: map[string]string{"X-Key": "line\r\ninjection"}}, {Protocol: "openai-chat", BaseURL: "https://example.com", Compatibility: &types.OpenAICompat{MaxTokensField: "bad"}}} {
		if _, err := Providers(map[string]ProviderConfig{"custom": provider}); err == nil {
			t.Fatalf("accepted invalid definition: %s", fmt.Sprint(provider.Protocol))
		}
	}
}

func TestDeepSeekReasoningReplay(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Messages []struct {
				Role      string `json:"role"`
				Reasoning string `json:"reasoning_content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if requests.Add(1) == 1 {
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"need tool\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
		} else {
			found := false
			for _, message := range payload.Messages {
				if message.Role == "assistant" && message.Reasoning == "need tool" {
					found = true
				}
			}
			if !found {
				t.Error("reasoning_content lost on tool continuation")
			}
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		}
	}))
	defer server.Close()
	router, err := NewProviderRouter(RoutingConfig{ModelID: "deepseek:deepseek-flash", BaseURL: server.URL, APIKey: "key", Env: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	history := types.Context{Messages: []types.Message{types.UserMessage{Content: types.StringContent("hi")}}, Tools: []types.Tool{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}}}
	first, err := router.StreamFn(context.Background(), router.Model, history, nil).Result(context.Background())
	if err != nil || first.StopReason != types.StopToolUse {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	history.Messages = append(history.Messages, *first, types.ToolResultMessage{ToolCallID: "call_1", ToolName: "lookup", Content: []types.ContentBlock{types.NewText("value")}})
	last, err := router.StreamFn(context.Background(), router.Model, history, nil).Result(context.Background())
	if err != nil || last.StopReason != types.StopStop {
		t.Fatalf("last=%+v err=%v", last, err)
	}
}

func TestProviderQueryAuthentication(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != "query-secret" || r.Header.Get("Authorization") != "" {
			t.Errorf("query authentication=%s %v", r.URL, r.Header)
		}
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
	}))
	defer server.Close()
	router, err := NewProviderRouter(RoutingConfig{ModelID: "query:test", Env: func(string) string { return "" }, Providers: map[string]ProviderConfig{"query": {Protocol: "openai-chat", BaseURL: server.URL, APIKey: "query-secret", AuthQuery: "token"}}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := router.StreamFn(context.Background(), router.Model, types.Context{}, nil).Result(context.Background())
	if err != nil || result.StopReason != types.StopStop {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestProviderPricesRemainUnknownWhenOmitted(t *testing.T) {
	var provider ProviderConfig
	if err := json.Unmarshal([]byte(`{"protocol":"openai-chat","base_url":"https://example.com","models":[{"id":"no-price"},{"id":"free","cost":{"input":0,"output":0}}]}`), &provider); err != nil {
		t.Fatal(err)
	}
	if !provider.Models[0].Cost.Unknown || provider.Models[1].Cost.Unknown {
		t.Fatalf("prices=%+v", provider.Models)
	}
}

func TestProviderLegacyAnthropicOAuthPriority(t *testing.T) {
	lookup := func(name string) string {
		switch name {
		case "ANTHROPIC_OAUTH_TOKEN":
			return "oauth"
		case "ANTHROPIC_API_KEY":
			return "api"
		}
		return ""
	}
	routed, err := resolveRoute(RoutingConfig{}, lookup)
	if err != nil || routed.APIKey != "oauth" {
		t.Fatalf("legacy OAuth priority=%q err=%v", routed.APIKey, err)
	}
	routed, err = resolveRoute(RoutingConfig{APIKey: "explicit"}, lookup)
	if err != nil || routed.APIKey != "explicit" {
		t.Fatalf("explicit key priority=%q err=%v", routed.APIKey, err)
	}
}

func TestProviderGoogleEndpointExpansion(t *testing.T) {
	if BuiltinProviders()["google"].BaseURL != "https://generativelanguage.googleapis.com" {
		t.Fatal("Google preset must not include the protocol's /v1beta path")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/models/gemini-test:streamGenerateContent" || r.URL.Query().Get("alt") != "sse" || r.Header.Get("x-goog-api-key") != "key" {
			t.Errorf("Google request=%s %v", r.URL, r.Header)
		}
		io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}]}\n\n")
	}))
	defer server.Close()
	router, err := NewProviderRouter(RoutingConfig{ModelID: "google:gemini-test", BaseURL: server.URL, APIKey: "key", Env: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	result, err := router.StreamFn(context.Background(), router.Model, types.Context{}, nil).Result(context.Background())
	if err != nil || result.StopReason != types.StopStop {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestProviderPresetEndpoints(t *testing.T) {
	expected := map[string]string{
		"anthropic":        "https://api.anthropic.com",
		"openai":           "https://api.openai.com/v1",
		"google":           "https://generativelanguage.googleapis.com",
		"openrouter":       "https://openrouter.ai/api/v1",
		"groq":             "https://api.groq.com/openai/v1",
		"mistral":          "https://api.mistral.ai/v1",
		"xai":              "https://api.x.ai/v1",
		"deepseek":         "https://api.deepseek.com",
		"together":         "https://api.together.ai/v1",
		"fireworks":        "https://api.fireworks.ai/inference/v1",
		"cerebras":         "https://api.cerebras.ai/v1",
		"perplexity":       "https://api.perplexity.ai/v1",
		"ollama":           "http://localhost:11434/v1",
		"lmstudio":         "http://localhost:1234/v1",
		"vllm":             "http://localhost:8000/v1",
		"llamacpp":         "http://localhost:8080/v1",
		"azure":            "",
		"openai-responses": "https://api.openai.com/v1",
		"bedrock":          "",
		"vertex":           "",
	}
	actual := BuiltinProviders()
	if len(expected) != len(actual) {
		t.Fatal("preset list changed without endpoint coverage")
	}
	for name, endpoint := range expected {
		if actual[name].BaseURL != endpoint {
			t.Errorf("%s endpoint = %s, want %s", name, actual[name].BaseURL, endpoint)
		}
	}
}
