package tools

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/evalkernel"
	"go.harness.dev/harness/internal/permission"
)

func TestEvalToolPersistsAndPreservesErrors(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	kernels := evalkernel.New(t.TempDir(), t.TempDir(), nil)
	defer kernels.Close()
	tool, err := NewTool(EvalTool, t.TempDir(), ToolsOptions{Kernels: kernels})
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []struct {
		code, want string
		failed     bool
	}{
		{"answer = 41", "(no output)", false},
		{"answer + 1", "42", false},
		{"print(' ')", " \n", false},
		{"raise ValueError('tool failure')", "ValueError: tool failure", true},
	} {
		params, err := json.Marshal(struct {
			Language string `json:"language"`
			Code     string `json:"code"`
		}{"py", input.code})
		if err != nil {
			t.Fatal(err)
		}
		result, err := tool.Execute(context.Background(), "call", params, nil)
		if err != nil || result.IsError != input.failed || !strings.Contains(resultText(result), input.want) {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	}
	if permission.ClassifyTool("eval") != permission.TierExec {
		t.Fatal("eval is not exec tier")
	}
	outcome := permission.Gate(context.Background(), permission.ModePlan, "eval", json.RawMessage(`{"language":"py","code":"1"}`), &permission.Policy{Overrides: map[string]permission.Decision{"eval": permission.DecideAllow}})
	if outcome.Allow {
		t.Fatal("plan mode allowed eval despite exec tier")
	}
}

func TestEvalToolRejectsInvalidTimeoutAndUnownedKernel(t *testing.T) {
	if _, err := NewTool(EvalTool, t.TempDir(), ToolsOptions{}); err == nil {
		t.Fatal("eval registered without a lifecycle owner")
	}
	tool := newEvalTool(nil)
	for _, params := range []string{`{"language":"py","code":"1","timeout":-1}`, `{"language":"py","code":"1","timeout":2147484}`} {
		if _, err := tool.Execute(context.Background(), "call", json.RawMessage(params), nil); err == nil {
			t.Fatal("invalid timeout accepted")
		}
	}
}
