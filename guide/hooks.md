# Lifecycle hooks

Hooks run shell commands at fixed points in a session. The command-hook wire format
follows [Claude Code hooks](https://code.claude.com/docs/en/hooks), so existing hook
scripts usually work unchanged.

## Configuration

Configure hooks in `~/.config/harness/settings.json`:

```json
{
  "hooks": {
    "PreToolUse": [{
      "matcher": "^(write|edit|bash)$",
      "hooks": [{"type": "command", "command": "$CLAUDE_PROJECT_DIR/check-tool.sh", "timeout": 60}]
    }]
  }
}
```

Commands receive JSON on stdin. Decisions come back two ways:

| Exit | Result |
|---|---|
| `2` | Denies the tool, with stderr fed back to the model. |
| `0` | May return `hookSpecificOutput.permissionDecision` (`allow`, `deny`, `ask`), `updatedInput`, or `additionalContext`. |

A hook `allow` never bypasses plan mode, explicit policy deny or prompt rules, or
approvals required by the current mode. It never makes bypass mode stricter.

Stop hooks can return `{"decision":"block","reason":"finish the checks"}` to
continue the run. `UserPromptSubmit` stdout or JSON context reaches the model before
its first request. Blocked prompt reasons are shown to the caller, not added to model
context. An interrupted queued prompt is visibly consumed as "prompt not submitted",
without retrying side effects or making the session unusable.

## Trust

Project hooks in `.harness/settings.json` and `.harness/settings.local.json`
**never run without explicit operator trust**. Use `-trust-project-hooks` for the
current workspace, or add its absolute path to user `trusted_hook_projects`.

User, trusted project, then local hooks run in file order: each project file's arrays
are appended per event to the hooks below it, and a project `null` is ignored, so a
project cannot replace or remove an operator's hooks. Project hooks are still a
settings layer: a profile or `-c` (`-c hooks=null`) above them replaces or unsets
them, managed requirements on `hooks` apply to the combined arrays, and
`tempest config show -key hooks` lists them with their file. Only exact key names
count; `Hooks` or `HOOKS` is an unknown key that never runs. `-no-hooks` or user
`"no_hooks": true` disables all hooks; project files cannot set `no_hooks`.
`tempest doctor` lists the commands without executing them and names dropped
untrusted project hooks in its config warnings. The SDK's `Options.Hooks` replaces a
session's hooks and is used as given.

The same trust admits the other executable project settings, such as `mcp_servers`,
`debug_adapters` and `permission_mode_default`; see
[Project settings and trust](configuration.md#project-settings-and-trust).

Model changes to hook-bearing settings require per-call approval, including bash file
targets and child patches applied by `integrate`, even in bypass mode.

## Events and lifetime

Supported events:

`SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `Stop`,
`SubagentStop`, `PreCompact`, `Notification`, `SessionEnd`.

The events are shared by CLI, ACP/serve and SDK. `SessionStart` and `SessionEnd` are
root-only; task children use `SubagentStop`, prompt and tool hooks.

Match actual **snake_case** tool names (`read`, `write`, `edit`, `bash`,
`web_search`, `manifest_diff`, `lsp_definition`, `eval`), not Claude's `Read` or
`Bash` names.

Hooks run in the workspace with `TEMPEST_PROJECT_DIR` and `CLAUDE_PROJECT_DIR`. They
default to 600 seconds (`UserPromptSubmit`: 30s; `SessionEnd`: 1.5s total) and have
process-group cancellation and bounded output.

Replay never runs shell hooks. Interrupted effects are not retried automatically. See
[sessions and durability](sessions-and-durability.md) for recovery rules.

[Guide index](README.md) · [Tempest](../README.md)
