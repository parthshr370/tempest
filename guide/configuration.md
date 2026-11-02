# Configuration

Settings come in layers. Each layer overrides the ones below it:

1. `-c key.path=value` overrides.
2. Flags.
3. `HARNESS_*` environment variables.
4. The selected profile.
5. Project-local settings, `.harness/settings.local.json` in the workspace.
6. Project settings, `.harness/settings.json` in the workspace.
7. User settings, `~/.config/harness/settings.json`.
8. Managed defaults from the administrator's managed settings file.
9. Built-in defaults.

Managed requirements sit above every layer: a pinned key cannot be changed. The
command does not read `.env`. Export variables before running it.

Every settings file is one JSON object with the keys in [Settings file](#settings-file).
Key names are exact: `Hooks`, `OPENAI_BASE_URL` or any other spelling of a settings
key, at any depth, is an unknown key and has no effect (see [Strict mode](#strict-mode)).
Names inside maps, such as provider route names, header names and environment
variables, are data and keep any spelling.
`tempest config show` prints the result; see [Inspecting settings](#inspecting-settings).

## How layers combine

- **Objects merge key by key.** A layer that sets `providers.gateway.base_url` keeps
  every other field and route from the layers below.
- **Arrays replace.** A layer that sets `permission_deny_tools` replaces the whole
  list; nothing is appended. The exceptions are trusted project `hooks` and an
  untrusted project's deny list, which add to the layers below (see
  [Project settings and trust](#project-settings-and-trust)).
- **`null` unsets.** `"max_tokens": null` in a higher layer removes the value from
  every layer below, so the built-in default applies again. `-c model=null` works the
  same way.
- **Provider entries replace by name between settings files.** A `providers.<name>`
  entry in a settings file or profile replaces the same entry from lower files whole,
  so a project cannot inherit a user's `api_key` by reusing a route name. Environment
  variables and `-c` set single fields.
- **`mcp_config` and `mcp_servers` exclude each other.** A layer that selects an MCP
  file replaces a lower inline server array, and the reverse. Both in one file is an
  error.

The `HARNESS_*` variables and flags in the tables below are layers for the settings
key they name. `AZURE_OPENAI_ENDPOINT` and `AZURE_OPENAI_API_VERSION` set
`providers.azure.base_url` and `providers.azure.query.api-version` in the
environment layer. `ANTHROPIC_BASE_URL` and `OPENAI_BASE_URL` are built-in defaults,
below every settings file.

## Project settings and trust

Repository files are untrusted. Settings in `.harness/settings.json`,
`.harness/settings.local.json`, and the profiles defined in them that run code, read
local files into the model context, write outside the repository, widen permissions,
turn on web tools, raise the operator's workspace listing, cost or resource caps, or
choose where credentials go apply only after the operator trusts the workspace:

| Keys | Applied when |
|---|---|
| `hooks`, `mcp_config`, `mcp_servers`, `mcp_sampling_allow_headless` other than `false`, `lsp_config`, `enable_lsp`, `debug_adapters`, `cloud_policy`, `permission_allow_tools`, `permission_mode_default` other than `"plan"`, `plan` other than `true`, `permission_deny_tools` set to `null`, `enable_web` other than `false`, `attach`, `skill_paths`, `agent_dir`, `context_files`, `reminders`, `session_dir`, `log_file`, `faux_script`, `workspace_listing`, `workspace_listing_max_files`, `workspace_listing_max_bytes`, `max_tokens`, `mcp_sampling_max_tokens`, `retry_enabled`, `retry_max_attempts`, `retry_max_delay_ms`, `bash_timeout` or `log_file_max_bytes` above the operator's value, `loop_detection` looser than the operator's, and `tool_search` deferring less than the operator's | `-trust-project-hooks`, `HARNESS_TRUST_PROJECT_HOOKS=true`, or the workspace's absolute canonical path in user `trusted_hook_projects`. |
| `providers`, `anthropic_base_url`, `openai_base_url`, `web_search_url`, `otel`, and a `model`, `plan_model`, `smol_model`, `mcp_sampling_model` or `fallback_models` entry with an `@http(s)://` endpoint | `-trust-project-providers`, `HARNESS_TRUST_PROJECT_PROVIDERS=true`, or the path in user `trusted_provider_projects`. |
| `no_hooks`, `disableAllHooks`, `trusted_hook_projects`, `trusted_provider_projects`, `environment`, `environments`, and `hooks` inside a project profile | Never. Only the operator sets them. |

An untrusted workspace still gets every other project setting. The keys that were
dropped are named in one warning in the log, in `tempest doctor` and `config show`,
and on stderr from the `mcp` commands and `agents list`.

An untrusted repository can narrow permissions and capabilities and never widen
them. Its `"permission_mode_default": "plan"`, `"plan": true` and
`"enable_web": false` apply, since nothing is stricter. Web tools fetch any URL the
model names, so a repository that could turn them on could have the model send its
contents out; `"enable_web": true` is dropped with the trust warning. Its
`permission_deny_tools`, in a project file or a project profile, adds
to the list the layers below it merge to: the operator's entries stay first and the
project's new entries follow, so `[]` changes nothing and a `null` is dropped with
the trust warning. `config show` attributes the combined list to the highest project
layer that added an entry, or to the operator's layer when the project added none.
Allow lists, looser modes and `"plan": false` are dropped with the trust warning. In
a trusted workspace these keys are a settings layer like the rest: the project's deny
list replaces the user's and its `null` unsets it.

The session-start [workspace listing](skills-and-context.md#workspace-listing) and
the cost and resource caps work the same way. An untrusted repository may turn the
listing or retries off (`"workspace_listing": false`, `"retry_enabled": false`) and
lower `workspace_listing_max_files`, `workspace_listing_max_bytes`, `max_tokens`,
`mcp_sampling_max_tokens`, `retry_max_attempts`, `retry_max_delay_ms` and
`log_file_max_bytes` to any whole number from 1, and `bash_timeout` to any positive
duration, up to the operator's value: the merged value of the managed, user,
profile, environment, flag and `-c` layers, or the built-in default when none sets
it. The ceiling for `mcp_sampling_max_tokens` is the operator's own sampling cap,
else the operator's `max_tokens`, which sampling inherits. When the operator set
`retry_max_delay_ms` to 0 or less (no limit), any whole number applies. A larger
value, `true` for a switch the operator turned off, and a `null` or invalid value,
which would fall back to the default, are dropped with the trust warning. Trust
lifts the ceiling. The caps bound prompt size, output tokens, retries, waiting,
command run time and log disk use, which are the operator's budget to set. A
repository `max_tokens` still counts as an explicit output cap, which ChatGPT plan
models refuse.

The per-run caps `max_input_tokens`, `max_output_tokens`, `max_total_tokens` and
`max_tool_calls` work the same way, except that their default, 0, means unlimited.
An untrusted repository may set any of them to a whole number from 1 when the
operator set none, or up to the operator's value when it did. Its own 0 or `null`
would lift the operator's cap, so like a larger value it is dropped with the trust
warning.

The [loop guard](sessions-and-durability.md#loops) works the same way in the other
direction: a lower threshold reports sooner. An untrusted repository's
`loop_detection` applies when every threshold it sets is a whole number from 2 up to
the operator's threshold (the built-in default when none sets it); where the
operator turned a pattern off with `0`, any threshold from 2 applies. A larger
threshold, `0`, a `null` or an invalid value drops the whole `loop_detection` object
with the trust warning.

[Tool search](tools.md#deferred-tools) also works one way: a repository may defer
MCP and extension tool schemas more than the operator does, never less, since
sending every schema with every request spends the operator's tokens. Its
`tool_search` applies when its `mode` is `"always"`, or `"auto"` with a
`threshold_tokens` at most the operator's (any threshold when the operator chose
`"never"`). `"never"`, a higher threshold, a `null` or an invalid value drops the
whole `tool_search` object with the trust warning.

Trusted project `hooks` add to the hooks below them instead of replacing them: each
event's array from `settings.json`, then from `settings.local.json`, is appended to the
user's. A project `null` for `hooks` or for one event is ignored with a warning, so a
repository cannot remove the operator's hooks. Otherwise project hooks are a settings
layer like any other: a profile, `-c hooks=null` or `-c hooks.SessionStart=[]`
replaces or unsets them, managed requirements on `hooks` see the combined arrays, and
`config show` lists them. A session opened in another workspace (`serve`, ACP, the
SDK) gets that workspace's project hooks from the same resolution. See
[hooks](hooks.md#trust).

A project file the operator has not trusted that cannot be used (invalid JSON, not an
object, a value of the wrong type, not a regular file, or larger than 4 MiB) is
skipped with a warning naming the file and the reason, and the run goes on with the
other layers. A trusted workspace's file, any user or managed file, and any file under
`-strict-config` stops startup with an error that names the file instead. A key the
trust rules drop is never type-checked, so its value cannot invalidate the file. A
value that parses but is invalid, such as `"max_tokens": 0` or an unknown `reasoning`
level, stops startup in any layer; the error names the key and the layer and file,
variable or flag that set it, and a `-c` override or flag replaces it.

Project files are read from the working directory; `-resume` reads them from the
resumed session's workspace, where it runs. With `-environment` set to a
remote environment, project files on the host are not read.

## Profiles

A profile is a partial settings object under `profiles.<name>` in any settings file:

```json
{
  "profiles": {
    "review": {"model": "anthropic:claude-opus-4-8", "plan": true},
    "cheap": {"model": "openrouter:deepseek/deepseek-chat", "reasoning": "off"}
  }
}
```

Select one with `-profile review` or `HARNESS_PROFILE=review`; the flag wins. The
profile layer sits above project-local settings and below `HARNESS_*` variables.
When several files define the same profile, their definitions merge in file order:
managed defaults, user, project, project-local, then `-c`. Project profiles pass the
same trust rules as project settings. `"profiles": {"review": null}` unsets `review`
from every lower file, and `"profiles": null` unsets every profile below; a later file
or `-c` can define it again. A null in a project file follows the trust rules: keys
of a user or managed definition that the project could not set, or set to `null`,
itself stay (in an untrusted workspace, `plan` and `permission_deny_tools` among
them), and the warning names them, unless an operator file or `-c` above unsets the
profile again. Selecting a profile that no file defines, or that a higher file
unsets, is an error.

`-profile` is a settings profile. `tempest login`, `logout` and `auth` take a
different `--profile`: the label of a saved ChatGPT account. The session controls
`fork`, `reset`, `compact`, `history` and `rewind` take `-profile`, `-c` and the
other settings flags too; there they choose the session directory and faux script,
and the session they reopen resolves its own settings from its workspace.

## Overrides with `-c`

`-c key.path=value` sets one setting at the highest layer. It is repeatable, and a
later `-c` wins. The path is dotted; the value is parsed as JSON when it is valid JSON,
and taken as a string otherwise:

```sh
tempest -p "fix the build" \
  -c max_tokens=8000 \
  -c providers.gateway.base_url=https://gateway.internal/v1 \
  -c 'permission_deny_tools=["bash"]' \
  -c model=anthropic:claude-opus-4-8
```

Quote a string that looks like JSON: `-c 'model="true"'`. `-c key=null` unsets the key
in every lower layer. Keys cannot contain dots. A malformed `-c` is an error that gives
its position among the overrides, never its text, since a mistyped override may hold a
credential.

## Managed requirements

An administrator controls a machine-wide file: `/etc/harness/managed-settings.json` on
Linux and other Unix systems, `/Library/Application Support/Harness/managed-settings.json`
on macOS. It has two optional sections:

```json
{
  "defaults": {"model": "anthropic:claude-opus-4-8"},
  "requirements": {
    "permission_mode_default": "default",
    "providers": {"openai": {"base_url": "https://llm-proxy.corp.example/v1"}}
  }
}
```

- `defaults` is the managed defaults layer, just above built-in defaults. Users and
  projects override it.
- `requirements` pins every leaf value under it. A pinned value applies even when no
  layer sets it. Any user, project, profile, environment, flag or `-c` layer that sets
  a pinned key to a different value, or unsets it, stops startup with an error that
  names the key, the managed file, and the layer and file, variable or flag that
  conflicts. Setting the same value, or an unpinned sibling such as
  `providers.openai.api_key_env`, is allowed. A project value dropped by the trust rules
  is not a conflict.

The managed file is strict at every depth: any other top-level section, or an unknown
key anywhere under `requirements` (such as `providers.openai.bsae_url`), is an error.
`requirements` cannot pin `profiles`. There is no environment variable that moves the
file, so a user's environment cannot drop a requirement.

## Strict mode

By default an unknown settings key is ignored with one warning per key in the log,
naming its dotted path (for example `providers.openai.bsae_url` or
`mcp_servers[0].bogus`) and the file, variable or `-c` that set it. An ignored key is
removed before anything reads the settings, so it never shows in `config show`. A value
of the wrong type is an error that names the file, except in an untrusted project file,
which is skipped instead (see [Project settings and trust](#project-settings-and-trust)).

`-strict-config` (or `HARNESS_STRICT_CONFIG=true`) turns unknown keys into errors at
any depth, including fields inside provider entries, their `compatibility` and
`models`, and `mcp_servers` entries.

## Inspecting settings

`tempest config show` resolves the settings exactly as a run does and prints every
effective value with its layer and source:

```text
$ tempest config show -c max_tokens=8000
max_tokens = 8000  (override -c)
model = "anthropic:claude-opus-4-8"  (user /home/me/.config/harness/settings.json)
permission_mode_default = "default"  (requirement /etc/harness/managed-settings.json)
...
```

- `-json` prints one object, `{"settings": {"<key>": {"value", "layer", "source"}},
  "warnings": [...]}`.
- `-key <path>` limits the output to one dotted key or every key below it, for
  example `-key providers.openai`.
- It accepts the run flags, including `-profile`, `-c`, `-strict-config` and the trust
  flags, so the output matches a run with the same flags.

Layers are `default`, `managed`, `user`, `project`, `project-local`, `profile`, `env`,
`flag`, `override` and `requirement`. The source is a file path, an environment
variable, a flag, or `-c`. Values under credential names (`api_key`, `*token`,
`secret`, `password`, `authorization`), under `headers`, `query` and `env` in any
letter case, and URL userinfo are shown as `[REDACTED]`. Configuration errors pass
through the same log redactor.

## Model roles

Three roles can each route to a different provider and model:

| Role | Used by | Flags | Environment |
|---|---|---|---|
| `default` | Normal turns | `-model` | `HARNESS_MODEL` |
| `plan` | Read-only planning (`-plan`) | `-plan-model` | `HARNESS_PLAN_MODEL` |
| `smol` | The `explore` subagent of `-enable-task` | `-smol-model` | `HARNESS_SMOL_MODEL` |

Reasoning effort follows the same pattern with `-reasoning`, `-plan-reasoning` and
`-smol-reasoning`, or `HARNESS_REASONING`, `HARNESS_PLAN_REASONING` and
`HARNESS_SMOL_REASONING`. The settings keys are `reasoning`, `plan_reasoning` and
`smol_reasoning`.

Each role can also name backup models for when its model is unavailable, with
settings `fallback_models` (`{"default": [...], "plan": [...], "smol": [...]}`),
`-fallback-model` and `HARNESS_FALLBACK_MODELS` for the default role. The flag and
variable set `fallback_models.default` in their layers, so they replace the default
role's list and leave the other roles' entries from lower layers in place. Plan and
smol use the default role's list unless they have their own. `serve`, `mcp` and SDK
sessions run every role on one session model behind the default role's list. See
[Backup models](providers.md#backup-models).

## Flags

| Flag | Purpose |
|---|---|
| `-fallback-model <route>` | Backup model for the default role, tried in order when the model is unavailable; repeatable. Also on `serve` and `mcp`. See [Backup models](providers.md#backup-models). |
| `-max-turns <n>`, `-max-cost <usd>`, `-max-wall-time <duration>` | Per-run budgets; see [Runs](sessions-and-durability.md#runs). Zero means unlimited. |
| `-max-input-tokens <n>`, `-max-output-tokens <n>`, `-max-total-tokens <n>`, `-max-tool-calls <n>` | Per-run token and tool-call caps, counted over the whole run including subagents (not per request, unlike `-max-tokens`); see [Budgets](sessions-and-durability.md#budgets). Zero means unlimited. Settings `max_input_tokens`, `max_output_tokens`, `max_total_tokens`, `max_tool_calls` and `HARNESS_MAX_INPUT_TOKENS`, `HARNESS_MAX_OUTPUT_TOKENS`, `HARNESS_MAX_TOTAL_TOKENS`, `HARNESS_MAX_TOOL_CALLS` set them too. Also on `serve` and `mcp`. |
| `-bash-timeout <duration>` | Default shell-command timeout (10m); `HARNESS_BASH_TIMEOUT` or settings `bash_timeout` can set it too. A model-supplied `timeout_seconds` overrides it, from 1 up to 2147483 seconds. Timeouts kill the whole process group. |
| `-enable-task` | Register the `task` tool with the built-in `explore` (read-only, `smol` role) and `general` (full coding, default role) subagents. |
| `-plan` | Run the read-only plan stack on the `plan` role model. Only read/search/list tools (including `ast_grep` when installed and authenticated `gh`), `manifest_diff`, enabled `lsp_*` tools (rename computes a plan only) and enabled read-only web/attachment tools are available; `-faux-script` still overrides the model provider. |
| `-attach <path>` | Attach a local file to the session; repeatable. Missing paths are startup errors. |
| `-image <path>` | Attach a PNG, JPEG, GIF or WebP image to the prompt as an image block; repeatable. Refused at startup when the model has no image input in the catalog or the image exceeds the `view_image` limits. |
| `-output-schema <path>` | JSON Schema for the final answer; see [Runs](sessions-and-durability.md#runs). |
| `-reasoning <level>`, `-plan-reasoning`, `-smol-reasoning` | Reasoning effort: `off`, `minimal`, `low`, `medium` or `high`; unset leaves the provider default. Plan and smol inherit `-reasoning`. Sent as OpenAI `reasoning_effort`, OpenRouter `reasoning.effort`, Gemini `thinkingLevel`, and an Anthropic thinking budget or adaptive effort. A model the catalog lists without reasoning gets no field and one warning. `serve` and `mcp` take `-reasoning`; `doctor` shows each role's effective value. |
| `-enable-web` | Enable the gated `web_search` and `web_fetch` tools. |
| `-enable-lsp` | Register the `lsp_*` tools; language servers start on first use. See [Language servers](code-intelligence.md#language-servers). |
| `-lsp-config <path>` | Operator language-server file; replaces the built-in server list. An empty path selects the built-ins. |
| `-cloud-policy <path>` | Operator kube-context rules for `kubectl` and `helm` writes. See [Cloud commands](permissions.md#cloud-commands). |
| `-no-hooks` | Disable all command hooks (`HARNESS_NO_HOOKS`, user `no_hooks`). |
| `-trust-project-hooks` | Authorize project hook commands and the other executable project settings in [Project settings and trust](#project-settings-and-trust) (`HARNESS_TRUST_PROJECT_HOOKS`); only use for trusted workspaces. |
| `-trust-project-providers` | Authorize provider settings in project files (`HARNESS_TRUST_PROJECT_PROVIDERS`); they are ignored without it. See [Providers](providers.md#project-provider-definitions). |
| `-profile <name>` | Apply `profiles.<name>` from the settings files (`HARNESS_PROFILE`). See [Profiles](#profiles). |
| `-c key.path=value` | Override one setting at the highest layer; repeatable. See [Overrides with `-c`](#overrides-with--c). |
| `-strict-config` | Fail on unknown settings keys instead of warning (`HARNESS_STRICT_CONFIG`). |
| `-skill <path>` | Additional skill directory or `.md` file; repeatable. See [Skills](skills-and-context.md#skill-format). |
| `-permission-mode <mode>` | Tool gate mode: `default`, `plan`, `acceptEdits` or `bypass`. |
| `-allow-tool <name>`, `-deny-tool <name>` | Always allow or deny one tool; repeatable, deny wins. A name that is neither a built-in tool nor `mcp__*` gets a stderr warning on one-shot runs, `serve` and `mcp`. |
| `-faux-script <path>` | Replace the model with a scripted one; see [Testing without a provider](testing.md#testing-without-a-provider). |

Every command takes flags only: the prompt goes in `-p`. A stray argument, such as
`run` in `tempest run -p hi`, or an unknown flag exits with status 2 and names the
problem.

## Environment variables

| Setting | Purpose |
|---|---|
| `ANTHROPIC_API_KEY` / `ANTHROPIC_OAUTH_TOKEN` / `ANTHROPIC_AUTH_TOKEN` | Anthropic credential (key, OAuth token, or proxy bearer). |
| `ANTHROPIC_BASE_URL` | Anthropic-compatible provider URL. `-anthropic-base-url`, `HARNESS_ANTHROPIC_BASE_URL` and settings `anthropic_base_url` override it, in that order. |
| `OPENAI_API_KEY` | OpenAI-compatible credential. |
| `OPENAI_BASE_URL` | OpenAI-compatible endpoint for `openai:<id>` models without `@<base-url>`. `-openai-base-url`, `HARNESS_OPENAI_BASE_URL` and settings `openai_base_url` override it, in that order. |
| `GEMINI_API_KEY` / `GOOGLE_API_KEY`, `OPENROUTER_API_KEY` | Google and OpenRouter credentials. Point those routes elsewhere with `google:<id>@<base-url>` or `openrouter:<id>@<base-url>`. |
| Preset and cloud credentials | `GROQ_API_KEY`, `MISTRAL_API_KEY`, `XAI_API_KEY`, `DEEPSEEK_API_KEY`, `TOGETHER_API_KEY`, `FIREWORKS_API_KEY`, `CEREBRAS_API_KEY`, `PERPLEXITY_API_KEY`, `AZURE_OPENAI_API_KEY`, `AWS_BEARER_TOKEN_BEDROCK`, and the AWS and Google Cloud variables in [Providers](providers.md#protocol-adapters). |
| `HARNESS_MODEL`, `HARNESS_PLAN_MODEL`, `HARNESS_SMOL_MODEL` | Model for each role. |
| `HARNESS_FALLBACK_MODELS` | Comma-separated backup models for the default role (settings `fallback_models`). |
| `HARNESS_REASONING`, `HARNESS_PLAN_REASONING`, `HARNESS_SMOL_REASONING` | Reasoning effort per role (settings `reasoning`, `plan_reasoning`, `smol_reasoning`). |
| `HARNESS_PERMISSION_MODE` | Default tool gate mode. |
| `HARNESS_PERMISSION_ALLOW_TOOLS`, `HARNESS_PERMISSION_DENY_TOOLS` | Per-tool allow and deny lists. |
| `HARNESS_SESSION_DIR`, `HARNESS_AGENT_DIR` | Session and agent data directories. |
| `HARNESS_BASH_TIMEOUT` | Default bash timeout as a Go duration, for example `1s` or `10m`. |
| `HARNESS_MAX_INPUT_TOKENS`, `HARNESS_MAX_OUTPUT_TOKENS`, `HARNESS_MAX_TOTAL_TOKENS`, `HARNESS_MAX_TOOL_CALLS` | Per-run token and tool-call caps (settings `max_input_tokens`, `max_output_tokens`, `max_total_tokens`, `max_tool_calls`). |
| `HARNESS_RETRY_*` | Retry policy: `ENABLED`, `MAX_ATTEMPTS`, `BASE_DELAY_MS`, `BACKOFF_CAP_MS`, `MAX_DELAY_MS`, `JITTER_MIN`, `JITTER_MAX`. |
| `HARNESS_WORKSPACE_LISTING`, `HARNESS_WORKSPACE_LISTING_MAX_FILES`, `HARNESS_WORKSPACE_LISTING_MAX_BYTES` | Session-start file listing and its caps (settings `workspace_listing`, `workspace_listing_max_files`, `workspace_listing_max_bytes`). |
| `HARNESS_STREAM_IDLE_SECONDS`, `HARNESS_STREAM_FIRST_OUTPUT_SECONDS` | Stream stall bounds: seconds without an SSE event once output has started (default 90), and before the first output (default 300, never below the idle bound). |
| `HARNESS_WEB_SEARCH_URL` | Web-search endpoint when web tools are enabled. |
| `HARNESS_ENABLE_LSP`, `HARNESS_LSP_CONFIG` | Language-server tools and server file (settings `enable_lsp`, `lsp_config`). |
| `HARNESS_CLOUD_POLICY` | Kube-context rule file (settings `cloud_policy`). |
| `HARNESS_NO_HOOKS`, `HARNESS_TRUST_PROJECT_HOOKS` | Disable all hooks, or explicitly authorize project hooks and other executable project settings. |
| `HARNESS_TRUST_PROJECT_PROVIDERS` | Trust provider settings in project files for this run. |
| `HARNESS_PROFILE`, `HARNESS_STRICT_CONFIG` | Settings profile, and strict settings validation, as `-profile` and `-strict-config`. |
| `HARNESS_MCP_CONFIG` | MCP server configuration file. An explicit empty path disables MCP. |
| `HARNESS_MCP_SAMPLING_MODEL`, `HARNESS_MCP_SAMPLING_MAX_TOKENS`, `HARNESS_MCP_SAMPLING_ALLOW_HEADLESS` | MCP sampling model, output cap and headless approval (settings `mcp_sampling_model`, `mcp_sampling_max_tokens`, `mcp_sampling_allow_headless`). See [Sampling](serve-and-protocols.md#sampling). |
| `HARNESS_FAUX_SCRIPT` | Scripted model for runs without a provider. |
| `HARNESS_LOG_LEVEL`, `HARNESS_DEBUG`, `HARNESS_LOG_FILE`, `HARNESS_LOG_FILE_MAX_BYTES` | Logging controls. |

## Settings file

`~/.config/harness/settings.json` holds user settings. Every settings file, the
managed `defaults`, and every profile take the same keys; project files are subject
to [Project settings and trust](#project-settings-and-trust). Each key below, and
each flag and variable above, names the same setting. The main keys:

| Key | Purpose |
|---|---|
| `model`, `plan_model`, `smol_model`, `max_tokens` | Role models and the output-token cap. |
| `max_input_tokens`, `max_output_tokens`, `max_total_tokens`, `max_tool_calls` | Per-run caps on prompt tokens, output tokens, both together, and tool calls (default 0, unlimited). A repository may only lower them. See [Budgets](sessions-and-durability.md#budgets). |
| `anthropic_base_url`, `openai_base_url` | Provider endpoints, overridden by flags and `HARNESS_*` variables. |
| `fallback_models` | Backup models per role: `default`, `plan`, `smol`. A role without an entry uses `default`'s list; `[]` turns backups off for it. |
| `reasoning`, `plan_reasoning`, `smol_reasoning` | Reasoning effort per role. |
| `permission_mode_default`, `permission_allow_tools`, `permission_deny_tools` | Tool gate mode and per-tool lists. |
| `bash_timeout` | Default shell-command timeout. |
| `enable_lsp`, `lsp_config` | Language-server tools and server file. |
| `cloud_policy` | Kube-context rule file. |
| `hooks`, `no_hooks`, `trusted_hook_projects` | Hook commands, disable hooks, or trust specific project paths. |
| `mcp_config`, `mcp_servers` | MCP configuration file or inline server array. A selected file replaces the array completely. |
| `mcp_sampling_model`, `mcp_sampling_max_tokens`, `mcp_sampling_allow_headless` | Model for MCP server sampling (default: the `smol` role), the output cap of one request (default: `max_tokens`), and whether opted-in servers may sample when no user can approve (default `false`). |
| `providers` | Endpoint registry keyed by route name. See [Providers](providers.md#endpoint-registry). |
| `trusted_provider_projects` | Absolute canonical paths whose project provider settings are trusted. |
| `otel` | OpenTelemetry export: endpoints, headers, service name, content capture. Each key also reads its standard `OTEL_*` variable. See [Telemetry](telemetry.md#settings). |
| `skill_paths` | Extra skill directories; `-skill` flags replace the list. |
| `workspace_listing`, `workspace_listing_max_files`, `workspace_listing_max_bytes` | Session-start file listing in the first user message (default on, 10000 paths, 4096 bytes). See [Skills and context](skills-and-context.md#workspace-listing). |
| `context_files` | Object: `fallback` file names tried after `AGENTS.md` (default `["CLAUDE.md"]`), `max_bytes`, the total context file cap (default 65536), `nested_max_bytes` per nested file (default 16384, at most `max_bytes`), and `nested` on-read discovery below the working directory (default `true`). See [Skills and context](skills-and-context.md#context-discovery). |
| `reminders` | Array of reminders added at the tail of model requests: `name`, `text`, `scope` (`request` or `conversation`), `every`, `first`, `max_fires` (default 1) and `when` (`final_turn`, `context_percent`, `tool_calls`, `without_tool`). Default none; a repository needs trust to add any. See [Skills and context](skills-and-context.md#reminders). |
| `loop_detection` | Object: the loop guard's thresholds `repeated_result` (default 3), `repeated_poll` (10) and `repeated_text` (3); each is `0` (that pattern off) or at least 2. See [Loops](sessions-and-durability.md#loops). |
| `tool_search` | Object: when MCP and extension tool schemas wait behind the `tool_search` tool. `mode` is `"auto"` (default: defer once their schemas reach `threshold_tokens`, estimated at 4 characters a token, default 10000, then for the rest of the conversation), `"always"` or `"never"`. See [Deferred tools](tools.md#deferred-tools). |
| `faux_script` | Scripted model, as `-faux-script`. |
| `profiles` | Named partial settings objects; see [Profiles](#profiles). |

Run `./bin/tempest -help` for the full flag list and the `serve`, `mcp`, `doctor`,
`config`, `rewind` and `version` subcommands.

[Guide index](README.md) · [Tempest](../README.md)
