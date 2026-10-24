package retry

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
)

type sequenceRandom struct {
	mu     sync.Mutex
	values []float64
	calls  int
}

func (r *sequenceRandom) Float64() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if len(r.values) == 0 {
		return 0
	}
	value := r.values[0]
	r.values = r.values[1:]
	return value
}

type blockingSleeper struct {
	mu      sync.Mutex
	delays  []time.Duration
	entered chan struct{}
}

func (s *blockingSleeper) Sleep(ctx context.Context, delay time.Duration) error {
	s.mu.Lock()
	s.delays = append(s.delays, delay)
	s.mu.Unlock()
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

type matrixAttempt struct {
	response types.ProviderResponse
	events   []types.StreamEvent
	open     bool
	mutate   func(*types.SimpleStreamOptions)
}

type matrixStreamer struct {
	mu           sync.Mutex
	started      chan struct{}
	attempts     []matrixAttempt
	calls        int
	options      []*types.SimpleStreamOptions
	callbackErrs []error
}

func (s *matrixStreamer) Stream(_ context.Context, _ types.Model, _ types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream {
	s.mu.Lock()
	index := s.calls
	if s.started != nil {
		select {
		case s.started <- struct{}{}:
		default:
		}
	}
	s.calls++
	s.options = append(s.options, opts)
	attempt := s.attempts[index]
	s.mu.Unlock()
	if opts != nil && opts.OnResponse != nil {
		err := opts.OnResponse(attempt.response, testModel())
		s.mu.Lock()
		s.callbackErrs = append(s.callbackErrs, err)
		s.mu.Unlock()
	}
	if attempt.mutate != nil {
		attempt.mutate(opts)
	}
	out := stream.NewAssistantStream("provider", "provider", "model")
	for _, event := range attempt.events {
		out.Push(event)
	}
	if !attempt.open && (len(attempt.events) == 0 || (attempt.events[len(attempt.events)-1].Type != types.EvDone && attempt.events[len(attempt.events)-1].Type != types.EvError)) {
		out.End()
	}
	return out
}

func matrixRetryEvent(message string) types.StreamEvent {
	return types.StreamEvent{Type: types.EvError, Reason: types.StopError, Err: &types.AssistantMessage{ErrorMessage: message, StopReason: types.StopError}}
}

func matrixDone() types.StreamEvent {
	return types.StreamEvent{Type: types.EvDone, Reason: types.StopStop, Message: &types.AssistantMessage{StopReason: types.StopStop}}
}

func matrixConfig(s Sleeper, random Random) Config {
	cfg := DefaultConfig()
	cfg.Clock = fixedClock{now: time.Unix(1_700_000_000, 0)}
	cfg.Sleeper = s
	cfg.Random = random
	return cfg
}

func matrixRetrier(t *testing.T, source Streamer, cfg Config) *Retrier {
	t.Helper()
	r, err := New(source, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func terminalCode(t *testing.T, events []types.StreamEvent) Code {
	t.Helper()
	if len(events) == 0 || events[len(events)-1].Type != types.EvError || events[len(events)-1].Err == nil {
		t.Fatalf("terminal events = %+v", events)
	}
	return Code(events[len(events)-1].Err.ErrorCode)
}

func TestRetryValidationMatrix(t *testing.T) {
	base := DefaultConfig()
	cases := []struct {
		name string
		edit func(*Config)
	}{
		{"max-attempts", func(c *Config) { c.MaxAttempts = 0 }},
		{"base-zero", func(c *Config) { c.BaseDelay = 0 }},
		{"base-negative", func(c *Config) { c.BaseDelay = -time.Millisecond }},
		{"cap-below-base", func(c *Config) { c.BackoffCap = c.BaseDelay - time.Nanosecond }},
		{"nil-clock", func(c *Config) { c.Clock = nil }},
		{"nil-sleeper", func(c *Config) { c.Sleeper = nil }},
		{"nil-random", func(c *Config) { c.Random = nil }},
		{"nan-min", func(c *Config) { c.Jitter.Min = math.NaN() }},
		{"inf-max", func(c *Config) { c.Jitter.Max = math.Inf(1) }},
		{"low-min", func(c *Config) { c.Jitter.Min = -.01 }},
		{"high-max", func(c *Config) { c.Jitter.Max = 1.01 }},
		{"reversed", func(c *Config) { c.Jitter = Jitter{Min: 1, Max: .75} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.edit(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
	var nilClock *fixedClock
	cfg := base
	cfg.Clock = nilClock
	if err := cfg.Validate(); err == nil {
		t.Fatal("typed nil clock accepted")
	}
	if _, err := New(nil, base); err == nil {
		t.Fatal("nil stream source accepted")
	}
	var nilSource *matrixStreamer
	if _, err := New(nilSource, base); err == nil {
		t.Fatal("typed nil stream source accepted")
	}
}

func TestExactBackoffSequence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		random float64
		want   []time.Duration
	}{
		{"max-jitter", 1, []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second}},
		{"min-jitter", 0, []time.Duration{375 * time.Millisecond, 750 * time.Millisecond, 1500 * time.Millisecond, 3 * time.Second, 6 * time.Second, 6 * time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sleeper := &recordedSleeper{}
			source := &matrixStreamer{}
			for range tc.want {
				source.attempts = append(source.attempts, matrixAttempt{events: []types.StreamEvent{matrixRetryEvent("connection reset")}})
			}
			source.attempts = append(source.attempts, matrixAttempt{events: []types.StreamEvent{matrixDone()}})
			events := collect(matrixRetrier(t, source, matrixConfig(sleeper, fixedRandom(tc.random))).Stream(context.Background(), testModel(), types.Context{}, nil))
			if len(events) != len(tc.want)+1 || events[len(tc.want)].Type != types.EvDone || len(sleeper.delays) != len(tc.want) {
				t.Fatalf("events=%+v delays=%v", events, sleeper.delays)
			}
			for i := range tc.want {
				if sleeper.delays[i] != tc.want[i] || events[i].Type != types.EvRetry || events[i].Attempt != i+1 || events[i].RetryDelay != tc.want[i] {
					t.Fatalf("delay[%d]=%v event=%+v want %v", i, sleeper.delays[i], events[i], tc.want[i])
				}
			}
		})
	}
}

func TestRandomSamplingBoundsAndSaturation(t *testing.T) {
	sleeper := &recordedSleeper{}
	random := &sequenceRandom{values: []float64{-.1, 0, .25, .5, .75, 1, 2}}
	source := &matrixStreamer{attempts: []matrixAttempt{
		{events: []types.StreamEvent{matrixRetryEvent("connection reset")}},
		{events: []types.StreamEvent{matrixRetryEvent("connection reset")}},
		{events: []types.StreamEvent{matrixRetryEvent("connection reset")}},
		{events: []types.StreamEvent{matrixRetryEvent("connection reset")}},
		{events: []types.StreamEvent{matrixRetryEvent("connection reset")}},
		{events: []types.StreamEvent{matrixRetryEvent("connection reset")}},
		{events: []types.StreamEvent{matrixRetryEvent("connection reset")}},
		{events: []types.StreamEvent{matrixDone()}},
	}}
	collect(matrixRetrier(t, source, matrixConfig(sleeper, random)).Stream(context.Background(), testModel(), types.Context{}, nil))
	if random.calls != 7 || len(sleeper.delays) != 7 {
		t.Fatalf("samples=%d delays=%v", random.calls, sleeper.delays)
	}
	for i, delay := range sleeper.delays {
		nominal := nominalDelay(500*time.Millisecond, 8*time.Second, i+1)
		if delay < time.Duration(float64(nominal)*.75) || delay > nominal {
			t.Fatalf("delay[%d]=%v outside [.75*%v,%v]", i, delay, nominal, nominal)
		}
	}
	if got := nominalDelay(time.Second, 8*time.Second, math.MaxInt); got != 8*time.Second {
		t.Fatalf("huge ordinal delay = %v", got)
	}
	terminalRandom := &sequenceRandom{}
	terminal := &matrixStreamer{attempts: []matrixAttempt{{events: []types.StreamEvent{matrixRetryEvent("authentication failed")}}}}
	collect(matrixRetrier(t, terminal, matrixConfig(&recordedSleeper{}, terminalRandom)).Stream(context.Background(), testModel(), types.Context{}, nil))
	if terminalRandom.calls != 0 {
		t.Fatalf("terminal failure sampled random %d times", terminalRandom.calls)
	}
	exhaustedRandom := &sequenceRandom{}
	exhaustedCfg := matrixConfig(&recordedSleeper{}, exhaustedRandom)
	exhaustedCfg.MaxAttempts = 1
	exhausted := &matrixStreamer{attempts: []matrixAttempt{{events: []types.StreamEvent{matrixRetryEvent("connection reset")}}}}
	collect(matrixRetrier(t, exhausted, exhaustedCfg).Stream(context.Background(), testModel(), types.Context{}, nil))
	if exhaustedRandom.calls != 0 {
		t.Fatalf("exhausted final failure sampled random %d times", exhaustedRandom.calls)
	}
}

func TestCancelDuringSleep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sleeper := &blockingSleeper{entered: make(chan struct{}, 1)}
	source := &matrixStreamer{attempts: []matrixAttempt{{events: []types.StreamEvent{matrixRetryEvent("connection reset")}}}}
	out := matrixRetrier(t, source, matrixConfig(sleeper, fixedRandom(1))).Stream(ctx, testModel(), types.Context{}, nil)
	select {
	case <-sleeper.entered:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("sleeper was not entered")
	}
	events := collect(out)
	if source.calls != 1 || eventTypes(events) != "retry error" || terminalCode(t, events) != CodeCanceled || events[1].Reason != types.StopAborted {
		t.Fatalf("calls=%d events=%+v", source.calls, events)
	}
}

func TestCancelDuringActiveAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	source := &matrixStreamer{attempts: []matrixAttempt{{open: true}}, started: make(chan struct{}, 1)}
	out := matrixRetrier(t, source, matrixConfig(&recordedSleeper{}, fixedRandom(1))).Stream(ctx, testModel(), types.Context{}, nil)
	select {
	case <-source.started:
	case <-time.After(time.Second):
		t.Fatal("active attempt did not start")
	}
	cancel()
	events := collect(out)
	if source.calls != 1 || len(events) != 1 || terminalCode(t, events) != CodeCanceled {
		t.Fatalf("calls=%d events=%+v", source.calls, events)
	}
}

func TestHintRoundingBoundaryAndParsing(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cases := []struct {
		name    string
		headers map[string]string
		want    time.Duration
		ok      bool
	}{
		{"mixed-case-integer", map[string]string{"rEtRy-AfTeR": "2"}, 2 * time.Second, true},
		{"fractional-seconds-round-up", map[string]string{"Retry-After": "0.0001"}, time.Millisecond, true},
		{"milliseconds-round-up", map[string]string{"retry-after-ms": "1.1"}, 2 * time.Millisecond, true},
		{"http-date", map[string]string{"Retry-After": now.Add(3 * time.Second).UTC().Format(http.TimeFormat)}, 3 * time.Second, true},
		{"relative-reset", map[string]string{"x-ratelimit-reset": "2"}, 2 * time.Second, true},
		{"relative-reset-ms", map[string]string{"X-RateLimit-Reset-MS": "3"}, 3 * time.Millisecond, true},
		{"absolute-reset", map[string]string{"x-ratelimit-reset": "1700000005"}, 5 * time.Second, true},
		{"absolute-reset-ms", map[string]string{"x-ratelimit-reset-ms": "1700000005000"}, 5 * time.Second, true},
		{"largest-wins", map[string]string{"Retry-After": "1", "retry-after-ms": "1100", "x-ratelimit-reset": "2"}, 2 * time.Second, true},
		{"invalid", map[string]string{"Retry-After": "NaN", "retry-after-ms": "0", "x-ratelimit-reset": "-1"}, 0, false},
		{"expired", map[string]string{"Retry-After": now.Add(-time.Second).UTC().Format(http.TimeFormat), "x-ratelimit-reset": "1699999999"}, 0, false},
		{"oversized", map[string]string{"retry-after-ms": "1e30", "Retry-After": "1e30"}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := RetryAfter(tc.headers, now)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("RetryAfter=%v,%v want %v,%v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestHintsExtendNeverShortenAndMaxDelay(t *testing.T) {
	for _, tc := range []struct {
		name      string
		hint      string
		max       time.Duration
		wantDelay time.Duration
		wantCode  Code
	}{
		{"shorter-never-shortens", "100", time.Second, 500 * time.Millisecond, ""},
		{"longer-extends", "750", time.Second, 750 * time.Millisecond, ""},
		{"equal-max-sleeps", "1000", time.Second, time.Second, ""},
		{"greater-max-fails", "1001", time.Second, 0, CodeRetryDelayExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sleeper := &recordedSleeper{}
			cfg := matrixConfig(sleeper, fixedRandom(1))
			cfg.MaxDelay = tc.max
			source := &matrixStreamer{attempts: []matrixAttempt{
				{response: types.ProviderResponse{Status: 429, Headers: map[string]string{"retry-after-ms": tc.hint}}, events: []types.StreamEvent{matrixRetryEvent("limited")}},
				{events: []types.StreamEvent{matrixDone()}},
			}}
			events := collect(matrixRetrier(t, source, cfg).Stream(context.Background(), testModel(), types.Context{}, nil))
			if tc.wantCode != "" {
				if terminalCode(t, events) != tc.wantCode || len(sleeper.delays) != 0 || source.calls != 1 {
					t.Fatalf("events=%+v delays=%v calls=%d", events, sleeper.delays, source.calls)
				}
				return
			}
			if len(sleeper.delays) != 1 || sleeper.delays[0] != tc.wantDelay {
				t.Fatalf("delays=%v", sleeper.delays)
			}
		})
	}
}

func TestOnResponseChainingAndOptionOwnership(t *testing.T) {
	callerErr := errors.New("caller callback")
	callerCalls := 0
	options := &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{
		Headers: map[string]string{"caller": "header"}, Metadata: map[string]any{"caller": "metadata"}, Env: map[string]string{"caller": "env"},
		OnResponse: func(types.ProviderResponse, types.Model) error { callerCalls++; return callerErr },
	}, ThinkingBudgets: map[string]int{"caller": 1}}
	source := &matrixStreamer{attempts: []matrixAttempt{
		{response: types.ProviderResponse{Status: 500}, events: []types.StreamEvent{matrixRetryEvent("server error")}, mutate: func(o *types.SimpleStreamOptions) {
			o.Headers["attempt"] = "one"
			o.Metadata["attempt"] = "one"
			o.Env["attempt"] = "one"
			o.ThinkingBudgets["attempt"] = 1
		}},
		{response: types.ProviderResponse{Status: 200}, events: []types.StreamEvent{matrixDone()}},
	}}
	collect(matrixRetrier(t, source, matrixConfig(&recordedSleeper{}, fixedRandom(1))).Stream(context.Background(), testModel(), types.Context{}, options))
	if callerCalls != 2 || len(source.callbackErrs) != 2 || source.callbackErrs[0] != callerErr || source.callbackErrs[1] != callerErr {
		t.Fatalf("caller calls=%d callback errors=%v", callerCalls, source.callbackErrs)
	}
	if options.Headers["attempt"] != "" || options.Metadata["attempt"] != nil || options.Env["attempt"] != "" || options.ThinkingBudgets["attempt"] != 0 {
		t.Fatalf("caller options mutated: %+v", options)
	}
	if source.options[1].Headers["attempt"] != "" || source.options[1].Metadata["attempt"] != nil || source.options[1].Env["attempt"] != "" || source.options[1].ThinkingBudgets["attempt"] != 0 {
		t.Fatalf("attempt options aliased: %+v", source.options[1])
	}
	if source.options[0].OnResponse == nil || source.options[1].OnResponse == nil {
		t.Fatal("retry callback missing")
	}
	// Nil caller options must still provide a usable response callback.
	nilSource := &matrixStreamer{attempts: []matrixAttempt{{response: types.ProviderResponse{Status: 400}, events: []types.StreamEvent{matrixRetryEvent("bad request")}}}}
	collect(matrixRetrier(t, nilSource, matrixConfig(&recordedSleeper{}, fixedRandom(1))).Stream(context.Background(), testModel(), types.Context{}, nil))
	if nilSource.options[0] == nil || nilSource.options[0].OnResponse == nil {
		t.Fatal("nil options were not cloned with response hook")
	}
}

func TestClassificationTablesAndStructuredCodePrecedence(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	for _, status := range []int{429, 500, 502, 503, 504, 529} {
		got := Classify(Failure{Status: status}, now)
		want := CodeServerFailure
		if status == 429 {
			want = CodeRateLimited
		}
		if got.Code != want || !got.Retryable {
			t.Fatalf("status %d = %+v", status, got)
		}
	}
	for _, status := range []int{400, 401, 403, 404, 422} {
		got := Classify(Failure{Status: status}, now)
		if got.Retryable {
			t.Fatalf("terminal status %d classified retryable: %+v", status, got)
		}
	}
	for _, message := range []string{"error", "closed", "limit", "timeout"} {
		if got := Classify(Failure{Message: message}, now); got.Retryable {
			t.Fatalf("vague %q classified retryable: %+v", message, got)
		}
	}
	if got := Classify(Failure{Status: 500, Message: "connection reset", ContextErr: context.Canceled}, now); got.Code != CodeCanceled || got.Retryable {
		t.Fatalf("cancel precedence = %+v", got)
	}
	for _, tc := range []struct {
		code  string
		want  Code
		retry bool
	}{{"rate_limit_error", CodeRateLimited, true}, {"authentication-error", CodeAuthentication, false}, {"invalid_request_error", CodeInvalidRequest, false}} {
		got := Classify(Failure{ProviderCode: tc.code, Message: "rate limit connection reset"}, now)
		if got.Code != tc.want || got.Retryable != tc.retry {
			t.Fatalf("structured-code-precedence %q = %+v", tc.code, got)
		}
	}
	for _, status := range []int{429, 500} {
		got := Classify(Failure{Status: status, Message: "maximum context length"}, now)
		if got.Code != CodeContextOverflow || got.Retryable {
			t.Fatalf("overflow-with-500 status=%d got=%+v", status, got)
		}
	}
}

func TestTypedErrorsAndRealExhaustedChain(t *testing.T) {
	cause := errors.New("connection reset")
	classified := Classify(Failure{Message: cause.Error()}, time.Unix(1_700_000_000, 0))
	var exposed *Error
	if !errors.As(classified, &exposed) || exposed.Code != CodeNetworkFailure || !exposed.Retryable || !errors.Is(classified, ErrRetryable) || !errors.Is(classified, &Error{Code: CodeNetworkFailure}) || errors.Is(classified, &Error{}) || classified.Unwrap() == nil {
		t.Fatalf("typed retry error behavior = %+v", classified)
	}
	raw := &Error{Code: CodeNetworkFailure, Cause: cause}
	if !errors.Is(raw, cause) || raw.Unwrap() != cause {
		t.Fatalf("raw cause was not preserved: %+v", raw)
	}
	terminal := Classify(Failure{Status: 400}, time.Unix(1_700_000_000, 0))
	if !errors.Is(terminal, ErrTerminal) || errors.Is(terminal, ErrRetryable) {
		t.Fatalf("terminal sentinel = %+v", terminal)
	}
	canceled := Classify(Failure{ContextErr: context.Canceled}, time.Unix(1_700_000_000, 0))
	if !errors.Is(canceled, ErrCanceled) || !errors.Is(canceled, ErrTerminal) {
		t.Fatalf("canceled sentinel = %+v", canceled)
	}
	source := &matrixStreamer{attempts: []matrixAttempt{{events: []types.StreamEvent{matrixRetryEvent("connection reset")}}, {events: []types.StreamEvent{matrixRetryEvent("connection reset")}}}}
	cfg := matrixConfig(&recordedSleeper{}, fixedRandom(1))
	cfg.MaxAttempts = 2
	events := collect(matrixRetrier(t, source, cfg).Stream(context.Background(), testModel(), types.Context{}, nil))
	if terminalCode(t, events) != CodeAttemptsExhausted || eventTypes(events) != "retry error" || !strings.Contains(events[1].Err.ErrorMessage, "2 attempts") || !strings.Contains(events[1].Err.ErrorMessage, "connection reset") {
		t.Fatalf("real-exhausted-chain events=%+v", events)
	}
	// The generated terminal frame is intentionally not the retryable provider cause.
	// retryClassified carries the provider's raw cause into the exhausted typed
	// outcome, rather than the retryable classification wrapper.
	exhausted := &Error{Code: CodeAttemptsExhausted, Cause: classified.Cause}
	if !errors.Is(exhausted, ErrExhausted) || errors.Is(exhausted, ErrRetryable) {
		t.Fatalf("exhausted Is behavior = %+v", exhausted)
	}
}

func TestAttemptsAndReplaySafety(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failures int
		wantCode Code
	}{{"ten-then-success", 10, ""}, {"eleven-exhausted", 11, CodeAttemptsExhausted}} {
		t.Run(tc.name, func(t *testing.T) {
			source := &matrixStreamer{}
			for range tc.failures {
				source.attempts = append(source.attempts, matrixAttempt{events: []types.StreamEvent{matrixRetryEvent("connection reset")}})
			}
			if tc.wantCode == "" {
				source.attempts = append(source.attempts, matrixAttempt{events: []types.StreamEvent{matrixDone()}})
			}
			sleeper := &recordedSleeper{}
			events := collect(matrixRetrier(t, source, matrixConfig(sleeper, fixedRandom(1))).Stream(context.Background(), testModel(), types.Context{}, nil))
			if source.calls != 11 || len(sleeper.delays) != 10 {
				t.Fatalf("calls=%d sleeps=%d", source.calls, len(sleeper.delays))
			}
			if tc.wantCode != "" && terminalCode(t, events) != tc.wantCode {
				t.Fatalf("events=%+v", events)
			}
		})
	}
	providerPartial := &types.AssistantMessage{API: "wrong"}
	source := &matrixStreamer{attempts: []matrixAttempt{{events: []types.StreamEvent{
		{Type: types.EvStart, Partial: providerPartial}, {Type: types.EvTextStart, ContentIndex: 0}, {Type: types.EvTextDelta, ContentIndex: 0, Delta: "ok"}, {Type: types.EvDone, Reason: types.StopStop, Message: &types.AssistantMessage{StopReason: types.StopStop}},
	}}, {events: []types.StreamEvent{{Type: types.EvStart}, {Type: types.EvTextStart, ContentIndex: 0}, matrixRetryEvent("connection reset")}}, {events: []types.StreamEvent{{Type: types.EvStart}, {Type: types.EvTextDelta, ContentIndex: 0, Delta: "again"}, matrixDone()}}}}
	// First run verifies logical partial ownership and accumulated deltas.
	first := &matrixStreamer{attempts: []matrixAttempt{{events: source.attempts[0].events}}}
	events := collect(matrixRetrier(t, first, matrixConfig(&recordedSleeper{}, fixedRandom(1))).Stream(context.Background(), testModel(), types.Context{}, nil))
	for i, event := range events[:3] {
		if event.Partial == nil || event.Partial.API != "test" || event.Partial.Provider != "test" || event.Partial.Model != "test" {
			t.Fatalf("partial[%d]=%+v", i, event.Partial)
		}
	}
	if events[2].Partial.Content[0].Text != "ok" || events[2].Partial == providerPartial {
		t.Fatalf("delta partial = %+v", events[2].Partial)
	}
	providerPartial.Content = []types.ContentBlock{{Type: types.BlockText, Text: "mutated"}}
	if events[2].Partial.Content[0].Text != "ok" {
		t.Fatal("forwarded partial aliases provider")
	}
	// A failed attempt is discarded by a retry event, then a coherent second attempt forwarded.
	replay := &matrixStreamer{attempts: source.attempts[1:]}
	events = collect(matrixRetrier(t, replay, matrixConfig(&recordedSleeper{}, fixedRandom(1))).Stream(context.Background(), testModel(), types.Context{}, nil))
	if replay.calls != 2 || eventTypes(events) != "start text_start retry start text_delta done" || len(events[4].Partial.Content) != 1 || events[4].Partial.Content[0].Text != "again" {
		t.Fatalf("replay events=%+v", events)
	}
}

func TestEveryOutputKindIsRetriedWithOwnedPartials(t *testing.T) {
	variants := []struct {
		name  string
		event types.StreamEvent
	}{
		{"text", types.StreamEvent{Type: types.EvTextDelta, ContentIndex: 0, Delta: "shown"}},
		{"thinking", types.StreamEvent{Type: types.EvThinkingDelta, ContentIndex: 0, Delta: "thought"}},
		{"tool-start", types.StreamEvent{Type: types.EvToolCallStart, ContentIndex: 0}},
		{"tool-delta", types.StreamEvent{Type: types.EvToolCallDelta, ContentIndex: 0, Delta: "{}"}},
		{"tool-end", types.StreamEvent{Type: types.EvToolCallEnd, ContentIndex: 0, ToolCall: &types.ContentBlock{Type: types.BlockToolCall, ID: "id", Name: "tool", Arguments: []byte(`{}`)}}},
		{"text-end", types.StreamEvent{Type: types.EvTextEnd, ContentIndex: 0, Content: "partial"}},
	}
	for _, tc := range variants {
		t.Run(tc.name, func(t *testing.T) {
			downstreamPartial := &types.AssistantMessage{API: "downstream"}
			tc.event.Partial = downstreamPartial
			source := &matrixStreamer{attempts: []matrixAttempt{{events: []types.StreamEvent{tc.event, matrixRetryEvent("connection reset")}}, {events: []types.StreamEvent{matrixDone()}}}}
			events := collect(matrixRetrier(t, source, matrixConfig(&recordedSleeper{}, fixedRandom(1))).Stream(context.Background(), testModel(), types.Context{}, nil))
			if source.calls != 2 || eventTypes(events) != string(tc.event.Type)+" retry done" {
				t.Fatalf("calls=%d events=%s", source.calls, eventTypes(events))
			}
			if events[0].Partial == nil || events[0].Partial.API != "test" || events[0].Partial.Provider != "test" || events[0].Partial.Model != "test" || events[0].Partial == downstreamPartial {
				t.Fatalf("forwarded event has an unowned logical partial: %+v", events[0])
			}
		})
	}
	terminalContent := &matrixStreamer{attempts: []matrixAttempt{{events: []types.StreamEvent{{Type: types.EvError, Reason: types.StopError, Err: &types.AssistantMessage{ErrorMessage: "connection reset", Content: []types.ContentBlock{{Type: types.BlockText, Text: "visible"}}}}}}, {events: []types.StreamEvent{matrixDone()}}}}
	if events := collect(matrixRetrier(t, terminalContent, matrixConfig(&recordedSleeper{}, fixedRandom(1))).Stream(context.Background(), testModel(), types.Context{}, nil)); terminalContent.calls != 2 || eventTypes(events) != "retry done" || events[0].Err.Content[0].Text != "visible" {
		t.Fatalf("terminal content events=%+v calls=%d", events, terminalContent.calls)
	}
}

// A provider that accepted the request (2xx) and then failed in the stream is
// classified by the failure's code, not by the accepted status.
func TestFailureAfterAcceptedResponseIsClassifiedByCode(t *testing.T) {
	stalled := matrixRetryEvent("stream idle for 1m30s (no events)")
	stalled.Err.ErrorCode = string(CodeStreamInterrupted)
	invalid := matrixRetryEvent("unsupported parameter")
	invalid.Err.ErrorCode = "invalid_request_error"
	accepted := types.ProviderResponse{Status: 200}
	source := &matrixStreamer{attempts: []matrixAttempt{
		{response: accepted, events: []types.StreamEvent{{Type: types.EvStart}, {Type: types.EvTextDelta, ContentIndex: 0, Delta: "partial"}, stalled}},
		{response: accepted, events: []types.StreamEvent{{Type: types.EvStart}, invalid}},
	}}
	events := collect(matrixRetrier(t, source, matrixConfig(&recordedSleeper{}, fixedRandom(1))).Stream(context.Background(), testModel(), types.Context{}, nil))
	if source.calls != 2 || eventTypes(events) != "start text_delta retry start error" || events[2].Err.ErrorCode != string(CodeStreamInterrupted) || events[4].Err.ErrorCode != "invalid_request_error" {
		t.Fatalf("calls=%d events=%s", source.calls, eventTypes(events))
	}
}

func TestStreamClose(t *testing.T) {
	before := &matrixStreamer{attempts: []matrixAttempt{{events: nil}, {events: []types.StreamEvent{matrixDone()}}}}
	events := collect(matrixRetrier(t, before, matrixConfig(&recordedSleeper{}, fixedRandom(1))).Stream(context.Background(), testModel(), types.Context{}, nil))
	if before.calls != 2 || eventTypes(events) != "retry done" {
		t.Fatalf("stream-close-before-output calls=%d events=%+v", before.calls, events)
	}
	after := &matrixStreamer{attempts: []matrixAttempt{{events: []types.StreamEvent{{Type: types.EvTextDelta, ContentIndex: 0, Delta: "visible"}}}, {events: []types.StreamEvent{matrixDone()}}}}
	events = collect(matrixRetrier(t, after, matrixConfig(&recordedSleeper{}, fixedRandom(1))).Stream(context.Background(), testModel(), types.Context{}, nil))
	if after.calls != 2 || eventTypes(events) != "text_delta retry done" || events[1].Err.ErrorCode != string(CodeStreamInterrupted) || events[1].Err.Content[0].Text != "visible" {
		t.Fatalf("stream-close-after-output calls=%d events=%+v", after.calls, events)
	}
	empty := &matrixStreamer{attempts: []matrixAttempt{{events: []types.StreamEvent{{Type: types.EvStart}, matrixDone()}}}}
	events = collect(matrixRetrier(t, empty, matrixConfig(&recordedSleeper{}, fixedRandom(1))).Stream(context.Background(), testModel(), types.Context{}, nil))
	if eventTypes(events) != "start done" {
		t.Fatalf("empty success events=%+v", events)
	}
}

func TestConcurrentRNG(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Clock = fixedClock{now: time.Unix(1_700_000_000, 0)}
	cfg.Sleeper = &recordedSleeper{}
	const streams = 8
	var wg sync.WaitGroup
	errCh := make(chan error, streams)
	for range streams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			source := &matrixStreamer{attempts: []matrixAttempt{{events: []types.StreamEvent{matrixRetryEvent("connection reset")}}, {events: []types.StreamEvent{matrixDone()}}}}
			r, err := New(source, cfg)
			if err != nil {
				errCh <- err
				return
			}
			events := collect(r.Stream(context.Background(), testModel(), types.Context{}, nil))
			if source.calls != 2 || eventTypes(events) != "retry done" {
				errCh <- errors.New("incoherent concurrent stream")
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}
