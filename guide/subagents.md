# Subagents and background jobs

`-enable-task` adds asynchronous children, agent-to-agent messages, and worktree
integration. Background bash jobs, services, stdin and pseudo-terminals are part of
the ordinary `bash` tool.

## Task children

`-enable-task` registers the asynchronous `task` tool with two built-in subagent
types:

| Type | Role | Behavior |
|---|---|---|
| `explore` | `smol` | Read-only investigation. |
| `general` | default | Full coding. |

`task` returns a durable job handle immediately rather than waiting for the child's
answer. Children have their own durable journals, cannot delegate again, and inherit
only selected built-ins. Their launch-time permission ceiling intersects the
parent's live policy at every call. Child approval requests route to the root
prompter with allow-once and reject-once only.

Selected extension bundles can add versioned named agent presets. Pass
`<bundle>/<preset>` as `subagent_type`. The host resolves the stored preset ID
and version, applies its instructions and model, and intersects its allowed tools
with the same child ceiling. `explore` and `general` remain built-in presets.
Preset definitions are data, not executable project configuration. See
[extensions](extensions.md).

### Agent files

An agent type can be a markdown file. At startup tempest reads, in order:

1. `.harness/agents/*.md` at the repository root
2. `.claude/agents/*.md` at the repository root (Claude Code's subagent format, limited to the keys below)
3. `<agent-dir>/agents/*.md`

The repository root is the nearest directory at or above the working directory
that holds `.git`; outside git it is the working directory. A session started in
a subdirectory loads the same repository agents as one started at the root.

The first definition of a name wins. A later file with the same name is skipped
with a startup warning that names both files. An invalid file is skipped with a
warning that names the file and the field at fault; the other files still load.
Files are read once per session, so an edit takes effect in the next session.

Loading is bounded and stays inside the repository. Only regular files load:
a FIFO or device is skipped without being opened. A repository agent file, or
an agent directory, that is a symlink pointing outside the repository is
skipped with a warning; a symlink to another file inside the repository loads.
Your own `<agent-dir>/agents` may link anywhere. A file over 64 KiB is skipped
unread, and at most 64 agent files load; later files are named in a warning.
A directory with more than 1024 entries is skipped.

```markdown
---
name: auditor
description: Read-only security review of the files it is given
tools: [read, grep]
model: smol
reasoning: high
---
Review only the files named in the task. Report each finding with its path and line.
```

| Key | Required | Meaning |
|---|---|---|
| `name` | yes | Lowercase letters, digits and single hyphens. `explore` and `general` are reserved. |
| `description` | yes | Shown to the model in the subagent listing. |
| `tools` | no | Allowlist, as a YAML list or a comma-separated string. Harness tool names, the Claude Code names `Read`, `Write`, `Edit`, `MultiEdit`, `Grep`, `Glob`, `LS`, `Bash`, `WebFetch`, `WebSearch`, `TodoWrite`, `Task`, or `mcp__*` names. Absent means the general child's tools; `[]` means none. |
| `model` | no | `default`, `plan`, `smol`, or a model spec such as `openai-responses:gpt-6-luna`. Claude Code's `inherit`, `sonnet` and `opus` mean `default`; `haiku` means `smol`. A spec with an `@http(s)` endpoint is rejected: define the route under settings `providers`. |
| `reasoning` | no | `off`, `minimal`, `low`, `medium` or `high`. |
| `output_schema` | no | JSON Schema for the child's final answer, as a YAML mapping or a JSON string. An empty string (`""`) is invalid; a key with no value, or `null`, means no schema. Used when the `task` call passes no `output_schema`. |

The body below the header is the agent's instructions; it is required.

Claude Code compatibility covers the file format, the body and the keys in the
table: `name`, `description`, `tools` with Claude Code tool names, and `model`
with Claude Code aliases. Every other key is ignored without a warning,
including Claude Code's `disallowedTools`, `permissionMode`, `effort`,
`maxTurns`, `skills`, `hooks` and `mcpServers`. A Claude Code agent that
restricts itself with one of those loses the restriction here; restrict it with
`tools`. Only `*.md` files directly in each directory load, and
`~/.claude/agents` is not read.

With `task` active, the `task` tool accepts each file agent's bare name as
`subagent_type`, and the system prompt lists the agents once, beside `explore`
and `general`. The listing is fixed for the session, so it does not change the
cached prompt prefix between turns.

An agent file is prompt text. It can narrow what a child may do, never widen
it:

- A file agent runs as a `general` child. Its tools are the intersection of its
  `tools` list, the general child's built-in tools and the parent's active tools,
  and every call still passes the child's permission ceiling. Children get no MCP
  tools, so an `mcp__*` entry offers nothing today.
- `default` (or no `model`) runs on the parent's default model. `smol` uses the
  smol model, its backup models and `smol_reasoning`; `plan` uses the plan
  model, with its own endpoint and backup models (the plan role's backups even
  when `plan_model` is unset), and `plan_reasoning`, or the default model when
  the host sets no plan model. A model spec is routed through the configured
  providers like a role model, behind the default role's backup models, and
  each request carries the credential of the model's own provider and
  endpoint, never the key the host gave for another route. `reasoning`
  overrides the role's effort.
- A task records the agent's version and a SHA-256 digest of its whole
  definition. A durable task resumed after the file changed fails with `agent
  preset changed since the task started` (or `recorded agent preset version
  unavailable`) instead of running a different agent.

With extension bundles selected, file agents are listed beside the
`<bundle>/<preset>` names. A file agent whose name equals a bundle preset's name
logs a startup warning naming the file and the bundle; both stay available, the
file agent as `<name>` and the bundle's as `<bundle>/<name>`. A session in a
remote environment loads no agent files.

`tempest agents list` prints each agent's name, model and source file. It exits 1
when any file is invalid, after naming the file and field on stderr.

### Worktrees and isolation

A general child in an eligible git repository gets a **detached worktree**. A clean
tree pins HEAD. A dirty tree pins a snapshot of tracked, staged and nonignored
untracked content using a temporary private index. Parent files and index are not
changed. The snapshot stays reachable through `refs/tempest/jobs/<jobID>` until
cleanup.

Non-git, unborn, bare, sparse or submodule workspaces (including missing git) run
**unfenced in the parent cwd**, with `isolation:"shared"` and at most one active
shared general child per session. `explore` remains read-only in the live cwd.
Worktrees fence cooperative writes, not arbitrary shell commands or hostile code.

Children export binary-capable patches against their pinned base; they never merge
into the parent on their own. Inspect the full job through `WaitJobs` or
`tempest/wait`, or read the patch at the artifact path in the job's completion
notice.

### Integration

`integrate {"job_id": ...}` brings a finished child's work in with a **three-way
merge** relative to the base the child started from, so parent and child edits to
different parts of one file both survive:

1. The parent's current files (uncommitted and untracked ones included, ignored ones
   excluded) are snapshotted into a tree through a private index. The child's tree is
   rebuilt by applying its patch to its base in another private index.
   `git merge-tree --write-tree` merges the two in the object database. The user's
   index, HEAD, refs and working files are never touched, and no worktree is checked
   out. The only residue is unreferenced objects `git gc` prunes.
2. Each file the child changed is reported as `clean` (only the child changed it),
   `merged` (both changed it, without overlap) or `conflict` (overlapping edits,
   shown as numbered diff3 marker hunks labelled parent, base and child; also symlink
   or submodule changes). Nothing is written while any file conflicts. The live file
   keeps the parent's side and the model carries the child's side over with `edit` or
   `hashline`. The full conflicted file stays readable with
   `git show <merged_tree>:<path>`.
3. A textually clean merge is not proof the code still works together, so merged
   files are written only when the call passes `accept_merged:true`. Before writing,
   every target is re-hashed under the file mutation locks; a file that changed since
   the snapshot refuses the whole integration with what changed. The write goes
   through workspace containment, the mutation queue, language server change notices
   and, for more than one file, the edit rollback journal.

`integrate` needs git and a root session with the job controller. In a workspace
without git it says so and points at the patch to apply with the edit tools.

Dirty worktrees are retained. `CleanupJob`, `tempest/cleanup_job`, or `jobs` with
`action:"cleanup"` remove only owned, safely exported trees. Missing or corrupt
patches, unique ignored files, or an uncertain surviving crash process block cleanup.
Shared jobs have no patch and no worktree cleanup.

### Task flow

A general task runs in a worktree pinned to its base, or in the parent cwd when no
eligible repository exists. The child exports a patch, and `integrate` merges it
three ways. Messages travel through a durable mailbox.

```mermaid
flowchart TD
  P["parent session"] --> T["task tool"]
  T --> C["job controller"]
  C --> E{"general task in a git repo?"}
  E -->|yes| W["worktree at pinned base"]
  E -->|no| H["shared parent cwd"]
  W --> CH["child agent and journal"]
  H --> CH
  P <--> M["durable mailbox"]
  CH <--> M
  CH --> X["export changes.patch"]
  X --> I["integrate: three-way merge"]
  I --> P
```

### Admission limits

| Limit | Value |
|---|---|
| Running jobs per kind (task, bash) | 4 |
| Jobs running or holding an undelivered completion notice | 8 |
| Jobs per run | 64 |
| Retained general-task worktrees | 16 |
| Unisolated general tasks at a time | 1 |

A refused `task` or background `bash` fails with `job_limit:` followed by which
limit is full, the job ids holding it, and the call that frees one (`wait`,
`cancel`, ending the turn to receive pending notices, or `jobs` cleanup). Invalid
task or command input names the field to fix.

## Messages between agents

With `-enable-task`, the root and its running task children can talk through the
`message` tool:

```json
{"to":"parent"|"<job id>"|"all","body":"...","await_reply_ms":N}
```

A send returns as soon as the message is durably in the recipient's mailbox. It
never waits for the recipient to answer unless `await_reply_ms` (at most 300000) is
set, in which case the call also returns the recipient's first message back
(`reply`) or `reply_timed_out:true`. An awaited reply goes only to that call, never
also into the transcript as a note.

From the root, `all` is every running child. From a child it is the parent plus
running siblings, because a child coordinating a shared resource needs to reach both.

A message is a **steering item** in the recipient's own durable queue. A busy agent
reads it at its next step boundary without being interrupted, and a root idling on
its children is woken the way a completion notice wakes it. A child that is about to
finish with a message pending takes another turn instead. Once a child has finished,
sending to it fails with its state.

The recipient sees host text marked `[Message from parent]`,
`[Message from job_… (child)]` or `(sibling)`, with the body flagged as untrusted and
the `message` call to reply with. Each agent holds at most 100 unread messages;
bodies are at most 8 KiB. Messages are journaled with idempotency keys derived from
the sending tool call, so recovery delivers each exactly once. Plan mode denies
`message` like `task`.

## Background jobs

`bash` runs a plain command in the foreground and returns its output. Giving it a
`name`, unique among the session's running jobs, starts the command as a background
job instead and returns the job handle at once. `async:true` does the same for an
unnamed job, and a name or readiness field implies it, so there is no
foreground/background mismatch to reject.

A named job without a readiness condition is ordinary background work: the bash
timeout bounds it and a one-shot run waits for it to finish.

A command, foreground or job, ends when its shell exits, and so does everything
left in its process group: a child started with `&` is killed then. A foreground
`sleep 30 & echo started` returns `started` with exit 0 and a note that the child
was stopped. Start a server or watcher as a named job or service instead of with
`&`, so `jobs`, `wait` and `cancel` can see it.

### Readiness conditions

Readiness conditions make the job a **service**:

- `ready_log`: an RE2 regex matched against each output line.
- `ready_port`: a TCP port on 127.0.0.1.
- `ready_timeout_ms`: default 30000, at most 300000.

With a condition the call returns once every condition holds, with
`readiness:"ready"` and the matched line or port in `ready_match`. If the process
exits first, or the deadline passes (the process is then killed, reason
`ready_timeout`), the call fails with the output tail.

A ready service does not keep the run open. It runs until it exits, until it is
cancelled, or until the run ends, which stops it. Unless `timeout_seconds` is given it
is not bounded by the bash timeout.

`jobs` lists `name` and `readiness`. `cancel` and `logs` take `name` in place of
`job_id`, and `wait` takes `names` besides `job_ids` and waits up to `timeout_seconds`
(default 30, at most 300). A name resolves to its running
job, else the one started last. Starting a name that is already running fails with
the job holding it; restart a service with `cancel {"name":…}` and start it again.

A job call that names no job, or an id or name the session does not have, fails with
the session's jobs (id, name, kind, state, newest first) or, when there are none,
with how to start one. `ready_timeout_ms` without `ready_log` or `ready_port` is
rejected rather than ignored: it bounds a readiness wait, and to wait for a job to
finish you call `wait`.

Like the foreground command, a background job writes stdout and stderr to one pipe,
as under `2>&1`, so `logs` and `write_stdin` show them interleaved in the order the
command wrote them. A bash job's `result_text` (in `wait`, `jobs` and the completion
notice) is the end of its output, where a failing command prints its error. `logs`
pages the whole of it.

## Interactive jobs

A background job's stdin is `/dev/null`, as for any background process. A command
that reads input (a credential or host-key prompt, a y/n installer, bare `cat`) sees
end of file and fails fast instead of waiting out its timeout. Only a job started
with `stdin:true` (a pipe, which also makes it a background job) or `pty:true` takes
input. `write_stdin` with text to any other job fails and says to start it with one
of those.

### stdin

`write_stdin {"name":…, "text":…}` writes the text as given (`\n` ends a line,
`\u0003` is Ctrl-C) and returns the output the job printed since the previous
`write_stdin` on it. It waits until new output has been quiet for 0.3 s, until
`wait_ms` (default 2000, at most 30000) passes, or until the job's output ends. It
returns at most the newest 16 KiB with raw `offset`/`next_offset`; bytes it skipped
stay readable with `logs`. Empty text polls any background job. `close_stdin:true`
ends the input after the text.

### Pseudo-terminals

`pty:true` starts the job on a pseudo-terminal (stdlib ioctls on darwin and Linux, no
dependency). The process is a session leader whose controlling terminal is 24x80,
with `TERM=dumb`, `NO_COLOR=1` and `cat` as pager, so REPLs and prompting tools run
as they do for a person without painting the log. The terminal echoes input and ends
lines with `\r\n`, and nothing is stripped. Ctrl-C reaches the foreground program
through the line discipline. On a pipe job, with or without `stdin:true`, text that
is exactly `\u0003` sends SIGINT to the job's process group instead. `close_stdin`
closes a pipe job's stdin and sends Ctrl-D on a pty.

Cancellation, timeouts and output bounds are those of every background job, and the
pty is closed once the job's output is drained. A process is never reattached after a
restart: a pty job interrupted by one settles as `unknown` like any other bash job,
and `write_stdin` to it says so. Children have no background jobs, so they get
neither `pty`, `stdin` nor `write_stdin`.

### Permissions for interactive jobs

`write_stdin` is in the `exec` tier: plan mode denies it, and a `write_stdin`
override (`prompt`, `deny`) applies to every call.

The destructive guard and cloud analysis cannot see what a program does with its
input, so `default` mode allows a write the way it allows the program's own command.
The job's command already passed the gate. A shell reading commands from stdin
(`bash`, `sh -s`) is opaque there, so starting one needed per-call approval (and is
denied without an approver). Approving such a shell approves what is typed into it.

[Guide index](README.md) · [Tempest](../README.md)
