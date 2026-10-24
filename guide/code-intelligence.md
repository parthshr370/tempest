# Code and manifest intelligence

Two optional tools look past plain text. Language servers answer semantic questions
about code, and `manifest_diff` compares rendered Kubernetes manifests without
running a renderer.

## Language servers

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

## Manifest diffs

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

[Guide index](README.md) · [Tempest](../README.md)
