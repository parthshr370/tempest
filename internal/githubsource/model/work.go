package model

import "time"

// AssignmentReason says why a generation started.
type AssignmentReason string

// Assignment reasons.
const (
	ReasonLaunch   AssignmentReason = "launch"
	ReasonRetry    AssignmentReason = "retry"
	ReasonRecovery AssignmentReason = "recovery"
	ReasonReopen   AssignmentReason = "reopen"
)

// AssignmentState is the lifecycle of one generation. A generation spans
// many worker runs; it ends when a retry, recovery or reopen replaces it or
// when its Ticket ends.
type AssignmentState string

// Assignment states.
const (
	AssignmentOpen  AssignmentState = "open"
	AssignmentEnded AssignmentState = "ended"
)

// Assignment is one execution generation of a Ticket. Its phase (queued,
// preparing, running, quiescent) is derived from its latest DispatchIntent,
// so the two records cannot disagree.
type Assignment struct {
	Ticket    TicketID         `json:"ticket"`
	Gen       uint32           `json:"gen"`
	Reason    AssignmentReason `json:"reason"`
	State     AssignmentState  `json:"state"`
	StartedAt time.Time        `json:"startedAt"`
	EndedAt   time.Time        `json:"endedAt,omitzero"`
	// EndReason is "replaced", "ticket_ended" or "stopped".
	EndReason string `json:"endReason,omitempty"`
	// Runs counts finished dispatches of this generation.
	Runs int `json:"runs"`
}

// SlotClass is the scheduler class a dispatch may occupy.
type SlotClass string

// Slot classes. A human dispatch may use a normal slot or the one extra
// human slot; a normal dispatch may use only normal slots.
const (
	SlotNormal SlotClass = "normal"
	SlotHuman  SlotClass = "human"
)

// DispatchKind says what a dispatch delivers.
type DispatchKind string

// Dispatch kinds.
const (
	// DispatchLaunch: first run of a generation (launch, retry, reopen).
	DispatchLaunch DispatchKind = "launch"
	// DispatchWake: pending Inputs for an idle worker.
	DispatchWake DispatchKind = "wake"
	// DispatchRecovery: restart notice after an interrupted run.
	DispatchRecovery DispatchKind = "recovery"
)

// DispatchState is the lifecycle of one worker activation. Admitted,
// preparing and submitted dispatches occupy a slot.
type DispatchState string

// Dispatch states.
const (
	DispatchQueued    DispatchState = "queued"
	DispatchAdmitted  DispatchState = "admitted"
	DispatchPreparing DispatchState = "preparing"
	DispatchSubmitted DispatchState = "submitted"
	DispatchFinished  DispatchState = "finished"
	DispatchCancelled DispatchState = "cancelled"
)

// DispatchIntent is a committed decision to run a Ticket's worker. It is
// written before any worktree, session or provider work, and every later
// step records its result here before the next step starts.
type DispatchIntent struct {
	ID     DispatchID    `json:"id"`
	Ticket TicketID      `json:"ticket"`
	Seq    uint64        `json:"seq"`
	Gen    uint32        `json:"gen"`
	Kind   DispatchKind  `json:"kind"`
	Class  SlotClass     `json:"class"`
	State  DispatchState `json:"state"`
	// Slot is the class actually occupied once admitted.
	Slot SlotClass `json:"slot,omitempty"`
	// ThroughInput is the last Input this dispatch delivers.
	ThroughInput uint64 `json:"throughInput"`
	// RequestID is LaunchRequestID(Ticket, Seq, Gen); SubmissionID and RunID
	// are the worker session's receipt for it.
	RequestID    string `json:"requestId"`
	SubmissionID string `json:"submissionId,omitempty"`
	RunID        string `json:"runId,omitempty"`
	// Reservation is held in both ledgers from admission until settlement.
	Reservation Spend       `json:"reservation"`
	Settled     *Spend      `json:"settled,omitempty"`
	Outcome     *RunOutcome `json:"outcome,omitempty"`
	// Attempts counts transient retries of this dispatch.
	Attempts     int       `json:"attempts,omitempty"`
	RetryAt      time.Time `json:"retryAt,omitzero"`
	CancelReason string    `json:"cancelReason,omitempty"`
	QueuedAt     time.Time `json:"queuedAt"`
	AdmittedAt   time.Time `json:"admittedAt,omitzero"`
	SubmittedAt  time.Time `json:"submittedAt,omitzero"`
	FinishedAt   time.Time `json:"finishedAt,omitzero"`
}

// RunOutcome is how a dispatch's runs ended, from the session's submission
// receipt. StopReason is the last run's tempest stop reason; "unknown" means
// the process stopped after dispatch and the receipt cannot prove a result.
// Usage sums every run of the dispatch, continuations included.
type RunOutcome struct {
	StopReason string `json:"stopReason"`
	Error      string `json:"error,omitempty"`
	Usage      Spend  `json:"usage"`
	// UsageKnown is false when the run's usage could not be read; the
	// ledger then settles the whole reservation.
	UsageKnown bool `json:"usageKnown"`
}

// Spend is an amount of model work. Cost is integer micro-USD so ledger
// arithmetic is exact.
type Spend struct {
	MicroUSD     int64 `json:"microUsd"`
	InputTokens  int64 `json:"inputTokens,omitempty"`
	OutputTokens int64 `json:"outputTokens,omitempty"`
	TotalTokens  int64 `json:"totalTokens,omitempty"`
	ToolCalls    int64 `json:"toolCalls,omitempty"`
	// CostUnknown marks spend whose price was unknown; it blocks further
	// autonomous reservations on the ledgers it reaches.
	CostUnknown bool `json:"costUnknown,omitempty"`
}

// LedgerScope says what a ledger bounds.
type LedgerScope string

// Ledger scopes.
const (
	LedgerTicket LedgerScope = "ticket"
	LedgerRepo   LedgerScope = "repo"
)

// Ledger is a finite allowance. A dispatch reserves before it starts and
// settles after its run; Reserved+Spent never exceeds Allowance+Granted at
// reservation time. Human input, retry and restart never reset it.
type Ledger struct {
	Key   LedgerKey   `json:"key"`
	Scope LedgerScope `json:"scope"`
	// PeriodStart and PeriodEnd bound a repository ledger.
	PeriodStart time.Time `json:"periodStart,omitzero"`
	PeriodEnd   time.Time `json:"periodEnd,omitzero"`
	Allowance   Spend     `json:"allowance"`
	// Granted is operator-added allowance, audited in gh/op.
	Granted Spend `json:"granted"`
	Spent   Spend `json:"spent"`
	// Reservations are open per dispatch; at most one per occupied slot.
	Reservations map[DispatchID]Spend `json:"reservations,omitempty"`
	// Estimated is true once any settlement charged a whole reservation
	// because the run's usage was unknown.
	Estimated bool `json:"estimated,omitempty"`
}

// EffectKind is a GitHub write the outbox can perform.
type EffectKind string

// Effect kinds.
const (
	// EffectComment posts to the Ticket's thread or a bound PR.
	EffectComment EffectKind = "comment"
	// EffectReviewReply replies in an inline review thread; a deleted
	// thread falls back to a PR conversation comment.
	EffectReviewReply EffectKind = "review_reply"
	// EffectPush updates TicketBranch on the remote to an exact commit.
	EffectPush EffectKind = "push"
	// EffectOpenPR opens a PR from TicketBranch.
	EffectOpenPR EffectKind = "open_pr"
	// EffectUpdatePR edits title, body or draft state of an own PR.
	EffectUpdatePR EffectKind = "update_pr"
)

// EffectState is the lifecycle of an outside write.
type EffectState string

// Effect states. Attempting is committed before the request is sent;
// an attempting intent found after a restart is reconciled, never resent.
const (
	EffectPending    EffectState = "pending"
	EffectAttempting EffectState = "attempting"
	EffectConfirmed  EffectState = "confirmed"
	EffectFailed     EffectState = "failed"
	// EffectUnknown: reconciliation could neither prove the write landed
	// nor prove it absent. Only an operator resolves it.
	EffectUnknown   EffectState = "unknown"
	EffectCancelled EffectState = "cancelled"
)

// OutboxIntent is one intended GitHub write, committed by a worker tool
// before anything is sent. Its ID, payload digest and generation fence it.
type OutboxIntent struct {
	ID     OutboxID    `json:"id"`
	Ticket TicketID    `json:"ticket"`
	Gen    uint32      `json:"gen"`
	Kind   EffectKind  `json:"kind"`
	State  EffectState `json:"state"`
	// Seq orders a Ticket's writes; the outbox publishes them in order.
	Seq     uint64        `json:"seq"`
	Payload EffectPayload `json:"payload"`
	// PayloadDigest is durable.Digest of Payload at intent time.
	PayloadDigest string `json:"payloadDigest"`
	Attempts      int    `json:"attempts"`
	// LastError is the latest failure or reconciliation finding.
	LastError string       `json:"lastError,omitempty"`
	Result    *EffectReply `json:"result,omitempty"`
	CreatedAt time.Time    `json:"createdAt"`
	// AttemptedAt bounds the reconciliation scan of comments.
	AttemptedAt time.Time `json:"attemptedAt,omitzero"`
	SettledAt   time.Time `json:"settledAt,omitzero"`
	RetryAt     time.Time `json:"retryAt,omitzero"`
}

// EffectPayload holds the fields of every effect kind; Kind selects which
// are meaningful. Targets are resolved by the controller from the Ticket,
// never accepted as free-form repository paths from the model.
type EffectPayload struct {
	// Number is the issue or PR a comment, reply or update targets.
	Number int `json:"number,omitempty"`
	// ReplyTo is the inline review comment a review reply answers.
	ReplyTo int64  `json:"replyTo,omitempty"`
	Body    string `json:"body,omitempty"`
	Title   string `json:"title,omitempty"`
	Draft   *bool  `json:"draft,omitempty"`
	// Branch and SHA are the exact ref update a push or PR open needs.
	Branch string `json:"branch,omitempty"`
	SHA    string `json:"sha,omitempty"`
	// LeaseSHA is the remote SHA the push expects to replace; empty for the
	// first push of the branch.
	LeaseSHA string `json:"leaseSha,omitempty"`
	Base     string `json:"base,omitempty"`
}

// EffectReply is what GitHub returned or what reconciliation found.
type EffectReply struct {
	// RemoteID is the comment ID or PR number.
	RemoteID int64  `json:"remoteId,omitempty"`
	URL      string `json:"url,omitempty"`
	SHA      string `json:"sha,omitempty"`
	// Reconciled is true when the result was found rather than returned.
	Reconciled bool `json:"reconciled,omitempty"`
}

// EffectSettlement is the outbox's report for one attempt.
type EffectSettlement struct {
	State     EffectState  `json:"state"`
	Result    *EffectReply `json:"result,omitempty"`
	Error     string       `json:"error,omitempty"`
	RetryAt   time.Time    `json:"retryAt,omitzero"`
	SettledAt time.Time    `json:"settledAt"`
}
