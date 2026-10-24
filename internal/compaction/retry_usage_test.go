package compaction

import (
	"context"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/stream"
	types "go.harness.dev/harness/internal/engine/types"
)

func TestSummaryCountsUsageOfRetriedAttempts(t *testing.T) {
	fn := agent.StreamFn(func(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream {
		response := stream.NewAssistantStream(model.API, model.Provider, model.ID)
		go func() {
			response.Push(types.StreamEvent{Type: types.EvStart})
			response.Push(types.StreamEvent{Type: types.EvRetry, Attempt: 1, Err: &types.AssistantMessage{StopReason: types.StopError, Usage: types.Usage{Input: 8, TotalTokens: 8}}})
			response.Push(types.StreamEvent{Type: types.EvDone, Reason: types.StopStop, Message: &types.AssistantMessage{
				StopReason: types.StopStop,
				Content:    []types.ContentBlock{types.NewText("summary")},
				Usage:      types.Usage{Input: 8, Output: 2, TotalTokens: 10},
			}})
		}()
		return response
	})

	summary, err := GenerateSummaryResult(context.Background(), fn, types.Model{ID: "m"}, DefaultCompactionSettings, []types.Message{types.UserMessage{Content: types.StringContent("summarize")}}, "")

	if err != nil || summary.Text != "summary" || summary.Usage.Input != 16 || summary.Usage.TotalTokens != 18 {
		t.Fatalf("summary = %+v, err = %v", summary, err)
	}
}
