# 07 — Concurrency and race conditions

## The problem

The harness streams model output through a chain of goroutines: the provider decodes SSE, the retry decorator forwards, the agent loop folds the result. Every link shares pointer-typed data. If goroutines operate on the same memory without synchronization, the result is a data race — nondeterministic, untestable without `-race`, and capable of producing silently wrong output that passes visual inspection.

We shipped one. The retry decorator received a `StreamEvent` whose `Partial` field pointed into the provider's live `AssistantBuilder`. The provider goroutine was still mutating that builder (appending content blocks, writing text deltas) while the retry decorator read `Partial` to build its logical snapshot. The Go race detector fired at `internal/retry/stream.go` `produce` / `forward`. The bug was not hypothetical: on a fast dispatch goroutine and an unbounded queue, the read and write genuinely overlapped.

The problem is not "retry is broken." The problem is that the `StreamEvent` type is an **ownership leak**: one goroutine (the provider) writes a struct it owns, and another goroutine (the consumer) receives a pointer into that struct's interior with no documented lifetime and no copy. The Go memory model does not protect you here. You get a race.

## Key decisions and the thinking process

### 1. The stream is single-producer, multi-consumer, unbounded

`internal/engine/stream/stream.go` models a generic `Stream[T, R]` as an **unbounded in-memory queue** drained by a dedicated `dispatch` goroutine. The producer calls `Push`; the queue grows via `append`. The `dispatch` goroutine pops from the front and sends on `s.events`. Consumers call `Events()` to get `<-chan T` or `Result(ctx)` to block for the terminal value.

**Why unbounded?** Module 12 (idiomatic Go vs TypeScript) says "slow subscribers must have an explicit policy: apply backpressure, disconnect, or use a bounded persisted replay. Never allow unbounded queues." The frozen engine predates this rule and uses an unbounded queue. The retry layer adds no second queue and no event buffer: it forwards each event as it arrives (`internal/retry/stream.go:293-302`) and keeps only the current attempt's logical message. Changing the frozen stream queue is out of scope.

**Why a dedicated dispatch goroutine?** The producer's `Push` never blocks on a slow consumer. The dispatch goroutine is the single sequencer: it drains the queue under a mutex, releases the lock, then sends. This means `Push` latency is `O(1)` append + signal, and consumers see events in order. The trade-off: the dispatch goroutine can run **ahead** of the producer, especially when the queue is empty. The producer pushes, releases the lock, and continues; the dispatch goroutine wakes, pops, and sends — all while the producer is already building the next event. This is the structural condition that made the `Partial` race possible.

### 2. Close-based happens-before for `Result()`, not a mutex

`Stream.Result(ctx)` blocks on `<-s.done` and then reads `s.result` and `s.haveResult` under a mutex (`internal/engine/stream/stream.go:132-156`). The mutex here protects the read of `result` and `haveResult` against concurrent `End`/`EndWith` calls, but the **visibility** of those writes to a blocked `Result` caller is guaranteed by the Go memory model's channel close rule:

> A close of a channel happens-before a receive from that channel that returns the zero value.

`finishLocked` writes `s.result`, `s.haveResult`, and then calls `close(s.done)` — in that order, under the mutex. The `Result` goroutine, blocked on `<-s.done`, observes the close. Because the close happens-before the receive, and the writes happen-before the close (program order within the locked critical section), the receive happens-after the writes. No separate mutex acquisition is needed for the happens-before edge; the mutex inside `resultLocked` guards only against a concurrent `End` call on an already-unblocked `Result`.

**Why this pattern?** It is the idiomatic Go way to signal "one-shot completion with a value." The channel close is the signal; the value is written before the close. The mutex prevents races between multiple callers of `End`/`EndWith`, not between the writer and the blocked reader. This is the same pattern `context.Context.Done()` uses.

### 3. The live-Partial race and why we must never read `event.Partial`

Here is the chain that produced the race:

1. The provider (e.g., Anthropic) calls `foldContentBlockStart` at `internal/provider/anthropic/events.go:184-214`. This function appends a `ContentBlock` to `state.Output.Content` (the live `AssistantBuilder`'s message) and returns `[]types.StreamEvent{{Type: types.EvThinkingStart, ContentIndex: contentIndex, Partial: output}}`. The `Partial` field **points directly at `state.Output`** — the same `*AssistantMessage` the provider continues mutating.

2. The provider pushes this event into the `AssistantStream`. The stream's `dispatch` goroutine dequeues it and sends it on the events channel.

3. The retry decorator's `produce` goroutine receives the event. At `internal/retry/stream.go:121`, it calls `copyEvent(event)`, which at line 249 does `event.Partial = nil`. **But before the fix**, the original code read `event.Partial` to decide whether the attempt had produced output.

4. Meanwhile, the provider goroutine is already processing the next SSE frame — maybe a `content_block_delta` that mutates `state.Output.Content[0].Thinking`. The `Partial` pointer the retry decorator held **now points to memory being written by another goroutine**.

The minimal illustration:

```
Provider goroutine              Retry produce goroutine
────────────────────            ─────────────────────────
output.Content = append(        event = <-attemptStream.Events()
  output.Content,               // event.Partial == &output
  ContentBlock{Thinking:        //   (live pointer into provider's builder)
    "[Reasoning redacted]"})
Push(StreamEvent{
  Type: EvThinkingStart,        read := event.Partial.Content[0].Thinking
  Partial: output})  // ← LIVE  //   ↑ DATA RACE: provider writes Content[0]
                                //     while retry reads it
foldContentBlockDelta(...)
  output.Content[0].Thinking
    += event.Delta.Text  // ← WRITE
```

The dispatch goroutine's ability to run ahead on the unbounded queue means there is no implicit "the producer has paused" moment. The provider pushes, the dispatch goroutine delivers, the retry decorator reads — all while the provider's next SSE frame is being decoded and folded.

### 4. The fix: never read `Partial`; rebuild from owned fields

The fix has three parts:

**Part A — `copyEvent` nils `Partial`** (`internal/retry/stream.go:248-249`):

```go
func copyEvent(event types.StreamEvent) types.StreamEvent {
    event.Partial = nil   // sever the live pointer immediately
    event.Message = cloneMessage(event.Message)
    event.Err = cloneMessage(event.Err)
    // ... deep-copy ToolCall
    return event
}
```

This is a defensive cut: the pointer is nilled before any code path can read it. If someone later adds a read of `event.Partial` between `copyEvent` and `forward`, they get nil, not a race.

**Part B — `forward` rebuilds `Partial` from the Retrier's own builder** (`internal/retry/stream.go:292-302`):

```go
func forward(output *stream.AssistantStream, logical *types.AssistantBuilder, event types.StreamEvent) {
    event.Partial = nil                  // belt and suspenders
    logical.Fold(event)                  // fold owned Delta/Content/ToolCall fields
    event.Partial = cloneMessagePtr(logical.Message())  // snapshot the accumulated message
    output.Push(event)
}
```

The Retrier owns its own `logical` `AssistantBuilder` (`internal/retry/stream.go:60`). `Fold` reads only the **owned** fields of the event — `Delta`, `Content`, `ToolCall`, `ContentIndex` — never `Partial`. After folding, `logical.Message()` returns the accumulated message. `cloneMessagePtr` deep-copies it (content blocks, argument bytes) into a fresh `*AssistantMessage`. This snapshot becomes the event's new `Partial`, owned by the output stream, not by any provider goroutine.

**Part C — no retry decision reads event content at all.** `forward` (`internal/retry/stream.go:292-302`) is the only place content events from an attempt enter the output stream, and it folds owned fields only. Whether an attempt is retried depends solely on the classification of its terminal event, computed in `forwardAttempt` (`internal/retry/stream.go:126-134`). Terminal events (`EvDone`, `EvError`) carry their own `Message`/`Err` fields, owned copies, and those are safe to inspect.

### 5. The remaining trade-off: `redacted_thinking` in the streamed `Partial`

The rule is absolute: no consumer reads `Partial`. One narrow, cosmetic gap remains in what the streamed snapshot shows.

At `internal/provider/anthropic/events.go:196-197`, the Anthropic provider emits `EvThinkingStart` for `redacted_thinking`:

```go
case "redacted_thinking":
    output.Content = append(output.Content, types.ContentBlock{
        Type: BlockThinking, Thinking: "[Reasoning redacted]",
        ThinkingSignature: event.ContentBlock.Data, Redacted: true,
    })
    state.Blocks = append(state.Blocks, foldBlock{...})
    return []types.StreamEvent{{Type: types.EvThinkingStart, ContentIndex: contentIndex, Partial: output}}
```

This event's **only** non-empty content is the `"[Reasoning redacted]"` string, which lives in `Partial.Content[contentIndex].Thinking`. The event's owned fields (`Delta`, `Content`, `ToolCall`) are all empty: it is a start event, not a delta or end. `forward` folds that event into the retrier's builder, which creates an empty thinking block, and attaches the rebuilt snapshot. The streamed start event therefore carries no redacted marker.

**What is actually lost?** Only the streamed snapshot of that one start event. The terminal `EvDone.Message` carries the full accumulated `AssistantMessage`, including the `"[Reasoning redacted]"` thinking block, and `copyEvent` deep-copies it. The content is preserved at **message** granularity. No user-visible corruption or duplication occurs. Retry no longer turns this into a lost event either: an attempt that fails is discarded as a whole message through `EvRetry`, never replayed event by event.

**Why isn't this a simple fix?** There are two worse alternatives:

1. **Read the live `Partial`**: the proven race, non-negotiable.
2. **Edit the frozen provider** to emit a delta event with owned content for `redacted_thinking`: requires a frozen-path exception. The design explicitly defers this; the frozen constraint stands for V1.

The **proper long-term fix** is an immutable event boundary at the producer: the provider should emit events whose `Partial` is an owned copy, a snapshot taken before `Push`, not a live pointer into the builder the provider continues to mutate. This would give the consumer a race-free `Partial` it can safely read. Until that boundary exists, the rule stays: never read `Partial`; rebuild from owned fields.

### 6. Goroutine ownership discipline

The harness follows a one-writer-per-builder rule:

- The **provider** owns its `foldState.Output` (`*AssistantMessage`). It writes content blocks, deltas, and usage into it. It is the sole writer.
- The **retry decorator** owns its `logical` `*AssistantBuilder`. No other goroutine touches it. `forward` folds events into it and snapshots the result. `EvRetry` folds into the same builder as a reset (`internal/engine/types/stream.go:114-115`), so the next attempt starts from an empty message.
- The **agent loop** receives events and folds them into its own builder. It never reads a provider's internal state.

Consumers get **clones, not references**: `cloneMessage` deep-copies `AssistantMessage` including its `Content` slice and each block's `Arguments` bytes (`internal/retry/stream.go:260-290`). A consumer mutating its clone cannot affect the producer.

### 7. Context cancellation as the universal stop signal

Every blocking operation in the retry path selects on `ctx.Done()`:

- **Before the first attempt** (`internal/retry/stream.go:62-65`): if already canceled, push a `CodeCanceled` terminal and return.
- **During attempt drain** (`internal/retry/stream.go:108-112`): `select` between `attemptStream.Events()` and `ctx.Done()`. The retry producer terminates even if a provider ignores cancellation and leaves its event channel open.
- **During sleep** (`internal/retry/config.go:63-68`): `timerSleeper.Sleep` selects between `timer.C` and `ctx.Done()`. On cancellation, it returns `ctx.Err()` immediately. The deferred cleanup stops and drains the timer channel to prevent a goroutine leak.

The `timerSleeper` drain pattern (`internal/retry/config.go:55-61`) is worth studying:

```go
defer func() {
    if !timer.Stop() {
        select {
        case <-timer.C:
        default:
        }
    }
}()
```

`timer.Stop()` returns `false` if the timer already fired. In that case, the channel `timer.C` has a value waiting. The `select` with a `default` drains it without blocking. Without this drain, a stopped-but-unread timer channel could retain the timer's underlying runtime resources. This is the standard Go idiom for context-aware timers.

### 8. `lockedRandom`: why the shared RNG needs a mutex

`DefaultConfig()` creates one `*rand.Rand` seeded with `time.Now().UnixNano()` (`internal/retry/config.go:84-95`). This single RNG is shared across **all concurrently running role streams** — the default, plan, and smol model roles can each have an active retry producer calling `Random.Float64()` for jitter.

`math/rand.Rand` is not safe for concurrent use. Its `Float64()` method reads and writes internal state (the PCG generator's state). Two goroutines calling `Float64()` on the same `*rand.Rand` without synchronization is a data race.

The fix is `lockedRandom` (`internal/retry/config.go:72-81`):

```go
type lockedRandom struct {
    mu sync.Mutex
    r  *rand.Rand
}

func (r *lockedRandom) Float64() float64 {
    r.mu.Lock()
    defer r.mu.Unlock()
    return r.r.Float64()
}
```

The mutex is held only for the duration of one `Float64()` call — nanoseconds. Jitter is sampled once per retry sleep, which happens at most 10 times per stream over seconds. The contention is negligible.

**Why not `rand.New(rand.NewPCG(seed, ...))` per `Retrier`?** Each `Retrier.Stream` call creates a producer goroutine with its own local attempt state, but the `Random` is a field of `Config`, which is shared. Making a new RNG per stream would require either a new `Config` per stream (breaking the shared-config model) or a `Random` factory (adding complexity for a non-problem). The mutex is simpler and sufficient.

## Signatures and types

### Stream

```go
func New[T, R any](buf int, isComplete func(T) bool, extract func(T) R) *Stream[T, R]
```
- `buf` — channel buffer size for `events`; the unbounded queue is separate.
- `isComplete` — predicate: does this event terminate the stream?
- `extract` — derives the final `R` from a completing event.

```go
func (s *Stream[T, R]) Push(ev T)
```
Appends `ev` to the unbounded queue. If `isComplete(ev)`, calls `finish` once. Drops events after `End`/`EndWith`.

```go
func (s *Stream[T, R]) End()
func (s *Stream[T, R]) EndWith(result R)
```
Close the stream. `EndWith` stores a result for `Result()` to return. Both are idempotent (first call wins via `sync.Once`).

```go
func (s *Stream[T, R]) Events() <-chan T
```
Returns the receive-only channel. Range over it; it closes after the stream ends and the queue drains.

```go
func (s *Stream[T, R]) Result(ctx context.Context) (R, error)
```
Blocks until a result is available or `ctx` is canceled. Returns `ErrNoResult` when the stream ended via `End()` (no completing event).

```go
var ErrNoResult = errors.New("stream ended without a result")
```

### Retrier

```go
type Streamer interface {
    Stream(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream
}
```
Consumer-defined interface (`internal/retry/stream.go:16`). The retry package owns this boundary; providers implement it indirectly via `StreamFunc`.

```go
type StreamFunc func(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream
func (f StreamFunc) Stream(...) *stream.AssistantStream
```
Adapter so a bare function satisfies `Streamer`.

```go
func New(next Streamer, cfg Config) (*Retrier, error)
```
Validates `next != nil` and `cfg.Validate()` before returning a retrier. Accept-interface, return-struct.

```go
func (r *Retrier) Stream(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream
```
Returns an output stream immediately. Spawns one producer goroutine that manages attempts and forwarding. The caller receives events through the returned stream.

### Config dependencies

```go
type Clock interface {
    Now() time.Time
}
```
Injected time source. Production: `realClock{}`. Tests: fake clock.

```go
type Sleeper interface {
    Sleep(ctx context.Context, delay time.Duration) error
}
```
Context-aware sleep. Production: `timerSleeper{}` selects on `timer.C` and `ctx.Done()`. Tests: fake sleeper that records requested delays.

```go
type Random interface {
    Float64() float64
}
```
Contract: returns a value in `[0, 1)`. Production: `lockedRandom` wraps a mutex-protected `*rand.Rand`. Tests: deterministic sequence source.

### Key internal helpers

```go
func copyEvent(event types.StreamEvent) types.StreamEvent
```
Deep-copies `Message`, `Err`, and `ToolCall`. Nils `Partial`. The caller receives an event with no live pointers into any provider.

```go
func forward(output *stream.AssistantStream, logical *types.AssistantBuilder, event types.StreamEvent)
```
Folds event's owned fields into `logical`, snapshots the builder message, assigns to `event.Partial`, and pushes. The single point where an attempt's content events enter the output stream.

```go
func (r *Retrier) forwardAttempt(...) (types.StreamEvent, *Error)
```
Drains one attempt, forwards its non-terminal events, and returns the attempt's terminal error event plus its classification. A nil classification means a terminal event was already pushed (success or cancellation).

```go
func (r *Retrier) plan(attempt int, classified *Error) (time.Duration, *Error)
```
Returns the backoff before the next attempt, or the terminal error that ends the stream (non-retryable, exhausted, or a delay over `MaxDelay`).

```go
func attemptMessage(model types.Model, logical *types.AssistantBuilder, providerMessage *types.AssistantMessage, classified *Error) *types.AssistantMessage
```
Builds the discarded attempt's message for `EvRetry`: the provider's terminal message or the forwarded content, marked as an error with the classified code and message.

## How it works

### The stream: Push → queue → dispatch → events channel

```
Producer goroutine          dispatch goroutine         Consumer goroutine
─────────────────          ──────────────────         ──────────────────
Push(ev)                    
  mu.Lock()                 
  queue = append(queue, ev) 
  cond.Signal()             
  mu.Unlock()               
                            mu.Lock()
                            wait for queue or closed
                            ev = queue[0]
                            shift queue left
                            mu.Unlock()
                            events <- ev  ──────────>  for ev := range Events()
```

The producer never blocks on a slow consumer. The dispatch goroutine is the single point of sequentialization. If the consumer is slow, events accumulate in the unbuffered `events` channel send, which blocks the dispatch goroutine, which holds no lock, so `Push` continues unimpeded.

### The retry decorator: one logical stream, multiple attempts

```
Retrier.Stream(ctx, model, c, opts)
  │
  ├─ output := NewAssistantStream(...)
  ├─ go produce(ctx, output, ...)
  └─ return output
```

Inside `produce`:

1. Check `ctx.Err()`. If canceled, push `CodeCanceled` and return.
2. Create `logical := NewAssistantBuilder(...)`, the Retrier-owned accumulator.
3. **Attempt loop:**
   - Clone options, install `OnResponse` capture callback.
   - Call `next.Stream(ctx, model, c, attemptOpts)`, the actual provider call.
   - **Drain loop** in `forwardAttempt` (`select` on `attemptStream.Events()` and `ctx.Done()`):
     - Receive event → `copyEvent(event)` (nils `Partial`, deep-copies owned fields).
     - **`EvDone`**: push it downstream, report success.
     - **`EvError`**: classify; report the event as the attempt's terminal failure.
     - **Non-terminal**: `forward(output, logical, event)` immediately.
     - **Channel closed without a terminal event**: synthesize a retryable `CodeStreamInterrupted`.
   - If there was no classification (done or canceled), return.
   - Build the discarded message (`attemptMessage`), ask `plan` for a delay or a terminal error.
   - On terminal failure: push the provider's error event unchanged, or `pushTerminal`.
   - On retry: push `EvRetry` (discarded message, attempt ordinal, delay), reset `logical`, sleep via `Sleeper`, continue the loop.

### Before/After: the `Partial` race fix

**Before (raced):** Consumer reads `event.Partial` — a pointer into the provider's live builder. Provider goroutine is concurrently mutating that builder. Data race.

**After (race-free):** `copyEvent` nils `Partial`. `forward` rebuilds it from the Retrier's own `logical` builder using only owned event fields (`Delta`/`Content`/`ToolCall`/`ContentIndex`). The provider and consumer never touch the same memory.

## Failure modes and invariants

### The live-Partial race (shipped and fixed)

**What failed:** `StreamEvent.Partial` is a `*AssistantMessage` pointing into the provider's `foldState.Output`. The provider goroutine mutates it (appending blocks, writing deltas, accumulating thinking text) after pushing the event. A consumer receiving the event and reading `Partial` races with the provider's next write. The Go race detector confirms this at `internal/retry/stream.go` `produce`/`forward`.

**Root cause:** The `StreamEvent` type has no ownership contract. Its doc comment at `internal/engine/types/stream.go:29-35` says "Partial (a live pointer to the builder's message — read-only until the next event)" but this is a **convention, not a synchronization mechanism**. "Read-only until the next event" assumes the consumer reads before the producer emits the next event, an assumption the unbounded dispatch queue does not guarantee.

**Fix:** Never read `Partial`. `copyEvent` nils it. `forward` rebuilds from owned fields. Retry decisions come from the attempt's terminal event only.

**Residual trade-off:** `redacted_thinking` emits `EvThinkingStart` whose only content lives in `Partial`, so the streamed snapshot of that start event lacks the redacted marker while the terminal `EvDone.Message` retains it. The proper fix is an immutable event boundary at the producer, deferred per the frozen constraint.

### Unbounded queue (known limitation)

The frozen `engine/stream.Stream` uses an unbounded in-memory queue (`internal/engine/stream/stream.go:65`). A fast producer with a blocked consumer can exhaust memory. Changing this is out of scope for the retry layer. The retry layer keeps no independent event queue: it holds one attempt's event at a time and forwards it (`internal/retry/stream.go:293-302`).

### dispatch goroutine lifecycle

`New` starts `dispatch` in a goroutine (`internal/engine/stream/stream.go:52`). `dispatch` exits only when the queue is empty and `s.closed` is true. If `Push` never calls `End`/`EndWith`, `dispatch` blocks forever on `cond.Wait()`. Every code path in the harness eventually calls `End` or `EndWith` on a stream; a leaked stream would leak a goroutine.

### Once-guarded finish

`finish` uses `sync.Once` (`internal/engine/stream/stream.go:88`). The first call to `Push` with a completing event, `End`, or `EndWith` wins. Subsequent calls are no-ops. This means: if a completing event arrives via `Push`, a later `End` call does not overwrite the result.

### Cancellation during sleep

`timerSleeper.Sleep` selects between `timer.C` and `ctx.Done()`. On cancellation, it returns `ctx.Err()`. The deferred cleanup stops and drains the timer. Without the drain (`internal/retry/config.go:56-60`), a timer that fired between `Stop()` returning false and the drain would leave a value in `timer.C`, and the channel (and its underlying runtime timer) would not be collected until the channel itself is GC'd. This is the standard Go idiom.

### Cancellation during attempt drain

The `select` at `internal/retry/stream.go:108-112` includes `ctx.Done()`. If the context is canceled while the producer is blocked waiting for the next event, the producer pushes `CodeCanceled` and returns. It does not leak behind an unresponsive provider.

### Shared RNG with mutex

`lockedRandom` protects a single `*rand.Rand` shared across concurrent role streams. Without the mutex, concurrent `Float64()` calls race on the PCG generator's internal state. Jitter values could become correlated or deterministic in ways the race detector does not catch (the race is on internal fields, not on the returned value). The mutex prevents both the data race and ensures correct random sequences.

### Retry after output: `EvRetry` and the discarded message

`produce` may re-send a request after events from the failed attempt were already forwarded (`internal/retry/stream.go:78-95`). It first builds the discarded attempt's message (`attemptMessage` at `stream.go:164-180`): the provider's terminal message when it sent one (with partial content and reported usage), otherwise the content forwarded so far. It pushes `types.EvRetry` with that message in `Err`, the ordinal in `Attempt`, and the delay in `RetryDelay`, and folds the event into the retrier's builder, which resets the message (`internal/engine/types/stream.go:114-115`). The agent loop turns `EvRetry` into a `message_end` for the errored attempt and appends it to the run's messages through `discarded` (`internal/agent/loop.go:796-813`, `internal/agent/loop.go:452-458`), so the journal keeps a metered record of the failed call. The next attempt's `EvStart` opens a fresh message. Nothing acts on the discarded attempt: tool calls run only after the final message, and transform, estimation and compaction skip errored assistant messages.

### Events-after-close

`Push` drops events when `s.closed` is true (`internal/engine/stream/stream.go:58-62`). The provider may push events after the stream is ended (e.g., the provider's SSE reader finishes after the terminal event was already pushed by a timeout). These are silently discarded.

## TypeScript to Go

### Single-threaded event loop vs. true parallelism

In TypeScript (Node.js / Bun), the event loop runs one callback at a time. If a provider emits an event object and the consumer receives it, the provider's next SSE frame cannot be processed until the consumer's callback returns. There is no shared-memory data race because there is no concurrent execution. The hazard in TypeScript is **aliasing**: if the consumer stores a reference to the event object and the provider reuses that object (mutating it for the next event), the stored reference now points to mutated data. This is a logical bug, not a data race, and `--race` does not exist.

In Go, goroutines run in true parallel on multiple OS threads. Two goroutines reading and writing the same memory location without synchronization is a data race — undefined behavior at the language level. The Go race detector (`go test -race`) instruments every memory access and reports conflicting reads and writes.

### Structural typing vs. explicit interfaces

TypeScript's `StreamEvent` would be a discriminated union (`type StreamEvent = { type: "text_delta"; delta: string; partial: AssistantMessage } | ...`). The `partial` field is always there for non-terminal arms; TypeScript's type narrowing makes it convenient to access. But there is no language-level concept of "this pointer is live; do not read after yielding."

In Go, `StreamEvent` is a flat struct with optional fields. The `Partial *AssistantMessage` field is a pointer — Go makes the aliasing explicit because you see the `*`. This is a double-edged sword: it makes the sharing visible, but it does not prevent the sharing. The discipline must be enforced by convention and verified by `-race`.

### `-race` is the verification tool

The TypeScript ecosystem has no equivalent to Go's race detector. In Go, `go test -race` is a standard part of the test suite for any concurrent code. It instruments every memory access (reads and writes) and reports when two goroutines access the same memory location without synchronization, where at least one access is a write. The `-race` flag should be run on every subsystem that manages goroutines: streams, retry, sessions, tools, providers.

### Callback hazards vs. channel ownership

TypeScript streaming APIs typically use async iterators (`for await (const event of stream)`) or event emitters. The consumer gets events one at a time, and backpressure is implicit in the iterator pattern.

Go uses channels. The producer creates and closes the channel. Receivers range over it. The ownership is explicit: only the producer may close. This maps well to the single-producer nature of a streaming model response, but it means consumers must not assume the producer has paused between events. The dispatch goroutine pattern means the producer and consumer are genuinely concurrent.

### Dependency injection for testability

Both TypeScript and Go support dependency injection, but Go makes it structural: interfaces are satisfied implicitly. `Clock`, `Sleeper`, and `Random` are single-method interfaces defined in the consumer's package. Tests supply fake implementations without imports from the production package. TypeScript would typically use Jest mocks or parameter reassignment, both of which are less type-safe and harder to verify at compile time.

## Where it lives

| File | Key symbols |
|---|---|
| `internal/engine/stream/stream.go` | `Stream[T,R]`, `New`, `Push`, `End`, `EndWith`, `Events`, `Result`, `ErrNoResult`, `finish`, `finishLocked`, `dispatch` |
| `internal/engine/types/stream.go` | `StreamEvent`, `StreamEventType` (all `Ev*` constants), `AssistantBuilder`, `Fold`, `Message` |
| `internal/engine/types/message.go` | `AssistantMessage` (struct fields including `Content []ContentBlock`) |
| `internal/engine/types/content.go` | `ContentBlock`, `BlockType` constants |
| `internal/retry/stream.go` | `Retrier`, `Streamer`, `StreamFunc`, `New`, `Stream`, `produce`, `forwardAttempt`, `plan`, `attemptMessage`, `copyEvent`, `forward`, `cloneMessage`, `cloneBlocks`, `cloneBlock`, `cloneMessagePtr` |
| `internal/provider/base/watchdog.go` | `StreamTimeouts`, `StreamTimeoutsFromEnv`, `Watchdog`, `WatchStream`, `Event`, `Output`, `Stop`, `Stalled`, `Err`, `run`, `bound` |
| `internal/retry/config.go` | `Config`, `Clock`, `Sleeper`, `Random`, `timerSleeper`, `lockedRandom`, `DefaultConfig`, `Validate` |
| `internal/provider/anthropic/events.go` | `foldAnthropicEvent`, `foldContentBlockStart`, `foldState` (the live builder the race originated in) |
| `12-idiomatic-go-vs-typescript.md` | Design rules for channels and streaming ownership, state and concurrency, errors, and race testing |
