# 03 — Tools and hash-anchored editing

## The problem

An agent that edits files without verifying that the file still looks the way it did when the agent read it is an agent that will silently corrupt source. Line numbers drift the moment another process, a git checkout, or even the agent's own earlier hunk in the same multi-hunk edit touches the file. A `:50-100` slice of a 5000-line file is useless if the agent cannot ask for the next chunk without accidentally reading to EOF. And a tool system whose result details are `any` erases type safety at the boundary every consumer touches — the JSON is already parsed but you cannot write a type switch against it.

Before the hash-anchored edit model, there were exactly two strategies, and both lose:

- **Line-number edits.** Send `{line: 42, newText: "..."}`. The file gains or loses three lines above line 42 between read and edit. The edit lands on the wrong code. No error.
- **Exact-text edits without an anchor.** Send `{oldText: "...", newText: "..."}`. The oldText appears zero times (someone deleted the function) or twice (someone duplicated the block). The tool either fails with a confusing "not found" or replaces the wrong match. No stale-detection.

The read tool had its own problems. A plain `cat`-style read of a 3000-line Go file floods the model with bodies it does not need — the agent wants the package, the imports, and one line per declaration so it can decide *which* functions to read in full. The TypeScript reference agent's approach of showing the first N lines with an `offset=N` continuation meant that a truncated `file:1-200` would advertise `offset=201`, and following that offset would read past line 200 — past the requested range — without any warning.

And at the result boundary, `Details any` meant every adapter, every test, and every hook callback had to type-assert from `interface{}` with no compile-time guarantee that the assertion matched what the tool actually produced. A tool changing its details struct was a silent runtime panic waiting for an integration test.

## Key decisions and the thinking process

### Hash-anchored edits: the full SHA-256, not a prefix

The core insight is that a read and an edit form a two-phase transaction over file bytes, not line numbers. The read tool returns a content anchor — a deterministic digest of the exact raw bytes it read. The edit tool receives that anchor back on every hunk and re-reads the file *while holding the mutation lock*, recomputes the anchor from the current bytes, and rejects the edit before any mutation if the anchors do not match.

The anchor **must be the full SHA-256 hex digest (64 characters)**. An earlier implementation truncated to 4 hex characters (16 bits) and shipped with a demonstrated collision: the byte sequences `content-83` and `content-194` both produced anchor `4884`. An edit guarded by the first content's anchor could silently pass after the file changed to the second content. The fix was straightforward: `AnchorLength = sha256.Size * 2` and `hex.EncodeToString(sum[:])` — no truncation. The anchor lives in a `[path#full64hex]` header on every read output; the edit tool's guard compares the full string against a digest it recomputes from the current raw bytes.

Why SHA-256 over a faster hash? The anchoring happens once per read and once per edit — not in a hot loop. Correctness dominates. A 16-bit digest was a correctness bug; a 64-bit digest would be a correctness bet. A full cryptographic hash is a correctness guarantee.

### `StaleAnchorError`: pointer receivers and `Is`

The stale error is a named struct with a pointer receiver so that `errors.As` works the way Go programmers expect:

```go
var stale *editdiff.StaleAnchorError
errors.As(err, &stale)  // works because the error chain wraps a *StaleAnchorError
```

The sentinel `ErrStaleAnchor` supports `errors.Is` for callers that only care about the class, not the expected/actual values. An earlier draft used a value receiver, which broke `errors.As(&stalePointer)` — the value in the chain did not match the pointer target. The fix: always wrap `&editdiff.StaleAnchorError{...}` with `%w`, use a pointer receiver on `Error()`, and implement `Is(target error) bool` to match `ErrStaleAnchor`.

### Multi-hunk atomicity without a filesystem transaction

The edit tool accepts an array of `edits`, each a `{old_text, new_text, anchor?}`. The question: if hunk 3 is stale but hunks 1 and 2 are fine, should we apply 1 and 2? The answer: no. Partial application leaves the file in a state the agent does not expect — hunk 3's `newText` was written assuming hunks 1 and 2 landed, and now the file is half-transformed.

The implementation enforces all-or-none in three layers:

1. **Per-file `FileMutationQueue`.** `queue.Do(absolutePath, fn)` acquires a mutex keyed on the resolved real path (symlinks evaluated). No two edits to the same real file interleave.
2. **Anchor check before any mutation.** Inside the lock, the code re-reads the file, computes the current anchor, and checks every supplied hunk anchor against it. Any mismatch returns `StaleAnchorError` immediately — `os.ReadFile` has been called but no write has occurred.
3. **`ApplyEditsToNormalizedContent` validates all hunks first.** It finds every `old_text` match, rejects not-found/duplicate/overlapping/no-op edits, and only then constructs the new content. If any hunk is invalid, no replacement string is built and no write occurs.

This is logical all-or-none, **not crash-safe atomicity**. `os.WriteFile` truncates and writes in place; a crash mid-write can leave a partial file. For the harness's threat model — an agent modifying source files on a developer's machine — this is acceptable. A production file-sync tool would use a temp-file + rename pattern. The code documents this distinction clearly.

### The structural read outline: overflow form, body-free, recovery selectors

When the read tool receives no selector, no offset, no limit, and `mode=auto` (the default), it returns the whole file as numbered lines whenever they fit the normal output budget (2000 lines, 50KB). Only when they don't, and the file is a parseable Go source file, does it return a structural outline instead, headed by a note that says it is an outline and how to read a range (`mode=outline` asks for it at any size):

```
[path/to/file.go#full64hex]
package main

import (...)

func NewTool(toolName ToolName, ...) (agent.AgentTool, error)   [path/to/file.go:180-224]
func CodingTools(cwd string, ...) []agent.AgentTool              [path/to/file.go:226-229]
type readToolInput struct { ... }                                [path/to/file.go:138-143]
```

Every declaration carries an exact `path:START-END` recovery selector so the agent can request the raw region. The outline is **body-free at every nesting depth**: `renderType` recursively elides struct and interface member bodies, even those nested inside maps, slices, channels, function types, and generic parameters. Type aliases, named underlying types, and generic constraints survive. Composite and function literals in initializers are elided.

Why the output budget as the threshold, and not parseability alone? An earlier version outlined every parseable Go file. In the cross-harness benchmark (`context/bench/results/20261023T131649Z/analysis.md`) 63 of 77 Tempest reads came back as outlines; the model needs exact text to edit, re-read the same 9-line file 17 times, and hashline rejected its edits because outline rows are not displayed lines. A file that fits one read loses nothing by being shown whole, and every shown row becomes editable. The outline earns its place only when the whole file can't be shown, where it beats paging blind. Any explicit selector, offset, limit, or `mode=raw` returns numbered lines.

Truncation continuations preserve the selector bounds: a truncated `file:1-2500` advertises `file:2001-2500`, not a bare `offset=2001` that could read past line 2500. A truncated outline tells the agent to read a declaration's range or page with `mode=raw` and `offset`.

For binary files (detected by a NUL byte in the first 8KB), the read tool returns `[binary file, N bytes, not shown]` with `Details.Binary = true`.

### `json.RawMessage` for Details, not `any`

The `AgentToolResult.Details` field is `json.RawMessage`. This is a deliberate choice with two justifications:

**Deferred decoding.** The generic tool-execution path does not know what struct each tool's details will be. It receives the result from `Execute`, packages it into an event, and hands it to adapters. With `json.RawMessage`, the generic path never decodes — it passes raw JSON bytes. The adapter or test that *knows* it is looking at a read result can decode into `readToolDetails`; an edit consumer decodes into `editToolDetails`. No type assertion, no `interface{}` cast, no silent nil.

**Marshal errors are not dropped.** The unexported `textResult` function accepts `any` (it is the internal producer's convenience) but marshals immediately:

```go
func textResult(text string, details any) agent.AgentToolResult {
    var raw json.RawMessage
    if details != nil {
        encoded, err := json.Marshal(details)
        if err != nil {
            encoded, _ = json.Marshal(map[string]string{"details_marshal_error": err.Error()})
        }
        raw = encoded
    }
    return agent.AgentToolResult{Content: ..., Details: raw}
}
```

If marshal fails — a programming error, since details structs are controlled — the error is embedded in the JSON rather than silently dropped. The consumer sees `{"details_marshal_error": "..."}` instead of `null` or a partial struct. This is a defense-in-depth choice: a bug that would be a silent data loss in a TypeScript `any` world becomes an observable error on the wire.

The `Details` field was `any` in an earlier iteration. The migration touched `AgentToolResult`, `AfterToolCallResult`, every tool's `textResult` call, and every adapter/test consumer. There is now no exported `any` details API in the agent types.

### The tool registry: functions, not an interface

`AgentTool` is a struct, not an interface. Its `Execute` is a `func` field:

```go
type AgentTool struct {
    types.Tool
    Label            string
    PrepareArguments func(args json.RawMessage) json.RawMessage
    ExecutionMode    ToolExecutionMode
    Execute          func(ctx context.Context, toolCallID string, params json.RawMessage, onUpdate AgentToolUpdateCallback) (AgentToolResult, error)
}
```

This is a closure-based pattern: `newReadTool` returns an `AgentTool` whose `Execute` closure captures `cwd` and `options`. There is no `ReadTool` struct implementing a `Tool` interface. Why? Because every tool's `Execute` has the same signature but completely different dependencies — the reader needs a `ReadResolver`, the editor needs a `FileMutationQueue`, the web search tool needs an HTTP client. An interface would force every tool into a single constructor shape or a fat config struct. Closures let each `new*Tool` function capture exactly what it needs.

The registry itself is a plain `switch` in `NewTool(toolName ToolName, cwd string, options ToolsOptions) (agent.AgentTool, error)`. Tool names are `const` strings (`ReadTool = "read"`, `EditTool = "edit"`, …) so a typo is a compile error. Convenience functions (`CodingTools`, `ReadOnlyTools`, `AllTools`) batch-construct groups. Unknown tool names return an error; tools with unmet prerequisites (e.g., `web_search` without `EnableWeb`) return an error at construction time, not at execution time.

## Signatures and types

### Tool contract (`internal/agent/types.go:44-60`)

```go
type AgentToolResult struct {
    Content   []types.ContentBlock   // Blocks for the model: text, image, etc.
    Details   json.RawMessage        // Tool-specific structured metadata; never decoded generically.
    Terminate bool                   // True if the tool result should end the turn.
}
```

`Content` is what the model sees. `Details` is what adapters, tests, and hooks decode — it carries the diff for an edit, the truncation info for a read, etc. The generic loop never looks inside `Details`.

```go
type AgentTool struct {
    types.Tool                       // Name, Description, Parameters (JSON Schema)
    Label            string          // Stable display label for logs and events.
    PrepareArguments func(args json.RawMessage) json.RawMessage  // Pre-processor; nil means pass-through.
    ExecutionMode    ToolExecutionMode  // Sync or queued execution policy.
    Execute          func(ctx context.Context, toolCallID string, params json.RawMessage, onUpdate AgentToolUpdateCallback) (AgentToolResult, error)
}
```

`PrepareArguments` is an escape hatch that normalizes legacy input shapes before the tool sees them. The edit tool uses it to convert `{old_text, new_text}` into the `{edits: [...]}` array form.

### Content anchoring (`internal/editdiff/editdiff.go:33-64`)

```go
const AnchorLength = sha256.Size * 2  // 64 hex characters.

func ContentAnchor(content []byte) string   // Full SHA-256 hex digest of raw file bytes.

var ErrStaleAnchor = errors.New("stale content anchor")   // Sentinel for errors.Is.

type StaleAnchorError struct {
    Path     string   // The file path the caller tried to edit.
    Expected string   // The anchor the caller supplied (from a prior read).
    Actual   string   // The anchor computed from the current file bytes.
}
// *StaleAnchorError implements Error() and Is(target error) bool.
```

The `Is` method matches `target == ErrStaleAnchor`, so both `errors.Is(err, ErrStaleAnchor)` and `errors.As(err, &stalePointer)` work.

### Edit application (`internal/editdiff/editdiff.go:66-83,197-201`)

```go
type Edit struct {
    OldText string   // Exact text to find; must be unique and non-empty.
    NewText string   // Replacement text.
    Anchor  string   // Optional content anchor from read output; empty means skip guard.
}

type AppliedEditsResult struct {
    BaseContent string   // The LF-normalized content before edits.
    NewContent  string   // The LF-normalized content after all edits applied.
}

func ApplyEditsToNormalizedContent(normalizedContent string, edits []Edit, path string) (AppliedEditsResult, error)
```

Validates: no empty `OldText`, every `OldText` found, every `OldText` unique, no overlap between edit regions, result must differ from input. Supports fuzzy matching via NFKC normalization and smart-quote folding.

### Mutation queue (`internal/toolio/toolio.go:20-61`)

```go
type FileMutationQueue struct { /* unexported: mu, locks map */ }

func NewFileMutationQueue() *FileMutationQueue

func (q *FileMutationQueue) Do(filePath string, fn func() error) error
    // Acquires the per-real-path lock, runs fn, releases. Discards fn's return value.

func DoValue[T any](q *FileMutationQueue, filePath string, fn func() (T, error)) (T, error)
    // Same as Do but returns fn's value. The key resolves symlinks via filepath.EvalSymlinks.
```

Different files are parallel-safe; same-file operations serialize on the resolved real path.

### Tool construction (`internal/tools/tools.go:46-78,183-255`)

```go
type ToolName string

const (
    ReadTool  ToolName = "read"
    BashTool  ToolName = "bash"
    EditTool  ToolName = "edit"
    WriteTool ToolName = "write"
    GrepTool  ToolName = "grep"
    FindTool  ToolName = "find"
    LsTool    ToolName = "ls"
    TodoTool  ToolName = "todo_write"
    TaskTool  ToolName = "task"
    SkillTool ToolName = "skill"
    WebSearch ToolName = "web_search"
    WebFetch  ToolName = "web_fetch"
)

func NewTool(toolName ToolName, cwd string, options ToolsOptions) (agent.AgentTool, error)
func CodingTools(cwd string, options ToolsOptions) []agent.AgentTool
func ReadOnlyTools(cwd string, options ToolsOptions) []agent.AgentTool
func AllTools(cwd string, options ToolsOptions) map[ToolName]agent.AgentTool
```

### Read tool details (`internal/tools/tools.go:138-167`)

```go
type readToolInput struct {
    Path   string   // May include a :START-END or :START+COUNT selector suffix.
    Offset *int     // 1-based start line; nil means from line 1.
    Limit  *int     // Max lines to return; nil means unlimited (subject to truncation caps).
    Mode   string   // "auto" (content, outline only for a Go file over the budget), "raw" (numbered lines) or "outline".
}

type readToolDetails struct {
    Truncation truncate.Result   // Line/byte cap info when output was truncated.
    Summary    bool              // True when output is a structural outline rather than raw lines.
    Binary     bool              // True when the file was detected as binary (NUL bytes).
}

type readPathSelector struct {
    Path  string   // Path stripped of the selector suffix.
    Start int      // 1-based inclusive start line.
    End   int      // 1-based inclusive end line.
    Set   bool     // False when no selector was present.
}
```

### Edit tool details (`internal/tools/tools.go:145-160`)

```go
type editToolInput struct {
    Path  string          // File to edit, resolved relative to cwd.
    Edits []editdiff.Edit // One or more find-and-replace hunks.
}

type editToolDetails struct {
    Diff             string   // Compact line-numbered diff of the change.
    Patch            string   // Standard unified diff patch.
    FirstChangedLine *int     // 1-based line of the first change, nil when unchanged.
}
```

### Internal result constructor (`internal/tools/tools.go:1420-1434`)

```go
func textResult(text string, details any) agent.AgentToolResult
    // Marshals details to json.RawMessage. Marshal errors become {"details_marshal_error": "..."}
    // rather than being silently dropped.
```

## How it works

### 1. Tool registration

At startup, `Config.Resolve()` builds a `ToolsOptions` and calls `AllTools(cwd, options)`, which calls `NewTool` for every known `ToolName`. `NewTool` is a `switch` that dispatches to `newReadTool`, `newEditTool`, etc. Each constructor returns an `AgentTool` whose `Execute` closure captures its dependencies. The resulting `map[ToolName]AgentTool` is stored in the runtime and used by the agent loop to look up tools by name when a model requests a tool call.

### 2. A read-edit cycle, step by step

**Read.** The model calls `read` with `{path: "foo.go"}`. `newReadTool`'s `Execute` closure fires:

1. Parse the optional `:START-END` selector from `input.Path`.
2. `os.ReadFile` the resolved absolute path. The raw bytes are stored in `data`.
3. If the file is an image (by extension), return the image block immediately.
4. Compute `header := fmt.Sprintf("[%s#%s]", displayPath, editdiff.ContentAnchor(data))`. The anchor is computed from the raw bytes, including any CRLF, BOM, and trailing newline bytes.
5. Detect binary: if `data` contains a NUL byte in the first 8KB, return `[binary file, N bytes, not shown]`.
6. If `mode=outline`: require a whole-file read of a parseable workspace Go file and return `goSourceOutline`'s body-free declaration outline.
7. Otherwise: split into lines, apply selector or offset/limit, number them, truncate. If `mode=auto` read the whole file, the numbered lines overflowed the caps, and the file is parseable Go, return the outline headed by a note on how to read a range; else emit the numbered rows with a continuation notice.

**Edit.** The model received `[foo.go#abc123...def456]` and now calls `edit` with `{path: "foo.go", edits: [{old_text: "...", new_text: "...", anchor: "abc123...def456"}]}`. Inside `newEditTool`'s `Execute` closure:

1. Resolve `absolutePath` from `input.Path` relative to `cwd`.
2. Call `queue.Do(absolutePath, fn)`. The queue resolves symlinks on `absolutePath` to get the real-path key, acquires the per-key mutex, and runs `fn`.
3. Inside `fn`, **re-read** the file: `rawContent, err := os.ReadFile(absolutePath)`.
4. Iterate all edits. For the first non-empty anchor, compute `actualAnchor = editdiff.ContentAnchor(rawContent)`. Compare every supplied anchor to `actualAnchor`. Any mismatch? Wrap a `&StaleAnchorError{...}` with `%w` and return immediately. **No write has occurred.**
5. Strip BOM, detect line endings, normalize to LF.
6. Call `ApplyEditsToNormalizedContent`. It normalizes edit `OldText`/`NewText` to LF, validates every `OldText` is found/unique/non-overlapping, and constructs `NewContent`. Any invalid hunk returns an error — the file is still unmodified.
7. Restore the original line endings, prepend BOM, `os.WriteFile` the final bytes.
8. Generate diff and patch for the details. Return success.

### 3. The mutation queue in detail

```go
func mutationQueueKey(filePath string) (string, error) {
    resolved, _ := filepath.Abs(filePath)
    real, err := filepath.EvalSymlinks(resolved)
    // If the file doesn't exist yet (write tool), use the unresolved absolute path.
    if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
        return resolved, nil
    }
    return real, nil
}
```

The key is the real path after symlink resolution. Two symlinks pointing to the same file serialize on the same lock. A `write` to a not-yet-existing path uses the absolute unresolved path — two writes creating the same file through different symlinks are independently serialized, which is the correct behavior since the second write sees the first's result.

The lock is reference-counted: `acquire` increments `refs`, `release` decrements and deletes the lock from the map when `refs` hits zero. This prevents a lock from accumulating for every file ever touched.

### 4. The structural Go outline

`goSourceOutline(displayPath, sourcePath, data)` (`internal/tools/read_summary.go:28-73`):

1. `parser.ParseFile` the source into an `*ast.File`.
2. Render the package clause and import block verbatim (to preserve import grouping).
3. For each top-level declaration, call `outlineDeclaration`.
4. `outlineDeclaration` renders functions with their receiver, name, type parameters, parameters, and results — all via `renderType` which recursively elides `*ast.StructType` and `*ast.InterfaceType` member bodies.
5. Each declaration gets a `[path:START-END]` recovery selector from the `token.FileSet` position.
6. The result is truncated to the standard line/byte caps. If truncated, the note points at the recovery selectors and at `mode=raw` paging.

## Failure modes and invariants

### Invariant: read anchor covers raw bytes, not normalized text

The read tool computes `ContentAnchor(data)` from the raw `[]byte` returned by `os.ReadFile`. The edit tool recomputes from `os.ReadFile` while holding the mutation lock. Both call the same `ContentAnchor` over raw bytes. The anchor includes CRLF bytes, BOM, trailing newlines — everything. If anything in the file changes, the anchor changes. This is the foundation the stale guard rests on.

### Invariant: stale rejection happens before any mutation

In `newEditTool.Execute`, the sequence inside `queue.Do` is:

1. `os.ReadFile` (re-read, not passed from caller)
2. Anchor comparison (may return `StaleAnchorError`)
3. `ApplyEditsToNormalizedContent` (may return parse/duplicate/overlap errors)
4. `os.WriteFile`

Steps 1 and 2 are the guard. If step 2 fails, steps 3 and 4 never execute. The file is read but not written. This is guaranteed by Go control flow — the `return staleErr` at line 715 exits `fn` before reaching `ApplyEditsToNormalizedContent` at line 720.

### Failure: mid-edit file change between re-read and write

The mutation queue serializes same-file operations. Between the re-read at step 1 and the write at step 4, no other goroutine can touch the same file through the queue. But an external process (git checkout, another editor) can. This is a TOCTOU window inherent to any userspace file editing. The anchor guard shrinks the window dramatically — the file must change *between* the re-read and the write, not between the model's read and the edit — but does not eliminate it. The harness judges this acceptable for a developer tool.

### Failure: binary files reach the text path

Files without a recognized image extension but containing NUL bytes are detected by `containsNUL(data)` which scans the first 8KB. This catches most binary formats (compiled objects, archives, PDFs) but a binary file whose first 8KB happen to be NUL-free will be treated as text. The output will be garbled but bounded by truncation caps. There is no magic-number database. This is a known coverage gap tracked as acceptable for a coding agent's file reader.

### Invariant: multi-hunk edits are validated atomically

`ApplyEditsToNormalizedContent` does not apply hunks incrementally. It finds all matches first, checks for duplicates and overlaps across the full set, and only then constructs the replacement. A call with 5 valid hunks and 1 missing `oldText` returns an error — none of the 5 are applied. The `matched` slice is built for all edits before `sortMatchedEdits` is called, and overlap detection runs on the sorted complete set. No partial application.

### Failure: marshal errors in details

`textResult` marshals its `details any` argument. If `json.Marshal` fails — which should never happen for controlled structs like `readToolDetails` — the error string becomes the details payload: `{"details_marshal_error": "json: unsupported type: ..."}`. The consumer sees a well-formed JSON object rather than `null`. This defense-in-depth prevents a programming error from becoming silent data loss.

### Invariant: truncation continuation preserves selector bounds

When a selector read like `foo.go:1-2500` is truncated at the line cap, the continuation notice uses `foo.go:2001-2500` (the clamped end), not `offset=2001`. A bare offset read would continue past line 2500. When no selector is present, the continuation uses `offset=N` as before — there is no end bound to preserve.

## TypeScript to Go

### `unknown` / `any` details → `json.RawMessage`

The TypeScript reference agent used `details?: unknown` on tool results. Every consumer cast it: `const d = result.details as ReadDetails`. The cast is a runtime promise with no compiler enforcement. If the tool's details shape changed, the cast silently returned the wrong shape and the consumer crashed on property access.

Go's approach: `json.RawMessage`. The field is `[]byte` of raw JSON. The generic tool-execution path never decodes it. When an adapter knows it has a read result, it calls `json.Unmarshal(result.Details, &details)` into a concrete `readToolDetails` struct. If the shape is wrong, `Unmarshal` returns an error. No silent wrong-shape propagation.

The trade-off: Go loses the TypeScript convenience of `result.details.truncation.lines` without a decode step. But the harness's tool results cross process boundaries (adapter HTTP responses, event streams, persisted JSONL). Raw JSON is already the wire format — `json.RawMessage` is zero-cost at the boundary because the bytes are never re-serialized. In TypeScript, `JSON.parse(JSON.stringify(details))` is a round-trip through serialization at every boundary; in Go, the bytes pass through unchanged.

### Dynamic tool maps → typed registry with closures

The TypeScript reference built tools by iterating a `Record<string, ToolFactory>` and calling each factory with a shared config blob. Adding a tool meant adding an entry to the record.

Go uses `NewTool(toolName ToolName, ...)` with a `switch` on `ToolName` constants. `ToolName` is a defined string type, not a bare `string`. Each `case` calls a constructor that returns an `AgentTool` with a closure. The `switch` is exhaustive — unknown tool names hit `default` and return an error. This is more verbose than a map of factories, but:

- Missing a `case` for a new `ToolName` constant is a compile-time exhaustive-check warning (with `go vet` or a linter), not a silent nil tool.
- Each constructor's closure captures only its dependencies. There is no fat config struct that every tool receives but only three fields are relevant to each.
- The `PrepareArguments` hook lets tools normalize their input once at construction time rather than on every execution.

### Event-loop single-threaded mutation → per-file mutex

The TypeScript reference ran in a single-threaded event loop. File mutations were naturally serialized — no two async operations interleaved because nothing preempted between `await readFile` and `await writeFile`.

Go has goroutines. Two concurrent tool calls can touch the same file simultaneously. The `FileMutationQueue` replaces the accidental serialization of the event loop with explicit per-real-path mutual exclusion. The key difference: in TypeScript, you get serialization for free but at the cost of blocking the entire agent on every I/O operation. In Go, only same-file operations serialize; different files proceed in parallel.

## Where it lives

| What | File | Key symbols |
|---|---|---|
| Tool contract | `internal/agent/types.go:44-97` | `AgentTool`, `AgentToolResult`, `AfterToolCallResult` |
| Tool registry | `internal/tools/tools.go:46-255` | `ToolName`, `NewTool`, `CodingTools`, `ReadOnlyTools`, `AllTools` |
| Read tool | `internal/tools/tools.go:472-606` | `newReadTool`, `readToolInput`, `readToolDetails`, `readPathSelector` |
| Edit tool | `internal/tools/tools.go:654-748` | `newEditTool`, `editToolInput`, `editToolDetails`, `prepareEditArguments` |
| Read summary | `internal/tools/read_summary.go` | `goSourceOutline`, `outlineDeclaration`, `renderType`, `renderValue` |
| Content anchor | `internal/editdiff/editdiff.go:33-64` | `ContentAnchor`, `AnchorLength`, `StaleAnchorError`, `ErrStaleAnchor` |
| Edit application | `internal/editdiff/editdiff.go:197-274` | `ApplyEditsToNormalizedContent`, `ApplyReplacementsPreservingUnchangedLines` |
| Mutation queue | `internal/toolio/toolio.go:20-97` | `FileMutationQueue`, `Do`, `DoValue`, `mutationQueueKey` |
| Result constructor | `internal/tools/tools.go:1420-1434` | `textResult` |

Tests: `internal/tools/tools_test.go` (read/edit/write integration, anchor rejection, selector parsing), `internal/editdiff/editdiff_test.go` (ContentAnchor, ApplyEditsToNormalizedContent, diff generation, collision regression).
