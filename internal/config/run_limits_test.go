package config

import (
	"strings"
	"testing"

	"go.harness.dev/harness/internal/agent"
)

var runLimitKeys = []string{"max_input_tokens", "max_output_tokens", "max_total_tokens", "max_tool_calls"}

// An untrusted repository may set or lower the per-run token and tool-call
// caps, but never raise one above the operator's value or lift it with 0
// (unlimited) or null. Under an operator without a cap any positive cap
// tightens it. Trust lifts the ceiling.
func TestProjectRunLimitsCanOnlyLowerTheOperatorsCaps(t *testing.T) {
	const operator = `{"max_input_tokens":1000,"max_output_tokens":100,"max_total_tokens":1100,"max_tool_calls":10}`
	operatorLimits := agent.Budget{MaxInputTokens: 1000, MaxOutputTokens: 100, MaxTotalTokens: 1100, MaxToolCalls: 10}
	const raise = `{"max_input_tokens":1001,"max_output_tokens":101,"max_total_tokens":1101,"max_tool_calls":11}`
	for _, testCase := range []struct {
		name     string
		operator string
		project  string
		want     agent.Budget
		dropped  []string
	}{
		{"raise", operator, raise, operatorLimits, runLimitKeys},
		{"lower", operator, `{"max_input_tokens":999,"max_output_tokens":99,"max_total_tokens":1099,"max_tool_calls":9}`, agent.Budget{MaxInputTokens: 999, MaxOutputTokens: 99, MaxTotalTokens: 1099, MaxToolCalls: 9}, nil},
		{"equal", operator, operator, operatorLimits, nil},
		{"zero lifts the cap", operator, `{"max_input_tokens":0,"max_output_tokens":0,"max_total_tokens":0,"max_tool_calls":0}`, operatorLimits, runLimitKeys},
		{"null lifts the cap", operator, `{"max_input_tokens":null,"max_output_tokens":null,"max_total_tokens":null,"max_tool_calls":null}`, operatorLimits, runLimitKeys},
		{"invalid", operator, `{"max_input_tokens":-1,"max_output_tokens":"many","max_total_tokens":1.5,"max_tool_calls":true}`, operatorLimits, runLimitKeys},
		{"operator unlimited", `{}`, `{"max_input_tokens":5000000,"max_tool_calls":1}`, agent.Budget{MaxInputTokens: 5000000, MaxToolCalls: 1}, nil},
		{"operator's zero is unlimited", `{"max_tool_calls":0}`, `{"max_tool_calls":40}`, agent.Budget{MaxToolCalls: 40}, nil},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			w := newLayerWorkspace(t)
			w.write(w.user, testCase.operator)
			w.write(w.projectPath(), testCase.project)
			resolved := w.mustResolve()
			if got := resolved.RunLimits(); got != testCase.want {
				t.Fatalf("untrusted limits = %+v, want %+v", got, testCase.want)
			}
			warnings := strings.Join(resolved.Warnings(), "\n")
			for _, key := range testCase.dropped {
				if !strings.Contains(warnings, ": "+key+" (needs -trust-project-hooks") {
					t.Fatalf("warnings %q do not name the dropped %s", warnings, key)
				}
			}
			if testCase.dropped == nil && warnings != "" {
				t.Fatalf("warnings = %q", warnings)
			}
		})
	}

	w := newLayerWorkspace(t)
	w.write(w.user, operator)
	w.write(w.projectPath(), raise)
	w.flags.TrustProjectHooks = BoolInput{Value: true, Set: true}
	if got, want := w.mustResolve().RunLimits(), (agent.Budget{MaxInputTokens: 1001, MaxOutputTokens: 101, MaxTotalTokens: 1101, MaxToolCalls: 11}); got != want {
		t.Fatalf("trusted limits = %+v, want %+v", got, want)
	}
}

// A flag beats HARNESS_MAX_*, which beats settings; each key resolves on its
// own, and the operator may set a negative value nowhere.
func TestRunLimitsPrecedence(t *testing.T) {
	w := newLayerWorkspace(t)
	w.write(w.user, `{"max_input_tokens":100,"max_output_tokens":200,"max_total_tokens":300,"max_tool_calls":4}`)
	w.env["HARNESS_MAX_OUTPUT_TOKENS"] = "20"
	w.env["HARNESS_MAX_TOOL_CALLS"] = "3"
	w.flags.MaxToolCalls = IntInput{Value: 2, Set: true}
	w.flags.MaxTotalTokens = IntInput{Value: 0, Set: true}
	if got, want := w.mustResolve().RunLimits(), (agent.Budget{MaxInputTokens: 100, MaxOutputTokens: 20, MaxTotalTokens: 0, MaxToolCalls: 2}); got != want {
		t.Fatalf("limits = %+v, want %+v", got, want)
	}

	w = newLayerWorkspace(t)
	w.write(w.user, `{"max_tool_calls":-1}`)
	if _, err := w.resolve(); err == nil || !strings.Contains(err.Error(), "max_tool_calls") {
		t.Fatalf("negative max_tool_calls: err = %v", err)
	}
}
