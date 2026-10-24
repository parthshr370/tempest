# 08 — Compaction

## The problem

The model has a context window — for Claude Sonnet 4, 200k tokens. A long build conversation with tool outputs, file diffs, and stacked turns burns through that budget fast. Once the conversation exceeds the window minus a reserve margin, the next provider request fails with `context_length_exceeded`. The user sees a crash, not progress.

The fix is not "use a bigger model" — the window is always finite, and the user's conversation grows without bound. The fix is **summarization**: periodically rewrite old history into a structured checkpoint summary, keep only the most recent turns, and re-inject the summary so the model sees past decisions without the raw log.

Compaction is that mechanism. It telescopes a long transcript into a dense summary without losing the engineering context the model needs to keep building.

## Key decisions and the thinking process

### Compaction is a between-turn hook, not a retry path

The TypeScript reference runs compaction and retry from the same `agent_end` event, but the order matters: retry classification runs first, and context overflow is explicitly excluded from retry. The `isContextOverflow` check short-circuits before 429/5xx/network matching, so overflow falls through to compaction recovery instead of standard retry.

This boundary is load-bearing. If a context-overflow error were mistaken for a transient 500, the retrier would sleep-and-replay the same oversized request, burning API quota and eventually exhausting retry attempts. The user gets "retry attempts exhausted" when the real fix was "summarize and retry with the summary."

The Go implementation preserves this boundary at the code level. The retry decorator (`internal/retry`) classifies `context_length_exceeded` as `CodeContextOverflow` with `Retryable=false`. That classification runs in `Classify()` before any 429/5xx/network matching. The retrier never imports, references, or calls anything from `internal/compaction` or `internal/session`. Conversely, the compaction layer never knows how the provider stream was decorated — it receives a `StreamFn` and calls it.

Compaction is wired into the agent loop as a `PrepareNextTurnWithContext` callback, which fires *after* a successful turn and *before* the next provider request. It inspects the current messages, estimates total tokens, and compacts when the estimate crosses the threshold. This is proactive maintenance, not reactive recovery — by the time a tool-heavy turn finishes and the message list balloons, compaction runs before the next streaming request ever starts.

### Sticky rules live in the system prompt, not in message history

The `RULES.md` files loaded at startup (user-level and project-level) must survive compaction. If rules were user messages injected into the transcript, a compaction pass would summarize them and strip the original text — a future turn might see a summary like "the user asked for plain English output" instead of the precise rule body.

The fix is structural: rule bodies go into `GenericRules`, which `RebuildSystemPrompt` embeds into the system prompt string. The system prompt is stored in `AgentState` and included in every `types.Context.SystemPrompt` the loop sends to the provider. Compaction transforms only `Messages` — it never touches `SystemPrompt`. So rules reattach to every request, cleanly, without synthetic history messages or engine-level exceptions.

This is the "sticky-reattachment contract" verified in `item11-spec.md:60`. It matters because a second compaction would summarize the first compaction's summary message, but the system prompt is always the original text.

### History stays byte-stable between compactions

Compaction is the only pass that rewrites history the model has already seen. Between compactions, every provider request is the previous request plus the new messages, byte for byte, so the provider's prompt cache serves the whole prefix and only the new turn is billed at the uncached rate.

An earlier design ran a per-request projection that kept the 16 most recent tool results and replaced older ones with "cleared" placeholders. Once a session passed 16 tool results, each request rewrote one more old result, deep in the history, so every request diverged from its predecessor early and the cached prefix stopped growing: in the 2026-10-23 cross-harness benchmark, every long Tempest run's cached tokens froze at about 4.9k from the 17th tool result on (`context/bench/results/20261023T131649Z`). The projection was removed. Tool output is already capped when the tool runs, and compaction, which estimates the full history, handles real context pressure.

### Compaction is a separate, non-streaming internal call

The summarizer is not the main agent loop. `runSummarization()` builds a fresh `types.Context` with a fixed system prompt (`SummarizationSystemPrompt` — "You are a context summarization assistant…") and a single user message containing the serialized conversation. It calls `streamFn.Result(ctx)` — the blocking result collector on the provider stream — not the streaming `Events()` channel the main loop uses. This is a oneshot: no tool calls, no follow-up, just a single provider request whose text blocks are joined into the summary string.

The timeout is 120 seconds (`compactionSummaryTimeout`), set in `maybeCompact()` with a `context.WithTimeout`. If the summarizer hangs, the deadline cancels it and the loop keeps the original history rather than installing a blank or truncated checkpoint.

### Split-turn handling

Not every message boundary is a safe cut point. Compaction cuts at user messages, assistant messages, branch summaries, and compaction summaries — never at a `toolResult`. If the accumulated token budget forces a cut mid-turn (e.g. the keep-recent window lands inside a long assistant→tool→assistant sequence), the cut is a "split turn." The dropped prefix gets its own `turnPrefixSummarizationPrompt` summary, merged into the final output under `**Turn Context (split turn):**`. The kept suffix retains tool-call/result pairing intact, so the model sees complete turns.

### The cut-point heuristics

`FindCutPoint` walks backward from the end of the boundary window, accumulating token estimates. When the accumulated estimate crosses `KeepRecentTokens` (default 20000), it finds the nearest valid cut point at or beyond that index. It then walks backward over metadata-only entries (model change markers, thinking level changes) to pull them into the kept region — metadata without a message is useless to summarize and confusing to drop.

If the chosen cut index sits on a non-user message (e.g. an assistant turn), it searches backward for the turn start (`FindTurnStartIndex`). If found, the split-turn path activates: the preamble gets a separate summary and the turn suffix stays intact.

## Signatures and types

### Compaction settings

```go
// compaction.go:59-64
type CompactionSettings struct {
    Enabled       bool  // `false` disables ALL compaction, including the threshold check
    ReserveTokens int   // tokens reserved from the context window for the current turn
    KeepRecentTokens int // recent-token budget that stays in context, NOT summarized
}
```

- `Enabled`: the master kill switch. When false, `ShouldCompact` returns false unconditionally.
- `ReserveTokens` (default 16384): the model needs room for its next response. Compaction triggers when `tokens > contextWindow - ReserveTokens`.
- `KeepRecentTokens` (default 20000): budget for the kept (unsummarized) messages. `FindCutPoint` accumulates tokens backward until it hits this floor.

### The threshold check

```go
// compaction.go:272-277
func ShouldCompact(contextTokens, contextWindow int, settings CompactionSettings) bool
```

Returns true when `settings.Enabled` and `contextTokens > contextWindow - settings.ReserveTokens`. Pure arithmetic — no side effects, no parameter mutation.

### Preparing a compaction

```go
// compaction.go:372-436
func PrepareCompaction(pathEntries []SessionEntry, settings CompactionSettings) *CompactionPreparation
```

Takes the full session entry list (including prior compaction entries). Returns nil when:
- The last entry is already a compaction entry (no new messages to summarize)
- No prior messages exist in the boundary
- The computed cut point has an empty entry ID

Returns a `*CompactionPreparation` carrying:
- `FirstKeptEntryID`: the entry ID of the cut point
- `MessagesToSummarize`: entries from boundary start to history end
- `TurnPrefixMessages`: present only on split turns (the dropped prefix of the current turn)
- `IsSplitTurn`: whether the cut fell mid-turn
- `TokensBefore`: the token estimate before compaction (for diagnostics)
- `PreviousSummary`: the prior compaction's summary text (fed into the update prompt for iterative compaction)
- `FileOps`: cumulative file read/write/edit tracking across the conversation

### Running the summarization

```go
// compaction.go:954-970
func GenerateSummary(ctx context.Context, streamFn agent.StreamFn, model ptypes.Model,
    settings CompactionSettings, messages []ptypes.Message, previousSummary string) (string, error)
```

Serializes `messages` with `SerializeConversation`, wraps in `<conversation>` tags, optionally includes `<previous-summary>`, appends the summarization or update prompt, and calls `runSummarization`. The returned string is the structured checkpoint summary.

```go
// compaction.go:1032-1061
func Compact(ctx context.Context, streamFn agent.StreamFn, model ptypes.Model,
    prep *CompactionPreparation) (string, error)
```

Orchestrates the full summarization pass. For split turns: generates a history summary, then a turn-prefix summary, merges both. For normal turns: generates a single summary. Appends the file operations list via `FormatFileOperations`.

```go
// compaction.go:990-1027
func runSummarization(ctx context.Context, streamFn agent.StreamFn, model ptypes.Model,
    maxTokens int, promptText string) (string, error)
```

Issues a oneshot provider request with a dedicated system prompt. Uses `streamFn.Result(ctx)` to block on the full response, then joins text blocks. Returns an error for nil/empty/error/aborted results.

### Re-injecting the summary

```go
// compaction.go:1068-1084
func ApplyCompaction(pathEntries []SessionEntry, prep *CompactionPreparation, summary string) []ptypes.Message
```

Rebuilds the message list:
1. Wraps the summary in `compactionSummaryPrefix` / `compactionSummarySuffix` ("The conversation history before this point was compacted into the following summary…")
2. Finds the kept messages from `FirstKeptEntryID` onward via `keptMessages`
3. If the first kept message is a user message, merges the summary into it (prepends as a text block) to avoid two consecutive user turns, which Anthropic rejects
4. Otherwise appends the summary as a standalone user message before the kept suffix

### Session wiring

```go
// compact.go:20-25
type compactionRunner struct {
    contextWindow int           // from model.ContextWindow
    model         ptypes.Model  // the active model for summarization
    streamFn      agent.StreamFn // the provider stream (wrapped through retry)
    settings      compaction.CompactionSettings
}
```

```go
// compact.go:30-40
func newCompactionRunner(model ptypes.Model, streamFn agent.StreamFn) *compactionRunner
```

Returns nil when `streamFn` is nil or `model.ContextWindow <= 0` — no hook is wired, so compaction never runs for models without a known window.

```go
// compact.go:46-62
func (r *compactionRunner) hook(commit func([]ptypes.Message)) func(agent.ShouldStopAfterTurnContext) *agent.AgentLoopTurnUpdate
```

Returns a `PrepareNextTurnWithContext` callback. When `commit` is non-nil (the main agent path), the compacted message list is persisted into the agent via `Agt.SetMessages(compacted)`. Subagents pass nil — their child agent runs a single Prompt and doesn't need between-turn state updates.

```go
// compact.go:69-89
func (r *compactionRunner) maybeCompact(messages []ptypes.Message) ([]ptypes.Message, bool)
```

The core gate: estimates tokens, checks the threshold, converts to entries, prepares, runs `Compact`, applies. Returns `(messages, false)` on any failure — the loop keeps the original history rather than replaying an over-budget request blindly.

## How it works

The compaction pass has two phases: threshold check, and summarization + re-injection. Nothing rewrites history before an ordinary request.

### Phase 1: Threshold check (between turns)

After a turn completes, the agent loop calls `PrepareNextTurn` (which delegates to `PrepareNextTurnWithContext`):

```
AgentLoop.runAgentLoop
  → TurnEnd emitted
  → config.PrepareNextTurn(ShouldStopAfterTurnContext{...})
    → compactionRunner.hook(commit)
      → maybeCompact(c.Context.Messages)
        → EstimateContextTokens(messages)        ← includes provider usage + estimates
        → ShouldCompact(tokens, contextWindow, settings)
        → if under threshold: return (messages, false)   ← no-op
```

`EstimateContextTokens` prefers real provider usage from the last assistant message. It falls back to character-count estimates (÷4) for messages after the last usage-carrying message. This prevents double-counting: prior-compacted entries are excluded from estimation because `buildSessionContext` skips entries before the last compaction's `FirstKeptEntryID`.

### Phase 2: Summarization and re-injection

When the threshold is crossed:

```
        → messagesToEntries(messages)             ← flat []Message → []SessionEntry
        → PrepareCompaction(entries, settings)
          → find prev compaction, set boundary
          → FindCutPoint(boundary, keepRecentTokens)
          → extract messages to summarize (may split turn)
          → extract cumulative file operations
          → return *CompactionPreparation

        → context.WithTimeout(120s)
        → Compact(ctx, streamFn, model, prep)
          → GenerateSummary(messagesToSummarize, previousSummary)
            → SerializeConversation(messages)      ← text with truncated tool results
            → wrap in <conversation> + prompt
            → runSummarization(streamFn, model, maxTokens, promptText)
              → build fresh types.Context{SystemPrompt: SummarizationSystemPrompt, ...}
              → streamFn(ctx, model, reqCtx, opts).Result(ctx) ← BLOCKING oneshot
              → join text blocks → summary string
          → if split turn: GenerateTurnPrefixSummary(turnPrefixMessages)
          → append FormatFileOperations(readFiles, modifiedFiles)

        → ApplyCompaction(entries, prep, summary)
          → wrap summary → "The conversation history before this point was compacted..."
          → keptMessages from FirstKeptEntryID onward
          → if first kept message is user: mergeSummaryIntoUser (prepend text block)
          → return compacted []Message

        → commit(compacted)                        ← persist into Agent.SetMessages
        → return AgentLoopTurnUpdate{Context: &next}
```

The next provider request picks up the compacted context through `currentContext.Messages` (updated in the loop at `loop.go:236`).

### What the model sees after compaction

```
Before (20 entries, ~180k tokens):
  system | user | assistant | tool | tool | user | assistant | ... | user | assistant | tool

After compaction + re-injection:
  system | summary | user | assistant | tool | user | assistant | tool
           ↑                     ↑
    "The conversation            kept entries from
     history before..."          FirstKeptEntryID

Total: 6 entries, ~30k tokens (summary ~5k + kept ~25k)
```

The system prompt (including `GenericRules`) is identical before and after — compaction never touches it.

## Failure modes and invariants

### Compaction failure is non-fatal

If the summarizer returns an error, produces empty output, or times out, `maybeCompact` returns `(messages, false)`. The loop proceeds with the full history. The next turn may fail with a genuine context overflow, but the system never installs a corrupt or empty checkpoint.

### Double-compaction guard

`PrepareCompaction` returns nil when the last entry is already a compaction entry. This prevents back-to-back compaction — the summarizer just ran, the resulting summary is a single user message, and the total token count is well below threshold. Running compaction again would try to summarize the summary, which is noise.

### Empty-ID rejection

`PrepareCompaction` returns nil when the cut point has an empty entry ID. Without this, `ApplyCompaction` would search for `FirstKeptEntryID=""` in `keptMessages` and return an empty kept suffix — a silent context reset.

### Two consecutive user turns

Anthropic's API rejects two consecutive user messages. A non-split cut commonly lands on a user message (the cut point is the start of the kept region). `ApplyCompaction` handles this by merging the compaction summary as a text block into that user message, so the rebuilt sequence is `summary-in-user | assistant | ...` instead of `summary-user | user | ...`.

### Concurrent summarization

The summarization call uses `context.WithTimeout(context.Background(), 120s)`, not the agent's cancellation context. This is intentional: a user aborting the agent loop shouldn't kill an in-flight compaction that already started — the worst case is 120 seconds of waste, not a corrupted state. The go-routine scheduling is single-threaded at the compaction point: `PrepareNextTurn` is called synchronously between turns, so there's no race between compaction and the next streaming request.

### Stream truncation and compaction interaction

A truncated stream reaches the retry layer as `CodeStreamInterrupted` with `Retryable: true` from two places: the shared watchdog when no SSE data event arrives within its bound (`internal/provider/base/watchdog.go:111-130`, reported through `Watchdog.Err` at `watchdog.go:101-109`), and the OpenAI provider when a clean EOF never delivered a `finish_reason` (`internal/provider/openai/stream.go:171-175`). Both are retryable before or after output: the retrier announces the failed attempt with `EvRetry` and re-sends. But if the truncation is caused by context overflow, `Classify` catches it first as `CodeContextOverflow` (non-retryable, `internal/retry/error.go:121-124`), before the status and provider-code tiers run. The retrier then surfaces the error, the loop sees `StopError`, and the turn ends without compaction: the proactive compaction hook fires on the *next* cycle (if the caller retries `Prompt`/`Continue`). Reactive overflow recovery is out of scope for V1.

### Token estimation drift

`EstimateContextTokens` biases high: it counts character length ÷4 for messages without real provider usage data, and document refs weight at `EstimatedDocumentPageChars` per page. Over-counting triggers compaction earlier, the safe direction. Under-counting (triggering too late) would cause a real context overflow on the next provider request.

## TypeScript to Go

The TypeScript reference runs compaction as an async task in the Node event loop, with `AgentSession` managing a complex state machine of retry promises, compaction flags, and session-level events. Summarization is another `completeSimple()` call through the same provider pipeline, indistinguishable from a regular assistant turn except for tool choice suppression.

Go inverts this in three ways:

**1. Separate call path, not the same event loop**

TypeScript: the summarizer reuses the agent's `completeSimple()`, which plumbs through the same async request pipeline as a user prompt. The TS runtime schedules it on the event loop alongside stream processing and TUI events.

Go: `runSummarization()` builds a fresh, standalone `types.Context` with a different system prompt and calls `streamFn.Result(ctx)` — the blocking result collector. This is a synchronous call from the compaction hook, which itself runs synchronously inside `PrepareNextTurn`. No goroutine, no event multiplexing, no shared agent state besides the same `StreamFn`. The timeout is an explicit `context.WithTimeout`, not a byproduct of the event loop's tick cadence.

This matters because Go's `PrepareNextTurn` runs between turns without concurrency. The TypeScript reference can interleave a compaction task with stream processing; Go guarantees the compaction pass completes (or times out) before the next turn starts.

**2. Typed message-list surgery vs. session-entry state machine**

TypeScript: the session is an append-only log of `SessionEntry` objects. Compaction appends a `CompactionEntry` with metadata (`firstKeptEntryId`, `tokensBefore`, `details`). On rebuild, `buildSessionContext()` walks the tree, finds the latest compaction, reconstructs the LLM-facing context by skipping entries before `firstKeptEntryId` and injecting the summary as a `compactionSummary` role message, which `convertToLlm()` maps back to a user message through a static template.

Go: the session layer (`internal/session`) passes a flat `[]ptypes.Message` slice, not a tree-indexed entry log. `messagesToEntries` synthesizes `compaction.SessionEntry` wrappers with auto-incrementing string IDs (`"1"`, `"2"`, …). The compaction engine operates on these synthetic entries, then `ApplyCompaction` returns a flat `[]ptypes.Message` — the summary wrapped in prefix/suffix text and merged into the first kept user message. There is no `CompactionEntry` persisted to disk in V1 (Phase 3 adds JSONL session persistence), so compaction state lives in the message list itself. The summary is a `UserMessage` with semantically tagged content, not a custom role that needs a downstream translator.

**3. Dependency injection vs. live session state**

TypeScript: the summarizer accesses `this.model`, `this.settings`, and `this.#client` through the live `AgentSession` instance. Compaction is a method on the session object, entangled with retry state, the event emitter, and the TUI controller.

Go: `CompactionSettings` is a plain struct with no methods. `Compact()` takes a `StreamFn` as a parameter — the same function signature used by the agent loop. The compaction runner is a thin adapter (`compactionRunner`) that bridges the loop's `PrepareNextTurnWithContext` signature to the stateless compaction functions. The `commit` closure (which calls `Agt.SetMessages`) is the only mutation boundary, and it's supplied by the caller, not discovered from ambient state.

This is the "accept interfaces, return structs" pattern applied to compaction: `GenerateSummary` and `Compact` accept a `StreamFn` interface (the provider boundary), return concrete strings and errors. The compaction package has no dependency on session, agent, or any notion of "who owns the message list."

## Where it lives

| File | Key symbols |
|---|---|
| `internal/compaction/compaction.go` | `ShouldCompact`, `PrepareCompaction`, `Compact`, `ApplyCompaction`, `GenerateSummary`, `runSummarization`, `SerializeConversation`, `EstimateContextTokens`, `FindCutPoint`, `FindTurnStartIndex`, `FormatFileOperations`, `CompactionSettings`, `CompactionPreparation`, `SessionEntry`, `CutPointResult` |
| `internal/compaction/compaction_test.go` | threshold tests, cut-point tests, build-session-context tests |
| `internal/compaction/compaction_apply_test.go` | `TestApplyCompactionMergesIntoKeptUser`, `TestApplyCompactionStandaloneBeforeAssistant`, `TestCompactGeneratesSummaryWithFileOps`, `TestCompactRejectsEmptySummary` |
| `internal/compaction/compaction_ref_test.go` | reference parity tests |
| `internal/session/compact.go` | `compactionRunner`, `newCompactionRunner`, `hook`, `maybeCompact`, `messagesToEntries` |
| `internal/session/compact_test.go` | sticky-rules compaction persistence test |
| `internal/session/stack.go:139-152` | `BuildAgentStack` wiring: `newCompactionRunner` → `runner.hook(Agt.SetMessages)` → `PrepareNextTurnWithContext` |
| `internal/agent/runtime.go:84,93` | `TransformContext` (optional per-request hook; the coding stack sets none), `PrepareNextTurnWithContext` (compaction hook) |
| `internal/agent/loop.go:227-249` | `PrepareNextTurn` call site in the turn loop |
| `internal/retry/error.go:100-124,179-198` | `Classify`, `isContextOverflow`, `classifyProviderCode`; the boundary between retry and compaction |
| `internal/retry/stream.go:45-97,103-159` | `Retrier.Stream`, `produce`, `forwardAttempt`, `plan`: transport retry that never touches compaction |
| `internal/provider/base/watchdog.go:101-130` | `Watchdog.Err`, `Watchdog.run`: a stalled stream becomes a retryable `CodeStreamInterrupted` |
| `internal/provider/openai/stream.go:171-175` | a stream that ends without `finish_reason` becomes a retryable `CodeStreamInterrupted` |
| `12-idiomatic-go-vs-typescript.md` | design rules: consumer-defined interfaces, accept-interface/return-struct, no public `any` |
