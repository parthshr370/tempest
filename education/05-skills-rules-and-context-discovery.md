# 05 - Skills, rules, and context discovery

## The problem

Before this subsystem, the harness started with a blank session. The agent had no idea what skills were installed, no context files to ground it in a project, and no persistent instructions that survived through a long conversation.

Worse, the codebase had **two incompatible skill shapes**: `skills.Skill` in the prompt/session layer and `content.SkillDef` in the tool layer. The skill tool couldn't find skills the prompt listed. The prompt couldn't show skills the tool could open. Every caller chose sides and the system didn't ship : `BuildAgentStack` passed zero skills to `ToolsOptions`, so the skill tool silently refused to register.

There was no containment for skill file reads. If a `SKILL.md` lived in `/home/user/.harness/skills/malware/`, a `skill://malware/../../../etc/passwd` URI could escape the base directory. A lexical prefix check (`strings.HasPrefix(resolved, baseDir)`) races with the filesystem: a symlink created between the check and the open walks right through. On Unix, opening a FIFO named pipe blocks until a writer connects : so a `skill://name/fifo` URI could hang the resolver forever, unkillable by context cancellation because `root.Open` doesn't take a context.

Rules were lost on compaction. Messages get summarized into a synthetic replacement every few turns, and any rule body injected as a message would evaporate with the rest of the transcript. The only way to keep rules alive was to edit the engine loop : and we treat the streaming engine as a stable core we don't touch for features.

Finally, `@file` imports in context files needed the same care: fence-aware token extraction, cycle detection, depth limits, and safe expansion without executing anything or leaking confidential file contents into diagnostics.

## Key decisions and the thinking process

### 1. Unify on `skills.Skill` and delete `internal/content`

The fork happened because the tool layer wanted something eager : `content.SkillDef` held `Body` as a string, parsed frontmatter with a stricter parser, and was indexed by `content.Find`. The prompt layer used `skills.Skill`, which stored metadata but no body, indexed by `skills.Find`.

The decision: **one canonical type, body loaded on demand.** `skills.Skill` became the single runtime shape. `content.SkillDef`, `content.Listing`, and `content.Find` were deleted after every import and test was migrated. No alias, no facade, no compatibility shim. `LoadBody(ctx, skill) (string, error)` reads the file at `Skill.FilePath`, strips frontmatter, and returns the body : once, per invocation, without storing every skill body for the entire session.

Why not keep both and bridge them? Two identical-but-not-identical types always drift. Someone adds a field to one, forgets the other, and now the skill tool opens a different file than the prompt displayed. One type, one index, one source of truth.

### 2. `os.Root` containment instead of lexical prefix checks

The first instinct from the TypeScript reference is to check that the resolved path starts with the base directory:

```
if !strings.HasPrefix(resolvedPath, baseDir) { reject }
```

This is wrong for two reasons in Go:

**Symlink TOCTOU.** Between the prefix check and `os.Open`, another process (or a skill author's repo setup) replaces a subdirectory with a symlink to `/etc`. The check passes, the open follows the link. `os.Root` (Go 1.26) closes this race at the kernel level : the returned handle can access only files within the rooted tree, period. Any path that resolves outside returns an error from the root itself, not from our code.

**Symlink awareness at rest.** `filepath.EvalSymlinks` + lexical containment catches some cases but misses dangling symlinks and requires re-evaluation to know what `..` resolves to after link following. `os.Root` handles it in the VFS layer.

The resolver opens the base directory once via `os.OpenRoot(skill.BaseDir)`, then uses `root.Stat(target)` and `root.Open(target)` for every path. If the target escapes, the root says no : not our code.

### 3. `Stat` before `Open`: classify BEFORE blocking

The original resolver code called `root.Open(target)` first, then checked `info.Mode().IsRegular()`. On Unix, `open()` on a FIFO read-end blocks until a writer appears. Since `os.OpenRoot` delegates to the kernel's `openat`, and `openat` doesn't return until the open completes, the resolver could block indefinitely : context cancellation is checked *after* the open, not during it.

The fix: call `root.Stat(target)` first. `Stat` never blocks on special files. Check `info.Mode().IsRegular()`. Only then call `root.Open(target)`. After open, re-`Stat` through the file handle and recheck `IsRegular` : this defends against a swap between the initial stat and the open (the file could be replaced with a symlink to a FIFO after our stat but before our open; rechecking through the open handle catches what we actually got).

This is the containment threat model in one sentence: **Stat first to classify without blocking, Open only regular files, revalidate through the open handle.**

### 4. Sticky `RULES.md` through `GenericRules` and the system prompt

`RULES.md` files contain unconditional instructions ("use checked inputs", "never log credentials"). They must persist across every model call, including after compaction.

The insight: the system prompt is **never compacted**. Compaction transforms only `Messages` : the conversation transcript. The system prompt is stored once at `AgentState.SystemPrompt` and copied into every provider context:

```
// internal/session/stack.go:154-155
agt = agent.NewAgent(agent.AgentOptions{
    InitialState: &agent.AgentState{SystemPrompt: systemPrompt, ...},
    ...
})
```

And in the agent loop:

```
// internal/agent/loop.go:315-341 : every provider dispatch:
cctx := types.Context{SystemPrompt: agt.State().SystemPrompt, ...}
```

By placing rule bodies directly into `StackConfig.GenericRules`, which `BuildSystemPrompt` renders into the system prompt template, the rules are automatically reattached to every request. Compaction replaces messages; the system prompt stays. No engine modification, no synthetic messages, no special compaction exclusion logic.

`PromptRule` (conditional rules with globs and names) is separate : those are metadata for domain-specific activation. V1 `RULES.md` is unconditional, so it lands in `GenericRules`.

### 5. Fixed-source discovery, not a provider registry

The TypeScript reference has an extensible provider system with precedence matrices and per-provider enable/disable. V1 doesn't need that : the sources are known and small:

- User context: `<AgentDir>/AGENTS.md`
- User rules: `<AgentDir>/RULES.md`
- Project context: nearest `<ancestor>/.harness/AGENTS.md`
- Standalone context: `AGENTS.md` on the ancestor chain (cwd to repo root)
- Project rules: nearest `<ancestor>/.harness/RULES.md`
- Skills: `<AgentDir>/skills`, `<cwd>/.harness/skills`, and explicit paths

`discovery.Discover` orchestrates these in one shot at startup. It returns concrete `SessionInputs` : no provider interface, no factory, no registry. This is a startup function, not a runtime service. The caller wires the results into `StackConfig` and we're done.

### 6. Consumer-owned resolver interface

The `skills.Resolver` is a concrete type in `internal/skills`. But the read tool lives in `internal/tools`. Who owns the interface? The consumer:

```go
// internal/tools/tools.go:106-110
type ReadResourceResolver interface {
    Resolve(ctx context.Context, uri string) (resource.Content, error)
}
```

This follows module 12 (idiomatic Go vs TypeScript) to the letter: accept interfaces, return structs. The tool package says "I need something that resolves URIs" in its own vocabulary. The skills package provides the concrete implementation. No circular dependency, no package-global registry.

### 7. Dot-directory skip before I/O

The review caught a bug: discovery called `readCandidate(ancestor/AGENTS.md)` then checked if the ancestor was a dot-directory. If `.private/AGENTS.md` existed but was a directory, `readCandidate` would fail with a typed error, stopping the ancestor walk.

Fix: check `strings.HasPrefix(filepath.Base(ancestor), ".")` before the read. The dot-directory guard applies before any I/O. The regression tests cover both readable files and directory nodes under dot-directory ancestors.

## Signatures and types

### Shared resource value

```go
// internal/resource/resource.go:11-16
type Content struct {
    URI       string   // normalized identity shown to the model (skill://name/path), never disk path
    MediaType string   // MarkdownMediaType or TextMediaType
    Data      []byte   // owned by caller
}
```

`URI` is the contract with the model: the read tool displays `skill://review/notes.md#A1B2`, never `/home/user/.harness/skills/review/notes.md`. This prevents path leakage and keeps skill internals opaque.

### Canonical skill

```go
// internal/skills/skills.go:37-50
type Skill struct {
    Name                   string     // validated: lowercase, hyphens, digits only
    Description            string     // required, max 1024 chars
    WhenToUse              string     // optional guidance
    AllowedTools           []string   // optional tool restriction
    Model                  string     // optional model override
    Fork                   bool       // optional context fork flag
    Paths                  []string   // optional asset paths
    FilePath               string     // absolute, canonical path to SKILL.md
    BaseDir                string     // absolute base for root containment
    SourceInfo             SourceInfo // where it was loaded from
    DisableModelInvocation bool       // hide from system-prompt listing
}
```

- `FilePath` is always absolute and canonical (`filepath.EvalSymlinks` applied). This is the disk location of the `SKILL.md` file : never shown to the model.
- `BaseDir` is the directory containing `SKILL.md` (or the directory itself for a single-file skill). This is the containment root.
- `SourceInfo` tracks origin (`user`, `project`, `path`) and scope.

```go
// internal/skills/skills.go:135-136
func LoadSkills(ctx context.Context, options LoadOptions) (LoadResult, error)

// internal/skills/skills.go:367-368
func Find(items []Skill, name string) (Skill, bool)

// internal/skills/skills.go:378
func LoadBody(ctx context.Context, skill Skill) (string, error)
```

- `LoadSkills`: loads user defaults, project defaults, then explicit paths. First-wins by name, canonical-path dedup, collision diagnostics collected.
- `Find`: exact-name lookup on a flat slice. Used by the skill tool at invocation time.
- `LoadBody`: reads `Skill.FilePath`, strips YAML frontmatter if present, returns the instruction body. On-demand, not cached.

### Skill URI resolver

```go
// internal/skills/resolver.go:56-59
type Resolver struct{ /* immutable name index */ }

// internal/skills/resolver.go:62
func NewResolver(items []Skill) (*Resolver, error)

// internal/skills/resolver.go:84
func (r *Resolver) Resolve(ctx context.Context, rawURI string) (resource.Content, error)
```

- `NewResolver`: validates every skill (name, absolute paths, file within base, no duplicates). Returns a construction error if any fails : the resolver is either valid or not constructed.
- `Resolve`: the containment algorithm. See the "How it works" section for the full trace.

Error codes: `invalid_uri`, `unknown_skill`, `invalid_path`, `path_escape`, `not_found`, `invalid_target`, `read_failed`. `ResolveError.Unwrap()` preserves the underlying `fs.ErrNotExist` for `not_found`.

### Context import expansion

```go
// internal/contextfile/imports.go
const MaxImportDepth = 5

type ExpandOptions struct {
    HomeDir  string // tilde resolution, still inside the root; empty = leave ~ literal
    MaxBytes int    // bytes all imports may read together; negative = no limit
}

func Expand(ctx context.Context, root *os.Root, filePath string, content []byte, options ExpandOptions) (text string, complete bool, err error)
```

- `filePath` is made absolute once. Relative imports resolve against the importing file's directory at each level.
- Every import is opened through `root`: a `../` or absolute path outside it, or a symlink out of it, leaves the token literal.
- `~` and `~/...` resolve against `HomeDir`. Missing `HomeDir` leaves the token literal.
- Imports stop reading at `MaxBytes`; `complete` is false when they were cut.
- Cycle detection is global per expansion (same file skipped everywhere, not just on one branch).
- Missing/unreadable/directory targets leave the `@token` literal; they don't fail discovery.

Startup uses the repository root for repository files (no `HomeDir`, so `~` stays literal) and the home directory, or the agent dir when it is outside home, for the user's own files. Nested discovery uses the working directory.

### Session discovery

```go
// internal/discovery/discovery.go:18-26
type Options struct {
    Cwd                  string   // required, canonicalized
    RepoRoot             string   // optional override; must contain cwd
    AgentDir             string   // required
    HomeDir              string   // optional tilde override
    SkillPaths           []string // explicit additional skill dirs/files
    IncludeDefaultSkills bool
}

// internal/discovery/discovery.go:45-52
type SessionInputs struct {
    ContextFiles []prompt.ContextFile
    Skills       []skills.Skill
    GenericRules []string     // user then project, one per non-empty RULES.md
    RepoRoot     string
    Diagnostics  []Diagnostic
}

// internal/discovery/discovery.go:90
func Discover(ctx context.Context, options Options) (SessionInputs, error)
```

- Returns concrete typed slices for direct `StackConfig` wiring. No intermediate abstractions.
- Diagnostics carry skill load warnings and name collisions. The CLI logs them at warn level : they don't block startup.

### Read tool seam

```go
// internal/tools/tools.go:106-110
type ReadResourceResolver interface {
    Resolve(ctx context.Context, uri string) (resource.Content, error)
}

// internal/tools/tools.go:113-128
type ToolsOptions struct {
    // ... existing fields ...
    Skills       []skills.Skill
    ReadResolver ReadResourceResolver
}
```

The read tool checks `strings.HasPrefix(selector.Path, "skill://")`. If true, dispatches to the resolver. If false, falls through to the existing `os.ReadFile` path. No generic scheme registry : one hardcoded prefix, one dispatch path.

## How it works

### Startup flow

```
cmd/harness/main.go
  → discovery.Discover(ctx, Options{...})        // [main.go:131-136]
    → canonicalize cwd, agentDir, home           // [discovery.go:94-108]
    → resolveRoot(cwd, supplied, home)           // [discovery.go:109]
      → find .git directory or file              // [discovery.go:254-292]
      → walk ancestors cwd→root                  // returns cwd-first list
      → fallback: user home, then volume root
    → readCandidate(agentDir/AGENTS.md)           // [discovery.go:119]
    → readCandidate(agentDir/RULES.md)            // [discovery.go:123]
    → for each ancestor (cwd→root):               // [discovery.go:130-161]
      → native: ancestor/.harness/AGENTS.md       // [discovery.go:134]
      → standalone: ancestor/AGENTS.md             // [discovery.go:138-143]
        → SKIP if ancestor basename starts with "." // [discovery.go:139]
      → first native wins; else nearest standalone
      → first .harness/RULES.md wins
    → expand context with @file imports           // [discovery.go:162-193]
    → reverse to root-first order                 // [discovery.go:172]
    → collapse identical expanded content → nearest // [discovery.go:184-193]
    → expand and append rule bodies               // [discovery.go:195-206]
    → LoadSkills(options)                         // [discovery.go:207]
  → session.BuildAgentStack(StackConfig{          // [main.go:144-156]
      ContextFiles: inputs.ContextFiles,
      Skills:       inputs.Skills,
      GenericRules: inputs.GenericRules,
    })
    → skills.NewResolver(cfg.Skills)              // [stack.go:106-112]
    → RebuildSystemPrompt(options)                // [stack.go:136-148]
    → agent.NewAgent(InitialState: {SystemPrompt}) // [stack.go:154-155]
```

### `skill://` resolution trace

```
Resolve(ctx, "skill://review/notes/a%20b.md")
  1. ctx.Err() check                                    // [resolver.go:85]
  2. url.Parse → scheme="skill", host="review"          // [resolver.go:88-91]
     Reject: wrong scheme, userinfo, RawQuery, ForceQuery,
             Fragment, hash in raw, empty host
  3. Exact host lookup in r.items                       // [resolver.go:92-95]
     Not found → ResolveUnknownSkill (wraps fs.ErrNotExist)
  4. EscapedPath = "/notes/a%20b.md"                    // [resolver.go:104-108]
     Trim "/" prefix → "notes/a%20b.md"
     url.PathUnescape → "notes/a b.md"
  5. Sanity checks on target:                           // [resolver.go:111-118]
     - Not absolute, no volume name, no NUL, no backslash
     - No ".." component (exact; rejects %2e%2e too)
     Accept "." components (cleaned away by filepath.Clean)
  6. filepath.Clean(target)                             // [resolver.go:119]
  7. os.OpenRoot(skill.BaseDir) → root handle            // [resolver.go:120]
  8. root.Stat(target) → FileInfo                       // [resolver.go:125]
     Not found → ResolveNotFound (or ResolvePathEscape if outsideBase)
     Not regular file → ResolveInvalidTarget
     ← KEY: Stat never blocks on FIFOs/devices
  9. root.Open(target) → file handle                    // [resolver.go:138]
  10. file.Stat() → revalidate IsRegular                // [resolver.go:149-155]
      ← second check catches Stat→Open race
  11. ctx.Err() → io.ReadAll(file) → ctx.Err()          // [resolver.go:156-165]
  12. MediaType: "text/markdown" if .md (case-insensitive) // [resolver.go:166-169]
      else "text/plain"
  13. Return Content{URI: "skill://review/notes/a%20b.md", ...}  // [resolver.go:170-177]
```

### AGENTS.md ancestor walk

The walk proceeds from `cwd` up to the repository root. For each ancestor directory:

1. Read `<ancestor>/.harness/AGENTS.md`. If present and we haven't yet selected a native context, this is the winner.
2. Otherwise, check `<ancestor>/AGENTS.md` : but only if the ancestor basename doesn't start with `.`. A `.private/AGENTS.md` is silently skipped.
3. After walking all ancestors, reverse the collected contexts to root-first order.
4. Collapse: if two contexts expand to identical content, keep only the nearer one (later in the reversed order). This catches the common case where `AGENTS.md` at the repo root and a subdirectory have the same content.

The walk uses `resolveRoot` to find the repo boundary. It looks for any `.git` node (directory or regular file : worktrees use `.git` files). If no git boundary is found, it falls back to the user home directory, then the volume root.

### `@file` import expansion

The expansion walks line-by-line through the source content, tracking fenced code blocks (``` or ~~~ with matching lengths) and inline backtick spans. Inside either, `@tokens` are left alone.

A candidate import starts when:
- The character is `@`
- It's at position 0 or follows a space/tab
- The next character matches `[./~A-Za-z0-9_-]`

The token extends to the next whitespace character. Trailing punctuation `.,;:!?)]}"'` is stripped from the token and reattached after the expanded content.

Resolution:
- `~` → HomeDir (literal if HomeDir is empty)
- `~/...` → joined with HomeDir
- Absolute → used as-is
- Anything else → resolved against the importing file's directory

The resolved path must lie inside the root and is opened through it, so a symlink cannot lead outside. Each resolved path is canonicalized (`filepath.EvalSymlinks` or `filepath.Clean`), checked against a global visited set, and depth-limited (`MaxImportDepth = 5`). At the depth limit, the file is read but its own imports stay literal : giving exactly five import edges from the root.

Missing files, directories, files outside the root, and unreadable targets leave the original `@token` literal, as do imports past the byte budget. Cycles and repeated includes also leave the literal. Only a missing root, source failures, and cancellation return errors.

### Sticky rule lifecycle

1. `discovery.Discover` reads `RULES.md` from agent dir and nearest `.harness/RULES.md`.
2. Each body is expanded (same `@file` pass as context files) and appended to `SessionInputs.GenericRules`.
3. `cmd/harness/main.go:155` passes `inputs.GenericRules` to `StackConfig.GenericRules`.
4. `BuildAgentStack` passes `cfg.GenericRules` to `RebuildSystemPrompt`, which forwards to `prompt.BuildSystemPrompt`.
5. The template renders `GenericRules` under `<generic-rules>` tags. The rendered system prompt is stored at `AgentState.SystemPrompt`.
6. On every provider dispatch, the agent loop copies `SystemPrompt` into the provider context.
7. Compaction replaces `Messages` only; `SystemPrompt` is untouched.
8. Result: rule bodies are present in every model request, including after compaction, without a single line of engine code changed.

## Failure modes and invariants

### Containment invariants

| Threat | Defense | Location |
|--------|---------|----------|
| Symlink escape to outside base | `os.Root` kernel-enforced sandbox | resolver.go:120-125 |
| `../` path traversal | Explicit component check before open | resolver.go:114-117 |
| `%2e%2e` encoded traversal | Decoded then checked; `..` rejected | resolver.go:105-117 |
| FIFO/device blocks resolver | `Stat` (nonblocking) before `Open` | resolver.go:125-135 |
| TOCTOU: file swapped between Stat and Open | Re-`Stat` through open handle | resolver.go:149-155 |
| Leaking disk paths to model | `Content.URI` is always `skill://...` | resolver.go:170-177 |
| `skill://alpha?x=1` or `skill://alpha#frag` | Rejected: empty query/fragment blocked | resolver.go:89 |

### Discovery invariants

| Invariant | Mechanism |
|-----------|-----------|
| Dot-directory AGENTS.md skipped before I/O | Basename check before `readCandidate` |
| Missing default skill dirs are not errors | `os.IsNotExist` → empty result |
| Identical context content picks nearest | Collapse pass after reverse |
| Root-first order, cwd-last | Reverse walk, then append mode |
| Name collisions don't block startup | Diagnostics collected, first-wins |
| Cancellation stops discovery at checkpoints | `ctx.Err()` between ancestors, between entries |

### Import expansion invariants

| Invariant | Mechanism |
|-----------|-----------|
| Git/email `@` tokens not expanded | `@` must follow space/tab or start of line |
| Fenced code blocks protected | At least 3 ` or ~, matching close length |
| Inline backtick spans protected | Delimiter parity tracking per line |
| Cycles don't infinite-loop | Global visited set, keyed by canonical path |
| Depth limited to 5 edges | `MaxImportDepth`; at limit, import stays literal |
| Missing imports don't fail discovery | Leave `@token` literal, continue |
| `~` without HomeDir leaves literal | Empty HomeDir → no-op |

### Sticky rule invariants

| Invariant | Mechanism |
|-----------|-----------|
| Rules survive every provider call | In system prompt, copied into each context |
| Rules survive compaction | Compaction transforms Messages, not SystemPrompt |
| Rules render exactly once per turn | `compactStrings` deduplicates |
| No engine change required | Uses existing `SystemPrompt` field in `AgentState` |

### Resolution error taxonomy

Every failure is typed. Callers can use `errors.Is` and `errors.As` to classify:

- `ResolveUnknownSkill` → the skill name doesn't exist in the index
- `ResolveNotFound` → the asset file doesn't exist; also unwraps `fs.ErrNotExist`
- `ResolveInvalidTarget` → target exists but isn't a regular file
- `ResolvePathEscape` → path resolves outside the base directory
- `ResolveInvalidPath` → path has NUL, backslash, is absolute, or malformed
- `ResolveReadFailed` → any other OS error (permission, IO error)
- `resolver_unavailable` (tool layer) → `read skill://...` called but no resolver registered

## TypeScript to Go

### Path sandboxing

**TypeScript (Node.js):** The reference checks path containment after the fact. `path.resolve(dir, userInput)` produces an absolute path, then `resolved.startsWith(baseDir + path.sep)` checks it's inside. This is a lexical check : it doesn't handle symlinks at all. Node has `fs.realpath` to resolve symlinks before checking, but that's a separate step, and the resolved path can change between the check and the open. Node's `fs.open` has no sandboxing mode.

**Go:** `os.OpenRoot(baseDir)` returns a directory handle that the kernel enforces. Every subsequent `root.Open(relativePath)` goes through `openat` with the rooted directory fd. The kernel resolves `..` and symlinks within the root's tree and rejects any path that would escape. No lexical check, no race, no `EvalSymlinks` dance. The resolver calls `OpenRoot` once at construction-time and uses the root for every resolution.

The FIFO hazard is also Go-specific: Go's `os.Open` and `root.Open` don't take a context, so there's no way to cancel a blocking open. Node's `fs.open` is async and takes an `AbortSignal` : `await fs.open(fifoPath, { signal: AbortSignal.timeout(1000) })` would time out. In Go, the fix is structural: don't open until you've stated.

### Interface ownership

**TypeScript:** The reference uses structural typing : anything with a `resolve(uri: string): Content` method satisfies the interface implicitly. This often leads to implicit coupling: the resolver is defined in one file, consumed in another, and TypeScript never complains because it's all structural.

**Go:** The `ReadResourceResolver` interface lives in `internal/tools` : the package that needs it. The concrete `*skills.Resolver` is in `internal/skills`. The interface has exactly one method, `Resolve(ctx context.Context, uri string) (resource.Content, error)`. The tool package says "I need a thing that resolves URIs with a context" in its own vocabulary. If the skills package changes its resolution strategy (adds caching, adds a second method), the tool's interface doesn't change. This is accept-interface/return-struct from `docs/07`.

### Errors and cancellation

**TypeScript:** Error handling is try/catch with `instanceof` checks. Cancellation is `AbortSignal` : the caller passes a signal, the callee checks `signal.aborted`. It's ad-hoc: some functions check, some don't.

**Go:** Every boundary takes `context.Context` as the first parameter. The resolver checks `ctx.Err()` at method entry, before file operations, and after reads. Errors are typed with `ResolveErrorCode`, implement `Unwrap()` for `errors.Is` chains, and the tool layer wraps with `%w` so `fs.ErrNotExist` propagates intact:

```go
// tools.go:511-512
content, err := options.ReadResolver.Resolve(ctx, selector.Path)
if err != nil { return ..., fmt.Errorf("resolve read resource: %w", err) }
```

This means the read tool can classify resolution failures by code without knowing they came from the skills package.

### Concurrency model

**TypeScript:** Single-threaded event loop. File reads are async but sequential by default. The reference processes discovery sequentially and doesn't worry about concurrent access to shared state.

**Go:** The resolver is **immutable after construction**. `NewResolver` validates and indexes the skill slice, then the `Resolver` struct has no mutable fields. This means `Resolve` is safe to call from any goroutine without locks. The same pattern holds for `discovery.Discover` : it reads everything once and returns owned data. Nothing mutates after return.

## Where it lives

| What | File | Key symbols |
|------|------|-------------|
| Resource value type | `internal/resource/resource.go` | `Content`, `MarkdownMediaType`, `TextMediaType` |
| Skill type & loading | `internal/skills/skills.go` | `Skill`, `LoadSkills`, `LoadSkillsFromDir`, `Find`, `LoadBody` |
| Skill URI resolver | `internal/skills/resolver.go` | `Resolver`, `NewResolver`, `Resolve`, `ResolveErrorCode`, `outsideBase` |
| @file import expansion | `internal/contextfile/imports.go` | `Expand`, `ExpandOptions`, `MaxImportDepth` |
| Session discovery | `internal/discovery/discovery.go` | `Discover`, `SessionInputs`, `Options`, `Diagnostic`, `readCandidate` |
| Read tool seam | `internal/tools/tools.go:106-110,501-515` | `ReadResourceResolver`, `skill://` dispatch |
| Session wiring | `internal/session/stack.go:105-155` | `BuildAgentStack`, `GenericRules` |
| Prompt rendering | `internal/prompt/prompt.go:39-108` | `BuildSystemPrompt`, `visiblePromptSkills` → `skill://` identity |
| Config | `internal/config/config.go:158,324-334` | `ProjectConfigDirName = ".harness"`, `skill_paths` resolution |
| CLI wiring | `cmd/harness/main.go:35-45,131-156` | `skillPathsFlag`, discovery → `StackConfig` |
| Template | `internal/prompt/templates/system.md.tmpl` | `<generic-rules>`, `<project_context>`, `<available_skills>` |
| Guard tests | `internal/skills/resolver_test.go:34-51` | Query/fragment/empty cases |
| Guard tests | `internal/discovery/discovery_test.go:51-92` | Dot-directory skip (readable + directory) |
| Guard tests | `internal/session/stack_test.go:123-177` | `GenericRules` survives compaction across two prompts |
