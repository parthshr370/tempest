// Package model is the shared contract of the GitHub ticket runner: the
// records stored in a repository's control journal, the identities that make
// every write idempotent, the operator configuration, and the interfaces
// between the source, control, dispatch and outbox slices.
//
// The package holds data and identity rules only. Behavior lives in the
// slices: source (polling and normalization), control (admission, Ticket
// state, budgets, scheduling), dispatch (worker sessions), outbox (GitHub
// writes and reconciliation) and the serve wiring.
//
// # Control journal
//
// One durable.Store per repository, opened with durable.OpenFile at
// <stateDir>/control.jsonl. Every record uses SessionID [ControlSession].
// Namespaces and record identities:
//
//	namespace       kind      id                         value
//	gh/meta         document  "repo" | "pause"           RepoMeta | Pause
//	gh/policy       entry     <policy digest>            Policy
//	gh/cursor       document  CursorKey                  Cursor
//	gh/thread       document  <number>                   Thread
//	gh/event        entry     EventID                    Event
//	gh/decision     entry     EventID                    Decision
//	gh/ticket       document  TicketID                   Ticket
//	gh/mandate      entry     TicketID                   Mandate
//	gh/input        entry     <TicketID>:<seq>           Input
//	gh/assignment   document  <TicketID>:<gen>           Assignment
//	gh/dispatch     document  DispatchID                 DispatchIntent
//	gh/session      document  <worker session id>        SessionBinding
//	gh/pr           document  <number>                   PullRequest
//	gh/outbox       document  OutboxID                   OutboxIntent
//	gh/ledger       document  LedgerKey                  Ledger
//	gh/op           entry     <operator command id>      OperatorCommand
//
// Entries are immutable: rewriting one with different content is a durable
// conflict, so a replayed ingest can never change what was decided. Documents
// change only under expected-revision checks.
package model
