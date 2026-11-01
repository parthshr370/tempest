package compaction

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/stream"
	ptypes "go.harness.dev/harness/internal/engine/types"
)

// summaryWriter answers every summarization request with a summary of
// summaryBytes and counts the requests.
type summaryWriter struct {
	mu           sync.Mutex
	requests     int
	summaryBytes int
}

func (w *summaryWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.requests
}

func (w *summaryWriter) stream() agent.StreamFn {
	return func(_ context.Context, model ptypes.Model, _ ptypes.Context, _ *ptypes.SimpleStreamOptions) *stream.AssistantStream {
		w.mu.Lock()
		w.requests++
		w.mu.Unlock()
		response := stream.NewAssistantStream(model.API, model.Provider, model.ID)
		message := ptypes.AssistantMessage{StopReason: ptypes.StopStop, Content: []ptypes.ContentBlock{ptypes.NewText(strings.Repeat("s", w.summaryBytes))}, Usage: ptypes.Usage{Input: 1, TotalTokens: 1}}
		response.Push(ptypes.StreamEvent{Type: ptypes.EvDone, Reason: ptypes.StopStop, Message: &message})
		response.End()
		return response
	}
}

// A summary sends at most the requests its history takes when every update
// carries a summary as large as the model may answer. Summaries of that
// size reach the bound exactly; a model writing past it ends the summary
// with an error at the bound, rather than sending ever more, smaller
// requests.
func TestSummaryRequestsStopAtTheBoundTheirHistoryAndWindowGive(t *testing.T) {
	const (
		window, output = 4000, 1600
		historyBytes   = 100_000
		// A summary this much past the full size leaves each update less
		// room than the bound was planned with.
		overrunBytes = 64
	)
	model := ptypes.Model{ID: "small", ContextWindow: window, MaxTokens: output}
	settings := ModelSettings(window, output)
	fullSize := summaryMaxTokens(model, settings) * bytesPerToken
	messages := []ptypes.Message{userMsg(strings.Repeat("u", historyBytes))}

	full := &summaryWriter{summaryBytes: fullSize}
	if _, err := (Summarizer{Stream: full.stream()}).Summary(context.Background(), model, settings, messages, ""); err != nil {
		t.Fatalf("full-size summaries: %v", err)
	}
	bound := full.count()
	if bound < 2 {
		t.Fatalf("history summarized in %d requests, want several", bound)
	}

	overrun := &summaryWriter{summaryBytes: fullSize + overrunBytes}
	_, err := Summarizer{Stream: overrun.stream()}.Summary(context.Background(), model, settings, messages, "")
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("summaries past their output size: err=%v after %d requests, want an error at the bound of %d", err, overrun.count(), bound)
	}
	if requests := overrun.count(); requests != bound {
		t.Fatalf("%d requests sent, want the bound of %d", requests, bound)
	}
}

// Every summary request goes through the meter, and one the meter does not
// admit is never sent: the summary stops there with the meter's error and
// the usage of the requests it did send.
func TestSummaryRequestsAreEachMetered(t *testing.T) {
	const (
		window, output = 4000, 1600
		historyBytes   = 100_000
		admitted       = 2
	)
	model := ptypes.Model{ID: "small", ContextWindow: window, MaxTokens: output}
	refused := errors.New("cost cap spent")
	metered := 0
	meter := func(ctx context.Context, _ ptypes.Model, send func(context.Context) (SummaryResult, error)) (SummaryResult, error) {
		if metered == admitted {
			return SummaryResult{}, refused
		}
		metered++
		return send(ctx)
	}
	writer := &summaryWriter{summaryBytes: 1}
	result, err := Summarizer{Stream: writer.stream(), Meter: meter}.Summary(context.Background(), model, ModelSettings(window, output), []ptypes.Message{userMsg(strings.Repeat("u", historyBytes))}, "")
	if !errors.Is(err, refused) {
		t.Fatalf("err = %v, want the meter's refusal", err)
	}
	if requests := writer.count(); requests != admitted || result.Usage.TotalTokens != admitted {
		t.Fatalf("%d requests sent with %d tokens of usage, want only the %d admitted", requests, result.Usage.TotalTokens, admitted)
	}
}
