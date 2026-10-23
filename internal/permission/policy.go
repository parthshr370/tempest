package permission

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.harness.dev/harness/internal/cloudcmd"
)

// Tier is a tool's approval tier. Unknown tools are [TierExec].
type Tier int

const (
	// TierRead allows tools that inspect existing data.
	TierRead Tier = iota
	// TierWrite allows tools that change workspace content.
	TierWrite
	// TierExec allows tools that may execute arbitrary actions.
	TierExec
)

// Decision is a per-tool user policy outcome.
type Decision int

const (
	// DecideAllow allows a tool invocation.
	DecideAllow Decision = iota
	// DecideDeny denies a tool invocation.
	DecideDeny
	// DecidePrompt requires interactive approval for a tool invocation.
	DecidePrompt
)

// PromptRequest describes an approval prompt for a tool call.
type PromptRequest struct {
	ToolCallID                                  string
	JobID, ChildSessionID, ChildToolCallID, Cwd string
	Args                                        json.RawMessage
	// Tool names the tool requesting approval.
	Tool string
	// Tier is the approval tier for Tool.
	Tier Tier
	// Reason provides a safety-override reason when one applies.
	Reason string
}

// Prompter asks a human to approve a tool call.
type Prompter interface {
	// Approve selects one of the four standard permission choices.
	Approve(ctx context.Context, req PromptRequest) (Choice, error)
}

// Policy is the per-session approval policy layered over the mode.
type Policy struct {
	// Overrides stores per-tool allow, deny, or prompt decisions.
	Overrides map[string]Decision
	// MCPDefaults supplies trusted server defaults without replacing exact overrides.
	MCPDefaults map[string]Decision
	// Prompter resolves prompt decisions; nil denies them in headless sessions.
	Prompter Prompter
	// CloudRules are normalized exact kube-context rules (see
	// cloudcmd.NormalizeRules). Copies of Policy share the slice; it is
	// replaced, never mutated in place.
	CloudRules []cloudcmd.Rule
	// promptOverrides stores session choices for ordinary asks. Guard asks
	// never use or set these overrides.
	promptOverrides map[string]Decision
}

// policyMu protects in-memory overrides. Approval never holds it while waiting.
var policyMu sync.Mutex

// ReplaceMCPDefaults installs a complete catalog's defaults between runs.
// Copies of Policy share this session-owned map, just like exact overrides.
func (p *Policy) ReplaceMCPDefaults(defaults map[string]Decision) {
	policyMu.Lock()
	defer policyMu.Unlock()
	if p.MCPDefaults == nil {
		p.MCPDefaults = make(map[string]Decision)
	}
	clear(p.MCPDefaults)
	for name, decision := range defaults {
		p.MCPDefaults[name] = decision
	}
}

// Choice is the selected scope and decision of an approval.
type Choice string

const (
	// AllowOnce approves only the current tool invocation.
	AllowOnce Choice = "allow_once"
	// AllowAlways approves subsequent invocations, except destructive guard asks.
	AllowAlways Choice = "allow_always"
	// RejectOnce denies only the current tool invocation.
	RejectOnce Choice = "reject_once"
	// RejectAlways denies subsequent invocations, except destructive guard asks.
	RejectAlways Choice = "reject_always"
)

// Source identifies the policy layer that made a decision.
type Source string

const (
	// SourceBypass identifies an unrestricted permission mode.
	SourceBypass Source = "bypass"
	// SourceGuard identifies a destructive-command guard denial.
	SourceGuard Source = "guard"
	// SourceOverride identifies a per-tool configured or interactive decision.
	SourceOverride Source = "override"
	// SourceMode identifies the current mode's default policy.
	SourceMode Source = "mode"
	// SourcePrompt identifies an explicit approval answer.
	SourcePrompt Source = "prompt"
	// SourcePromptTimeout identifies an unanswered approval deadline.
	SourcePromptTimeout Source = "prompt_timeout"
	// SourcePromptCancelled identifies an aborted approval.
	SourcePromptCancelled Source = "prompt_cancelled"
	// SourceNoPrompter identifies an approval requirement with no prompt surface.
	SourceNoPrompter Source = "no_prompter"
	// SourceCloudPolicy identifies an exact kube-context deny or allow_write rule.
	SourceCloudPolicy Source = "cloud_policy"
	// SourceInherited identifies a child policy intersection.
	SourceInherited Source = "inherited"
)

// Outcome is the decision and, for interactive decisions, its wait time.
type Outcome struct {
	Allow    bool
	Reason   string
	Source   Source
	Prompted bool
	Wait     time.Duration
}

// ClassifyTool returns the approval tier for toolName.
func ClassifyTool(toolName string) Tier {
	switch toolName {
	case "read", "grep", "find", "ls", "web_search", "web_fetch", "attachment", "skill", "wait", "logs",
		"lsp_definition", "lsp_references", "lsp_rename", "lsp_diagnostics", "manifest_diff", "view_image":
		return TierRead
	case "write", "edit", "hashline", "integrate", "todo_write":
		return TierWrite
	default:
		// Includes message: it directs other agents' work, so plan mode
		// denies it like task. Includes write_stdin: input to a running job
		// is code its program runs, which the guard and cloud analysis
		// cannot inspect. It needs no per-call ask because the job's command
		// already passed this gate, and a shell reading commands from stdin
		// is opaque, so starting one needed per-call approval. Plan mode
		// denies it, and a write_stdin override (prompt, deny) applies.
		return TierExec
	}
}

// ParseDecision converts an approval settings value into a [Decision].
func ParseDecision(value string) (Decision, error) {
	switch strings.TrimSpace(value) {
	case "allow":
		return DecideAllow, nil
	case "deny":
		return DecideDeny, nil
	case "prompt":
		return DecidePrompt, nil
	default:
		return DecideDeny, fmt.Errorf("unknown permission decision %q", value)
	}
}
