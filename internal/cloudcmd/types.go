// Package cloudcmd classifies the cloud intent of a shell command without
// executing it. It recognizes kubectl, helm, terraform and kustomize
// invocations inside conservatively parsed shell syntax, including through
// resolvable wrappers and inline shell code, and reports their
// read/write/destructive/opaque effect and literal kube-context target.
//
// The analysis never runs a shell, reads kubeconfig or the environment,
// resolves PATH, performs network I/O or grants permission. Shell syntax it
// cannot resolve is opaque when the command names a cloud tool; a dynamic
// command word or unseen shell code is always opaque. PATH, aliases inside
// invoked scripts and ambient credential helpers remain trusted host context:
// this is not hostile-code containment.
package cloudcmd

// Effect is the semantic effect of a command, ordered by risk.
type Effect string

const (
	// EffectNone means no recognized cloud invocation and nothing that could
	// hide one; the caller's ordinary non-cloud classification still applies.
	EffectNone Effect = "none"
	// EffectRead is a proven read-only cloud form.
	EffectRead Effect = "read"
	// EffectWrite changes cluster, release, state or local tool configuration.
	EffectWrite Effect = "write"
	// EffectDestructive deletes, replaces, rolls back or disrupts resources.
	EffectDestructive Effect = "destructive"
	// EffectOpaque could not be attributed: unknown cloud verbs or flags,
	// dynamic cloud arguments, a dynamic command word or unseen shell code,
	// or unresolved shell syntax in a command that names a cloud tool.
	EffectOpaque Effect = "opaque"
)

func (e Effect) rank() int {
	switch e {
	case EffectNone:
		return 0
	case EffectRead:
		return 1
	case EffectWrite:
		return 2
	case EffectDestructive:
		return 3
	default:
		return 4
	}
}

// Join returns the riskier of two effects; unknown values count as opaque.
func Join(a, b Effect) Effect {
	if b.rank() > a.rank() {
		return normalizeEffect(b)
	}
	return normalizeEffect(a)
}

func normalizeEffect(e Effect) Effect {
	if e.rank() == 4 {
		return EffectOpaque
	}
	return e
}

// Rule actions and tools accepted by [NormalizeRules].
const (
	ActionAllowWrite = "allow_write"
	ActionDeny       = "deny"
	ToolKubectl      = "kubectl"
	ToolHelm         = "helm"
	ToolTerraform    = "terraform"
	ToolKustomize    = "kustomize"
)

// Rule is one operator-configured exact kube-context policy entry.
type Rule struct {
	Tool    string `json:"tool"`    // kubectl | helm
	Context string `json:"context"` // exact nonempty literal, no patterns
	Action  string `json:"action"`  // allow_write | deny
}

// Analysis is the classification of one complete shell command.
type Analysis struct {
	// Effect is the maximum-risk join over every invocation. Unresolved shell
	// syntax joins opaque only when the command names a cloud tool.
	Effect Effect
	// Cloud reports whether any kubectl/helm/terraform/kustomize invocation was found.
	Cloud bool
	// Proven reports plain syntax: no substitution, here-document, compound,
	// wrapper, inline shell code, dynamic command word or invalid input, even
	// one the analysis resolved. Plan mode requires it, so narrowing Effect
	// for non-cloud commands never widens what plan runs.
	Proven bool
	// Invocations lists cloud invocations and dynamic (opaque) command words
	// in command order, substitutions before the command that uses them,
	// bounded to MaxInvocations. Ordinary non-cloud programs are not listed.
	Invocations []Invocation
	// Reason is a bounded human summary, including unresolved syntax that did
	// not make Effect opaque; it never contains flag values other than
	// literal context names.
	Reason string
}

// Invocation is one simple command inside the analyzed shell text.
type Invocation struct {
	// Tool is the executable basename (kubectl, helm, terraform, kustomize),
	// the shell or wrapper whose code is opaque (bash, eval, sudo), or empty
	// for a dynamic command word.
	Tool string
	// Verb is the recognized subcommand path, e.g. "get", "rollout undo",
	// "state rm"; empty when it could not be established.
	Verb string
	// Context is the literal kube context named by exactly one --context
	// (kubectl) or --kube-context (helm) flag, even when other flags override
	// the endpoint. Empty when absent, repeated or dynamic.
	Context string
	// Effect is this invocation's own classification.
	Effect Effect
	// ExplicitContext reports that Context is the established target: a single
	// literal context flag, no kubeconfig/endpoint/credential override, no
	// dynamic text and no option whose arity is unknown. Only such
	// invocations are eligible for allow_write grants.
	ExplicitContext bool

	// cluster is false for forms that never contact a cluster (help, version
	// --client, kubeconfig edits, local helm chart operations). Such forms have
	// no target, so deny rules cannot match them by target_unknown.
	cluster bool
	// grantable is false for forms that rules never grant (config mutation)
	// and for any invocation reached through a wrapper, substitution or
	// inline shell code.
	grantable bool
}

// Parser bounds. Larger input is opaque.
const (
	MaxCommandBytes = 64 << 10
	MaxTokens       = 4096
	MaxInvocations  = 64
	maxReasonBytes  = 512
)
