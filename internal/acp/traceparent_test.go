package acp

import (
	"context"
	"encoding/json"
	"testing"

	"go.harness.dev/harness/internal/tracecontext"
)

func TestPromptTraceparentWinsOverTheConnection(t *testing.T) {
	connection := tracecontext.SpanContext{TraceID: tracecontext.NewTraceID(), SpanID: tracecontext.NewSpanID(), Sampled: true}
	client := (&Client{}).WithRemoteParent(connection)
	const prompt = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

	if got := tracecontext.FromContext(client.traced(context.Background(), traceMeta{Traceparent: prompt})).Traceparent(); got != prompt {
		t.Fatalf("_meta.traceparent parent = %q, want %q", got, prompt)
	}
	if got := tracecontext.FromContext(client.traced(context.Background(), traceMeta{Traceparent: "not-a-traceparent"})); got != connection {
		t.Fatalf("invalid _meta.traceparent parent = %+v, want the connection's", got)
	}
	if got := tracecontext.FromContext((&Client{}).traced(context.Background(), traceMeta{})); got.IsValid() {
		t.Fatalf("untraced connection produced parent %+v", got)
	}
}

// A prompt's _meta.tracestate travels with its traceparent (W3C Trace
// Context: a vendor that receives tracestate passes it on); a malformed one
// is dropped without losing the parent.
func TestPromptTracestateTravelsWithItsTraceparent(t *testing.T) {
	const prompt = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	for _, test := range []struct{ tracestate, want string }{
		{"vendor=opaque,other=1", "vendor=opaque,other=1"},
		{"Not A Member", ""},
	} {
		var meta struct {
			Meta struct{ traceMeta } `json:"_meta"`
		}
		raw, err := json.Marshal(map[string]any{"_meta": map[string]string{"traceparent": prompt, "tracestate": test.tracestate}})
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			t.Fatal(err)
		}
		parent := tracecontext.FromContext((&Client{}).traced(context.Background(), meta.Meta.traceMeta))
		if parent.Traceparent() != prompt || parent.TraceState != test.want {
			t.Fatalf("tracestate %q: parent %q state %q, want %q", test.tracestate, parent.Traceparent(), parent.TraceState, test.want)
		}
	}
}
