package config

import (
	"errors"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/agent"
)

func TestLoopDetectionDefaultsAndLayers(t *testing.T) {
	w := newLayerWorkspace(t)
	if got := w.mustResolve().LoopDetection(); got != agent.DefaultLoopThresholds {
		t.Fatalf("default thresholds = %+v, want %+v", got, agent.DefaultLoopThresholds)
	}
	// Objects merge by key: the user's member and -c's member both apply.
	w.write(w.user, `{"loop_detection":{"repeated_call":7,"repeated_text":0}}`)
	w.flags.Overrides = StringsInput{Set: true, Values: []string{"loop_detection.alternating_calls=4"}}
	want := agent.LoopThresholds{RepeatedCall: 7, RepeatedResult: 3, AlternatingCalls: 4, RepeatedText: 0}
	if got := w.mustResolve().LoopDetection(); got != want {
		t.Fatalf("layered thresholds = %+v, want %+v", got, want)
	}
}

func TestLoopDetectionRejectsThresholdOne(t *testing.T) {
	w := newLayerWorkspace(t)
	w.write(w.user, `{"loop_detection":{"repeated_result":1}}`)
	_, err := w.resolve()
	var configErr *Error
	if !errors.As(err, &configErr) || configErr.Field != "loop_detection" || !strings.Contains(err.Error(), "repeated_result") {
		t.Fatalf("resolve = %v, want a loop_detection error naming repeated_result", err)
	}
}

// An untrusted repository may tighten the loop guard, never loosen it: a
// threshold above the operator's, a detector turned off, a null that would
// fall back to the default, or one loosening member among tightening ones
// drops the whole object with the trust warning. Under a detector the
// operator turned off, any threshold tightens. Trust lifts the rule.
func TestProjectLoopDetectionCannotLoosenTheOperators(t *testing.T) {
	const operator = `{"loop_detection":{"repeated_call":4,"repeated_text":0}}`
	operatorThresholds := agent.LoopThresholds{RepeatedCall: 4, RepeatedResult: 3, AlternatingCalls: 3, RepeatedText: 0}
	for _, testCase := range []struct {
		name    string
		project string
		want    agent.LoopThresholds
		dropped bool
	}{
		{"tighten", `{"loop_detection":{"repeated_call":2,"repeated_result":3,"repeated_text":6}}`, agent.LoopThresholds{RepeatedCall: 2, RepeatedResult: 3, AlternatingCalls: 3, RepeatedText: 6}, false},
		{"equal", `{"loop_detection":{"repeated_call":4}}`, operatorThresholds, false},
		{"raise above the operator", `{"loop_detection":{"repeated_call":5}}`, operatorThresholds, true},
		{"raise above the default", `{"loop_detection":{"alternating_calls":4}}`, operatorThresholds, true},
		{"turn off", `{"loop_detection":{"repeated_result":0}}`, operatorThresholds, true},
		{"null member", `{"loop_detection":{"repeated_call":null}}`, operatorThresholds, true},
		{"one loosening member", `{"loop_detection":{"repeated_call":2,"repeated_result":9}}`, operatorThresholds, true},
		{"invalid", `{"loop_detection":{"repeated_call":1}}`, operatorThresholds, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			w := newLayerWorkspace(t)
			w.write(w.user, operator)
			w.write(w.projectPath(), testCase.project)
			resolved := w.mustResolve()
			if got := resolved.LoopDetection(); got != testCase.want {
				t.Fatalf("untrusted thresholds = %+v, want %+v", got, testCase.want)
			}
			warnings := strings.Join(resolved.Warnings(), "\n")
			if dropped := strings.Contains(warnings, ": loop_detection (needs -trust-project-hooks"); dropped != testCase.dropped {
				t.Fatalf("warnings = %q, want loop_detection dropped: %v", warnings, testCase.dropped)
			}
		})
	}

	w := newLayerWorkspace(t)
	w.write(w.user, operator)
	w.write(w.projectPath(), `{"loop_detection":{"repeated_call":9,"repeated_result":0}}`)
	w.flags.TrustProjectHooks = BoolInput{Value: true, Set: true}
	if got, want := w.mustResolve().LoopDetection(), (agent.LoopThresholds{RepeatedCall: 9, RepeatedResult: 0, AlternatingCalls: 3, RepeatedText: 0}); got != want {
		t.Fatalf("trusted thresholds = %+v, want %+v", got, want)
	}
}
