# Skills and context

## Context discovery

At startup the agent walks up from your working directory to the repository root
and loads one context file per directory, loads a sticky `RULES.md` that survives
compaction, expands safe `@file` imports, and registers skills from your agent
directory, the project and `-skill` paths. Skill bodies load on demand through the
`skill` tool or a `skill://` read, so nothing bloats the prompt until it is needed.
A resumed session (`-resume`) runs in the working directory it was created in, so
discovery walks up from there, not from the directory you resume it from.

In each directory the first of these files that exists and is not blank loads:

1. `AGENTS.override.md`
2. `AGENTS.md`
3. each name in `context_files.fallback`, by default `["CLAUDE.md"]`

So a personal `AGENTS.override.md` shadows the checked-in `AGENTS.md`, and a
repository that only has `CLAUDE.md` still gets its instructions. Directories
whose name starts with `.` are skipped. The nearest `.harness/AGENTS.md` keeps
its place: it replaces the standalone file of its own directory. At user level,
`<agent-dir>/AGENTS.override.md` wins over `<agent-dir>/AGENTS.md`; fallback names
do not apply there.

Files load user first, then repository root down to the working directory.
`@file` imports expand in every kind of context file, including `CLAUDE.md`,
and in `RULES.md`. Each file and its imports are read inside one root:

- **Repository files** (context files and `.harness/RULES.md`): the repository
  root, the directory startup walks up to (the git root, else your home
  directory when the working directory is under it, else the filesystem root).
  A file that is a symlink pointing outside it is skipped with a warning. An
  import that is a symlink pointing outside it, a `../` or absolute import
  outside it, and a `~/` import stay literal. A repository file never pulls a
  file from outside the repository into the prompt.
- **Your own files** (`<agent-dir>/AGENTS.override.md`, `AGENTS.md` and
  `RULES.md`): your home directory, or the agent dir itself when it is outside
  home. `~/` imports resolve against home; an import outside the root stays
  literal.

A context file that is not a regular file (a directory or FIFO named
`AGENTS.md`) stops startup with an error instead of blocking.

The loaded files share one budget, `context_files.max_bytes` (default 65536
bytes, counted after imports expand). The file that crosses it is cut and ends
with a `[harness: context files exceed context_files.max_bytes ...]` notice that
names the cut and every file left out, so the model knows its instructions are
incomplete. The budget bounds the work as well: each context file is read only
up to `context_files.max_bytes` bytes plus one, the extra byte telling whether it
is cut, and its imports only up to as many again, however large the files are. `RULES.md` has no byte cap. `harness doctor` lists
the files a run in the current directory would load, their sizes, and the
notice if any.

```json
{
  "context_files": {
    "fallback": ["CLAUDE.md", "GEMINI.md"],
    "max_bytes": 65536,
    "nested_max_bytes": 16384,
    "nested": true
  }
}
```

### Nested context files

Startup only covers the working directory and its ancestors. When `read` or `ls`
succeeds on a path below the working directory, the agent checks each directory
between the working directory and that path, shallowest first, and surfaces the
first context file it finds there (same name order as above). Reading
`a/b/x.go` shows `a/AGENTS.md`, then `a/b/CLAUDE.md`.

- The file is appended after that tool result, inside a
  `<repository_context path="a/AGENTS.md">` frame that marks it as repository
  data: it refines shallower instructions for files under its directory, and the
  user's requests win over it. The system prompt never changes, so the provider's
  prompt cache keeps working.
- A directory surfaces once while the conversation holds the tool result that
  carried its file, with that file's frame still in it. Another result with
  the same call ID and turn time does not count. A rewind or reset to before
  that result, a fork made before it, or a compaction that summarizes it away
  lets the file surface again on the next read there; a fork made after it
  keeps it. The record is written
  into the same journal entry as the tool result, so a resumed session neither
  shows the file again nor spends its bytes twice, and a crash before that
  result was saved leaves no record: the resumed session shows it again.
- Each file is cut at `context_files.nested_max_bytes` (default 16384 bytes),
  and nested files count against `context_files.max_bytes` together with the
  startup files. A cut file ends with a `[harness: cut at ...]` notice. The work
  follows the same budget: a file is read only up to the bytes it may keep plus
  one, the extra byte telling whether it is cut, and its imports only up to as
  many again. Once the total is spent, the next file
  is named but not loaded, and no further directory is looked at.
- Files are read inside the working directory, imports included. A context
  file or an `@file` import that is a symlink pointing outside it, a `../` or
  absolute import outside it, and a `~/` import stay unread, so a context file
  never shows what `read` itself may not. Only regular files are opened; a FIFO
  is skipped.
- `@file` imports expand; directories whose name starts with `.` are skipped.
  Reading a context file whole does not repeat it after the result; a read of
  part of it (a line range, a limit, a page cut by the read budget), or of a
  file whose imports the read did not expand, still surfaces the file.
- Only `read` (files, directories, batches) and `ls` trigger it; `grep` and
  `find` results do not. It runs only for a workspace on this host, not a remote
  environment. Set `context_files.nested` to `false` to turn it off.

## Workspace listing

A new session lists its working directory once, the way you run `ls` after
`cd`, so the model can open the files a task needs without spending its first
turns on `ls` and `find`.

- **Source.** In a git work tree the listing is `git ls-files --cached --others
  --exclude-standard`: tracked files plus untracked files that are not ignored,
  relative to the working directory. `git` runs with every inherited `GIT_*`
  variable removed and `core.fsmonitor` off, so it reads the working directory's
  own repository and runs no configured program. Outside git it is `rg --files
  --hidden --no-require-git` when `rg` is installed (with `--no-config`,
  `--no-ignore-parent` and `--no-ignore-global`), and otherwise a directory
  walk. Both list hidden files, skip `.git`, and apply every `.ignore` and
  `.gitignore` in the tree the way rg does: each file's patterns apply in its
  own directory and below, a matching `.ignore` pattern outranks every
  `.gitignore`, and for each name the deepest file with a matching pattern
  decides. The walk matches patterns with a port of git 2.54's own matcher
  (`wildmatch.c` and the exclude rules in `dir.c`), so a `.gitignore` excludes
  exactly what `git ls-files --others --exclude-standard` would: the last
  matching pattern wins, `!` re-includes a file but nothing under an excluded
  directory, `/` anchors, and `**`, character classes, escapes and trailing
  spaces follow git. rg uses its own glob matcher, which can differ from git
  on unusual patterns, and it does not name symbolic links; the walk names
  regular files and symbolic links, as git does. Neither reads
  `.git/info/exclude` or `core.excludesFile`. The walk opens only a regular
  ignore file (never a FIFO or a symbolic link) of at most 1 MiB; a larger one
  is skipped. All three stop reading after `workspace_listing_max_files`
  paths (default 10000). Listing gets 2 seconds, a hard limit: rg or a walk
  that runs out of time lists what it found, matching included, and a slower
  `git` leaves the session without a listing. rg blocks on a FIFO named
  `.ignore` or `.gitignore`, so when rg has listed no name by half the time
  left, the walk starts beside it: whichever finishes first is the listing, and
  at the deadline rg's names are, or the walk's when rg has none. The loser is
  stopped, and the listing waits for a stopped walk to end, at most until the
  deadline. A remote
  environment lists its own workspace with the same code in 1.5 of the 2
  seconds, leaving the rest for its reply to arrive.
- **Size.** The listed names fit `workspace_listing_max_bytes` (default 4096).
  Past it the listing folds to per-directory file counts (`internal/tools/ (40
  files)`); if that still does not fit it is cut, and a final
  `[truncated: … use find or ls]` line says so.
- **Placement.** The listing goes at the start of the first user message in a
  `<workspace_files>` block marked as repository data, not in the system
  prompt. File names are untrusted repository content, and the system prompt
  stays the same in every repository. The transcript keeps your message as you
  sent it; the block is added to each provider request.
- **Snapshot.** The listing is taken when the session is created and journaled
  with the session. Resume and forks of the same workspace reuse those bytes and
  never list again, so the cached prompt prefix does not change. A fork into
  another working directory or environment lists its own.
- **Delegated children.** A child that works in its parent's directory (an
  `explore` child, or a `general` child outside git) sends its parent's
  listing bytes. A `general` child in its own git worktree lists that worktree
  when it starts, so it sees files committed after the parent's listing, and
  journals the bytes in its own journal; a resumed child sends them again.
- **Remote environments.** The environment takes the listing on its own host
  with the same code, in one `workspace_listing` call of its search service:
  `git` there gets the same environment clean-up, and a workspace outside git
  gets the same walk. One reply carries at most 64 KiB of names and says when
  there are more. An environment without the search service, or one that does
  not know the call, leaves the session without a listing.
- **Off switch.** Set `"workspace_listing": false` in settings (or
  `HARNESS_WORKSPACE_LISTING=false`, or `-c workspace_listing=false`) to stop new
  sessions from taking one. A session that already has a listing keeps it.
  `tempest doctor` shows the current policy and `tempest config show` the layer
  that set it. A repository's own settings may turn the listing off or lower its
  caps, never raise them above yours without trust; see
  [Configuration](configuration.md#project-settings-and-trust).

## Skill format

A skill is a markdown file with YAML frontmatter between `---` lines at the very
top. The body below it is what the `skill` tool returns:

```markdown
---
name: deploy-app
description: Deploy the app to staging. Use when asked to ship or deploy.
---
# Deploy
Run `make deploy` and report the URL it prints.
```

| Key | Rules |
|---|---|
| `description` | Required, at most 1024 bytes. The system prompt lists each skill's name and description so the model knows when to load it. |
| `name` | Defaults to the name of the directory holding the file. Lowercase `a-z`, `0-9` and single hyphens, at most 64 characters, and must not start or end with a hyphen. |
| `disable-model-invocation: true` (or `hide: true`) | Leaves the skill out of the prompt list; `skill` and `skill://<name>` still load it by name. |

`when_to_use`, `allowed-tools`, `model`, `context` and `paths` are accepted for
compatibility and currently have no effect. Other keys are ignored.

## Discovery

Skills load from three places, in order:

1. `<agent-dir>/skills`, by default `~/.config/harness/agent/skills`.
2. `.harness/skills` in the working directory.
3. Each `-skill` path, or the settings `skill_paths` list when no `-skill` flag is
   given.

A `-skill` path may be a directory or a single `.md` file. In each directory:

- Every `.md` file directly inside it is a skill.
- Below that, only files named `SKILL.md` count. A directory holding a `SKILL.md`
  is one skill; its subdirectories are not searched.
- Hidden entries, `node_modules` and paths matched by `.gitignore`, `.ignore` or
  `.fdignore` are skipped.

A file with a missing description or an invalid name is skipped with a
`discovery diagnostic` warning on stderr. When two skills share a name, the first
one loaded wins and the collision is logged.

## Access

`skill://<name>/<path>` reads other files from the skill's directory and cannot
leave it.

[Guide index](README.md) · [Tempest](../README.md)
