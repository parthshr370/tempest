package agent

import (
	"context"
	"testing"

	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
)

func TestAssistantResultKeepsUnknownPrice(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "ended", true: "canceled"}[canceled], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			response := stream.NewAssistantStream("test", "test", "unpriced")
			if canceled {
				cancel()
			} else {
				response.End()
			}
			message := assistantResult(ctx, response, AgentLoopConfig{Model: types.Model{ID: "unpriced", Cost: types.ModelCost{Unknown: true}}})
			if !message.Usage.Cost.Unknown {
				t.Fatalf("synthesized usage lost unknown price: %+v", message.Usage)
			}
			wantStop := types.StopError
			if canceled {
				wantStop = types.StopAborted
			}
			if message.StopReason != wantStop {
				t.Fatalf("stop = %s, want %s", message.StopReason, wantStop)
			}
		})
	}
}
