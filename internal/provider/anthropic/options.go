package anthropic

import (
	"go.harness.dev/harness/internal/engine/estimate"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/base"
)

// Effort is the Anthropic thinking-effort level for adaptive-thinking models.
type Effort string

const (
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
	EffortXHigh  Effort = "xhigh"
	EffortMax    Effort = "max"
)

// ThinkingDisplay controls whether reasoning blocks are summarized or omitted
// in the stream (Anthropic thinking_display param).
type ThinkingDisplay string

const (
	ThinkingSummarized ThinkingDisplay = "summarized"
	ThinkingOmitted    ThinkingDisplay = "omitted"
)

// ToolChoice is the Anthropic tool_choice body (auto/any/tool references a
// specific tool by name).
type ToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// Options is the full set of Anthropic-specific stream parameters. It embeds
// [types.StreamOptions] for the common provider fields and adds thinking /
// cache / tool-choice knobs.
type Options struct {
	types.StreamOptions
	BlobReader           types.BlobReader
	ThinkingEnabled      *bool
	ThinkingBudgetTokens int
	Effort               Effort
	ThinkingDisplay      ThinkingDisplay
	InterleavedThinking  *bool
	ToolChoice           *ToolChoice
}

const (
	// contextSafetyTokens provides headroom for the model's response so
	// max_tokens + prompt tokens do not bump against the context window ceiling.
	contextSafetyTokens = 4096
	minMaxTokens        = 1
)

// streamTimeouts bounds stream silence for the process lifetime.
var streamTimeouts = base.StreamTimeoutsFromEnv()

// clampMaxTokensToContext caps maxTokens to what the model's context window
// can accommodate after subtracting estimated prompt tokens and a safety margin.
func clampMaxTokensToContext(model types.Model, c types.Context, maxTokens int) int {
	if model.ContextWindow <= 0 {
		return maxInt(minMaxTokens, maxTokens)
	}
	available := model.ContextWindow - estimate.EstimateContextTokens(c).Tokens - contextSafetyTokens
	return minInt(maxTokens, maxInt(minMaxTokens, available))
}

// buildBaseOptions constructs a [types.StreamOptions] from the simple options,
// clamping max tokens to the context window. It is the first step before
// thinking-config layering in [StreamSimple].
func buildBaseOptions(model types.Model, c types.Context, opts *types.SimpleStreamOptions, apiKey string) types.StreamOptions {
	var base types.StreamOptions
	if opts != nil {
		base = opts.StreamOptions
	}
	requestedMaxTokens := model.MaxTokens
	if opts != nil && opts.MaxTokens != 0 {
		requestedMaxTokens = opts.MaxTokens
	}
	base.MaxTokens = clampMaxTokensToContext(model, c, requestedMaxTokens)
	if apiKey != "" {
		base.APIKey = apiKey
	}
	return base
}

// clampReasoning caps the reasoning level to "high" — Anthropic rejects
// "xhigh" in the fixed-thinking-budget path, so it maps to "high".
func clampReasoning(level string) string {
	if level == "xhigh" {
		return "high"
	}
	return level
}

// adjustMaxTokensForThinking computes the final max_tokens and thinking_budget
// for the fixed-thinking path. It adds the level's thinking budget to the
// base max tokens, clamps to the model maximum, and ensures at least
// [minOutputTokens] remain for the visible response.
func adjustMaxTokensForThinking(baseMaxTokens int, modelMaxTokens int, reasoningLevel string, customBudgets map[string]int) (maxTokens int, thinkingBudget int) {
	budgets := map[string]int{
		"minimal": 1024,
		"low":     2048,
		"medium":  8192,
		"high":    16384,
	}
	for key, value := range customBudgets {
		budgets[key] = value
	}

	level := clampReasoning(reasoningLevel)
	thinkingBudget = budgets[level]
	maxTokens = minInt(baseMaxTokens+thinkingBudget, modelMaxTokens)
	if baseMaxTokens == 0 {
		maxTokens = modelMaxTokens
	}

	const minOutputTokens = 1024
	if maxTokens <= thinkingBudget {
		thinkingBudget = maxInt(0, maxTokens-minOutputTokens)
	}
	return maxTokens, thinkingBudget
}

// mapThinkingLevelToEffort maps a reasoning-level string to an [Effort] for
// adaptive-thinking models. It consults the model's ThinkingLevelMap first,
// then falls back to the default level→effort mapping.
func mapThinkingLevelToEffort(model types.Model, level string) Effort {
	if level != "" && model.ThinkingLevelMap != nil {
		if mapped, ok := model.ThinkingLevelMap[level]; ok && mapped != nil {
			return Effort(*mapped)
		}
	}
	switch level {
	case "minimal", "low":
		return EffortLow
	case "medium":
		return EffortMedium
	case "high":
		return EffortHigh
	default:
		return EffortHigh
	}
}

// boolPtr returns a pointer to v. Used to set optional bool fields on Options.
func boolPtr(v bool) *bool { return &v }

// maxInt returns the larger of a and b.
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// minInt returns the smaller of a and b.
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
