# Execution environments

An environment binds built-in workspace operations to one workspace host. The default is Local. A Remote lease uses a dedicated JSON-RPC connection to an operator-selected daemon. Remote file paths never become fallback paths in the harness process.

## Selecting a remote workspace

Start the daemon through an explicit argv. No shell expands the launch definition.

```sh
tempest -p 'Inspect this workspace' \
  -environment development \
  -environment-command '["ssh","builder","tempest","env","-id","development","-cwd","/srv/project"]'
```

For a local fake remote host, replace the launch array with `["/absolute/path/to/tempest","env","-id","development","-cwd","/absolute/workspace"]`. The same environment flags work on `tempest serve` and `tempest mcp`.

User settings can register multiple operator-owned launch definitions:

```json
{
  "environment": "development",
  "environments": {
    "development": {
      "argv": ["ssh", "builder", "tempest", "env", "-id", "development", "-cwd", "/srv/project"],
      "timeout": "30s"
    }
  }
}
```

Flags override `HARNESS_ENVIRONMENT` and `HARNESS_ENVIRONMENT_COMMAND`, which override user settings. The command value is a JSON argv array. A selected name without an operator launch definition fails. Project settings cannot install daemons.

The daemon's stdout contains only protocol frames. stderr carries bounded diagnostics to the host's stderr. Its inherited environment contains ordinary process-location and locale variables plus `SSH_AUTH_SOCK`, `USER`, and `LOGNAME` for SSH agent authentication, not arbitrary provider credentials. `EnvironmentProcess.Env` supplies an explicit replacement when the operator needs more variables.

## Identity and recovery

A persisted `environment.Reference` contains `ID`, `Generation`, and a slash-separated relative `Cwd`. The bundled daemon binds its name to the canonical root directory's filesystem identity. Its cwd is `.`. It does not offer changing the leased root through tool arguments.

The agent document and each workspace tool intent retain the full resolved reference. Reopening a remote environment obtains the same generation before workspace discovery, hooks, language servers, checkpoints, or tools can access files. Replacing the directory with another generation blocks remote recovery. Local conversations retain their logical cwd for journal grouping and can resume after a re-clone; an active Local lease still refuses calls if its root directory is replaced while it is open.

Remote journals use an `env://<name>/<relative-cwd>` location. This is a storage grouping key, not a host filesystem path. Storage remains host-owned and independent of workspace execution.

A dispatched call gets the journal's session-scoped occurrence key, not a reusable provider tool-call ID. A lost or oversized mutation response produces `environment outcome unknown`; later calls rejected by an already closed connection are definite failures, not dispatched effects. Tempest does not reconnect and resend a Never effect. On resume, an environment that explicitly negotiates durable lookup can supply the original completed receipt, which Tempest commits before using it in the transcript. Missing, incomplete, or unavailable receipts remain unknown without replay. The bundled daemon does not advertise lookup or deduplication.

Cancellation is separate. Sending a cancel request, closing a pipe, or killing the local ssh process does not prove that remote work stopped. Only a positive daemon acknowledgment confirms cancellation. Failed cancellation remains unconfirmed, and a disconnected dispatched effect stays unknown.

## Supported services

The bundled remote daemon offers:

- The existing bounded file reader, directory listing, conditional edit, write, and atomic hashline batches.
- Literal and regular-expression search, plus filename search.
- Shell commands and direct argv execution.
- Bounded command output, with output artifacts read through the same remote lease.
- Byte-range UTF-8 reads and portable stat metadata for extension consumers.
- Workspace-host permission inspection. The host still applies tool tiers, plan mode, deny rules, approval policy, and child ceilings.
- Acknowledged cancellation that joins the selected invocation.

Local uses the existing `os.Root` containment, hashline snapshots, mutation queues, process groups, and edit transaction machinery. The daemon binds those same implementations. It does not implement a second editor or search convention.

Environment streams negotiate an 8 MiB frame limit so bounded image reads retain the same image-size support as Local. Ordinary tool results remain bounded to 1 MiB; edit summaries bound their diff and patch details before transport.

The bundled daemon does not offer checkpoint or rewind, eval kernels, LSP, Git isolation, AST tools, or a local debugger. Unsupported workspace services fail explicitly. Remote sessions with host MCP servers configured fail with a clear capability error; an `env://` journal location is never used as an MCP process directory. Remote general children are rejected because they need Git isolation. Explore children inherit the parent's remote workspace and permission ceiling.

Remote sessions, including CLI resume without an environment flag, do not discover the harness host's project rules, project providers, or project hooks. Command hooks are rejected rather than run against a local substitute cwd. Explicit host-owned `skill://`, `job://`, `history://`, and `local://` handles remain host resources. Progress and spilled MCP results use `local://` handles. Do not mix host handles and remote workspace paths in one read or search batch. Internal host URLs cannot expand into remote shell arguments or hashline paths; ordinary URLs such as `https://` and `postgres://` remain valid shell arguments and edit content.

File containment is not a command sandbox. Authorized shell or argv execution has the daemon operator's authority on that host. Tempest does not provision VMs, containers, credentials, or remote accounts.

Standalone and daemon output artifacts belong to their lease and disappear on close. Multi-file rollback records live separately under the daemon user's configuration directory, in `harness/environments/<workspace-root-digest>`, independent of the daemon name or generation. Daemons sharing a workspace serialize file mutations through process-shared file locks. Startup recovers interrupted batches before accepting new workspace operations. A changed generation with retained batches, or retained recovery conflicts, blocks startup with their evidence available for operator attention.

## Go embedding

```go
resolver := tempest.ProcessEnvironmentResolver(map[string]tempest.EnvironmentProcess{
    "development": {Argv: []string{"ssh", "builder", "tempest", "env", "-id", "development", "-cwd", "/srv/project"}},
})

session, err := tempest.Open(ctx, tempest.Options{
    SessionDir: journalDir,
    Environment: environment.Reference{ID: "development", Cwd: "."},
    EnvironmentResolver: resolver,
    New: &tempest.NewSession{Model: "your-configured-model"},
})
```

Import `go.harness.dev/harness/tempest/environment` for the leaf contract. `NewLocalEnvironment` and `NewRemoteEnvironment` expose the adapters without opening a conversation. `Session.Environment()` returns the resolved reference. Session close owns and closes its lease. A resolver should return a separately owned lease for each session.

`HostOptions.EnvironmentResolver` supplies the same resolver to hosted conversations. `AgentOptions.Environment` or `NewSession.Environment` selects a registered name. ACP `session/new` selects it through `_meta.tempest.environment`, using the same reference fields. The client supplies identity, never executable launch authority.

## Acceptance tests

`TestEnvironmentConformance` runs the same file, search, edit, batch, output-paging, argv, cancellation, symlink, and identity checks against Local and a fake Remote stream. Wire tests cover a mutation with a lost response, explicitly negotiated lookup, and failed cancellation. SDK tests cover identity persistence, replacement rejection, and unknown Never outcomes. `TestRemoteEnvironmentBinaryNeverUsesHostWorkspace` builds the actual binary and launches a separate workspace daemon with a faux provider, without a live provider credential.
