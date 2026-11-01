package anthropic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/tracecontext"
)

func TestStreamSendsTraceparentOfTheRequestSpan(t *testing.T) {
	received := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get(tracecontext.Header)
		writeMinimalAnthropicStream(w)
	}))
	defer server.Close()
	model := baseModel()
	model.BaseURL = server.URL
	request := types.Context{Messages: []types.Message{types.UserMessage{Content: types.StringContent("hi")}}}
	options := &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{APIKey: "sk-test"}}

	span := tracecontext.SpanContext{TraceID: tracecontext.NewTraceID(), SpanID: tracecontext.NewSpanID(), Sampled: true}
	drainAssistantStream(t, StreamSimple(tracecontext.WithRemoteParent(context.Background(), span), model, request, options))
	if got := <-received; got != span.Traceparent() {
		t.Fatalf("traceparent = %q, want %q", got, span.Traceparent())
	}

	drainAssistantStream(t, StreamSimple(context.Background(), model, request, options))
	if got := <-received; got != "" {
		t.Fatalf("untraced request sent traceparent %q", got)
	}
}
