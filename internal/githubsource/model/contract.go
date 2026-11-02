package model

import (
	"context"
	"errors"
	"time"
)

// Errors shared across slices. Control returns them wrapped with context.
var (
	ErrNotFound = errors.New("github: not found")
	// ErrNotBound: a worker tool ran in a session bound to no Ticket.
	ErrNotBound = errors.New("github: session is not bound to a ticket")
	// ErrStopped: the Ticket was stopped; nothing more runs or publishes.
	ErrStopped = errors.New("github: ticket stopped")
	// ErrEnded: the Ticket ended; reopen needs an authoritative follow-up.
	ErrEnded = errors.New("github: ticket ended")
	// ErrHeld: the Ticket holds automatic work (see Hold).
	ErrHeld = errors.New("github: ticket held")
	// ErrPaused: the repository is paused by the operator.
	ErrPaused = errors.New("github: repository paused")
	// ErrBudget: a ledger cannot reserve the minimum run.
	ErrBudget = errors.New("github: budget exhausted")
	// ErrInvalidTarget: a write named a thread, PR or branch outside the
	// Ticket's Mandate.
	ErrInvalidTarget = errors.New("github: target outside ticket scope")
)

// Batch bounds keep one intake commit inside durable.MaxBatchBytes and the
// store's 1024-change limit after control adds decisions and Inputs.
const (
	MaxBatchEvents    = 100
	MaxBatchBodyBytes = 512 << 10
)

// Clock is the only time source of every slice, so tests are deterministic.
type Clock interface {
	Now() time.Time
}

// Source reads GitHub through a GHClient and hands normalized batches to
// Intake. It owns no durable state of its own.
type Source interface {
	// Poll runs one bounded round: notifications, enabled observe streams,
	// then watched threads and their PRs. Each resource fails on its own;
	// a failed page commits nothing and moves no position.
	Poll(ctx context.Context) (PollReport, error)
	// Health is the in-memory view of recent rounds.
	Health() SourceHealth
}

// PollReport summarizes one round.
type PollReport struct {
	StartedAt  time.Time       `json:"startedAt"`
	FinishedAt time.Time       `json:"finishedAt"`
	Batches    int             `json:"batches"`
	NewEvents  int             `json:"newEvents"`
	NextPollAt time.Time       `json:"nextPollAt"`
	Errors     []ResourceError `json:"errors,omitempty"`
}

// ResourceError is one failed resource read in a round.
type ResourceError struct {
	Resource string    `json:"resource"`
	Status   int       `json:"status,omitempty"`
	Message  string    `json:"message"`
	At       time.Time `json:"at"`
}

// SourceHealth turns unhealthy after three consecutive failed rounds.
type SourceHealth struct {
	Healthy             bool            `json:"healthy"`
	ConsecutiveFailures int             `json:"consecutiveFailures"`
	LastRoundAt         time.Time       `json:"lastRoundAt,omitzero"`
	LastSuccessAt       time.Time       `json:"lastSuccessAt,omitzero"`
	NextPollAt          time.Time       `json:"nextPollAt,omitzero"`
	RateLimit           RateLimit       `json:"rateLimit"`
	Errors              []ResourceError `json:"errors,omitempty"`
}

// Intake is control's side of the source boundary.
type Intake interface {
	Cursor(ctx context.Context, key CursorKey) (Cursor, bool, error)
	// Watched lists threads to read this round: subscribed by an open
	// Ticket, flagged NeedsReconcile, or within WatchUntil.
	Watched(ctx context.Context, now time.Time) ([]WatchedThread, error)
	// Ingest decides and commits one batch atomically: new events, their
	// Decisions, Tickets, Mandates, Inputs, dispatch intents, PR facts and
	// the positions read up to. Known EventIDs are skipped, so a reread
	// batch commits nothing new. Positions only move forward.
	Ingest(ctx context.Context, batch IntakeBatch) (IngestResult, error)
}

// WatchedThread is one thread to read, with the PR facts known for it.
type WatchedThread struct {
	Thread Thread       `json:"thread"`
	PR     *PullRequest `json:"pr,omitempty"`
}

// IntakeBatch is everything one bounded read produced. Events are in
// GitHub order (oldest first) and at most MaxBatchEvents.
type IntakeBatch struct {
	// Resource names what was read, e.g. "thread:42" or "notifications";
	// with the content digest it forms the commit ID.
	Resource string         `json:"resource"`
	Events   []Event        `json:"events,omitempty"`
	Cursor   *Cursor        `json:"cursor,omitempty"`
	Threads  []ThreadUpdate `json:"threads,omitempty"`
	PRs      []PullRequest  `json:"prs,omitempty"`
	ReadAt   time.Time      `json:"readAt"`
}

// ThreadUpdate moves a thread's positions and metadata. Nil positions are
// unchanged. Reconcile true marks a notification; false records a complete
// read and clears the mark.
type ThreadUpdate struct {
	Ref            ThreadRef `json:"ref"`
	Title          string    `json:"title,omitempty"`
	State          string    `json:"state,omitempty"`
	Author         *Actor    `json:"author,omitempty"`
	Comments       *Position `json:"comments,omitempty"`
	ReviewComments *Position `json:"reviewComments,omitempty"`
	Reviews        *Position `json:"reviews,omitempty"`
	Reconcile      *bool     `json:"reconcile,omitempty"`
	NotifiedAt     time.Time `json:"notifiedAt,omitzero"`
	LastActivityAt time.Time `json:"lastActivityAt,omitzero"`
}

// IngestResult reports what a batch changed so loops can be woken.
type IngestResult struct {
	Seq       uint64     `json:"seq"`
	NewEvents int        `json:"newEvents"`
	Decisions []Decision `json:"decisions,omitempty"`
	// Dispatch and Outbox are true when the commit created work for them.
	Dispatch bool `json:"dispatch"`
	Outbox   bool `json:"outbox"`
}

// DispatchStore is control's side of the dispatcher boundary. Every method
// is one control-journal commit or a read at one cut.
type DispatchStore interface {
	// AdmitNext applies the slot policy (N normal slots, one human
	// continuation slot, human class first, FIFO within class), reserves
	// both ledgers for each admitted dispatch and commits the admissions.
	// Tickets that cannot reserve are held for budget in the same commit.
	AdmitNext(ctx context.Context, now time.Time) ([]AdmittedDispatch, error)
	// Active lists admitted, preparing and submitted dispatches, for
	// restart reconciliation before any slot is released.
	Active(ctx context.Context) ([]AdmittedDispatch, error)
	// Deliveries lists running Tickets with undelivered Inputs.
	Deliveries(ctx context.Context) ([]Delivery, error)
	Preparing(ctx context.Context, id DispatchID, now time.Time) error
	// Prepared records the Ticket's worktree once it exists and its
	// prepare command, if any, succeeded.
	Prepared(ctx context.Context, id TicketID, worktree, baseSHA string) error
	// Bind records the worker session after its WorkerTicket document is
	// committed and before its first submission.
	Bind(ctx context.Context, binding SessionBinding) error
	// Submitted records the worker session's receipt for a dispatch.
	Submitted(ctx context.Context, id DispatchID, receipt SubmissionReceipt) error
	// Delivered records Inputs through seq accepted by a running worker.
	Delivered(ctx context.Context, id TicketID, gen uint32, throughSeq uint64) error
	// Finished settles a dispatch: ledgers, slot release, run-outcome holds,
	// completion rules, and the next wake if Inputs are pending.
	Finished(ctx context.Context, id DispatchID, outcome RunOutcome, now time.Time) error
	// Abandon settles a dispatch that never submitted. Transient failures
	// requeue it at retryAt; others hold the Ticket as failed.
	Abandon(ctx context.Context, id DispatchID, failure DispatchFailure) error
	// Inputs pages a Ticket's Inputs with their events, oldest first.
	Inputs(ctx context.Context, id TicketID, afterSeq uint64, limit int) ([]InputView, error)
}

// AdmittedDispatch is a dispatch holding a slot, with what executing it
// needs.
type AdmittedDispatch struct {
	Dispatch DispatchIntent `json:"dispatch"`
	Ticket   Ticket         `json:"ticket"`
	Mandate  Mandate        `json:"mandate"`
	// Budget is the run budget to apply with Session.SetBudget: the
	// configured run cap lowered to the reservation.
	Budget RunBudget `json:"budget"`
}

// Delivery is a running Ticket whose worker has not received some Inputs.
type Delivery struct {
	Ticket    TicketID    `json:"ticket"`
	Gen       uint32      `json:"gen"`
	SessionID string      `json:"sessionId"`
	Inputs    []InputView `json:"inputs"`
}

// SubmissionReceipt is the worker session's durable receipt.
type SubmissionReceipt struct {
	RequestID    string    `json:"requestId"`
	SubmissionID string    `json:"submissionId"`
	RunID        string    `json:"runId"`
	ThroughInput uint64    `json:"throughInput"`
	At           time.Time `json:"at"`
}

// DispatchFailure explains a dispatch that never reached the worker.
type DispatchFailure struct {
	Reason    string    `json:"reason"`
	Transient bool      `json:"transient"`
	RetryAt   time.Time `json:"retryAt,omitzero"`
	At        time.Time `json:"at"`
}

// InputView is an Input with the event it carries.
type InputView struct {
	Input Input `json:"input"`
	Event Event `json:"event"`
}

// Dispatcher runs admitted work in worker sessions.
type Dispatcher interface {
	// Run reconciles active dispatches, then admits and executes work until
	// ctx ends. It never releases a slot it cannot prove settled.
	Run(ctx context.Context) error
	// Wake nudges Run after a commit that may have created work.
	Wake()
}

// WorkerCall identifies the tool call a worker made. Authority comes from
// the session's binding, never from tool arguments.
type WorkerCall struct {
	SessionID  string    `json:"sessionId"`
	ToolCallID string    `json:"toolCallId"`
	At         time.Time `json:"at"`
}

// ReportRequest is the worker's report tool input.
type ReportRequest struct {
	Status   ReportStatus `json:"status"`
	Summary  string       `json:"summary"`
	Question *Question    `json:"question,omitempty"`
}

// EffectRequest is a worker's requested write. Control fills targets from
// the Ticket and rejects anything outside its Mandate with ErrInvalidTarget.
type EffectRequest struct {
	Kind    EffectKind    `json:"kind"`
	Payload EffectPayload `json:"payload"`
}

// ThreadQuery pages events of the Ticket's thread or a bound PR.
type ThreadQuery struct {
	// Number 0 selects the Ticket's own thread.
	Number int    `json:"number,omitempty"`
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// WorkerGateway is what the github extension's tools call. Every method
// resolves the Ticket from the calling session's SessionBinding.
type WorkerGateway interface {
	// Report records a structured report. It is idempotent per tool call.
	Report(ctx context.Context, call WorkerCall, request ReportRequest) (Ticket, error)
	// Enqueue commits an outbox intent keyed by NewOutboxID; a repeated
	// call returns the original intent.
	Enqueue(ctx context.Context, call WorkerCall, request EffectRequest) (OutboxIntent, error)
	// Effect reads an outbox intent of the caller's Ticket.
	Effect(ctx context.Context, sessionID string, id OutboxID) (OutboxIntent, error)
	Status(ctx context.Context, sessionID string) (TicketDetail, error)
	Thread(ctx context.Context, sessionID string, query ThreadQuery) (EventPage, error)
}

// EffectStore is control's side of the outbox boundary.
type EffectStore interface {
	// Due lists intents to work on now: the oldest unsettled intent of each
	// Ticket (pending past RetryAt, or attempting from before a restart).
	// Paused repositories and stopped Tickets return none.
	Due(ctx context.Context, now time.Time) ([]OutboxIntent, error)
	// Attempt commits the attempting state before the request is sent.
	Attempt(ctx context.Context, id OutboxID, now time.Time) (OutboxIntent, error)
	// Settle records the outcome. A confirmed open_pr binds the PR to its
	// Ticket as own in the same commit.
	Settle(ctx context.Context, id OutboxID, settlement EffectSettlement) (OutboxIntent, error)
	Ticket(ctx context.Context, id TicketID) (Ticket, error)
}

// Outbox publishes committed intents and reconciles interrupted ones.
type Outbox interface {
	Run(ctx context.Context) error
	Wake()
}

// OperatorRequest is one audited operator control. ID is the client's
// idempotency key; a repeated ID returns the original result.
type OperatorRequest struct {
	ID     string    `json:"id"`
	Target string    `json:"target,omitempty"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

// Control is the operator surface ACP methods call.
type Control interface {
	Status(ctx context.Context) (StatusView, error)
	Tickets(ctx context.Context, query TicketQuery) (TicketPage, error)
	Ticket(ctx context.Context, id TicketID) (TicketDetail, error)
	Events(ctx context.Context, query EventQuery) (EventPage, error)
	// Stop is permanent: it ends the Ticket as stopped, cancels queued
	// dispatches and pending intents, and the dispatcher cancels its run.
	Stop(ctx context.Context, request OperatorRequest) error
	// Retry starts a new generation of an open or failed Ticket.
	Retry(ctx context.Context, request OperatorRequest) error
	SetPaused(ctx context.Context, request OperatorRequest, paused bool) error
	// Grant adds allowance to a Ticket ledger, or the current repository
	// ledger when Target is empty.
	Grant(ctx context.Context, request OperatorRequest, amount Spend) error
	// ResolveEffect settles an unknown intent as confirmed or failed.
	ResolveEffect(ctx context.Context, request OperatorRequest, state EffectState) error
}

// TicketFilter selects a Ticket listing.
type TicketFilter string

// Ticket filters. Attention lists Tickets in a person/* stage first.
const (
	FilterAttention TicketFilter = "attention"
	FilterOpen      TicketFilter = "open"
	FilterEnded     TicketFilter = "ended"
	FilterAll       TicketFilter = "all"
)

// TicketQuery pages Tickets.
type TicketQuery struct {
	Filter TicketFilter `json:"filter,omitempty"`
	Cursor string       `json:"cursor,omitempty"`
	Limit  int          `json:"limit,omitempty"`
}

// TicketPage is a bounded Ticket listing.
type TicketPage struct {
	Tickets    []TicketSummary `json:"tickets"`
	NextCursor string          `json:"nextCursor,omitempty"`
}

// TicketSummary is one listing row.
type TicketSummary struct {
	ID        TicketID  `json:"id"`
	Title     string    `json:"title"`
	Thread    ThreadRef `json:"thread"`
	Lifecycle Lifecycle `json:"lifecycle"`
	Outcome   Outcome   `json:"outcome,omitempty"`
	Stage     Stage     `json:"stage"`
	Hold      Hold      `json:"hold,omitempty"`
	Question  *Question `json:"question,omitempty"`
	PRs       []PRLink  `json:"prs,omitempty"`
	Spent     Spend     `json:"spent"`
	SessionID string    `json:"sessionId,omitempty"`
}

// TicketDetail is everything an operator or worker status view needs.
// SessionID lets an ACP client load the worker transcript directly.
type TicketDetail struct {
	Ticket       Ticket           `json:"ticket"`
	Mandate      Mandate          `json:"mandate"`
	Stage        Stage            `json:"stage"`
	Assignments  []Assignment     `json:"assignments"`
	Dispatches   []DispatchIntent `json:"dispatches"`
	Inputs       []InputView      `json:"inputs"`
	Outbox       []OutboxIntent   `json:"outbox"`
	Ledger       Ledger           `json:"ledger"`
	PullRequests []PullRequest    `json:"pullRequests"`
}

// EventQuery pages events with their decisions, newest first.
type EventQuery struct {
	Thread int      `json:"thread,omitempty"`
	Ticket TicketID `json:"ticket,omitempty"`
	Cursor string   `json:"cursor,omitempty"`
	Limit  int      `json:"limit,omitempty"`
}

// EventPage is a bounded event listing.
type EventPage struct {
	Events     []EventView `json:"events"`
	NextCursor string      `json:"nextCursor,omitempty"`
}

// EventView is an event and what admission decided about it.
type EventView struct {
	Event    Event     `json:"event"`
	Decision *Decision `json:"decision,omitempty"`
}

// Occupancy counts occupied slots.
type Occupancy struct {
	Normal    int `json:"normal"`
	Human     int `json:"human"`
	MaxNormal int `json:"maxNormal"`
	MaxHuman  int `json:"maxHuman"`
}

// StatusView is the repository summary from the control journal. ACP adds
// Source.Health to it.
type StatusView struct {
	Repo           string    `json:"repo"`
	Account        Actor     `json:"account"`
	Pause          Pause     `json:"pause"`
	Occupancy      Occupancy `json:"occupancy"`
	Queued         int       `json:"queued"`
	Attention      int       `json:"attention"`
	OpenTickets    int       `json:"openTickets"`
	UnknownEffects int       `json:"unknownEffects"`
	RepoLedger     Ledger    `json:"repoLedger"`
	Seq            uint64    `json:"seq"`
}
