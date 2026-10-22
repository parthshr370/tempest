package compaction

import (
	"context"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/stream"
	types "go.harness.dev/harness/internal/engine/types"
)

func TestSummaryCancellationJoinsAndRetainsReportedUsage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, joined := make(chan struct{}), make(chan struct{})
	usage := types.Usage{Input: 8, TotalTokens: 8}
	fn := agent.StreamFn(func(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream {
		response := stream.NewAssistantStream(model.API, model.Provider, model.ID)
		go func() {
			close(entered)
			<-ctx.Done()
			message := types.AssistantMessage{StopReason: types.StopAborted, ErrorMessage: ctx.Err().Error(), Usage: usage}
			close(joined)
			response.EndWith(&message)
		}()
		return response
	})
	type result struct {
		summary SummaryResult
		err     error
	}
	done := make(chan result, 1)
	go func() {
		summary, err := GenerateSummaryResult(ctx, fn, types.Model{ID: "m"}, DefaultCompactionSettings, []types.Message{types.UserMessage{Content: types.StringContent("summarize")}}, "")
		done <- result{summary, err}
	}()
	<-entered
	cancel()
	got := <-done
	if got.err == nil || got.summary.Usage != usage {
		t.Fatalf("result=%+v err=%v", got.summary, got.err)
	}
	select {
	case <-joined:
	default:
		t.Fatal("summarizer returned without joining producer")
	}
}
