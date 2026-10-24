package permission

import (
	"encoding/json"
	"testing"

	"go.harness.dev/harness/internal/cloudcmd"
)

func TestDebugEvaluateUsesEvalGuards(t *testing.T) {
	for _, mode := range []Mode{ModeDefault, ModeAcceptEdits, ModeBypass, ModePlan} {
		for _, expression := range []string{
			"1 + 1",
			"__import__('shutil').rmtree('src')",
			"open('.harness/settings.json', 'w').write('{}')",
			`__import__('subprocess').run(['kubectl','--context','production','delete','pod','api'])`,
		} {
			policy := &Policy{Overrides: map[string]Decision{"debug": DecideAllow, "eval": DecideAllow}, CloudRules: []cloudcmd.Rule{{Tool: "kubectl", Context: "production", Action: "deny"}}}
			args, err := json.Marshal(struct{ Action, Expression string }{"evaluate", expression})
			if err != nil {
				t.Fatal(err)
			}
			cwd := t.TempDir()
			got := GateInDirectory(t.Context(), mode, "debug", args, policy, cwd)
			want := GateInDirectory(t.Context(), mode, "eval", evalArgs("py", expression), policy, cwd)
			if got.Allow != want.Allow || got.Source != want.Source {
				t.Fatalf("%s %q: debug=%+v eval=%+v", mode, expression, got, want)
			}
		}
	}
}

func TestDebugApprovalsNeverRemembered(t *testing.T) {
	for _, mode := range []Mode{ModeDefault, ModeAcceptEdits, ModeBypass} {
		for _, action := range []string{"launch", "attach"} {
			for _, choice := range []Choice{AllowAlways, RejectAlways} {
				prompter := &recordingPrompter{choice: choice}
				policy := &Policy{Overrides: map[string]Decision{"debug": DecideAllow}, Prompter: prompter}
				args := json.RawMessage(`{"action":"` + action + `","arguments":{"python":["/bin/sh","-c","kubectl --context production delete pod api"]}}`)
				for range 2 {
					out := GateInDirectory(t.Context(), mode, "debug", args, policy, t.TempDir())
					if out.Allow != (choice == AllowAlways) || !out.Prompted {
						t.Fatalf("%s %s: %+v", mode, action, out)
					}
				}
				if len(prompter.requests) != 2 || prompter.requests[0].Reason == "" {
					t.Fatalf("remembered debug approval: %+v", prompter.requests)
				}
				out := GateInDirectory(t.Context(), mode, "debug", args, &Policy{Overrides: map[string]Decision{"debug": DecideAllow}}, t.TempDir())
				if out.Allow || out.Source != SourceGuard {
					t.Fatalf("headless launch allowed: %+v", out)
				}
			}
		}
	}
	for _, action := range []string{"evaluate", "launch", "attach"} {
		prompter := &recordingPrompter{choice: AllowAlways}
		policy := &Policy{Prompter: prompter}
		for range 2 {
			out := GateInDirectory(t.Context(), ModeDefault, "debug", json.RawMessage(`{"action":"`+action+`","expression":"__import__('shutil').rmtree('src')"}`), policy, t.TempDir())
			if !out.Allow || !out.Prompted {
				t.Fatalf("%s: %+v", action, out)
			}
		}
		if len(prompter.requests) != 2 {
			t.Fatalf("remembered %s approval", action)
		}
	}
}
