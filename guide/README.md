# Tempest guide

Start with the [project README](../README.md) for installation and a first run.
These pages hold the command, tool, and protocol reference.

## Using Tempest

| Page | What it covers |
|---|---|
| [Providers](providers.md) | Routes, the endpoint registry, presets, protocol adapters, credentials, caching, costs, and doctor |
| [Configuration](configuration.md) | Flags, environment variables, settings, precedence, and model roles |
| [Tools](tools.md) | Hashline patches, atomic edits, reads, search, tool inventory, and internal URLs |
| [Sessions and durability](sessions-and-durability.md) | Journals, recovery, retries, budgets, stop reasons, structured output, and streaming |
| [Permissions](permissions.md) | Tool tiers, plan mode, destructive-command guard, and cloud policies |
| [Checkpoints](checkpoints.md) | Captures, rewind, protected paths, retention, and recovery bounds |
| [Skills and context](skills-and-context.md) | `AGENTS.md`, `RULES.md`, skill format, discovery, and imports |

## Extending your workflow

| Page | What it covers |
|---|---|
| [Subagents and jobs](subagents.md) | Worktrees, three-way integration, messages, services, limits, stdin, and PTYs |
| [Hooks](hooks.md) | Claude Code-compatible command hooks, trust, events, permissions, and lifetime |
| [Extensions](extensions.md) | Named Go and subprocess bundles, prompt sections, typed hooks, memos, replay policy, and hot installation |
| [Durable tasks](tasks.md) | Persisted phases, timers, ownership, cancellation, migrations, alarms, and subprocess workflows |
| [Code intelligence](code-intelligence.md) | Language servers, rename plans, diagnostic freshness, and Kubernetes manifest diffs |
| [Eval](eval.md) | Persistent Python and JavaScript kernels, runtime requirements, resets, and side effects |

## Integrating and developing

| Page | What it covers |
|---|---|
| [Servers and protocols](serve-and-protocols.md) | MCP client/server, ACP, A2A 1.0, authentication, progress, approval, and recovery |
| [Go SDK](sdk.md) | In-process sessions, watches, prompts, cancellation, and the embedding example |
| [Testing](testing.md) | Faux provider scripts, provider conformance, build commands, smoke checks, and the codebase tour |

Protocol examples use **camelCase** keys. Model tool arguments and results use
**snake_case** keys. Each page states its own limits and trust boundaries.
