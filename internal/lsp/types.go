// Package lsp is Tempest's generic stdio Language Server Protocol 3.17 client.
//
// A Manager owns at most one lazily started server per route for one session
// workspace. It synchronizes disk-backed document snapshots, converts
// one-based Unicode-scalar positions to UTF-16, and answers definition,
// references, rename (as an anchored edit plan, never a write) and diagnostics
// queries with bounded, normalized JSON. File access goes only through the
// caller's contained Workspace.
package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Config selects the trusted server definitions for a session. JSON settings
// use snake_case.
type Config struct {
	Enabled bool           `json:"enabled"`
	Servers []ServerConfig `json:"servers"`
}

// ServerConfig replaces a built-in route's executable, arguments and settings.
type ServerConfig struct {
	Name                  string            `json:"name"`
	Command               string            `json:"command"`
	Args                  []string          `json:"args,omitempty"`
	Env                   map[string]string `json:"env,omitempty"`
	Disabled              bool              `json:"disabled,omitempty"`
	InitializationOptions json.RawMessage   `json:"initialization_options,omitempty"`
	Settings              json.RawMessage   `json:"settings,omitempty"`
}

// Document is an immutable, contained snapshot of one workspace file.
type Document struct {
	Path   string // canonical, slash-separated root-relative filename
	Text   []byte // owned immutable snapshot, original UTF-8 bytes
	Anchor string // SHA-256 of Text, before BOM/newline normalization
}

// Workspace is the contained filesystem view the Manager reads through.
type Workspace interface {
	Root() string                                       // canonical absolute session cwd
	ReadFile(context.Context, string) (Document, error) // contained, queue-serialized
	Snapshot(context.Context) ([]Document, error)       // bounded rename-only workspace scan
	Close() error
}

// Position is one-based; Column counts Unicode scalar values from the start
// of the line. On the first line a leading byte-order mark isn't counted.
type Position struct{ Line, Column int }

// Query methods.
const (
	MethodDefinition  = "definition"
	MethodReferences  = "references"
	MethodRename      = "rename"
	MethodDiagnostics = "diagnostics"
)

// Query fields map to the lsp_* tool arguments. Defaults are applied by the
// tool decoder; a zero MaxResults selects the method default.
type Query struct {
	Method, Path, Server string
	Position             Position
	NewName              string
	IncludeDeclaration   bool
	MaxResults           int
	Severity             string
	Wait                 time.Duration
}

// Result is a validated normalized value, never a raw server frame. Callers
// emit Value both as one text block and as structured details; the combined
// encoded size is bounded by the Manager.
type Result struct {
	Value json.RawMessage
}

// Stable error codes.
const (
	CodeConfig            = "lsp_config"
	CodeDisabled          = "lsp_disabled"
	CodeClosed            = "lsp_closed"
	CodeInvalidArgument   = "lsp_invalid_argument"
	CodeInvalidPosition   = "lsp_invalid_position"
	CodePath              = "lsp_path"
	CodeInvalidDocument   = "lsp_invalid_document"
	CodeDocumentLimit     = "lsp_document_limit"
	CodeNoServer          = "lsp_no_server"
	CodeServerMissing     = "lsp_server_missing"
	CodeServerUnavailable = "lsp_server_unavailable"
	CodeUnsupported       = "lsp_unsupported"
	CodeProtocol          = "lsp_protocol"
	CodeOutputLimit       = "lsp_output_limit"
	CodeServerError       = "lsp_server_error"
	CodeContentModified   = "lsp_content_modified"
	CodeRenameScopeLimit  = "lsp_rename_scope_limit"
	CodeRenameRejected    = "lsp_rename_rejected"
	CodeTimeout           = "lsp_timeout"
	CodeCancelled         = "lsp_cancelled"
)

// Error is every failure returned by Query. Detail is bounded and never holds
// raw server configuration or environment.
type Error struct {
	Code   string
	Detail string
	// RetryAfter is set for lsp_server_unavailable while a cooldown or open
	// circuit blocks a new start.
	RetryAfter time.Duration
	cause      error
}

func (e *Error) Error() string {
	if e.Detail == "" {
		return e.Code
	}
	return e.Code + ": " + e.Detail
}

// Unwrap exposes the workspace or context error that caused e, if any.
func (e *Error) Unwrap() error { return e.cause }

func newError(code, detail string) *Error { return &Error{Code: code, Detail: detail} }

func wrapError(code, detail string, cause error) *Error {
	return &Error{Code: code, Detail: detail, cause: cause}
}

// asError returns err as an *Error, wrapping context and workspace failures.
func asError(err error) *Error {
	if err == nil {
		return nil
	}
	var lspErr *Error
	if errors.As(err, &lspErr) {
		return lspErr
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return wrapError(CodeTimeout, "language server request exceeded its deadline", err)
	case errors.Is(err, context.Canceled):
		return wrapError(CodeCancelled, "language server request cancelled", err)
	}
	return wrapError(CodePath, boundText(err.Error(), serverMessageMax), err)
}
