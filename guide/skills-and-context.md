# Skills and context

## Context discovery

At startup the agent walks `AGENTS.md` up from your working directory, loads a
sticky `RULES.md` that survives compaction, expands safe `@file` imports, and
registers skills from your agent directory, the project and `-skill` paths. Skill
bodies load on demand through the `skill` tool or a `skill://` read, so nothing
bloats the prompt until it is needed.

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
  `.ignore` or `.gitignore`; an rg that has listed no name by half the time
  left is stopped, and the walk lists the workspace in the other half.
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
