package model

import "time"

// MandateBasis says why work was authorized.
type MandateBasis string

// Mandate bases. Wave G admits explicit requests only; inferred work from
// observe streams is recorded as DecisionObserved and never authorized.
const (
	BasisExplicitMention MandateBasis = "explicit_mention"
	BasisOperator        MandateBasis = "operator"
)

// Mandate is the immutable authorization of one Ticket: who asked, on which
// thread, under which policy. It is not a credential and does not widen what
// the worker process can reach; it bounds what the controller will dispatch
// and publish for this Ticket.
type Mandate struct {
	Ticket       TicketID     `json:"ticket"`
	Repo         string       `json:"repo"`
	Thread       ThreadRef    `json:"thread"`
	Actor        Actor        `json:"actor"`
	Basis        MandateBasis `json:"basis"`
	RequestEvent EventID      `json:"requestEvent"`
	PolicyDigest string       `json:"policyDigest"`
	CreatedAt    time.Time    `json:"createdAt"`
}

// Lifecycle is the persistent Ticket state. A running worker is not a
// lifecycle state: an open Ticket may be idle for days awaiting review.
type Lifecycle string

// Ticket lifecycles.
const (
	LifecycleOpen  Lifecycle = "open"
	LifecycleEnded Lifecycle = "ended"
)

// Outcome is why an ended Ticket ended.
type Outcome string

// Ticket outcomes.
const (
	OutcomeCompleted Outcome = "completed"
	OutcomeMerged    Outcome = "merged"
	OutcomeClosed    Outcome = "closed"
	OutcomeDropped   Outcome = "dropped"
	OutcomeStopped   Outcome = "stopped"
)

// Hold keeps an open Ticket from automatic wakes. Only an authoritative
// Input or an operator retry releases a hold; a stop is permanent.
type Hold string

// Holds, in precedence order when several apply.
const (
	HoldNone Hold = ""
	// HoldRevoked: the Mandate's actor is no longer authorized by policy.
	HoldRevoked Hold = "revoked"
	// HoldBudget: a ledger cannot reserve the configured minimum run.
	HoldBudget Hold = "budget"
	// HoldFailed: the latest report or run outcome is a failure.
	HoldFailed Hold = "failed"
	// HoldBlocked: the worker reported it cannot continue without a human.
	HoldBlocked Hold = "blocked"
	// HoldWakeLimit: automatic wakes hit Budgets.MaxAutoWakesPerDay.
	HoldWakeLimit Hold = "wake_limit"
)

// ReportStatus is a worker's structured statement about its Assignment.
type ReportStatus string

// Report statuses.
const (
	ReportProgress ReportStatus = "progress"
	ReportWaiting  ReportStatus = "waiting"
	ReportReady    ReportStatus = "ready"
	ReportBlocked  ReportStatus = "blocked"
	ReportFailed   ReportStatus = "failed"
	ReportDropped  ReportStatus = "dropped"
)

// Question is a structured request for a human answer. Needing a human is
// read from this field, never inferred from assistant prose.
type Question struct {
	Text    string   `json:"text"`
	Options []string `json:"options,omitempty"`
}

// Report is the latest structured statement of an Assignment.
// ThroughInput is the highest Input sequence the worker had been delivered
// when it reported; completion rules compare it with newer Inputs.
type Report struct {
	Status       ReportStatus `json:"status"`
	Summary      string       `json:"summary"`
	Question     *Question    `json:"question,omitempty"`
	Gen          uint32       `json:"gen"`
	ThroughInput uint64       `json:"throughInput"`
	ToolCallID   string       `json:"toolCallId"`
	At           time.Time    `json:"at"`
}

// PRLink is a PR a Ticket follows. Own PRs were opened by this Ticket's
// outbox on TicketBranch; their closure decides completion. A followed PR
// that is not own delivers context and keeps nothing alive once every own
// PR has closed.
type PRLink struct {
	Number int  `json:"number"`
	Own    bool `json:"own"`
	Closed bool `json:"closed"`
	Merged bool `json:"merged"`
}

// Ticket is the mutable aggregate of one authorized unit of work. It is
// open until an outcome ends it; an ended Ticket that was not stopped can
// reopen under the same Mandate for an authoritative follow-up.
type Ticket struct {
	ID        TicketID  `json:"id"`
	Repo      string    `json:"repo"`
	Thread    ThreadRef `json:"thread"`
	Title     string    `json:"title"`
	Branch    string    `json:"branch"`
	CreatedAt time.Time `json:"createdAt"`

	Lifecycle Lifecycle `json:"lifecycle"`
	Outcome   Outcome   `json:"outcome,omitempty"`
	// EndedBy is "executor", "source", "operator" or "human:<login>".
	EndedBy string    `json:"endedBy,omitempty"`
	EndedAt time.Time `json:"endedAt,omitzero"`
	// Stopped is permanent: no wake, reopen, dispatch or publish follows.
	Stopped bool `json:"stopped,omitempty"`

	Hold       Hold   `json:"hold,omitempty"`
	HoldReason string `json:"holdReason,omitempty"`

	// Gen is the current Assignment generation; zero before the first.
	Gen uint32 `json:"gen"`
	// InputSeq is the last appended Input. DeliveredSeq is the last Input
	// accepted by the worker session. HandledSeq is the last Input covered
	// by a non-progress report or a completed run.
	InputSeq     uint64 `json:"inputSeq"`
	DeliveredSeq uint64 `json:"deliveredSeq"`
	HandledSeq   uint64 `json:"handledSeq"`
	// DispatchSeq is the last DispatchIntent sequence of this Ticket.
	DispatchSeq uint64 `json:"dispatchSeq"`

	LastReport *Report `json:"lastReport,omitempty"`
	// DeliveryFailed records an Input the worker session refused.
	DeliveryFailed string `json:"deliveryFailed,omitempty"`

	PRs []PRLink `json:"prs,omitempty"`

	// SessionID, Worktree and Prepared describe the single worker session
	// and checkout that every Assignment of this Ticket reuses.
	SessionID string `json:"sessionId,omitempty"`
	Worktree  string `json:"worktree,omitempty"`
	BaseSHA   string `json:"baseSha,omitempty"`
	Prepared  bool   `json:"prepared,omitempty"`

	// AutoWakeDay and AutoWakes count non-human wakes in the current UTC
	// day for Budgets.MaxAutoWakesPerDay.
	AutoWakeDay string `json:"autoWakeDay,omitempty"`
	AutoWakes   int    `json:"autoWakes,omitempty"`
}

// Stage is the derived, attention-first view of an open Ticket. It is
// computed by control from Ticket, Assignment and dispatch facts and is
// never stored, so it cannot drift from them.
type Stage string

// Stages, in the order the first matching condition wins.
const (
	StageDelivery Stage = "person/delivery"
	StageAnswer   Stage = "person/answer"
	StageReview   Stage = "person/review"
	StageBlocked  Stage = "person/blocked"
	StageFailed   Stage = "person/failed"
	StageHeld     Stage = "person/held"
	StageQueued   Stage = "working/queued"
	StageIdle     Stage = "idle"
	StageWorking  Stage = "working"
	StageEnded    Stage = "ended"
)

// InputClass decides what an Input may cause.
type InputClass string

// Input classes.
const (
	// InputAuthoritative: a currently authorized actor on an own thread, or
	// an operator. May wake through the human slot and release holds.
	InputAuthoritative InputClass = "authoritative"
	// InputContext: any other human. Delivered with the next wake; never
	// wakes, reopens or releases anything by itself.
	InputContext InputClass = "context"
	// InputMachine: CI, PR state, bot reviews. Wakes through a normal slot
	// unless the Ticket is held, and counts toward the auto-wake limit.
	InputMachine InputClass = "machine"
)

// Input is one event delivered to one Ticket, in Ticket-local order.
type Input struct {
	Ticket TicketID   `json:"ticket"`
	Seq    uint64     `json:"seq"`
	Event  EventID    `json:"event"`
	Class  InputClass `json:"class"`
	// Wakes is false for Inputs that only ride along with the next wake.
	Wakes bool      `json:"wakes"`
	At    time.Time `json:"at"`
}

// Thread is the source's view of one Issue or PR conversation: read
// positions and the Tickets subscribed to it. Its positions advance only in
// the commit that stores the events read up to them.
type Thread struct {
	Ref    ThreadRef `json:"ref"`
	Title  string    `json:"title"`
	State  string    `json:"state"`
	Author Actor     `json:"author"`
	// Tickets subscribed to this thread, open or ended within retention.
	Tickets []TicketID `json:"tickets,omitempty"`
	// NeedsReconcile is set by a notification and cleared only by a
	// complete read of the thread. A notification never moves a position.
	NeedsReconcile bool      `json:"needsReconcile,omitempty"`
	NotifiedAt     time.Time `json:"notifiedAt,omitzero"`
	// Positions per stream. Zero means never read: the next read starts at
	// the thread's creation.
	Comments       Position  `json:"comments"`
	ReviewComments Position  `json:"reviewComments"`
	Reviews        Position  `json:"reviews"`
	LastActivityAt time.Time `json:"lastActivityAt,omitzero"`
	// WatchUntil bounds polling after every subscribed Ticket ended.
	WatchUntil time.Time `json:"watchUntil,omitzero"`
}

// Position is a committed read position within one paginated stream.
// Since is the newest updated_at fully read; rereads start at Since minus
// Config.Poll.Lookback and rely on EventID deduplication.
type Position struct {
	Since  time.Time `json:"since,omitzero"`
	LastID int64     `json:"lastId,omitempty"`
}

// PullRequest is the latest known state of a PR the repository watches.
type PullRequest struct {
	Number   int       `json:"number"`
	Author   Actor     `json:"author"`
	HeadRef  string    `json:"headRef"`
	HeadRepo string    `json:"headRepo"`
	HeadSHA  string    `json:"headSha"`
	BaseRef  string    `json:"baseRef"`
	State    string    `json:"state"`
	Merged   bool      `json:"merged"`
	MergeSHA string    `json:"mergeSha,omitempty"`
	ClosedAt time.Time `json:"closedAt,omitzero"`
	Draft    bool      `json:"draft,omitempty"`
	// Owner is the single open Ticket the PR is bound to, if any.
	Owner TicketID `json:"owner,omitempty"`
	// CI is the latest settled snapshot of HeadSHA. A snapshot for an
	// older head never describes the current one.
	CI        *CISnapshot  `json:"ci,omitempty"`
	Reviews   []ReviewFact `json:"reviews,omitempty"`
	UpdatedAt time.Time    `json:"updatedAt"`
}

// ReviewFact is one submitted review, pinned to the commit it reviewed.
type ReviewFact struct {
	ID          int64     `json:"id"`
	Actor       Actor     `json:"actor"`
	State       string    `json:"state"`
	CommitSHA   string    `json:"commitSha"`
	SubmittedAt time.Time `json:"submittedAt"`
}

// SessionBinding maps a worker session to its Ticket. Worker tools resolve
// their Ticket through this record, never through model-supplied arguments.
type SessionBinding struct {
	SessionID string    `json:"sessionId"`
	Ticket    TicketID  `json:"ticket"`
	Worktree  string    `json:"worktree"`
	BoundAt   time.Time `json:"boundAt"`
}

// RepoMeta is the validated identity of the configured repository and
// account, written at first start and compared on every later start.
type RepoMeta struct {
	Repo          string    `json:"repo"`
	RepoID        int64     `json:"repoId"`
	DefaultBranch string    `json:"defaultBranch"`
	Account       Actor     `json:"account"`
	CreatedAt     time.Time `json:"createdAt"`
}

// Pause holds dispatch and outbox publishing while intake continues. It
// does not claim a running worker turn or an attempting write stopped.
type Pause struct {
	Paused bool      `json:"paused"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

// WorkerTicket is the worker-session document the github extension's prompt
// sections render. It is written once per Ticket and never changed, so the
// rendered system prompt stays byte-identical for the cache prefix; changing
// facts reach the worker as Inputs and through the status tool.
type WorkerTicket struct {
	Ticket   TicketID  `json:"ticket"`
	Repo     string    `json:"repo"`
	Thread   ThreadRef `json:"thread"`
	Title    string    `json:"title"`
	URL      string    `json:"url"`
	Branch   string    `json:"branch"`
	Base     string    `json:"base"`
	Worktree string    `json:"worktree"`
	Mandate  Mandate   `json:"mandate"`
	// Request is the bounded text of the requesting event.
	Request string `json:"request"`
	// Team holds the operator prompt sections captured at Ticket creation.
	Team TeamPrompts `json:"team"`
}
