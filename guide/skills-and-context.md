# Skills and context

## Context discovery

At startup the agent walks `AGENTS.md` up from your working directory, loads a
sticky `RULES.md` that survives compaction, expands safe `@file` imports, and
registers skills from your agent directory, the project and `-skill` paths. Skill
bodies load on demand through the `skill` tool or a `skill://` read, so nothing
bloats the prompt until it is needed.

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
