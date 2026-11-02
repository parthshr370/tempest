package model

import "time"

// Actor is a GitHub account as the API reported it. Authorization keys on
// the numeric ID; Login is display text and can be renamed.
type Actor struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
	// Type is GitHub's account type: "User", "Bot" or "Organization".
	Type string `json:"type"`
}

// ThreadRef is an Issue or PR conversation in the configured repository.
// Branch CI events have Number 0.
type ThreadRef struct {
	Number int  `json:"number"`
	IsPR   bool `json:"isPr"`
}

// EventKind classifies a normalized event.
type EventKind string

// Event kinds. Opened events carry the title and body; edits of an opened
// body are not events (see IssueOpenedID).
const (
	EventIssueOpened         EventKind = "issue_opened"
	EventPROpened            EventKind = "pr_opened"
	EventComment             EventKind = "comment"
	EventCommentEdited       EventKind = "comment_edited"
	EventReviewComment       EventKind = "review_comment"
	EventReviewCommentEdited EventKind = "review_comment_edited"
	EventReview              EventKind = "review"
	EventReviewDismissed     EventKind = "review_dismissed"
	EventPRHead              EventKind = "pr_head"
	EventPRClosed            EventKind = "pr_closed"
	EventPRMerged            EventKind = "pr_merged"
	EventPRReopened          EventKind = "pr_reopened"
	EventPRCI                EventKind = "pr_ci"
	EventBranchCI            EventKind = "branch_ci"
)

// Command is a deterministic instruction parsed from the first line of a
// comment that addresses the account ("@bot stop"). Commands are never
// inferred by a model.
type Command string

// Commands an authorized actor can address to the account.
const (
	CommandNone   Command = ""
	CommandStop   Command = "stop"
	CommandStatus Command = "status"
)

// Event is one normalized GitHub fact. Text is untrusted outside data:
// prompts quote it, and nothing executes or authorizes because of its
// content except the explicit mention/command rules applied by admission.
type Event struct {
	ID     EventID   `json:"id"`
	Kind   EventKind `json:"kind"`
	Thread ThreadRef `json:"thread"`
	Actor  Actor     `json:"actor"`
	// At is GitHub's timestamp of the fact; ObservedAt is when the source
	// read it. The gap is the indexing delay the source must tolerate.
	At         time.Time `json:"at"`
	ObservedAt time.Time `json:"observedAt"`
	URL        string    `json:"url,omitempty"`
	// Title is set on opened events.
	Title string `json:"title,omitempty"`
	// Body is bounded by Config.Limits.MaxEventBodyBytes. BodyBytes keeps
	// the original size and Truncated says whether Body was cut.
	Body      string `json:"body,omitempty"`
	BodyBytes int    `json:"bodyBytes,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	// Mention is true when Body addresses the configured account with an
	// @login token. Command is set only when the first line is a command.
	Mention bool    `json:"mention,omitempty"`
	Command Command `json:"command,omitempty"`
	// CommentID is the GitHub ID of a comment, inline comment or review.
	CommentID int64 `json:"commentId,omitempty"`
	// InReplyTo is the root comment of an inline review thread.
	InReplyTo int64  `json:"inReplyTo,omitempty"`
	Path      string `json:"path,omitempty"`
	Line      int    `json:"line,omitempty"`
	// ReviewState is APPROVED, CHANGES_REQUESTED, COMMENTED or DISMISSED.
	ReviewState string `json:"reviewState,omitempty"`
	// CommitSHA pins a review, PR head, merge or CI state to a commit.
	CommitSHA string `json:"commitSha,omitempty"`
	// Branch names the default branch for EventBranchCI.
	Branch string      `json:"branch,omitempty"`
	CI     *CISnapshot `json:"ci,omitempty"`
}

// CISnapshot is the settled check state of one commit. A snapshot is
// emitted only when no check is pending, so recovery is never claimed while
// checks still run.
type CISnapshot struct {
	SHA         string   `json:"sha"`
	Fingerprint string   `json:"fingerprint"`
	Failing     []string `json:"failing,omitempty"`
	Passing     int      `json:"passing"`
	Skipped     int      `json:"skipped,omitempty"`
}

// DecisionKind is what admission did with an event.
type DecisionKind string

// Decision kinds.
const (
	// DecisionIgnored: the account's own message, or a fact with no reader.
	DecisionIgnored DecisionKind = "ignored"
	// DecisionObserved: an observe-stream fact recorded without work.
	DecisionObserved DecisionKind = "observed"
	// DecisionDenied: a mention or command from an actor without authority.
	DecisionDenied DecisionKind = "denied"
	// DecisionRequest: created a Ticket and its Mandate.
	DecisionRequest DecisionKind = "request"
	// DecisionInput: appended as Input to the listed Tickets.
	DecisionInput DecisionKind = "input"
	// DecisionReopen: reopened an ended Ticket under its existing Mandate.
	DecisionReopen DecisionKind = "reopen"
	// DecisionStop: stopped the listed Tickets.
	DecisionStop DecisionKind = "stop"
)

// Decision is the immutable admission outcome of one event, recorded in the
// same commit as the event, its Inputs, Tickets and the cursor move.
type Decision struct {
	Event        EventID      `json:"event"`
	Kind         DecisionKind `json:"kind"`
	Tickets      []TicketID   `json:"tickets,omitempty"`
	Reason       string       `json:"reason"`
	PolicyDigest string       `json:"policyDigest"`
	DecidedAt    time.Time    `json:"decidedAt"`
}
