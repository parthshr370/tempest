package tempest

import (
	"context"
	"encoding/json"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
)

func appendEval(f *faux.Faux, code string, reset bool) {
	arguments, err := json.Marshal(struct {
		Language string `json:"language"`
		Code     string `json:"code"`
		Reset    bool   `json:"reset"`
	}{"py", code, reset})
	if err != nil {
		panic(err)
	}
	f.AppendResponses(faux.Respond(faux.ToolCall("eval", arguments, "cell")), faux.Respond(faux.Text("done")))
}

func lastEvalResult(t *testing.T, session *Session) types.ToolResultMessage {
	t.Helper()
	messages := session.Messages()
	for index := len(messages) - 1; index >= 0; index-- {
		if result, ok := messages[index].(types.ToolResultMessage); ok && result.ToolName == "eval" {
			return result
		}
	}
	t.Fatal("eval result missing")
	return types.ToolResultMessage{}
}

func TestEvalSessionOwnsKernelAcrossPromptsAndLosesItOnResume(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	provider := faux.New(faux.Options{})
	sessionDir, cwd := t.TempDir(), t.TempDir()
	options := Options{SessionDir: sessionDir, Cwd: cwd, New: &NewSession{PermissionMode: permission.ModeBypass}, Stack: &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}}
	session, err := Open(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(context.Background())
	appendEval(provider, "saved = 41", false)
	if _, err := session.Prompt(context.Background(), Prompt{Text: "define"}); err != nil {
		t.Fatal(err)
	}
	appendEval(provider, "saved + 1", false)
	if _, err := session.Prompt(context.Background(), Prompt{Text: "read"}); err != nil {
		t.Fatal(err)
	}
	result := lastEvalResult(t, session)
	if result.IsError || strings.TrimSpace(result.Content[0].Text) != "42" {
		t.Fatalf("persistent state: %+v", result)
	}
	appendEval(provider, "import os\nos.getpid()", false)
	if _, err := session.Prompt(context.Background(), Prompt{Text: "pid"}); err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(lastEvalResult(t, session).Content[0].Text))
	if err != nil {
		t.Fatal(err)
	}
	path := session.core.store.Path()
	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("session leaked kernel %d: %v", pid, err)
	}
	options.New = nil
	options.Resume = &ResumeSession{Path: path, PermissionMode: permission.ModeBypass}
	resumed, err := Open(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close(context.Background())
	appendEval(provider, "saved", false)
	if _, err := resumed.Prompt(context.Background(), Prompt{Text: "resume"}); err != nil {
		t.Fatal(err)
	}
	result = lastEvalResult(t, resumed)
	if !result.IsError || !strings.Contains(result.Content[0].Text, "state was lost") {
		t.Fatalf("resume pretended to retain state: %+v", result)
	}
	appendEval(provider, "saved = 7\nsaved", true)
	if _, err := resumed.Prompt(context.Background(), Prompt{Text: "explicit setup"}); err != nil {
		t.Fatal(err)
	}
	result = lastEvalResult(t, resumed)
	if result.IsError || strings.TrimSpace(result.Content[0].Text) != "7" {
		t.Fatalf("resume setup: %+v", result)
	}
}

func TestEvalChildToolCeilingAndExecuteKind(t *testing.T) {
	if !slices.Contains(childToolNames("general", []string{"eval"}), "eval") {
		t.Fatal("general child lost inherited eval")
	}
	if slices.Contains(childToolNames("general", []string{"read"}), "eval") ||
		slices.Contains(childToolNames("explore", []string{"eval"}), "eval") {
		t.Fatal("eval escaped child tool ceiling")
	}
	title, kind := toolDescription("eval", json.RawMessage(`{"language":"py","code":"secret_code()"}`))
	if kind != "execute" || title != "eval (py)" {
		t.Fatalf("approval description %q %q", title, kind)
	}
}
