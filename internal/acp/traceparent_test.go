package acp

import (
	"context"
	"testing"

	"go.harness.dev/harness/internal/tracecontext"
)

func TestPromptTraceparentWinsOverTheConnection(t *testing.T) {
	connection := tracecontext.SpanContext{TraceID: tracecontext.NewTraceID(), SpanID: tracecontext.NewSpanID(), Sampled: true}
	client := (&Client{}).WithRemoteParent(connection)
	const prompt = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

	if got := tracecontext.FromContext(client.traced(context.Background(), prompt)).Traceparent(); got != prompt {
		t.Fatalf("_meta.traceparent parent = %q, want %q", got, prompt)
	}
	if got := tracecontext.FromContext(client.traced(context.Background(), "not-a-traceparent")); got != connection {
		t.Fatalf("invalid _meta.traceparent parent = %+v, want the connection's", got)
	}
	if got := tracecontext.FromContext((&Client{}).traced(context.Background(), "")); got.IsValid() {
		t.Fatalf("untraced connection produced parent %+v", got)
	}
}
