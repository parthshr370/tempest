package retry

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
)

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

type recordedSleeper struct {
	mu     sync.Mutex
	delays []time.Duration
}

func (s *recordedSleeper) Sleep(ctx context.Context, delay time.Duration) error {
	s.mu.Lock()
	s.delays = append(s.delays, delay)
	s.mu.Unlock()
	return nil
}

type fixedRandom float64

func (r fixedRandom) Float64() float64 { return float64(r) }

type fakeStreamer struct {
	mu      sync.Mutex
	calls   int
	streams []*stream.AssistantStream
	options []*types.SimpleStreamOptions
}

func (s *fakeStreamer) Stream(_ context.Context, _ types.Model, _ types.Context, options *types.SimpleStreamOptions) *stream.AssistantStream {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.options = append(s.options, options)
	return s.streams[s.calls-1]
}

func testModel() types.Model { return types.Model{API: "test", Provider: "test", ID: "test"} }
func testStream(events ...types.StreamEvent) *stream.AssistantStream {
	out := stream.NewAssistantStream("test", "test", "test")
	for _, event := range events {
		out.Push(event)
	}
	if len(events) == 0 || (events[len(events)-1].Type != types.EvDone && events[len(events)-1].Type != types.EvError) {
		out.End()
	}
	return out
}
func retryError(message string) types.StreamEvent {
	return types.StreamEvent{Type: types.EvError, Reason: types.StopError, Err: &types.AssistantMessage{ErrorMessage: message, StopReason: types.StopError}}
}
func eventTypes(events []types.StreamEvent) string {
	names := make([]string, len(events))
	for i, event := range events {
		names[i] = string(event.Type)
	}
	return strings.Join(names, " ")
}
func collect(s *stream.AssistantStream) []types.StreamEvent {
	var result []types.StreamEvent
	for event := range s.Events() {
		result = append(result, event)
	}
	return result
}
func testConfig(s Sleeper) Config {
	cfg := DefaultConfig()
	cfg.Clock = fixedClock{now: time.Unix(1_700_000_000, 0)}
	cfg.Sleeper = s
	cfg.Random = fixedRandom(1)
	return cfg
}

func TestDefaultConfigAndValidation(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.MaxAttempts != 11 || cfg.BaseDelay != 500*time.Millisecond || cfg.BackoffCap != 8*time.Second || cfg.MaxDelay != 5*time.Minute || cfg.Jitter != (Jitter{Min: .75, Max: 1}) {
		t.Fatalf("default config = %+v", cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.MaxAttempts = 0
	if err := cfg.Validate(); err == nil {
		t.Fatal("invalid attempts accepted")
	}
}

func TestHintParsingAndClassificationPrecedence(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	delay, ok := RetryAfter(map[string]string{"Retry-After": "0.5", "retry-after-ms": "600", "X-RateLimit-Reset": "2"}, now)
	if !ok || delay != 2*time.Second {
		t.Fatalf("hint = %v, %v", delay, ok)
	}
	date := now.Add(3 * time.Second).UTC().Format(http.TimeFormat)
	if delay, ok = RetryAfter(map[string]string{"RETRY-AFTER": date}, now); !ok || delay != 3*time.Second {
		t.Fatalf("http date = %v, %v", delay, ok)
	}
	classified := Classify(Failure{Message: "maximum context length", Status: 429}, now)
	if classified.Code != CodeContextOverflow || classified.Retryable {
		t.Fatalf("overflow classification = %+v", classified)
	}
	classified = Classify(Failure{Message: "connection reset"}, now)
	if !errors.Is(classified, ErrRetryable) || classified.Code != CodeNetworkFailure {
		t.Fatalf("network classification = %+v", classified)
	}
}

func TestRetryAttemptsHintsAndDiscardedAttempt(t *testing.T) {
	first := testStream(types.StreamEvent{Type: types.EvStart}, retryError("connection reset"))
	second := testStream(types.StreamEvent{Type: types.EvStart}, types.StreamEvent{Type: types.EvTextStart, ContentIndex: 0}, types.StreamEvent{Type: types.EvTextDelta, ContentIndex: 0, Delta: "ok"}, types.StreamEvent{Type: types.EvDone, Reason: types.StopStop, Message: &types.AssistantMessage{StopReason: types.StopStop}})
	source := &fakeStreamer{streams: []*stream.AssistantStream{first, second}}
	sleeper := &recordedSleeper{}
	r, err := New(source, testConfig(sleeper))
	if err != nil {
		t.Fatal(err)
	}
	events := collect(r.Stream(context.Background(), testModel(), types.Context{}, &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{Headers: map[string]string{"x": "caller"}}}))
	if source.calls != 2 || len(sleeper.delays) != 1 || sleeper.delays[0] != 500*time.Millisecond {
		t.Fatalf("calls=%d delays=%v", source.calls, sleeper.delays)
	}
	if got := eventTypes(events); got != "start retry start text_start text_delta done" {
		t.Fatalf("events = %s", got)
	}
	if retry := events[1]; retry.Attempt != 1 || retry.RetryDelay != 500*time.Millisecond || retry.Err == nil || retry.Err.ErrorMessage != "connection reset" || retry.Err.ErrorCode != string(CodeNetworkFailure) || retry.Err.StopReason != types.StopError {
		t.Fatalf("retry event = %+v err=%+v", retry, retry.Err)
	}
	if events[4].Partial == nil || events[4].Partial.Content[0].Text != "ok" {
		t.Fatalf("events = %+v", events)
	}
	source.options[0].Headers["attempt"] = "one"
	if source.options[1].Headers["attempt"] != "" {
		t.Fatal("attempt headers aliased")
	}
}

// A transient failure after output was forwarded discards the attempt (its
// content and reported usage travel in the retry event) and re-sends the request.
func TestRetryAfterOutputDiscardsAttempt(t *testing.T) {
	stalled := retryError("stream idle for 1m30s (no events)")
	stalled.Err.ErrorCode = string(CodeStreamInterrupted)
	stalled.Err.Content = []types.ContentBlock{{Type: types.BlockThinking, Thinking: "Let's look"}}
	stalled.Err.Usage = types.Usage{Input: 100, TotalTokens: 100}
	first := testStream(types.StreamEvent{Type: types.EvStart}, types.StreamEvent{Type: types.EvThinkingDelta, ContentIndex: 0, Delta: "Let's look"}, stalled)
	final := &types.AssistantMessage{StopReason: types.StopStop, Content: []types.ContentBlock{{Type: types.BlockText, Text: "done"}}, Usage: types.Usage{Input: 100, Output: 5, TotalTokens: 105}}
	second := testStream(types.StreamEvent{Type: types.EvStart}, types.StreamEvent{Type: types.EvTextDelta, ContentIndex: 0, Delta: "done"}, types.StreamEvent{Type: types.EvDone, Reason: types.StopStop, Message: final})
	source := &fakeStreamer{streams: []*stream.AssistantStream{first, second}}
	r, err := New(source, testConfig(&recordedSleeper{}))
	if err != nil {
		t.Fatal(err)
	}
	out := r.Stream(context.Background(), testModel(), types.Context{}, nil)
	events := collect(out)
	if source.calls != 2 || eventTypes(events) != "start thinking_delta retry start text_delta done" {
		t.Fatalf("calls=%d events=%s", source.calls, eventTypes(events))
	}
	discarded := events[2].Err
	if discarded.Content[0].Thinking != "Let's look" || discarded.Usage.Input != 100 || discarded.ErrorCode != string(CodeStreamInterrupted) {
		t.Fatalf("discarded attempt = %+v", discarded)
	}
	if partial := events[4].Partial; len(partial.Content) != 1 || partial.Content[0].Text != "done" {
		t.Fatalf("second attempt partial carries the discarded attempt: %+v", partial)
	}
	message, err := out.Result(context.Background())
	if err != nil || message.Usage.TotalTokens != 105 || out.Final().Content[0].Text != "done" {
		t.Fatalf("result = %+v, %v; final = %+v", message, err, out.Final())
	}
}

func TestTerminalFailureAfterOutputIsNotRetried(t *testing.T) {
	invalid := retryError("bad tool schema")
	invalid.Err.ErrorCode = "invalid_request_error"
	visible := testStream(types.StreamEvent{Type: types.EvTextDelta, ContentIndex: 0, Delta: "shown"}, invalid)
	source := &fakeStreamer{streams: []*stream.AssistantStream{visible}}
	r, err := New(source, testConfig(&recordedSleeper{}))
	if err != nil {
		t.Fatal(err)
	}
	events := collect(r.Stream(context.Background(), testModel(), types.Context{}, nil))
	if source.calls != 1 || eventTypes(events) != "text_delta error" || events[1].Err.ErrorMessage != "bad tool schema" {
		t.Fatalf("calls=%d events=%s", source.calls, eventTypes(events))
	}
}

func TestCanceledBeforeAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	source := &fakeStreamer{}
	r, err := New(source, testConfig(&recordedSleeper{}))
	if err != nil {
		t.Fatal(err)
	}
	events := collect(r.Stream(ctx, testModel(), types.Context{}, nil))
	if source.calls != 0 || len(events) != 1 || events[0].Err.ErrorCode != string(CodeCanceled) {
		t.Fatalf("calls=%d events=%+v", source.calls, events)
	}
}
