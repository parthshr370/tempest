package tempest

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/tempest/durable"
)

// An active run that finishes after Close has started must not release the
// store under a task drain that registered before Close sealed the session.
func TestActiveRunFinalizationJoinsRegisteredDrain(t *testing.T) {
	provider := faux.New(faux.Options{})
	entered := make(chan struct{})
	streamFn := func(ctx context.Context, model types.Model, c types.Context, options *types.SimpleStreamOptions) *stream.AssistantStream {
		close(entered)
		<-ctx.Done()
		return provider.StreamSimple(ctx, model, c, options)
	}
	owner, err := Open(t.Context(), Options{Cwd: t.TempDir(), SessionDir: t.TempDir(), New: &NewSession{}, Stack: &StackConfig{Model: provider.Model(), StreamFn: streamFn}})
	if err != nil {
		t.Fatal(err)
	}
	run, err := owner.Begin(t.Context(), Prompt{Text: "hold until Close"})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	// Hold a registered drain where a descheduled ResumePending would sit:
	// past registration, before its first store access.
	registered, release := make(chan struct{}), make(chan struct{})
	drained := make(chan error, 1)
	go func() {
		drained <- owner.core.drainTasks(func() error {
			close(registered)
			<-release
			_, err := owner.core.store.Read(context.Background(), durable.Query{})
			return errors.Join(err, owner.core.jobs.Tasks.Resume(context.Background()))
		})
	}()
	<-registered
	closed := make(chan error, 1)
	go func() { closed <- owner.Close(context.Background()) }()
	runDone := make(chan struct{})
	go func() { _, _ = run.Wait(); close(runDone) }()
	select {
	case <-runDone:
		t.Error("active run finalized the session while a registered drain was running")
	case <-time.After(250 * time.Millisecond):
	}
	close(release)
	if err := <-drained; err != nil && !errors.Is(err, ErrClosed) {
		t.Errorf("drain saw the store closed under it: %v", err)
	}
	if err := <-closed; err != nil {
		t.Errorf("Close = %v", err)
	}
	<-runDone
}
