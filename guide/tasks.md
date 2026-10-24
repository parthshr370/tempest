# Durable tasks

Durable tasks extend the existing jobs supervisor. A task stores its input,
checkpoint, phase, definition version, code revision, owner, status, attempt,
revision, waits, and optional absolute wake deadline in the session journal.
Executable handlers never enter the journal.

## Define and admit work

Install an `extension.TaskDefinition` in a selected Go bundle. Supply a name,
nonzero version, input and checkpoint JSON schemas, an initial checkpoint, and a
`Run` handler. `Abort` and a pure `Migrate` handler are optional. Names resolve as
`<bundle>/<task>`. Both Go handlers and subprocess task descriptors use this
runtime.

Use `Session.CreateTask` with a stable ID, definition name, and JSON input. A
repeated ID with identical input returns its original task. Changed input or
ownership conflicts. Background roots require `Background: true`. Ordinary
roots require an active run. Plan mode cannot admit tasks. Child tasks inherit
their owner's run and background lifetime. This does not relax nested tool
permissions or the existing child-agent tool ceiling.

Admission does not execute code. Call `Session.ResumePending` or
`Host.ResumePending` explicitly. Loading history, opening a session, reading a
task, and watching snapshots do not execute phases.

## Phase decisions

Each phase receives an immutable `extension.Task` snapshot and a revocable
`Invocation`. `Task.Now` comes from the configured host clock. Return one
`TaskDecision`:

- `advance` stores a checkpoint and optionally changes the phase.
- `sleep` stores an absolute `WakeAt`, checkpoint, and next phase.
- `wait` stores task IDs and optionally `FailFast`.
- `complete` stores the result. `Error` selects a failed outcome.

`Commit` carries application document writes and expected document revisions.
`Children` declares owned tasks. `Memos` inserts first-write-wins phase values.
`Submissions` declares keyed conversational messages. The host validates all of
them and commits the phase transition, document changes, memo insertions, child
admissions, and delivery receipts atomically. A delivery
request ID is `task:<task ID>:<submission key>`. A repeated key must retain the
same content, even after its delivery has completed.

`Invocation.Commit` commits documents under the live task revision without
changing the phase or checkpoint. The handler must still return a phase decision
to advance. A replay uses the original commit ID and batch, including its original
revision fence; changed content conflicts. Memo callbacks use task, bundle, phase,
and memo key as their logical scope. Attempts share those memos. Abort, phase
transitions, and suspension revoke late callbacks. Nested tool calls retain their
own keyed intents, permission gate, results, and conservative interrupted-effect
policy.

There are at most four live phase invocations and 64 active workflow tasks per
session. Sleeping tasks retain no invocation worker. A phase drain yields after a
bounded transition allowance, without failing work or counting a crash; a later
resume continues it. `TaskOptions.Now` injects the clock;
`TaskOptions.MaxCrashAttempts` controls the interrupted-phase parking threshold.
The default threshold is three.

## Ownership, waits, and cancellation

Owner edges form a tree. Wait edges are separate. Missing or foreign-session
references, owner cycles, and wait deadlocks are rejected before commit.
`FailFast` can wait only on direct owned children. All-settled waits may observe
unrelated tasks but never abort them.

A decided successful result remains `completing` until owned work settles.
Aborting records intent before cancelling invocations. Descendant admission
stops. Local invocations are joined. Children run cleanup before their parent's
abort handler. Abort handlers can checkpoint, sleep, and resume like ordinary
phases. A failed abort handler becomes `cleanup_faulted`, which exposes an
unresolved cleanup obligation instead of claiming clean cancellation.

Repeated abort requests preserve an in-progress compensation's phase, checkpoint,
and backoff. Explicit retry reopens a task's own faulted cleanup; an owner marked
unresolved because of its child remains terminal and cannot rerun completed work.

`Run.Cancel` and ordinary session cancellation record foreground abort intent and
preserve explicit background roots. Run finish doesn't wait for compensation:
cleanup runs in a session-owned cancellation scope that `Session.Close` can stop.
`Session.CancelTask` targets the owned subtree. `Session.CancelAll` includes
background roots. `Session.Close` joins local invocations and suspends durable
phases without recording background user abort. It cancels and joins in-flight
task drains before releasing the store. No resume scope can bypass suspension.
Reopen and explicitly resume to continue them. Sleeping foreground tasks don't
prevent steering or follow-up delivery.

Native delegated-agent and bash jobs keep their existing supervisor, receipts,
limits, output artifacts, worktree handling, and permission ceilings. Calls from
a phase record its ownership and revision at admission. The owner cannot publish
completion before its native children join. Ready services and ordinary bash
still stop on run finish. A persisted PID never authorizes a new signal after
restart. Interrupted shell and eval effects remain unknown and are not replayed.

## Recovery and alarms

A phase intent commits before dispatch. A crash before the decision can invoke
the phase again, with the same task and phase identity and a new attempt.
Revision comparisons fence stale decisions. Repeated interrupted attempts park
instead of looping forever. Missing definitions, incompatible code, and migration
failures also park work with a visible reason and keep its owner edges.

Rejected decisions (including invalid waits, reused child IDs, and changed
submission content) park with the validation reason and preserve the original
phase/checkpoint. They aren't crashes and don't fault the native jobs supervisor.
Document CAS conflicts release the phase for a fresh attempt on the next resume,
also without a crash count. If both a commit acknowledgment and its receipt lookup
are lost, the runtime retains the exact batch and reconciles it before another
transition. Storage outages remain errors, not validation rejections.
Parked foreground work (including owners waiting on it) doesn't keep a
conversation busy or turn normal completion into an abort. Explicit cancellation
still records abort intent for that run's unsettled foreground work.

Install compatible code, then use `Session.RetryTask` to unblock parked work.
Abort can still be requested while a definition is missing. Cleanup parks until
that definition is available. A newer task version requires a pure migration
from the old checkpoint. Its validated state commits before the new handler
runs. Changing only the code revision does not authorize replay of an old task.

`Session.NextWake` reports the earliest persisted timer. `Host.NextWake` also
scans archived session task facts without opening writers or launching
extensions. The file-backed archived alarm query scans the journal. It is not an
indexed database operation. `Host.ResumePending` reopens archived due tasks and
uses the normal session runtime, then closes the sessions it reopened after their
drain and any admitted run finish. One unreadable archive or unavailable extension
doesn't hide healthy alarms or prevent other sessions from progressing. Failed
alarm opens/drains report a per-session error and retry after one minute instead
of spinning on a past-due alarm. A service can set one alarm and explicitly
resume. A CLI process that has exited cannot deliver a reminder by itself.

Phase execution is at least once, not an exactly-once charge. Use a stable
external idempotency key such as task ID, phase, and effect name. The remote
service must retain the original effect result under that key. An effect can
succeed before its decision commits. A restart can then call that service again.
A service without deduplication leaves that outcome ambiguous. Compensation is
another external effect with its own stable key, not an atomic rollback.

## Subprocess and ACP controls

Subprocess initialization accepts task descriptors and the `tasks` capability.
`_tempest/extension/tasks/step` receives `name`, `task`, `abort`, and a scoped
`context`. Its result is the same `TaskDecision` as Go. Declare `migrations` as
supported source versions to receive a pure migration step with `name` and
`migrate: {fromVersion, checkpoint}`. That step has no callback token and returns
`{checkpoint: ...}`.

ACP advertises task support under `_meta.tempest.durable.tasks`. Controls are
`_tempest/durable/create_task`, `get_task`, `tasks`, `cancel_task`, `retry_task`,
`cancel_all`, `resume_pending`, and `next_wake`. They use `sessionId`, plus `task`
or `taskId` where appropriate. `next_wake` returns `wakeAt` and `present`.
`tasks` accepts `taskQuery: {limit, cursor}` and returns `{tasks, nextCursor, seq}`:
a stable page of summaries, including terminal history. The limit defaults to 100
and cannot exceed 100. `Session.Tasks(ctx, TaskQuery)` uses the same page shape.
Use `get_task` or `Session.Task` for full input, checkpoint, waits, and result.
Summaries bound long labels/reasons and omit payloads, so a large checkpoint can't
blank a hub snapshot or overflow a task-list frame. The scheduler retains only
active tasks, validates changed reachable graph edges, and borrows terminal
references from the store; durable read queries page the complete records.

The real-binary restart proof is reproducible without a provider key:

```sh
lockf -k /tmp/parth-heavy.lock go build -o /tmp/tasks-tempest ./cmd/harness
lockf -k /tmp/parth-heavy.lock python3 internal/taskstate/testdata/binary_restart.py /tmp/tasks-tempest "$PWD"
```

The fixture restarts a persisted reminder and an owned checkout child. It also
proves bounded crash parking, explicit retry, and restart during compensation.
The fake external service deduplicates stable effect keys. These checks do not
claim generic exactly-once effects.

The transport follows [JSON-RPC 2.0](https://www.jsonrpc.org/specification).
ACP task controls use the official
[custom method and capability rules](https://agentclientprotocol.com/protocol/v1/extensibility).
Task decisions themselves are a Tempest extension, not a standard ACP task API.

[Guide index](README.md)
