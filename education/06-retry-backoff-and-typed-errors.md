# 06 — Retry, backoff, and typed errors

## The problem

Every provider call is a network call. Networks fail, rate-limiters kick in, servers return 500s, and streams drop mid-chunk. Without retry, any of these turns a provider outage into a user-visible error: the agent loop sees an `EvError`, the turn ends, and the user gets a failure message they have to retype.

Worse, the failure symptoms leak through. An HTTP 429 from Anthropic looks different from a 503 from Google, which looks different from a TCP reset during an SSE stream. If every call site — or worse, the agent loop — has to know about `Retry-After` headers, exponential backoff math, and provider-specific error codes, the system rots into string matching scattered across packages. The agent loop is already complex enough with tool folding, transcript management, and compaction. Adding per-attempt sleep, attempt counting, and header parsing would violate every separation-of-concerns boundary.

The TypeScript reference agent handled this inline: `try/catch` blocks in the prompt-loop tried up to `maxRetries` times, parsed `Retry-After` headers from raw HTTP responses, and called `setTimeout` between attempts. It worked, but retry policy, stream ownership, and the prompt loop were tangled into one `while (true)` block, and the clock was always `Date.now()` — no determinism, no test isolation.

## Key decisions and the thinking process

### Decision 1: decorate at the provider stream boundary, not the agent loop

The frozen packages `internal/agent`, `internal/provider`, and `internal/engine` cannot be edited in V1. The question was where to insert retry as a non-frozen consumer.

The seam already existed. Every provider is called through a single function signature ([`internal/agent/types.go`](internal/agent/types.go)):

```go
type StreamFn func(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream
```

`internal/adapter.NewProviderRouter` constructs the concrete provider function (binding API keys, credentials, and a `base.SimpleStreamFunc`), stores it as `ProviderRouter.StreamFn`, and returns the router. The agent loop calls `StreamFn` and folds the returned stream. The decorator slides in between construction and storage:

```go
// internal/adapter/provider.go:105-110
if cfg.Retry != nil {
    retrier, err := retry.New(retry.StreamFunc(r.StreamFn), *cfg.Retry)
    ...
    r.StreamFn = agent.StreamFn(retrier.Stream)
}
```

So `Retrier.Stream` has the exact same signature as `agent.StreamFn`. The agent loop never knows it's talking to a retry wrapper. The frozen code is untouched.

Alternatives rejected:

- **Edit the frozen agent loop.** Would couple retry policy to tool-loop and transcript semantics. The loop shouldn't know whether a stream came from a real provider or a retry wrapper.
- **Wrap inside each frozen provider.** Duplicates policy across Anthropic, OpenAI, and Google. Any change requires touching `internal/provider/**`.
- **Wrap `http.DefaultTransport`.** Process-global mutable state. Would retry *all* HTTP in the process, including user tool calls. Not acceptable.
- **Wrap in `cmd/harness` only.** Easy to bypass from other stack constructors. Decorating `ProviderRouter` means every real router returned by `ResolveRoles` gets retry.
- **Wrap in `session.BuildAgentStack`.** Would make session assembly own provider HTTP classification and would retry the faux/test script by default.

The faux script override in `cmd/harness` intentionally stays undecorated: it's a deterministic fixture, not a transient network provider, and retrying it would consume extra scripted steps.

### Decision 2: dependency injection for determinism

The TypeScript reference called `Date.now()` and `setTimeout` directly. In Go, we inject three interfaces:

```go
// internal/retry/config.go:21-33
type Clock interface { Now() time.Time }
type Sleeper interface { Sleep(ctx context.Context, delay time.Duration) error }
type Random interface { Float64() float64 }
```

`Config` holds them as fields alongside `MaxAttempts`, `BaseDelay`, `BackoffCap`, `MaxDelay`, and `Jitter`. `DefaultConfig()` fills in `realClock{}`, `timerSleeper{}`, and a mutex-protected `lockedRandom`. Tests inject fake clocks (pinned to a known instant), fake sleepers (no-op or time-advancing), and deterministic random sources.

This means every test that asserts "after a 429 with `Retry-After: 5s`, we sleep 5s" is deterministic, not flaky. The fake sleeper records what delay it was asked to sleep for; the test asserts the exact value. Same for hint parsing: the injected clock is pinned, so "a reset 30 seconds from now" is always exactly 30 seconds.

Without DI, backoff tests would be probabilistic at best (jitter sampling), or require `time.Sleep` in test code.

### Decision 3: classification precedence with a structured-code tier

Classify a provider failure with twelve possible outcomes (from `CodeRateLimited` to `CodeProviderFailure`). The order matters because a failure can match multiple categories: a 429 with message "context length exceeded" should be `CodeContextOverflow` (terminal), not `CodeRateLimited` (retryable). The precedence in `Classify` ([`internal/retry/error.go:100-168`](internal/retry/error.go)):

1. **Cancel/abort first.** If `ctx.Err() != nil` or stop reason is `StopAborted`, return `CodeCanceled` — terminal, never retry.
2. **Context overflow before status.** The combined `providerCode + " " + message` is scanned for `context_length_exceeded`, `prompt_too_long`, `maximum context length`, `too many input tokens`, or `context window ... exceeded`. If matched, return `CodeContextOverflow` even if the gateway supplied 429. Overflow is the compaction module's concern, not retry's.
3. **HTTP status.** Only a status of 400 or above classifies the failure. 429 → `CodeRateLimited` (retryable). 5xx → `CodeServerFailure` (retryable). 401/403 → `CodeAuthentication`, 400/404/409/422 → `CodeInvalidRequest`, other 4xx → `CodeInvalidRequest`, other statuses at or above 400 → `CodeProviderFailure`. All terminal. A 2xx status means the provider accepted the request and the stream failed afterwards (a stall, a dropped connection, an in-stream error event), so the status tier is skipped and the code and message tiers below classify it (`internal/retry/error.go:131-147`). Before this change any nonzero status, including 200, fell into a terminal provider-failure branch, so no in-stream failure after a 200 was ever retried.
4. **Structured provider code.** When the status does not classify the failure (network errors, stream drops, a 2xx that failed afterwards), `classifyProviderCode` matches the normalized `ProviderCode` against known categories before falling through to message patterns. `rate_limit_error` → retryable `CodeRateLimited`; `overloaded_error`/`server_error` → retryable `CodeServerFailure`; `authentication_error`/`invalid_api_key` → terminal `CodeAuthentication`; `invalid_request_error` → terminal `CodeInvalidRequest`. It also maps the codes this package assigns back to themselves: `stream_interrupted`, `network_failure`, `attempt_timeout`, and `server_failure` are retryable, `authentication` and `invalid_request` are terminal, and `canceled` is never mapped because only the caller's context decides cancellation (`error.go:179-198`). This tier was added as a post-review fix: without it, `ProviderCode: "rate_limit_error"` would fall through message matching and land on terminal `CodeProviderFailure` because `"rate_limit_error"` doesn't contain the substring `"rate limit"`.
5. **Message pattern fallback.** Narrow, bounded patterns only: `i/o timeout`, `tls handshake timeout`, `connection reset`, `broken pipe`, `unexpected eof`, bare `eof`, `socket hang up`, `stream idle`, `stream ended without finish_reason`, `fetch failed`, `upstream ... reset`, `overloaded`, `service unavailable`, `too many requests`, `rate limit`, `provider returned error`. No bare `error`, `closed`, `limit`, or `timeout` matches.
6. **Terminal fallback.** `CodeProviderFailure`, not retryable.

The key property: a recognized structured provider code cannot be overridden by broad message wording. A terminal `authentication_error` code stays terminal even if the message happens to contain `"timed out"`.

### Decision 4: typed errors with `Unwrap`/`Is`/`As`

The error type carries everything a caller needs without exposing provider internals:

```go
// internal/retry/error.go:41-49
type Error struct {
    Code       Code
    Message    string
    Retryable  bool
    RetryAfter time.Duration
    Attempts   int
    Cause      error
}
```

Four sentinel errors (`ErrRetryable`, `ErrTerminal`, `ErrExhausted`, `ErrCanceled`) let callers branch on the shape of failure without inspecting codes. `Error.Is` dispatches: `ErrRetryable` matches any retryable classified error, `ErrTerminal` matches any non-retryable, `ErrExhausted` matches `CodeAttemptsExhausted`, `ErrCanceled` matches `CodeCanceled`, and a code-level match works for `errors.Is(err, &retry.Error{Code: CodeRateLimited})`.

`Unwrap` returns `e.Cause` — the raw provider failure text. So `errors.Is(retryErr, someSentinel)` checks the classified outcome, and `errors.As(retryErr, &providerErr)` digs into the original.

Critically, exhaustion does not unwrap to the retryable classified error. When `plan` finds `attempt >= MaxAttempts` (`internal/retry/stream.go:148-150`), it constructs an `Error` whose `Cause` is the *original* `classified.Cause` (the provider message), not the retryable `classified` pointer itself. So `errors.Is(exhaustedErr, ErrRetryable)` is false: exhaustion is terminal even though the individual failure that triggered it was retryable. Its message is `retry attempts exhausted after N attempts: <cause>`.

### Decision 5: retry after output, not only before it

This is the policy's behavioral core, but the concurrency/race story lives in module 07. Here, we cover the *policy*.

Each `Retrier.Stream` call creates one output `AssistantStream` and launches one producer goroutine (`internal/retry/stream.go:45-49`). The producer loops through attempts:

1. Call the inner `Streamer` (which calls the real provider).
2. Forward every non-terminal event of the attempt as it arrives (`forward` at `stream.go:293-302`); the terminal event is either pushed (done) or classified (error).
3. If the attempt ends with `EvDone`, push it and return.
4. If the attempt ends with a non-retryable error, push the provider's error event unchanged and return.
5. If the attempt fails with a retryable error, before or after output, build the record of the discarded attempt (`attemptMessage` at `stream.go:164-180`). That record is the provider's terminal message with its partial content and reported usage when the provider sent one, otherwise the content forwarded so far. Push `EvRetry` carrying that message in `Err`, the attempt ordinal in `Attempt`, and the backoff in `RetryDelay` (`stream.go:89-91`). Fold the event into the logical builder (which resets it), sleep, and re-send the same `types.Context` and freshly cloned options (same cache key).

The content events of every attempt reach the caller on the same output stream; a retryable failure replaces that attempt's terminal error with `EvRetry`. Re-sending after output is safe because nothing acts on a response before its terminal event: the agent loop runs tool calls only after the final message. On `EvRetry` the agent loop closes the partial message it had opened as an errored message, emits its `message_end` (the journal records it as a metered message with its usage record), and appends it to the run's messages through the `discarded` callback (`internal/agent/loop.go:452-458`, `loop.go:796-813`). The next attempt starts a new message. Errored assistant messages are skipped by the transform, the token estimate and compaction, so the re-sent and later requests never include the discarded attempt.

The `Partial` pointer on forwarded events is never the provider's live pointer. The decorator owns a private `AssistantBuilder`; each event is folded into it, and a fresh snapshot is attached as `event.Partial` before pushing. Module 07 explains why the provider's live `Partial` is a race condition waiting to happen.

### Decision 6: the ms-ceiling hint rounding and overflow bounds

Provider retry hints arrive as decimal strings in various units. Rounding is deliberate:

- **Decimal milliseconds** (`retry-after-ms: 1.1`): `durationCeilMillis` computes `ceil(value)` whole milliseconds. `1.1ms` becomes `2ms`.
- **Decimal seconds** (`Retry-After: 0.0001`): converts to milliseconds first: `ceil(0.0001 * 1000)` = `ceil(0.1)` = `1ms`.
- **Absolute reset timestamps** (> `1e12` interpreted as Unix ms, > `1e9` as Unix s): converts to a time instant, subtracts the injected clock's `Now()`, rejects already-expired instants.

Before conversion, `durationCeilMillis` bounds-checks in millisecond units: if `ceil(millis) >= float64(math.MaxInt64) / float64(time.Millisecond)`, it rejects the hint as invalid. This prevents a float-to-`time.Duration` conversion from wrapping to a negative value.

The config layer has the same guard: `maxRetryDurationMS = math.MaxInt64 / 1_000_000` ([`internal/config/config.go:238`](internal/config/config.go)), and `Resolve` rejects any positive millisecond value above it. This was a post-review fix — without it, `retry_max_delay_ms=10000000000000` passes validation but overflows to a negative `time.Duration`, silently disabling the fail-fast cap.

## Signatures and types

### Configuration and injected dependencies

```go
// internal/retry/config.go
type Jitter struct { Min, Max float64 }           // line 15-18

type Config struct {                              // line 36-45
    MaxAttempts int                                // 1 + retry count; initial call is attempt 1
    BaseDelay   time.Duration                      // nominal delay for retry ordinal 1
    BackoffCap  time.Duration                      // ceiling on exponential growth
    MaxDelay    time.Duration                      // > 0: fail-fast cap; <= 0: disabled
    Jitter      Jitter                             // multiplicative factor bounds
    Clock       Clock                              // injected time source
    Sleeper     Sleeper                            // injected context-aware sleep
    Random      Random                             // injected jitter source
}

// Consumer-defined, one-method interfaces for determinism.
type Clock interface {
    Now() time.Time                                // line 21-23
}
type Sleeper interface {
    Sleep(ctx context.Context, delay time.Duration) error // line 25-28
}
type Random interface {
    Float64() float64 // contract: [0, 1)          // line 30-33
}
```

| Parameter | Role |
|---|---|
| `MaxAttempts` | Total provider calls allowed for one logical stream. Default 11. |
| `BaseDelay` | Delay before the first retry (attempt 1 fails → sleep `BaseDelay` → attempt 2). Default 500ms. |
| `BackoffCap` | Maximum nominal delay. Exponential growth saturates here. Default 8s. |
| `MaxDelay` | Fail-fast ceiling on *any* delay (local + provider hint). `> 0` enforces it; `<= 0` disables. Default 5m. |
| `Jitter.Min`/`Max` | Multiplier range applied to nominal delay. Default `[0.75, 1.0]`. |
| `Clock` | Supplies `time.Now()` for hint parsing. Production: `realClock{}`. Tests: pinned. |
| `Sleeper` | Blocks for `delay` while honoring `ctx.Done()`. Production: `timerSleeper{}`. Tests: recording fake. |
| `Random` | Supplies independent `[0,1)` samples per retry. Production: mutex-protected PCG. Tests: deterministic seq. |

```go
func DefaultConfig() Config                         // line 84-96
func (c Config) Validate() error                    // line 99-119
```

`DefaultConfig`: 11 attempts, 500ms base, 8s cap, 5m max, jitter `[0.75, 1.0]`, real clock, timer sleeper, locked per-instance PCG RNG. `Validate` rejects `MaxAttempts < 1`, `BaseDelay <= 0`, `BackoffCap < BaseDelay`, nil deps, NaN/Inf jitter, jitter outside `[0,1]`, `Min > Max`. `MaxDelay <= 0` is valid.

### Typed error taxonomy

```go
// internal/retry/error.go
type Code string                                    // line 13

const (                                             // lines 15-28
    CodeRateLimited        Code = "rate_limited"     // 429 or rate-limit wording
    CodeServerFailure      Code = "server_failure"   // 5xx or overload wording  
    CodeNetworkFailure     Code = "network_failure"  // TCP reset, broken pipe, EOF
    CodeAttemptTimeout     Code = "attempt_timeout"  // i/o timeout, TLS timeout
    CodeStreamInterrupted  Code = "stream_interrupted" // stream drop before terminal
    CodeContextOverflow    Code = "context_overflow" // context-length exceeded
    CodeCanceled           Code = "canceled"         // ctx cancelled or StopAborted
    CodeAuthentication     Code = "authentication"   // 401, 403, invalid API key
    CodeInvalidRequest     Code = "invalid_request"  // 400, 404, 409, 422, other 4xx
    CodeProviderFailure    Code = "provider_failure" // unclassified terminal
    CodeAttemptsExhausted  Code = "attempts_exhausted" // all retries used
    CodeRetryDelayExceeded Code = "retry_delay_exceeded" // delay > MaxDelay
)
```

```go
var (                                               // lines 30-39
    ErrRetryable = errors.New("retryable provider failure")
    ErrTerminal  = errors.New("terminal provider failure")
    ErrExhausted = errors.New("retry attempts exhausted")
    ErrCanceled  = errors.New("retry canceled")
)
```

```go
type Error struct {                                 // lines 42-49
    Code       Code         // stable category, never empty
    Message    string       // safe, non-sensitive provider/retry text
    Retryable  bool         // intrinsic transience; retried before or after output
    RetryAfter time.Duration // largest valid provider hint, or zero
    Attempts   int          // for exhausted/exceeded outcomes; 0 for classified failures
    Cause      error        // raw provider failure; may be nil
}
```

| Parameter | Role |
|---|---|
| `Code` | Stable enum for programmatic matching (`errors.Is`). |
| `Message` | Human-readable but safe: no request data, no headers. |
| `Retryable` | Whether the *category* is transient. The retrier retries it before or after output; only the policy limits (attempts, delay cap) can end the stream. |
| `RetryAfter` | Parsed from `retry-after-ms` / `Retry-After` / `x-ratelimit-reset*` headers; zero if absent/invalid. |
| `Attempts` | The retry ordinal where exhaustion or delay-exceeded occurred. |
| `Cause` | The original failure text (`errors.Unwrap` target). |

```go
func (e *Error) Error() string                      // line 52-60
func (e *Error) Unwrap() error                      // line 62-68
func (e *Error) Is(target error) bool               // line 70-87
```

`Error()` returns `e.Message` if non-empty, otherwise `"provider failure: <code>"`. `Unwrap()` returns `e.Cause`. `Is` dispatches: `ErrRetryable` → `e.Retryable`, `ErrTerminal` → `!e.Retryable`, `ErrExhausted` → `CodeAttemptsExhausted`, `ErrCanceled` → `CodeCanceled`, and code-level match for `(*Error)` with a non-empty `Code`.

### Classification and hints

```go
type Failure struct {                               // lines 90-97
    Message      string            // provider error text
    ProviderCode string            // structured provider error code
    StopReason   types.StopReason  // stop_sequence, max_tokens, stop_aborted, stop_error
    Status       int               // HTTP status, or 0 if unavailable
    Headers      map[string]string // response headers for retry hints
    ContextErr   error             // parent ctx.Err() at classification time
}

func Classify(f Failure, now time.Time) *Error      // line 100
```

| Parameter | Role |
|---|---|
| `f.Message` | The terminal assistant error message. Trimmed, case-folded for matching but preserved verbatim in the result. |
| `f.ProviderCode` | Structured code from the provider (e.g. `"rate_limit_error"`). Normalized (lowercased, dashes→underscores) and matched before message fallback. |
| `f.StopReason` | `StopAborted` → cancel. Others inform but don't drive classification directly. |
| `f.Status` | Drives the HTTP-based tiers (429, 5xx, specific 4xx). Zero means no status available. |
| `f.Headers` | Source of `Retry-After`, `retry-after-ms`, `x-ratelimit-reset*` hints. |
| `f.ContextErr` | Non-nil → cancel, even if the provider returned a status. |
| `now` | Injected clock time for parsing absolute date/reset hints. |

```go
func RetryAfter(headers map[string]string, now time.Time) (time.Duration, bool) // hints.go:12
```

Returns the largest valid parsed delay across all recognized headers, or `(0, false)`. Headers: `retry-after-ms` (decimal ms, ceil-rounded), `Retry-After` (decimal seconds or HTTP date), `x-ratelimit-reset-ms` and `x-ratelimit-reset` (absolute or relative, with magnitude heuristics). Invalid, non-finite, zero, negative, and expired values are skipped.

### The retrier and stream boundary

```go
// internal/retry/stream.go
type Streamer interface {                           // lines 16-18
    Stream(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream
}

type StreamFunc func(context.Context, types.Model, types.Context, *types.SimpleStreamOptions) *stream.AssistantStream // line 21

func (f StreamFunc) Stream(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream // line 24

type Retrier struct { /* private */ }               // lines 29-32

func New(next Streamer, cfg Config) (*Retrier, error) // line 35

func (r *Retrier) Stream(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream // line 46
```

| Parameter | Role |
|---|---|
| `next` | The decorated provider stream. Rejected if nil. |
| `cfg` | Copied on construction; immutable after `New`. Validated immediately. |
| `ctx` | Cancels both active provider calls and backoff sleep. Not stored on `Retrier`. |
| `opts` | Deep-copied per attempt; caller's callback is chained, never replaced. |

## How it works

### The producer loop

`Retrier.Stream` creates an output `AssistantStream` and launches `produce` as a goroutine, then returns the stream immediately ([`stream.go:45-49`](internal/retry/stream.go)). The caller (the agent loop) reads from the returned stream while `produce` manages attempts, backoff, and reconnection behind it.

`produce` ([`stream.go:59-97`](internal/retry/stream.go)):

1. **Pre-call cancel check.** If `ctx` is already done, push `CodeCanceled` and return.
2. **Attempt loop.** For `attempt := 1; ; attempt++`:
   a. Cancel check before each attempt.
   b. Clone options with an attempt-local `OnResponse` that captures a copy of the `ProviderResponse` (status + headers), then chains the caller's callback.
   c. Call `r.next.Stream(ctx, ...)`, the real provider, and hand the result to `forwardAttempt`.
   d. `forwardAttempt` ([`stream.go:103-140`](internal/retry/stream.go)) drains events in a select over `attemptStream.Events()` and `ctx.Done()`:
      - `EvDone`: push it downstream and report success.
      - `EvError`: record the event and its `Classify` result, and report the failure.
      - Any other event: `forward` it immediately with a fresh `Partial` snapshot from the retrier-owned builder.
      - Channel closed without a terminal event: synthesize `CodeStreamInterrupted` with `Retryable: true` ([`stream.go:118-119`](internal/retry/stream.go)).
   e. If the attempt reported no classification (done or canceled), return.
   f. Build the discarded attempt message (`attemptMessage`, [`stream.go:164-180`](internal/retry/stream.go)) and ask `plan` for the delay or the terminal error ([`stream.go:144-159`](internal/retry/stream.go)).
   g. Terminal: push the provider's error event unchanged when the attempt had one, otherwise `pushTerminal` with the discarded message ([`stream.go:190-198`](internal/retry/stream.go)). Return.
   h. Retryable: push `EvRetry` with the discarded message, attempt ordinal and delay; fold it into the logical builder to reset it; sleep; start the next attempt.

### Classification → retry decision → backoff

When an attempt produces an `EvError`, `forwardAttempt` builds a `Failure` from the event's stop reason, the captured status and headers, and `ctx.Err()`, then calls `Classify` with the injected clock time ([`stream.go:126-134`](internal/retry/stream.go)). `produce` passes the result to `plan` ([`stream.go:144-159`](internal/retry/stream.go)):

1. **Not retryable?** Return the classified error as the terminal outcome. `produce` forwards the provider's error event unchanged when it got one.
2. **Exhausted?** `attempt >= MaxAttempts` → return `CodeAttemptsExhausted` with the message `retry attempts exhausted after N attempts: <cause>` and `Cause` = the raw provider failure, not the retryable `*Error`.
3. **Compute delay.** `nominalDelay(base, cap, attempt)` doubles until saturating at the cap. Then `jitterDelay(nominal, jitter, random)` multiplies by `Min + (Max-Min)*sample`, defends against NaN/Inf/out-of-range samples, rounds. The final delay is `max(jittered_local, classified.RetryAfter)`: the provider hint can extend but never shorten.
4. **Fail-fast check.** If `MaxDelay > 0 && delay > MaxDelay`, return `CodeRetryDelayExceeded` with the message `retry delay <delay> exceeds the configured maximum <max>: <cause>` instead of retrying.
5. **Return the delay.** `produce` announces the retry with `EvRetry`.
6. **Sleep.** `Sleeper.Sleep(ctx, delay)`. If the sleeper returns an error or `ctx` is done, push `CodeCanceled` and return.
7. **Next attempt.** The loop calls the provider again with the same context and freshly cloned options.

### Backoff math

`nominalDelay` ([`config.go:134-146`](internal/retry/config.go)) doubles each ordinal with a saturation guard:

```
ordinal 1 → base              (500ms)
ordinal 2 → base * 2          (1s)
ordinal 3 → base * 4          (2s)
ordinal 4 → base * 8          (4s)
ordinal 5 → cap               (8s)
ordinal N ≥ 5 → cap           (8s)
```

If `delay > cap/2`, the next iteration would overflow, so it returns `cap` immediately.

`jitterDelay` ([`config.go:148-164`](internal/retry/config.go)) multiplies by a factor in `[Jitter.Min, Jitter.Max]` (default `[0.75, 1.0]`), rounds, and returns a `time.Duration`. Defensive: NaN/negative samples clamped to 0, >1/infinite clamped to 1. On a zero or negative result, returns 0 (the backoff then becomes the provider hint or stays minimal).

### Hint parsing and rounding

`RetryAfter` ([`hints.go:12-35`](internal/retry/hints.go)) iterates all headers case-insensitively, parses each recognized name, and keeps the largest valid delay.

`durationCeilMillis` ([`hints.go:86-96`](internal/retry/hints.go)) is the rounding workhorse:

```go
millis := value * (float64(unit) / float64(time.Millisecond))
// NaN, <= 0 → reject
millis = math.Ceil(millis)
// >= MaxInt64/millis → reject (overflow guard)
return time.Duration(millis) * time.Millisecond
```

For reset headers, a magnitude heuristic disambiguates relative vs absolute: `> 1e12` → absolute Unix ms, `> 1e9` → absolute Unix s, smaller → relative. Absolute times are converted to instants and subtracted from the injected clock.

## Failure modes and invariants

### What the code assumes

- **The frozen providers call `OnResponse` before turning a non-2xx into an error.** The decorator installs a per-attempt callback that captures status and headers. If a provider ever skips `OnResponse` on failure, the classifier sees `Status: 0` and `Headers: nil` and falls through to the provider-code and message-pattern tiers. Network errors (TCP resets, TLS failures) reach this path naturally — they have no HTTP status.
- **The agent loop treats a terminal `EvError` as the end of the run, and `EvRetry` as a boundary inside it.** A retryable failure, before or after output, is announced as `EvRetry`. Events already forwarded remain valid for the attempt that produced them, and the loop discards that attempt at message granularity. Only a non-retryable provider error, or a generated terminal such as exhaustion or cancellation, reaches the loop as `EvError`. Re-sending after output is safe because nothing acts on a response before its terminal event: tool calls run only after the final message.
- **`Classify` returns a non-nil `*Error` with a populated `Code` and deterministic `Retryable`.** There is no "unknown" or "maybe" outcome. The terminal fallback is `CodeProviderFailure` with `Retryable: false`.
- **Exhaustion does not unwrap to a retryable error.** `plan` constructs `CodeAttemptsExhausted` with `Cause: classified.Cause` (the raw provider text), not `Cause: classified` (the retryable `*Error`). So `errors.Is(exhaustedErr, ErrRetryable)` is false.
- **A stream close without a terminal event is retryable at any point.** `forwardAttempt` synthesizes `CodeStreamInterrupted` with `Retryable: true` and the message `provider stream ended without terminal event` ([`stream.go:118-119`](internal/retry/stream.go)). Output produced before the close does not change the outcome: the attempt is announced as discarded and the request is re-sent.

### Edge cases and defenses

- **Context overflow with 429 status.** Checked *before* the 429 check. A 429 with message "context length exceeded" is `CodeContextOverflow` (terminal), not `CodeRateLimited` (retryable). The overflow check combines `providerCode` and `message` so a structured code like `context_length_exceeded` also triggers.
- **Non-terminal event with content only in `Partial`.** `forward` folds owned fields (`Delta`, `Content`, `ToolCall`, `ContentIndex`) into the retrier's builder, then attaches the rebuilt snapshot as `Partial` ([`stream.go:293-302`](internal/retry/stream.go)). Content that exists only in the provider's live `Partial`, such as the `"[Reasoning redacted]"` text of an Anthropic `redacted_thinking` start event, is therefore absent from the forwarded snapshot. It still reaches the caller in the terminal `EvDone.Message`, which `copyEvent` deep-copies. Module 07 covers the ownership reason and the residual trade-off.
- **Huge positive hints.** `durationCeilMillis` bounds-checks in millisecond units before multiplying into `time.Duration`. A hint that would overflow is rejected as invalid rather than wrapping to negative. `absoluteDelay` applies the same guard: `nanos >= float64(math.MaxInt64)` → invalid.
- **Config millisecond overflow.** `maxRetryDurationMS` in the config layer rejects any positive millisecond value that can't convert to nanoseconds without overflow. The CLI converts only already-validated values.
- **Jitter sample defense.** If an injected `Random` returns NaN, negative, or >1, `jitterDelay` clamps to valid bounds. The delay stays within `[Min, Max]` of nominal.
- **Nil `next` streamer.** `New` rejects it at construction time, before any session starts.
- **`ctx.Done()` during event drain.** The select loop includes `ctx.Done()`. If the context is cancelled while the producer is draining an active provider stream, it pushes `CodeCanceled` and returns — no stall.
- **`ctx.Done()` after sleep.** After `Sleeper.Sleep` returns, the code checks `ctx.Err()` again. If the sleep completed normally but the context was cancelled during the sleep window, one `CodeCanceled` is pushed and no next attempt starts.

### Races (summary; module 07 details)

The `Partial` pointer on forwarded events is a retrier-owned snapshot, not the provider's live builder pointer. Without this, the provider goroutine could mutate `Partial` while the agent loop reads it — a data race caught under `-race`. Module 07 covers the full ownership discipline.

## TypeScript to Go

### Errors: `instanceof` and string matching vs typed sentinels

In TypeScript:

```typescript
// Reference pattern: catch, instanceof, string matching
try {
    const res = await fetch(...)
    if (res.status === 429) {
        const after = res.headers.get("retry-after")
        // string → number conversion, manual setTimeout
        throw new RateLimitError(after)
    }
} catch (e) {
    if (e instanceof RateLimitError) { /* retry with e.retryAfter */ }
    else if (e.message?.includes("context_length_exceeded")) { /* terminal */ }
    else if (e instanceof AxiosError && e.code === "ECONNRESET") { /* retry */ }
    // ad-hoc string matching on e.message
}
```

Problems: no exhaustiveness check, string matching is fragile across provider versions, `instanceof` breaks across npm duplicates, and `e.message` may contain user request data.

In Go:

```go
// Classification is pure, centralized, and deterministic
classified := retry.Classify(retry.Failure{
    Message:      errMsg,
    ProviderCode: providerCode,
    Status:       resp.Status,
    Headers:      resp.Headers,
    ContextErr:   ctx.Err(),
}, clock.Now())

// Callers branch on typed sentinels
if errors.Is(classified, retry.ErrRetryable) {
    // apply backoff
} else if errors.Is(classified, retry.ErrCanceled) {
    // context done
}
// Or inspect the code directly
var re *retry.Error
if errors.As(err, &re) && re.Code == retry.CodeContextOverflow {
    // compaction territory
}
```

Why Go forces this:

- No exceptions. Every error is a value returned on the stack. There's no `catch` block to intercept arbitrary failures — the retry decorator must see the error before it propagates.
- `errors.Is` and `errors.As` walk the `Unwrap` chain. The sentinel values (`ErrRetryable`, `ErrTerminal`) are stable identities, not string comparisons. Adding a new error code doesn't break callers that match on sentinels.
- `%w` wrapping preserves the original cause. `Unwrap()` gives callers the raw provider text without the retrier discarding information.

### Time: `Date.now()` and `setTimeout` vs injected interfaces

In TypeScript:

```typescript
const delay = Math.max(
    baseDelay * Math.pow(2, attempt) * (0.75 + Math.random() * 0.25),
    parseRetryAfter(headers)
)
await new Promise(resolve => setTimeout(resolve, delay))
```

Tests are flaky: `Math.random()` is uncontrollable, `Date.now()` drifts, and `setTimeout` is real wall-clock time. You can mock `Date` and `setTimeout` in Jest, but the mock bleeds across tests and requires careful cleanup.

In Go:

```go
type Clock interface { Now() time.Time }
type Sleeper interface { Sleep(ctx context.Context, delay time.Duration) error }
type Random interface { Float64() float64 }

// Production:
DefaultConfig() // realClock{}, timerSleeper{}, lockedRandom{}

// Test:
Config{
    Clock:   fixedClock(t),
    Sleeper: recordingSleeper{},
    Random:  sequenceRandom{0.5, 0.5, 0.5},
}
```

Why Go rewards this:

- Interfaces are satisfied implicitly. A test can define a one-method struct without importing any mocking library. No framework, no global mock state.
- The injected `Sleeper` takes `context.Context`. If a test wants to assert cancellation, it just cancels the context and checks that `Sleep` was never called (or was called and returned the context error).
- `Random` returns `float64`. A deterministic sequence (`0.0`, `0.5`, `1.0`) reproduces exact boundary behavior. No seed management.

### Backoff math: JavaScript number drift vs saturating integer arithmetic

In TypeScript:

```typescript
let delay = baseDelay
for (let i = 1; i < attempt && delay < cap; i++) {
    delay *= 2
}
// delay is a JS number (IEEE 754 double). For large bases, double-precision
// rounding kicks in, but in practice nobody hits it with millisecond values.
```

In Go:

```go
func nominalDelay(base, cap time.Duration, ordinal int) time.Duration {
    delay := base
    for i := 1; i < ordinal && delay < cap; i++ {
        if delay > cap/2 { return cap }  // saturate, don't overflow
        delay *= 2
    }
    if delay > cap { return cap }
    return delay
}
```

Why Go demands this:

- `time.Duration` is `int64` nanoseconds. A base delay of 500ms doubled 60 times overflows. The `delay > cap/2` guard prevents overflow by saturating early — the next double would exceed the cap anyway.
- The saturating pattern (`if delay > cap/2 { return cap }`) is idiomatic Go for "double until you can't." It never produces an intermediate value larger than `cap`.
- `jitterDelay` rounds via `math.Round` back to integer nanoseconds. No floating-point duration leaks into the sleep call.

### Stream ownership: event loop vs goroutine

In TypeScript (single-threaded event loop):

```typescript
async function* retryStream(inner: AsyncGenerator<Event>) {
    for (let attempt = 1; ; attempt++) {
        try {
            for await (const event of inner()) yield event
            return
        } catch (err) {
            if (attempt >= max) throw err
            yield { type: "retry", attempt, delay, err }
            await sleep(delay)
        }
    }
}
```

Yielding from an async generator is cooperative. The caller pulls events one at a time. Events are yielded as they arrive; a failed attempt is announced with an explicit `retry` event, and the retry decision does not depend on how much output was already shown. There's no concurrent mutation of `event.Partial` because the event loop serializes everything.

In Go:

```go
func (r *Retrier) Stream(...) *stream.AssistantStream {
    output := stream.NewAssistantStream(...)
    go r.produce(ctx, output, ...)    // goroutine!
    return output                      // caller reads from output
}
```

The producer runs in its own goroutine. The caller reads from a channel. Two concurrent goroutines share the event data, so the retrier must *copy* events before forwarding, and must own a private `AssistantBuilder` for the logical snapshot. It also cannot lean on a generator's call stack: once an event is pushed to the output stream it is gone, so a re-sent attempt has to be announced explicitly with `EvRetry` carrying the discarded message (`stream.go:89-91`). Module 07 covers why `event.Partial = nil` and the retrier-owned builder are necessary; the policy lesson here is that Go's concurrency forces explicit ownership boundaries where TypeScript's event loop hides them.

## Where it lives

| What | Where |
|---|---|
| `Config`, `Clock`, `Sleeper`, `Random`, `Jitter`, `DefaultConfig`, `Validate`, `nominalDelay`, `jitterDelay` | `internal/retry/config.go` |
| `Error`, `Code`, sentinels (`ErrRetryable` etc.), `Failure`, `Classify`, `classifyProviderCode`, `isContextOverflow` | `internal/retry/error.go` |
| `RetryAfter`, `durationCeilMillis`, header parsing (`retryAfterSecondsOrDate`, `resetDelay`, `absoluteDelay`, `decimalDuration`) | `internal/retry/hints.go` |
| `Streamer`, `StreamFunc`, `Retrier`, `New`, `Stream`, `produce`, `forwardAttempt`, `plan`, `attemptMessage`, `forward`, `copyEvent`, `cloneOptions`, defensive copy helpers | `internal/retry/stream.go` |
| `RetrySettings`, `maxRetryDurationMS`, config resolution, env/settings/flag precedence | `internal/config/config.go` |
| Wiring: `NewProviderRouter` decorates `StreamFn` with `retry.New` after credential binding | `internal/adapter/provider.go:105-110` |
| Wiring: `RoleRoutingConfig.Retry` propagates to each role's router | `internal/adapter/roles.go` |
| Go design rules (DI, accept-interface/return-struct, typed errors) | `12-idiomatic-go-vs-typescript.md` |
| Concurrency hazard deep-dive (live `Partial`, ownership discipline) | Module 07 |
