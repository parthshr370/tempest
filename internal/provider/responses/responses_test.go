package responses

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/conformance"
)

func TestConformance(t *testing.T) {
	conformance.Run(t, conformance.Factory{Dialect: "responses", Stream: StreamSimple})
}

func TestEncryptedReasoningRoundTrip(t *testing.T) {
	const reasoning = `{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"plan"}],"encrypted_content":"ciphertext"}`
	requests := make(chan map[string]json.RawMessage, 2)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || r.URL.Query().Get("route") != "fixture" || r.Header.Get("Authorization") != "Bearer key" {
			t.Errorf("request: %s %v", r.URL, r.Header)
		}
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		requests <- payload
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"rs_1\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"response.reasoning_summary_text.delta\",\"output_index\":0,\"delta\":\"plan\"}\n\n")
		io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":"+reasoning+"}\n\n")
		io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"output_index\":1,\"content_index\":0,\"delta\":\"answer\"}\n\n")
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"usage\":{\"input_tokens\":12,\"output_tokens\":8,\"total_tokens\":20,\"input_tokens_details\":{\"cached_tokens\":5},\"output_tokens_details\":{\"reasoning_tokens\":3}}}}\n\n")
	}))
	defer endpoint.Close()
	model := types.Model{ID: "reasoner", API: "openai-responses", Provider: "openai", BaseURL: endpoint.URL + "/v1", MaxTokens: 100, Reasoning: true, Input: []string{"text", "image"}}
	history := types.Context{SystemPrompt: "system", Messages: []types.Message{types.UserMessage{Content: types.BlockContent(types.NewText("question"), types.NewImage("aW1hZ2U=", "image/png"))}}, Tools: []types.Tool{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}}}
	opts := &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{APIKey: "key", SessionID: "session", Query: map[string]string{"route": "fixture"}}, Reasoning: "high"}
	result, err := StreamSimple(context.Background(), model, history, opts).Result(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != types.StopStop || len(result.Content) != 2 || result.Content[0].ThinkingSignature != reasoning || result.Usage.Reasoning == nil || *result.Usage.Reasoning != 3 || result.Usage.Input != 7 {
		t.Fatalf("result: %+v", result)
	}
	first := <-requests
	if string(first["store"]) != "false" || string(first["prompt_cache_key"]) != `"session"` || !strings.Contains(string(first["include"]), "reasoning.encrypted_content") || !strings.Contains(string(first["input"]), "input_image") || !strings.Contains(string(first["tools"]), `"type":"function"`) {
		t.Fatalf("wire: %s", first)
	}
	history.Messages = append(history.Messages, *result, types.UserMessage{Content: types.StringContent("next")})
	if _, err := StreamSimple(context.Background(), model, history, opts).Result(context.Background()); err != nil {
		t.Fatal(err)
	}
	next := <-requests
	if !strings.Contains(string(next["input"]), `"encrypted_content":"ciphertext"`) {
		t.Fatalf("reasoning lost: %s", next["input"])
	}
	model.ID = "other"
	if strings.Contains(mustJSON(t, params(model, history, opts)), "ciphertext") {
		t.Fatal("encrypted reasoning crossed models")
	}
}
func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestTruncatedResponse(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
	}))
	defer endpoint.Close()
	model := types.Model{ID: "test", BaseURL: endpoint.URL}
	result, err := StreamSimple(context.Background(), model, types.Context{}, &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{NoAuth: true}}).Result(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.ErrorCode != "stream_interrupted" || len(result.Content) != 1 {
		t.Fatalf("truncated response: %+v", result)
	}
}

func TestInStreamErrorCodes(t *testing.T) {
	for _, test := range []struct{ Code, Want string }{
		{"rate_limit_exceeded", "rate_limited"},
		{"server_error", "server_failure"},
		{"invalid_api_key", "authentication"},
	} {
		t.Run(test.Code, func(t *testing.T) {
			endpoint := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				encoded, err := json.Marshal(map[string]string{"type": "error", "code": test.Code, "message": "request failed"})
				if err != nil {
					t.Error(err)
					return
				}
				io.WriteString(writer, "data: "+string(encoded)+"\n\n")
			}))
			defer endpoint.Close()
			result, err := StreamSimple(context.Background(), types.Model{ID: "test", BaseURL: endpoint.URL}, types.Context{}, &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{NoAuth: true}}).Result(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if result.ErrorCode != test.Want {
				t.Fatalf("error code: %s, want %s", result.ErrorCode, test.Want)
			}
		})
	}
}

func TestResponsesVendorCapabilities(t *testing.T) {
	model := types.Model{ID: "vendor-model", API: "openai-responses", Provider: "vendor", OpenAICompat: &types.OpenAICompat{SupportsPromptCacheKey: new(false), SupportsEncryptedReasoning: new(false)}}
	history := types.Context{Messages: []types.Message{types.AssistantMessage{API: model.API, Provider: model.Provider, Model: model.ID, Content: []types.ContentBlock{types.NewThinking("plan", `{"type":"reasoning","encrypted_content":"ciphertext"}`), types.NewText("answer")}}}}
	payload := params(model, history, &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{SessionID: "session"}})
	if payload["include"] != nil || payload["prompt_cache_key"] != nil {
		t.Fatalf("unsupported OpenAI fields: %+v", payload)
	}
	if strings.Contains(mustJSON(t, payload), "ciphertext") {
		t.Fatal("encrypted reasoning replayed to unsupported vendor")
	}
}

func TestStatelessReasoningAndCacheKey(t *testing.T) {
	model := types.Model{ID: "reasoner", API: "openai-responses", Provider: "openai", Reasoning: true}
	history := types.Context{Messages: []types.Message{types.AssistantMessage{API: model.API, Provider: model.Provider, Model: model.ID, Content: []types.ContentBlock{
		types.NewThinking("not reusable", `{"type":"reasoning","id":"rs_missing","summary":[]}`),
		types.NewThinking("null", `{"type":"reasoning","encrypted_content":null}`),
		types.NewThinking("empty", `{"type":"reasoning","encrypted_content":""}`),
		types.NewThinking("reusable", `{"type":"reasoning","encrypted_content":"ciphertext"}`),
	}}}}
	opts := &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{SessionID: strings.Repeat("session", 20)}}
	first := params(model, history, opts)
	key := first["prompt_cache_key"].(string)
	if len(key) != 64 || key != params(model, history, opts)["prompt_cache_key"] {
		t.Fatalf("invalid or unstable cache key: %q", key)
	}
	body := mustJSON(t, first)
	if strings.Contains(body, "rs_missing") || strings.Count(body, `"encrypted_content"`) != 1 || !strings.Contains(body, "ciphertext") {
		t.Fatalf("unusable reasoning replayed: %s", body)
	}
	model.Reasoning = false
	if p := params(model, history, opts); p["include"] != nil || strings.Contains(mustJSON(t, p), "ciphertext") {
		t.Fatalf("reasoning sent to non-reasoning model: %+v", p)
	}
}

func TestResponseFailedFallbackAndType(t *testing.T) {
	for _, test := range []struct{ Error, Message, Code string }{
		{`null`, "response failed", "provider_failure"},
		{`{}`, "response failed", "provider_failure"},
		{`{"code":"unrecognized_code","type":"server_error","message":"upstream failed"}`, "upstream failed", "server_failure"},
		{`{"code":"unrecognized_code","type":"invalid_request_error","message":"rate limit setting is invalid"}`, "rate limit setting is invalid", "invalid_request"},
	} {
		t.Run(test.Error, func(t *testing.T) {
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, "data: {\"type\":\"response.failed\",\"response\":{\"error\":"+test.Error+"}}\n\n")
			}))
			defer endpoint.Close()
			got, err := StreamSimple(context.Background(), types.Model{ID: "test", BaseURL: endpoint.URL}, types.Context{}, &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{NoAuth: true}}).Result(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got.ErrorMessage != test.Message || got.ErrorCode != test.Code {
				t.Fatalf("failure: %+v", got)
			}
		})
	}
}
