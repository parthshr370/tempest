# 04 — Config and model roles

## The problem

Every nontrivial CLI tool has configuration. The naive approach is a `config.json` and some `os.Getenv` calls sprinkled through the codebase. That works for one developer. It breaks the moment you need to debug a misconfiguration, test a subsystem without a real environment, or trace why `max_tokens` is 4096 when you set it to 8192.

Two specific pains drive this module's design:

**Pain 1: scattered environment reads.** When `os.Getenv("MODEL")` appears in the provider constructor, `os.Getenv("MAX_TOKENS")` in the retry module, and the settings file is parsed in `main.go`, there is no single place to answer "what configuration is this process running with?" A field set in the settings file is silently overridden by an environment variable in the shell profile, and the developer wastes twenty minutes tracing the value.

**Pain 2: mutable global config.** A package-level `var cfg = Config{}` that gets mutated during startup, then read from anywhere, is convenient until it isn't. Tests can't run in parallel. A subsystem can modify a field after startup. The config struct accumulates responsibility for credentials, transport state, and runtime flags that have no business being passed to the compaction engine or the tool registry.

The harness solves both pains with a single resolve-and-freeze pattern: read every input source once, resolve the precedence chain, validate across fields, and return an immutable struct with typed accessor methods. Credentials are resolved through a separate `SecretResolver` interface that never enters the config struct. After `Resolve()` returns, no code reads an environment variable or a settings file — every value comes from the frozen result.

## Key decisions and the thinking process

### Resolve once, then freeze

The `Config.Resolve()` method (`config.go:240-367`) collects every field from every source, validates the result, and returns a `*ResolvedConfig`. The resolved struct's fields are unexported — only accessor methods like `Model()`, `MaxTokens()`, and `Retry()` exist. There are no setters. There is no package-level global pointing at the resolved config.

This means:
- Any function that takes a `*ResolvedConfig` can trust its values won't change during the call — parallelism is safe without a mutex.
- A test can construct a `Config` with known `FlagValues`, call `Resolve()`, and pass the result to a subsystem — no environment mutation, no settings file, no shared state.
- A subsystem that only needs `MaxTokens()` doesn't see the `EnableWeb` field — the accessor methods define the interface implicitly, and callers depend only on what they use.

The alternative — a mutable struct with exported fields — would let any subsystem set `cfg.LogLevel = "debug"` to enable verbose output, creating action-at-a-distance bugs. In Go, the convention is "make invalid states unrepresentable," and an immutable resolved config is the simplest way to achieve that.

### Precedence: flag > env > settings.json > default

The chain is explicit in the `resolveString`, `resolveInt`, `resolveBool`, and `resolveFloat` helpers. Each follows the same pattern (`config.go:414-425`):

```
1. flag.Set?  → use flag value (even if empty/zero)
2. env != ""? → use environment variable
3. file != nil? → use settings.json value
4. else → use the built-in default
```

The `Input` types (`StringInput`, `IntInput`, `BoolInput`, `StringsInput`) carry a `Set` boolean that records whether the flag appeared on the command line. This matters. Without it, a zero-value flag (`-max-tokens 0`) would be indistinguishable from "the user didn't pass the flag," and the resolver would fall through to the environment/settings/default. The `Set` flag makes the distinction explicit.

Environment variables use the `HARNESS_` prefix (e.g. `HARNESS_MODEL`, `HARNESS_MAX_TOKENS`, `HARNESS_RETRY_MAX_ATTEMPTS`). The prefix is defined once in the `EnvAgentDir` and `EnvSessionDir` constants (`config.go:30-34`); the remaining env keys are constructed inline as string literals in `Resolve()`. The `LookupEnv` field on `Config` defaults to `os.Getenv` but is replaceable — tests inject a map-based lookup to control the environment layer.

### Cross-field validation happens during resolve, not after

Several constraints span multiple fields:

- `backoff_cap_ms >= retry_base_delay_ms` (`config.go:297`)
- `jitter_max >= jitter_min` and both within `[0, 1]` (`config.go:311,317`)
- `max_tokens > 0` (`config.go:261`)
- `anthropic_api_key` and `anthropic_auth_token` are mutually exclusive (`config.go:254-256`)
- model specs must be valid (`provider:model` format), base URLs must be valid URLs (`config.go:369-377,378-384`)

Each returns a typed `*Error{Field: "...", Err: ...}` that names the failing field. The caller gets a single point of failure — `Resolve()` either succeeds or returns one error — rather than a cascade of panics from downstream code discovering invalid combinations at runtime.

This cross-field validation is only possible because all values are gathered before the resolved struct is built. If the code read `os.Getenv` and `settings.json` piecemeal in each subsystem, the backoff-cap-greater-than-base-delay invariant would be impossible to enforce without a separate validation pass anyway.

### Secrets stay separate

`ResolvedConfig` stores model names, URLs, paths, and retry numbers. It does not store API keys or auth tokens. Those are resolved through `SecretResolver` (`config.go:100-110`), an interface with a single method:

```go
type SecretResolver interface {
    Resolve(name string) (value string, present bool)
}
```

The `SecretResolver` is built in `main.go:283-306` as a closure over the flag values and the real `os.Getenv`. It is passed to `adapter.ResolveRoles` as a function field (`roles.go:18`), not embedded in the resolved config struct. This ensures that dumping the config to a log or an event never leaks credentials.

### Model roles: default, plan, smol

The harness uses three model roles, not one. The default role handles the main agent loop — tool calls, file edits, reasoning. The plan role is a read-only agent that can read and reason but not edit. The smol role is a small, fast helper for classification, filtering, and other bounded tasks.

Each role maps to a model specification (a string like `"openai:gpt-5.1"` or `"anthropic:claude-sonnet-4-5"`). The plan and smol roles default to the default model if not explicitly set — `ResolveRoles` uses `firstNonEmpty(planModel, defaultModel)` and `firstNonEmpty(smolModel, defaultModel)` (`roles.go:33-34`).

Each role resolves to a fully routed `ProviderRouter` with its own `StreamFn`, credentials, base URL, and retry policy. If the default model is Anthropic and the smol model is OpenAI, the two routers have different API keys and base URLs even though they share the same `SecretLookup` function. `newRoleRouter` looks up the model's provider in the endpoint registry and calls `ProviderConfig.Credential` to resolve its key.

## Signatures and types

### Config: the raw input boundary

```go
// config.go:112-118
type Config struct {
    Flags        FlagValues
    LookupEnv    func(string) string  // defaults to os.Getenv
    SettingsPath string               // defaults to ~/.config/harness/settings.json
}
```

- `Flags`: the parsed command-line flag values, including their "was set" state.
- `LookupEnv`: the environment variable lookup function. Replaceable for testing.
- `SettingsPath`: path to `settings.json`. If empty, `Resolve()` calls `GetSettingsPath()`.

### FlagValues: typed flags with presence tracking

```go
// config.go:67-87
type FlagValues struct {
    Model              StringInput
    PlanModel          StringInput
    SmolModel          StringInput
    MaxTokens          IntInput
    PermissionMode     StringInput
    LogLevel           StringInput
    LogFile            StringInput
    LogFileMaxBytes    IntInput
    EnableWeb          BoolInput
    WebSearchURL       StringInput
    AgentDir           StringInput
    SessionDir         StringInput
    AnthropicBaseURL   StringInput
    OpenAIBaseURL      StringInput
    AnthropicAPIKey    StringInput
    AnthropicAuthToken StringInput
    FauxScript         StringInput
    SkillPaths         StringsInput
}
```

Each `*Input` type pairs a value with a `Set` boolean:

```go
// config.go:43-65
type StringInput  struct { Value string; Set bool }
type StringsInput struct { Values []string; Set bool }
type IntInput     struct { Value int; Set bool }
type BoolInput    struct { Value bool; Set bool }
```

`Set` is false when the flag never appeared on the command line. `flag.FlagSet.Visit` in `main.go:232-269` sets it to true for every flag the user explicitly passed. The `Set` field is what makes the precedence chain work — it distinguishes "the user wants the default" from "the user set nothing."

### ResolvedConfig: the frozen result

```go
// config.go:120-140
type ResolvedConfig struct {
    model                 string        // default model spec (never empty after resolve)
    planModel             string        // plan-agent model spec (may be empty; falls back to default)
    smolModel             string        // smol-helper model spec (may be empty; falls back to default)
    maxTokens             int           // validated > 0
    permissionModeDefault string        // validated: default, plan, acceptEdits, bypass
    logLevel              string        // validated: debug, info, warn, error
    logFile               string        // may be empty (no file logging)
    logFileMaxBytes       int64         // validated > 0
    enableWeb             bool          // gated web_search and web_fetch tools
    webSearchURL          string        // may be empty (uses provider default)
    agentDir              string        // tilde-expanded, resolved once
    sessionDir            string        // tilde-expanded, resolved once
    anthropicBaseURL      string        // may be empty (uses provider default)
    openAIBaseURL         string        // may be empty (uses provider default)
    fauxScript            string        // hermetic faux-provider script path
    settingsPath          string        // the settings file that was actually read
    skillPaths            []string      // explicit skill locations (defensive copy on access)
    retry                 RetrySettings // validated numeric policy
}
```

All fields are unexported. Every accessor returns a value type or a defensive copy:

```go
// config.go:142-196
func (c *ResolvedConfig) Model() string               { return c.model }
func (c *ResolvedConfig) PlanModel() string            { return c.planModel }
func (c *ResolvedConfig) SmolModel() string            { return c.smolModel }
func (c *ResolvedConfig) MaxTokens() int               { return c.maxTokens }
func (c *ResolvedConfig) PermissionModeDefault() string { return c.permissionModeDefault }
func (c *ResolvedConfig) LogLevel() string             { return c.logLevel }
func (c *ResolvedConfig) LogFile() string              { return c.logFile }
func (c *ResolvedConfig) LogFileMaxBytes() int64       { return c.logFileMaxBytes }
func (c *ResolvedConfig) EnableWeb() bool              { return c.enableWeb }
func (c *ResolvedConfig) WebSearchURL() string         { return c.webSearchURL }
func (c *ResolvedConfig) AgentDir() string             { return c.agentDir }
func (c *ResolvedConfig) SessionDir() string           { return c.sessionDir }
func (c *ResolvedConfig) AnthropicBaseURL() string     { return c.anthropicBaseURL }
func (c *ResolvedConfig) OpenAIBaseURL() string        { return c.openAIBaseURL }
func (c *ResolvedConfig) FauxScript() string           { return c.fauxScript }
func (c *ResolvedConfig) SettingsPath() string         { return c.settingsPath }
func (c *ResolvedConfig) Retry() RetrySettings         { return c.retry }
func (c *ResolvedConfig) SkillPaths() []string {
    return append([]string(nil), c.skillPaths...)
}
```

`SkillPaths()` returns a fresh copy — the caller can mutate the returned slice without affecting the resolved config. `Retry()` returns a value copy of `RetrySettings` (all fields are value types — `int`, `float64`, `bool` — so the copy is independent).

### Resolve: the single entry point

```go
// config.go:240
func (c Config) Resolve() (*ResolvedConfig, error)
```

Takes `Config` by value (the raw inputs are consumed). Returns a pointer to an immutable resolved config or a typed `*Error`. On success, the returned config has passed all cross-field validation and every field is suitable for immediate use. No further configuration reads happen after this call.

- `c.LookupEnv` defaults to `os.Getenv` if nil.
- `c.SettingsPath` defaults to `GetSettingsPath()` if empty.
- The settings file is optional: `loadSettings` returns a zero-value `fileSettings` when the file doesn't exist.
- After all fields are resolved into a `ResolvedConfig`, `validateResolved` checks model specs, base URLs, permission modes, and log levels.

### RetrySettings: the numeric retry contract

```go
// config.go:89-98
type RetrySettings struct {
    Enabled      bool
    MaxAttempts  int
    BaseDelayMS  int
    BackoffCapMS int
    MaxDelayMS   int
    JitterMin    float64
    JitterMax    float64
}
```

All fields are exported but the struct is returned by value from `Retry()`. The caller in `main.go:85-94` translates this into a `retry.Config` (which uses `time.Duration` fields) before passing it to `ResolveRoles`. The config package stores milliseconds to avoid coupling to `time.Duration`'s nanosecond representation.

### ResolveRoles: model roles to provider routers

```go
// roles.go:31
func ResolveRoles(cfg RoleRoutingConfig) (RoleRouters, error)
```

- `cfg.PlanModel` and `cfg.SmolModel` fall back to `cfg.DefaultModel` when empty.
- Each role model is routed through `newRoleRouter`, which resolves the endpoint registry entry and its API key or auth token based on the model's provider prefix.
- Returns `RoleRouters{Default, Plan, Smol}`, each a `ProviderRouter` with a `StreamFn` ready to make streaming provider calls.

```go
// roles.go:11-27
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

type RoleRouters struct {
    Default ProviderRouter
    Plan    ProviderRouter
    Smol    ProviderRouter
}
```

`SecretLookup` is a plain function, not an interface — the caller in `main.go:103` passes `opts.Secrets.Resolve`, which is the `SecretResolver.Resolve` method bound to the closure built in `secretResolver()`.

### Config.Error: field-qualified configuration failures

```go
// config.go:198-208
type Error struct {
    Field string
    Err   error
}

func (e *Error) Error() string { return fmt.Sprintf("config %s: %v", e.Field, e.Err) }
func (e *Error) Unwrap() error { return e.Err }
```

Every validation failure names its field. The caller can use `errors.As` to extract the field and present a targeted error message. This is used in `resolveCommandConfig` (`main.go:274-281`), which returns the error directly to `runOneShot`, which prints it to stderr.

## How it works

### 1. Parse flags into FlagValues (main.go:200-272)

`parseCommandOptions` creates a `flag.FlagSet` and registers every flag. The key detail: each flag writes into a `FlagValues` field's `.Value` (e.g. `values.Model.Value`). After `fs.Parse`, `fs.Visit` iterates only the flags the user explicitly passed and sets the corresponding `.Set = true`. Flags the user omitted keep `.Set = false`.

The `SkillPaths` field uses a custom `skillPathsFlag` type (`main.go:35-45`) that implements `flag.Value` — each `-skill /path/to/skills` appends to `Values` and sets `Set = true`.

A `SecretResolver` is built from the flag values immediately (`main.go:270`) — it's a closure that checks the explicit `-anthropic-api-key` and `-anthropic-auth-token` flags first, then falls back to the real `ANTHROPIC_API_KEY` and `OPENAI_API_KEY` environment variables.

### 2. Resolve configuration (config.go:240-367, main.go:274-281)

`resolveCommandConfig` calls `config.Config{Flags: opts.flags}.Resolve()`:

1. **Set up defaults**: `LookupEnv` → `os.Getenv`, settings path → `~/.config/harness/settings.json`.
2. **Load settings.json**: `loadSettings` reads and unmarshals the file. If the file doesn't exist, it returns a zero `fileSettings` (all pointer fields nil) — no error. Decode failures return a `config.Error`.
3. **Validate flag combinations**: `-anthropic-api-key` and `-anthropic-auth-token` together are rejected.
4. **Resolve each field**: for example, the default model resolves via:
   ```
   resolveString(c.Flags.Model, lookup("HARNESS_MODEL"), settings.Model, defaultModel)
   ```
   `settings.Model` is a `*string` — nil means "not in settings file." `resolveString` (`config.go:414-425`) checks `flag.Set`, then non-empty env, then the file pointer, then the fallback.
5. **Cross-validate numerics**: `maxTokens > 0`, `backoffCapMS >= baseDelayMS`, `jitterMax >= jitterMin`, all durations below `maxRetryDurationMS`.
6. **Construct the `ResolvedConfig`** with all private fields set. Agent and session dirs are tilde-expanded via `ExpandTildePath`.
7. **Run `validateResolved`**: model specs must be valid `provider:model` format, base URLs must be valid URLs, permission mode must be one of the four known values, log level must be one of the five known values.
8. **Return** the frozen `*ResolvedConfig`.

On failure at any step, `nil` + a `*config.Error` naming the failing field.

### 3. Wire retry config (main.go:85-94)

`runOneShot` reads `opts.Config.Retry()` (a value copy of `RetrySettings`). If retry is enabled, it populates a `retry.Config` (which uses `time.Duration` fields) from the millisecond values and passes it to `ResolveRoles`.

### 4. Resolve model roles (main.go:95-108)

`runOneShot` calls `adapter.ResolveRoles` with a `RoleRoutingConfig` built from the resolved config's accessors. The plan and smol models fall back to the default model if empty. The `SecretLookup` is `opts.Secrets.Resolve` — the closure method built in step 1.

Each role model string is parsed for its provider prefix, the correct credential is looked up, and a `ProviderRouter` with a bound `StreamFn` is returned. If the default model is `anthropic:claude-sonnet-4-5` and the smol model is `openai:gpt-5.1-mini`, the resulting routers have different API keys, base URLs, and provider adapters — but they share the same retry policy and `MaxTokens`.

### 5. Use the routers (main.go:109-122)

`roles.Default` provides the main `StreamFn` and the selected model. `roles.Plan` provides the plan agent's model. If `-faux-script` is set, a hermetic faux provider replaces the real router — used for deterministic testing without live API calls.

### Data flow diagram

```
CLI flags                os.Environ            settings.json
    │                        │                       │
    ▼                        ▼                       ▼
FlagValues              LookupEnv              fileSettings
(Set booleans)          (func(string)string)   (pointer fields: nil=absent)
    │                        │                       │
    └────────────────────────┼───────────────────────┘
                             │
                    Config{Flags, LookupEnv, SettingsPath}
                             │
                    .Resolve() ─── loadSettings (optional file)
                             │
                    resolveString/Int/Bool/Float
                    (flag.Set ? flag : env ? env : file ? file : default)
                             │
                    cross-field validation
                    (backoff cap >= base, jitter range, etc.)
                             │
                    build ResolvedConfig (private fields)
                             │
                    validateResolved (model specs, base URLs, modes)
                             │
                    *ResolvedConfig ─────────────────────┐
                             │                           │
                    Accessor methods               Retry()
                    Model(), PlanModel(),          (value copy of
                    SmolModel(), MaxTokens(),      RetrySettings)
                    AgentDir(), SessionDir(),           │
                    SkillPaths() (defensive copy)        │
                             │                           ▼
                             │                   retry.Config
                             │                   (time.Duration fields)
                             │                           │
                             └───────────┬───────────────┘
                                         │
                              RoleRoutingConfig{
                                  DefaultModel, PlanModel, SmolModel,
                                  AnthropicBaseURL, OpenAIBaseURL,
                                  MaxTokens, SecretLookup, Retry
                              }
                                         │
                              adapter.ResolveRoles()
                                         │
                              ┌──────────┼──────────┐
                              ▼          ▼          ▼
                          Default     Plan       Smol
                          Router      Router     Router
```

## Failure modes and invariants

### Missing settings file is not an error

`loadSettings` (`config.go:399-412`) treats `os.ErrNotExist` as a success — it returns a zero `fileSettings` with all pointer fields nil. The resolve helpers treat nil file pointers as "absent," falling through to the built-in default. A fresh install with no `settings.json` runs entirely on defaults and environment variables.

### Explicit empty flag wins over everything

`resolveString` checks `flag.Set` before the value. If the user passes `-model ""`, the empty string is selected. This is deliberate: the user explicitly overrode the env/settings/default. `validateResolved` then rejects the empty model string, producing a clear error rather than silently falling back to the default.

### Mutually exclusive credentials stop early

`Resolve()` checks `c.Flags.AnthropicAPIKey.Set && c.Flags.AnthropicAuthToken.Set` before any field resolution (`config.go:254-256`). This is checked on `Set`, not on the value — the conflict exists regardless of what the values contain, and catching it early avoids a confusing downstream error from the provider.

### Cross-field ordering constraints

`backoff_cap_ms >= retry_base_delay_ms` (`config.go:297`) ensures the backoff ceiling is at least the starting delay. `jitter_max >= jitter_min` (`config.go:318`) ensures the jitter range is non-empty and ordered. These are enforced after all values are resolved, before `Resolve()` returns. The retry decorator in `internal/retry` never checks these constraints — it trusts the config is valid.

### Defensive copy for slice accessor

`SkillPaths()` returns `append([]string(nil), c.skillPaths...)` rather than `c.skillPaths`. If the caller appends to the returned slice, it doesn't corrupt the resolved config. For value-type accessors like `Model()` or `Retry()`, a copy is inherent — strings are immutable and `RetrySettings` is returned by value.

### nil-receiver safety for accessors

Every accessor on `*ResolvedConfig` dereferences `c`. If called on a nil pointer, it panics — the same behavior as any Go method on a nil value receiver. The convention is that `Resolve()` never returns `nil` without an error, and callers only dereference the config after a nil-error check.

### Missing credential resolver

`ProviderConfig.Credential` accepts a nil lookup and falls back to the provider's configured key. `ResolveRoles` can therefore build a router without an environment resolver. If neither a key nor a native credential chain nor `no_auth` is available, the router returns a missing-credential error on the first request.

### Retry configuration disabled path

When `RetrySettings.Enabled` is false, `main.go:86` skips the retry config construction entirely — `retryConfig` stays nil. `ResolveRoles` receives a nil `Retry` pointer, and `NewProviderRouter` uses a nil retry config to build a non-retrying router. The resolved config still carries the retry values (they're resolved and validated regardless of the enabled flag), but the transport layer never reads them.

### Env prefix consistency

All environment variables use the `HARNESS_` prefix. There is no mechanism to change the prefix — it's hardcoded as string literals in `Resolve()`. This is a deliberate simplification: the harness is one product with one env namespace. If future embedders need a different prefix, the `LookupEnv` function can be replaced to remap prefixes before the resolver sees the values.

## TypeScript to Go

### Mutable config object vs. resolve-then-freeze

**TypeScript instinct**: A class with mutable properties, often populated by a `loadConfig()` method that reads environment variables and a JSON file asynchronously:

```typescript
class AppConfig {
  model: string = "claude-sonnet-4-5";
  maxTokens: number = 4096;
  // ... more fields

  async load(): Promise<void> {
    const settings = await readSettings();
    this.model = process.env.MODEL ?? settings.model ?? this.model;
    this.maxTokens = parseInt(process.env.MAX_TOKENS ?? settings.maxTokens ?? String(this.maxTokens));
  }
}

// Later, anywhere in the codebase:
if (config.maxTokens < 1000) { /* ... */ }
```

This is convenient but carries three risks. First, any subsystem can reassign `config.model` at any time, creating action-at-a-distance bugs. Second, the config object is typically a singleton — tests must either mutate and restore it (breaking parallel execution) or use a mocking library to replace the module. Third, there's no distinction between "the user didn't set maxTokens" and "the user set maxTokens to the default" — the resolver can't validate user intent.

**Go alternative**: `Config.Resolve()` returns an immutable `*ResolvedConfig` with unexported fields and typed accessors. After `Resolve()`, nothing can change the values. Tests construct a `Config` with known `FlagValues`, call `Resolve()`, and pass the result to the subsystem under test — no globals, no environment mutation, no mocking.

```go
cfg, err := config.Config{Flags: flags}.Resolve()
if err != nil { /* ... */ }
// cfg is frozen. cfg.Model() always returns the same value.
```

The same rule is covered in module 12 (idiomatic Go vs TypeScript): "Resolve file/env/flag configuration once into an immutable concrete config" and "No hidden environment reads below provider/config constructors."

### Scattered `process.env` reads vs. centralized precedence

**TypeScript instinct**: `process.env.MODEL` appears wherever a subsystem needs a model name. `process.env.HARNESS_RETRY_MAX_ATTEMPTS` appears in the retry module. The settings file is read separately, possibly at a different point in the startup sequence. When a value is wrong, the developer traces through a chain of `??` operators across multiple files.
Environment variables use the `HARNESS_` prefix; this is a real convention in `internal/config`.

**Go alternative**: Every value is resolved in one function (`Resolve()`), with one precedence chain (`resolveString`/`resolveInt`/`resolveBool`/`resolveFloat`), and every subsequent consumer reads from the frozen `ResolvedConfig`:

```go
// The precedence is in one place (config.go:414-425):
func resolveString(flag StringInput, env string, file *string, fallback string) string {
    if flag.Set         { return strings.TrimSpace(flag.Value) }
    if value := strings.TrimSpace(env); value != "" { return value }
    if file != nil      { return strings.TrimSpace(*file) }
    return fallback
}
```

After `Resolve()`, no code calls `os.Getenv("HARNESS_MODEL")`. No code reads `settings.json`. The only question is "what did Resolve return?"

### `any`-typed config vs. typed structs

**TypeScript instinct**: Configuration is often `Record<string, any>` or a `Map<string, unknown>`, with runtime type checks scattered through consuming code:

```typescript
const maxTokens = config.maxTokens; // number | undefined
if (typeof maxTokens !== 'number' || maxTokens <= 0) { ... }
```

**Go alternative**: `ResolvedConfig` has typed fields — `int` for `maxTokens`, `string` for `model`, `bool` for `enableWeb`. The resolver validates types at the boundary (parsing env vars with `strconv.Atoi`, `strconv.ParseBool`, `strconv.ParseFloat`), so consumers never receive a string where they expect an int.

### Optional fields: `?` vs. pointer

**TypeScript instinct**: `settings.model?: string` — the field may be absent. The resolver uses `??` to fall through.

**Go alternative**: `fileSettings` uses pointer fields — `Model *string` (`config.go:211`). `nil` means "absent from the settings file." The `resolveString` helper checks `file != nil` before dereferencing, treating nil as "skip this source." This is the Go idiom for "optional" in JSON — a `*string` serializes as `null` when nil, and the resolver's nil-check is the explicit equivalent of TypeScript's `??`.

### Role routing: function injection vs. class dependency

**TypeScript instinct**: The role router might be a class that accepts a `Config` instance in its constructor and reads model names and credentials through `this.config`:

```typescript
class RoleRouter {
  constructor(private config: AppConfig) {}
  resolve() {
    const creds = this.config.apiKey; // ambient dependency on the config object
  }
}
```

**Go alternative**: `ResolveRoles` is a package-level function that takes a `RoleRoutingConfig` struct. All dependencies are explicit parameters — `DefaultModel`, `SecretLookup`, `Retry`. The function has no ambient state, no `this`, and no package-level globals:

```go
roles, err := adapter.ResolveRoles(adapter.RoleRoutingConfig{
    DefaultModel:     opts.Config.Model(),
    PlanModel:        opts.Config.PlanModel(),
    SmolModel:        opts.Config.SmolModel(),
    SecretLookup:     opts.Secrets.Resolve, // function injection, not ambient state
    Retry:            retryConfig,
    // ...
})
```

This is "dependency injection by parameter" — the same pattern covered in module 12 (idiomatic Go vs TypeScript). The function is trivially testable: pass a `RoleRoutingConfig` with a fake `SecretLookup` that returns known credentials, and assert the resulting routers have the expected provider adapters.

### No global mutable state

**TypeScript instinct**: It's common to see `export const config = new AppConfig()` at module scope, populated during an async initialization step. Subsystems import `config` and read its properties directly.

**Go alternative**: There is no package-level `var resolvedConfig *ResolvedConfig`. `resolveCommandConfig` stores the result in `opts.Config`, and `runOneShot` passes it (or its accessor return values) explicitly to every subsystem that needs it. The agent loop, the tool registry, the compaction hook — each receives only the values it consumes, not the entire config struct.

This is the "accept interfaces, return structs" principle applied to configuration: subsystems accept narrow parameters (a string, an int, a `RetrySettings`), not the whole `*ResolvedConfig`. A subsystem that only needs `MaxTokens()` shouldn't import `EnableWeb()` into its namespace.

## Where it lives

| File | Key symbols |
|---|---|
| `internal/config/config.go` | `Config`, `ResolvedConfig`, `FlagValues`, `StringInput`, `IntInput`, `BoolInput`, `StringsInput`, `RetrySettings`, `SecretResolver`, `SecretResolverFunc`, `Error`, `fileSettings`, `Resolve()`, `validateResolved()`, `loadSettings()`, `resolveString()`, `resolveInt()`, `resolveInt64()`, `resolveBool()`, `resolveFloat()`, `resolveLogLevel()`, `GetAgentDir()`, `GetSettingsPath()`, `ExpandTildePath()`, `defaultModel`, `defaultPermissionMode`, `defaultLogLevel`, `defaultLogFileMax`, `maxRetryDurationMS` |
| `internal/config/config_test.go` | precedence table tests, cross-field validation tests, tilde-expansion tests, model spec validation, retry range tests |
| `internal/adapter/roles.go` | `ResolveRoles()`, `RoleRoutingConfig`, `RoleRouters`, `ProviderRouter`, `newRoleRouter()` |
| `cmd/harness/main.go:200-281` | `parseCommandOptions()` (flag parsing and `Set` tracking), `resolveCommandConfig()` (Resolve call), `secretResolver()` (credential lookup closure), `skillPathsFlag` |
| `cmd/harness/main.go:63-198` | `runOneShot()` (config consumption: retry wiring, ResolveRoles call, agent stack construction) |
| `12-idiomatic-go-vs-typescript.md` | design rules: resolve-once, immutable config, no hidden env reads, no global registries |
