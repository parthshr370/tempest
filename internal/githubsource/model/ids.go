package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ControlSession is the durable SessionID of every control-journal record.
// The store holds one repository, so a constant keeps record keys short.
const ControlSession = "github"

// Control-journal namespaces. See the package documentation for the value
// stored under each.
const (
	NamespaceMeta       = "gh/meta"
	NamespacePolicy     = "gh/policy"
	NamespaceCursor     = "gh/cursor"
	NamespaceThread     = "gh/thread"
	NamespaceEvent      = "gh/event"
	NamespaceDecision   = "gh/decision"
	NamespaceTicket     = "gh/ticket"
	NamespaceMandate    = "gh/mandate"
	NamespaceInput      = "gh/input"
	NamespaceAssignment = "gh/assignment"
	NamespaceDispatch   = "gh/dispatch"
	NamespaceSession    = "gh/session"
	NamespacePR         = "gh/pr"
	NamespaceOutbox     = "gh/outbox"
	NamespaceLedger     = "gh/ledger"
	NamespaceOperator   = "gh/op"
)

// Worker-session document namespace, declared by the github extension. The
// controller writes it once before the worker's first provider request.
const (
	WorkerNamespace = "github/ticket"
	WorkerDocument  = "ticket"
)

// shortDigestHex is the hex length of digests embedded in identities: 48
// bits keep Ticket IDs and branch names short while a collision inside one
// repository's lifetime stays negligible.
const shortDigestHex = 12

// ticketDigestHex keeps Ticket IDs short enough to read aloud in a comment.
const ticketDigestHex = 6

// EventID is the deterministic identity of one normalized GitHub fact. The
// same GitHub object at the same version always yields the same ID, so a
// reread page is deduplicated by the control journal, never by time windows.
type EventID string

// TicketID names one authorized unit of work. It is derived from the event
// that requested it, so a duplicate request cannot create a second Ticket.
type TicketID string

// DispatchID names one scheduled worker activation of a Ticket.
type DispatchID string

// OutboxID names one intended GitHub write.
type OutboxID string

// LedgerKey names one budget ledger document.
type LedgerKey string

// CursorKey names one repository-wide read position. Per-thread positions
// live in the Thread document so a thread read commits with its own events.
type CursorKey string

// Repository-wide cursors.
const (
	CursorNotifications  CursorKey = "notifications"
	CursorIssues         CursorKey = "stream:issues"
	CursorComments       CursorKey = "stream:comments"
	CursorReviewComments CursorKey = "stream:review_comments"
	CursorBranchCI       CursorKey = "ci:default"
)

// IssueOpenedID identifies an Issue or PR title and body. Issues and PRs
// share one number sequence. Later body edits keep this ID, so editing an
// old body never creates a request; a fresh comment does.
func IssueOpenedID(number int) EventID {
	return EventID("issue:" + strconv.Itoa(number) + ":opened")
}

// CommentID identifies one version of an issue or PR conversation comment.
// The version is GitHub's updated_at in Unix seconds.
func CommentID(commentID int64, updatedAt time.Time) EventID {
	return EventID(fmt.Sprintf("comment:%d:v%d", commentID, updatedAt.Unix()))
}

// ReviewCommentID identifies one version of an inline review comment.
func ReviewCommentID(commentID int64, updatedAt time.Time) EventID {
	return EventID(fmt.Sprintf("review_comment:%d:v%d", commentID, updatedAt.Unix()))
}

// ReviewID identifies a submitted review, including a bodyless approval.
func ReviewID(reviewID int64) EventID {
	return EventID(fmt.Sprintf("review:%d", reviewID))
}

// ReviewDismissedID identifies the dismissal of a submitted review.
func ReviewDismissedID(reviewID int64) EventID {
	return EventID(fmt.Sprintf("review:%d:dismissed", reviewID))
}

// PRHeadID identifies a PR head moving to sha.
func PRHeadID(number int, sha string) EventID {
	return EventID(fmt.Sprintf("pr:%d:head:%s", number, sha))
}

// PRClosedID identifies a PR or Issue closing without merge at closedAt.
func PRClosedID(number int, closedAt time.Time) EventID {
	return EventID(fmt.Sprintf("pr:%d:closed:%d", number, closedAt.Unix()))
}

// PRMergedID identifies a PR merge by its merge commit.
func PRMergedID(number int, mergeSHA string) EventID {
	return EventID(fmt.Sprintf("pr:%d:merged:%s", number, mergeSHA))
}

// PRReopenedID identifies a PR or Issue reopening at reopenedAt.
func PRReopenedID(number int, reopenedAt time.Time) EventID {
	return EventID(fmt.Sprintf("pr:%d:reopened:%d", number, reopenedAt.Unix()))
}

// PRCIID identifies a settled CI state of one PR head. The fingerprint is
// CIFingerprint of the completed check set, so only a changed outcome on the
// same head is a new event.
func PRCIID(number int, headSHA, fingerprint string) EventID {
	return EventID(fmt.Sprintf("ci:pr:%d:%s:%s", number, headSHA, fingerprint))
}

// BranchCIID identifies a settled CI state of a default-branch commit.
func BranchCIID(branch, sha, fingerprint string) EventID {
	return EventID(fmt.Sprintf("ci:branch:%s:%s:%s", branch, sha, fingerprint))
}

// CIFingerprint digests check outcomes given as sorted "name=conclusion"
// lines. Callers sort; the digest does not, so it stays a pure function.
func CIFingerprint(sortedOutcomes []string) string {
	return shortDigest(strings.Join(sortedOutcomes, "\n"))
}

// NewTicketID derives the Ticket identity from the requesting event:
// "i42-a1b2c3" for an Issue thread, "p42-a1b2c3" for a PR thread.
func NewTicketID(thread ThreadRef, request EventID) TicketID {
	kind := "i"
	if thread.IsPR {
		kind = "p"
	}
	digest := shortDigest(string(request))[:ticketDigestHex]
	return TicketID(fmt.Sprintf("%s%d-%s", kind, thread.Number, digest))
}

// TicketBranch is the branch a Ticket's worktree commits to. Outbox pushes
// and PR ownership checks accept only this branch for the Ticket.
func TicketBranch(id TicketID) string { return "agent/" + string(id) }

// InputKey is the gh/input record ID of a Ticket's seq-th input.
func InputKey(id TicketID, seq uint64) string {
	return fmt.Sprintf("%s:%d", id, seq)
}

// AssignmentKey is the gh/assignment record ID of a Ticket's gen-th attempt.
func AssignmentKey(id TicketID, gen uint32) string {
	return fmt.Sprintf("%s:%d", id, gen)
}

// NewDispatchID names a Ticket's seq-th activation.
func NewDispatchID(id TicketID, seq uint64) DispatchID {
	return DispatchID(fmt.Sprintf("%s:d%d", id, seq))
}

// LaunchRequestID is the durable submission key of a dispatch that starts
// a worker run (placement input): gh:<ticket>:d<seq>:<gen>.
func LaunchRequestID(id TicketID, dispatchSeq uint64, gen uint32) string {
	return requestID(id, fmt.Sprintf("d%d", dispatchSeq), gen)
}

// DeliveryRequestID is the durable queue key of one input steered or
// queued into a running worker: gh:<ticket>:i<seq>:<gen>.
func DeliveryRequestID(id TicketID, inputSeq uint64, gen uint32) string {
	return requestID(id, fmt.Sprintf("i%d", inputSeq), gen)
}

// requestID builds gh:<ticket>:<event>:<gen>. Keys stay far below the
// 128-byte limit of tempest.Session.Submit: a TicketID is at most 18 bytes
// and the other fields are decimal counters, so a key is under 70 bytes.
func requestID(id TicketID, event string, gen uint32) string {
	return fmt.Sprintf("gh:%s:%s:%d", id, event, gen)
}

// NewOutboxID names the write a worker tool call intends. The provider's
// tool-call ID is digested because its length and alphabet vary by provider.
func NewOutboxID(id TicketID, gen uint32, toolCallID string) OutboxID {
	return OutboxID(fmt.Sprintf("%s:o%d:%s", id, gen, shortDigest(toolCallID)))
}

// OutboxMarker is appended to every comment the outbox posts. Reconciliation
// finds a comment by this marker, never by "latest comment" heuristics.
func OutboxMarker(id OutboxID) string {
	return "<!-- tempest:" + string(id) + " -->"
}

// TicketLedger is the lifetime ledger of one Ticket.
func TicketLedger(id TicketID) LedgerKey { return LedgerKey("ticket:" + string(id)) }

// RepoLedger is the repository ledger of the period starting at start.
func RepoLedger(start time.Time) LedgerKey {
	return LedgerKey(fmt.Sprintf("repo:%d", start.Unix()))
}

func shortDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:shortDigestHex]
}
