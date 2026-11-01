package telemetry

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/base"
	"go.harness.dev/harness/internal/retry"
)

// missingModelProvider fails every call the way a provider's HTTP error path
// reports a model that does not exist: a 404 through OnResponse and OpenAI's
// model_not_found body in the error.
type missingModelProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *missingModelProvider) Stream(_ context.Context, model types.Model, _ types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	if opts != nil && opts.OnResponse != nil {
		_ = opts.OnResponse(types.ProviderResponse{Status: http.StatusNotFound}, model)
	}
	body := `{"error":{"message":"The model ` + "`" + model.ID + "`" + ` does not exist or you do not have access to it.","type":"invalid_request_error","param":null,"code":"model_not_found"}}`
	message := types.AssistantMessage{API: model.API, Provider: model.Provider, Model: model.ID, Content: []types.ContentBlock{}, StopReason: types.StopError}
	base.ApplyError(&message, base.ResponseError(&http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}}, []byte(body)))
	events := stream.NewAssistantStream(model.API, model.Provider, model.ID)
	events.Push(types.StreamEvent{Type: types.EvError, Reason: types.StopError, Err: &message})
	events.End()
	return events
}

func (p *missingModelProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

var (
	fallbackPrimary = types.Model{API: "openai-responses", Provider: "openai", ID: "gpt-nope-0", BaseURL: "https://api.openai.com/v1"}
	fallbackBackup  = types.Model{API: "anthropic-messages", Provider: "anthropic", ID: "claude-backup", BaseURL: "https://backup.example:8443"}
)

// attributeOf returns one metric series attribute, or "" when it is absent.
func attributeOf(entry series, key string) string {
	for _, attr := range entry.attributes {
		if attr.key == key {
			return attr.text
		}
	}
	return ""
}

// A run whose primary model is missing or keeps failing switches to its
// backup. The failed primary attempts are child spans of the request, the
// switch is an event on it, and the request itself, every later attempt, and
// their token and duration metrics belong to the backup model.
func TestFallbackAttributesTheRequestToTheBackup(t *testing.T) {
	for _, tc := range []struct {
		name string
		// primary serves the primary model behind its retry policy.
		primary func(t *testing.T) (agent.StreamFn, func() int)
		// outcomes are the failed primary attempts' outcomes, in order.
		outcomes []string
		reason   string
	}{
		{
			name: "404 model_not_found before any output",
			primary: func(t *testing.T) (agent.StreamFn, func() int) {
				provider := &missingModelProvider{}
				retrier, err := retry.New(provider, retry.DefaultConfig())
				if err != nil {
					t.Fatal(err)
				}
				return retrier.Stream, provider.callCount
			},
			outcomes: []string{"fallback"},
			reason:   string(retry.CodeModelNotFound),
		},
		{
			name: "connection reset after partial output on every attempt",
			primary: func(t *testing.T) (agent.StreamFn, func() int) {
				provider := &scriptedProvider{attempts: []func(*stream.AssistantStream){partialFailedAttempt, partialFailedAttempt, partialFailedAttempt}}
				return retrying(t, provider), func() int {
					provider.mu.Lock()
					defer provider.mu.Unlock()
					return provider.calls
				}
			},
			outcomes: []string{"retry", "retry", "fallback"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCollector(t)
			tel := newTestTelemetry(t, c, false)
			primaryStream, primaryCalls := tc.primary(t)
			backup := &scriptedProvider{attempts: []func(*stream.AssistantStream){toolCallAttempt("echo", `{}`), textAttempt("done")}}
			chain, err := retry.NewFallback([]retry.Candidate{
				{Model: fallbackPrimary, Stream: streamer(primaryStream)},
				{Model: fallbackBackup, Stream: streamer(retrying(t, backup))},
			})
			if err != nil {
				t.Fatal(err)
			}
			agt := agent.NewAgent(agent.AgentOptions{
				InitialState:   &agent.AgentState{Model: fallbackPrimary, Tools: []agent.AgentTool{echoTool(nil)}},
				StreamFn:       chain.Stream,
				FallbackModels: []types.Model{fallbackBackup},
				Trace:          tel.Tracing(),
			})
			detach := tel.Attach(agt, AgentInfo{SessionID: "session-1"})
			defer detach()
			result, err := agt.Prompt(context.Background(), types.UserMessage{Content: types.StringContent("say done")})
			if err != nil || result.StopReason != agent.StopCompleted {
				t.Fatalf("run = %+v, %v", result, err)
			}
			backup.mu.Lock()
			backupCalls := backup.calls
			backup.mu.Unlock()
			if primaryCalls() != len(tc.outcomes) || backupCalls != 2 {
				t.Fatalf("calls: primary %d, backup %d", primaryCalls(), backupCalls)
			}
			if err := tel.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}

			byName := spansByName(collectedSpans(t, c))
			run := byName["invoke_agent tempest"]
			requests := byName["chat claude-backup"]
			attempts := byName["chat gpt-nope-0"]
			if len(run) != 1 || len(requests) != 2 || len(attempts) != len(tc.outcomes) || len(byName["execute_tool echo"]) != 1 {
				t.Fatalf("span names = %v", c.spanNames(t))
			}
			var switched receivedSpan
			for _, request := range requests {
				if request.ParentSpanID != run[0].SpanID || request.Status.Code != 0 {
					t.Fatalf("request span = %+v", request)
				}
				if request.attr("gen_ai.provider.name") != "anthropic" || request.attr("gen_ai.request.model") != "claude-backup" ||
					request.attr("server.address") != "backup.example" || request.attr("server.port") != "8443" {
					t.Fatalf("request span is not attributed to the backup: %+v", request.Attributes)
				}
				if len(request.Events) > 0 {
					switched = request
				}
			}
			if len(switched.Events) != 1 {
				t.Fatalf("fallback events on the requests: %+v", requests)
			}
			event := switched.Events[0]
			if event.Name != "tempest.model.fallback" || event.TimeUnixNano == "" ||
				event.attr("tempest.fallback.from_provider") != "openai" || event.attr("tempest.fallback.from_model") != "gpt-nope-0" ||
				event.attr("tempest.fallback.to_provider") != "anthropic" || event.attr("tempest.fallback.to_model") != "claude-backup" ||
				event.attr("tempest.fallback.reason") == "" || event.attr("tempest.fallback.index") != "1" ||
				event.attr("tempest.fallback.attempts") != strconv.Itoa(len(tc.outcomes)) {
				t.Fatalf("fallback event = %+v", event)
			}
			if tc.reason != "" && event.attr("tempest.fallback.reason") != tc.reason {
				t.Fatalf("fallback reason = %q, want %q", event.attr("tempest.fallback.reason"), tc.reason)
			}
			seen := map[string]int{}
			for _, failed := range attempts {
				if failed.ParentSpanID != switched.SpanID || failed.Status.Code != int(statusError) ||
					failed.attr("gen_ai.provider.name") != "openai" || failed.attr("gen_ai.request.model") != "gpt-nope-0" {
					t.Fatalf("failed attempt = %+v", failed)
				}
				seen[failed.attr("tempest.attempt.outcome")]++
			}
			want := map[string]int{}
			for _, outcome := range tc.outcomes {
				want[outcome]++
			}
			if len(seen) != len(want) || seen["retry"] != want["retry"] || seen["fallback"] != want["fallback"] {
				t.Fatalf("attempt outcomes = %v, want %v", seen, want)
			}
			if tc.reason != "" && attempts[0].attr("error.type") != tc.reason {
				t.Fatalf("failed attempt error.type = %q", attempts[0].attr("error.type"))
			}

			fallbacks := collectedSeries(t, tel, "tempest.model.fallbacks")
			if len(fallbacks) != 1 || fallbacks[0].value != 1 ||
				attributeOf(fallbacks[0], "tempest.fallback.from_model") != "gpt-nope-0" || attributeOf(fallbacks[0], "tempest.fallback.to_model") != "claude-backup" ||
				attributeOf(fallbacks[0], "tempest.fallback.reason") != event.attr("tempest.fallback.reason") {
				t.Fatalf("fallback counter = %+v", fallbacks)
			}
			failedAttempts, ok := seriesWith(collectedSeries(t, tel, "tempest.model.failed_attempts"), "tempest.attempt.outcome", "fallback")
			if !ok || failedAttempts.value != 1 || attributeOf(failedAttempts, "gen_ai.request.model") != "gpt-nope-0" {
				t.Fatalf("failed fallback attempts = %+v", failedAttempts)
			}
			// The backup answered both requests: their durations and their
			// tokens are the backup's. Only a discarded primary attempt's
			// own usage is the primary's.
			durations := collectedSeries(t, tel, "gen_ai.client.operation.duration")
			if len(durations) != 1 || durations[0].count != 2 || attributeOf(durations[0], "gen_ai.request.model") != "claude-backup" {
				t.Fatalf("durations = %+v", durations)
			}
			var backupTokens uint64
			for _, entry := range collectedSeries(t, tel, "gen_ai.client.token.usage") {
				if attributeOf(entry, "gen_ai.request.model") == "claude-backup" && attributeOf(entry, "gen_ai.token.type") == "input" {
					backupTokens += entry.count
				}
			}
			if backupTokens != 2 {
				t.Fatalf("backup input token points = %d, want 2", backupTokens)
			}
		})
	}
}

// streamer adapts an agent stream function to a fallback chain candidate.
type streamer agent.StreamFn

func (s streamer) Stream(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream {
	return s(ctx, model, c, opts)
}
