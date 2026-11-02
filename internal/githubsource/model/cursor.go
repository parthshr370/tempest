package model

import "time"

// Cursor is a committed repository-wide read position. It moves only in the
// commit that stores everything read up to it.
type Cursor struct {
	Key CursorKey `json:"key"`
	// Since is the newest updated_at fully read. For notifications it is a
	// listing position only: it schedules thread reconciliation and is never
	// used as a lower bound for reading a thread's comments.
	Since time.Time `json:"since,omitzero"`
	// LastModified and ETag are conditional-request validators.
	LastModified string `json:"lastModified,omitempty"`
	ETag         string `json:"etag,omitempty"`
	// EnabledAt is when an observe stream was first enabled. Nothing older
	// is read: enabling a stream does not import a backlog.
	EnabledAt time.Time `json:"enabledAt,omitzero"`
	// BranchSHA and Fingerprint are the last settled default-branch CI.
	BranchSHA   string `json:"branchSha,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

// Policy is the admission part of the operator configuration, stored by
// digest so every Mandate and Decision names the exact rules it used.
type Policy struct {
	Digest string    `json:"digest"`
	Mode   Mode      `json:"mode"`
	Actors []ActorID `json:"actors"`
	Bots   []ActorID `json:"bots"`
	// Account is the dedicated GitHub user the source acts as.
	Account  ActorID   `json:"account"`
	LoadedAt time.Time `json:"loadedAt"`
}

// OperatorCommand is the audit record of one operator control.
type OperatorCommand struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	// Target is the Ticket or outbox intent the command acted on.
	Target string `json:"target,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Grant is set for budget grants.
	Grant *Spend    `json:"grant,omitempty"`
	At    time.Time `json:"at"`
}
