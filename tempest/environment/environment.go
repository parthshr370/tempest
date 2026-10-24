// Package environment binds workspace operations to one identified workspace.
package environment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
)

// Reference identifies the same workspace generation across reconnects.
// Cwd is slash-separated and relative to the daemon's workspace root.
type Reference struct {
	ID         string `json:"id"`
	Generation string `json:"generation"`
	Cwd        string `json:"cwd"`
}

// Validate rejects references that could move an operation outside its lease.
func (r Reference) Validate() error {
	if r.ID == "" || r.Generation == "" || r.Cwd == "" || strings.Contains(r.Cwd, "\\") || path.IsAbs(r.Cwd) || path.Clean(r.Cwd) != r.Cwd || r.Cwd == ".." || strings.HasPrefix(r.Cwd, "../") {
		return errors.New("invalid environment identity, generation or relative cwd")
	}
	return nil
}

// Key includes generation and cwd, not just the operator's workspace name.
func (r Reference) Key() string { b, _ := json.Marshal(r); return string(b) }

// Capability is an independently negotiated workspace service.
type Capability string

const (
	// Files supplies bounded reads, directory listing and conditional edits.
	Files Capability = "files"
	// Search supplies literal and regular-expression search.
	Search Capability = "search"
	// Exec supplies command and argv execution.
	Exec Capability = "exec"
	// Checkpoint supplies workspace capture and rewind on the workspace host.
	Checkpoint Capability = "checkpoint"
	// Eval supplies workspace-host interpreter kernels.
	Eval Capability = "eval"
	// LSP supplies workspace-host language servers.
	LSP Capability = "lsp"
	// Git supplies workspace-host Git operations and isolated child worktrees.
	Git Capability = "git"
	// AST supplies workspace-host syntax-aware search and edits.
	AST Capability = "ast"
	// Cancel supplies acknowledged invocation cancellation.
	Cancel Capability = "cancel"
	// Lookup supplies durable deduplication and terminal result lookup.
	Lookup Capability = "lookup"
	// Permissions supplies workspace-host facts for the host's permission gate.
	Permissions Capability = "permission_inspection"
)

// Capabilities is the immutable negotiated service set.
type Capabilities []Capability

// Has reports whether the peer negotiated c.
func (cs Capabilities) Has(c Capability) bool {
	for _, have := range cs {
		if have == c {
			return true
		}
	}
	return false
}

// ToolCapability classifies filesystem-sensitive built-ins before construction.
func ToolCapability(name string) Capability {
	switch name {
	case "read", "read_bytes", "stat", "write", "edit", "hashline", "ls", "view_image":
		return Files
	case "grep", "find":
		return Search
	case "bash", "exec_argv":
		return Exec
	case "inspect":
		return Permissions
	case "checkpoint", "rewind":
		return Checkpoint
	case "eval":
		return Eval
	case "lsp_definition", "lsp_references", "lsp_rename", "lsp_diagnostics":
		return LSP
	case "integrate", "gh", "gh_checkout", "manifest_diff":
		return Git
	case "ast_grep", "ast_edit":
		return AST
	}
	return ""
}

// Call uses the built-in's bounded input and result schema on both adapters.
// Key is the journal occurrence ID, never the provider's reusable tool-call ID.
type Call struct {
	Environment Reference       `json:"environment"`
	Key         string          `json:"key"`
	Operation   string          `json:"operation"`
	Arguments   json.RawMessage `json:"arguments"`
}

// ReadRequest pages UTF-8 bytes without rendering tool line anchors.
type ReadRequest struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
	Limit  int    `json:"limit"`
}

// ReadResult contains at most the requested bytes.
type ReadResult struct {
	Text string `json:"text"`
}

// StatRequest names one workspace file or directory.
type StatRequest struct {
	Path string `json:"path"`
}

// FileInfo is portable stat metadata, not a host-specific os.FileInfo.
type FileInfo struct {
	Size      int64  `json:"size"`
	Mode      uint32 `json:"mode"`
	Directory bool   `json:"directory"`
}

// ExecRequest launches argv without interpreting any argument as shell syntax.
type ExecRequest struct {
	Argv           []string `json:"argv"`
	TimeoutSeconds int      `json:"timeoutSeconds,omitempty"`
	MaxBytes       int      `json:"maxBytes,omitempty"`
}

// ExecResult retains a bounded output prefix and the actual exit status.
type ExecResult struct {
	Output    string `json:"output"`
	ExitCode  int    `json:"exitCode"`
	Truncated bool   `json:"truncated"`
}

// Cancellation separates an abort request from a confirmed stopped invocation.
type Cancellation struct {
	Requested bool `json:"requested"`
	Confirmed bool `json:"confirmed"`
}

// Outcome is a daemon-owned durable invocation receipt.
type Outcome struct {
	Found    bool            `json:"found"`
	Complete bool            `json:"complete"`
	Result   json.RawMessage `json:"result,omitempty"`
	Error    string          `json:"error,omitempty"`
}

// Lease owns a workspace connection. A transport error never authorizes replay.
type Lease interface {
	Reference() Reference
	Capabilities() Capabilities
	Call(context.Context, Call) (json.RawMessage, error)
	Cancel(context.Context, string) (Cancellation, error)
	Lookup(context.Context, string) (Outcome, error)
	Close() error
}

// Resolver opens only operator-configured environments for persisted references.
type Resolver func(context.Context, Reference) (Lease, error)

// ErrUnsupported means the workspace host did not negotiate a required service.
var ErrUnsupported = errors.New("environment capability unavailable")

// ErrIdentity means recovery reached a different workspace or generation.
var ErrIdentity = errors.New("environment identity mismatch")

// ErrUnknown means a dispatched operation may have taken effect.
var ErrUnknown = errors.New("environment outcome unknown")

// Require rejects missing services before their local resources can be created.
func Require(l Lease, c Capability) error {
	if l != nil && !l.Capabilities().Has(c) {
		return fmt.Errorf("%w: %s", ErrUnsupported, c)
	}
	return nil
}

// IsLocal reports whether a lease explicitly exposes its host workspace root.
func IsLocal(l Lease) bool {
	if l == nil {
		return true
	}
	_, ok := l.(interface{ LocalRoot() string })
	return ok
}

// Match fences calls and resumed work to one workspace generation and cwd.
func Match(want, got Reference) error {
	if err := got.Validate(); err != nil {
		return err
	}
	if want.ID != "" && want.ID != got.ID || want.Generation != "" && want.Generation != got.Generation || want.Cwd != "" && want.Cwd != got.Cwd {
		return fmt.Errorf("%w: wanted %s, got %s", ErrIdentity, want.Key(), got.Key())
	}
	return nil
}

// Location groups journals by workspace without turning remote cwd into a host path.
func Location(reference Reference) string {
	cwd := reference.Cwd
	if cwd == "" {
		cwd = "."
	}
	return "env://" + url.PathEscape(reference.ID) + "/" + cwd
}

// IsLocation reports whether text is a canonical identified-workspace location.
func IsLocation(text string) bool {
	remainder, ok := strings.CutPrefix(text, "env://")
	if !ok {
		return false
	}
	name, cwd, ok := strings.Cut(remainder, "/")
	if !ok {
		return false
	}
	id, err := url.PathUnescape(name)
	if err != nil {
		return false
	}
	reference := Reference{ID: id, Generation: "location", Cwd: cwd}
	return reference.Validate() == nil && Location(reference) == text
}
