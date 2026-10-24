// Package extension defines operator-installed agent bundles and scoped handlers.
package extension

import (
	"context"
	"encoding/json"

	"go.harness.dev/harness/tempest/durable"
)

// ReplayPolicy describes whether repeating the complete effective chain is safe.
type ReplayPolicy = durable.ReplayPolicy

const (
	ReplayNever = durable.ReplayNever
	ReplaySafe  = durable.ReplaySafe
)

// EffectClass is immutable permission metadata, independent of the model-visible name.
type EffectClass string

const (
	EffectRead    EffectClass = "read"
	EffectWrite   EffectClass = "write"
	EffectExecute EffectClass = "execute"
)

// Selection names a bundle. Required bundles must be available before new work.
type Selection struct {
	ID       string `json:"id"`
	Required bool   `json:"required,omitempty"`
}

// Definition is a complete replacement of one bundle. Tasks are not supported yet.
type Definition struct {
	ID            string                       `json:"id"`
	Revision      string                       `json:"revision"`
	ConfigVersion uint32                       `json:"configVersion,omitempty"`
	StateVersion  uint32                       `json:"stateVersion,omitempty"`
	Sections      []Section                    `json:"sections,omitempty"`
	Tools         []Tool                       `json:"tools,omitempty"`
	Hooks         []Hook                       `json:"hooks,omitempty"`
	Wraps         []Wrap                       `json:"wraps,omitempty"`
	Documents     []durable.DocumentDefinition `json:"documents,omitempty"`
	Presets       []AgentPreset                `json:"presets,omitempty"`
	Tasks         []TaskDefinition             `json:"tasks,omitempty"`
	Close         func(context.Context) error  `json:"-"`
	available     func() bool
}

// TaskDefinition decodes unsupported manifest descriptors so installation can
// reject them explicitly. No task capability or phase handler is advertised.
type TaskDefinition struct {
	Name        string          `json:"name"`
	Version     uint32          `json:"version"`
	InputSchema json.RawMessage `json:"inputSchema"`
	StateSchema json.RawMessage `json:"stateSchema"`
	Initial     json.RawMessage `json:"initial"`
}

// Tool is a namespaced definition. Never is the conservative replay default.
type Tool struct {
	Name         string                                                  `json:"name"`
	Title        string                                                  `json:"title,omitempty"`
	Description  string                                                  `json:"description"`
	InputSchema  json.RawMessage                                         `json:"inputSchema"`
	OutputSchema json.RawMessage                                         `json:"outputSchema,omitempty"`
	Effect       EffectClass                                             `json:"effect"`
	Replay       ReplayPolicy                                            `json:"replay,omitempty"`
	Execute      func(context.Context, Call, Invocation) (Result, error) `json:"-"`
}

// Call contains the immutable identity and already gated arguments.
type Call struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Content follows the text/resource content envelope used by tool transports.
type Content struct {
	Type         string `json:"type"`
	Text         string `json:"text,omitempty"`
	Data         string `json:"data,omitempty"`
	MimeType     string `json:"mimeType,omitempty"`
	RefStore     string `json:"refStore,omitempty"`
	RefKey       string `json:"refKey,omitempty"`
	RefMediaType string `json:"refMediaType,omitempty"`
	RefFilename  string `json:"refFilename,omitempty"`
	RefSizeBytes int64  `json:"refSizeBytes,omitempty"`
	RefPageCount int    `json:"refPageCount,omitempty"`
}

// Result keeps model output, machine output and UI-only details separate.
type Result struct {
	Content           []Content       `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	Details           json.RawMessage `json:"details,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
	Usage             json.RawMessage `json:"usage,omitempty"`
	Control           string          `json:"control,omitempty"`
}

// Section renders from read-only committed state before each new provider request.
type Section struct {
	Name   string                                          `json:"name"`
	Render func(context.Context, ReadView) (string, error) `json:"-"`
}

// ReadView exposes no mutation capabilities.
type ReadView interface {
	Identity() InvocationIdentity
	Read(context.Context, durable.Query) (durable.Page, error)
	Environment(context.Context) (EnvironmentLease, error)
}

// EnvironmentLease is a host-scoped identity, not an arbitrary filesystem proxy.
type EnvironmentLease interface {
	Identity() string
	ReadFile(context.Context, string, int64, int) (string, error)
}

// HookEvent identifies one typed operation boundary.
type HookEvent string

const (
	BeforeRequest    HookEvent = "before_request"
	AfterResponse    HookEvent = "after_response"
	BeforeTool       HookEvent = "before_tool"
	AfterTool        HookEvent = "after_tool"
	OnYield          HookEvent = "on_yield"
	BeforeCompaction HookEvent = "before_compaction"
)

// Hook executes in selected bundle order.
type Hook struct {
	Name   string                                                             `json:"name"`
	Event  HookEvent                                                          `json:"event"`
	Replay ReplayPolicy                                                       `json:"replay,omitempty"`
	Handle func(context.Context, HookInput, Invocation) (HookDecision, error) `json:"-"`
}

// HookInput contains only values appropriate to the selected event.
type HookInput struct {
	Event    HookEvent       `json:"event"`
	Call     *Call           `json:"call,omitempty"`
	Result   *Result         `json:"result,omitempty"`
	System   string          `json:"system,omitempty"`
	Response json.RawMessage `json:"response,omitempty"`
}

// HookDecision is a closed event-specific union: continue, deny, rewrite, redact or continue_with.
type HookDecision struct {
	Kind         string          `json:"kind"`
	Reason       string          `json:"reason,omitempty"`
	Arguments    json.RawMessage `json:"arguments,omitempty"`
	Result       *Result         `json:"result,omitempty"`
	System       *string         `json:"system,omitempty"`
	Continuation string          `json:"continuation,omitempty"`
}

// Wrap decorates an immutable tool identity. A wrap can only weaken replay safety.
type Wrap struct {
	Name   string                                                        `json:"name"`
	Target string                                                        `json:"target"`
	Replay ReplayPolicy                                                  `json:"replay,omitempty"`
	Handle func(context.Context, Call, Invocation, Next) (Result, error) `json:"-"`
}

// Next invokes the pinned next implementation once, with the gated arguments.
type Next interface {
	Call(context.Context) (Result, error)
}

// InvocationIdentity remains fixed for the lifetime of one capability grant.
type InvocationIdentity struct {
	SessionID    string `json:"sessionId"`
	InvocationID string `json:"invocationId"`
	Phase        string `json:"phase,omitempty"`
	Attempt      uint64 `json:"attempt"`
	Revision     string `json:"revision"`
	ExtensionID  string `json:"extensionId"`
	Handler      string `json:"handler"`
	Token        string `json:"token"`
}

// OutputChunk is bounded, ordered progress scoped to one invocation.
type OutputChunk struct {
	Number  uint64    `json:"number"`
	Content []Content `json:"content"`
}

// Invocation is a revocable host grant. Every method rejects calls after settlement.
type Invocation interface {
	ReadView
	Commit(context.Context, durable.Commit) (durable.Receipt, error)
	Memo(context.Context, string, json.RawMessage) (json.RawMessage, error)
	ReadMemo(context.Context, string) (json.RawMessage, bool, error)
	Output(context.Context, OutputChunk) error
	CallTool(context.Context, string, string, json.RawMessage) (Result, error)
}

// AgentPreset is data. It cannot launch executables or increase a child's authority.
type AgentPreset struct {
	ID           string   `json:"id"`
	Version      uint32   `json:"version"`
	Description  string   `json:"description"`
	Instructions string   `json:"instructions"`
	Model        string   `json:"model,omitempty"`
	AllowedTools []string `json:"allowedTools"`
}
