# Extensions

Extensions are named, operator-installed bundles. A bundle can supply prompt
sections, tools, typed hooks, result wrappers, document definitions, and agent
presets. Go implementations and subprocess implementations use the same handler
contract. General extension task phases are not supported.

## Go bundles

Import `go.harness.dev/harness/tempest/extension`. Install a complete
`extension.Definition` in an `extension.Registry`. Pass that registry and an
ordered `Options.Extensions` selection to `tempest.Open` or the host's session
options. Mark safety bundles `Required: true`.

Each definition has a stable ID and an explicit code revision. Tool names become
`ext__<bundle>__<tool>`, escaping `_` as `-u` and `-` as `-h` in each
component so distinct identities cannot collide. Their immutable identity is
`<bundle>/<tool>`. A wrapper names that identity, not a display name. Built-ins
use `builtin/<tool>`.

`Registry.Install` publishes a validated generation atomically. Running requests
keep their selected generation through the response and its tools. Later requests
resolve the replacement. `Uninstall` blocks new work that requires the missing
bundle. It does not cancel admitted work. Retired processes close after their
last generation lease ends. A caller-supplied registry remains caller-owned.

Document definitions currently support version 1 only. Replacing a live bundle
with changed document schemas, defaults, or policies is rejected before
publication. Reopening with an incompatible definition fails visibly and leaves
the stored definition intact. Document migration requires an explicit migration
implementation, not a code revision change.

Prompt sections render from scoped committed reads before each new provider
request. Their read-only environment capability supports bounded UTF-8 reads
inside the session working directory. The host records the exact system bytes, tool
schema digest, and selected revisions before dispatch. Historical frames do not
rerender during recovery.

## Hooks and wrappers

Hook events are `before_request`, `after_response`, `before_tool`, `after_tool`,
`on_yield`, and `before_compaction`. Handlers run in selected bundle order.
Before-tool rewrites feed the next handler. The final input is validated again
before the permission gate. A hook cannot bypass plan mode, an explicit deny, or
a child's inherited ceiling. After-response observer failures are reported.
After-tool failures withhold model content, structured output, and details.
Before-compaction deny skips compaction; faults stop the operation. Other
preparation failures stop the operation. On-yield takes the first continuation
and shares the loop's bounded continuation allowance.

Command hooks retain their Claude compatibility semantics and use the same
preparation, execution, and result boundaries. Nonzero shell exits do not acquire
a new meaning because an extension is installed.
If an extension subsequently rewrites arguments, a command hook's earlier allow
does not authorize the rewritten call. The final gate evaluates it again.

Wrappers replace only the execution body. The host keeps validation, the gate,
intent persistence, result persistence, and provider accounting outside the
wrapper. `Next.Call(ctx)` receives the pinned, gated arguments. It is single-use
and expires when the wrapper settles. Redaction covers model content, structured
content, and details. Intermediate wrapper outputs and after-tool hook inputs and
decisions are recorded as digests, not raw output. Only the final approved result
is stored as the terminal tool result.

## Memos and callback authority

`Invocation.Memo` returns the persisted first value for a key. `ReadMemo` reports
presence separately. `false` and `null` are values, not missing keys. Scope includes
the session, logical invocation, extension, and handler. A new call does not reuse
an old approval.
Subprocess memo writes return `value`, `present`, and an atomic `inserted` flag.

An invocation can read and commit only its declared document namespaces. It
cannot enumerate other bundles' documents, memos, or host receipts. Commit IDs
are scoped by session, logical invocation, extension, and handler. Invocations
can publish bounded ordered progress or make a keyed nested tool call. Nested
calls use ordinary validation and permissions. Callbacks expire on return or
cancellation. Sections get no mutation or nested-call capability. A memo is a
decision receipt, not an exactly-once guarantee for outside effects.

## Subprocesses

Use `Options.ExtensionProcesses` or the explicit `-extension-config <path>` flag.
The config is a JSON array of operator launch definitions:

```json
[
  {
    "id": "sample",
    "argv": ["python3", "/absolute/path/to/extension.py"],
    "cwd": "/workspace",
    "capabilities": ["read", "commit", "memo", "output", "next"]
  }
]
```

The host never discovers executables in project files, installs packages, or
loads Go shared-library plugins. Skill directories remain data. An operator may
use an explicit SSH argv for a remote peer. Tokens scope callbacks; they are not
a sandbox against a native process running as that operator.

By default processes inherit only path, home, temporary-directory, and locale
variables, not provider credentials. `Env` explicitly replaces that environment.
Manifest read effects become execute effects unless the operator names the tool
in `ReadTools`. Safe replay is disabled unless the operator names the handler in
`SafeHandlers`, using `tool:<name>`, `hook:<name>`, or `wrap:<name>`. These launch
options are executable operator policy and are never restored from a journal.

The transport is bidirectional JSON-RPC 2.0 over NDJSON. Stdout carries protocol
frames only. Stderr carries bounded logs. Initialization negotiates
`tempest-extension/1`, the operator-fixed identity, code revision, descriptors,
and callback capabilities. The host grants only the requested/offered
intersection. Methods use `_tempest/extension/*`. This is not ACP or MCP
conformance for hooks or wrappers.

Frames are limited to 1 MiB. Invalid UTF-8, JSON, versions, descriptors, and
schemas fail installation or retire the peer. At most 32 ordinary callback workers
run per process; excess callbacks wait instead of killing it. Single-use `Next`
callbacks do not occupy those workers while the nested implementation runs.
Each invocation permits at most 1,024 callbacks and 1 MiB of ordered progress.
Exhausted MCP progress stops observation, not an otherwise successful call.
Default process calls time out after 30 seconds, excluding time inside `Next`.
Caller cancellation, ordinary RPC errors, and oversized outgoing requests fail
only their call. A hard process timeout, malformed result, or disconnect makes
the generation unavailable to new required selections. Retirement drains, then
terminates and kills an unresponsive process group within a bounded close.
A disconnect is not proof an outside effect stopped.

The repository includes equivalent Go, Python standard-library, and JavaScript
fixtures in `tempest/extension/testdata`.

## Replay and recovery

`ReplayNever` is the zero value. Old journals and ordinary tools keep Never
semantics. MCP read-only and idempotency hints do not authorize replay. MCP
catalogs can be represented as tools-only bundles without pretending hooks are
MCP tools.

A completed result is reused. User-cancelled calls record a terminal abort even
after their context is cancelled and are not replayed. An interrupted Never effect remains unknown.
Explicit resume can rerun a Safe effect only when the stored and current whole
chain remain Safe and the revision, schema, and environment match. A new more
permissive revision cannot authorize an old call. A charging or effectful wrapper
makes the chain Never unless repeating that whole chain is independently safe.
Missing compatible code parks the call instead of silently using the latest code.

## Named agent presets

Bundles can declare versioned `AgentPreset` values. The task tool accepts the
selected `<bundle>/<preset>` names alongside built-in `explore` and `general`.
Preset instructions, model selection, and tool restrictions use the existing
durable child runner. They do not create another scheduler or relax child
permissions. Preset IDs and versions remain in the child admission record.

[Guide index](README.md) · [Tempest](../README.md)
