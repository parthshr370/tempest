# Conversation controls

A session has two histories. Its active context is the projection sent to the
provider. Its audit archive retains committed messages, controls and receipts.
Fork, reset, compaction and rewind do not subtract lifetime usage.

## Fork a conversation

`Host.Fork(ctx, parentID, entryID, options)` creates an ownerless sibling. Use a
durable entry ID from an archive read or `SearchHistory`, not a message index.
The boundary must contain a result for every tool call in its assistant round.
`ForkOptions.PrecedingSafe` explicitly selects the preceding complete boundary
when the requested entry interrupts a tool round.
Entry IDs inherited from a fork's pinned archive are valid too. Resolution follows
the immutable archive chain, even after the original session has been deleted.

```go
sibling, err := host.Fork(ctx, parent.ID(), entryID, tempest.ForkOptions{})
```

The sibling receives the agent selection recorded at the cut. Each application
document follows its declared policy:

| Fork policy | Initial sibling value |
|---|---|
| `asOf` | The value at the selected entry. This requires `rewindable` history. |
| `current` | The parent's committed value when the fork captures state. |
| `initial` | The definition's default value. |

The sibling has a fresh session and provider cache identity. Its spend starts at
zero. It does not inherit queued input, tasks, invocation approvals, live job
handles or interpreter memory. Operator runtime policy still applies.

The JSONL adapter seals a content-addressed manifest under `<session-dir>/archive`.
Each manifest pins immutable record, message and request-frame payloads. Different
cuts share overlapping payloads instead of copying them again. The capture lock
protects the snapshot, not archive writes or syncs, so the parent can keep appending
while sealing finishes. Parent reset and deletion cannot change those bytes.
Missing or changed manifest or payload bytes are corruption. Keep the whole
archive directory when moving or backing up sessions. Tempest doesn't
garbage-collect it.

A fork does not branch files. By default it shares the parent's identified
workspace generation. Supply `ForkOptions.Environment` for another resolved
workspace. A local fork can use `ForkOptions.Cwd` instead. Neither choice is an
authorization to provision a workspace or fall back from remote files to local
files.

```sh
tempest fork -session <parent-id> -entry <entry-id>
tempest fork -session <parent-id> -entry <entry-id> -preceding-safe
```

## Reset and handoff

```go
receipt, err := session.Reset(ctx, "Start a fresh investigation", "reset-2026-10-24")
```

Reset admits a durable, keyed control. Reusing the request ID returns the same
receipt. Changing its note under that ID is a conflict. An idle session applies
it immediately. A busy session applies it after the current complete tool batch,
then ends that run. A reset does not fabricate a user message or start another
provider call.
A reset admitted after new input was appended waits until that input's first
provider turn finishes. ACP orders reset admission before a following prompt.

Application documents use their separate reset policy, `preserve` or `initial`.
The audit archive, accepted input, lifetime spend, grants and task facts remain.
Reset does not automatically cancel background tasks. A summary from the old
context becomes stale and cannot replace the new context.

A tool can return `tempest.ToolResult{Control: "handoff", Content: ...}`. Its text
is the handoff note. Every tool result is journaled as soon as it completes, so a
crash during a long batch doesn't lose already-finished output. The host waits
for every sibling result before committing the handoff context head and ending
the run, including when a hook also stops the run. A crash before that head
doesn't expose a partial context reset. Tool-start and finish facts remain
auditable; an incomplete batch never authorizes rerunning an unknown effect.
Submit a separately keyed input to start the next run.

```sh
tempest reset -session <id> -request-id reset-2026-10-24 -note 'Fresh investigation'
```

Checkpoint rewind also changes the context epoch. It restores `rewindable`
application documents at the checkpoint cut. It leaves `latest` documents,
permissions, submissions and lifetime usage alone. See [checkpoints](checkpoints.md).

## Background and manual compaction

Automatic compaction is a native background task on the session's durable task
scheduler. A soft input threshold starts a speculative summary. Generation can
continue while it is running. The hard threshold waits for that summary before
requesting an oversized context.

```go
settings := tempest.CompactionSettings{
    Enabled: true,
    ReserveTokens: 16384,
    KeepRecentTokens: 20000,
    SoftThresholdTokens: 90000,
}
// Supply settings through StackConfig.Compaction for the selected model.
task, err := session.Compact(ctx, tempest.CompactOptions{Wait: true})
```

The default reserve accounts for the selected model's output budget. Explicit
settings select the reserve, recent tail and optional soft threshold. The hard
input threshold is the context window minus the reserve. A zero soft threshold
is derived from that budget.

Manual and automatic requests coalesce with the active cut. `Wait: false` admits
and starts work without waiting. Completion and placement are distinct. An idle
waiting caller can place the completed summary; a busy run places it at a later
safe boundary. Cancelling the caller's wait does not cancel the task.
`BeginContinue` and `ResumePending` resume session-background tasks asynchronously;
they don't drain a held summarizer before admitting foreground work.

Placement validates the context epoch and the captured prefix, not equality with
the entire current transcript. It preserves turns appended after capture. A
placement receipt prevents adopting the same cut twice. Reset and rewind make an
old cut stale. Summary phase receipts and known usage survive restart. Cancellation
and suspension don't write final failure receipts. A started summary with no
finished receipt remains outcome unknown and isn't paid for again for that cut.
A parked, failed or aborted task gets a durable disposition and releases the
active cut. A later request can capture the grown conversation; neither failure
nor restart pins all future compactions to the abandoned cut.
Summary calls share the live root run's cost-admission gate. Their durable receipts
charge its cost budget once, including CLI `-max-cost`, before another provider
call can spend the same remaining budget.

A provider context-overflow error may compact and retry once for the latest
admitted input. The retry claim is durable. Continuing after restart does not
replenish it. A second overflow or a terminal summary failure remains visible.
Disabling automatic compaction also disables paid overflow recovery. A fork with
no local admitted input preserves the original provider overflow error.

```sh
tempest compact -session <id>
# Hermetic provider scripts work for context controls too.
tempest compact -session <id> -faux-script ./summary.json
```

## Search old history

`SearchHistory` searches literal text case-insensitively. It pages the immutable
audit archive, including messages hidden by reset, rewind or compaction. It does
not inject archived messages into the provider context. Search covers text and
reasoning blocks, decoded tool arguments, tool result text, and context notes or
summaries, not JSON field names, tool details or signatures.

```go
page, err := session.SearchHistory(ctx, tempest.HistoryOptions{
    Query: "original failure",
    Limit: 20,
})
// Pass page.NextCursor to the next call with the same query and limit.
```

A page scans at most the requested number of archive records, from 1 to 100.
It can have no matches and still have a next cursor. Always follow that cursor.
Later appends cannot move its cut. Matches carry durable entry IDs. Large matches
return a bounded excerpt around the match and `clipped: true` instead of the full
entry. Use the ordinary durable read API for the full record.

The read-tier `search_history` tool uses the same paging and is available in plan
mode. Host authorization applies to SDK and ACP archive reads. Fork cursors can
only address that session's pinned prefix.

```sh
tempest history -session <id> -query 'original failure' -limit 20
tempest history -session <id> -query 'original failure' -limit 20 -cursor '<nextCursor>'
```

## ACP controls

ACP advertises these custom methods in `_meta.tempest.durable.methods`. They are
Tempest extensions, not standard ACP session methods. Each call requires
`sessionId`.

| Method | Additional parameters |
|---|---|
| `_tempest/durable/fork` | `entryId`, optional `forkOptions` |
| `_tempest/durable/reset` | `note`, `requestId` |
| `_tempest/durable/compact` | Optional `compactOptions`, including `wait` |
| `_tempest/durable/search_history` | `history` with `query`, `cursor`, `limit` |

Fork returns the new `sessionId`. Reset returns an ordinary durable submission
receipt. Compact returns its native task record. Search returns `matches`, `cut`
and an optional `nextCursor`. Existing durable task and submission lookups can
observe work after reconnect. Disconnecting a client does not withdraw accepted
controls or grant authority to repeat an interrupted effect.
