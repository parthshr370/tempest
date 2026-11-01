package retry

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
)

func TestAvailabilityTriggersOnlyOnUnavailableModels(t *testing.T) {
	now := time.Now()
	exhausted := func(status int, cause string) (*types.AssistantMessage, int) {
		classified := Classify(Failure{Status: status, Message: cause}, now)
		return &types.AssistantMessage{StopReason: types.StopError, ErrorCode: string(CodeAttemptsExhausted), ErrorMessage: "retry attempts exhausted after 3 attempts: " + classified.Error()}, status
	}
	provider := func(status int, body string) (*types.AssistantMessage, int) {
		classified := Classify(Failure{Status: status, Message: "provider HTTP: " + body}, now)
		return &types.AssistantMessage{StopReason: types.StopError, ErrorCode: string(classified.Code), ErrorMessage: classified.Error()}, status
	}
	for _, tc := range []struct {
		name      string
		message   func() (*types.AssistantMessage, int)
		ctxErr    error
		available bool
		reason    Code
	}{
		{"503 after retries", func() (*types.AssistantMessage, int) { return exhausted(503, "overloaded") }, nil, true, CodeServerFailure},
		{"529 after retries", func() (*types.AssistantMessage, int) { return exhausted(529, "overloaded") }, nil, true, CodeServerFailure},
		{"429 after retries", func() (*types.AssistantMessage, int) { return exhausted(429, "slow down") }, nil, true, CodeRateLimited},
		{"network error after retries", func() (*types.AssistantMessage, int) { return exhausted(0, "dial tcp: connection refused") }, nil, true, CodeNetworkFailure},
		{"no first event before the watchdog", func() (*types.AssistantMessage, int) { return exhausted(200, "stream idle for 1m30s (no events)") }, nil, true, CodeStreamInterrupted},
		{"retry delay over the cap", func() (*types.AssistantMessage, int) {
			return &types.AssistantMessage{StopReason: types.StopError, ErrorCode: string(CodeRetryDelayExceeded), ErrorMessage: "retry delay 1h exceeds the configured maximum 5m: too many requests"}, 429
		}, nil, true, CodeRateLimited},
		{"503 without a retry policy", func() (*types.AssistantMessage, int) { return provider(503, "unavailable") }, nil, true, CodeServerFailure},
		{"model_not_found on 404", func() (*types.AssistantMessage, int) {
			return provider(404, `{"error":{"code":"model_not_found","message":"The model does not exist"}}`)
		}, nil, true, CodeModelNotFound},
		{"model_not_found on 400", func() (*types.AssistantMessage, int) {
			return provider(400, `{"error":{"code":"model_not_found","message":"The requested model does not exist"}}`)
		}, nil, true, CodeModelNotFound},
		{"unknown model without a code", func() (*types.AssistantMessage, int) {
			return provider(400, `{"error":{"message":"The model 'gpt-nope' does not exist or you do not have access to it."}}`)
		}, nil, true, CodeModelNotFound},
		{"anthropic unknown model", func() (*types.AssistantMessage, int) {
			return provider(404, `{"type":"error","error":{"type":"not_found_error","message":"model: claude-nope"}}`)
		}, nil, true, CodeModelNotFound},
		{"400 rejected request", func() (*types.AssistantMessage, int) {
			return provider(400, `{"error":{"message":"tools are invalid"}}`)
		}, nil, false, CodeInvalidRequest},
		{"404 wrong endpoint", func() (*types.AssistantMessage, int) { return provider(404, "404 page not found") }, nil, false, CodeInvalidRequest},
		{"401", func() (*types.AssistantMessage, int) { return provider(401, `{"error":{"code":"model_not_found"}}`) }, nil, false, CodeAuthentication},
		{"403", func() (*types.AssistantMessage, int) { return provider(403, "forbidden") }, nil, false, CodeAuthentication},
		{"context overflow", func() (*types.AssistantMessage, int) {
			return provider(400, `{"error":{"code":"context_length_exceeded"}}`)
		}, nil, false, CodeContextOverflow},
		{"cancelled", func() (*types.AssistantMessage, int) { return exhausted(503, "overloaded") }, context.Canceled, false, CodeCanceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message, status := tc.message()
			reason, available := Availability(message, status, tc.ctxErr)
			if available != tc.available || reason != tc.reason {
				t.Fatalf("Availability = %s, %v; want %s, %v", reason, available, tc.reason, tc.available)
			}
		})
	}
}

// chainProvider answers per model: an error status fails the call, zero
// answers with the model's id.
type chainProvider struct {
	mu     sync.Mutex
	status map[string]int
	calls  []string
}

func (p *chainProvider) Stream(_ context.Context, model types.Model, _ types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream {
	p.mu.Lock()
	status := p.status[model.ID]
	p.calls = append(p.calls, model.ID)
	p.mu.Unlock()
	out := stream.NewAssistantStream(model.API, model.Provider, model.ID)
	if status != 0 {
		if opts != nil && opts.OnResponse != nil {
			_ = opts.OnResponse(types.ProviderResponse{Status: status}, model)
		}
		classified := Classify(Failure{Status: status, Message: "provider HTTP error"}, time.Now())
		out.Push(types.StreamEvent{Type: types.EvError, Reason: types.StopError, Err: &types.AssistantMessage{API: model.API, Provider: model.Provider, Model: model.ID, StopReason: types.StopError, ErrorCode: string(classified.Code), ErrorMessage: classified.Error()}})
	} else {
		out.Push(types.StreamEvent{Type: types.EvDone, Reason: types.StopStop, Message: &types.AssistantMessage{API: model.API, Provider: model.Provider, Model: model.ID, StopReason: types.StopStop, Content: []types.ContentBlock{types.NewText(model.ID)}}})
	}
	out.End()
	return out
}

func (p *chainProvider) called() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

func drainChain(t *testing.T, s *stream.AssistantStream) ([]types.StreamEvent, *types.AssistantMessage) {
	t.Helper()
	var events []types.StreamEvent
	for event := range s.Events() {
		events = append(events, event)
	}
	final, err := s.Result(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return events, final
}

func TestFallbackChainWalksUnavailableModelsInOrder(t *testing.T) {
	provider := &chainProvider{status: map[string]int{"first": 503, "second": 429}}
	first := types.Model{Provider: "a", API: "a-api", ID: "first"}
	second := types.Model{Provider: "b", API: "b-api", ID: "second"}
	third := types.Model{Provider: "c", API: "c-api", ID: "third"}
	chain, err := NewFallback([]Candidate{{first, provider}, {second, provider}, {third, provider}})
	if err != nil {
		t.Fatal(err)
	}
	events, final := drainChain(t, chain.Stream(context.Background(), first, types.Context{}, nil))
	if final.Model != "third" || final.StopReason != types.StopStop {
		t.Fatalf("final = %+v", final)
	}
	var switches []types.ModelFallback
	for _, event := range events {
		if event.Type == types.EvFallback {
			switches = append(switches, *event.Fallback)
		}
	}
	if len(switches) != 2 || switches[0].To.ID != "second" || switches[0].Reason != string(CodeServerFailure) || switches[1].From.ID != "second" || switches[1].To.ID != "third" || switches[1].Index != 2 || switches[1].Reason != string(CodeRateLimited) {
		t.Fatalf("switches = %+v, final = %+v", switches, final)
	}
}

func TestFallbackStartsAtTheModelItIsAskedFor(t *testing.T) {
	provider := &chainProvider{status: map[string]int{"primary": 503}}
	primary := types.Model{Provider: "a", API: "a-api", ID: "primary"}
	backup := types.Model{Provider: "b", API: "b-api", ID: "backup"}
	chain, err := NewFallback([]Candidate{{primary, provider}, {backup, provider}})
	if err != nil {
		t.Fatal(err)
	}
	_, final := drainChain(t, chain.Stream(context.Background(), backup, types.Context{}, nil))
	if final.Model != "backup" || len(provider.called()) != 1 {
		t.Fatalf("sticky backup request went to %v", provider.called())
	}
	unknown := types.Model{Provider: "a", API: "a-api", ID: "other"}
	_, final = drainChain(t, chain.Stream(context.Background(), unknown, types.Context{}, nil))
	if final.Model != "other" || provider.called()[1] != "other" {
		t.Fatalf("model outside the chain = %+v calls %v", final, provider.called())
	}
}

func TestFallbackEndsWithTheLastModelsFailure(t *testing.T) {
	provider := &chainProvider{status: map[string]int{"primary": 503, "backup": 529}}
	primary := types.Model{Provider: "a", API: "a-api", ID: "primary"}
	backup := types.Model{Provider: "b", API: "b-api", ID: "backup"}
	chain, err := NewFallback([]Candidate{{primary, provider}, {backup, provider}})
	if err != nil {
		t.Fatal(err)
	}
	_, final := drainChain(t, chain.Stream(context.Background(), primary, types.Context{}, nil))
	if final.StopReason != types.StopError || final.Model != "backup" || final.ErrorCode != string(CodeServerFailure) {
		t.Fatalf("final = %+v", final)
	}
	if _, err := NewFallback(nil); err == nil {
		t.Fatal("empty chain accepted")
	}
	if _, err := NewFallback([]Candidate{{Model: primary}}); err == nil {
		t.Fatal("nil stream accepted")
	}
}
