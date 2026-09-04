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

`-model` selects the model: a bare name, `anthropic:model`, `google:model` (Gemini
via `GEMINI_API_KEY`), or `openrouter:<vendor/model>` via `OPENROUTER_API_KEY`;
`openai:model@baseURL` targets any OpenAI-compatible endpoint with that base URL
(OpenRouter, Together, Groq, Ollama, vLLM all work this way).
Output is text by default; `-output-format json` prints the final message as JSON.

## Features

### 01 · One binary, every provider

Anthropic, OpenAI, Google Gemini, and OpenRouter, plus any OpenAI-compatible
endpoint by base URL — chosen per run, mixed per role. A bare model name resolves
against the embedded catalog; an unknown provider is a configuration error, never
a silent fallback. No SDK zoo, no wrapper processes: the provider layer speaks
each wire protocol directly.

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

Every run is journaled to an append-only JSONL file. `-continue` picks up the most
recent session for the current directory; `-resume <id>` restores a specific one. The
prior transcript seeds the agent and only new messages are appended — and sessions
are isolated by working directory, so a resume never bleeds another project's history
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

## Flags

| Flag | Purpose |
|---|---|
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
