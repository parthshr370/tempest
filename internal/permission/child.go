package permission

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"go.harness.dev/harness/internal/work"
)

// CaptureCeiling copies the policy and selected tools without retaining mutable maps.
func CaptureCeiling(mode Mode, tools []string, policy *Policy) work.Ceiling {
	c := work.Ceiling{Mode: string(mode), Tools: append([]string(nil), tools...), Overrides: map[string]string{}, Interactive: map[string]string{}}
	if policy == nil {
		return c
	}
	policyMu.Lock()
	defer policyMu.Unlock()
	for k, v := range policy.Overrides {
		c.Overrides[k] = decisionName(v)
	}
	for k, v := range policy.promptOverrides {
		c.Interactive[k] = decisionName(v)
	}
	return c
}
func decisionName(d Decision) string {
	switch d {
	case DecideAllow:
		return "allow"
	case DecidePrompt:
		return "prompt"
	default:
		return "deny"
	}
}
func ceilingPolicy(c work.Ceiling) *Policy {
	p := &Policy{Overrides: map[string]Decision{}, promptOverrides: map[string]Decision{}}
	for k, v := range c.Overrides {
		d, err := ParseDecision(v)
		if err != nil {
			d = DecideDeny
		}
		p.Overrides[k] = d
	}
	for k, v := range c.Interactive {
		d, err := ParseDecision(v)
		if err != nil {
			d = DecideDeny
		}
		p.promptOverrides[k] = d
	}
	return p
}

type decisionProbe struct{ request *PromptRequest }

func (p *decisionProbe) Approve(_ context.Context, r PromptRequest) (Choice, error) {
	p.request = &r
	return AllowOnce, nil
}
func evaluate(ctx context.Context, mode Mode, tool string, args json.RawMessage, policy *Policy, cwd string) (Outcome, *PromptRequest) {
	saved := CaptureCeiling(mode, nil, policy)
	copy := ceilingPolicy(saved)
	probe := &decisionProbe{}
	copy.Prompter = probe
	out := GateInDirectory(ctx, mode, tool, args, copy, cwd)
	return out, probe.request
}

// ChildScope identifies the root-owned approval target without changing raw child ids.
type ChildScope struct{ JobID, SessionID, RunID, CallID, ToolCallID, Cwd string }

// GateChild intersects launch and live policy, asking the root at most once.
func GateChild(ctx context.Context, ceiling work.Ceiling, liveMode Mode, liveTools []string, policy *Policy, tool string, args json.RawMessage, scope ChildScope) Outcome {
	denied := Outcome{Reason: "Tool " + tool + " denied by inherited policy", Source: SourceInherited}
	if !slices.Contains(ceiling.Tools, tool) || !slices.Contains(liveTools, tool) {
		return denied
	}
	if (liveMode == ModePlan || Mode(ceiling.Mode) == ModePlan) && ClassifyTool(tool) != TierRead && !(tool == "bash" && IsSafeCommand(commandFromArgs(args))) {
		return denied
	}
	launch, askLaunch := evaluate(ctx, Mode(ceiling.Mode), tool, args, ceilingPolicy(ceiling), scope.Cwd)
	live, askLive := evaluate(ctx, liveMode, tool, args, policy, scope.Cwd)
	if !launch.Allow || !live.Allow {
		return denied
	}
	req := askLive
	if req == nil {
		req = askLaunch
	}
	if req == nil {
		return Outcome{Allow: true, Source: SourceInherited}
	}
	req.ToolCallID = fmt.Sprintf("child:%s:%s:%s", scope.SessionID, scope.RunID, scope.CallID)
	req.JobID, req.ChildSessionID, req.ChildToolCallID, req.Cwd = scope.JobID, scope.SessionID, scope.ToolCallID, scope.Cwd
	var prompter Prompter
	if policy != nil {
		prompter = policy.Prompter
	}
	return resolvePrompt(ctx, &Policy{Prompter: oncePrompter{prompter}}, *req)
}

type oncePrompter struct{ root Prompter }

func (p oncePrompter) Approve(ctx context.Context, r PromptRequest) (Choice, error) {
	if p.root == nil {
		return RejectOnce, nil
	}
	c, err := p.root.Approve(ctx, r)
	if c != AllowOnce && c != RejectOnce {
		c = RejectOnce
	}
	return c, err
}
