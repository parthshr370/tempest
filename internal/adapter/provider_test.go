package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/retry"
)

type adapterRetryClock struct{ now time.Time }

func (c adapterRetryClock) Now() time.Time { return c.now }

type adapterRetrySleeper struct {
	mu     sync.Mutex
	delays []time.Duration
}

func (s *adapterRetrySleeper) Sleep(_ context.Context, delay time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delays = append(s.delays, delay)
	return nil
}
func (s *adapterRetrySleeper) Delays() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.delays...)
}

type adapterRetryRandom float64

func (r adapterRetryRandom) Float64() float64 { return float64(r) }

func TestProviderRouterRetriesRateLimitedResponse(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempt := requests.Add(1)
		if attempt == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"limited"}`))
			return
		}
		writeMinimalAnthropicStream(w)
	}))
	defer server.Close()

	sleeper := &adapterRetrySleeper{}
	policy := retry.DefaultConfig()
	policy.Clock = adapterRetryClock{now: time.Unix(1_700_000_000, 0)}
	policy.Sleeper = sleeper
	policy.Random = adapterRetryRandom(1)
	router := mustRouter(t, RoutingConfig{ModelID: "claude-test", BaseURL: server.URL, APIKey: "test", Retry: &policy, Env: func(string) string { return "" }})
	final := drain(t, router.StreamFn(context.Background(), router.Model, ptypes.Context{Messages: []ptypes.Message{ptypes.UserMessage{Content: ptypes.StringContent("hello")}}}, nil))
	if requests.Load() != 2 || final.StopReason != ptypes.StopStop {
		t.Fatalf("requests=%d final=%+v", requests.Load(), final)
	}
	if delays := sleeper.Delays(); len(delays) != 1 || delays[0] != time.Second {
		t.Fatalf("delays=%v", delays)
	}
}
func TestProviderRouterDoesNotRetryBrokenStreamAfterText(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":1}}}\n\n")
		_, _ = fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"visible\"}}\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		// Close without message_stop: the real provider emits a terminal stream
		// failure after the observable text event.
	}))
	defer server.Close()

	policy := retry.DefaultConfig()
	policy.Clock = adapterRetryClock{now: time.Unix(1_700_000_000, 0)}
	policy.Sleeper = &adapterRetrySleeper{}
	policy.Random = adapterRetryRandom(1)
	router := mustRouter(t, RoutingConfig{ModelID: "claude-test", BaseURL: server.URL, APIKey: "test", Retry: &policy, Env: func(string) string { return "" }})
	var events []ptypes.StreamEvent
	for event := range router.StreamFn(context.Background(), router.Model, ptypes.Context{Messages: []ptypes.Message{ptypes.UserMessage{Content: ptypes.StringContent("hello")}}}, nil).Events() {
		events = append(events, event)
	}
	if requests.Load() != 1 {
		t.Fatalf("second real-seam integration retried observable output: requests=%d events=%+v", requests.Load(), events)
	}
	var sawText, sawTerminal bool
	for _, event := range events {
		sawText = sawText || (event.Type == ptypes.EvTextDelta && event.Delta == "visible")
		sawTerminal = sawTerminal || event.Type == ptypes.EvError
	}
	if !sawText || !sawTerminal {
		t.Fatalf("broken observable stream events=%+v", events)
	}
}

func TestProviderRouterResolvesProxyConfigFromEnv(t *testing.T) {
	env := map[string]string{
		"ANTHROPIC_BASE_URL":             "http://localhost:8891",
		"ANTHROPIC_AUTH_TOKEN":           "sk-or-test",
		"ANTHROPIC_DEFAULT_SONNET_MODEL": "anthropic/claude-opus-4-8",
	}
	router := mustRouter(t, RoutingConfig{Env: func(key string) string { return env[key] }})
	if router.Model.BaseURL != "http://localhost:8891" {
		t.Fatalf("base URL = %q", router.Model.BaseURL)
	}
	if router.Model.ID != "anthropic/claude-opus-4-8" || router.Model.API != AnthropicMessagesAPI || router.Model.Provider != "anthropic" {
		t.Fatalf("model = %+v", router.Model)
	}
	if p, ok := router.Registry.Resolve(AnthropicMessagesAPI); !ok || p.ID != "anthropic" || p.StreamSimple == nil {
		t.Fatalf("registry resolve = %+v ok=%v", p, ok)
	}
}

func TestResolveProviderForModelRoutesThroughAnthropicProxy(t *testing.T) {
	for _, model := range []string{"claude-sonnet-4", "anthropic/claude-opus-4-8", "openai/gpt-4o", "google/gemini-2.5-pro"} {
		if got := ResolveProviderForModel(model); got != "anthropic" {
			t.Fatalf("ResolveProviderForModel(%q) = %q", model, got)
		}
	}
}

func TestProviderRouterMaxTokensDefaultAndOverride(t *testing.T) {
	router := mustRouter(t, RoutingConfig{Env: func(string) string { return "" }})
	// The default model id hits the catalog, so the catalog output cap (128000
	// for claude-opus-4-8) is the effective default, not defaultMaxTokens.
	if router.Model.MaxTokens != 128000 {
		t.Fatalf("catalog default MaxTokens = %d, want 128000", router.Model.MaxTokens)
	}
	env := map[string]string{"HARNESS_MAX_TOKENS": "12000"}
	override := mustRouter(t, RoutingConfig{Env: func(k string) string { return env[k] }})
	if override.Model.MaxTokens != 12000 {
		t.Fatalf("override MaxTokens = %d, want 12000", override.Model.MaxTokens)
	}
	// invalid/zero override falls back to the catalog default for the
	// default model id, same as the no-override case above.
	bad := map[string]string{"HARNESS_MAX_TOKENS": "0"}
	fallback := mustRouter(t, RoutingConfig{Env: func(k string) string { return bad[k] }})
	if fallback.Model.MaxTokens != 128000 {
		t.Fatalf("bad-override MaxTokens = %d, want 128000", fallback.Model.MaxTokens)
	}
}

func TestProviderRouterBaseURLAndAuthTokenAreHonored(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writeMinimalAnthropicStream(w)
	}))
	defer server.Close()

	router := mustRouter(t, RoutingConfig{ModelID: "anthropic/claude-opus-4-8", BaseURL: server.URL, AuthToken: "sk-or-proxy", Env: func(string) string { return "" }})
	stream := router.StreamFn(context.Background(), router.Model, ptypes.Context{Messages: []ptypes.Message{ptypes.UserMessage{Content: ptypes.StringContent("hello")}}}, nil)
	final := drain(t, stream)

	if gotPath != "/v1/messages" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer sk-or-proxy" {
		t.Fatalf("authorization = %q", gotAuth)
	}
	if gotBody["model"] != "anthropic/claude-opus-4-8" {
		t.Fatalf("body = %#v", gotBody)
	}
	if final.StopReason != ptypes.StopStop {
		t.Fatalf("final = %+v", final)
	}
}

func TestProviderRouterThreadsBlobReader(t *testing.T) {
	tests := []struct {
		name         string
		configReader bool
	}{
		{name: "routing config", configReader: true},
		{name: "incoming options"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var gotBody map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
					t.Errorf("decode request: %v", err)
				}
				writeMinimalAnthropicStream(w)
			}))
			defer server.Close()

			reader := &adapterStubBlobReader{data: []byte("pdf-bytes")}
			cfg := RoutingConfig{
				ModelID: "claude-test",
				BaseURL: server.URL,
				APIKey:  "sk-test",
				Env:     func(string) string { return "" },
			}
			var opts *ptypes.SimpleStreamOptions
			if test.configReader {
				cfg.BlobReader = reader
			} else {
				opts = &ptypes.SimpleStreamOptions{BlobReader: reader}
			}
			router := mustRouter(t, cfg)
			ref := ptypes.NewDocumentRef("session-local", "document-key", "application/pdf", "report.pdf", int64(len(reader.data)), 1)
			final := drain(t, router.StreamFn(
				context.Background(),
				router.Model,
				ptypes.Context{Messages: []ptypes.Message{ptypes.UserMessage{Content: ptypes.BlockContent(ref)}}},
				opts,
			))
			if final.StopReason != ptypes.StopStop {
				t.Fatalf("final = %+v", final)
			}

			messages := gotBody["messages"].([]any)
			content := messages[0].(map[string]any)["content"].([]any)
			block := content[0].(map[string]any)
			source := block["source"].(map[string]any)
			if block["type"] != "document" || source["type"] != "base64" || source["media_type"] != "application/pdf" {
				t.Fatalf("document block = %#v", block)
			}
		})
	}
}

type adapterStubBlobReader struct {
	data []byte
}

func (r *adapterStubBlobReader) StatBlob(context.Context, string, string) (int64, error) {
	return int64(len(r.data)), nil
}

func (r *adapterStubBlobReader) OpenBlob(context.Context, string, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(r.data)), nil
}

func writeMinimalAnthropicStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":1}}}\n\n"))
	_, _ = w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n"))
	_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
}

func drain(t *testing.T, s interface {
	Events() <-chan ptypes.StreamEvent
	Final() *ptypes.AssistantMessage
}) *ptypes.AssistantMessage {
	t.Helper()
	for range s.Events() {
	}
	return s.Final()
}

func mustRouter(t *testing.T, cfg RoutingConfig) ProviderRouter {
	t.Helper()
	r, err := NewProviderRouter(cfg)
	if err != nil {
		t.Fatalf("NewProviderRouter: %v", err)
	}
	return r
}

func writeMinimalOpenAIStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, ev := range []string{
		`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"delta":{"content":"ok"}}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		"data: [DONE]",
	} {
		_, _ = fmt.Fprint(w, ev+"\n\n")
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// An unknown provider is a deterministic configuration error, never a silent
// Anthropic fallback.
func TestProviderRouterUnsupportedProviderErrors(t *testing.T) {
	if _, err := NewProviderRouter(RoutingConfig{ModelID: "cohere:command-r", Env: func(string) string { return "" }}); err == nil {
		t.Fatal("expected error for unsupported provider")
	}
}

// A generic openai: model keeps its own endpoint and OPENAI_API_KEY.
func TestProviderRouterOpenAIRouteUsesOwnCredentials(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("authorization")
		writeMinimalOpenAIStream(w)
	}))
	defer server.Close()

	env := map[string]string{"OPENAI_API_KEY": "sk-openai"}
	router := mustRouter(t, RoutingConfig{ModelID: "openai:gpt-4o@" + server.URL, Env: func(k string) string { return env[k] }})
	if router.Model.Provider != "openai" || router.Model.API != OpenAICompletionsAPI {
		t.Fatalf("model = %+v", router.Model)
	}
	drain(t, router.StreamFn(context.Background(), router.Model, ptypes.Context{Messages: []ptypes.Message{ptypes.UserMessage{Content: ptypes.StringContent("hi")}}}, nil))
	if gotAuth != "Bearer sk-openai" {
		t.Fatalf("authorization = %q", gotAuth)
	}
}
