package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/catalog"
	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
)

func TestProviderProtocolFallbackLimits(t *testing.T) {
	for name := range BuiltinProviders() {
		t.Run(name, func(t *testing.T) {
			rt, err := resolveRoute(RoutingConfig{ModelID: name + ":unlisted", Providers: map[string]ProviderConfig{name: {BaseURL: "https://example.test"}}}, func(string) string { return "" })
			if err != nil {
				t.Fatal(err)
			}
			if rt.Model.ContextWindow <= 0 || rt.Model.MaxTokens <= 0 || rt.Model.MaxTokens > rt.Model.ContextWindow-4096 {
				t.Fatalf("fallback limits: %+v", rt.Model)
			}
		})
	}
}

func TestProviderCatalogFamilies(t *testing.T) {
	for _, tc := range []struct{ route, family, id string }{
		{"openai-responses:gpt-5.6", "openai", "gpt-5.6"},
		{"vertex:gemini-3.8-flash", "google", "gemini-3.8-flash"},
		{"vertex:claude-haiku-4-5@20251001", "anthropic", "claude-haiku-4-5"},
		{"vertex:projects/p/locations/us/publishers/anthropic/models/claude-haiku-4-5@20251001", "anthropic", "claude-haiku-4-5"},
		{"bedrock:us.anthropic.claude-haiku-4-5-20251001-v1:0", "anthropic", "claude-haiku-4-5"},
		{"bedrock:arn:aws:bedrock:us-east-1:123:inference-profile/us.anthropic.claude-haiku-4-5-20251001-v1:0", "anthropic", "claude-haiku-4-5"},
	} {
		t.Run(tc.route, func(t *testing.T) {
			rt, err := resolveRoute(RoutingConfig{ModelID: tc.route}, func(string) string { return "" })
			if err != nil {
				t.Fatal(err)
			}
			cm, ok := catalog.Lookup(tc.family, tc.id)
			if !ok {
				t.Fatal("missing catalog fixture")
			}
			if rt.Model.ContextWindow != cm.ContextWindow || rt.Model.Cost != cm.Cost || !rt.Model.Reasoning || !rt.Model.SupportsVision() || rt.Model.ID != parseModelSpec(tc.route).ModelID {
				t.Fatalf("catalog metadata lost: %+v", rt.Model)
			}
		})
	}
	rt, err := resolveRoute(RoutingConfig{ModelID: "perplexity:sonar"}, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if !rt.Model.Cost.Unknown {
		t.Fatal("Perplexity must not inherit OpenAI prices")
	}
}

func TestProviderModelPartialOverrides(t *testing.T) {
	cm, ok := catalog.Lookup("openai", "gpt-5.6")
	if !ok {
		t.Fatal("missing fixture")
	}
	for _, tc := range []struct {
		name, raw string
		reasoning bool
		cost      types.ModelCost
	}{
		{"price only", `{"models":[{"id":"gpt-5.6","cost":{"input":7,"output":8}}]}`, true, types.ModelCost{Input: 7, Output: 8}},
		{"name only", `{"models":[{"id":"gpt-5.6","name":"custom"}]}`, true, cm.Cost},
		{"explicit false and free", `{"models":[{"id":"gpt-5.6","reasoning":false,"cost":{"input":0,"output":0}}]}`, false, types.ModelCost{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var p ProviderConfig
			if err := json.Unmarshal([]byte(tc.raw), &p); err != nil {
				t.Fatal(err)
			}
			rt, err := resolveRoute(RoutingConfig{ModelID: "openai-responses:gpt-5.6", Providers: map[string]ProviderConfig{"openai-responses": p}}, func(string) string { return "" })
			if err != nil {
				t.Fatal(err)
			}
			if rt.Model.ContextWindow != cm.ContextWindow || !rt.Model.SupportsVision() || rt.Model.Reasoning != tc.reasoning || rt.Model.Cost != tc.cost {
				t.Fatalf("partial override erased metadata: %+v", rt.Model)
			}
		})
	}
	for _, id := range []string{"unlisted", "gpt-5.6"} {
		rt, err := resolveRoute(RoutingConfig{ModelID: "openai:" + id, Providers: map[string]ProviderConfig{"openai": {Models: []types.Model{{ID: id}}}}}, func(string) string { return "" })
		if err != nil {
			t.Fatal(err)
		}
		if id == "unlisted" && !rt.Model.Cost.Unknown {
			t.Fatal("Go model literal invented a free price")
		}
		if id == "gpt-5.6" && rt.Model.Cost != cm.Cost {
			t.Fatal("Go model literal erased catalog price")
		}
	}
}

func TestProviderAzureEndpointShapes(t *testing.T) {
	for _, tc := range []struct{ base, path, version string }{
		{"", "/openai/deployments/reasoner/chat/completions", "2025-04-01-preview"},
		{"/openai/deployments/deployed", "/openai/deployments/deployed/chat/completions", "2025-04-01-preview"},
		{"/openai/v1/", "/openai/v1/chat/completions", ""},
	} {
		t.Run(tc.base, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.path || r.URL.Query().Get("api-version") != tc.version || r.Header.Get("api-key") != "azure-fixture" {
					t.Errorf("Azure route: %s", r.URL)
				}
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body["max_tokens"] != nil || string(body["max_completion_tokens"]) != "64" {
					t.Errorf("Azure token fields: %s", body)
				}
				io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
			}))
			defer server.Close()
			router, err := NewProviderRouter(RoutingConfig{ModelID: "azure:reasoner", APIKey: "azure-fixture", MaxTokens: 64, BaseURL: server.URL + tc.base, Env: func(string) string { return "" }})
			if err != nil {
				t.Fatal(err)
			}
			result, err := router.StreamFn(context.Background(), router.Model, types.Context{}, nil).Result(context.Background())
			if err != nil || result.StopReason != types.StopStop {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

func TestProviderQueryCredentialsStayOutOfErrors(t *testing.T) {
	const secret = "credential /+?fixture"
	for _, transportFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(transportFailure), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(400)
				fmt.Fprintf(w, "echo %s header-private query-private", secret)
			}))
			endpoint := server.URL
			if transportFailure {
				server.Close()
			} else {
				defer server.Close()
			}
			router, err := NewProviderRouter(RoutingConfig{ModelID: "private:test", Env: func(string) string { return "" }, Providers: map[string]ProviderConfig{"private": {Protocol: "openai-chat", BaseURL: endpoint, APIKey: secret, AuthQuery: "arbitrary", Headers: map[string]string{"X-Custom": "header-private"}, Query: map[string]string{"opaque": "query-private"}}}})
			if err != nil {
				t.Fatal(err)
			}
			result, err := router.StreamFn(context.Background(), router.Model, types.Context{}, nil).Result(context.Background())
			if err != nil || result.StopReason != types.StopError {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			encoded, _ := json.Marshal(result)
			for _, value := range []string{secret, "header-private", "query-private", "arbitrary="} {
				if strings.Contains(string(encoded), value) {
					t.Fatal("credential reached persisted error")
				}
			}
			if transportFailure && result.ErrorCode != "network_failure" {
				t.Fatalf("lost classification: %s", result.ErrorCode)
			}
		})
	}
}

func TestProviderDeepSeekAssistantTurnsAlwaysCarryReasoning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []map[string]any `json:"messages"`
			Tools    []any            `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		assistants := 0
		for _, m := range body.Messages {
			if m["role"] == "assistant" {
				assistants++
				if _, ok := m["reasoning_content"].(string); !ok {
					t.Error("assistant missing reasoning_content")
				}
			}
		}
		if assistants != 4 || len(body.Tools) != 1 {
			t.Errorf("assistants=%d tools=%d", assistants, len(body.Tools))
		}
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
	}))
	defer server.Close()
	router, err := NewProviderRouter(RoutingConfig{ModelID: "deepseek:deepseek-flash", APIKey: "fixture", BaseURL: server.URL, Env: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	history := types.Context{Tools: []types.Tool{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}}, Messages: []types.Message{
		types.UserMessage{Content: types.StringContent("hi")},
		types.AssistantMessage{Model: "claude", Provider: "anthropic", StopReason: types.StopStop, Content: []types.ContentBlock{types.NewThinking("foreign", "signature"), types.NewText("answer")}},
		types.UserMessage{Content: types.StringContent("again")},
		types.AssistantMessage{Model: "deepseek-flash", Provider: "deepseek", API: OpenAICompletionsAPI, StopReason: types.StopStop, Content: []types.ContentBlock{types.NewThinking("earlier thinking", ""), types.NewText("answer")}},
		types.UserMessage{Content: types.StringContent("think only")},
		types.AssistantMessage{Model: "deepseek-flash", Provider: "deepseek", API: OpenAICompletionsAPI, StopReason: types.StopStop, Content: []types.ContentBlock{types.NewThinking("only thinking", "")}},
		types.UserMessage{Content: types.StringContent("tool")},
		types.AssistantMessage{Model: "deepseek-flash", Provider: "deepseek", API: OpenAICompletionsAPI, StopReason: types.StopToolUse, Content: []types.ContentBlock{{Type: types.BlockToolCall, ID: "call1", Name: "lookup", Arguments: json.RawMessage(`{}`)}}},
		types.ToolResultMessage{ToolCallID: "call1", ToolName: "lookup", Content: []types.ContentBlock{types.NewText("result")}},
	}}
	result, err := router.StreamFn(context.Background(), router.Model, history, nil).Result(context.Background())
	if err != nil || result.StopReason != types.StopStop {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestProviderOpenAICacheKeyTrailingSlash(t *testing.T) {
	for _, endpoint := range []string{"https://api.openai.com/v1", "https://api.openai.com/v1/"} {
		rt, err := resolveRoute(RoutingConfig{ModelID: "openai:unlisted", BaseURL: endpoint}, func(string) string { return "" })
		if err != nil {
			t.Fatal(err)
		}
		if rt.Model.OpenAICompat == nil || rt.Model.OpenAICompat.SupportsPromptCacheKey == nil || !*rt.Model.OpenAICompat.SupportsPromptCacheKey {
			t.Fatalf("cache key disabled for %s", endpoint)
		}
	}
}

func TestProviderToolChoiceAndMistralIDs(t *testing.T) {
	for _, name := range []string{"mistral", "ollama", "cerebras"} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Choice   any `json:"tool_choice"`
					Messages []struct {
						Role       string `json:"role"`
						ToolCallID string `json:"tool_call_id"`
						ToolCalls  []struct {
							ID string `json:"id"`
						} `json:"tool_calls"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if name == "ollama" && body.Choice != nil {
					t.Error("Ollama received tool_choice")
				}
				if name != "ollama" && body.Choice != "auto" {
					t.Error("tool_choice auto missing")
				}
				if name == "mistral" {
					call, result := body.Messages[1].ToolCalls[0].ID, body.Messages[2].ToolCallID
					if len(call) != 9 || call != result || strings.Trim(call, "0123456789abcdef") != "" {
						t.Error("Mistral IDs were not normalized consistently")
					}
				}
				io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
			}))
			defer server.Close()
			router, err := NewProviderRouter(RoutingConfig{ModelID: name + ":unlisted", APIKey: "fixture", BaseURL: server.URL, Env: func(string) string { return "" }})
			if err != nil {
				t.Fatal(err)
			}
			history := types.Context{Tools: []types.Tool{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}}, Messages: []types.Message{
				types.UserMessage{Content: types.StringContent("hi")},
				types.AssistantMessage{Provider: "openai", API: OpenAICompletionsAPI, Model: "foreign", StopReason: types.StopToolUse, Content: []types.ContentBlock{{Type: types.BlockToolCall, ID: "invalid/foreign-tool-id", Name: "lookup", Arguments: json.RawMessage(`{}`)}}},
				types.ToolResultMessage{ToolCallID: "invalid/foreign-tool-id", ToolName: "lookup", Content: []types.ContentBlock{types.NewText("done")}},
			}}
			result, err := router.StreamFn(context.Background(), router.Model, history, nil).Result(context.Background())
			if err != nil || result.StopReason != types.StopStop {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

func TestEveryProtocolClampsOutputToContextAndModel(t *testing.T) {
	for _, protocol := range []string{"openai-chat", "openai-responses", "anthropic", "google", "vertex", "bedrock"} {
		t.Run(protocol, func(t *testing.T) {
			model := types.Model{ID: "unlisted", API: protocolAPI(protocol), ContextWindow: 4097, MaxTokens: 64}
			rt := route{Model: model, Provider: ProviderConfig{Protocol: protocol, NoAuth: new(true)}}
			router := (ProviderRouter{Model: model}).withStream(func(_ context.Context, model types.Model, _ types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream {
				if opts.MaxTokens != 1 {
					t.Errorf("max tokens = %d, want 1", opts.MaxTokens)
				}
				s := stream.NewAssistantStream(model.API, model.Provider, model.ID)
				s.Push(types.StreamEvent{Type: types.EvDone, Reason: types.StopStop, Message: &types.AssistantMessage{StopReason: types.StopStop}})
				s.End()
				return s
			}, rt, nil)
			result, err := router.StreamFn(context.Background(), model, types.Context{Messages: []types.Message{types.UserMessage{Content: types.StringContent("prompt")}}}, &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{MaxTokens: 100000}}).Result(context.Background())
			if err != nil || result.StopReason != types.StopStop {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}
