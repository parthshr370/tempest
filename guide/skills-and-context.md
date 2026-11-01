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
  relative to the working directory. Outside git it is a directory walk that
  skips `.git` and honours the root `.gitignore`. Both stop reading after
  `workspace_listing_max_files` paths (default 10000). Listing gets 2 seconds:
  a walk that runs out of time lists what it found, and a slower `git` leaves
  the session without a listing.
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
  never list again, so the cached prompt prefix does not change. Delegated
  children reuse their parent's listing. A fork into another working directory
  or environment lists its own.
- **Remote environments.** A remote workspace runs the same `git ls-files`
  through the environment's exec service in one call (at most 64 KiB of
  output). Without exec, or outside git, a remote session starts without a
  listing.
- **Off switch.** Set `"workspace_listing": false` in settings (or
  `HARNESS_WORKSPACE_LISTING=false`, or `-c workspace_listing=false`) to stop new
  sessions from taking one. A session that already has a listing keeps it.
  `tempest doctor` shows the current policy and `tempest config show` the layer
  that set it.

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
