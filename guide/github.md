# GitHub ticket runner

`tempest serve -github owner/repo -github-config /abs/file.json` runs a GitHub
ticket runner inside the normal serve process, beside ACP and A2A. It polls one
repository as a dedicated GitHub user, turns explicit `@mentions` from listed
people into **Tickets**, runs one worker session per Ticket in its own worktree,
and publishes the worker's comments, pushes and pull requests itself. Nobody's
comment is ever an instruction to merge: merging stays manual.

The control path has no model in it. Admission, Ticket state, scheduling, budgets
and completion are deterministic code over a durable control journal, so a
restart resumes from exactly what was committed.

## Read this first: trust model

**There is no sandbox in this build.** Workers are ordinary Tempest sessions:
they run `bash`, edit files and start processes as the OS user that runs
`serve`, limited only by their [permission mode](permissions.md). GitHub text
(issue bodies, comments, reviews, CI output) reaches the model as quoted data,
and a crafted comment can still steer a worker to run commands its permission
mode allows.

Run the runner only:

- on **trusted repositories**, where you would let the listed people run commands
  on the host through the agent;
- under a **dedicated OS account** that holds nothing else of value: no personal
  SSH keys, cloud credentials, other `gh` logins, browser profiles or other
  checkouts. Anything that account can read, a worker can read.

Workers hold **no GitHub write credential**. The dedicated account's `gh`
configuration directory is passed only to the controller's own `gh` and `git`
subprocesses. Workers ask for writes through the `github` extension's tools, and
the controller performs them. This is credential selection, not isolation: a
worker shell can still read files the OS account can read, which is why the
account must be dedicated. `serve` refuses to start when `GH_TOKEN`,
`GITHUB_TOKEN`, `GH_ENTERPRISE_TOKEN` or `GITHUB_ENTERPRISE_TOKEN` is set in its
environment, because worker shells would inherit it.

## Setup

1. **Create a dedicated GitHub user** (for example `acme-tempest`) and add it to
   the repository with write access. The runner checks `permissions.push` at
   startup. Note its numeric ID: `gh api users/acme-tempest --jq .id`.
2. **Log it in to its own `gh` configuration directory**, as the dedicated OS
   account:

   ```sh
   GH_CONFIG_DIR=/home/tempest/.config/gh-acme gh auth login
   ```

   The OAuth login's `repo` scope covers what the runner uses: notifications,
   issues, comments, pull requests, checks and pushes. Use the same `gh` binary
   you put in `account.ghPath`.
3. **Clone the repository** where the runner can branch worktrees from it. The
   configured remote URL (`git config remote.origin.url`, without `insteadOf`
   rewriting) must name `github.com/<owner>/<repo>` in `https://`, `ssh://` or
   `git@github.com:` form. The clone is only branched from; pushes go from an
   outbox-owned repository to `https://github.com/<owner>/<repo>.git`.
4. **Write the operator file** (below). It is executable policy (it names
   binaries, directories and who may authorize work), so it comes from the
   operator only, never from a repository. Every path is absolute.
5. **Start serve** with a listener so operators can connect over ACP:

   ```sh
   TEMPEST_TOKEN=change-me ./bin/tempest serve -listen 127.0.0.1:7777 \
     -github acme/widgets -github-config /home/tempest/acme-widgets.json
   ```

   `-github` and `-github-config` must be set together. `-extension-config`
   cannot be combined with `-github`. `workers.model` empty means serve's
   `-model`.

Startup refuses, in this order and before any poll or dispatch, naming the cause:

1. a GitHub token variable in the serve environment;
2. `gh api user` under `account.ghConfigDir` not matching `account.login` and
   `account.id`;
3. the repository missing, renamed, or without push permission for the account;
4. the clone's remote URL not naming the repository;
5. another serve already holding `<stateDir>/control.jsonl` for this repository;
6. a control journal recorded for a different repository ID or account.

### Operator file

Strict JSON: unknown fields, trailing data and bare-number durations are load
errors, and each error names its field. Durations use Go syntax (`"90s"`,
`"45m"`). The file may be at most 1 MiB.

```json
{
  "version": 1,
  "repository": "acme/widgets",
  "account": {"login": "acme-tempest", "id": 123456789, "ghPath": "/usr/local/bin/gh", "ghConfigDir": "/home/tempest/.config/gh-acme"},
  "clone": "/home/tempest/clones/widgets",
  "stateDir": "/home/tempest/state/acme-widgets",
  "poll": {"interval": "60s", "lookback": "1h", "retention": "336h", "callTimeout": "30s", "maxPages": 10},
  "observe": {"issues": false, "pulls": false, "comments": false, "ci": false},
  "admission": {"mode": "observe", "actors": [{"login": "parth", "id": 1111}], "bots": [{"login": "coderabbitai[bot]", "id": 2222}]},
  "workers": {"max": 3, "model": "", "permissionMode": "default", "prepare": ["make", "deps"], "prepareTimeout": "10m", "baseBranch": "", "remote": "origin"},
  "budgets": {
    "run": {"maxTurns": 60, "maxCostUsd": 2, "maxWallTime": "45m", "maxTotalTokens": 0, "maxToolCalls": 300},
    "ticketUsd": 10, "repoUsd": 100, "repoPeriod": "720h", "minRunUsd": 0.2, "maxAutoWakesPerDay": 12
  },
  "limits": {"maxEventBodyBytes": 65536, "maxWakeBytes": 65536},
  "prompts": {"communication": "", "scope": "", "delivery": ""},
  "merge": "manual"
}
```

| Field | Meaning and rules |
|---|---|
| `version` | Must be `1`. |
| `repository` | `owner/repo`; must equal `-github` (case-insensitive). |
| `account` | The dedicated user. `id` is what reconciliation and self-filtering use; `login` is checked against GitHub at startup. `ghPath` and `ghConfigDir` are absolute. |
| `clone` | Absolute path of the local clone. |
| `stateDir` | Holds the control journal, Ticket worktrees and the outbox's push repository. Default `<session-dir>/github/<owner>/<repo>`. |
| `poll.interval` | 60s to 1h, default 60s. A larger GitHub `X-Poll-Interval` wins. |
| `poll.lookback` | Reread window for late-indexed items, default 1h. Event IDs deduplicate rereads. |
| `poll.retention` | How long a thread stays polled after its last Ticket ended, default 336h. |
| `poll.callTimeout`, `poll.maxPages` | Bound each `gh` call (default 30s) and the pages read per resource per round (default 10). |
| `observe.*` | Record repository-wide issue, PR, comment and CI streams. Observed events are context only; they never start work. |
| `admission.mode` | `observe` or `off`. `auto` is rejected: this build never works on something nobody asked for. |
| `admission.actors` | People who may request, stop, reopen and wake work, by numeric `id` (login is a guard). At least one; IDs positive, distinct from `bots` and from `account.id`. |
| `admission.bots` | Bot accounts whose comments are machine input. Bots never authorize work. |
| `workers.max` | Normal worker slots, 1 to 16, default 3. One more slot serves only human follow-ups to existing workers. |
| `workers.permissionMode` | `default`, `plan` or `acceptEdits`; `bypass` is rejected. Default `default`. |
| `workers.prepare`, `prepareTimeout` | An argv (no shell) run once in each new worktree, for example dependency installs. |
| `workers.baseBranch`, `workers.remote` | Branch new worktrees from `<remote>/<base>`. Defaults: the repository's default branch, `origin`. |
| `budgets.run` | Per-run caps applied to every worker run. `maxCostUsd`, `maxTurns` and `maxWallTime` are required and positive. |
| `budgets.ticketUsd`, `repoUsd`, `repoPeriod` | Each Ticket's lifetime allowance, and the repository's allowance per period. Required and positive; there is no unlimited setting. `ticketTokens` and `repoTokens` optionally cap tokens too. |
| `budgets.minRunUsd` | Smallest reservation worth a run, at most `run.maxCostUsd`; default a tenth of it. |
| `budgets.maxAutoWakesPerDay` | Machine-input wakes per Ticket per UTC day, default 12. Ends bot and CI loops. |
| `limits.*` | Stored event body cap and wake prompt cap, default 64 KiB each. |
| `prompts.*` | Team guidance added to every worker's system prompt, captured when a Ticket is created. |
| `merge` | Must be `"manual"`. |

## Working with it on GitHub

Mention the account at the start of a word, for example `@acme-tempest please
fix the flaky parser test`:

- A listed actor's mention on an issue or PR with no open Ticket **creates a
  Ticket**. On a thread with an open Ticket it is a follow-up instead.
- Any comment, review or inline comment by a listed actor on a thread with an
  open Ticket **wakes** the worker. Other people's comments reach the worker as
  context but do not wake it. Bot comments and PR facts (new head, settled CI)
  wake it, up to `maxAutoWakesPerDay`.
- `@acme-tempest stop` as the first line **stops** every open Ticket on that
  thread, permanently. `@acme-tempest status` posts a status reply.
- An ended Ticket **reopens** only on a fresh mention by a listed actor, written
  after it ended. "Thanks, merged" does not restart paid work.
- Edited comments are context only: an edit never requests, stops, reopens or
  wakes anything.
- Mentions by people who are not listed are recorded as denied, with no reply.

The account's own comments are never read back as input.

## Lifecycle

A Ticket is **open** until it ends as `completed` (the worker reported ready and
there is no PR), `merged` or `closed` (every PR it opened is closed; merged if the
last one merged), `dropped` (the worker reported dropping it) or `stopped`
(permanent). A comment that arrives while the worker is finishing keeps it open.

Each Ticket gets one worker session for its whole life, in the worktree
`<stateDir>/worktrees/<TicketID>` on branch `agent/<TicketID>`, with the required
`github` extension. Load the worker's transcript over ACP with `session/load` and
the `sessionId` from the Ticket. The worker sees the request, the thread and team
guidance in its system prompt, and each wake as a block per new input with the
GitHub text fenced as data. Its GitHub tools are:

| Tool (model name) | What it does |
|---|---|
| `report` (`ext__github__report`) | Records `progress`, `waiting`, `ready` (for review), `blocked` with a question, `failed` or `dropped`. |
| `reply` (`ext__github__reply`) | Comments on the Ticket's thread or one of its PRs, or answers a review thread. |
| `push` (`ext__github__push`) | Pushes the worktree's committed `HEAD` to `agent/<TicketID>`. |
| `open_pr` (`ext__github__open-upr`) | Pushes if needed, then opens a PR against the base. Extension tool names escape `_` as `-u`. |
| `status` (`ext__github__status`) | The Ticket's PRs (head, CI and reviews pinned to that head), outbox states, budget and undelivered inputs. |
| `thread` (`ext__github__thread`) | Pages stored events of the thread or a linked PR. |

Writes are recorded as intents and published by the controller; the tools never
take a repository, branch or thread outside the Ticket. Under the default
permission mode these tools and ordinary `git add`/`git commit` need no
approver. Guarded and destructive commands go to ACP approvers and are denied
after `-permission-timeout`; a GitHub comment saying "yes" never answers one.

An open Ticket's **stage** is derived, first match wins:

| Stage | Meaning |
|---|---|
| `person/delivery` | A write's outcome is unknown, or delivery to the worker failed. An operator must act. |
| `person/answer` | The worker is blocked on a question. |
| `person/review` | The worker reported ready and a PR awaits review at its current head. |
| `person/blocked`, `person/failed`, `person/held` | Held: blocked, failed (a run ended in a terminal error, preparation failed, or transient failures repeated), or `revoked`, `budget` or `wake_limit`. |
| `working/queued` | Waiting for a slot. |
| `working` | A worker run holds a slot. |
| `idle` | Nothing to do until new input. |

Holds stop automatic wakes, not intake. A listed actor's comment releases
`blocked`, `failed` and `wake_limit`; `budget` needs a grant or a new period;
`revoked` (the requester was removed from `actors`) needs an operator retry.

## Operator controls over ACP

When the runner is running, `initialize` advertises
`agentCapabilities._meta.tempest.github = {version: 1, repo, methods, notifications}`.
The methods use the same connection and bearer token as the rest of
[ACP](serve-and-protocols.md#acp-protocol). Control methods take a `requestId`:
repeating it returns the original result without acting again, and each one is
recorded in the journal with its `reason`.

| Method | Params | Result |
|---|---|---|
| `_tempest/github/status` | `{}` | Repository summary (occupancy, queued, attention, open Tickets, unknown effects, repository ledger, pause, `seq`) and `health` of the poll loop. |
| `_tempest/github/tickets` | `{filter?, cursor?, limit?}` | `filter` is `attention` (person stages first), `open`, `ended` or `all`; `limit` at most 100. |
| `_tempest/github/ticket` | `{ticketId}` | Ticket, mandate, stage, generations, dispatches, inputs, outbox intents, ledger and PRs. |
| `_tempest/github/events` | `{thread? \| ticketId?, cursor?, limit?}` | Stored events with admission decisions, newest first. |
| `_tempest/github/stop` | `{requestId, ticketId, reason}` | Stops the Ticket permanently and cancels its run and pending writes. |
| `_tempest/github/retry` | `{requestId, ticketId, reason}` | Releases any hold and starts a new generation of an open Ticket whose worker is not running. |
| `_tempest/github/pause`, `unpause` | `{requestId, reason}` | Holds or releases dispatch and publishing. Polling continues. |
| `_tempest/github/grant` | `{requestId, ticketId?, usd?, tokens?}` | Adds allowance to a Ticket's ledger, or to the current repository period without `ticketId`. |
| `_tempest/github/resolve_effect` | `{requestId, intentId, state, reason}` | Settles an `unknown` write: `confirmed`, `failed` or `resend` (below). |
| `_tempest/github/poll_now` | `{}` | Runs one poll round now and returns its report. |
| `_tempest/github/watch` | `{buffer?}` | A snapshot `{status, attention, seq}`, then updates. |
| `_tempest/github/unwatch` | `{}` | Stops this connection's watch. |

After a watch, every control commit with a sequence above the snapshot's sends
`_tempest/github/update {seq, tickets, health}` naming the Tickets the commit
changed (possibly none); fetch those with `_tempest/github/ticket`. A watcher that
falls `buffer` updates behind (default 256) gets `_tempest/github/gap {lastSeq}`
and is dropped, never slowing the runner; watch again for a fresh snapshot. A
second watch on one connection replaces the first. Disconnecting ends the watch;
reconnect and watch again.

Errors: `-32030 github_not_found`, `-32031 github_stopped`, `-32032 github_held`,
`-32033 github_paused`, `-32034 github_budget`, `-32035 github_ended`,
`-32024 durable_conflict` (the record is not in a state that allows the call, for
example resolving a write that is not unknown), `-32602 invalid_params`, and
`-32601` for every github method when serve runs without `-github`.

## Budgets and limits

Spending is finite at three levels, in micro-USD (and optionally tokens):

- Each run is capped by `budgets.run`, lowered to what the ledgers can reserve.
- Each dispatch reserves `min(run cap, Ticket left, repository left)` before it
  starts. A Ticket that cannot reserve `minRunUsd` is held `budget`.
- Settlement charges the run's real usage. A run whose usage cannot be proven
  (the process stopped mid-run) charges its whole reservation. Run caps can be
  exceeded by one request; the overrun is recorded, so the next reservation
  shrinks.

Nothing resets a ledger: not human input, retries, recovery, reopening or a
restart. Only `grant` or a new repository period adds money.

Other bounds:

| Bound | Value |
|---|---|
| Worker slots | `workers.max` normal plus one for human follow-ups to existing workers. One live dispatch per Ticket. |
| Transient dispatch failures | 3 retries with backoff, then hold `failed`. |
| Write attempts | 5 per intent (refusals such as rate limits retry with backoff), then `failed`. |
| Publishing | At most 4 Tickets at once, each Ticket's writes in order. |
| Polling share | 40% of the account's primary rate limit; the last 20% is reserved for writes. A `Retry-After` pauses all writes of the account. |
| Source health | Unhealthy after 3 consecutive failed rounds (shown in `status.health`). |
| Event bodies and wakes | `limits.maxEventBodyBytes` and `limits.maxWakeBytes`; older inputs are counted and readable with the `thread` tool. |

## Failures and unknown writes

Every write is committed as an intent before anything is sent, marked
`attempting` before the request, and settled after it. A comment or PR body ends
with the marker line `<!-- tempest:<intentId> -->`; a push carries an exact
expected SHA (`--force-with-lease`).

When a request's reply is lost (a 502, a timeout, no status, a crash), the runner
looks for the write instead of sending it again: a comment or PR by the account
ending with its marker, in the intended thread, or the branch at the pushed SHA.
Found means `confirmed`. GitHub's listings can lag a write, so a comment or PR not
found is read again on later passes, three reads with the last at least two
minutes after the attempt, before it settles **`unknown`**. A push whose branch is
still at the expected SHA is retried; a branch moved by someone else is `unknown`.
Only a write GitHub refused before writing (or that never left) is retried
automatically.

An `unknown` write is never repeated on its own: a crash just before sending and
one just after leave the same evidence. It puts the Ticket in
`person/delivery`. To resolve it, look at the thread or PR on GitHub (search for
the intent ID in the marker; a human may have edited it away) and call
`_tempest/github/resolve_effect` with:

- `confirmed` if the write is there;
- `failed` to abandon it (the worker sees it failed);
- `resend` if it is not there and should be sent: the intent goes back to
  `pending` with a fresh attempt limit and publishes before the Ticket's later
  writes. Only an `unknown` write can be resent, and not for a stopped Ticket.

After a restart, the runner reconciles before it starts anything new: running
dispatches are settled from the worker's durable receipt (an interrupted run
starts a recovery generation that tells the worker to check `git status`,
`git log` and the `status` tool before repeating anything), and `attempting`
writes are reconciled as above. On shutdown the loops stop before sessions close;
running dispatches keep their slots until the next start settles them.

## Not in this build

No webhooks (polling only), no `auto` admission or model router for unrequested
work, no merges by the runner, no sandbox, and no browser panel. The control
journal keeps its history in memory, growing with repository and Ticket activity,
not with poll count.
