package session

import (
	"os/exec"
	"strings"
	"testing"

	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/evalkernel"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/session/journal"
)

func TestEvalResetOnlyRequiredForPreviouslyUsedResumedLanguage(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	for _, scenario := range []struct {
		name   string
		loaded *journal.Loaded
		seed   []ptypes.Message
		lost   bool
	}{
		{name: "SDK seeded conversation", seed: []ptypes.Message{ptypes.UserMessage{Content: ptypes.StringContent("seeded")}}},
		{name: "resume without eval", loaded: &journal.Loaded{}},
		{name: "resume other language", loaded: &journal.Loaded{EvalLanguages: map[string]bool{"js": true}}},
		{name: "resume used Python", loaded: &journal.Loaded{EvalLanguages: map[string]bool{"py": true}}, lost: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			provider := faux.New(faux.Options{})
			stack, err := BuildAgentStack(StackConfig{Cwd: t.TempDir(), Model: provider.Model(), StreamFn: provider.StreamSimple, Loaded: scenario.loaded, InitialMessages: scenario.seed})
			if err != nil {
				t.Fatal(err)
			}
			defer stack.Close()
			result, err := stack.kernels.Eval(t.Context(), evalkernel.Request{Language: "py", Code: "42"})
			if scenario.lost {
				if err == nil || !strings.Contains(err.Error(), "state was lost") {
					t.Fatalf("used resumed language was silently recreated: %v", err)
				}
			} else if err != nil || result.Output.Content != "42\n" {
				t.Fatalf("unused language unnecessarily required reset: %+v %v", result, err)
			}
		})
	}
}
