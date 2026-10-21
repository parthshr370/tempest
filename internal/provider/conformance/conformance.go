// Package conformance exercises provider contracts against local SSE replays.
package conformance

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/retry"
)

// Factory binds an adapter's public entry point to its wire dialect.
type Factory struct {
	Dialect string
	Stream  func(context.Context, types.Model, types.Context, *types.SimpleStreamOptions) *stream.AssistantStream
}

//go:embed testdata/*.sse
var fixtures embed.FS

// Run checks the same behavioral boundaries for every supported wire dialect.
func Run(t *testing.T, factory Factory) {
	t.Helper()
	for _, test := range []struct {
		name string
		run  func(*testing.T, Factory)
	}{
		{"text_and_cache_usage", textAndUsage},
		{"unknown_cost", unknownCost},
		{"tool_round_trip", toolRoundTrip},
		{"thinking_controls", thinkingControls},
		{"cancel_preserves_partial", cancellation},
		{"retry_classes", retryClasses},
	} {
		t.Run(test.name, func(t *testing.T) { test.run(t, factory) })
	}
}

func model(factory Factory, endpoint string) types.Model {
	return types.Model{ID: "fixture-model", Provider: factory.Dialect, API: factory.Dialect, BaseURL: endpoint,
		MaxTokens: 8192, ContextWindow: 200000, Cost: types.ModelCost{Input: 2, Output: 10, CacheRead: 0.5, CacheWrite: 3}}
}

func conversation() types.Context {
	return types.Context{Messages: []types.Message{types.UserMessage{Content: types.StringContent("hi")}}}
}

func options() *types.SimpleStreamOptions {
	return &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{APIKey: "fixture-key"}}
}

func fixture(t *testing.T, factory Factory, kind string) string {
	t.Helper()
	contents, err := fixtures.ReadFile("testdata/" + factory.Dialect + "_" + kind + ".sse")
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func server(t *testing.T, replay string, requests chan<- json.RawMessage) *httptest.Server {
	t.Helper()
	endpoint := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if requests != nil {
			requests <- body
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(writer, replay); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(endpoint.Close)
	return endpoint
}

func result(t *testing.T, factory Factory, routed types.Model, history types.Context, opts *types.SimpleStreamOptions) *types.AssistantMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	message, err := factory.Stream(ctx, routed, history, opts).Result(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func textAndUsage(t *testing.T, factory Factory) {
	endpoint := server(t, fixture(t, factory, "text"), nil)
	message := result(t, factory, model(factory, endpoint.URL), conversation(), options())
	if message.StopReason != types.StopStop || len(message.Content) != 1 || message.Content[0].Text != "hello" {
		t.Fatalf("text result: %+v", message)
	}
	usage := message.Usage
	if usage.Input != 10 || usage.Output != 4 || usage.CacheRead != 3 {
		t.Fatalf("usage: %+v", usage)
	}
	wantWrite := 2
	wantCost := (20.0 + 40 + 1.5 + 6) / 1e6
	if factory.Dialect == "anthropic" {
		wantCost = (20.0 + 40 + 1.5 + 3 + 4) / 1e6
	}
	if factory.Dialect == "google" {
		wantWrite = 0
		wantCost = (20.0 + 40 + 1.5) / 1e6
	}
	if usage.CacheWrite != wantWrite || math.Abs(usage.Cost.Total-wantCost) > 1e-12 {
		t.Fatalf("cache cost: %+v, want %g", usage, wantCost)
	}
}

func unknownCost(t *testing.T, factory Factory) {
	endpoint := server(t, fixture(t, factory, "text"), nil)
	routed := model(factory, endpoint.URL)
	routed.Cost.Unknown = true
	message := result(t, factory, routed, conversation(), options())
	if !message.Usage.Cost.Unknown || message.Usage.Cost.Total != 0 {
		t.Fatalf("cost: %+v", message.Usage.Cost)
	}
	encoded, err := json.Marshal(message.Usage.Cost)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"unknown":true}` {
		t.Fatalf("unknown price advertised as free: %s", encoded)
	}
}

func toolRoundTrip(t *testing.T, factory Factory) {
	requests := make(chan json.RawMessage, 2)
	endpoint := server(t, fixture(t, factory, "tool"), requests)
	routed := model(factory, endpoint.URL)
	history := conversation()
	history.Tools = []types.Tool{{Name: "lookup", Description: "Read a file", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)}}
	message := result(t, factory, routed, history, options())
	<-requests
	if message.StopReason != types.StopToolUse || len(message.Content) != 1 {
		t.Fatalf("tool result: %+v", message)
	}
	call := message.Content[0]
	var arguments struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(call.Arguments, &arguments); err != nil || arguments.Path != "file.txt" || call.Name != "lookup" || call.ID == "" {
		t.Fatalf("invalid reconstructed call: %+v, %v", call, err)
	}
	history.Messages = append(history.Messages, *message, types.ToolResultMessage{ToolCallID: call.ID, ToolName: call.Name, Content: []types.ContentBlock{types.NewText("file contents")}})
	result(t, factory, routed, history, options())
	body := <-requests
	assertToolResult(t, factory.Dialect, body, call.ID)
}

func assertToolResult(t *testing.T, dialect string, body json.RawMessage, callID string) {
	t.Helper()
	var request struct {
		Messages []struct {
			Role       string          `json:"role"`
			ToolCallID string          `json:"tool_call_id"`
			Content    json.RawMessage `json:"content"`
		} `json:"messages"`
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				FunctionResponse *struct {
					ID       string `json:"id"`
					Name     string `json:"name"`
					Response struct {
						Output string `json:"output"`
					} `json:"response"`
				} `json:"functionResponse"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	switch dialect {
	case "openai":
		last := request.Messages[len(request.Messages)-1]
		var text string
		if err := json.Unmarshal(last.Content, &text); err != nil {
			t.Fatal(err)
		}
		if last.Role != "tool" || last.ToolCallID != callID || text != "file contents" {
			t.Fatalf("tool wire: %s", body)
		}
	case "anthropic":
		last := request.Messages[len(request.Messages)-1]
		var blocks []struct {
			Type      string          `json:"type"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(last.Content, &blocks); err != nil {
			t.Fatal(err)
		}
		if last.Role != "user" || len(blocks) == 0 || blocks[0].Type != "tool_result" || blocks[0].ToolUseID != callID || !strings.Contains(string(blocks[0].Content), "file contents") {
			t.Fatalf("tool wire: %s", body)
		}
	case "google":
		last := request.Contents[len(request.Contents)-1]
		if len(last.Parts) == 0 || last.Parts[0].FunctionResponse == nil {
			t.Fatalf("tool wire: %s", body)
		}
		response := last.Parts[0].FunctionResponse
		if last.Role != "user" || response.Name != "lookup" || response.Response.Output != "file contents" {
			t.Fatalf("tool wire: %s", body)
		}
	}
}

func thinkingControls(t *testing.T, factory Factory) {
	for _, mode := range []string{"unsupported", "enabled", "disabled", "adaptive", "none-effort"} {
		t.Run(mode, func(t *testing.T) {
			requests := make(chan json.RawMessage, 1)
			endpoint := server(t, fixture(t, factory, "text"), requests)
			routed := model(factory, endpoint.URL)
			routed.Reasoning = mode != "unsupported"
			opts := options()
			opts.Reasoning = "high"
			if mode == "disabled" || mode == "none-effort" {
				opts.Reasoning = ""
			}
			if mode == "adaptive" {
				adaptive := true
				routed.Compat = &types.AnthropicCompat{ForceAdaptiveThinking: &adaptive}
			}
			if mode == "none-effort" {
				routed.OpenAICompat = &types.OpenAICompat{ReasoningDisableMode: types.ReasoningDisableNoneEffort}
			}
			message := result(t, factory, routed, conversation(), opts)
			if message.StopReason == types.StopError {
				t.Fatalf("thinking failed: %+v", message)
			}
			body := <-requests
			var request struct {
				Thinking *struct {
					Type   string `json:"type"`
					Budget int    `json:"budget_tokens"`
				} `json:"thinking"`
				OutputConfig struct {
					Effort string `json:"effort"`
				} `json:"output_config"`
				Effort     string `json:"reasoning_effort"`
				Generation struct {
					Thinking *struct {
						Level  string `json:"thinkingLevel"`
						Budget *int   `json:"thinkingBudget"`
					} `json:"thinkingConfig"`
				} `json:"generationConfig"`
				MisplacedThinking json.RawMessage `json:"thinkingConfig"`
			}
			if err := json.Unmarshal(body, &request); err != nil {
				t.Fatal(err)
			}
			switch factory.Dialect {
			case "anthropic":
				if mode == "unsupported" {
					if request.Thinking != nil {
						t.Fatalf("unsupported thinking: %s", body)
					}
					return
				}
				if request.Thinking == nil {
					t.Fatalf("missing thinking: %s", body)
				}
				if mode == "adaptive" {
					if request.Thinking.Type != "adaptive" || request.OutputConfig.Effort != "high" {
						t.Fatalf("adaptive: %s", body)
					}
				} else if opts.Reasoning != "" {
					if request.Thinking.Type != "enabled" || request.Thinking.Budget < 1024 {
						t.Fatalf("budget: %s", body)
					}
				} else if request.Thinking.Type != "disabled" {
					t.Fatalf("disable: %s", body)
				}
			case "openai":
				want := opts.Reasoning
				if mode == "unsupported" {
					want = ""
				}
				if mode == "none-effort" {
					want = "none"
				}
				if request.Effort != want {
					t.Fatalf("effort want %q: %s", want, body)
				}
			case "google":
				if len(request.MisplacedThinking) > 0 {
					t.Fatalf("thinkingConfig outside generationConfig: %s", body)
				}
				if mode == "unsupported" {
					if request.Generation.Thinking != nil {
						t.Fatalf("unsupported thinking: %s", body)
					}
					return
				}
				if request.Generation.Thinking == nil {
					t.Fatalf("missing thinking: %s", body)
				}
				if opts.Reasoning != "" {
					if request.Generation.Thinking.Level != "high" {
						t.Fatalf("thinking level: %s", body)
					}
				} else if request.Generation.Thinking.Budget == nil || *request.Generation.Thinking.Budget != 0 {
					t.Fatalf("disable: %s", body)
				}
			}
		})
	}
}

func cancellation(t *testing.T, factory Factory) {
	replay := fixture(t, factory, "text")
	// Send only enough complete frames to expose text, then hold the body open.
	cut := strings.Index(replay, "hello")
	cut += strings.Index(replay[cut:], "\n\n") + 2
	closed := make(chan struct{})
	endpoint := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(writer, replay[:cut]); err != nil {
			t.Error(err)
			return
		}
		writer.(http.Flusher).Flush()
		<-request.Context().Done()
		close(closed)
	}))
	defer endpoint.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	streamed := factory.Stream(ctx, model(factory, endpoint.URL), conversation(), options())
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		select {
		case event, ok := <-streamed.Events():
			if !ok {
				t.Fatal("stream closed without terminal event")
			}
			if event.Type == types.EvTextDelta {
				cancel()
			}
			if event.Type != types.EvError {
				continue
			}
			if event.Err == nil || event.Err.StopReason != types.StopAborted || event.Err.ErrorCode != string(retry.CodeCanceled) || len(event.Err.Content) == 0 || event.Err.Content[0].Text != "hello" {
				t.Fatalf("cancel result: %+v", event.Err)
			}
			select {
			case <-closed:
				return
			case <-deadline.C:
				t.Fatal("cancellation didn't close HTTP body within 1s")
			}
		case <-deadline.C:
			t.Fatal("cancellation didn't terminate stream within 1s")
		}
	}
}

func retryClasses(t *testing.T, factory Factory) {
	for _, status := range []int{429, 529, 500, 503, 200} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			endpoint := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "text/event-stream")
				writer.WriteHeader(status)
				body := `{"error":{"type":"overloaded_error","message":"overloaded"}}`
				if status == 200 {
					if factory.Dialect == "anthropic" {
						body = "event: error\ndata: " + body + "\n\n"
					} else {
						body = "data: " + body + "\n\n"
					}
				}
				if _, err := io.WriteString(writer, body); err != nil {
					t.Error(err)
				}
			}))
			defer endpoint.Close()
			message := result(t, factory, model(factory, endpoint.URL), conversation(), options())
			want := retry.CodeServerFailure
			if status == 429 {
				want = retry.CodeRateLimited
			}
			if message.StopReason != types.StopError || message.ErrorCode != string(want) {
				t.Fatalf("status %d: %+v, want %s", status, message, want)
			}
		})
	}
}
