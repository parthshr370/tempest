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

[Guide index](README.md) · [Tempest](../README.md)
