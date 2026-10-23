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

**3** provider families · **17** built-in tools (**12** on by default) · **one** static binary · **zero** runtime deps.

## Install

```sh
git clone https://github.com/parthshr370/tempest.git
cd tempest
go build -o bin/tempest ./cmd/harness
```

That produces a single self-contained `bin/tempest` binary you can drop on your `PATH`.

## Quick start

```sh
ANTHROPIC_API_KEY=... ./bin/tempest -p "Create hello.txt with hi"
```

`-model` selects the model: a bare Anthropic route ID, `anthropic:<id>`,
`google:<id>` (via `GEMINI_API_KEY` or `GOOGLE_API_KEY`), or
`openrouter:~google/gemini-flash-latest` (via `OPENROUTER_API_KEY`).
`openai:<id>@<base-url>` targets an OpenAI Chat Completions-compatible endpoint;
use `OPENAI_API_KEY` for that route. The ID is opaque and endpoint-specific:
`claude-fable-5-1` on Anthropic differs from `anthropic/claude-fable-5.1`
on OpenRouter, whose aliases include a literal `~` prefix.
Output is text by default; `-output-format json` prints a run report and
`-output-format stream-json` streams NDJSON updates followed by that report
(see [Runs](#runs)).

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

`./bin/tempest doctor` is offline and never prints credentials; it prints a one-line
catalog count, and `-models` lists every catalog model with its prices.
`./bin/tempest doctor --ping` makes one billable request (prompt `hi`, output cap
16 tokens) to each configured model: the default model, plus the plan and smol
models when they differ. Each line names the roles and the exact model it pinged,
then latency on success, or the typed failure code with the provider's HTTP status
and bounded, redacted message, e.g. `provider HTTP 404: {...}` for an
account/endpoint restriction.

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
current directory; `-resume <id>` restores a specific one. The CLI prints the id on
stderr in text mode and as `session_id` in JSON reports. You can also pass the
journal filename (with or without `.jsonl`) instead of the bare id; ambiguous ids
are rejected with filename hints. If the process died mid-run,
a tool call that had started but not finished comes back to the model as "outcome
unknown" and is never re-run automatically; a torn final line is trimmed. Sessions are
isolated by working directory, so a resume never bleeds another project's history
into yours.

Journals now use format **v3**. Opening a v1/v2 journal upgrades it in place under
the writer lock before new facts are written. Older binaries refuse v3: this is
an intentional on-disk downgrade barrier, not a protocol-version change.
Back up or move each `.jsonl` together with its `.jsonl.d` directory; committed
compaction snapshots and child journals are required recovery data, not caches.

### 05 · Tiered permissions with a destructive-command guard

Every tool call passes one chokepoint. Each tool carries a `read`, `write`, or `exec`
tier, and the permission mode sets the baseline: `plan` runs read-only, `default`
and `acceptEdits` allow edits and non-destructive shell, and `bypass` opens everything
you don't explicitly deny.

Outside `bypass`, the shell guard requires approval for deleting files/directories,
overwriting or truncating existing files, in-place edits, and destructive git
operations (`reset --hard`, `clean -f`, `checkout -- <path>`, `restore`, force push,
and `branch -D`). It catches direct removals, `find -delete`/`-exec rm`, `xargs rm`,
`sed -i`/`perl -i`, `truncate`, `dd of=`, and `shred`. Existing protections for
dangerous system commands, package-changing commands, and fork bombs remain.

Appends (`>>`, `tee -a`) and writes to new redirect, `tee`, `cp`, or `mv` targets
don't require guard approval. Targets resolve relative to the command's working
directory; copying/moving into a directory checks the resulting destination file.
If the guard can't determine whether a target exists, it asks. `git stash` is
allowed because it's reversible (discarding stashes with `drop`/`clear` still asks).
Plan mode still rejects non-destructive writes.

The guard also recognizes obvious deletion/overwrite calls in `python -c`,
`python3 -c`, `node -e`, `perl -e`, and `ruby -e` code, including `rmtree`,
`os.remove`, `unlink`, `rmdir`, `fs.rm`, `unlinkSync`, and `open(..., 'w')`.
Arbitrary scripts, obfuscated code, and shell expansions can't be fully classified:
this is a guardrail, **not a sandbox**, and existence checks aren't atomic with
execution. Guard approval runs before ordinary allow/Always choices; even an
Always answer applies only once and the next destructive call asks again.
Without an interactive prompter, guarded commands are denied.

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

`-enable-task` registers the asynchronous `task` tool with two built-in subagent
types: `explore`, read-only investigation on the `smol` role, and `general`, full
coding on the default role. It returns a durable job handle immediately rather
than waiting for the child's answer. Children have their own durable journals,
can't delegate again, and inherit only selected built-ins. Their launch-time
permission ceiling intersects the parent's live policy at every call. Child
approval requests route to the root prompter with allow-once/reject-once only.

A general child in an eligible git repository gets a detached worktree. A clean
tree pins HEAD; a dirty tree pins a snapshot of tracked, staged and nonignored
untracked content using a temporary private index. Parent files and index aren't
changed. The snapshot stays reachable through `refs/tempest/jobs/<jobID>` until
cleanup. Non-git, unborn, bare, sparse or submodule workspaces (including missing
git) run **unfenced in the parent cwd**, with `isolation:"shared"` and at most one
active shared general child per session. Explore remains read-only in the live cwd.
Worktrees fence cooperative writes, not arbitrary shell commands or hostile code.

Children export binary-capable patches against their pinned base; they never
merge into the parent. Inspect the full job through `WaitJobs` or `tempest/wait`
and explicitly apply its patch through ordinary gated bash/edit. Dirty worktrees
are retained; `CleanupJob`, `tempest/cleanup_job`, or `jobs` with `action:"cleanup"`
remove only owned, safely exported trees. Missing/corrupt patches, unique ignored
files, or an uncertain surviving crash process block cleanup. Shared jobs have no
patch and no worktree cleanup.

### 09 · Reads that summarize, not dump

Point `read` at a source file and it returns a structural outline — declarations
without bodies — so the agent orients in a large file without spending the context
window on lines it doesn't need yet.

## Every tool, in one namespace

Seventeen built-in tools share one namespace, **12 of them on by default**; pin the
active set with `-exclude-tools`, and gate the extras behind flags (see Flags below).
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

- `bash` — workspace shell with a destructive-command guard and a default ten-minute timeout; `async:true` starts a supervised background job.
- `jobs` — page durable job summaries, or explicitly clean one exported worktree.
- `wait` — wait for any requested job, without cancelling it on wait timeout.
- `cancel` — cancel one task or background bash job.
- `logs` — page a background command's bounded stdout/stderr by byte offset.

**Coordination**

- `task` — admit asynchronous children and return job handles *(behind `-enable-task`)*.
- `todo_write` — ordered task list with phase tracking.

**Context**

- `skill` — load a skill body on demand when skills are loaded (also via `skill://`).
- `attachment` — read resolved session attachments *(behind `-attach`)*.

**Web** *(gated behind `-enable-web`)*

- `web_search` — one query against the configured search endpoint.
- `web_fetch` — fetch a URL as bounded, structured text.

## Runs

A run is one `-p` prompt. The model calls tools, sees their results, and keeps going.
A reply with no tool calls reaches quiescence: the run waits for owned background
jobs and consumes their result follow-ups before completing. A result follow-up is a
short host notice in the transcript: job id, kind, state, exit code or reason, a
bounded output/result preview, artifact paths, and the `wait`/`logs` calls that
return the full details. No detached next run is started automatically. Every run
ends with a stop reason:

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
  Finishing naturally on call N still reports `completed`; the cap stops a run
  only when another turn or outstanding work remains.
- `-max-cost USD` caps the summed cost of the run's model calls. When the
  model's price is unknown the run is refused at startup: an unknown price is
  not a zero price.
- `-max-wall-time D` caps elapsed time, as a Go duration such as `10m`.

Background bash and the four management tools ship with ordinary bash; task
remains opt-in. Cancellation, `Session.Close`, CLI signals and server shutdown
cancel and join owned jobs before the root terminal event. This isn't a detached
process service: no background command is intentionally kept alive after clean
shutdown. SIGKILL can leave OS descendants running; recovery reports their outcome
as unknown, never reruns the command and never signals a stale PID.

`-output-format json` prints a run report:

```json
{"session_id": "d6b6fcd56cee7675", "stop_reason": "max_turns", "turns": 3, "usage": {"input": 5120, "output": 410, "...": "..."},
 "cost_usd": 0.0213, "elapsed_ms": 18450, "text": "last assistant text", "message": {"role": "toolResult", "...": "..."}}
```

`cost_usd` is `null` when any call's price was unknown, and `error` appears on
`error` stops. `message` is the transcript's last message. In text mode the last
assistant text goes to stdout, and a stop reason other than `completed` goes to
stderr. Text mode ends with `session: <id> (resume with -resume <id>)` on stderr.

The three output modes are `text`, `json` (a pretty-printed report), and
`stream-json` (one compact JSON object per line). `-print` remains a legacy
alias; `-output-format` wins when both are supplied.

```sh
./bin/tempest -p "Explain this package" -output-format stream-json
```

Stream event lines are the existing Tempest Update envelope:
`{"seq":1,"at":"...","sessionId":"...","update":{...}}`. The last line is
the existing report above, compact, identified by `stop_reason`, with no
extra wrapper or sequence number. Resuming streams only the new run, not
old conversation history. A slow consumer never blocks the agent: if the
256-update queue overflows, stdout contains the actual `tempest/gap`
marker and the final report, stderr explains the incomplete stream, and
the command exits 1. A broken pipe cancels and joins the run.

Streams include the same `tempest/queue`, `tempest/job`, `tempest/job_output` and
`tempest/compaction` updates as ACP/Go watchers. Output events only announce byte
availability; read command logs separately. Child transcripts/thinking aren't
forwarded into the parent stream, and the final run-report shape is unchanged.

## MCP tools

Select trusted MCP servers explicitly with `-mcp-config PATH` on one-shot,
`serve`, or `mcp`; `HARNESS_MCP_CONFIG` and the `mcp_config` settings key
are also supported (flags win). An explicit empty path disables MCP.
Settings may instead supply `mcp_servers`; a selected file replaces that
array completely. Repository `.mcp.json` files are never auto-loaded.

A file holds one `mcpServers` key. It may be an object keyed by server name, the
Claude Desktop/Cursor shape, or Tempest's array of named servers:

```json
{
  "mcpServers": {
    "everything": {"command": "npx", "args": ["-y", "@modelcontextprotocol/server-everything@2025.11.25"], "env": {"DEBUG": "1"}},
    "docs": {"url": "https://docs.example/mcp", "headers": {"Authorization": "Bearer ..."}, "permission": "deny"},
    "off": {"command": "broken-mcp", "disabled": true}
  }
}
```

```json
{
  "mcpServers": [
    {"name":"issues","transport":"stdio","command":"issue-mcp","args":["--stdio"],"permission":"prompt"},
    {"name":"docs","transport":"http","url":"https://docs.example/mcp","permission":"deny"}
  ]
}
```

`transport` (or `type`) is `stdio` or `http`; when omitted, `command` means stdio and
`url` means Streamable HTTP. `"disabled": true` skips an entry. Other fields are
`startupTimeout` (default `30s`), `callTimeout` (default `2m`) and `permission`;
unknown fields are errors that list the supported ones. Every configured server
must start: one that fails aborts startup with an error naming the server, so fix
it, disable it, or pass `-mcp-config ""` to run without MCP.

Both stdio and Streamable HTTP clients support MCP `2025-11-25`
(preferred) and `2025-06-18`. The handshake-free `2026-07-28` era, legacy
HTTP+SSE, OAuth, sampling, and resource browsing aren't supported.
Discovered tools use names such as `mcp__issues__lookup`. They default to
prompt/exec-tier regardless of read-only annotations; without an approver,
headless runs **deny** them. Opt into one exact tool with
`-allow-tool mcp__issues__lookup`, or trust all current and future tools of
a locally configured server with `"permission":"allow"`. `-deny-tool`
always wins. Server startup itself executes arbitrary code before tool
approval: configuration and credentials must come from a trusted operator.
Environment/header values aren't interpolated or stored in the journal;
resume must resupply runtime MCP configuration. Unknown-outcome MCP calls
are never automatically replayed.

## MCP server

```sh
./bin/tempest mcp -model anthropic:claude-opus-4-8 -max-turns 10
```

`tempest mcp` is a **stdio-only** MCP server, distinct from ACP `serve`;
there's no MCP HTTP endpoint. Initialize with `2025-11-25` or `2025-06-18`,
then send `notifications/initialized`. Its one tool, `tempest_run`, accepts
`prompt`, optional absolute `cwd`, `sessionId`, `model`, and a `budget`
(`maxTurns`, `maxCostUsd`, `maxWallSeconds`). Per-call budgets may tighten
but not relax host caps. Results include final text, structured run/session
ids, usage, stop reason and nullable cost. Reuse `sessionId` for continuity;
busy sessions fail immediately instead of queuing.

Clients may request tokened progress and cancel by the original JSON-RPC
request id. A cancelled call's reply is suppressed; its journal can still
resume by session id. With client form-elicitation support, host tool
prompts become an allow-once boolean form; absent support, decline,
malformed answers and timeout deny. There are no always approvals over
MCP. Idle sessions close after 30m. EOF or signals cancel/join accepted
runs; stdout contains protocol frames only.


## Serve

`tempest serve` exposes the same durable sessions over JSON-RPC 2.0 with the
Agent Client Protocol (ACP) method set. No listener flag means NDJSON over stdio:

```sh
./bin/tempest serve -model anthropic:claude-opus-4-8
```

Each line is one JSON object. Stdout is protocol-only; diagnostics go to stderr
or `-log-file`. For WebSocket clients, set a listener:

```sh
TEMPEST_TOKEN=change-me ./bin/tempest serve -listen 127.0.0.1:7777
```

Once bound, it prints one stderr line such as
`tempest serve listening on http://127.0.0.1:7777 (ACP /acp, A2A /a2a)`;
with `-listen 127.0.0.1:0` the line shows the port the OS picked.
Connect to `ws://127.0.0.1:7777/acp` with `Authorization: Bearer change-me`
or `?token=change-me`. `-token` overrides `TEMPEST_TOKEN`. A token is required
off loopback; when one is configured, loopback connections need it too.
`GET /healthz` is unauthenticated and reports session and running counts.
WebSocket mode doesn't also serve stdio.

HTTP mode also mounts the A2A **1.0 JSON-RPC** endpoint `POST /a2a` and
public discovery at `GET /.well-known/agent-card.json`. Use
`A2A-Version: 1.0`, `Content-Type: application/json`, and the same bearer
token as ACP; new A2A routes don't accept query tokens. `SendMessage`,
`SendStreamingMessage`, `GetTask`, `ListTasks`, `CancelTask`, and
`SubscribeToTask` operate on runs in the server's fixed startup workspace.
Set `-a2a-base-url https://agent.example` behind a proxy or for a
non-loopback listener; numeric loopback derives the actual bound address.
A2A 0.3 method names (`message/send`, `message/stream`, `tasks/get`,
`tasks/cancel`, ...) fail with `-32601` and a message naming the 1.0 method; a
missing or unsupported `A2A-Version` fails with `-32009` and asks for
`A2A-Version: 1.0`.

Pending host permissions appear as `TASK_STATE_INPUT_REQUIRED`.
**A2A cannot approve them**: text saying “yes” isn't permission. Answer
through authenticated ACP `/acp`: `tempest/watch` with `approver:true`,
then `tempest/permission` with the session id, tool-call id, and
`optionId:"allow_once"`; otherwise cancel the task or let the host timeout
deny. `GetTask` can reconstruct an exact historical run from journals after restart,
without starting providers or resuming work. Interrupted runs are FAILED with
`interrupted:true,recoverable:true`, not falsely WORKING. Reconstructed history is
bounded and marked `historyReconstructed:true`; `historyLength:0` omits it.
Lookup is limited to the configured workspace, 10,000 files, 256 MiB and two seconds;
limits and a foreign live owner return explicit unsupported-operation errors.
`ListTasks`, cursors, cancellation and subscription handles remain process-local
with bounded retention; they don't list or control all durable history. Resume
interrupted work through ACP/Go Continue with a new run/task id. No push
notifications, task follow-up, uploaded-file input, or file-change artifacts are
implemented.

Server flags include `-session-dir`, `-listen`, `-token`, `-idle-timeout`
(30m), `-permission-timeout` (5m), and `-shutdown-timeout` (10s). Model,
permission, budget, and bash-timeout flags supply defaults for new sessions.
`serve` and `mcp` also take `-enable-task`, `-enable-web`, and repeatable
`-skill` with the same meaning as in one-shot mode; each flag overrides its
`HARNESS_*` variable. Tools run on the server host, not the client.

SIGINT or SIGTERM stops admission, cancels runs, and flushes journals before
exit, even if a stdio client keeps stdin open. A second signal exits with
code 130; `-shutdown-timeout` bounds the graceful wait.

### Protocol

Initialize first, create or load a session, then prompt. A `session/prompt`
response waits for the run to finish; `session/update` and `tempest/*`
notifications carry progress while it runs. ACP reports `end_turn`,
`cancelled`, or `max_turn_requests`; `_meta.tempest.stopReason` preserves the
exact Tempest reason.

`session/new`, `session/load` and `session/resume` take an absolute `cwd` that
must be an existing directory; anything else fails with `-32602` before a
session opens. Every tool call starts with a `tool_call` update; its permission
decision (`_meta.tempest.permission`), progress and result follow as
`tool_call_update`s with the same `toolCallId`.

`tempest/watch` returns an atomic snapshot followed by live updates.
Reconnect with `session/resume` and watch again: an accepted prompt keeps
running after its connection closes. A slow watcher is dropped with
`tempest/gap`, never allowed to stall the run; re-watch for a fresh snapshot.
Use `tempest/history` to page older updates. Frames are limited to 1 MiB;
an oversized history page fails with `-32000`, so request a smaller page.
After compaction, late-attach history shows the compacted view, not the full
journal transcript. The snapshot's `budget` and the `tempest/set_budget`
result use the request shape `{maxTurns, maxCostUsd, maxWallSeconds}`; builds
before this one sent Go field names (`MaxTurns`, `MaxCostUSD`, and
`MaxWallTime` in nanoseconds).

Permissions arrive as `session/request_permission` requests with
`allow_once`, `allow_always`, `reject_once`, and `reject_always` choices.
The first answer wins. Always-choices last only for this session in this
server process. Destructive-command guard asks treat Always as Once: they
ask again for each guarded command, regardless of earlier session choices.
Unanswered asks deny after the permission timeout. Reconnecting approvers
can watch with `approver:true` and answer pending asks with
`tempest/permission`. `session/cancel` aborts the run.

`tempest/steer` and `tempest/follow_up` accept `{sessionId,text,key?}` and return a
durable `{item}` receipt. Steer waits for a safe checkpoint; follow-up waits for
quiescence. Idle input stays pending until explicit Prompt/Continue. Accepted
input **survives parent cancellation**; only `tempest/cancel_queued` with `queueId`
withdraws it. Same-key retries recover the receipt, including after consumption;
different text/kind conflicts. User input is text-only (16 KiB each, 32 pending).
Snapshots include pending queues, the latest 20 job summaries and all active
jobs (at most 28 total). If `jobsTruncated` is true, start `tempest/jobs` at the
empty `jobsCursor`, page with returned `nextCursor`/`limit`, and deduplicate by
job id; recent/active snapshots aren't a contiguous id page. Use `tempest/wait`
with `jobIds` for full details, `tempest/cancel_job`, `tempest/logs` or
`tempest/cleanup_job` for management. Cancelling or disconnecting a wait stops
only the wait, not the jobs.

Here's a short stdio exchange. `s1` stands for the returned session ID;
responses show selected fields, and the watch snapshot and non-text
notifications are omitted:

```json
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{},"clientInfo":{"name":"example","version":"1"}}}
{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}
{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/workspace","mcpServers":[]}}
{"jsonrpc":"2.0","id":2,"result":{"sessionId":"s1"}}
{"jsonrpc":"2.0","id":3,"method":"tempest/watch","params":{"sessionId":"s1"}}
{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"s1","prompt":[{"type":"text","text":"Say hello"}]}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","messageId":"msg_2","content":{"type":"text","text":"Hello"},"_meta":{"tempest":{"seq":5}}}}}
{"jsonrpc":"2.0","id":4,"result":{"stopReason":"end_turn","_meta":{"tempest":{"stopReason":"completed","turns":1}}}}
```

`session/new` doesn't subscribe automatically; wait for the `tempest/watch`
response before prompting. `session/load` replays history and subscribes;
`session/resume` opens without replay or subscription.

## Embed in Go

Import `go.harness.dev/harness/tempest` to drive the harness in-process:

```go
session, err := tempest.Open(ctx, tempest.Options{
    Cwd: cwd,
    New: &tempest.NewSession{Model: "anthropic:claude-opus-4-8"},
})
if err != nil {
    return err
}
defer session.Close(context.Background())

snapshot, updates, stopWatch := session.Watch(ctx, tempest.WatchOptions{})
defer stopWatch()
go func() {
    for update := range updates {
        fmt.Println(update.Kind, string(update.Update))
    }
}()
fmt.Println("session:", snapshot.SessionID)
result, err := session.Prompt(ctx, tempest.Prompt{Text: "Explain this project"})
if err != nil {
    return err
}
fmt.Println("stop reason:", result.StopReason)
```

`Prompt` blocks on its caller's goroutine. `Cancel` is safe from another
goroutine; `Close` cancels, joins the run, and closes its journal.
[The complete example](examples/embed/main.go) uses the faux provider by
default, so it needs no API key. Pass `-model` to use a real provider:

```sh
go -C examples/embed run .
ANTHROPIC_API_KEY=... go -C examples/embed run . -model anthropic:claude-opus-4-8
```

The example is a separate module with a local `replace`; build it with
`go -C examples/embed build ./...` or `make build-examples`.

## Flags

| Flag | Purpose |
|---|---|
| `-max-turns <n>`, `-max-cost <usd>`, `-max-wall-time <duration>` | Per-run budgets; see [Runs](#runs). Zero means unlimited. |
| `-bash-timeout <duration>` | Default shell-command timeout (10m); `HARNESS_BASH_TIMEOUT` or settings `bash_timeout` can set it too. A positive model-supplied `timeout` in seconds overrides it, up to 2147483 seconds. Timeouts kill the whole process group. |
| `-enable-task` | Register the `task` tool with the built-in `explore` (read-only, `smol` role) and `general` (full coding, default role) subagents. |
| `-plan` | Run the read-only plan stack on the `plan` role model. Only read/search/list tools (and enabled read-only web/attachment tools) are available; `-faux-script` still overrides the model provider. |
| `-attach <path>` | Attach a local file to the session; repeatable. Missing paths are startup errors. |
| `-enable-web` | Enable the gated `web_search` and `web_fetch` tools. |
| `-skill <path>` | Additional skill location; repeatable. |
| `-permission-mode <mode>` | Tool gate mode: `default`, `plan`, `acceptEdits` or `bypass`. |
| `-allow-tool <name>`, `-deny-tool <name>` | Always allow or deny one tool; repeatable, deny wins. A name that is neither a built-in tool nor `mcp__*` gets a stderr warning. |

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
| `HARNESS_BASH_TIMEOUT` | Default bash timeout as a Go duration, for example `1s` or `10m`. |
| `HARNESS_RETRY_*` | Retry policy: `ENABLED`, `MAX_ATTEMPTS`, `BASE_DELAY_MS`, `BACKOFF_CAP_MS`, `MAX_DELAY_MS`, `JITTER_MIN`, `JITTER_MAX`. |
| `HARNESS_WEB_SEARCH_URL` | Web-search endpoint when web tools are enabled. |
| `HARNESS_LOG_LEVEL`, `HARNESS_DEBUG`, `HARNESS_LOG_FILE`, `HARNESS_LOG_FILE_MAX_BYTES` | Logging controls. |

Run `./bin/tempest -help` for the full flag list and the `serve`, `mcp`, `doctor`
and `version` subcommands.

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
