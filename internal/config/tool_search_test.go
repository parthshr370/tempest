package config

import (
	"errors"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/agent"
)

func TestToolSearchDefaultsAndLayers(t *testing.T) {
	w := newLayerWorkspace(t)
	if got, want := w.mustResolve().ToolSearch(), (agent.ToolSearch{Mode: agent.ToolSearchAuto, ThresholdTokens: agent.DefaultToolSearchThreshold}); got != want {
		t.Fatalf("default = %+v, want %+v", got, want)
	}
	// Objects merge by key: the user's member and -c's member both apply.
	w.write(w.user, `{"tool_search":{"threshold_tokens":2000}}`)
	w.flags.Overrides = StringsInput{Set: true, Values: []string{"tool_search.mode=always"}}
	if got, want := w.mustResolve().ToolSearch(), (agent.ToolSearch{Mode: agent.ToolSearchAlways, ThresholdTokens: 2000}); got != want {
		t.Fatalf("layered = %+v, want %+v", got, want)
	}
}

func TestToolSearchRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{`{"mode":"lazy"}`, `{"threshold_tokens":0}`, `{"threshold_tokens":-5}`} {
		w := newLayerWorkspace(t)
		w.write(w.user, `{"tool_search":`+value+`}`)
		_, err := w.resolve()
		var configErr *Error
		if !errors.As(err, &configErr) || configErr.Field != "tool_search" {
			t.Fatalf("%s: resolve = %v, want a tool_search error", value, err)
		}
	}
}

// An untrusted repository may defer tool schemas more than the operator
// does, never less: never, a higher threshold, null or an invalid value
// drops the whole object with the trust warning. Trust lifts the rule.
func TestProjectToolSearchCannotDeferLessThanTheOperator(t *testing.T) {
	const operator = `{"tool_search":{"threshold_tokens":5000}}`
	operatorSearch := agent.ToolSearch{Mode: agent.ToolSearchAuto, ThresholdTokens: 5000}
	for _, testCase := range []struct {
		name     string
		operator string
		project  string
		want     agent.ToolSearch
		dropped  bool
	}{
		{"always", operator, `{"tool_search":{"mode":"always"}}`, agent.ToolSearch{Mode: agent.ToolSearchAlways, ThresholdTokens: 5000}, false},
		{"lower threshold", operator, `{"tool_search":{"threshold_tokens":100}}`, agent.ToolSearch{Mode: agent.ToolSearchAuto, ThresholdTokens: 100}, false},
		{"equal", operator, `{"tool_search":{"mode":"auto","threshold_tokens":5000}}`, operatorSearch, false},
		{"higher threshold", operator, `{"tool_search":{"threshold_tokens":5001}}`, operatorSearch, true},
		{"never", operator, `{"tool_search":{"mode":"never"}}`, operatorSearch, true},
		{"null member", operator, `{"tool_search":{"mode":null}}`, operatorSearch, true},
		{"invalid", operator, `{"tool_search":{"threshold_tokens":0}}`, operatorSearch, true},
		{"unknown mode", operator, `{"tool_search":{"mode":"lazy"}}`, operatorSearch, true},
		{"auto under always", `{"tool_search":{"mode":"always"}}`, `{"tool_search":{"mode":"auto","threshold_tokens":1}}`, agent.ToolSearch{Mode: agent.ToolSearchAlways, ThresholdTokens: agent.DefaultToolSearchThreshold}, true},
		{"auto under never", `{"tool_search":{"mode":"never"}}`, `{"tool_search":{"mode":"auto","threshold_tokens":90000}}`, agent.ToolSearch{Mode: agent.ToolSearchAuto, ThresholdTokens: 90000}, false},
		{"default operator", `{}`, `{"tool_search":{"threshold_tokens":20000}}`, agent.ToolSearch{Mode: agent.ToolSearchAuto, ThresholdTokens: agent.DefaultToolSearchThreshold}, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			w := newLayerWorkspace(t)
			w.write(w.user, testCase.operator)
			w.write(w.projectPath(), testCase.project)
			resolved := w.mustResolve()
			if got := resolved.ToolSearch(); got != testCase.want {
				t.Fatalf("untrusted tool_search = %+v, want %+v", got, testCase.want)
			}
			warnings := strings.Join(resolved.Warnings(), "\n")
			if dropped := strings.Contains(warnings, ": tool_search (needs -trust-project-hooks"); dropped != testCase.dropped {
				t.Fatalf("warnings = %q, want tool_search dropped: %v", warnings, testCase.dropped)
			}
		})
	}

	w := newLayerWorkspace(t)
	w.write(w.user, operator)
	w.write(w.projectPath(), `{"tool_search":{"mode":"never"}}`)
	w.flags.TrustProjectHooks = BoolInput{Value: true, Set: true}
	if got, want := w.mustResolve().ToolSearch(), (agent.ToolSearch{Mode: agent.ToolSearchNever, ThresholdTokens: 5000}); got != want {
		t.Fatalf("trusted tool_search = %+v, want %+v", got, want)
	}
}
