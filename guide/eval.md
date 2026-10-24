# Persistent code kernels

`eval` executes one cell at a time in each language's session-owned process. State
survives between cells and between prompts, so a session can build up a value and
reuse it later.

```json
{"language":"py","code":"answer = 41"}
{"language":"py","code":"answer + 1"}
{"language":"js","code":"let answer = 41"}
{"language":"js","code":"answer + 1"}
```

## Runtimes

| Language | Runtime | Behavior |
|---|---|---|
| Python | `python3` and the standard library, not IPython | Returns the `repr` of a trailing expression. |
| JavaScript | A synchronous persistent VM under `node` (preferred) or `bun` | Exposes `require` and displays the last completion value. JS timers and Promise callbacks run between cells; background output may appear in a later result. |

Neither driver rewrites code or supports top-level `await`. Missing runtimes produce
an explicit tool error. Tempest itself still needs no interpreter to start or use its
other tools.

## Timeouts and resets

- `timeout` is seconds, default 30. `0` disables it.
- A timeout interrupts the cell, keeping variables and effects that happened before
  the interruption.
- SIGINT targets only the kernel, not subprocesses started by earlier cells.
- An uncooperative cell or a broken protocol or output stream kills the process group
  after a bounded grace. The result reports unknown outcome and lost state.
- Prefer Node for interruptible JS. Bun versions without VM interruption (including
  1.3.14) require the hard-kill fallback on a CPU-bound timeout and lose state.
- `reset:true` replaces only the selected language's process before running the
  supplied code.

Output keeps a bounded tail and spills the full stream under
`<journal>.d/tool-output/`, like `bash`. A 16 MiB total-output ceiling stops runaway
writers.

## Lifetime

Kernels live across prompts, but not across session close, resume or rewind.

- After resume, previously used languages require `reset:true` and setup again.
- Unused languages and SDK-seeded conversations start normally.
- Rewind stops existing kernels before restoring files and requires the same explicit
  reset.
- An interrupted cell is never replayed automatically: it may already have performed
  side effects.

CLI, ACP/serve and the Go SDK use the same stack and lifetime. General task children
have separate kernels. `eval` is exec-tier and unavailable in plan mode.

Cells are foreground journaled tools, not background `jobs`, and idle kernels do not
prevent a run from completing. Session close kills and joins their processes and
descendants, and a parent-lifetime pipe prevents orphan kernels after a harness crash.

## Safety

Eval code uses the same destructive-operation scan and cloud policy as inline Python
and JS in `bash`. Matching filesystem operations ask on every call in `default` and
`acceptEdits` mode. Code naming a cloud tool is opaque, so a deny rule for that tool
wins even in bypass mode, and allow-write rules cannot grant opaque code. These are
heuristic approval guards, not a sandbox for arbitrary interpreter code.

## Images

For matplotlib figures or PNG bytes, save an image inside the workspace and call
`view_image`. This keeps image validation, vision-model gating and provider delivery
in the existing image tool, without importing matplotlib or implicitly inspecting
every cell value. See [permissions](permissions.md) for the shared approval gate.

[Guide index](README.md) · [Tempest](../README.md)
