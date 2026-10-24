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
	"time"

	"go.harness.dev/harness/internal/compaction"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/retry"
)

func TestChatGPTCredentialSourceAllRoles(t *testing.T) {
	var credentials atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/responses" || request.Header.Get("Authorization") != fmt.Sprintf("Bearer request-token-%d", credentials.Load()) {
			t.Error("credential was not acquired for this Responses request")
		}
		if request.Header.Get("ChatGPT-Account-ID") != "" || request.Header.Get("OpenAI-Beta") != "" {
			t.Error("legacy headers leaked into the supported route")
		}
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if _, ok := payload["max_output_tokens"]; ok {
			t.Error("ChatGPT route sent an output cap")
		}
		if _, ok := payload["temperature"]; ok {
			t.Error("ChatGPT route sent a temperature")
		}
		if string(payload["store"]) != "false" || string(payload["stream"]) != "true" || len(payload["tools"]) > 0 && !strings.Contains(string(payload["tools"]), `"type":"namespace"`) {
			t.Error("ChatGPT route did not use its request profile")
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"done\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"fixture\"}}\n\n")
	}))
	defer endpoint.Close()
	source := func(context.Context) (RequestCredential, error) {
		version := credentials.Add(1)
		return RequestCredential{AccessToken: fmt.Sprintf("request-token-%d", version), BaseURL: endpoint.URL + "/v1"}, nil
	}
	roles, err := ResolveRoles(RoleRoutingConfig{DefaultModel: "openai-chatgpt:gpt-6.1-sol", ChatGPTCredentials: source})
	if err != nil {
		t.Fatal(err)
	}
	history := types.Context{Messages: []types.Message{types.UserMessage{Content: types.StringContent("hello")}}, Tools: []types.Tool{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}}}
	temperature := 0.2
	options := &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{MaxTokens: 13107, Temperature: &temperature}}
	for _, router := range []ProviderRouter{roles.Default, roles.Plan, roles.Smol} {
		if router.Model.API != "openai-responses" || !router.Model.Cost.Unknown {
			t.Fatal("plan route protocol or billing mode changed")
		}
		response, err := router.StreamFn(context.Background(), router.Model, history, options).Result(context.Background())
		if err != nil || response.StopReason != types.StopStop || !response.Usage.Cost.Unknown {
			t.Fatalf("role response: %v %+v", err, response)
		}
	}
	if credentials.Load() != 3 {
		t.Fatal("credential source was captured as a static token")
	}
	if options.MaxTokens != 13107 || options.Temperature == nil {
		t.Fatal("route mutated caller options")
	}
	for _, summarize := range []func(context.Context, types.Context) error{
		func(ctx context.Context, history types.Context) error {
			_, err := compaction.GenerateSummaryResult(ctx, roles.Default.StreamFn, roles.Default.Model, compaction.CompactionSettings{ReserveTokens: 16384}, history.Messages, "")
			return err
		},
		func(ctx context.Context, history types.Context) error {
			_, err := compaction.GenerateTurnPrefixSummaryResult(ctx, roles.Default.StreamFn, roles.Default.Model, compaction.CompactionSettings{ReserveTokens: 16384}, history.Messages)
			return err
		},
	} {
		if err := summarize(context.Background(), history); err != nil {
			t.Fatalf("ChatGPT compaction/overflow summary: %v", err)
		}
	}
}

func TestChatGPTRejectsCredentialAndEndpointOverrides(t *testing.T) {
	source := func(context.Context) (RequestCredential, error) {
		return RequestCredential{AccessToken: "never-send", BaseURL: "https://api.openai.com/v1"}, nil
	}
	for _, configuration := range []RoutingConfig{
		{ModelID: "openai-chatgpt:test"},
		{ModelID: "openai-chatgpt:test@https://example.test/v1", ChatGPTCredentials: source},
		{ModelID: "openai-chatgpt:test", BaseURL: "https://example.test/v1", ChatGPTCredentials: source},
		{ModelID: "openai-chatgpt:test", APIKey: "paid-api-key", ChatGPTCredentials: source},
		{ModelID: "openai-chatgpt:test", MaxTokens: 100, ChatGPTCredentials: source},
		{ModelID: "openai-chatgpt:test", ChatGPTCredentials: source, Providers: map[string]ProviderConfig{"openai-chatgpt": {Headers: map[string]string{"Authorization": "Bearer replaced"}}}},
	} {
		configuration.Env = func(string) string { return "" }
		if _, err := NewProviderRouter(configuration); err == nil {
			t.Fatal("unsafe ChatGPT routing configuration was accepted")
		}
	}
	router, err := NewProviderRouter(RoutingConfig{ModelID: "openai-chatgpt:test", ChatGPTCredentials: source, Env: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	for _, options := range []*types.SimpleStreamOptions{
		{StreamOptions: types.StreamOptions{APIKey: "replacement"}},
		{StreamOptions: types.StreamOptions{Headers: map[string]string{"Authorization": "Bearer replacement"}}},
		{StreamOptions: types.StreamOptions{Query: map[string]string{"key": "replacement"}}},
	} {
		response, err := router.StreamFn(context.Background(), router.Model, types.Context{}, options).Result(context.Background())
		if err != nil || response.StopReason != types.StopError {
			t.Fatalf("request override was not rejected: %v %+v", err, response)
		}
	}
}

func TestChatGPTTokenErrorsDoNotFallbackToAPIKeys(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "paid-key-must-not-be-used")
	source := func(context.Context) (RequestCredential, error) {
		return RequestCredential{}, fmt.Errorf("OpenAI sign-in revoked; run tempest login openai")
	}
	router, err := NewProviderRouter(RoutingConfig{ModelID: "openai-chatgpt:test", ChatGPTCredentials: source})
	if err != nil {
		t.Fatal(err)
	}
	response, err := router.StreamFn(context.Background(), router.Model, types.Context{}, nil).Result(context.Background())
	if err != nil || !strings.Contains(response.ErrorMessage, "tempest login openai") {
		t.Fatalf("token error: %v %+v", err, response)
	}
	ordinary, err := resolveRoute(RoutingConfig{ModelID: "openai:test"}, func(name string) string {
		if name == "OPENAI_API_KEY" {
			return "paid-key"
		}
		return ""
	})
	if err != nil || ordinary.API != OpenAICompletionsAPI || ordinary.APIKey != "paid-key" {
		t.Fatal("existing API-key route changed")
	}
}

func TestChatGPTAuthenticationFailureSuggestsLogin(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			endpoint := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.WriteHeader(status)
				io.WriteString(writer, `{"detail":"admission denied fixture-secret"}`)
			}))
			defer endpoint.Close()
			source := func(context.Context) (RequestCredential, error) {
				return RequestCredential{AccessToken: "fixture-secret", BaseURL: endpoint.URL + "/v1"}, nil
			}
			router, err := NewProviderRouter(RoutingConfig{ModelID: "openai-chatgpt:test", ChatGPTCredentials: source, Env: func(string) string { return "" }})
			if err != nil {
				t.Fatal(err)
			}
			response, err := router.StreamFn(context.Background(), router.Model, types.Context{}, nil).Result(context.Background())
			if err != nil || strings.Contains(response.ErrorMessage, "fixture-secret") || !strings.Contains(response.ErrorMessage, fmt.Sprintf("provider HTTP %d", status)) {
				t.Fatalf("unsafe authentication diagnostic: %v %+v", err, response)
			}
			if wantsLogin := status == http.StatusUnauthorized; strings.Contains(response.ErrorMessage, "tempest login openai") != wantsLogin {
				t.Fatalf("incorrect recovery for HTTP %d: %s", status, response.ErrorMessage)
			}
		})
	}
}

func TestChatGPTIgnoresUnrelatedOpenAIBaseURLs(t *testing.T) {
	source := func(context.Context) (RequestCredential, error) {
		return RequestCredential{AccessToken: "fixture", BaseURL: "https://api.openai.com/v1"}, nil
	}
	router, err := NewProviderRouter(RoutingConfig{
		ModelID: "openai-chatgpt:test", OpenAIBaseURL: "https://another-tool.example/v1", ChatGPTCredentials: source,
		MaxTokens: 32000, MaxTokensDefaulted: true,
		Env: func(name string) string {
			if name == "OPENAI_BASE_URL" {
				return "https://unrelated-tool.example/v1"
			}
			return ""
		},
	})
	if err != nil || router.Model.BaseURL != "https://api.openai.com/v1" {
		t.Fatalf("unrelated URL blocked or redirected plan route: %v", err)
	}
}

func TestChatGPTPlanUsageLimitDoesNotRetry(t *testing.T) {
	var attempts atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"code":"subscription_sharing_usage_limit_exceeded","message":"app usage limit reached"}}`)
	}))
	defer endpoint.Close()
	policy := retry.DefaultConfig()
	policy.MaxAttempts = 2
	policy.BaseDelay = time.Millisecond
	router, err := NewProviderRouter(RoutingConfig{
		ModelID: "openai-chatgpt:test", Retry: &policy, Env: func(string) string { return "" },
		ChatGPTCredentials: func(context.Context) (RequestCredential, error) {
			return RequestCredential{AccessToken: "fixture", BaseURL: endpoint.URL + "/v1"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := router.StreamFn(t.Context(), router.Model, types.Context{}, nil).Result(t.Context())
	if err != nil || result.StopReason != types.StopError || attempts.Load() != 1 {
		t.Fatalf("plan usage limit retried: attempts=%d result=%+v err=%v", attempts.Load(), result, err)
	}
}
