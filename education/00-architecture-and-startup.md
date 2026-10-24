# 00: Architecture and startup

## The problem

A coding-agent harness has to turn a user prompt into a correct, bounded run: resolve the operator's choices, select providers and models, collect the project context, assemble an agent, and drive it until the work completes. The difficult part is not any one step. It is keeping those steps explicit enough that a change to configuration, routing, or tools does not silently alter the runtime contract.

The core engine is valuable precisely because streaming, cancellation, provider events, and the agent loop have many edge cases. A broad rewrite to add a feature risks changing event ordering or termination behavior that only appears under a real stream. The safer design is to preserve a stable engine core and compose new behavior around its typed boundaries.

The harness is a self-contained, single Go module with a stable engine core and a boot path that wires config to roles to session to the completion loop, without assumptions about a particular deployment environment.

## Key decisions and the thinking process

### The single-module layout

The repository is one Go module:

```
harness-parth-plane/
├── go.mod                         module go.harness.dev/harness
├── cmd/
│   └── harness/                   CLI entrypoint (package main)
└── internal/                      implementation packages
    ├── adapter/ agent/ build/ compaction/ config/ contextfile/
    ├── discovery/ document/ editdiff/ engine/ obs/ pathutil/
    ├── permission/ progress/ prompt/ provider/ resource/ retry/
    ├── schema/ session/ skills/ subagent/ toolio/ tools/
    └── transport/ truncate/
```

`cmd/harness/` owns process entry and command wiring. Every subsystem lives below `internal/`: provider routing, the engine, session assembly, tools, permissions, prompt construction, context discovery, and supporting packages. The module exports no reusable library package; the public product surface is the `harness` CLI.

That layout is deliberately restrictive. Go permits imports of an `internal/` package only from code rooted under its parent directory. Outside code cannot depend on the harness implementation by accident, so the team can change subsystem APIs together without committing them as a public compatibility contract. The CLI is the narrow boundary where untrusted process inputs become typed runtime values.

### The stable engine core

`internal/engine/`, `internal/provider/`, `internal/agent/`, and the transport packages contain the streaming event model, provider protocols, and turn loop. Treat them as a stable core: understand their contracts before editing them, and prefer adding behavior in the packages that assemble or decorate the core.

That is a design choice, not an assertion that these packages may never change. A change to an event type, cancellation path, or stream dispatch must be justified by the runtime contract it changes and covered by the relevant tests. A new policy, prompt contribution, attachment source, or tool normally belongs at an existing typed seam instead: `internal/session/`, `internal/adapter/`, `internal/tools/`, `internal/permission/`, or `internal/compaction/`.

The payoff is local reasoning. `session.BuildAgentStack` composes a stream function, permission hook, tools, prompt, and compaction hook; it does not need to fork the agent loop to do so. The engine remains the substrate, while surrounding packages carry product-specific decisions.

### The boot path: one-shot only

The harness CLI runs a coding-agent request and prints the final result. There is no REPL or daemon command in `cmd/harness/`. The binary accepts a prompt, resolves configuration, builds the agent stack, runs the completion loop, persists a configured session when applicable, and exits.

This is intentional. The one-shot path exercises the complete stack—config → roles → discovery → session → loop—without requiring a long-lived service. The path is small enough to test with a faux provider while still exercising the same routing and assembly code used for a live provider.

### Config extends into roles, not the other way around

The boot path does not hand an unbounded raw configuration object to every package. Instead:

- `parseCommandOptions` collects raw flags into `config.FlagValues` (`cmd/harness/main.go:223-300`).
- `resolveCommandConfig` calls `config.Config{Flags: opts.flags}.Resolve()` and stores the immutable `*config.ResolvedConfig` (`cmd/harness/main.go:303-312`).
- `adapter.ResolveRoles` receives a compact `RoleRoutingConfig` containing the resolved model identifiers, base URLs, token limit, retry policy, and secret lookup function (`internal/adapter/roles.go:20-59`). It returns three ready-to-call `ProviderRouter` values: default, plan, and smol.

Configuration is the source of truth; roles are its capability projection. The session receives a resolved `types.Model` and `agent.StreamFn`, not flag precedence or environment lookups. `SecretLookup` is a function specifically so credential material does not enter durable resolved configuration (`internal/adapter/roles.go:20-31`).

### Discovery runs once at startup

`discovery.Discover` collects project instruction files, sticky rules, and canonical skills before the agent stack is built (`internal/discovery/discovery.go:89-219`). It returns a `SessionInputs` value containing context files, skills, generic rules, repository root, and non-fatal diagnostics (`internal/discovery/discovery.go:45-52`). Fatal failures are typed `discovery.Error` values with an `ErrorCode` (`internal/discovery/discovery.go:54-82`).

Running discovery once gives the prompt builder and tool registry a stable session snapshot. Files may change on disk after a run starts, but the current run does not gain or lose prompt inputs or skill resolution midway through a turn.

### No deployment assumptions through seams

`session.StackConfig` carries typed extension seams for tools, prompt additions, attachments, and sanitization (`internal/session/stack.go:25-63`). The stack builder consumes these values as data; it does not import an integration-specific package or accept loosely typed maps. A caller can add behavior through `ConfiguredTools`, `PromptAdditions`, and the attachment fields while the generic session assembly stays focused on its own contract.

---

## Signatures and types

### Entry point

```go
func main()
```

The OS entry point. It calls `run(os.Args[1:], os.Stdout, os.Stderr)` and exits with the returned code (`cmd/harness/main.go:51-53`).

```go
func run(args []string, stdout, stderr io.Writer) int
```

Dispatches `doctor` and `version`; every other argument sequence enters `runOneShot` (`cmd/harness/main.go:55-65`).

### Subcommands

```go
func runVersion(stdout io.Writer) int
```

Reads `build.Current()` and prints the binary name plus version, commit, and date (`cmd/harness/version.go:10-14`).

```go
func runDoctor(args []string, stdout, stderr io.Writer) int
```

Resolves configuration and role routing, then prints non-secret settings, selected models and routes, configured paths, and path or credential diagnostics. It does not run an agent (`cmd/harness/doctor.go:12-62`).

### One-shot agent run

```go
func runOneShot(args []string, stdout, stderr io.Writer) int
```

The default path. It parses flags, resolves configuration, configures logging, resolves model roles or a faux script, discovers context, opens an optional session, builds the stack, runs the completion loop, persists new messages, and prints the final assistant output (`cmd/harness/main.go:67-220`).

### Build metadata

```go
type Metadata struct {
    Version string
    Commit  string
    Date    string
}
```

The immutable build identity printed by `runVersion` (`internal/build/build.go:27-32`).

```go
func Current() Metadata
```

Returns linker-injected values and fills development defaults from `debug.ReadBuildInfo()` when possible (`internal/build/build.go:34-61`). Release builds can set `Version`, `Commit`, and `Date` with `-ldflags` (`internal/build/build.go:12-22`).

### Config resolution

```go
func (c Config) Resolve() (*ResolvedConfig, error)
```

Reads settings once and returns validated, non-secret, immutable runtime configuration (`internal/config/config.go:114-144, 279-280`). `resolveCommandConfig` owns the CLI-level call and stores the result in `commandOptions` (`cmd/harness/main.go:303-312`).

### Role routing

```go
type RoleRoutingConfig struct {
    DefaultModel     string
    PlanModel        string
    SmolModel        string
    AnthropicBaseURL string
    OpenAIBaseURL    string
    MaxTokens        int
    SecretLookup     func(string) (string, bool)
    Retry            *retry.Config
}
```

The resolved configuration needed to create the three roles. `SecretLookup` keeps resolved credentials out of durable configuration (`internal/adapter/roles.go:20-31`).

```go
type RoleRouters struct {
    Default ProviderRouter
    Plan    ProviderRouter
    Smol    ProviderRouter
}
```

Three routed providers; empty plan and smol identifiers inherit the default model (`internal/adapter/roles.go:33-45`).

```go
func ResolveRoles(cfg RoleRoutingConfig) (RoleRouters, error)
```

Builds each router through `newRoleRouter` and returns an error if any selected model cannot be routed (`internal/adapter/roles.go:40-59`).

```go
type ProviderRouter struct {
    Registry *base.Registry
    Model    types.Model
    StreamFn agent.StreamFn
}
```

The selected model, registered provider APIs, and a ready stream entry for the agent (`internal/adapter/provider.go:68-74`).

```go
func NewProviderRouter(cfg RoutingConfig) (ProviderRouter, error)
```

Validates a model specification, resolves its route, registers provider stream implementations, binds credentials into `StreamFn`, and optionally wraps that stream in retry logic (`internal/adapter/provider.go:76-112`).

### Context discovery

```go
func Discover(ctx context.Context, options Options) (SessionInputs, error)
```

Collects context files, rules, and skills once at startup (`internal/discovery/discovery.go:89-219`). Its non-fatal diagnostics travel in the result; typed failures stop startup.

```go
type SessionInputs struct {
    RepoRoot     string
    ContextFiles []prompt.ContextFile
    Skills       []skills.Skill
    GenericRules []string
    Diagnostics  []Diagnostic
}
```

The concrete startup snapshot consumed by `BuildAgentStack` (`internal/discovery/discovery.go:45-52`).

### Session stack assembly

```go
func BuildAgentStack(cfg StackConfig) (*AgentStack, error)
```

Builds the tool registry, chooses active tools, rebuilds the system prompt, installs permission and compaction hooks, creates `agent.Agent`, and returns the assembled stack (`internal/session/stack.go:74-172`).

```go
type AgentStack struct {
    Agent      *agent.Agent
    Model      ptypes.Model
    StreamFn   agent.StreamFn
    ProjectDir string
}
```

A configured agent ready for prompt and continuation calls (`internal/session/stack.go:65-72`).

### Completion loop

```go
func RunCompletionLoop(ctx context.Context, agt *agent.Agent, prompt InitialPrompt, opts CompletionOptions) (CompletionOutcome, error)
```

Consumes a stale completion sentinel, sends the initial text or media prompt, and delegates to `ContinueUntilComplete` (`internal/session/completion.go:80-98`).

```go
func ContinueUntilComplete(ctx context.Context, agt *agent.Agent, opts CompletionOptions) (CompletionOutcome, error)
```

Checks cancellation, assistant status, a completion sentinel, terminal errors, empty output, and the continuation cap. When none applies, it sends the configured continuation prompt and repeats (`internal/session/completion.go:100-168`).

### Faux provider

```go
func loadFauxScript(path string) (*faux.Faux, types.Model, error)
```

Loads a hermetic response script into a faux provider for offline tests and smoke runs without a live provider key (`cmd/harness/faux_script.go:23-56`).

---

## How it works

### The boot sequence, traced

Here is the execution trail from `harness -p "write a test"` to the final printed result:

**1. OS entry → dispatch** (`cmd/harness/main.go:51-65`)

`main` calls `run`. `run` dispatches `doctor` and `version` explicitly; no matching subcommand means the arguments are processed as a one-shot run.

**2. Flag parsing** (`cmd/harness/main.go:223-300`)

`parseCommandOptions` creates a `flag.FlagSet`, registers model, permission, logging, web, session, endpoint, credential, faux-script, and skill flags, then records which configuration flags were explicitly set. Its result carries raw `config.FlagValues`, the prompt, output choices, tool exclusions, and session selection.

**3. Config resolution** (`cmd/harness/main.go:303-312`, `internal/config/config.go:114-144`)

`resolveCommandConfig` calls `Config.Resolve`. The config layer applies the declared precedence—explicit flags, then environment values, then the settings file, then defaults—and produces a non-secret immutable snapshot.

**4. Logging and permissions** (`cmd/harness/main.go:79-99`)

The CLI configures logging, parses the default permission mode, builds the tool policy, and translates enabled retry settings into a `retry.Config`.

**5. Role routing** (`cmd/harness/main.go:100-114`)

`adapter.ResolveRoles` creates default, plan, and smol routers. Each router parses the selected model, resolves a provider route, binds credentials through a closure, and may wrap its stream function with retry. The selected default router supplies the model and stream used for this run.

**6. Optional faux override** (`cmd/harness/main.go:117-128`)

When `-faux-script` is set, the selected model and stream function are replaced with a local scripted provider. Otherwise the CLI warns when the selected provider has no resolved credential; routing remains deterministic and the provider call will report its own failure if attempted.

**7. Startup discovery and session restoration** (`cmd/harness/main.go:130-160`)

The CLI obtains the working directory, calls `discovery.Discover`, logs non-fatal discovery diagnostics, and opens an optional session. A resumed session supplies seed messages before the stack is built.

**8. Agent stack build** (`cmd/harness/main.go:162-180`, `internal/session/stack.go:74-172`)

`session.BuildAgentStack`:

- builds the tool registry, including optional web and caller-configured tools;
- uses skills to resolve supported resources when skills are present;
- chooses a read-only tool subset only when `StackConfig.Plan` is true, otherwise selects the normal set minus exclusions;
- rebuilds the system prompt from discovered context, skills, rules, and active tools;
- installs the permission gate and compaction hook; and
- creates `agent.Agent` with the initial transcript, model, stream function, tools, and hooks.

**9. Completion loop** (`cmd/harness/main.go:186-193`, `internal/session/completion.go:80-168`)

`RunCompletionLoop` sends the initial prompt and enters `ContinueUntilComplete`. The loop stops for context cancellation, a terminal provider result, a completion status in the assistant transcript, a successfully consumed completion sentinel, empty initial output, or the configured continuation cap. Otherwise it sends the continuation prompt and increments a local counter.

**10. Output and persistence** (`cmd/harness/main.go:194-220`)

New messages are persisted when a session store is active. The final assistant message is printed as text when possible, otherwise as JSON; explicit JSON output always marshals the full final message. Plan-mode output is also offered to the plan collector.

### What `harness doctor` actually does

`runDoctor` resolves the same flags, configuration, and role routing as a normal run but prints diagnostics instead of constructing an agent. It prints model identifiers, token and permission settings, logging and web settings, provider routes, configured paths, and redacted credential status. It also reports missing configured paths and missing provider credentials (`cmd/harness/doctor.go:12-106`).

### What `harness version` actually does

`runVersion` calls `build.Current` and prints the product name and metadata (`cmd/harness/version.go:10-14`). `Current` first uses linker-injected values and then fills development defaults from Go build information, including the installed module version and VCS revision or time when available (`internal/build/build.go:34-61`).

---

## Failure modes and invariants

### What breaks if discovery runs mid-session

If context files or skills are re-scanned after the agent starts, the system prompt and tool registry can drift from the startup snapshot. A newly added skill could be resolvable after the agent already received a prompt that did not describe it. Discovery runs before stack assembly and `SessionInputs` is passed to the builder, so each run uses stable context inputs for its lifetime.

### What breaks without the `Partial` nil in `copyEvent`

Streaming events can contain a mutable partial assistant message. The retry decorator makes a forwarding copy before it forwards an event; `copyEvent` explicitly clears `Partial` and deep-copies the message, error, and tool call fields (`internal/retry/stream.go:248-257`). Removing that clearing step would let retry bookkeeping retain a pointer whose producer may still mutate. The invariant is simple: a copied event must not share the live partial message.

### What breaks if config resolution is mutable

`Config.Resolve` produces a resolved snapshot with unexported fields and read-only accessors (`internal/config/config.go:122-183, 279-280`). If callers could mutate it during startup, the roles and session could be created from different values. Immutable resolution ensures the model, tool policy, paths, and logging settings describe one coherent run.

### Continuation loop invariants

- The completion sentinel is removed before a terminal return and counts only when removal succeeds; a stale or undeletable file cannot complete a later run (`internal/session/completion.go:134-155, 250-266`).
- A terminal `StopError` or `StopAborted` on the latest assistant message stops the loop before a completion status can be treated as success (`internal/session/completion.go:139-147, 231-247`).
- `PromiseStatusFromText` gives `WORKFLOW_COMPLETE` precedence over `STUCK`, then `STEP_COMPLETE`; the loop treats only workflow completion and stuck as terminal status cases (`internal/session/completion.go:148-182`).
- The continuation counter is local, not inferred from messages. A custom continuation prompt still increments the same capped counter (`internal/session/completion.go:104-106, 126-167`).
- When `CompletionOptions.Enabled` is false, the loop returns after the initial turn without adding a continuation prompt (`internal/session/completion.go:114-117`).

---

## TypeScript to Go

### Single-package agent vs. Go packages with internal boundaries

A TypeScript agent is often one package whose `index.ts` re-exports its public implementation. Imports can cross directories freely inside that package, and a convention has to prevent unrelated subsystems from depending on one another.

The harness is one Go module, but it partitions implementation into capability packages below `internal/`. `internal/adapter` owns provider routing, `internal/session` owns runtime assembly, `internal/permission` owns tool policy, and `internal/tools` owns tool construction. Each package is a real compile-time boundary with a narrow exported API; `internal/` also stops outside consumers from importing implementation packages at all.

This preserves the convenience of one build and one binary while making dependency direction visible in imports. A capability may evolve behind its package API without turning every internal type into a library promise.

### `internal/` visibility as access control

Go's `internal/` directory convention is enforced by the toolchain. A package below the repository's `internal/` directory can be imported only by code in the repository tree. A third-party program cannot import `go.harness.dev/harness/internal/session` or `go.harness.dev/harness/internal/engine/types`; its build fails before execution.

TypeScript can control package entrypoints with an `exports` map, but relative imports inside a package remain available unless the project adds tooling and conventions. Go therefore gives this repository a compiler-enforced implementation boundary while retaining normal package imports inside the module.

### Package-by-capability vs. directory-by-kind

A TypeScript project may collect `models`, `services`, `utils`, and `types` as horizontal categories. The harness groups code by capability: engine, provider, agent, session, tools, permissions, prompts, discovery, and configuration. A Go package is both a compilation unit and an API contract, so capability-oriented packages keep exported symbols coherent and avoid generic dumping grounds.

The result is practical rather than ideological. A change to provider routing starts in `internal/adapter`; a change to completion starts in `internal/session`; a change to a model event starts in `internal/engine`. The directory tells a maintainer which contract they are about to alter.

### Stable engine core vs. unrestricted refactoring in TypeScript

TypeScript repositories often make every source file equally easy to refactor. In a streaming harness, that ease can obscure the cost of changing the loop's event, cancellation, and retry behavior. The Go code treats engine-facing APIs—such as `agent.StreamFn`, `types.Model`, `types.Message`, and stream events—as stable contracts consumed by routing and session assembly.

That does not prohibit change. It asks for a narrower question first: is the required behavior an engine contract change, or can it be expressed by a decorator, hook, configuration value, or stack input? The latter keeps a feature local and limits the test surface; the former deserves direct, contract-level tests.

### Single binary vs. monorepo with workspaces

The harness ships one command: `harness`. `cmd/harness/` is the `main` package; the rest of the module is library code linked into that binary. A TypeScript system may use workspace packages or multiple processes to separate the CLI, server, and shared logic. Here, Go package boundaries provide source-level separation while the compiler produces one executable.

### Dependency injection without a framework

TypeScript commonly passes dependencies through constructors or factories. The harness uses the same idea with explicit typed fields and functions rather than decorators, reflection, or a container:

- `config.SecretResolver` supplies credentials at startup; `opts.Secrets.Resolve` is passed to role routing as a function (`cmd/harness/main.go:100-109`, `internal/config/config.go:102-112`).
- `agent.StreamFn` is a typed function passed into `session.StackConfig`, so session assembly calls a provider stream without knowing its protocol details (`internal/agent/types.go:11-13`, `internal/session/stack.go:28-63`).
- `PermissionModeFunc` lets the permission hook read a mode at tool-call time without rebuilding the stack (`internal/session/stack.go:20-23, 89-105`).

These boundaries are ordinary Go values. Dependencies are visible in the struct literal that assembles the system, which makes tests able to replace them without a framework.

---

## Where it lives

| What | File | Key symbols |
|---|---|---|
| Module identity | `go.mod` | `module go.harness.dev/harness` (line 1) |
| CLI entry and one-shot run | `cmd/harness/main.go` | `main` (line 51), `run` (line 55), `runOneShot` (line 67) |
| CLI configuration boundary | `cmd/harness/main.go` | `parseCommandOptions` (line 223), `resolveCommandConfig` (line 303) |
| Doctor subcommand | `cmd/harness/doctor.go` | `runDoctor` (line 12) |
| Version subcommand | `cmd/harness/version.go` | `runVersion` (line 10) |
| Faux provider loader | `cmd/harness/faux_script.go` | `loadFauxScript` (line 23) |
| Build metadata | `internal/build/build.go` | `Metadata` (line 27), `Current` (line 36), `Version`, `Commit`, `Date` |
| Config resolution | `internal/config/config.go` | `Config`, `ResolvedConfig`, `SecretResolver`, `Config.Resolve` (line 280) |
| Role routing | `internal/adapter/roles.go` | `RoleRoutingConfig` (line 22), `RoleRouters` (line 34), `ResolveRoles` (line 42) |
| Provider router | `internal/adapter/provider.go` | `ProviderRouter` (line 70), `NewProviderRouter` (line 83), `withStream` (line 120) |
| Model route parsing | `internal/adapter/modelspec.go` | `parseModelSpec`, `ValidateModelSpec`, `resolveRoute` |
| Startup discovery | `internal/discovery/discovery.go` | `Options` (line 19), `SessionInputs` (line 46), `Discover` (line 90) |
| Session stack | `internal/session/stack.go` | `StackConfig` (line 28), `AgentStack` (line 67), `BuildAgentStack` (line 77) |
| Completion loop | `internal/session/completion.go` | `CompletionOptions` (line 48), `CompletionOutcome` (line 64), `RunCompletionLoop` (line 81), `ContinueUntilComplete` (line 110) |
| Retry stream safety | `internal/retry/stream.go` | `Retrier`, `copyEvent` (line 253) |
| Agent stream boundary | `internal/agent/types.go` | `StreamFn` (line 13) |

The module is intentionally compact at its public edge: `cmd/harness/` starts the process, and `internal/` holds all implementation capabilities. The build, test, vet, race, and smoke entry points live in `Makefile`; the user-facing command summary lives in `README.md`.
