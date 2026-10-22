package tempest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/stream"
	types "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
)

func TestChildPermissionRoutesThroughRootWithScopedOnceChoices(t *testing.T) {
	root := faux.New(faux.Options{})
	child := faux.New(faux.Options{})
	asks := make(chan Ask, 2)
	dispatch := func(ctx context.Context, model types.Model, input types.Context, options *types.SimpleStreamOptions) *stream.AssistantStream {
		if strings.Contains(input.SystemPrompt, "You are a delegated child.") {
			return child.StreamSimple(ctx, model, input, options)
		}
		return root.StreamSimple(ctx, model, input, options)
	}
	cwd := t.TempDir()
	owner, err := Open(t.Context(), Options{SessionDir: t.TempDir(), Cwd: cwd, New: &NewSession{}, Prompter: promptFunc(func(ctx context.Context, ask Ask) (Choice, error) { asks <- ask; <-ctx.Done(); return "", ctx.Err() }), Stack: &StackConfig{Model: root.Model(), StreamFn: dispatch, EnableTask: true, PermissionPolicy: permission.Policy{Overrides: map[string]permission.Decision{"bash": permission.DecidePrompt}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := owner.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	var firstID string
	for index, choice := range []Choice{permission.AllowOnce, permission.RejectOnce} {
		target := filepath.Join(cwd, []string{"approved", "rejected"}[index])
		arguments, err := json.Marshal(struct {
			Command string `json:"command"`
		}{"printf permitted > " + target})
		if err != nil {
			t.Fatal(err)
		}
		child.SetResponses(faux.Respond(faux.ToolCall("bash", arguments, "call_1")), faux.Respond(faux.Text("child done")))
		root.SetResponses(faux.Respond(faux.ToolCall("task", json.RawMessage(`{"prompt":"work","subagent_type":"general"}`), "root_task")), faux.Respond(faux.Text("waiting")), faux.Respond(faux.Text("root done")))
		run, err := owner.Begin(t.Context(), Prompt{Text: "delegate"})
		if err != nil {
			t.Fatal(err)
		}
		var ask Ask
		select {
		case ask = <-asks:
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		}
		if !strings.HasPrefix(ask.ToolCallID, "child:") || ask.ToolCallID == firstID || ask.JobID == "" || ask.ChildSessionID == "" || ask.ChildToolCallID != "call_1" || ask.Cwd != cwd || len(ask.Options) != 2 {
			t.Fatalf("child ask=%+v", ask)
		}
		for _, option := range ask.Options {
			if option.OptionID != "allow_once" && option.OptionID != "reject_once" {
				t.Fatal("child offered an always choice")
			}
		}
		if err = owner.Answer(ask.ToolCallID, permission.AllowAlways); err == nil {
			t.Fatal("child accepted allow_always")
		}
		if err = owner.Answer(ask.ToolCallID, choice); err != nil {
			t.Fatal(err)
		}
		result, err := run.Wait()
		if err != nil || result.StopReason != agent.StopCompleted {
			t.Fatalf("run=%+v %v", result, err)
		}
		_, statErr := os.Stat(target)
		if (statErr == nil) != (choice == permission.AllowOnce) {
			t.Fatalf("approval side effect=%v choice=%s", statErr, choice)
		}
		firstID = ask.ToolCallID
	}
}
