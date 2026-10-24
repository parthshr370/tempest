# Go SDK

The CLI, `serve` and the Go SDK all drive the same session stack. Import
`go.harness.dev/harness/tempest` to open sessions, watch updates, prompt, cancel and
close in-process.

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

`Options.Provider` (`tempest.ProviderConfig`) selects endpoint routing for the
session. Its `Providers` map holds `tempest.ProviderDefinition` values, the same
registry shape as settings. A nil map keeps the host defaults; a non-nil map replaces
them for that session.

[The complete example](../examples/embed/main.go) uses the faux provider by
default, so it needs no API key. Pass `-model` to use a real provider:

```sh
go -C examples/embed run .
ANTHROPIC_API_KEY=... go -C examples/embed run . -model anthropic:claude-opus-4-8
```

The example is a separate module with a local `replace`; build it with
`go -C examples/embed build ./...` or `make build-examples`.

## Durable submissions and state

Use a stable request key when the caller may retry after losing a response:

```go
receipt, err := session.Submit(ctx, tempest.Input{
    RequestID: "client-request-42",
    Prompt: tempest.Prompt{Text: "Explain this project"},
    Placement: tempest.PlacementInput,
})
if err != nil {
    return err
}
settled, err := session.WaitSubmission(ctx, receipt.RequestID)
if err != nil {
    return err
}
fmt.Println(settled.ID, settled.Answer, settled.Result.StopReason)
```

`Submit` acknowledges the backend durability point. `Submission(ctx, requestID)`
looks up the lifetime receipt. `WaitSubmission` cancels only its wait.
`Withdraw(ctx, requestID)` explicitly withdraws pending input. Same-key retries
return the original identity; changed content conflicts. `Begin` and `Prompt`
use the same admission implementation with generated keys.
`Prompt` retains its deadline semantics: cancelling its context cancels and joins
its own run before it returns. `Submit` and `WaitSubmission` separate admission
from waiting. Use `CancelSubmission(ctx, requestID)` to cancel only that request's
live run; an old request cannot cancel another caller's run.


Import `go.harness.dev/harness/tempest/durable` for declarative `Commit` and
paged `Query` types. `Session.Commit` validates document namespaces and expected
revisions before adopting a whole batch. `Session.Read` reads committed state at
an immutable cut. Host control records cannot be written as application documents.
The file backend syncs commits. The explicit memory backend is nonpersistent.
Application commit IDs occupy a separate namespace from host decisions. Keep the
original caller ID for `Session.Read(Query{CommitID: id})` receipt lookup.
Application documents remain bounded to 512 KiB per value. Large transcript and
submission batches spill to synced, SHA-256-addressed immutable blobs beside the
journal before their bounded reference envelope commits. Missing or changed blobs
are corruption, not empty history. Keep the `.blobs` directory with its journal;
there is no automatic blob garbage collection.

Injected shared backends must also implement `durable.SessionLeaser`.
`AcquireSession` rejects a second writer for a live session and releases its claim
on session close. Backends without that capability fail with `ErrUnsupported`
instead of allowing unfenced writes. The memory and file logs implement it.
Snapshots carry bounded newest-first document and submission metadata, not
application values, prompt payloads, answers, or private agent configuration.
Use paged `Session.Read` and `Submission` for full committed values.


## Hosting several sessions

`OpenHost(ctx, tempest.HostOptions{SessionDir: dir})` owns the shared session
registry used by serve. `Create(ctx, tempest.AgentOptions{Cwd: cwd, New: settings})`
creates a session. `Open(ctx, id)` reuses a live owner or opens its journal.
`List(ctx, tempest.ListOptions{Cwd: cwd, Limit: 100})` pages session metadata
newest first, including archived run summaries. Missing journal files stay out of
the listing. `CloseSession(ctx, id)` closes one owner; `Close(ctx)` closes the host.
`HostOptions.MCPServers` supplies runtime endpoints when a create or resume omits
its own MCP selection.

Opening and listing do not run accepted input. Call `ResumePending(ctx)` explicitly
to schedule durable pending work in currently open sessions. It attempts every
open session and joins errors rather than stopping at the first busy session.
A CLI history read must not make that lifecycle decision. Provider credentials and
executable configuration remain runtime inputs, not journaled secrets.

Set `HostOptions.Limits` to `tempest.HostLimits{MaxConcurrentRuns: 4,
MaxConcurrentProcesses: 8}` to bound work across conversations. Zero is unlimited.
A full limit returns `tempest.ErrHostLimit` immediately. Child generations count
as runs too, so a parent never waits for a permit held by its own child.
Process permits cover actual lifetimes, including persistent eval kernels.

The file host serializes publication through a stable `.tempest-host.lock` lease,
not slow session initialization. Its durable catalog publishes only fully
initialized journals. Recovery can finish publication after initialization
committed but the host lost its reply. Partially initialized journals stay hidden;
invalid or unreadable journals are skipped with a diagnostic. Catalog pages keep
one immutable cut, so sessions created between pages do not shift that listing.

Passing a backend transfers ownership only after `OpenHost` succeeds. A failed
constructor leaves that backend open for the caller. A successful host closes it
only after all sessions join, even when an individual `Close` waiter times out.


[Guide index](README.md) · [Tempest](../README.md)
