# Tools

Tempest ships built-in coding tools in one namespace. Pin the
active set with `-exclude-tools` and enable the optional groups with flags:
`-enable-task`, `-enable-web`, `-enable-lsp`, `-attach`. All file tools are contained
to the working directory through `os.Root`. `grep` and `find` fall back to pure Go
when `rg` and `fd` are absent. Both search hidden files, honor `.gitignore` inside a
Git repository, and skip `.git` unless you point them at it.

The one read-only exception to containment: `read` may also open this session's own
job artifacts (`<journal>.d/jobs/<job>/` `changes.patch`, `result.txt`, `output.log`),
its `progress.md` checkpoint, and spilled tool output under
`<journal>.d/tool-output/`. Spilled output includes the "Full output: <path>" of
truncated bash output, tool results cleared from context, and oversized MCP results.
Child journals, worktrees and every other path outside the workspace stay refused,
except through the internal URLs below, of which `local://` is writable. Spilled
output lives as long as the session's files do.

Model tools use **snake_case** keys in arguments and results (`job_id`, `job_ids`,
`subagent_type`, `old_text`, `ignore_case`). ACP and `tempest/*` wire methods use
**camelCase** (`jobId`, `jobIds`).

## Hashline edits

`read` and `grep` label every file with a 4-hex content tag, `[path#TAG]`, and
`LINE:TEXT` rows. `hashline` edits by those line numbers instead of retyping old
text, so whitespace battles and "string not found" loops stop:

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

| Operation | Effect |
|---|---|
| `PUT N.=M:` | Replace lines N through M. |
| `PUT <N:` / `PUT >N:` | Insert before line N or after line N. `<1` is the head, `>$` the tail. |
| `CUT` | Delete and capture into a register that `PUT … @name` pastes. Named registers persist across calls; the unlabeled one lasts one call. |
| `REM` | Delete the file. |
| `MV DEST` | Rename the file. |

`N*` targets the construct that opens on line N without counting its lines. Go uses
`go/parser`, Markdown uses sections and code fences, brace matching covers JS/TS,
Java, C/C++, Rust, C#, Kotlin, Swift, PHP, Scala and JSON, and indentation covers
Python and YAML. Other files use explicit ranges.

Line numbers name the file as read and are never shifted by earlier hunks. A section
only applies if its tag names a version this session saw whose full SHA-256 matches
the live file, so a 16-bit tag collision is rejected. A tag from before a resume
applies if the live content hashes to it. Hunks may touch only lines a `read`, `grep`
or edit result actually displayed under that tag, and hunks may not overlap. Every
section of a patch is validated before any file is written.

Each result returns the new tag and a numbered preview of the changed lines, so the
next edit can chain from it. A stale tag is rejected with the current tag and the
rows around the lines you named. LF/CRLF endings and a UTF-8 BOM are preserved. `edit`
(exact-text replacement) accepts the same 4-hex tag as its anchor, as well as the full
anchor an `lsp_rename` plan carries.

## Atomic writes and rollback

Every write is **atomic**. `write`, `edit` and `hashline` stage the new bytes in a
hidden file beside the target, sync it, rename it over the target, and sync the
directory. A crash leaves the old file or the new one, never a truncated mix. The
file keeps its permission bits, and a hard link to it is not followed.

A patch that writes more than one path (several sections, `MV`, `REM`, or an
`integrate`) is a **batch under a rollback journal**, following the discipline of
[SQLite's atomic commit](https://sqlite.org/atomiccommit.html):

1. Before the first write, every path's prior existence, bytes and mode are synced to
   `<journal>.d/edit-rollback/<batch>/` under an `flock`.
2. Deleting the record is the commit.
3. A write that fails mid-batch rolls the written paths back before the tool returns.
4. A process killed mid-batch leaves the record. The next session open in that
   workspace (a resume, or any new session, CLI, `serve` or `mcp`) restores each path
   whose content is still the batch's pre or post state.
5. A path someone changed since is reported as a conflict and left alone. Its
   pre-batch copy stays under `edit-rollback/conflicts/<batch>/prior/`, and one
   diagnostic per batch appears in the session snapshot and log.
6. A batch whose owner is still running holds its lock and is never touched.

This is crash recovery, not isolation: other processes can see intermediate files
while a batch runs.

## Reading files

Point `read` at a file and it returns numbered content whenever the whole file fits
one read (**2000 lines and 50KB**). Every row it shows is editable with `hashline`. A
Go file too large for that returns a structural outline instead: declarations without
bodies, each with its `path:START-END` range, headed by a note saying it is an outline
and how to read a range. The agent can then pick the lines it needs rather than page
blind. Any other large file returns its first page and the offset to continue.
`mode:"outline"` asks for the outline of a Go file of any size; `mode:"raw"` always
pages numbered rows.

**Several files, one call.** `paths` takes the place of `path` when the agent knows
several files it needs:

```json
{"paths":["go.mod","internal/a.go:40-90","README.md:1-30"]}
```

This returns one `[path#TAG]` section per entry, in order, each recording the rows it
showed for `hashline`. The sections share one 2000-line/50KB budget. A file the budget
cuts short ends with the `path:START-END` that continues it; a file the budget leaves
no room for is named, not read. A failing entry reports its error in place of its
section. `offset` and `limit` page a single `path` only. `grep` takes `paths` the same
way, with one shared match `limit`.

The system prompt tells the model to batch known reads this way and to put independent
tool calls in one response: the opening look at a task, searches, and edits or writes
to different files. The agent loop already runs those concurrently.

`read` takes more than files. Every text output below starts with the `[path#TAG]`
header and accepts the same `path:START-END` / `path:START+COUNT` line ranges and
`offset`/`limit` paging:

| Path | Output |
|---|---|
| `src/` | The directory's entries, directories with a trailing `/` (500 at most). |
| `bundle.zip`, `.tar`, `.tar.gz`, `.tgz` | The archive's members with sizes (1000 at most). |
| `bundle.zip:dir/file.txt` | One member, read in memory (16 MiB cap, nothing extracted to disk). `bundle.zip:dir` lists a folder. |
| `report.docx`, `.xlsx`, `.pptx` | The document's extracted text. |
| `package.json?q=.scripts \| keys` | A JSON query, printed as indented JSON. |
| `paper.pdf` | PDF text through `pdftotext -layout`. Install Poppler if the command is absent. |
| `app.db`, `.db3`, `.sqlite`, `.sqlite3` | SQLite tables and their definitions. |
| `app.db:items` | The table's schema and first 20 rows. `?limit=20&offset=20` selects a page. |
| `app.db?q=SELECT name FROM items` | A read-only SQL query, capped at 1000 rows. Put optional paging before `q`: `?limit=20&offset=20&q=SELECT...`. |

The JSON query is a small jq subset implemented in-house: `.` (identity), `.a.b`,
`."any key"` and `.["any key"]`, `.[0]` and `.[-1]`, `.[]` (array elements or object
values), `?` after any step to skip values of the wrong type instead of failing
(`.items[]?.name?`), `keys`, `length`, and `|` pipes. Object keys print sorted.
Queries combine with members (`bundle.zip:data.json?q=.a`).

SQLite reads use a private copy of the contained database and its WAL, so reading
cannot create sidecars or change the source. Do not write the database while taking
the copy. Queries must be a single `SELECT` or read-only `WITH ... SELECT`.
Write statements, shell commands and external file functions are refused.
The CLI also runs with `-readonly`, `-safe`, `query_only` and `trusted_schema=OFF`.
Everything after `q=` is raw SQL: `+`, `&` and `%` keep their SQL meaning.
PDF, database and WAL inputs are capped at 32 MiB each before conversion/copying.
PDF text and SQLite results use ordinary line selectors and `offset`/`limit` paging.
Missing readers report the required executable instead of showing binary data.

## Internal URLs

`read` resolves the following schemes. The four file and session schemes also work
with `grep`:

| URL | Resolves to |
|---|---|
| `skill://<name>` | The skill's `SKILL.md`. `skill://<name>/` lists the skill's directory and `skill://<name>/<path>` reads a file beside it. |
| `job://` | The session tree's background jobs, one per line. |
| `job://<job_id>` | One job's record: state, exit code, command or task, artifacts. `job://<job_id>/` lists its files. |
| `job://<job_id>/<file>` | One artifact: `output.log`, `result.txt`, `changes.patch`. |
| `history://` | The child sessions, with their job ids. |
| `history://<id>` | A child's transcript rendered as text, by task job id or child session id. |
| `local://<path>` | A private scratch directory, `<journal>.d/local/`, shared by the root session and its children. |
| `issue://<number>`, `pr://<number>` | A GitHub issue or pull request in the caller's workspace repository. `owner/repo/number` selects another repository. |
| `issue://`, `pr://owner/repo` | Recent GitHub items. Listings accept `state`, `limit` (1 to 100), `author` and `label` query parameters. |
| `pr://<number>/diff` | Changed files, numbered from one. Append `/1` for that file's diff or `/all` for the full diff. Line selectors page each result. |

`local://` is the one writable scheme: `write` and `edit` accept it, so an agent hands
a large payload to a child (or back to its parent) by writing `local://plan.md` and
passing the URL instead of pasting the text.

Every file-backed scheme is rooted. `..` is refused, file access goes through an `os.Root` on the
scheme's directory, and a symlink that leads outside it is refused. A child resolves
`local://`, `job://` and `history://` against its root session, so a session tree
shares one namespace and other sessions stay out of reach. `job://`, `history://` and
`local://` need a journaled session. `?`, `#` and `%` in names are percent-encoded, and
so are spaces inside `bash` commands (`local://my%20notes.md`).

In `bash`, a `skill://` or `local://` word is rewritten to its quoted absolute path
before the command runs (`cat skill://deploy/SKILL.md`,
`python3 local://extract.py > local://out.json`; a bare `skill://<name>` is the
skill's directory). Only whole words expand, bare or quoted in full. A URL inside
longer quoted text or glued to other characters stays as written. The rewrite happens
before the permission gate, so the destructive-command guard and the cloud tiers judge
the real paths. A word that cannot resolve fails the call with the reason instead of
running.

## Structural search and rewrites

`ast_grep` and `ast_edit` register only when `ast-grep` or its `sg` alias is on PATH.
`tempest doctor` reports why they are absent. There is no text-search fallback.

```json
{"pat":"oldApi($$$ARGS)","path":"src/**/*.ts"}
```

`ast_grep` returns 50 matches per call. Use `skip` to continue. File extensions
select the language; `lang` overrides it. Paths may name files, directories or globs.
Search scopes can be separated by semicolons.

```json
{"ops":[{"pat":"oldApi($$$ARGS)","out":"newApi($$$ARGS)"}],"paths":["src/**/*.ts"]}
```

The first `ast_edit` call writes nothing. It shows a unified diff and a `preview`
token. Repeat the request with `"apply":true` and that token to commit. Changed
source or arguments invalidate it. Operations run in order. Empty `out` deletes
matches. Overlapping matches are refused. Narrow the pattern before retrying.
Rewrite scopes are bounded to 256 files and 16 MiB of source per call.
Search scans files independently and retains only the requested 50-match page;
it does not inherit the rewrite snapshot limits. Gitignored directories are pruned.

The binary reads source through stdin in a private directory. It never receives
workspace paths or loads workspace parser configuration. Symlinks discovered
during directory walks are skipped; explicitly named scopes follow links only
inside the workspace.
Writes use the same mutation locks, atomic writes, multi-file rollback journal,
hook-settings approval and checkpoint path as ordinary edits.
General children inherit these tools only when their parent allows them.
Plan and scout sessions get structural search, not structural edits.
Previews are read-tier calls and do not capture checkpoints. Large structural
results and GitHub output are bounded like bash, with full text spilled into the
session's `tool-output` artifacts (the path is returned for `read`).

The JSON byte ranges and replacement offsets follow the
[ast-grep JSON protocol](https://ast-grep.github.io/guide/tools/json.html).

## GitHub

`gh` is a read-tier tool. Operations are `issue_view`, `pr_view`, `pr_diff`,
`repo_view`, `file_read`, `search_issues`, `search_prs`, `search_code`,
`search_commits`, `search_repos`, `run_watch` and `run_logs`.
Pass `repo:"owner/repo"` or use the workspace repository. `number` and `run` are
positive decimal strings. Search takes `query` and an optional `limit` (1 to 100).
Repository search is global. Other searches use the workspace repository when no
explicit `repo` or query scope (`repo:`, `org:`, `user:`, `owner:`) is given.
`file_read` takes a repository-relative `path` and optional `branch`.
`run_watch` polls until completion, up to `timeout` seconds (1 to 300, default 30).
`run_logs` returns failed-job logs.

`gh_checkout` requires exec permission and is absent from all child, plan and scout sessions.
Pass `number`, a new workspace `path`, and optional `repo`. It fetches the numbered
PR head, verifies its SHA, and creates a detached Git worktree. It does not switch
or overwrite the caller's checkout. Git hooks are disabled for worktree creation.
Existing destinations, paths outside the workspace, and paths inside Git metadata
directories (including filesystem aliases) are refused. The fetched tree's
would-be paths pass through hook-settings approval before checkout. Fetching uses
the repository's actual host and `gh auth git-credential`, with terminal prompts
disabled and a separate five-minute budget. A failed checkout removes partial
worktrees, their registrations, and parents created by the call.
Git worktree metadata is not checkpointed: successful checkout invalidates older
rewind targets rather than pretending rewind can remove the registration. Use
`git worktree remove` to remove the worktree.

These tools require the GitHub CLI. Authentication failures say to run
`gh auth login`. JSON flags are used where the CLI supports them. Diffs and logs
use text because those subcommands have no JSON mode.
Search uses [`gh api --method GET`](https://cli.github.com/manual/gh_api), preserving
multi-word queries as one field.

## Tool reference

### Files and search

- `read`: files, directories, archives and their members, office documents, PDF text, SQLite, JSON queries, and internal URLs (`skill://`, `job://`, `history://`, `local://`, `issue://`, `pr://`). Numbered content when the file fits one read, a structural outline for a Go file that does not. `paths` reads up to 20 files in one call, each with its own selector.
- `write`: create or overwrite a file, or a `local://` scratch file.
- `edit`: exact-text replacement, guarded by the `[path#TAG]` tag (or an `lsp_rename` anchor). Also edits `local://` files.
- `hashline`: line-anchored `PUT`/`CUT`/`REM`/`MV` patches over one or many files, with block targets and clipboard registers. See [Hashline edits](#hashline-edits).
- `grep`: regex search across the tree, under up to 20 `paths` at once, or under an internal URL.
- `find`: glob-based path lookup.
- `ls`: directory listing.
- `ast_grep`: structural workspace search, when ast-grep is installed.
- `ast_edit`: preview and apply structural rewrites, when ast-grep is installed.
- `gh`: read GitHub issues, PRs, files, search and Actions output.
- `gh_checkout`: check out a PR in a new workspace worktree.

### Runtime

- `bash`: workspace shell with a destructive-command guard and a default ten-minute timeout. `skill://` and `local://` words expand to paths. `name` (or `async:true` for an unnamed job) starts a supervised background job with stdin from `/dev/null`, optionally a service that returns once `ready_log`/`ready_port` holds. `stdin:true` keeps a stdin pipe open for `write_stdin`, and `pty:true` runs the job on a pseudo-terminal. See [Subagents and jobs](subagents.md).
- `eval`: persistent per-session Python/JavaScript code cells, with captured output, tracebacks, interruptible timeouts and explicit reset. Requires `python3`, or `node`/`bun` for JS, only when called. See [Eval](eval.md).
- `jobs`: page durable job summaries (with job name and service readiness), or explicitly clean one exported worktree.
- `wait`: wait for any requested job, by id or background bash job name, without cancelling it on wait timeout.
- `cancel`: cancel one task or background bash job, by id or name.
- `logs`: page a background command's bounded output (stdout and stderr in write order) by byte offset, by id or name.
- `write_stdin`: type into a running background job (text, Ctrl-C, end of input) and get back the output it printed since the last call, by id or name.

### Coordination

- `task`: admit asynchronous children and return job handles *(behind `-enable-task`)*.
- `integrate`: three-way merge a finished general task's patch into the live workspace. Per file the result is clean, merged or conflict (with markers). It writes only when nothing conflicts and the live files still match the snapshot, and merged files only with `accept_merged:true` *(behind `-enable-task`; plan mode blocks it)*.
- `message`: message the parent, a running child, or all peers, optionally awaiting the first reply *(behind `-enable-task`; children get it too)*.
- `todo_write`: the task list, replaced in full (`todos`) or edited by one op naming todos by content (`init`, `append`, `start`, `done`, `block` with a reason, `drop`), with optional phases. Checkpointed to `progress.md` in the session's data directory (`<journal>.d/`), never in your workspace. The result shows the updated list.
- `checkpoint`: save workspace bytes, modes and the conversation before an investigation. Returns a retained checkpoint id.
- `rewind`: restore checkpoint files and fork the conversation, retaining findings. Defaults to the newest explicit checkpoint. Outside edits need operator force, capture gaps cannot be bypassed, and running jobs must settle first.

### Context

- `skill`: load a skill body on demand when skills are loaded (also via `skill://`).
- `attachment`: read resolved session attachments *(behind `-attach`)*.

### Web *(behind `-enable-web`)*

- `web_search`: one query against the configured search endpoint.
- `web_fetch`: fetch a URL as bounded, structured text.

### Code and manifest intelligence

- `manifest_diff`: compare two rendered Kubernetes manifest snapshots resource by resource. Never runs a renderer.
- `view_image`: show a PNG, JPEG, GIF or WebP file inside the working directory to the model as an image (at most 3,932,160 bytes, 5 MB once base64-encoded, and 8000 px per side). It refuses when the model has no image input in the catalog. Anthropic gets the image inside the `tool_result`, Gemini 3 inside the `functionResponse`, and OpenAI-compatible routes as a user message right after the tool messages, since chat-completions tool messages carry text only.
- `lsp_definition`, `lsp_references`: semantic navigation through a language server *(behind `-enable-lsp`)*.
- `lsp_rename`: a rename **plan** of anchored edits for the ordinary `edit` tool; it writes nothing *(behind `-enable-lsp`)*.
- `lsp_diagnostics`: a file's diagnostics with an explicit freshness verdict *(behind `-enable-lsp`)*.

See [Code and manifest intelligence](code-intelligence.md) for the LSP and manifest
details.

[Guide index](README.md) · [Tempest](../README.md)
