package permission

import (
	"encoding/json"
	"testing"

	"go.harness.dev/harness/internal/cloudcmd"
)

func evalArgs(language, code string) json.RawMessage {
	args, err := json.Marshal(struct {
		Language string `json:"language"`
		Code     string `json:"code"`
	}{language, code})
	if err != nil {
		panic(err)
	}
	return args
}

func TestEvalInlineCodeUsesDestructiveGuardPerCall(t *testing.T) {
	for _, mode := range []Mode{ModeDefault, ModeAcceptEdits} {
		for _, input := range []struct{ language, code string }{
			{"py", "import shutil; shutil.rmtree('src')"},
			{"py", "from pathlib import Path; Path('src').write_text('replace')"},
			{"js", `require('fs').rmSync('src', {recursive:true})`},
			{"js", `const fs = require('node:fs'); fs.writeFileSync('src', 'replace')`},
		} {
			t.Run(string(mode)+"/"+input.code, func(t *testing.T) {
				prompter := &recordingPrompter{choice: AllowAlways}
				policy := &Policy{Prompter: prompter, Overrides: map[string]Decision{"eval": DecideAllow}}
				args := evalArgs(input.language, input.code)
				for range 2 {
					out := GateInDirectory(t.Context(), mode, "eval", args, policy, t.TempDir())
					if !out.Allow || !out.Prompted {
						t.Fatalf("guard didn't ask per call: %+v", out)
					}
				}
				if len(prompter.requests) != 2 || prompter.requests[0].Reason == "" || prompter.requests[1].Reason == "" {
					t.Fatalf("guard approval remembered: %+v", prompter.requests)
				}
				out := GateInDirectory(t.Context(), mode, "eval", args, &Policy{}, t.TempDir())
				if out.Allow || out.Source != SourceGuard {
					t.Fatalf("unguarded noninteractive code: %+v", out)
				}
			})
		}
	}
}

func TestEvalCloudDenyWinsIncludingBypass(t *testing.T) {
	for _, mode := range []Mode{ModeDefault, ModeAcceptEdits, ModeBypass} {
		for _, code := range []string{
			`import subprocess; subprocess.run(['kubectl', '--context', 'production', 'delete', 'pod', 'api'])`,
			`require('child_process').execSync("kubectl --context production delete pod api")`,
			`const command = 'kubectl'; require('child_process').execFileSync(command, ['get', 'pods'])`,
		} {
			prompter := &recordingPrompter{choice: AllowAlways}
			policy := &Policy{Prompter: prompter, Overrides: map[string]Decision{"eval": DecideAllow}, CloudRules: []cloudcmd.Rule{{Tool: "kubectl", Context: "production", Action: "deny"}}}
			out := GateInDirectory(t.Context(), mode, "eval", evalArgs("py", code), policy, t.TempDir())
			if out.Allow || out.Prompted || out.Source != SourceCloudPolicy {
				t.Fatalf("%s cloud deny bypassed for %s: %+v", mode, code, out)
			}
		}
	}
}

func TestEvalSafeCellsDoNotRequireGuardApproval(t *testing.T) {
	for _, language := range []string{"py", "js"} {
		out := GateInDirectory(t.Context(), ModeDefault, "eval", evalArgs(language, "1 + 1"), &Policy{}, t.TempDir())
		if !out.Allow {
			t.Fatalf("safe %s cell denied: %+v", language, out)
		}
	}
}
