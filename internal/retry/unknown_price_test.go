package retry

import (
	"context"
	"testing"

	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
)

// A terminal message the retrier generates itself (exhausted, canceled) must
// keep the model's unknown price: the CLI prints cost_usd 0 instead of null
// for an unknown-price model whose call failed.
func TestGeneratedTerminalKeepsUnknownPrice(t *testing.T) {
	model := testModel()
	model.Cost.Unknown = true

	t.Run("attempts exhausted", func(t *testing.T) {
		cfg := testConfig(&recordedSleeper{})
		cfg.MaxAttempts = 1
		source := &fakeStreamer{streams: []*stream.AssistantStream{testStream(types.StreamEvent{Type: types.EvStart}, retryError("connection reset"))}}
		retrier, err := New(source, cfg)
		if err != nil {
			t.Fatal(err)
		}
		assertUnknownTerminal(t, collect(retrier.Stream(context.Background(), model, types.Context{}, nil)))
	})

	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		retrier, err := New(&fakeStreamer{}, testConfig(&recordedSleeper{}))
		if err != nil {
			t.Fatal(err)
		}
		assertUnknownTerminal(t, collect(retrier.Stream(ctx, model, types.Context{}, nil)))
	})
}

func assertUnknownTerminal(t *testing.T, events []types.StreamEvent) {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("no events")
	}
	terminal := events[len(events)-1]
	if terminal.Err == nil {
		t.Fatalf("terminal event = %+v, want an error message", terminal)
	}
	if !terminal.Err.Usage.Cost.Unknown {
		t.Fatalf("generated terminal usage cost = %+v, want Unknown for an unknown-price model", terminal.Err.Usage.Cost)
	}
}
