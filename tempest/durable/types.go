// Package durable defines the versioned data exchanged with Tempest stores.
// It contains no executable extension handlers or runtime credentials.
package durable

import (
	"context"
	"encoding/json"
	"errors"
)

// Store owns one fenced transaction domain. Commit acknowledges its durability
// point before making a batch visible. Close releases the writer lease.
type Store interface {
	Read(context.Context, Query) (Page, error)
	Commit(context.Context, Commit) (Receipt, error)
	Close(context.Context) error
}

// ReplayPolicy describes whether an interrupted whole invocation may repeat.
type ReplayPolicy string

const (
	// ReplayNever is the conservative default, including old journal records.
	ReplayNever ReplayPolicy = ""
	// ReplaySafe permits explicit scheduling with a compatible implementation.
	ReplaySafe ReplayPolicy = "safe"
)

// Kind identifies a record in the closed commit vocabulary.
type Kind string

const (
	// EntryKind appends immutable transcript or execution evidence.
	EntryKind Kind = "entry"
	// DocumentKind replaces or deletes a versioned application document.
	DocumentKind Kind = "document"
	// SubmissionKind records admission and subsequent delivery state.
	SubmissionKind Kind = "submission"
	// TaskKind records a task's persisted state, not executable code.
	TaskKind Kind = "task"
	// ChildKind records deterministic child creation intent.
	ChildKind Kind = "child"
	// MemoKind inserts a lifetime, first-write-wins decision.
	MemoKind Kind = "memo"
)

// Commit is an atomic, declarative batch. IDs are stable within its store.
type Commit struct {
	ID       string          `json:"id"`
	Expected []RevisionCheck `json:"expected,omitempty"`
	Changes  []Change        `json:"changes"`
}

// RevisionCheck compares a record revision; zero requires an absent record.
type RevisionCheck struct {
	SessionID string `json:"sessionId"`
	Kind      Kind   `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	ID        string `json:"id"`
	Revision  uint64 `json:"revision"`
}

// Change contains one versioned JSON value. Delete applies only to documents.
// Session adapters reject host-reserved namespaces before calling the store.
type Change struct {
	SessionID string          `json:"sessionId"`
	Kind      Kind            `json:"kind"`
	Namespace string          `json:"namespace,omitempty"`
	ID        string          `json:"id"`
	Version   uint32          `json:"version"`
	Value     json.RawMessage `json:"value,omitempty"`
	Delete    bool            `json:"delete,omitempty"`
}

// Receipt identifies the committed decision and its canonical digest.
type Receipt struct {
	ID     string `json:"id"`
	Seq    uint64 `json:"seq"`
	Digest string `json:"digest"`
}

// Query selects records at an immutable cut. Cursor is opaque and query-bound.
type Query struct {
	SessionID string `json:"sessionId,omitempty"`
	Kind      Kind   `json:"kind,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	ID        string `json:"id,omitempty"`
	AsOf      uint64 `json:"asOf,omitempty"`
	Cursor    string `json:"cursor,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	// History selects all revisions instead of the current value at the cut.
	History bool `json:"history,omitempty"`
	// CommitID resolves an ambiguous outcome without repeating the decision.
	CommitID string `json:"commitId,omitempty"`
	// Reverse returns newest revisions first, preserving the cursor's cut.
	Reverse bool `json:"reverse,omitempty"`
	// Active selects nonterminal submission receipts.
	Active bool `json:"active,omitempty"`
	// Application excludes host-reserved namespaces.
	Application bool `json:"application,omitempty"`
}

// SessionLeaser fences individual writers within an injected shared backend.
// The backend must revoke the lease when its owning process/domain closes.
type SessionLeaser interface {
	AcquireSession(context.Context, string) (func(), error)
}

// Record is a committed revision; deleted documents retain a tombstone.
type Record struct {
	Change
	Revision uint64 `json:"revision"`
	Seq      uint64 `json:"seq"`
	CommitID string `json:"commitId"`
}

// Page carries a bounded result and the stable cut for its next cursor.
type Page struct {
	Records    []Record  `json:"records"`
	Receipts   []Receipt `json:"receipts,omitempty"`
	NextCursor string    `json:"nextCursor,omitempty"`
	Seq        uint64    `json:"seq"`
}

// DocumentDefinition declares validation and projection policies for a namespace.
type DocumentDefinition struct {
	Namespace string          `json:"namespace"`
	Version   uint32          `json:"version"`
	Schema    json.RawMessage `json:"schema"`
	Initial   json.RawMessage `json:"initial"`
	History   HistoryPolicy   `json:"history"`
	Fork      ForkPolicy      `json:"fork"`
	Reset     ResetPolicy     `json:"reset"`
}

// HistoryPolicy controls historical value access.
type HistoryPolicy string

const (
	// HistoryRewindable retains prior values for as-of reads.
	HistoryRewindable HistoryPolicy = "rewindable"
	// HistoryLatest deliberately rejects historical document reads.
	HistoryLatest HistoryPolicy = "latest"
)

// ForkPolicy determines a new conversation's initial value.
type ForkPolicy string

const (
	// ForkAsOf samples the fork cut and requires rewindable history.
	ForkAsOf ForkPolicy = "asOf"
	// ForkCurrent samples the parent's current committed value.
	ForkCurrent ForkPolicy = "current"
	// ForkInitial uses the definition's initial value.
	ForkInitial ForkPolicy = "initial"
)

// ResetPolicy determines whether a context reset preserves application state.
type ResetPolicy string

const (
	// ResetPreserve keeps the current value.
	ResetPreserve ResetPolicy = "preserve"
	// ResetInitial restores the initial value.
	ResetInitial ResetPolicy = "initial"
)

// ErrConflict means the decision changed or a compared revision lost its race.
var ErrConflict = errors.New("durable conflict")

// ErrClosed means the store no longer owns its writer lease.
var ErrClosed = errors.New("durable store closed")

// ErrCorrupt means committed data cannot be recovered safely.
var ErrCorrupt = errors.New("durable store corruption")

// ErrUnsupported rejects required versions or unavailable capabilities.
var ErrUnsupported = errors.New("durable operation unsupported")

// ConflictError reports current revisions without adopting any changes.
type ConflictError struct{ Current []RevisionCheck }

func (e *ConflictError) Error() string { return ErrConflict.Error() }
func (e *ConflictError) Unwrap() error { return ErrConflict }
