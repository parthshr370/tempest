# 01 · The streaming engine and agent loop

## The problem

An LLM harness has exactly one job: run a turn. Call a provider, get tokens back as they appear, detect when the model wants to call a tool, run that tool, feed the result back, and repeat until the model produces a final answer. Break that loop and there is no product.

The TypeScript reference did this with an async generator — `for await (const event of stream)` — and kept the "what does a turn look like" logic in one file but scattered event accumulation across every consumer. The Go port had to solve the same problem with different tools: goroutines, channels, and a language that has no async iteration.

The design had to answer four things at once:

1. **How does an event travel from provider bytes to consumer code?** The wire is SSE/JSON with ten event kinds; the consumer sees Go types. Somewhere in between, bytes become a typed event.
2. **Who owns accumulating those events into a message?** Text arrives as delta events (`Hel` → `lo` → authoritative `Hello`). If every consumer reassembles, the fold logic duplicates and fragments.
3. **How do you signal "this stream is done" and hand back a final value?** The TypeScript async generator returns its last `yield` naturally. Go channels close, but a closed channel carries no value.
4. **How does the tool-calling loop stay correct under cancellation, errors, and concurrent tool dispatch?** The loop body is straightforward in the happy case, but cancellation must reach provider calls, tool execution, and stream cleanup without leaving dangling goroutines or half-appended messages.

## Key decisions and the thinking process

### Decision 1: Typed event stream with a terminal-result contract

The harness models a provider response as a **stream of typed events** that resolves to a **single terminal result**. Not a callback. Not a `Promise<Event[]>`. A push-based channel that the consumer ranges over, plus a blocking `Result()` that returns the final value *after* the stream closes.

The TypeScript reference used an async generator: `yield` for each event, `return` for the terminal result. Go has no equivalent. The alternatives:

- **Return `([]Event, Result)` only after the stream finishes.** Non-starter — the whole point is incremental delivery. The consumer must see text as it arrives.
- **Two channels: one for events, one for the result.** Workable but fragile — nothing enforces that the result channel fires *after* the event channel closes, and a consumer that reads the wrong channel first deadlocks.
- **A generic `Stream[T, R]` that owns both.** This is what we built. The channel close order (`close(done)` → `close(events)`) creates a happens-before edge that makes `Result()` wait-free after the events channel drains.

The generic `Stream[T, R any]` at `internal/engine/stream/stream.go:25-39` is the result: a single-producer, multi-consumer channel with an unbounded in-memory queue, a dispatch goroutine, and a `sync.Once`-guarded close. `R` is the terminal result extracted from a completing event (or supplied via `EndWith`).

### Decision 2: The builder folds events; consumers read the result

`AssistantBuilder` at `internal/engine/types/stream.go:42-44` folds a stream of `StreamEvent` values into an `AssistantMessage`. Every consumer that wants the accumulated message calls `builder.Message()`. Nobody reassembles deltas themselves.

Why not let the consumer fold? The reference scattered fold logic across every GUI, CLI, and test consumer. When a tool-call delta format changed, three consumers broke. The builder centralizes one fold loop that every consumer depends on. A `*_end` event **overwrites** the accumulated block with the provider's final content — the provider's assembled value is authoritative, not the sum of deltas — and the builder encodes this once.

### Decision 3: Tagged struct unions, not interface sums

`StreamEvent` is one flat struct with a `Type` discriminant and fields that are populated only for relevant arms. The same pattern holds for `ContentBlock`, `Message` (an interface — the exception), and `AgentEvent`. See module 12 (idiomatic Go vs TypeScript).

The TypeScript reference used discriminated unions (`type Event = {type: "text_delta"; delta: string} | {type: "done"; message: Message}`). Go has two paths:

- **Interface + type switch.** Works for `Message` (user | assistant | toolResult) because the loop filters by role and consumers type-switch. But for ten event variants it would mean ten tiny types, ten allocations per event, and boilerplate interface satisfaction.
- **One tagged struct.** Zero allocations per arm. One `switch ev.Type` in the fold loop. JSON marshal/unmarshal through a single code path. The cost: unused fields are zero, which `omitempty` hides. The win: the fold loop is a dense 43-line switch (`internal/engine/types/stream.go:61-104`) that any engineer can read in one screen.

`Message` remains an interface because the transcript is a heterogeneous `[]Message` slice and consumers genuinely need to filter by `Role()` — the interface carries its weight.

### Decision 4: Two-pass transform before the provider call

Before messages reach the provider, `TransformMessages` (`internal/engine/transform/transform.go:87-222`) runs a two-pass normalization:

1. **Pass 1:** Downgrade images for non-vision models, strip cross-model thinking signatures, normalize tool-call IDs.
2. **Pass 2:** Synthesize "No result provided" for orphaned tool calls, skip errored/aborted assistant messages.

This is a pure function over `[]Message` → `[]Message`. It does not touch the stored transcript. It runs once per provider call, not once per event. The transform package exists because provider adapters should not each implement normalization — that would duplicate image-downgrade logic across Anthropic, OpenAI, and Google backends.

### Decision 5: Validate before execute, coerce generously

`ValidateToolArguments` (`internal/engine/validate/validate.go:467-496`) runs JSON Schema validation *and* light coercion before a tool's `Execute` is called. The coercion converts strings to numbers (matching JavaScript's loose typing), collapses single-element arrays into scalars for non-array schemas, and applies `anyOf`/`oneOf` resolution. This means tool authors write strict schemas but the runtime tolerates model output quirks.

The validator cache (`sync.Map` at line 20) avoids re-parsing the same schema bytes across turns. A tool called 50 times in one session compiles its schema once.

### Decision 6: The loop is self-owned, not framework-owned

`runLoop` (`internal/agent/loop.go:142-285`) is a plain `for` loop, not a state machine driven by a framework. It calls `streamFn` (the provider seam), ranges over events, dispatches tools, appends results, and loops. The `Agent` wrapper (`internal/agent/runtime.go:120-152`) adds concurrency guards, listener fan-out, and the steering/follow-up queue — but the loop itself is synchronous, single-goroutine, and owns its own control flow.

This matters because tool-calling loops in other Go agent frameworks use callback chains or event-driven state machines. When a bug report says "the loop stopped after tool call 3," you can read the `for` loop and trace it. No inversion of control.

## Signatures and types

### StreamEvent — the event union

```go
// internal/engine/types/stream.go:7-48
type StreamEventType string

const (
    EvStart         StreamEventType = "start"
    EvTextStart     StreamEventType = "text_start"
    EvTextDelta     StreamEventType = "text_delta"
    EvTextEnd       StreamEventType = "text_end"
    EvThinkingStart StreamEventType = "thinking_start"
    EvThinkingDelta StreamEventType = "thinking_delta"
    EvThinkingEnd   StreamEventType = "thinking_end"
    EvToolCallStart StreamEventType = "toolcall_start"
    EvToolCallDelta StreamEventType = "toolcall_delta"
    EvToolCallEnd   StreamEventType = "toolcall_end"
    EvDone          StreamEventType = "done"
    EvError         StreamEventType = "error"
    // EvRetry: the attempt so far failed transiently; the same request is
    // sent again and later events belong to the next attempt.
    EvRetry         StreamEventType = "retry"
)

type StreamEvent struct {
    Type         StreamEventType    // discriminant — switch on this
    ContentIndex int                // which content block this event targets
    Delta        string             // text_delta / thinking_delta / toolcall_delta
    Content      string             // text_end / thinking_end (authoritative full content)
    ToolCall     *ContentBlock      // toolcall_end (assembled tool call, overwrites deltas)
    Reason       StopReason         // done / error stop reason
    Partial      *AssistantMessage  // non-terminal arms — live pointer to builder's message
    Message      *AssistantMessage  // done — the final message
    Err          *AssistantMessage  // error: the error-bearing message; retry: the discarded attempt
    Attempt      int                // retry: 1-based ordinal of the failed attempt
    RetryDelay   time.Duration      // retry: wait before the next attempt
}
```

Every non-terminal event (`*_start`, `*_delta`) carries `Partial`, a pointer to the builder's live message buffer. Terminal events (`EvDone`, `EvError`) carry the final message. The `*_end` events are authoritative: `TextEnd.Content` overwrites whatever deltas accumulated, because the provider sometimes emits a final assembled string that differs from the sum of deltas.

`EvRetry` is not terminal. It reports that the attempt streamed so far failed with a transient error and the same request is being sent again. It carries the discarded attempt in `Err` (partial content, usage, error), its ordinal in `Attempt`, and the wait before the next attempt in `RetryDelay` (`internal/engine/types/stream.go:22-26,45-47`). Folding it resets the builder, so the stream's `Final()` reflects only the last attempt (`stream.go:114-115`).

### AssistantBuilder — the fold

```go
// internal/engine/types/stream.go:50-121
type AssistantBuilder struct {
    msg AssistantMessage   // mutated in place by Fold
}

func NewAssistantBuilder(api, provider, model string) *AssistantBuilder
    // Tags the message with source metadata — which API, provider, model produced it.

func (b *AssistantBuilder) Fold(ev StreamEvent)
    // Applies one event to the accumulating message. Switches on ev.Type:
    //   *_start  → allocates a content block slot at ev.ContentIndex
    //   *_delta  → appends ev.Delta to the block's accumulating field
    //   *_end    → overwrites the block with ev.Content / ev.ToolCall (authoritative)
    //   EvDone   → records ev.Reason as StopReason
    //   EvError  → records ev.Reason + copies ErrorMessage/ErrorCode/ErrorDetails from ev.Err
    //   EvRetry  → resets the accumulated message to the provenance header

func (b *AssistantBuilder) Message() AssistantMessage
    // Returns the accumulated message. Content slice is shared — callers that
    // need independence must copy (see snapshotAssistant in agent/loop.go:685-702).
```

The `block(i, typ)` helper (`stream.go:64-70`) grows `b.msg.Content` with zero-value blocks of the right type so folds can arrive by content index. The model may interleave text and tool-call blocks; the builder tracks each by its index.

### Stream[T,R] — the generic channel

```go
// internal/engine/stream/stream.go:25-156
type Stream[T, R any] struct { /* private fields */ }

func New[T, R any](buf int, isComplete func(T) bool, extract func(T) R) *Stream[T, R]
    // buf      — channel buffer size for Events()
    // isComplete — predicate: does this event terminate the stream?
    // extract  — derives the terminal R from the completing event

func (s *Stream[T, R]) Push(ev T)
    // Enqueues event. Dropped if stream already closed. Completing events still
    // reach Events() before close.

func (s *Stream[T, R]) End()
    // Closes without a result. Safe to call more than once.

func (s *Stream[T, R]) EndWith(result R)
    // Closes with an explicit result. Used when no completing event was pushed
    // (e.g., the agent loop ending its event stream with the message list).

func (s *Stream[T, R]) Events() <-chan T
    // Receive-only channel. Range until close.

func (s *Stream[T, R]) Result(ctx context.Context) (R, error)
    // Blocks until a terminal result is available or ctx is canceled.
    // Returns ErrNoResult when the stream ended without a completing event.
```

`Result()` is independent of event consumption. A goroutine can call `Result()` while another ranges over `Events()`. The dispatch goroutine drains the internal queue onto the events channel; `close(done)` fires before `close(events)`, so `Result()` unblocks before `range Events()` exits.

### AssistantStream — the typed specialization

```go
// internal/engine/stream/assistant.go:10-68
type AssistantStream struct {
    *Stream[types.StreamEvent, *types.AssistantMessage]
    builder *types.AssistantBuilder
    final   *types.AssistantMessage
}

func NewAssistantStream(api, provider, model string) *AssistantStream
    // Wires isComplete = EvDone || EvError
    // Wires extract    = ev.Message (EvDone) or ev.Err (EvError)

func (a *AssistantStream) Push(ev types.StreamEvent)
    // Folds ev into builder, snapshots final message on terminal events,
    // then forwards to Stream.Push.

func (a *AssistantStream) Final() *types.AssistantMessage
    // Returns the folded message. Safe after Result() returns or Events() closes.
```

### StreamFn — the provider seam

```go
// internal/agent/types.go:13
type StreamFn func(
    ctx context.Context,
    model types.Model,
    c types.Context,
    opts *types.SimpleStreamOptions,
) *stream.AssistantStream
```

The loop never imports a provider directly. `StreamFn` is the single injection point. Failures are encoded in the returned stream as error/aborted terminal events — the function never throws. This lets tests supply a fake stream (`faux.Faux`) and lets the loop switch providers by swapping the function.

### AgentEvent — the loop's event surface

```go
// internal/agent/types.go:151-228
type AgentEventType string

const (
    EventAgentStart         AgentEventType = "agent_start"
    EventAgentEnd           AgentEventType = "agent_end"
    EventTurnStart          AgentEventType = "turn_start"
    EventTurnEnd            AgentEventType = "turn_end"
    EventMessageStart       AgentEventType = "message_start"
    EventMessageUpdate      AgentEventType = "message_update"
    EventMessageEnd         AgentEventType = "message_end"
    EventToolExecutionStart AgentEventType = "tool_execution_start"
    EventToolExecutionUpdate AgentEventType = "tool_execution_update"
    EventToolExecutionEnd   AgentEventType = "tool_execution_end"
)

type AgentEvent struct {
    Type                 AgentEventType
    Turn                 int
    Message              types.Message
    AssistantMessageEvent *types.StreamEvent  // carried by message_update
    ToolCallID           string
    ToolName             string
    Args                 any
    PartialResult        any
    Result               any
    IsError              bool
    ToolResults          []types.ToolResultMessage
    Messages             []types.Message       // carried by agent_end
}

type AgentEventSink func(ctx context.Context, ev AgentEvent) error
```

The `AgentEvent` surface is the public API that listeners (`Subscribe`) observe. It wraps the internal `StreamEvent` stream in lifecycle events (`agent_start`, `turn_start`, `tool_execution_start`) so a UI can render progress without knowing about provider deltas.

## How it works

### The event fold: from deltas to a message

A provider stream produces a sequence like this:

```
EvStart
EvTextStart     ContentIndex=0
EvTextDelta     ContentIndex=0  Delta="Hel"
EvTextDelta     ContentIndex=0  Delta="lo"
EvTextEnd       ContentIndex=0  Content="Hello"     ← authoritative
EvToolCallStart ContentIndex=1  ToolCall={ID:"c1", Name:"read"}
EvToolCallDelta ContentIndex=1  Delta=`{"path":`
EvToolCallDelta ContentIndex=1  Delta=`"a.txt"}`
EvToolCallEnd   ContentIndex=1  ToolCall=assembled   ← authoritative
EvDone          Reason=StopToolUse
```

The builder folds them:

1. `EvStart` — no-op for the builder (source metadata was set at construction).
2. `EvTextStart` at index 0 — `block(0, BlockText)` allocates `Content[0]` as a text block.
3. `EvTextDelta` at index 0 — appends `"Hel"` to `Content[0].Text`.
4. `EvTextDelta` at index 0 — appends `"lo"` to `Content[0].Text`. `Text` is now `"Hello"`.
5. `EvTextEnd` at index 0 with `Content="Hello"` — **overwrites** `Content[0].Text = "Hello"`.
6. `EvToolCallStart` at index 1 — `block(1, BlockToolCall)` allocates `Content[1]` as a tool-call block, copies ID and Name from `ev.ToolCall`.
7. `EvToolCallDelta` at index 1 — appends `{"path":` to `Content[1].Arguments` (a `json.RawMessage`).
8. `EvToolCallDelta` at index 1 — appends `"a.txt"}`. Arguments is now `{"path":"a.txt"}`.
9. `EvToolCallEnd` at index 1 with `ToolCall=assembled` — **overwrites** the entire block with the provider's assembled tool call.
10. `EvDone` with `Reason=StopToolUse` — sets `msg.StopReason = StopToolUse`.

The overwrite pattern (`*_end` is authoritative) matters because some providers emit a final text string that differs from the concatenation of deltas (Unicode normalization, whitespace trimming). The builder trusts the provider's final rendering.

### The stream: from Push to Events channel

`Stream[T,R]` uses an internal architecture with three parts:

1. **Unbounded queue** (`[]T`, guarded by `sync.Mutex`). `Push` appends and signals a `sync.Cond`.
2. **Dispatch goroutine** (`dispatch()`). Runs in `New`. Waits on the condition variable, dequeues head, sends on `s.events` channel. Exits when queue is empty AND stream is closed — then closes `s.events`.
3. **Result gate** (`done` channel + `sync.Once`). `finish()` stores the result and calls `close(s.done)` exactly once. The `close(done)` → `close(events)` ordering creates a happens-before edge: when `Result()` sees `done` closed, the dispatch goroutine has either already closed `events` or is about to — but the stored `result` value is visible.

This design means `Result()` does not need to consume events. A caller that only wants the final message calls `Result()` and ignores `Events()`. A caller that wants every event ranges over `Events()` and calls `Result()` afterward. Both work without coordination.

### The turn loop: one call, step by step

`runLoop` at `internal/agent/loop.go:142-285` orchestrates one agent run. Here is one turn:

1. **Drain steering messages.** If `GetSteeringMessages` returns messages, emit `message_start`/`message_end` for each and append them to the transcript. Steering messages (e.g., "stop and summarize") inject user turns mid-run.

2. **Call the provider.** `streamAssistantResponse` (`loop.go:315-426`) does:
   - Transform context via `TransformContext` hook (optional).
   - Convert messages to LLM format via `ConvertToLlm` (drops system messages, passes user/assistant/toolResult).
   - Build `types.Context` with system prompt, messages, and tools.
   - Resolve API key via `GetAPIKey` hook.
   - Call `streamFn(ctx, model, context, &options)`.
   - **Range over `response.Events()`.** For each event: fold into a local builder, snapshot the partial, emit `message_start` (on `EvStart`), `message_update` (on every delta/end event), or `message_end` (on `EvDone`/`EvError`).
   - The `snapshotAssistant` call at line 360 is critical: it copies the builder's Content slice so the emitted partial is independent of future folds. Without it, a listener reading the partial on another goroutine would race with the fold loop (see module 07 for the live-Partial race that this snapshot fixed).
   - The `EvDone`/`EvError` branch reads `assistantResult()` under `context.Background()` — not the run's context — to avoid losing the terminal message when the run context was canceled.

3. **Check stop reason.** If `StopError` or `StopAborted`, end the turn AND the run immediately. No tool execution. A truncated stream may have incomplete tool calls.

4. **Extract tool calls.** Scan `message.Content` for `BlockToolCall` blocks.

5. **Execute tools.** `executeToolCalls` (`internal/agent/executor.go:57-82`) decides sequential vs parallel dispatch. Parallel mode runs a two-phase pipeline: prepare (validation + `BeforeToolCall` hook, sequential) then execute (goroutines + `WaitGroup`). Results write into source-order slots so `ToolResultMessage` order matches tool-call order even though end-events fire in completion order.

6. **Append results.** Tool results become `ToolResultMessage` values appended to the transcript.

7. **Emit `turn_end`.**

8. **Check `ShouldStopAfterTurn`.** If the hook returns true, end the run.

9. **Check `PrepareNextTurn`.** The hook can swap the model, context, or thinking level for the next inner iteration.

10. **Check `hasMoreToolCalls`.** If any tool result did NOT set `Terminate`, the model gets another round. Otherwise, the outer loop drains follow-up messages and continues, or breaks.

The `Terminate` flag on tool results controls the inner/outer distinction. A tool that sets `Terminate: true` signals "this batch completes the turn." A tool that omits it (or sets it false) lets the model continue calling tools. The batch terminates only when *every* result sets Terminate (`executor.go:225-235`).

### The validation layer: before Execute is called

When a tool call arrives, `prepareToolCall` (`executor.go:260-316`) runs:

1. **Look up the tool** by name in the registry. Unknown tool → error result.
2. **Run `PrepareArguments`** if the tool declares it. The byte-equality check preserves the "unchanged" branch.
3. **Validate with `ValidateToolArguments`.** This compiles the tool's JSON Schema (cached), coerces the arguments (string→number, array→singleton), validates, and returns the coerced JSON. Validation failure → error result with the schema violation details.
4. **Run `BeforeToolCall` hook.** The hook can return `Allowed: false` (blocked), `ModifiedArgs` (substituted arguments), or an error (aborted). Panics from the hook are recovered into error results.

### The runtime wrapper: Agent

`Agent` (`internal/agent/runtime.go:120-152`) wraps the low-level loop with:

- **Single-run guard.** `mu` protects `activeRun`; a second `Prompt`/`Continue` returns a busy error.
- **Steering/follow-up queues.** `Steer()` and `FollowUp()` enqueue messages. The loop drains them at inner/outer iteration boundaries via closures in `createLoopConfig`.
- **Listener fan-out.** `Subscribe()` registers a listener; `processEvents` fans out each event to all listeners outside the mutex so a slow listener does not block state mutations. Listener errors stop fan-out immediately.
- **Lifecycle.** `runWithLifecycle` creates a derived context (so `Abort` cancels the run without canceling the caller's context), calls the executor, recovers panics via `handleRunFailure` (synthesizing failure events so listeners see a complete start→end cycle), and always runs `finishRun` to close the done channel.

## Failure modes and invariants

### Invariant: `*_end` is authoritative

The provider may emit a final text string or assembled tool call that differs from the sum of deltas. The `Fold` method overwrites on `*_end`, never appends. A consumer that reads `builder.Message()` after `EvDone` gets the provider's final rendering, not the intermediate deltas. The test at `types/stream_test.go:11-51` verifies this: text deltas accumulate to "Hello", the end event delivers "Hello" again, and the final message shows `Text = "Hello"`.

### Invariant: Result is visible after Events closes

`finish()` calls `close(s.done)` under `once.Do`. The dispatch goroutine closes `s.events` only after `s.closed && len(s.queue) == 0`. Since `finishLocked` sets `s.closed = true` before `close(s.done)`, and the dispatch goroutine checks `s.closed` under the same mutex, when `Result()` sees `done` closed, the result value is stored and the events channel will close (or has closed). The test at `stream/stream_test.go:103-129` verifies: `Result()` returns the correct value without consuming any events.

### Invariant: No tool execution on errored/aborted streams

When `StopReason` is `StopError` or `StopAborted`, the loop skips tool extraction (`runLoop` at line 184-195) and emits `turn_end` immediately. The comment says why: the assistant message may be incomplete and its tool calls would be half-formed. The transform layer also skips errored/aborted assistant messages (pass 2 at `transform.go:156-158`), so a truncated message with orphaned tool calls does not reach the provider.

### Invariant: One active run

`Agent.mu` guards `activeRun`. `runWithLifecycle` checks it under lock and returns a busy error if a run is in progress. `startRunLocked` sets it under lock. `finishRun` clears it under lock, then calls `cancel()` as a safety net. The `activeRun.done` channel closes before `cancel()` so `WaitForIdle` unblocks cleanly.

### Race: live Partial pointer

The builder's `Message()` returns a struct whose `Content` slice is shared with the builder's internal buffer. If the fold loop mutates `Content[i].Text` while a listener reads the previously-emitted partial, that is a data race. `streamAssistantResponse` at line 360 calls `snapshotAssistant` to copy the Content slice and each tool-call's Arguments bytes before emitting. This decouples the emitted event from future folds. The snapshot is documented at `loop.go:287-308`. Module 07 covers the live-Partial race this fixed.

### Race: late partial update after tool_execution_end

`executePreparedToolCall` (`executor.go:323-394`) uses an `acceptingUpdates` flag + `sync.WaitGroup` to ensure any in-flight `onUpdate` goroutine settles before the function returns. Without this, a tool's partial update could fire after `tool_execution_end` and confuse listeners that expect start→update*→end ordering.

### Edge: stream ends without EvDone/EvError

The `range response.Events()` loop in `streamAssistantResponse` has a fallback after the loop body (lines 410-425). If the stream's events channel closes without a terminal event (e.g., context cancellation mid-fold), it calls `assistantResult()` to extract whatever the stream resolved, synthesizes `message_start` if needed, and emits `message_end`.

## TypeScript to Go

### Async generators → channels + terminal-result contract

TypeScript:
```typescript
async function* streamResponse(): AsyncGenerator<StreamEvent, AssistantMessage> {
    for (const chunk of sseStream) {
        yield parseEvent(chunk);
    }
    return buildFinalMessage();
}

// Consumer:
for await (const event of streamResponse()) {
    if (event.type === "text_delta") render(event.delta);
}
// After the loop, the return value is... gone. You need `.next()` manually or
// a wrapper that captures `.return()`.
```

An async generator's `return` value is invisible to `for await`. To get the final message, you must call `.next()` after the loop exits, or wrap the generator in a helper that stores the return value. Both patterns are fragile — the TypeScript reference had bugs where the final message was silently dropped by consumers that only iterated.

Go:
```go
// Producer returns a *Stream that owns both paths.
func streamResponse() *stream.AssistantStream { ... }

// Consumer A: wants every event.
for ev := range response.Events() { handle(ev) }
msg, err := response.Result(ctx)

// Consumer B: only wants the final message.
msg, err := response.Result(ctx)
```

The Go design makes the terminal result a first-class channel. `Result()` blocks until the stream settles, independent of event consumption. No wrapper needed. No silent drop.

Why Go rewards this: channels close explicitly, and a `range` loop naturally terminates on close. The terminal-result contract (`Stream[T,R]`) uses `close(done)` as a signal that the result is ready — a pattern that async generators can emulate but do not natively support.

### Discriminated unions → tagged structs

TypeScript:
```typescript
type StreamEvent =
    | { type: "text_delta"; contentIndex: number; delta: string }
    | { type: "text_end";   contentIndex: number; content: string }
    | { type: "done";       reason: StopReason; message: AssistantMessage }
    | { type: "error";      reason: StopReason; err: AssistantMessage };
    // ... 8 more arms

function handle(event: StreamEvent) {
    switch (event.type) {
        case "text_delta": return event.delta;      // narrowed
        case "done":       return event.message;     // narrowed
    }
}
```

TypeScript narrows each arm to its specific fields. The compiler guarantees you cannot access `event.delta` on a `"done"` event.

Go:
```go
type StreamEvent struct {
    Type         StreamEventType
    ContentIndex int
    Delta        string
    Content      string
    ToolCall     *ContentBlock
    Reason       StopReason
    Partial      *AssistantMessage
    Message      *AssistantMessage
    Err          *AssistantMessage
}

func (b *AssistantBuilder) Fold(ev StreamEvent) {
    switch ev.Type {
    case EvTextDelta:
        b.block(ev.ContentIndex, BlockText).Text += ev.Delta
    case EvDone:
        b.msg.StopReason = ev.Reason
    }
}
```

Go has no narrowing. Every field is always in scope. The compiler does not prevent you from reading `ev.Delta` on an `EvDone` event — it will be `""` (the zero value), which is correct but not enforced. The tagged struct trades compile-time exhaustiveness for zero allocations and a single `switch` without interface dispatch.

Why Go rewards this: `StreamEvent` is 104 bytes on the stack (five strings, two pointers, an int). Ten interface-typed union members would allocate on every event. In a hot fold loop processing hundreds of events per turn, the difference is measurable. The tagged struct is also trivially JSON-decodable — the provider adapter unmarshals into one struct and sets `Type`, no discriminator gymnastics needed.

### Event loop + callbacks → goroutines + channels

TypeScript:
```typescript
// Single-threaded event loop. Tool calls must not block the event loop.
const results = await Promise.all(toolCalls.map(tc => executeTool(tc)));
```

The JS event loop is cooperative. `Promise.all` runs tool calls concurrently on the microtask queue, but CPU-bound work blocks all progress. There is no true parallelism.

Go:
```go
// Parallel tool dispatch: goroutines + WaitGroup + source-order slots.
func executeToolCallsParallel(...) {
    slots := make([]executedOutcome, len(toolCalls))
    var wg sync.WaitGroup
    for i, tc := range prepared {
        wg.Add(1)
        go func(idx int, call preparedToolCall) {
            defer wg.Done()
            slots[idx] = executePreparedToolCall(ctx, &call, emit)
        }(i, tc)
    }
    wg.Wait()
}
```

Go runs tool calls on OS threads. The loop uses `errgroup`-style patterns for cancellation propagation: if one tool fails and the config says to abort, the context cancels the remaining goroutines. The `safeEmit` serializer (a mutex-protected channel send) ensures events fire in the order the listener expects, even when tool execution completes out of order.

Why Go rewards this: goroutines are cheap (a few KB of stack). Spawning one per tool call is idiomatic. The TypeScript equivalent requires workers, task queues, or `worker_threads` — none of which are the default path.

### Optional values → explicit result/error

TypeScript:
```typescript
// Undefined means "no tool calls."
const toolCalls = message.content.filter(b => b.type === "toolCall");
if (toolCalls.length === 0) return; // implicit: no error, just done
```

Go:
```go
hasMoreToolCalls := false
if len(toolCalls) > 0 {
    executedToolBatch, err := executeToolCalls(ctx, ...)
    if err != nil {
        return newMessages, err  // explicit error propagation
    }
    hasMoreToolCalls = !executedToolBatch.Terminate
}
```

Go forces you to handle the error return. The TypeScript reference sometimes let errors propagate as unhandled promise rejections. The Go loop surfaces every error through the `emit` sink or the return value, so the caller (the `Agent` runtime) can synthesize failure events.

## Where it lives

| What | File | Key symbols |
|---|---|---|
| Event types and builder | `internal/engine/types/stream.go` | `StreamEventType`, `StreamEvent`, `AssistantBuilder`, `Fold`, `Message` |
| Content block types | `internal/engine/types/content.go` | `BlockType`, `ContentBlock`, `StopReason` |
| Message union | `internal/engine/types/message.go` | `Message`, `UserMessage`, `AssistantMessage`, `ToolResultMessage` |
| Tool types | `internal/engine/types/tool.go` | `Tool`, `Context` |
| Stream options | `internal/engine/types/options.go` | `StreamOptions`, `SimpleStreamOptions` |
| Generic stream | `internal/engine/stream/stream.go` | `Stream[T,R]`, `New`, `Push`, `End`, `EndWith`, `Events`, `Result` |
| Assistant stream | `internal/engine/stream/assistant.go` | `AssistantStream`, `NewAssistantStream`, `Push`, `Final` |
| Transform pipeline | `internal/engine/transform/transform.go` | `TransformMessages`, `downgradeUnsupportedImages` |
| Validation layer | `internal/engine/validate/validate.go` | `ValidateToolArguments`, `ValidateToolCall` |
| Agent types | `internal/agent/types.go` | `StreamFn`, `AgentTool`, `AgentContext`, `AgentLoopConfig`, `AgentEvent`, `AgentEventSink` |
| Turn loop | `internal/agent/loop.go` | `AgentLoop`, `AgentLoopContinue`, `runLoop`, `streamAssistantResponse`, `assistantResult` |
| Tool executor | `internal/agent/executor.go` | `executeToolCalls`, `prepareToolCall`, `executePreparedToolCall` |
| Runtime wrapper | `internal/agent/runtime.go` | `Agent`, `Prompt`, `Continue`, `Abort`, `Subscribe`, `Steer`, `FollowUp` |
| Stream tests | `internal/engine/stream/stream_test.go` | `TestStreamOrderAndResult`, `TestAssistantStreamFinal`, `TestStreamConcurrentResultAndEvents` |
| Builder tests | `internal/engine/types/stream_test.go` | `TestFoldReducer`, `TestFoldError` |
| Loop tests | `internal/agent/loop_test.go` | `TestAgentLoopSingleTurnNoTools`, `TestAgentLoopOneToolRoundContinuesToSecondTurn`, `TestAgentLoopErrorStopEndsImmediately` |
| Design contract | `12-idiomatic-go-vs-typescript.md` | Sections 6 (channels), 7 (state), 9 (typed events) |
