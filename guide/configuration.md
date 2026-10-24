# Configuration

Three layers set behavior. Each layer overrides the one below it:

1. Flags.
2. `HARNESS_*` environment variables.
3. `~/.config/harness/settings.json`.
4. Built-in defaults.

The command does not read `.env`. Export variables before running it.

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

## Flags

| Flag | Purpose |
|---|---|
| `-max-turns <n>`, `-max-cost <usd>`, `-max-wall-time <duration>` | Per-run budgets; see [Runs](sessions-and-durability.md#runs). Zero means unlimited. |
| `-bash-timeout <duration>` | Default shell-command timeout (10m); `HARNESS_BASH_TIMEOUT` or settings `bash_timeout` can set it too. A positive model-supplied `timeout` in seconds overrides it, up to 2147483 seconds. Timeouts kill the whole process group. |
| `-enable-task` | Register the `task` tool with the built-in `explore` (read-only, `smol` role) and `general` (full coding, default role) subagents. |
| `-plan` | Run the read-only plan stack on the `plan` role model. Only read/search/list tools, `manifest_diff`, enabled `lsp_*` tools (rename computes a plan only) and enabled read-only web/attachment tools are available; `-faux-script` still overrides the model provider. |
| `-attach <path>` | Attach a local file to the session; repeatable. Missing paths are startup errors. |
| `-image <path>` | Attach a PNG, JPEG, GIF or WebP image to the prompt as an image block; repeatable. Refused at startup when the model has no image input in the catalog or the image exceeds the `view_image` limits. |
| `-output-schema <path>` | JSON Schema for the final answer; see [Runs](sessions-and-durability.md#runs). |
| `-reasoning <level>`, `-plan-reasoning`, `-smol-reasoning` | Reasoning effort: `off`, `minimal`, `low`, `medium` or `high`; unset leaves the provider default. Plan and smol inherit `-reasoning`. Sent as OpenAI `reasoning_effort`, OpenRouter `reasoning.effort`, Gemini `thinkingLevel`, and an Anthropic thinking budget or adaptive effort. A model the catalog lists without reasoning gets no field and one warning. `serve` and `mcp` take `-reasoning`; `doctor` shows each role's effective value. |
| `-enable-web` | Enable the gated `web_search` and `web_fetch` tools. |
| `-enable-lsp` | Register the `lsp_*` tools; language servers start on first use. See [Language servers](code-intelligence.md#language-servers). |
| `-lsp-config <path>` | Operator language-server file; replaces the built-in server list. An empty path selects the built-ins. |
| `-cloud-policy <path>` | Operator kube-context rules for `kubectl` and `helm` writes. See [Cloud commands](permissions.md#cloud-commands). |
| `-no-hooks` | Disable all command hooks (`HARNESS_NO_HOOKS`, user `no_hooks`). |
| `-trust-project-hooks` | Authorize project and local hook commands (`HARNESS_TRUST_PROJECT_HOOKS`); only use for trusted workspaces. |
| `-trust-project-providers` | Authorize provider definitions in project settings (`HARNESS_TRUST_PROJECT_PROVIDERS`); project files are ignored without it. See [Providers](providers.md#project-provider-definitions). |
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
| `HARNESS_REASONING`, `HARNESS_PLAN_REASONING`, `HARNESS_SMOL_REASONING` | Reasoning effort per role (settings `reasoning`, `plan_reasoning`, `smol_reasoning`). |
| `HARNESS_PERMISSION_MODE` | Default tool gate mode. |
| `HARNESS_PERMISSION_ALLOW_TOOLS`, `HARNESS_PERMISSION_DENY_TOOLS` | Per-tool allow and deny lists. |
| `HARNESS_SESSION_DIR`, `HARNESS_AGENT_DIR` | Session and agent data directories. |
| `HARNESS_BASH_TIMEOUT` | Default bash timeout as a Go duration, for example `1s` or `10m`. |
| `HARNESS_RETRY_*` | Retry policy: `ENABLED`, `MAX_ATTEMPTS`, `BASE_DELAY_MS`, `BACKOFF_CAP_MS`, `MAX_DELAY_MS`, `JITTER_MIN`, `JITTER_MAX`. |
| `HARNESS_STREAM_IDLE_SECONDS`, `HARNESS_STREAM_FIRST_OUTPUT_SECONDS` | Stream stall bounds: seconds without an SSE event once output has started (default 90), and before the first output (default 300, never below the idle bound). |
| `HARNESS_WEB_SEARCH_URL` | Web-search endpoint when web tools are enabled. |
| `HARNESS_ENABLE_LSP`, `HARNESS_LSP_CONFIG` | Language-server tools and server file (settings `enable_lsp`, `lsp_config`). |
| `HARNESS_CLOUD_POLICY` | Kube-context rule file (settings `cloud_policy`). |
| `HARNESS_NO_HOOKS`, `HARNESS_TRUST_PROJECT_HOOKS` | Disable all hooks, or explicitly authorize project and local hooks. |
| `HARNESS_TRUST_PROJECT_PROVIDERS` | Trust provider definitions in project settings for this run. |
| `HARNESS_MCP_CONFIG` | MCP server configuration file. An explicit empty path disables MCP. |
| `HARNESS_FAUX_SCRIPT` | Scripted model for runs without a provider. |
| `HARNESS_LOG_LEVEL`, `HARNESS_DEBUG`, `HARNESS_LOG_FILE`, `HARNESS_LOG_FILE_MAX_BYTES` | Logging controls. |

## Settings file

`~/.config/harness/settings.json` holds user settings. Project settings in
`.harness/settings.json` and `.harness/settings.local.json` may add hooks, but their
commands never run without explicit operator trust. See [hooks](hooks.md#trust).

| Key | Purpose |
|---|---|
| `anthropic_base_url`, `openai_base_url` | Provider endpoints, overridden by flags and `HARNESS_*` variables. |
| `reasoning`, `plan_reasoning`, `smol_reasoning` | Reasoning effort per role. |
| `bash_timeout` | Default shell-command timeout. |
| `enable_lsp`, `lsp_config` | Language-server tools and server file. |
| `cloud_policy` | Kube-context rule file. |
| `no_hooks`, `trusted_hook_projects` | Disable hooks, or trust specific project paths. |
| `mcp_config`, `mcp_servers` | MCP configuration file or inline server array. A selected file replaces the array completely. |
| `providers` | Endpoint registry keyed by route name. See [Providers](providers.md#endpoint-registry). |
| `trusted_provider_projects` | Absolute canonical paths whose project provider definitions are trusted. |
| `skill_paths` | Extra skill directories when no `-skill` flag is given. |
| `faux_script` | Scripted model, as `-faux-script`. |

Run `./bin/tempest -help` for the full flag list and the `serve`, `mcp`, `doctor`,
`rewind` and `version` subcommands.

[Guide index](README.md) · [Tempest](../README.md)
