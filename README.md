<p align="center">
  <strong>Tempest</strong>
</p>

<p align="center">
  <strong>A blazing-fast coding agent in pure Go.</strong>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.26-00ADD8?style=flat&colorA=222222&logo=go&logoColor=white" alt="Go 1.26">
  <img src="https://img.shields.io/badge/license-MIT-58A6FF?style=flat&colorA=222222" alt="License MIT">
  <img src="https://img.shields.io/badge/binary-single%20static-3FB950?style=flat&colorA=222222" alt="Single static binary">
  <img src="https://img.shields.io/badge/deps-zero%20runtime-E05735?style=flat&colorA=222222" alt="Zero runtime deps">
</p>

<p align="center">
  In the spirit of <a href="https://github.com/badlogic/pi-mono">Pi</a>, Claude Code, and opencode — built to fit your coding workflow.
</p>

One prompt in, real work out. Tempest reads and edits your files, runs commands, and
drives itself turn by turn until the job is done — as a single static Go binary with
no Node runtime, no interpreter, and nothing to install but the executable.

**3** provider families · **13** built-in tools (**8** on by default) · **one** static binary · **zero** runtime deps.

## Install

```sh
git clone https://github.com/parthshr370/tempest.git
cd tempest
go build -o tempest ./cmd/harness
```

That produces a single self-contained `tempest` binary you can drop on your `PATH`.

## Quick start

```sh
ANTHROPIC_API_KEY=... ./tempest -p "Create hello.txt with hi"
```

`-model` selects the model: a bare Anthropic route ID, `anthropic:<id>`,
`google:<id>` (via `GEMINI_API_KEY` or `GOOGLE_API_KEY`), or
`openrouter:~google/gemini-flash-latest` (via `OPENROUTER_API_KEY`).
`openai:<id>@<base-url>` targets an OpenAI Chat Completions-compatible endpoint;
use `OPENAI_API_KEY` for that route. The ID is opaque and endpoint-specific:
`claude-fable-5-1` on Anthropic differs from `anthropic/claude-fable-5.1`
on OpenRouter, whose aliases include a literal `~` prefix.
Output is text by default; `-output-format json` prints a run report (see
[Runs](#runs)).

## Features

### 01 · One binary, every provider

Keep four concepts separate:

- **Model:** the endpoint's exact route ID, never rewritten or normalized.
- **Endpoint:** the HTTP service and credential that serve that ID; an explicit
  `openai:<id>@<base-url>` overrides the configured OpenAI endpoint.
- **Protocol adapter:** Anthropic Messages, OpenAI Chat Completions, or Google
  Generative AI. OpenRouter uses the OpenAI adapter with its own endpoint/key.
  Responses, Bedrock, and Vertex protocols aren't implemented.
- **Catalog metadata:** embedded context/output limits, capabilities, compatibility
  controls, and USD-per-million prices. Catalog membership isn't proof of endpoint
  access, protocol compatibility, or account permission.

Unknown model IDs still run with provider-default limits and **unknown price**,
not a fabricated free price. Computed JSON costs encode `{"unknown":true}` rather
than zero-dollar components. A cost budget requires known pricing. Unknown provider
prefixes are configuration errors; bare IDs use the Anthropic route.

`internal/catalog/models.json` records import provenance (`generated_from`, `as_of`)
and each entry's `source` URL and `verified_at` date. Most direct-provider entries
retain the imported snapshot's date; GPT-5.6 Luna's route and metadata were checked
against OpenAI's model page on 2026-10-21. OpenRouter entries use the public discovery
snapshot from that date. Discovery confirms exact IDs and advertised metadata, not
a successful generation request. To validate the embedded JSON and the live IDs:

```sh
go test ./internal/catalog/...
TEMPEST_LIVE=1 go test ./internal/catalog/ -run TestVerifyOpenRouter
```

`./tempest doctor` is offline and never prints credentials. `./tempest doctor --ping`
makes one billable request per configured provider: prompt `hi`, output cap 1 token,
using the catalog model with the lowest input-plus-output price. It reports the
model and latency on success, or a typed failure code. Endpoint/account restrictions
can still reject a discovered model.

### 02 · Hash-anchored edits that never corrupt a file

The model points at content by hash instead of retyping whole lines, so whitespace
battles and "string not found" loops just stop. Edit a file that drifted and the
anchors diverge — the patch is rejected before it can scramble your code, with a
recovery hint the model uses to re-read and retry.

### 03 · Robust error handling that nudges the model back on track

The run bends, it doesn't break. Transient provider failures — rate limits, resets,
timeouts — are sorted by a typed error taxonomy and retried with exponential backoff
and jitter; only genuinely terminal conditions stop the loop, and context overflow
is handed off to compaction instead of failing. Tool errors take the other road:
a stale-anchor edit, a blocked destructive command, a bad path, a failing shell
command each come back to the model as a clear, actionable result, so it re-reads,
re-anchors, and course-corrects on the next turn instead of corrupting state.

### 04 · Resumable sessions

Every run is journaled to an append-only JSONL file as it happens: each message, each
tool call's start and finish, per-call usage, and how the run ended. A tool's start is
on disk before the tool runs. `-continue` picks up the most recent session for the
current directory; `-resume <id>` restores a specific one. If the process died mid-run,
a tool call that had started but not finished comes back to the model as "outcome
unknown" and is never re-run automatically; a torn final line is trimmed. Sessions are
isolated by working directory, so a resume never bleeds another project's history
into yours.

### 05 · Tiered permissions with a destructive-command guard

Every tool call passes one chokepoint. Each tool carries a `read`, `write`, or `exec`
tier, and the permission mode sets the baseline: `plan` runs read-only, `default`
allows edits and non-destructive shell, `bypass` opens everything you don't explicitly
deny. `rm -rf`, fork bombs, and file-clobbering redirects are blocked outside `bypass`
no matter the mode.

### 06 · Model roles, routed by intent

A `default` agent for normal turns, a read-only `plan` model (used by `-plan`), and a
small `smol` helper (the model behind the `explore` subagent of `-enable-task`) — each
routable to a different provider and model. Set them with
`-model`/`-plan-model`/`-smol-model` or the matching `HARNESS_*_MODEL` variables.

### 07 · Context that discovers itself

At startup the agent walks `AGENTS.md` up from your working directory, loads a sticky
`RULES.md` that survives compaction, expands safe `@file` imports, and registers
bundled or `-skill`-supplied skills. Skill bodies load on demand through the `skill`
tool or a `skill://` read — nothing bloats the prompt until it's needed.

### 08 · Subagents, behind `-enable-task`

`-enable-task` registers the `task` tool with two built-in subagent types: `explore`,
read-only investigation on the `smol` role, and `general`, full coding on the default
role. Children never get the `task` tool, so delegation never recurses.

### 09 · Reads that summarize, not dump

Point `read` at a source file and it returns a structural outline — declarations
without bodies — so the agent orients in a large file without spending the context
window on lines it doesn't need yet.

## Every tool, in one namespace

Thirteen built-in tools live beside `read` and `bash`, **8 of them on by default**; pin
the active set with `-exclude-tools`, and gate the extras behind flags (see Flags below).
All file tools are contained to the working directory via `os.Root`, and `grep`/`find`
fall back to pure Go when `rg`/`fd` are absent.

**Files & search**

- `read` — files, directories, and internal `://` resources through one path, with structural outlines for code.
- `write` — create or overwrite a file.
- `edit` — hash-anchored patches with stale-anchor rejection and recovery.
- `grep` — regex search across the tree.
- `find` — glob-based path lookup.
- `ls` — directory listing.

**Runtime**

- `bash` — workspace shell with a destructive-command guard.

**Coordination**

- `task` — fan out subagents and collect their results *(behind `-enable-task`)*.
- `todo_write` — ordered task list with phase tracking.

**Context**

- `skill` — load a skill body on demand when skills are loaded (also via `skill://`).
- `attachment` — read resolved session attachments *(behind `-attach`)*.

**Web** *(gated behind `-enable-web`)*

- `web_search` — one query against the configured search endpoint.
- `web_fetch` — fetch a URL as bounded, structured text.

## Runs

A run is one `-p` prompt. The model calls tools, sees their results, and keeps
going until it replies with no tool calls; that reply completes the run. There is
no completion tag and no re-prompting. Every run ends with a stop reason:

| Stop reason | Meaning | Exit code |
|---|---|---|
| `completed` | The model replied without calling a tool. | 0 |
| `max_turns`, `max_cost`, `max_wall_time` | A budget tripped. | 0 |
| `aborted` | SIGINT or SIGTERM. Running tools are cancelled and the run waits for them to return before it ends; a second signal kills the process. | 130 |
| `error` | The provider failed, or the session journal could not be written. | 1 |

Budgets are checked at turn boundaries: after a turn's tools finish and before
the next model call. A budget-stopped run therefore ends on a complete turn, and
`-continue` picks the session up from there. One long turn can overrun
`-max-wall-time`.

- `-max-turns N` caps model calls.
- `-max-cost USD` caps the summed cost of the run's model calls. When the
  model's price is unknown the run is refused at startup: an unknown price is
  not a zero price.
- `-max-wall-time D` caps elapsed time, as a Go duration such as `10m`.

`-output-format json` prints a run report:

```json
{"stop_reason": "max_turns", "turns": 3, "usage": {"input": 5120, "output": 410, "...": "..."},
 "cost_usd": 0.0213, "elapsed_ms": 18450, "text": "last assistant text", "message": {"role": "toolResult", "...": "..."}}
```

`cost_usd` is `null` when any call's price was unknown, and `error` appears on
`error` stops. `message` is the transcript's last message. In text mode the last
assistant text goes to stdout, and a stop reason other than `completed` goes to
stderr.

## Flags

| Flag | Purpose |
|---|---|
| `-max-turns <n>`, `-max-cost <usd>`, `-max-wall-time <duration>` | Per-run budgets; see [Runs](#runs). Zero means unlimited. |
| `-enable-task` | Register the `task` tool with the built-in `explore` (read-only, `smol` role) and `general` (full coding, default role) subagents. |
| `-plan` | Run the read-only plan stack on the `plan` role model. |
| `-attach <path>` | Attach a local file to the session; repeatable. Missing paths are startup errors. |
| `-enable-web` | Enable the gated `web_search` and `web_fetch` tools. |
| `-skill <path>` | Additional skill location; repeatable. |

## Configuration

Flags win over `HARNESS_*` environment variables, which win over
`~/.config/harness/settings.json`, which win over built-in defaults. The command does
not read `.env` — export variables before running it.

| Setting | Purpose |
|---|---|
| `ANTHROPIC_API_KEY` / `ANTHROPIC_OAUTH_TOKEN` / `ANTHROPIC_AUTH_TOKEN` | Anthropic credential (key, OAuth token, or proxy bearer). |
| `ANTHROPIC_BASE_URL` / `HARNESS_ANTHROPIC_BASE_URL` | Anthropic-compatible provider URL. |
| `OPENAI_API_KEY`, `OPENAI_BASE_URL` | OpenAI-compatible credential and endpoint. |
| `HARNESS_MODEL`, `HARNESS_PLAN_MODEL`, `HARNESS_SMOL_MODEL` | Model for each role. |
| `HARNESS_PERMISSION_MODE` | Default tool gate mode. |
| `HARNESS_PERMISSION_ALLOW_TOOLS`, `HARNESS_PERMISSION_DENY_TOOLS` | Per-tool allow/deny lists. |
| `HARNESS_SESSION_DIR`, `HARNESS_AGENT_DIR` | Session and agent data directories. |
| `HARNESS_RETRY_*` | Retry policy: `ENABLED`, `MAX_ATTEMPTS`, `BASE_DELAY_MS`, `BACKOFF_CAP_MS`, `MAX_DELAY_MS`, `JITTER_MIN`, `JITTER_MAX`. |
| `HARNESS_WEB_SEARCH_URL` | Web-search endpoint when web tools are enabled. |
| `HARNESS_LOG_LEVEL`, `HARNESS_DEBUG`, `HARNESS_LOG_FILE`, `HARNESS_LOG_FILE_MAX_BYTES` | Logging controls. |

Run `./tempest -help` for the full flag list.

## Development

```sh
go build ./...
go test ./...
make check   # build + vet + test + race
make smoke   # end-to-end run in a scratch dir with the faux provider
```

## Learn how it works

The [`education/`](education/README.md) directory is a guided, subsystem-by-subsystem
tour of the codebase: the streaming engine and agent loop, providers and SSE
transport, the tool layer and hash-anchored editing, config and model roles, retry
and typed errors, concurrency, compaction, the prompt system, permissions, and the
idiomatic-Go decisions behind each.

## License

MIT — see [`LICENSE`](LICENSE). Behavior is ported from
[Pi](https://github.com/badlogic/pi-mono) by [Mario Zechner](https://github.com/mariozechner),
extended via [oh-my-pi](https://github.com/can1357/oh-my-pi) by Can Bölük; full
attribution in [`THIRD_PARTY_NOTICES`](THIRD_PARTY_NOTICES).
