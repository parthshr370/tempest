package tempest

import (
	"context"
	"sync"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/provider/faux"
)

func TestBusyResetCannotConsumeJustAppendedPrompt(t *testing.T) {
	provider := faux.New(faux.Options{})
	provider.SetResponses(faux.Respond(faux.Text("answer")))
	owner, err := Open(t.Context(), Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &NewSession{}, Stack: &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(context.Background())
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	owner.core.agent.Subscribe(func(_ context.Context, event agent.AgentEvent) error {
		if event.Type == agent.EventMessageEnd && event.Message.Role() == "user" {
			close(entered)
			<-release
		}
		return nil
	})
	run, err := owner.Begin(t.Context(), Prompt{Text: "must reach provider"})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	reset, err := owner.Reset(t.Context(), "fresh", "reset-at-prompt")
	if err != nil || reset.State != "accepted" {
		t.Fatal(reset, err)
	}
	once.Do(func() { close(release) })
	result, err := run.Wait()
	if err != nil || result.Err != nil || provider.CallCount() != 1 {
		t.Fatal("new prompt was swallowed", result, err, provider.CallCount())
	}
	reset, err = owner.Submission(t.Context(), "reset-at-prompt")
	if err != nil || reset.State != "completed" || len(owner.core.agent.State().Messages) != 0 {
		t.Fatal("reset didn't settle after first provider turn", reset, err)
	}
}
