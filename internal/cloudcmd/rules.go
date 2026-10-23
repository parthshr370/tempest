package cloudcmd

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxRules bounds an operator cloud policy.
const MaxRules = 256

const maxContextBytes = 1024

// NormalizeRules validates rules and returns a sorted, deduplicated deep copy.
// A nil input stays nil; a non-nil empty input returns a non-nil empty slice.
// Contexts are exact literals: whitespace, control characters and glob
// metacharacters are rejected rather than silently interpreted.
func NormalizeRules(rules []Rule) ([]Rule, error) {
	if rules == nil {
		return nil, nil
	}
	if len(rules) > MaxRules {
		return nil, fmt.Errorf("cloud policy has %d rules; at most %d are allowed", len(rules), MaxRules)
	}
	out := make([]Rule, 0, len(rules))
	seen := make(map[Rule]bool, len(rules))
	for i, rule := range rules {
		if err := validateRule(rule); err != nil {
			return nil, fmt.Errorf("cloud policy rule %d: %w", i, err)
		}
		if !seen[rule] {
			seen[rule] = true
			out = append(out, rule)
		}
	}
	slices.SortFunc(out, func(a, b Rule) int {
		return cmp.Or(cmp.Compare(a.Tool, b.Tool), cmp.Compare(a.Context, b.Context), cmp.Compare(a.Action, b.Action))
	})
	return out, nil
}

func validateRule(rule Rule) error {
	switch rule.Tool {
	case ToolKubectl, ToolHelm:
	case "":
		return errors.New("tool is required (kubectl or helm)")
	default:
		return fmt.Errorf("tool %q is not supported; context rules apply only to kubectl and helm", bounded(rule.Tool))
	}
	switch rule.Action {
	case ActionAllowWrite, ActionDeny:
	case "":
		return errors.New("action is required (allow_write or deny)")
	default:
		return fmt.Errorf("action %q is not supported; use allow_write or deny", bounded(rule.Action))
	}
	context := rule.Context
	switch {
	case context == "":
		return errors.New("context is required")
	case len(context) > maxContextBytes:
		return fmt.Errorf("context exceeds %d bytes", maxContextBytes)
	case !utf8.ValidString(context):
		return errors.New("context is not valid UTF-8")
	case strings.ContainsAny(context, "*?[]"):
		return fmt.Errorf("context %q contains a pattern character; rules match exact context names only", bounded(context))
	}
	for _, r := range context {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return fmt.Errorf("context %q contains whitespace or a control character", bounded(context))
		}
	}
	return nil
}

func bounded(s string) string {
	if len(s) <= 64 {
		return s
	}
	cut := 64
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

// Verdict is the result of checking an [Analysis] against context rules.
type Verdict struct {
	// Denied means a deny rule matched an invocation's literal context, or a
	// deny rule exists for a cluster-targeting invocation whose target could
	// not be established (target_unknown).
	Denied bool
	// Granted means the analysis is a write and every write invocation is
	// covered by an exact allow_write rule for its established context.
	Granted bool
	// Reason explains a denial or grant; empty otherwise.
	Reason string
}

// Evaluate applies normalized rules to an analysis. Deny wins regardless of
// rule order and matches every effect, including reads. Grants cover only
// grantable write invocations with an established explicit context; they
// never cover destructive or opaque analyses, Terraform, kustomize or
// configuration mutations.
func Evaluate(analysis Analysis, rules []Rule) Verdict {
	if len(rules) == 0 {
		return Verdict{}
	}
	deny := make(map[[2]string]bool)
	allow := make(map[[2]string]bool)
	denyTool := make(map[string]bool)
	for _, rule := range rules {
		key := [2]string{rule.Tool, rule.Context}
		switch rule.Action {
		case ActionDeny:
			deny[key] = true
			denyTool[rule.Tool] = true
		case ActionAllowWrite:
			allow[key] = true
		}
	}
	for _, inv := range analysis.Invocations {
		if inv.Tool != ToolKubectl && inv.Tool != ToolHelm {
			continue
		}
		if inv.Context != "" && deny[[2]string{inv.Tool, inv.Context}] {
			return Verdict{Denied: true, Reason: fmt.Sprintf("Cloud policy denies %s context %q", inv.Tool, bounded(inv.Context))}
		}
	}
	for _, inv := range analysis.Invocations {
		if (inv.Tool == ToolKubectl || inv.Tool == ToolHelm) && inv.cluster && !inv.ExplicitContext && denyTool[inv.Tool] {
			return Verdict{Denied: true, Reason: fmt.Sprintf("target_unknown: cloud policy has %s deny rules and %s does not name a single literal --%s without endpoint overrides", inv.Tool, describe(inv), contextFlag(inv.Tool))}
		}
	}
	if analysis.Effect != EffectWrite {
		return Verdict{}
	}
	var granted []string
	for _, inv := range analysis.Invocations {
		if inv.Effect != EffectWrite {
			continue
		}
		if !inv.grantable || !inv.ExplicitContext || !allow[[2]string{inv.Tool, inv.Context}] {
			return Verdict{}
		}
		granted = append(granted, fmt.Sprintf("%s context %q", inv.Tool, bounded(inv.Context)))
	}
	if len(granted) == 0 {
		return Verdict{}
	}
	return Verdict{Granted: true, Reason: clip("Cloud write granted by policy for " + strings.Join(granted, ", "))}
}

func contextFlag(tool string) string {
	if tool == ToolHelm {
		return "kube-context"
	}
	return "context"
}
