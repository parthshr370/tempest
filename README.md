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
without `@<base-url>`, the endpoint comes from `-openai-base-url`,
`HARNESS_OPENAI_BASE_URL`, settings `openai_base_url` or `OPENAI_BASE_URL`, in
that order. Use `OPENAI_API_KEY` for that route. The ID is opaque and endpoint-specific:
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
than zero-dollar components. Unknown provider prefixes are configuration errors;
bare IDs use the Anthropic route.

**Cost** comes from the provider's bill when the route reports one. OpenRouter
returns each request's billed cost in its usage (`usage.cost`); with a
bring-your-own-key upstream, the request costs OpenRouter's fee plus
`cost_details.upstream_inference_cost`. That amount, which already prices cached
and cache-write tokens, is the call's cost and counts against `-max-cost`, so an
OpenRouter model needs no catalog entry for a cost budget. Billed costs carry only
a total, no per-component split. The catalog price is the fallback when a response
reports no billed cost; with neither, the call's cost stays unknown.

**Prompt caching.** Within a session, every request extends the previous one byte
for byte: history is only rewritten by compaction, and the tool list is in name
order. On OpenAI's endpoint and on OpenRouter each request carries the session id
as `prompt_cache_key`; OpenRouter also gets it as `session_id`, its sticky-routing
key, so later turns reach the endpoint that holds the cache. Claude models on
OpenRouter get a top-level `cache_control`, without which they cache nothing.
Other OpenAI-compatible endpoints get none of these fields.

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

### 02 · Line-anchored edits that never corrupt a file

`read` and `grep` label every file with a 4-hex content tag, `[path#TAG]`, and
`LINE:TEXT` rows. The `hashline` tool edits by those line numbers instead of
retyping old text, so whitespace battles and "string not found" loops just stop:

```
[internal/demo/demo.go#1A2B]
PUT 8*:
+func Sub(a, b int) int {
+	return b - a
+}
CUT 3.=6 @add
[internal/demo/util.go#3C4D]
PUT >$ @add
```

`PUT N.=M:` replaces lines, `PUT <N:`/`PUT >N:` insert (`<1` head, `>$` tail),
`CUT` deletes and captures into a register that `PUT … @name` pastes (named
registers persist across calls, the unlabeled one lasts one call), `REM` deletes
the file and `MV DEST` renames it. `N*` targets the construct opening on line N
without counting its lines: Go through `go/parser`, Markdown sections and code
fences, brace matching for JS/TS, Java, C/C++, Rust, C#, Kotlin, Swift, PHP, Scala
and JSON, and indentation for Python and YAML; other files use explicit ranges.

Line numbers name the file as read, never shifted by earlier hunks. A section only
applies if its tag names a version this session saw whose full SHA-256 matches the
live file (a 16-bit tag collision is rejected), or, for a tag from before a
resume, if the live content hashes to it. Hunks may touch only lines a `read`,
`grep` or edit result actually displayed under that tag, may not overlap, and every
section of a patch is validated before any file is written. Each result returns
the new tag and a numbered preview of the changed lines to chain the next edit
from; a stale tag is rejected with the current one and the rows around the lines
you named. LF/CRLF endings and a UTF-8 BOM are preserved. `edit` (exact-text
replacement) accepts the same 4-hex tag as its anchor, as well as the full anchor
`lsp_rename` plans carry.

Every write is atomic: `write`, `edit` and `hashline` stage the new bytes in a
hidden file beside the target, sync it, rename it over the target and sync the
directory, so a crash leaves the old file or the new one, never a truncated mix
(the file keeps its permission bits; a hard link to it is not followed). A patch
that writes more than one path (several sections, `MV`, `REM`, or an `integrate`)
is a batch under a rollback journal, the discipline of
[SQLite's atomic commit](https://sqlite.org/atomiccommit.html): before the first
write, every path's prior existence, bytes and mode are synced to
`<journal>.d/edit-rollback/<batch>/` under an `flock`, and deleting the record is
the commit. A write that fails mid-batch rolls the written paths back before the
tool returns. A process killed mid-batch leaves the record; the next session open
in that workspace (a resume, or any new session, CLI, `serve` or `mcp`) restores
each path whose content is still the batch's pre or post state, reports a path
someone changed since as a conflict and leaves it alone, keeps the conflicted
paths' pre-batch copies under `edit-rollback/conflicts/<batch>/prior/`, and
surfaces one diagnostic per batch in the session snapshot and log. A batch whose
owner is still running holds its lock and is never touched. This is crash
recovery, not isolation: other processes can see intermediate files while a batch
runs.

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

#### Checkpoint and rewind

Before the first file-mutating call in each assistant turn, Tempest attempts a
checkpoint in a session-private Git store. Your index, HEAD, branches and stash
stay untouched. Git's private index stat cache keeps repeated captures incremental,
including in non-Git workspaces without creating `.git`. The model can also call
`checkpoint` with an investigation goal, then `rewind` with findings to retain.

```sh
tempest rewind -session <id> -list
tempest rewind -session <id> -checkpoint <checkpoint-id>
```

Rewind restores files **and** forks the conversation by appending a journal event;
the discarded branch remains auditable. Outside edits require explicit `-force`.
Settle running jobs and children first; force never bypasses this guard or a capture
gap. Capture failures never block tools or replace their results: they record a gap
that older rewinds cannot cross, including after crash/resume. Ignored files,
symlinks, nested repositories and files over 8 MiB aren't protected; skipped paths
get notes and are never overwritten/deleted by rewind. No workspace file-count or
total-byte limit refuses a large repository. Automatic and explicit checkpoints
have separate 32-entry retention rings. An omitted id selects the newest explicit
checkpoint, otherwise the newest automatic one. Only operators can force outside
edits; neither checkpoint nor rewind is exposed to the model in plan mode.

SDK controls are `CreateCheckpoint`, `Checkpoints` and `Rewind`; ACP exposes
`tempest/checkpoint`, `tempest/checkpoints` and `tempest/rewind` with `sessionId`.
See [checkpoint durability and bounds](docs/design/09-checkpoint-and-rewind.md).

#### Lifecycle hooks

Configure shell hooks in `~/.config/harness/settings.json`. The command-hook
wire format follows [Claude Code hooks](https://code.claude.com/docs/en/hooks):

```json
{
  "hooks": {
    "PreToolUse": [{
      "matcher": "^(write|edit|bash)$",
      "hooks": [{"type": "command", "command": "$CLAUDE_PROJECT_DIR/check-tool.sh", "timeout": 60}]
    }]
  }
}
```

Commands receive JSON on stdin. Exit `2` denies a tool with stderr fed back to
the model; exit `0` can return `hookSpecificOutput.permissionDecision`
(`allow`, `deny`, `ask`), `updatedInput`, or `additionalContext`. Hook allow
never bypasses plan mode, explicit policy deny/prompt rules, or approvals required by
the current mode; it never makes bypass mode stricter.
Stop hooks can return `{"decision":"block","reason":"finish the checks"}` to
continue the run. `UserPromptSubmit` stdout or JSON context reaches the model
before its first request.
Blocked prompt reasons are shown to the caller, not added to model context.
An interrupted queued prompt is visibly consumed as “prompt not submitted,”
without retrying side effects or making the session unusable.

Project hooks in `.harness/settings.json` and `.harness/settings.local.json`
**never run without explicit operator trust**: use `-trust-project-hooks` for
the current workspace or add its absolute path to user `trusted_hook_projects`.
User, trusted project, then local hooks run in file order. `-no-hooks` or user
`"no_hooks": true` disables all hooks; `tempest doctor` lists commands and skipped
untrusted sources without executing them.
Model changes to hook-bearing settings require per-call approval, including
bash file targets and child patches applied by `integrate`, even in bypass mode.

Supported events are `SessionStart`, `UserPromptSubmit`, `PreToolUse`,
`PostToolUse`, `Stop`, `SubagentStop`, `PreCompact`, `Notification`, and
`SessionEnd`, shared by CLI, ACP/serve and SDK. SessionStart/SessionEnd are root-only;
task children use SubagentStop, prompt and tool hooks. Match actual
snake_case tool names (`read`, `write`, `edit`, `bash`, `web_search`,
`manifest_diff`, `lsp_definition`, `eval`), not Claude's `Read`/`Bash` names.
Hooks run in the workspace with `TEMPEST_PROJECT_DIR` and `CLAUDE_PROJECT_DIR`,
default to 600 seconds (UserPromptSubmit: 30s; SessionEnd: 1.5s total), and have
process-group cancellation and bounded output.
Replay never runs shell hooks; interrupted effects aren't retried automatically.
See [hook semantics, trust and durability](docs/design/10-hooks.md).


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

**Cloud commands get their own intent check.** Inside the same bash gate,
`kubectl`, `helm`, `terraform` and `kustomize` invocations are classified as
read, write, destructive or opaque, without running anything or reading your
kubeconfig. Proven reads (`kubectl get`, `helm list`, `terraform show`, …) are
allowed as before. Writes (`kubectl apply`, `helm upgrade --install`,
`terraform plan`/`apply`, any `--dry-run`) **prompt per call in `default` and
`acceptEdits`, even with `-allow-tool bash`**: accepting code edits is not consent
to deploy. Destructive forms (`kubectl delete`, `helm uninstall`,
`terraform destroy`, …) need per-call guard approval. Literal wrappers and
inline shells are looked through: `timeout 30 kubectl get pods` and
`bash -c 'kubectl get pods'` are reads, `timeout 5 kubectl delete pod x` is
destructive. Opaque forms prompt too: unknown verbs or flags, `kustomize build`,
environment changes in front of a cloud tool (`KUBECONFIG=x kubectl …`), shell
syntax the classifier can't resolve (substitutions, here-documents, loops,
`xargs`, `sudo`, …) **when the command names a cloud tool anywhere**, and code it
can't see at all: a dynamic command word (`$K get pods`), `eval "$x"`,
`bash -c "$CMD"`, or `curl … | sh`. Commands that name no cloud tool keep their
previous behavior whatever syntax they use, so `go test $(go list ./...)`,
`cat <<EOF > main.go`, `timeout 30 make` and `find . | xargs grep x` run as
before. Compound commands are judged by their riskiest part, and an Always
answer never covers a later cloud call. Without an approver, all of these are
denied.

An operator can pre-approve or forbid exact kube contexts with
`-cloud-policy FILE` (`HARNESS_CLOUD_POLICY`, settings `cloud_policy`):

```json
{"rules":[
  {"tool":"kubectl","context":"kind-dev","action":"allow_write"},
  {"tool":"helm","context":"kind-dev","action":"allow_write"},
  {"tool":"kubectl","context":"production","action":"deny"}
]}
```

`allow_write` lets a non-destructive write run without a prompt only when the
command itself names that context literally (`kubectl --context kind-dev apply …`,
`helm --kube-context kind-dev upgrade …`); an ambient current context, a
`--kubeconfig`/`--server`/credential override, a dynamic value, or a call made
through a wrapper, substitution or `bash -c` never matches.
`deny` wins over everything, including `bypass`, and also blocks reads; when a
deny rule exists for a tool and the target context can't be established, that
call is denied too. Contexts are exact names, not patterns; at most 256 rules,
1 MiB file. Rules cover `kubectl` and `helm` only, and a context name is policy
within your trusted kubeconfig, not a verified cluster identity.

**Plan mode can't be overridden.** `plan` now denies every mutation before any
allow rule, remembered approval or cloud grant is consulted: `-allow-tool bash`
no longer lets a plan run write. Only commands proven read-only in every part
run in plan mode, in plain syntax: substitutions, here-documents, wrappers and
`bash -c` are refused there even when they name no cloud tool.

### 06 · Model roles, routed by intent

A `default` agent for normal turns, a read-only `plan` model (used by `-plan`), and a
small `smol` helper (the model behind the `explore` subagent of `-enable-task`) — each
routable to a different provider and model. Set them with
`-model`/`-plan-model`/`-smol-model` or the matching `HARNESS_*_MODEL` variables.

### 07 · Context that discovers itself

At startup the agent walks `AGENTS.md` up from your working directory, loads a sticky
`RULES.md` that survives compaction, expands safe `@file` imports, and registers
skills from your agent directory, the project and `-skill` paths. Skill bodies load
on demand through the `skill` tool or a `skill://` read — nothing bloats the prompt
until it's needed.

#### Skills

A skill is a markdown file with YAML frontmatter between `---` lines at the very
top; the body below it is what the `skill` tool returns:

```markdown
---
name: deploy-app
description: Deploy the app to staging. Use when asked to ship or deploy.
---
# Deploy
Run `make deploy` and report the URL it prints.
```

- `description` is required (at most 1024 bytes); the system prompt lists each
  skill's name and description so the model knows when to load it.
- `name` defaults to the name of the directory holding the file. It must be
  lowercase `a-z`, `0-9` and single hyphens, at most 64 characters, and must not
  start or end with a hyphen.
- `disable-model-invocation: true` (or `hide: true`) leaves the skill out of
  that list; `skill` and `skill://<name>` still load it by name.
- `when_to_use`, `allowed-tools`, `model`, `context` and `paths` are accepted for
  compatibility and currently have no effect; other keys are ignored.

Skills load from `<agent-dir>/skills` (by default `~/.config/harness/agent/skills`),
then `.harness/skills` in the working directory, then each `-skill` path (or the
settings `skill_paths` list when no `-skill` flag is given). A `-skill` path may
be a directory or a single `.md` file. In each directory:

- every `.md` file directly inside it is a skill;
- below that, only files named `SKILL.md` count, and a directory holding a
  `SKILL.md` is one skill: its subdirectories aren't searched;
- hidden entries, `node_modules` and paths matched by `.gitignore`, `.ignore` or
  `.fdignore` are skipped.

A file with a missing description or an invalid name is skipped with a
`discovery diagnostic` warning on stderr. When two skills share a name, the first
one loaded wins and the collision is logged. `skill://<name>/<path>` reads other
files from the skill's directory and cannot leave it.

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
merge into the parent on their own. Inspect the full job through `WaitJobs` or
`tempest/wait`, or read the patch at the artifact path in the job's completion
notice. `integrate {"job_id": ...}` brings a finished child's work in with a
three-way merge relative to the base the child started from, so parent and child
edits to different parts of one file both survive:

1. The parent's current files (uncommitted and untracked ones included, ignored
   ones excluded) are snapshotted into a tree through a private index; the child's
   tree is rebuilt by applying its patch to its base in another private index; and
   `git merge-tree --write-tree` merges the two in the object database. The user's
   index, HEAD, refs and working files are never touched, and no worktree is
   checked out; the only residue is unreferenced objects `git gc` prunes.
2. Each file the child changed is reported as `clean` (only the child changed it),
   `merged` (both changed it, without overlap) or `conflict` (overlapping edits,
   shown as numbered diff3 marker hunks labelled parent, base and child; also
   symlink or submodule changes). Nothing is written while any file conflicts; the
   live file keeps the parent's side and the model carries the child's side over
   with `edit` or `hashline`. The full conflicted file stays readable with
   `git show <merged_tree>:<path>`.
3. A textually clean merge is not proof the code still works together, so merged
   files are written only when the call passes `accept_merged:true`. Before
   writing, every target is re-hashed under the file mutation locks; a file that
   changed since the snapshot refuses the whole integration with what changed.
   The write goes through workspace containment, the mutation queue, language
   server change notices and, for more than one file, the edit rollback journal.

`integrate` needs git and a root session with the job controller; in a workspace
without git it says so and points at the patch to apply with the edit tools.
Dirty worktrees are retained; `CleanupJob`, `tempest/cleanup_job`, or `jobs` with
`action:"cleanup"` remove only owned, safely exported trees. Missing/corrupt
patches, unique ignored files, or an uncertain surviving crash process block
cleanup. Shared jobs have no patch and no worktree cleanup.

Admission limits: 4 running jobs per kind (task, bash), 8 jobs running or holding
an undelivered completion notice, 64 jobs per run, 16 retained general-task
worktrees, and one unisolated general task at a time. A refused `task` or
background `bash` fails with `job_limit:` followed by which limit is full, the
job ids holding it, and the call that frees one (`wait`, `cancel`, ending the turn
to receive pending notices, or `jobs` cleanup). Invalid task or command input
names the field to fix.

#### Messages between agents

With `-enable-task`, the root and its running task children can talk through the
`message` tool: `{"to":"parent"|"<job id>"|"all","body":"...","await_reply_ms":N}`.
A send returns as soon as the message is durably in the recipient's mailbox; it
never waits for the recipient to answer unless `await_reply_ms` (at most 300000)
is set, in which case the call also returns the recipient's first message back
(`reply`) or `reply_timed_out:true`. An awaited reply goes only to that call, never
also into the transcript as a note. From the root, `all` is every running child;
from a child it is the parent plus running siblings (a child coordinating a shared
resource needs to reach both).

A message is a steering item in the recipient's own durable queue, so a busy agent
reads it at its next step boundary without being interrupted, and a root idling on
its children is woken the way a completion notice wakes it. A child that is about
to finish with a message pending takes another turn instead; once a child has
finished, sending to it fails with its state. The recipient sees host text marked
`[Message from parent]`, `[Message from job_… (child)]` or `(sibling)`, with the
body flagged as untrusted and the `message` call to reply with. Each agent holds at
most 100 unread messages; bodies are at most 8 KiB. Messages are journaled with
idempotency keys derived from the sending tool call, so recovery delivers each
exactly once. Plan mode denies `message` like `task`.

#### Named background jobs and services

`bash` runs a plain command in the foreground and returns its output. Giving it a
`name`, unique among the session's running jobs, starts the command as a
background job instead and returns the job handle at once; `async:true` does the
same for an unnamed job, and a name or readiness field implies it, so there is no
foreground/background mismatch to reject. Readiness conditions make the job a
service: `ready_log` (an RE2 regex matched against each output line) and/or
`ready_port` (a TCP port on 127.0.0.1), with `ready_timeout_ms` (default 30000, at
most 300000). With a condition the call returns once every
condition holds, with `readiness:"ready"` and the matched line or port in
`ready_match`; if the process exits first, or the deadline passes (the process is
then killed, reason `ready_timeout`), the call fails with the output tail. A ready
service doesn't keep the run open: it runs until it exits, until it is cancelled,
or until the run ends, which stops it. Unless `timeout` is given it is not bounded
by the bash timeout. A named job without a readiness condition is ordinary
background work: the bash timeout bounds it and a one-shot run waits for it to
finish. `jobs` lists `name` and `readiness`; `cancel` and `logs` take
`name` in place of `job_id`, and `wait` takes `names` besides `job_ids`. A name
resolves to its running job, else the one started last. Starting a name that is
already running fails with the job holding it; restart a service with `cancel
{"name":…}` and start it again. A job call that names no job, or an id or name the
session doesn't have, fails with the session's jobs (id, name, kind, state, newest
first) or, when there are none, with how to start one. `ready_timeout_ms` without
`ready_log` or `ready_port` is rejected rather than ignored: it bounds a readiness
wait, and to wait for a job to finish you call `wait`.

Like the foreground command, a background job writes stdout and stderr to one
pipe, as under `2>&1`, so `logs` and `write_stdin` show them interleaved in the
order the command wrote them. A bash job's `result_text` (in `wait`, `jobs` and
the completion notice) is the end of its output, where a failing command prints
its error; `logs` pages the whole of it.

#### Interactive jobs: stdin and pseudo-terminals

A background job's stdin is `/dev/null`, as for any background process: a command
that reads input (a credential or host-key prompt, a y/n installer, bare `cat`)
sees end of file and fails fast instead of waiting out its timeout. Only a job
started with `stdin:true` (a pipe, which also makes it a background job) or
`pty:true` takes input; `write_stdin` with text to any other job fails and says to
start it with one of those. `write_stdin {"name":…, "text":…}` writes the text as
given (`\n` ends a line, `\u0003` is Ctrl-C) and returns the output the job
printed since the previous `write_stdin` on it: it waits until new output has been
quiet for 0.3 s, until `wait_ms` (default 2000, at most 30000) passes, or until the
job's output ends, and returns at most the newest 16 KiB with raw
`offset`/`next_offset` (bytes it skipped stay readable with `logs`). Empty text
polls any background job; `close_stdin:true` ends the input after the text.

`pty:true` starts the job on a pseudo-terminal (stdlib ioctls on darwin and Linux,
no dependency): the process is a session leader whose controlling terminal is
24x80, with `TERM=dumb`, `NO_COLOR=1` and `cat` as pager so REPLs and prompting
tools run as they do for a person without painting the log. The terminal echoes
input and ends lines with `\r\n`, and nothing
is stripped. Ctrl-C reaches the foreground program through the line discipline;
on a pipe job, with or without `stdin:true`, text that is exactly `\u0003` sends
SIGINT to the job's process group instead. `close_stdin` closes a pipe job's stdin
and sends Ctrl-D on a pty.
Cancellation, timeouts and output bounds are those of every background job, and the
pty is closed once the job's output is drained. A process is never reattached after
a restart: a pty job interrupted by one settles as `unknown` like any other bash
job, and `write_stdin` to it says so. Children have no background jobs, so they
get neither `pty`, `stdin` nor `write_stdin`.

`write_stdin` is in the `exec` tier: plan mode denies it, and a `write_stdin`
override (`prompt`, `deny`) applies to every call. The destructive guard and cloud
analysis can't see what a program does with its input, so `default` mode allows a
write the way it allows the program's own command: the job's command already
passed the gate, and a shell reading commands from stdin (`bash`, `sh -s`) is
opaque there, so starting one needed per-call approval (and is denied without an
approver). Approving such a shell approves what is typed into it.

### 09 · Reads that show the file, and outline it only when it won't fit

Point `read` at a file and it returns the numbered content whenever the whole file
fits one read (2000 lines and 50KB), and every row it shows is editable with
`hashline`. A Go file too large for that returns a structural outline instead —
declarations without bodies, each with its `path:START-END` range — headed by a
note saying it is an outline and how to read a range, so the agent picks the lines
it needs rather than paging blind. Any other large file returns its first page and
the offset to continue. `mode:"outline"` asks for the outline of a Go file of any
size; `mode:"raw"` always pages numbered rows.

**Several files, one call.** `paths` takes the place of `path` when the agent knows
several files it needs: `{"paths":["go.mod","internal/a.go:40-90","README.md:1-30"]}`
returns one `[path#TAG]` section per entry, in order, each recording the rows it
showed for `hashline`. The sections share one 2000-line/50KB budget; a file the
budget cuts short ends with the `path:START-END` that continues it, and a file it
leaves no room for is named, not read. A failing entry reports its error in place
of its section. `offset`/`limit` page a single `path` only. `grep` takes `paths`
the same way, with one shared match `limit`. The system prompt tells the model to
batch known reads this way and to put independent tool calls in one response (the
opening look at a task, searches, edits or writes to different files), which the
agent loop already runs concurrently.

`read` takes more than files. Every text output below starts with the
`[path#TAG]` header and accepts the same `path:START-END` / `path:START+COUNT`
line ranges and `offset`/`limit` paging:

| Path | Output |
|---|---|
| `src/` | the directory's entries, directories with a trailing `/` (500 at most) |
| `bundle.zip`, `.tar`, `.tar.gz`, `.tgz` | the archive's members with sizes (1000 at most) |
| `bundle.zip:dir/file.txt` | one member, read in memory (16 MiB cap, nothing extracted to disk); `bundle.zip:dir` lists a folder |
| `report.docx`, `.xlsx`, `.pptx` | the document's extracted text |
| `package.json?q=.scripts \| keys` | a JSON query, printed as indented JSON |

The JSON query is a small jq subset implemented in-house: `.` (identity),
`.a.b`, `."any key"` and `.["any key"]`, `.[0]` and `.[-1]`, `.[]` (array
elements or object values), `?` after any step to skip values of the wrong type
instead of failing (`.items[]?.name?`), `keys`, `length`, and `|` pipes. Object
keys print sorted. Queries combine with members (`bundle.zip:data.json?q=.a`).
PDFs and SQLite databases are refused with a pointer to `pdftotext` or
`sqlite3` through `bash`.

#### Internal URLs

`read` and `grep` resolve four schemes, so the agent can name things that do not
live in the workspace:

| URL | Resolves to |
|---|---|
| `skill://<name>` | the skill's `SKILL.md`; `skill://<name>/` lists the skill's directory and `skill://<name>/<path>` reads a file beside it |
| `job://` | the session tree's background jobs, one per line |
| `job://<job_id>` | one job's record: state, exit code, command or task, artifacts; `job://<job_id>/` lists its files |
| `job://<job_id>/<file>` | one artifact: `output.log`, `result.txt`, `changes.patch` |
| `history://` | the child sessions, with their job ids |
| `history://<id>` | a child's transcript rendered as text, by task job id or child session id |
| `local://<path>` | a private scratch directory, `<journal>.d/local/`, shared by the root session and its children |

`local://` is the one writable scheme: `write` and `edit` accept it, so an agent
hands a large payload to a child (or back to its parent) by writing
`local://plan.md` and passing the URL instead of pasting the text. Every scheme is
rooted: `..` is refused, file access goes through an `os.Root` on the scheme's
directory, and a symlink that leads outside it is refused. A child resolves
`local://`, `job://` and `history://` against its root session, so a session tree
shares one namespace and other sessions stay out of reach. `job://`, `history://`
and `local://` need a journaled session. `?`, `#` and `%` in names are
percent-encoded, and so are spaces inside `bash` commands (`local://my%20notes.md`).

In `bash`, a `skill://` or `local://` word is rewritten to its quoted absolute
path before the command runs (`cat skill://deploy/SKILL.md`,
`python3 local://extract.py > local://out.json`; a bare `skill://<name>` is the
skill's directory). Only whole words expand, bare or quoted in full; a URL inside
longer quoted text or glued to other characters stays as written. The rewrite
happens before the permission gate, so the destructive-command guard and the
cloud tiers judge the real paths, and a word that cannot resolve fails the call
with the reason instead of running.

## Every tool, in one namespace

Built-in tools share one namespace; pin the active set with `-exclude-tools`,
and gate the extras behind flags (see Flags below).
All file tools are contained to the working directory via `os.Root`, and `grep`/`find`
fall back to pure Go when `rg`/`fd` are absent. Both search hidden files, honor
`.gitignore` inside a Git repository, and skip the `.git` directory unless you
point them at it. The one exception to containment is read-only: `read` may also
open this session's own job artifacts (`<journal>.d/jobs/<job>/` `changes.patch`,
`result.txt`, `output.log`), its `progress.md` checkpoint, and spilled tool output
under `<journal>.d/tool-output/` (the "Full output: <path>" of truncated bash
output, tool results cleared from context, and oversized MCP results). Child
journals, worktrees and every other path outside the workspace stay refused,
except through the internal URLs above (`local://` is writable).
Spilled output lives as long as the session's files do.

Model tools use snake_case keys in both arguments and results (`job_id`,
`job_ids`, `subagent_type`, `old_text`, `ignore_case`); the ACP and `tempest/*`
wire methods use camelCase (`jobId`, `jobIds`).

**Files & search**

- `read` — files, directories, archives and their members, office documents, JSON queries, and internal URLs (`skill://`, `job://`, `history://`, `local://`); numbered content when the file fits one read, a structural outline for a Go file that doesn't; `paths` reads up to 20 files in one call, each with its own selector.
- `write` — create or overwrite a file, or a `local://` scratch file.
- `edit` — exact-text replacement, guarded by the `[path#TAG]` tag (or an `lsp_rename` anchor); also edits `local://` files.
- `hashline` — line-anchored `PUT`/`CUT`/`REM`/`MV` patches over one or many files, with block targets and clipboard registers; see [02](#02--line-anchored-edits-that-never-corrupt-a-file).
- `grep` — regex search across the tree, under up to 20 `paths` at once, or under an internal URL.
- `find` — glob-based path lookup.
- `ls` — directory listing.

**Runtime**

- `bash` — workspace shell with a destructive-command guard and a default ten-minute timeout; `skill://` and `local://` words expand to paths; `name` (or `async:true` for an unnamed job) starts a supervised background job with stdin from `/dev/null`, optionally a service that returns once `ready_log`/`ready_port` holds; `stdin:true` keeps a stdin pipe open for `write_stdin`, and `pty:true` runs the job on a pseudo-terminal.
- `eval` — persistent per-session Python/JavaScript code cells, with captured output, tracebacks, interruptible timeouts and explicit reset; requires `python3`, or `node`/`bun` for JS, only when called. See [Persistent code kernels](#persistent-code-kernels).
- `jobs` — page durable job summaries (with job name and service readiness), or explicitly clean one exported worktree.
- `wait` — wait for any requested job, by id or background bash job name, without cancelling it on wait timeout.
- `cancel` — cancel one task or background bash job, by id or name.
- `logs` — page a background command's bounded output (stdout and stderr in write order) by byte offset, by id or name.
- `write_stdin` — type into a running background job (text, Ctrl-C, end of input) and get back the output it printed since the last call, by id or name.

**Coordination**

- `task` — admit asynchronous children and return job handles *(behind `-enable-task`)*.
- `integrate` — three-way merge a finished general task's patch into the live workspace: per file clean, merged or conflict (with markers); writes only when nothing conflicts and the live files still match the snapshot, merged files only with `accept_merged:true` *(behind `-enable-task`; plan mode blocks it)*.
- `message` — message the parent, a running child, or all peers, optionally awaiting the first reply *(behind `-enable-task`; children get it too)*.
- `todo_write` — the task list, replaced in full (`todos`) or edited by one op naming todos by content (`init`, `append`, `start`, `done`, `block` with a reason, `drop`), with optional phases; checkpointed to `progress.md` in the session's data directory (`<journal>.d/`), never in your workspace, and the result shows the updated list.
- `checkpoint` — save workspace bytes, modes and the conversation before an investigation; returns a retained checkpoint id.
- `rewind` — restore checkpoint files and fork the conversation, retaining findings; defaults to the newest explicit checkpoint. Outside edits need operator force, capture gaps cannot be bypassed, and running jobs must settle first.

**Context**

- `skill` — load a skill body on demand when skills are loaded (also via `skill://`).
- `attachment` — read resolved session attachments *(behind `-attach`)*.

**Web** *(gated behind `-enable-web`)*

- `web_search` — one query against the configured search endpoint.
- `web_fetch` — fetch a URL as bounded, structured text.

**Code & manifest intelligence** *(see [Code and manifest intelligence](#code-and-manifest-intelligence))*

- `manifest_diff` — compare two rendered Kubernetes manifest snapshots resource by resource; never runs a renderer.
- `view_image` — show a PNG, JPEG, GIF or WebP file inside the working directory to the model as an image (at most 3,932,160 bytes, 5 MB once base64-encoded, and 8000 px per side). It refuses when the model has no image input in the catalog. Anthropic gets the image inside the `tool_result`, Gemini 3 inside the `functionResponse`, and OpenAI-compatible routes as a user message right after the tool messages, since chat-completions tool messages carry text only.
- `lsp_definition`, `lsp_references` — semantic navigation through a language server *(behind `-enable-lsp`)*.
- `lsp_rename` — a rename **plan** of anchored edits for the ordinary `edit` tool; it writes nothing *(behind `-enable-lsp`)*.
- `lsp_diagnostics` — a file's diagnostics with an explicit freshness verdict *(behind `-enable-lsp`)*.

## Code and manifest intelligence

### Language servers

`-enable-lsp` (`HARNESS_ENABLE_LSP`, settings `enable_lsp`) registers the four
`lsp_*` tools. It is off by default because a language server is installed code
that Tempest executes for you: enabling it is a trust grant, not a sandbox. Nothing
starts when a session opens; each server starts on the first query that needs it,
is reused for the rest of the session, retires after five idle minutes, and stops
when the session closes. Only the root session gets language servers; `task`
children don't.

| Route | Default command | Files |
|---|---|---|
| `go` | `gopls` | `.go` |
| `yaml` | `yaml-language-server --stdio` | `.yaml`, `.yml` outside a Helm chart's `templates/` |
| `terraform` | `terraform-ls serve` | `.tf`, `.tfvars` |
| `helm` | `helm_ls serve` | `.yaml`, `.yml`, `.tpl` under a chart's `templates/` |

A missing executable fails only that tool call (`lsp_server_missing`), with the
tools still registered. `-lsp-config FILE` (`HARNESS_LSP_CONFIG`, settings
`lsp_config`) replaces the whole server list; an empty `servers` array means no
servers and an explicit `-lsp-config ""` selects the built-ins above:

```json
{"servers":[
  {"name":"go","command":"./bin/gopls","env":{"GOFLAGS":"-mod=mod","GOPROXY":"off"},
   "settings":{"gopls":{"staticcheck":true}}},
  {"name":"yaml","command":"yaml-language-server","args":["--stdio"],"disabled":true}
]}
```

Only the four route names are accepted; a relative command containing a slash is
resolved against the file's directory, a bare one through `PATH`. Unknown fields,
duplicate names, non-object `settings`/`initialization_options` (64 KiB each) and
files over 1 MiB are errors. Servers inherit only the same minimal environment as
MCP stdio servers plus their `env`; pass `GOPATH`, `GOMODCACHE`, `GOPROXY`,
`TF_DATA_DIR` or credentials explicitly. `-enable-lsp=false` always wins, even
with a configured file.

Positions are one-based line and column, where a column counts Unicode characters
exactly as numbered `read` output shows them. Paths and returned locations stay
inside the working directory: locations in dependency sources (for example the Go
module cache) are counted as `external_locations_omitted`, not shown. Every result
is compact JSON of at most 64 KiB with `total`/`omitted`/`truncated` counts.

`lsp_rename` returns `{"applied":false,"files":[{"path","anchor","edits":[…]}]}`.
Apply each file with one ordinary `edit` call using those `old_text`/`new_text`/
`anchor` values: each call passes the permission gate on its own, plan mode can
compute a plan but not apply it, and a file that changed since the plan is rejected
by its anchor. The files are separate edits, not one atomic rename; after a
conflict, ask for a fresh plan. Plans over 64 files, 2,000 edits or 128 KiB fail
whole, and renames in workspaces with more than 256 source files fail with
`lsp_rename_scope_limit` rather than guess.

`lsp_diagnostics` reports `fresh` and `freshness` (`current`, `unversioned`,
`pending` or `outdated`). Only `current` means the report matches the file on disk;
no report is `pending`, never "no errors". Edits never wait on a language server
and diagnostics are never attached to edit results automatically.

### Manifest diffs

`manifest_diff` compares two local inputs, each a file (any extension) or a
directory walked for `.yaml`, `.yml` and `.json`, and pairs resources by
`(apiVersion, kind, namespace, name)`, not by filename. Key order, comments,
anchors, merge keys and document layout don't count as changes; list order,
`status`, nulls and default-looking fields do. An absent namespace differs from
`default`, and an `apiVersion` change is a removal plus an addition. Core `v1`
`List` objects are expanded; custom `*List` kinds are not. Duplicate resources,
duplicate keys, custom tags and `generateName` without `name` are errors.

It runs nothing. To review a chart or overlay change, render both versions into the
working directory first (`helm template … > old.yaml`), which goes through the
normal bash gate, then call `manifest_diff` on the two files. Output is JSON with a
full `summary`, up to `max_resources` (50, at most 200) changed resources sorted by
identity, and a canonical-YAML display patch per resource (8 KiB each, 64 KiB per
result), which is a reading aid, not an applicable patch. Core `v1` `Secret`
`data`/`stringData` values are replaced by `<redacted>` while still compared, so a
secret-only change shows `secret_data_changed: true`; pass `redact_secrets: false`
to see them. Limits per side: 2,048 files, 2 MiB per file, 16 MiB total, 2,000
resources, 64 nesting levels and 200,000 YAML nodes including aliases. Symlinks in
the inputs are rejected rather than skipped.

## Persistent code kernels

`eval` executes one cell at a time in each language's session-owned process:

```json
{"language":"py","code":"answer = 41"}
{"language":"py","code":"answer + 1"}
{"language":"js","code":"let answer = 41"}
{"language":"js","code":"answer + 1"}
```

Python uses `python3` and the standard library, not IPython. It returns the
`repr` of a trailing expression; JavaScript uses a synchronous persistent VM
under `node` (preferred) or `bun`, exposes `require`, and displays the last
completion value. JS timers and Promise callbacks run between cells; background
output may appear in a later result. Neither driver rewrites code or supports
top-level `await`.
Missing runtimes produce an explicit tool error; Tempest itself still needs
no interpreter to start or use its other tools.

`timeout` is seconds (default 30, `0` disables it). A timeout interrupts the
cell, keeping variables and effects that happened before the interruption.
SIGINT targets only the kernel, not subprocesses started by earlier cells.
An uncooperative cell or a broken protocol/output stream kills the process group
after a bounded grace; the result reports unknown outcome and lost state.
Prefer Node for interruptible JS: Bun versions without VM interruption (including
1.3.14) require that hard-kill fallback on a CPU-bound timeout and lose state.
`reset:true` replaces only the selected language's process before running
the supplied code. Output keeps a bounded tail and spills the full stream
under `<journal>.d/tool-output/`, like `bash`; a 16 MiB total-output ceiling
stops runaway writers.

Kernels live across prompts, but not across session close, resume or rewind.
After resume, previously used languages require `reset:true` and setup again;
unused languages and SDK-seeded conversations start normally. Rewind stops
existing kernels before restoring files and requires the same explicit reset.
An interrupted cell is never replayed automatically: it may already have
performed side effects.
CLI, ACP/serve and the Go SDK use the same stack and lifetime; general task
children have separate kernels. `eval` is exec-tier and unavailable in plan
mode. Cells are foreground journaled tools, not background `jobs`; idle
kernels don't prevent a run from completing. Session close kills and joins
their processes and descendants, and a parent-lifetime pipe prevents orphan
kernels after a harness crash.

Eval code uses the same destructive-operation scan and cloud policy as inline
Python/JS in `bash`: matching filesystem operations ask on every call in default
and acceptEdits mode. Code naming a cloud tool is opaque, so a deny rule for that
tool wins even in bypass mode; allow-write rules cannot grant opaque code.
These are heuristic approval guards, not a sandbox for arbitrary interpreter code.

For matplotlib figures or PNG bytes, save an image inside the workspace and
call `view_image`. This keeps image validation, vision-model gating and
provider delivery in the existing image tool, without importing matplotlib
or implicitly inspecting every cell value. See [the kernel design](docs/design/08-eval.md).

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
| `repeated_failure` | The model sent the same tool call, with the same arguments, until it had failed with the same error five times in a row. | 1 |

Budgets are checked at turn boundaries: after a turn's tools finish and before
the next model call. A budget-stopped run therefore ends on a complete turn, and
`-continue` picks the session up from there. One long turn can overrun
`-max-wall-time`.

- `-max-turns N` caps model calls.
  Finishing naturally on call N still reports `completed`; the cap stops a run
  only when another turn or outstanding work remains.
- `-max-cost USD` caps the summed cost of the run's model calls. When the
  model's price is unknown and its route doesn't report billed cost, the run is
  refused at startup: an unknown price is not a zero price. On a billed-cost
  route, a call that comes back without a cost stops the run with that error.
- `-max-wall-time D` caps elapsed time, as a Go duration such as `10m`.

A failed tool call sent again unchanged fails the same way, so the loop watches for
it. A failed result that repeats the failure just before it (same tool, same
arguments after JSON canonicalization, same error) gets a host notice appended,
from the second time on, saying how many times in a row the call has failed and
that the arguments or the approach must change. The fifth identical failure in a
row ends the run with `repeated_failure` after that turn; `error` in the report
names the call and its error, and over ACP `session/prompt` fails with `-32023
repeated_failure`. Any other tool result or a user message (a new prompt included)
resets the count, and a successful result is never changed. The count is read from
the transcript, so continuing a resumed session (`Session.Continue`,
`tempest/continue`) carries it on.

Background bash and the four management tools ship with ordinary bash; task
remains opt-in. Cancellation, `Session.Close`, CLI signals and server shutdown
cancel and join owned jobs before the root terminal event. This isn't a detached
process service: no background command is intentionally kept alive after clean
shutdown. SIGKILL can leave OS descendants running; recovery reports their outcome
as unknown, never reruns the command and never signals a stale PID.

A model call that fails transiently is sent again, unchanged, after an
exponential backoff (`HARNESS_RETRY_*`; 11 attempts by default): HTTP 429 and
5xx, dropped connections, error events inside a stream, and stalled streams. A
stream has stalled when no SSE event arrives for 5 minutes before the model's
first output (queueing, or reasoning the model doesn't stream) or for 90
seconds once output has started. Keep-alive comments don't count, so a proxy
holding a hung upstream open is still caught. Sending the call again after part
of a reply was streamed is safe because tool calls run only once a reply is
complete. The abandoned attempt ends as an errored assistant message, in the
transcript and the journal, carrying whatever usage the provider reported for it
(OpenAI-compatible routes report usage only at the end of a stream, so a stalled
attempt there records none, and its `responseId` is kept for looking up the
bill). It is never sent back to the model; the next attempt starts a new
message, and stderr logs `assistant stream retry` with the attempt, the delay and
the cause. Cancellation and other 4xx failures (authentication, invalid request,
context overflow) are not retried. When the attempts run out, the run stops with
`error` naming the attempt count and the last cause.

`-output-format json` prints a run report:

```json
{"session_id": "d6b6fcd56cee7675", "stop_reason": "max_turns", "turns": 3, "usage": {"input": 5120, "output": 410, "...": "..."},
 "cost_usd": 0.0213, "elapsed_ms": 18450, "text": "last assistant text", "message": {"role": "toolResult", "...": "..."}}
```

`cost_usd` is `null` when any call's price was unknown, and `error` appears on
`error`, `output_invalid` and `repeated_failure` stops.

`-output-schema FILE` makes the run's final answer one JSON value that satisfies
the JSON Schema in FILE. The provider is asked for native structured output
(OpenAI-compatible `response_format: {type: json_schema, strict: true}`,
Anthropic `output_config.format`, Gemini `responseJsonSchema`); tool calls stay
free, so the agent still works before it answers. The prompt also carries the
schema, and the final text is validated locally with the same validator as a
task's `output_schema`. An invalid answer gets one correction turn naming the
validation error; if it still fails, the run stops with `output_invalid` and
exits 1. The report's `output` field holds the parsed value. Write strict
schemas (`additionalProperties: false`, every property required): OpenAI's and
Anthropic's native modes require them. In Go, set `Prompt.OutputSchema` and read
`RunResult.Output`; over ACP, send `_meta.tempest.outputSchema` on
`session/prompt` and read `_meta.tempest.output` from the response. `message` is the transcript's last message. In text mode the last
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

Streams include the same `tempest/queue`, `tempest/job`, `tempest/job_output`,
`tempest/message` and `tempest/compaction` updates as ACP/Go watchers. Output
events only announce byte availability; read command logs separately. Child
transcripts/thinking aren't forwarded into the parent stream, but every
agent-to-agent message is, as `tempest/message`. The final run-report shape is
unchanged.

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
`serve` and `mcp` also take `-enable-task`, `-enable-web`, `-enable-lsp`,
`-lsp-config`, `-cloud-policy` and repeatable `-skill` with the same meaning as in
one-shot mode; each flag overrides its `HARNESS_*` variable. Protocol clients
can't choose language-server commands or cloud grants. Tools and language servers
run on the server host, not the client.

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

`tempest/continue` takes `{sessionId}` and starts a run without adding a user
message; its response and errors match `session/prompt`. After a host crash,
`session/load` or `session/resume` the session and call it to finish the
interrupted run: tool calls that never recorded a result come back to the model
as "outcome unknown" and are not re-run. When the transcript already ends with
an assistant reply, continue instead delivers pending `tempest/steer` and
`tempest/follow_up` input or recovered background-job results; with none, it
fails with `-32603` (`Cannot continue without pending input`). Unlike
`session/prompt`, it is admitted while interrupted background jobs still need
recovery (`-32021` `recovery_required`).

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
`Options.LSP` (`*tempest.LSPConfig`) and `Options.CloudRules`
(`[]tempest.CloudRule`) replace the host's language-server and kube-context
defaults for one session; nil keeps them. A session that enables language servers
owns their processes, and `Close` stops and joins them.
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
| `-plan` | Run the read-only plan stack on the `plan` role model. Only read/search/list tools, `manifest_diff`, enabled `lsp_*` tools (rename computes a plan only) and enabled read-only web/attachment tools are available; `-faux-script` still overrides the model provider. |
| `-attach <path>` | Attach a local file to the session; repeatable. Missing paths are startup errors. |
| `-image <path>` | Attach a PNG, JPEG, GIF or WebP image to the prompt as an image block; repeatable. Refused at startup when the model has no image input in the catalog or the image exceeds the `view_image` limits. |
| `-output-schema <path>` | JSON Schema for the final answer; see [Runs](#runs). |
| `-reasoning <level>`, `-plan-reasoning`, `-smol-reasoning` | Reasoning effort: `off`, `minimal`, `low`, `medium` or `high`; unset leaves the provider default. Plan and smol inherit `-reasoning`. Sent as OpenAI `reasoning_effort`, OpenRouter `reasoning.effort`, Gemini `thinkingLevel`, and an Anthropic thinking budget or adaptive effort. A model the catalog lists without reasoning gets no field and one warning. `serve` and `mcp` take `-reasoning`; `doctor` shows each role's effective value. |
| `-enable-web` | Enable the gated `web_search` and `web_fetch` tools. |
| `-enable-lsp` | Register the `lsp_*` tools; language servers start on first use. See [Language servers](#language-servers). |
| `-lsp-config <path>` | Operator language-server file; replaces the built-in server list. An empty path selects the built-ins. |
| `-cloud-policy <path>` | Operator kube-context rules for `kubectl`/`helm` writes. See [Tiered permissions](#05--tiered-permissions-with-a-destructive-command-guard). |
| `-no-hooks` | Disable all command hooks (`HARNESS_NO_HOOKS`, user `no_hooks`). |
| `-trust-project-hooks` | Authorize project/local hook commands (`HARNESS_TRUST_PROJECT_HOOKS`); only use for trusted workspaces. |
| `-skill <path>` | Additional skill directory or `.md` file; repeatable. See [Skills](#skills). |
| `-permission-mode <mode>` | Tool gate mode: `default`, `plan`, `acceptEdits` or `bypass`. |
| `-allow-tool <name>`, `-deny-tool <name>` | Always allow or deny one tool; repeatable, deny wins. A name that is neither a built-in tool nor `mcp__*` gets a stderr warning on one-shot runs, `serve` and `mcp`. |
| `-faux-script <path>` | Replace the model with a scripted one; see [Testing without a provider](#testing-without-a-provider). |

Every command takes flags only: the prompt goes in `-p`. A stray argument, such
as `run` in `tempest run -p hi`, or an unknown flag exits with status 2 and names
the problem.

## Configuration

Flags win over `HARNESS_*` environment variables, which win over
`~/.config/harness/settings.json`, which win over built-in defaults. The command does
not read `.env` — export variables before running it.

| Setting | Purpose |
|---|---|
| `ANTHROPIC_API_KEY` / `ANTHROPIC_OAUTH_TOKEN` / `ANTHROPIC_AUTH_TOKEN` | Anthropic credential (key, OAuth token, or proxy bearer). |
| `ANTHROPIC_BASE_URL` | Anthropic-compatible provider URL. `-anthropic-base-url`, `HARNESS_ANTHROPIC_BASE_URL` and settings `anthropic_base_url` override it, in that order. |
| `OPENAI_API_KEY` | OpenAI-compatible credential. |
| `OPENAI_BASE_URL` | OpenAI-compatible endpoint for `openai:<id>` models without `@<base-url>`. `-openai-base-url`, `HARNESS_OPENAI_BASE_URL` and settings `openai_base_url` override it, in that order. |
| `GEMINI_API_KEY` / `GOOGLE_API_KEY`, `OPENROUTER_API_KEY` | Google and OpenRouter credentials. Point those routes elsewhere with `google:<id>@<base-url>` or `openrouter:<id>@<base-url>`. |
| `HARNESS_MODEL`, `HARNESS_PLAN_MODEL`, `HARNESS_SMOL_MODEL` | Model for each role. |
| `HARNESS_REASONING`, `HARNESS_PLAN_REASONING`, `HARNESS_SMOL_REASONING` | Reasoning effort per role (settings `reasoning`, `plan_reasoning`, `smol_reasoning`). |
| `HARNESS_PERMISSION_MODE` | Default tool gate mode. |
| `HARNESS_PERMISSION_ALLOW_TOOLS`, `HARNESS_PERMISSION_DENY_TOOLS` | Per-tool allow/deny lists. |
| `HARNESS_SESSION_DIR`, `HARNESS_AGENT_DIR` | Session and agent data directories. |
| `HARNESS_BASH_TIMEOUT` | Default bash timeout as a Go duration, for example `1s` or `10m`. |
| `HARNESS_RETRY_*` | Retry policy: `ENABLED`, `MAX_ATTEMPTS`, `BASE_DELAY_MS`, `BACKOFF_CAP_MS`, `MAX_DELAY_MS`, `JITTER_MIN`, `JITTER_MAX`. |
| `HARNESS_STREAM_IDLE_SECONDS`, `HARNESS_STREAM_FIRST_OUTPUT_SECONDS` | Stream stall bounds: seconds without an SSE event once output has started (default 90), and before the first output (default 300, never below the idle bound). |
| `HARNESS_WEB_SEARCH_URL` | Web-search endpoint when web tools are enabled. |
| `HARNESS_ENABLE_LSP`, `HARNESS_LSP_CONFIG` | Language-server tools and server file (settings `enable_lsp`, `lsp_config`). |
| `HARNESS_CLOUD_POLICY` | Kube-context rule file (settings `cloud_policy`). |
| `HARNESS_NO_HOOKS`, `HARNESS_TRUST_PROJECT_HOOKS` | Disable all hooks or explicitly authorize project/local hooks. |
| `HARNESS_LOG_LEVEL`, `HARNESS_DEBUG`, `HARNESS_LOG_FILE`, `HARNESS_LOG_FILE_MAX_BYTES` | Logging controls. |

Run `./bin/tempest -help` for the full flag list and the `serve`, `mcp`, `doctor`,
`rewind` and `version` subcommands.

## Testing without a provider

`-faux-script <path>` (also `HARNESS_FAUX_SCRIPT` or settings `faux_script`)
replaces the model with a scripted one, so a run needs no API key or network.
It works for one-shot runs, `serve` and `mcp`, and overrides `-model`; reports
name the model `faux-1`. The script is a JSON array with one entry per model
call, used in order:

```json
[
  {"text": "Listing files.", "toolCalls": [{"id": "b1", "name": "bash", "arguments": {"command": "ls"}}]},
  {"text": "done"}
]
```

- `text` is the assistant text for that call; it may be omitted.
- `toolCalls` lists the tool calls the model makes: `name` is required,
  `arguments` is the tool's JSON input (default `{}`) and `id` is the tool-call
  id. The tools really run, through the normal permission gate.
- After an entry with tool calls, the tool results go back to the model and the
  next entry answers them. An entry without tool calls ends the run.
- An empty array is a startup error. A model call after the last entry fails
  the run with `No more faux responses queued`.

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
