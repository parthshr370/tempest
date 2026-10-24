# Servers and protocols

Tempest can call MCP tools, act as an MCP server, and serve durable sessions over
ACP and A2A. All of these share the same journals, permission gate and budgets as
the CLI.

## MCP client tools

Select trusted MCP servers explicitly with `-mcp-config PATH` on one-shot runs,
`serve`, or `mcp`. `HARNESS_MCP_CONFIG` and the `mcp_config` settings key are also
supported; flags win. An explicit empty path disables MCP. Settings may instead
supply `mcp_servers`; a selected file replaces that array completely. Repository
`.mcp.json` files are never auto-loaded.

A file holds one `mcpServers` key. It may be an object keyed by server name (the
Claude Desktop and Cursor shape), or Tempest's array of named servers:

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

| Field | Meaning |
|---|---|
| `transport` (or `type`) | `stdio` or `http`. When omitted, `command` means stdio and `url` means Streamable HTTP. |
| `disabled` | `true` skips an entry. |
| `startupTimeout` | Default `30s`. |
| `callTimeout` | Default `2m`. |
| `permission` | Tool permission for the server. |

Unknown fields are errors that list the supported ones. Every configured server
must start: one that fails aborts startup with an error naming the server. Fix it,
disable it, or pass `-mcp-config ""` to run without MCP.

Both stdio and Streamable HTTP clients support MCP `2025-11-25` (preferred) and
`2025-06-18`. The handshake-free `2026-07-28` era, legacy HTTP+SSE, OAuth, sampling,
and resource browsing are not supported.

Discovered tools use names such as `mcp__issues__lookup`. They default to
prompt/exec-tier regardless of read-only annotations; without an approver, headless
runs **deny** them. Opt into one exact tool with `-allow-tool mcp__issues__lookup`,
or trust all current and future tools of a locally configured server with
`"permission":"allow"`. `-deny-tool` always wins.

Server startup itself executes arbitrary code before tool approval: configuration
and credentials must come from a trusted operator. Environment and header values are
not interpolated or stored in the journal; resume must resupply runtime MCP
configuration. Unknown-outcome MCP calls are never automatically replayed.

## MCP server

```sh
./bin/tempest mcp -model anthropic:claude-opus-4-8 -max-turns 10
```

`tempest mcp` is a **stdio-only** MCP server, distinct from ACP `serve`. There is no
MCP HTTP endpoint. Initialize with `2025-11-25` or `2025-06-18`, then send
`notifications/initialized`.

Its one tool, `tempest_run`, accepts `prompt`, optional absolute `cwd`, `sessionId`,
`model`, and a `budget` (`maxTurns`, `maxCostUsd`, `maxWallSeconds`). A caller's
`model` cannot override an endpoint with `@<base-url>`; endpoints come from the
server's provider configuration. Per-call budgets may tighten but not relax host
caps. Results include final text, structured run and session ids, usage, stop reason
and nullable cost. Reuse `sessionId` for continuity; busy sessions fail immediately
instead of queuing.

Clients may request tokened progress and cancel by the original JSON-RPC request id.
A cancelled call's reply is suppressed; its journal can still resume by session id.
With client form-elicitation support, host tool prompts become an allow-once boolean
form. Absent support, decline, malformed answers and timeout deny. There are no
always approvals over MCP. Idle sessions close after 30m. EOF or signals cancel and
join accepted runs; stdout contains protocol frames only.

## Serve

`tempest serve` exposes the same durable sessions over JSON-RPC 2.0 with the Agent
Client Protocol (ACP) method set. No listener flag means NDJSON over stdio:

```sh
./bin/tempest serve -model anthropic:claude-opus-4-8
```

Each line is one JSON object. Stdout is protocol-only; diagnostics go to stderr or
`-log-file`. For WebSocket clients, set a listener:

```sh
TEMPEST_TOKEN=change-me ./bin/tempest serve -listen 127.0.0.1:7777
```

Once bound, it prints one stderr line such as
`tempest serve listening on http://127.0.0.1:7777 (ACP /acp, A2A /a2a)`. With
`-listen 127.0.0.1:0` the line shows the port the OS picked.

Connect to `ws://127.0.0.1:7777/acp` with `Authorization: Bearer change-me` or
`?token=change-me`. `-token` overrides `TEMPEST_TOKEN`. A token is required off
loopback; when one is configured, loopback connections need it too.
`GET /healthz` is unauthenticated and reports session and running counts. WebSocket
mode does not also serve stdio.

### A2A 1.0

HTTP mode also mounts the A2A **1.0 JSON-RPC** endpoint `POST /a2a` and public
discovery at `GET /.well-known/agent-card.json`. Use `A2A-Version: 1.0`,
`Content-Type: application/json`, and the same bearer token as ACP; new A2A routes do
not accept query tokens.

`SendMessage`, `SendStreamingMessage`, `GetTask`, `ListTasks`, `CancelTask`, and
`SubscribeToTask` operate on runs in the server's fixed startup workspace. Set
`-a2a-base-url https://agent.example` behind a proxy or for a non-loopback listener;
numeric loopback derives the actual bound address.

A2A 0.3 method names (`message/send`, `message/stream`, `tasks/get`, `tasks/cancel`,
and so on) fail with `-32601` and a message naming the 1.0 method. A missing or
unsupported `A2A-Version` fails with `-32009` and asks for `A2A-Version: 1.0`.

Pending host permissions appear as `TASK_STATE_INPUT_REQUIRED`. **A2A cannot approve
them**: text saying "yes" is not permission. Answer through authenticated ACP `/acp`:
`tempest/watch` with `approver:true`, then `tempest/permission` with the session id,
tool-call id, and `optionId:"allow_once"`. Otherwise cancel the task or let the host
timeout deny.

`GetTask` can reconstruct an exact historical run from journals after restart,
without starting providers or resuming work. Interrupted runs are FAILED with
`interrupted:true,recoverable:true`, not falsely WORKING. Reconstructed history is
bounded and marked `historyReconstructed:true`; `historyLength:0` omits it. Lookup is
limited to the configured workspace, 10,000 files, 256 MiB and two seconds. Limits
and a foreign live owner return explicit unsupported-operation errors.

`ListTasks`, cursors, cancellation and subscription handles remain process-local
with bounded retention; they do not list or control all durable history. Resume
interrupted work through ACP or Go Continue with a new run or task id. No push
notifications, task follow-up, uploaded-file input, or file-change artifacts are
implemented.

### Server flags and shutdown

Server flags include `-session-dir`, `-listen`, `-token`, `-idle-timeout` (30m),
`-permission-timeout` (5m), and `-shutdown-timeout` (10s). Model, permission,
budget, and bash-timeout flags supply defaults for new sessions.

`serve` and `mcp` also take `-enable-task`, `-enable-web`, `-enable-lsp`,
`-lsp-config`, `-cloud-policy` and repeatable `-skill` with the same meaning as in
one-shot mode. They read the same `providers` registry as the CLI. Each flag
overrides its `HARNESS_*` variable. Protocol clients cannot choose language-server
commands or cloud grants. Tools and language servers run on the server host, not
the client.

SIGINT or SIGTERM stops admission, cancels runs, and flushes journals before exit,
even if a stdio client keeps stdin open. A second signal exits with code 130;
`-shutdown-timeout` bounds the graceful wait.

## ACP protocol

Initialize first, create or load a session, then prompt. A `session/prompt` response
waits for the run to finish; `session/update` and `tempest/*` notifications carry
progress while it runs. ACP reports `end_turn`, `cancelled`, or
`max_turn_requests`; `_meta.tempest.stopReason` preserves the exact Tempest reason.

`session/new`, `session/load` and `session/resume` take an absolute `cwd` that must
be an existing directory. Anything else fails with `-32602` before a session opens.
Every tool call starts with a `tool_call` update; its permission decision
(`_meta.tempest.permission`), progress and result follow as `tool_call_update`s with
the same `toolCallId`.

### Watching and history

`tempest/watch` returns an atomic snapshot followed by live updates. Reconnect with
`session/resume` and watch again: an accepted prompt keeps running after its
connection closes. A slow watcher is dropped with `tempest/gap`, never allowed to
stall the run; re-watch for a fresh snapshot. Use `tempest/history` to page older
updates.

Frames are limited to 1 MiB; an oversized history page fails with `-32000`, so
request a smaller page. After compaction, late-attach history shows the compacted
view, not the full journal transcript.

The snapshot's `budget` and the `tempest/set_budget` result use the request shape
`{maxTurns, maxCostUsd, maxWallSeconds}`. Builds before this one sent Go field names
(`MaxTurns`, `MaxCostUSD`, and `MaxWallTime` in nanoseconds).

### Permissions

Permissions arrive as `session/request_permission` requests with `allow_once`,
`allow_always`, `reject_once`, and `reject_always` choices. The first answer wins.
Always-choices last only for this session in this server process.

Destructive-command guard asks treat Always as Once: they ask again for each guarded
command, regardless of earlier session choices. Unanswered asks deny after the
permission timeout. Reconnecting approvers can watch with `approver:true` and answer
pending asks with `tempest/permission`. `session/cancel` aborts the run.

### Continue, steer and follow-up

`tempest/continue` takes `{sessionId}` and starts a run without adding a user
message; its response and errors match `session/prompt`.

After a host crash, `session/load` or `session/resume` the session and call it to
finish the interrupted run. Tool calls that never recorded a result come back to the
model as "outcome unknown" and are not re-run. When the transcript already ends with
an assistant reply, continue instead delivers pending `tempest/steer` and
`tempest/follow_up` input or recovered background-job results. With none, it fails
with `-32603` (`Cannot continue without pending input`). Unlike `session/prompt`, it
is admitted while interrupted background jobs still need recovery (`-32021`
`recovery_required`).

`tempest/steer` and `tempest/follow_up` accept `{sessionId,text,key?}` and return a
durable `{item}` receipt. Steer waits for a safe checkpoint; follow-up waits for
quiescence. Idle input stays pending until explicit Prompt or Continue. Accepted
input **survives parent cancellation**; only `tempest/cancel_queued` with `queueId`
withdraws it. Same-key retries recover the receipt, including after consumption;
different text or kind conflicts. User input is text-only (16 KiB each, 32 pending).

### Jobs and queues

Snapshots include pending queues, the latest 20 job summaries and all active jobs
(at most 28 total). If `jobsTruncated` is true, start `tempest/jobs` at the empty
`jobsCursor`, page with returned `nextCursor` and `limit`, and deduplicate by job id.
Recent and active snapshots are not a contiguous id page. Use `tempest/wait` with
`jobIds` for full details, `tempest/cancel_job`, `tempest/logs` or
`tempest/cleanup_job` for management. Cancelling or disconnecting a wait stops only
the wait, not the jobs.

### Example exchange

Here is a short stdio exchange. `s1` stands for the returned session ID; responses
show selected fields, and the watch snapshot and non-text notifications are omitted:

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

`session/new` does not subscribe automatically; wait for the `tempest/watch`
response before prompting. `session/load` replays history and subscribes;
`session/resume` opens without replay or subscription.

[Guide index](README.md) · [Tempest](../README.md)
