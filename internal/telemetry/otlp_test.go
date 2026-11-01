package telemetry

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.harness.dev/harness/internal/tracecontext"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata golden files")

// goldenSpans covers every attribute kind, a root and a child span, and an
// error status, with fixed ids and times so the encoding is byte-stable.
func goldenSpans() []*span {
	trace := tracecontext.TraceID{0x4b, 0xf9, 0x2f, 0x35, 0x77, 0xb3, 0x4d, 0xa6, 0xa3, 0xce, 0x92, 0x9d, 0x0e, 0x0e, 0x47, 0x36}
	root := tracecontext.SpanID{0x00, 0xf0, 0x67, 0xaa, 0x0b, 0xa9, 0x02, 0xb7}
	child := tracecontext.SpanID{0x53, 0x99, 0x5c, 0x3f, 0x42, 0xcd, 0x8a, 0xd8}
	start := time.Unix(1_700_000_000, 123_456_789)
	return []*span{
		{
			traceID: trace, spanID: root, name: "invoke_agent tempest", kind: spanKindInternal,
			start: start, end: start.Add(2 * time.Second),
			attributes: []attribute{
				stringAttribute("gen_ai.operation.name", "invoke_agent"),
				intAttribute("gen_ai.aggregated_usage.input_tokens", 9_007_199_254_740_993),
				doubleAttribute("tempest.cost.usd", 0.0125),
				boolAttribute("tempest.cost.unknown", false),
			},
		},
		{
			traceID: trace, spanID: child, parentID: root, name: "chat claude-sonnet-4-5", kind: spanKindClient,
			start: start.Add(time.Millisecond), end: start.Add(time.Second),
			attributes: []attribute{
				stringsAttribute("gen_ai.response.finish_reasons", []string{"tool_call"}),
				deferredAttribute("gen_ai.input.messages", func() string { return `[{"role":"user"}]` }),
				stringAttribute("error.type", "overloaded"),
			},
			status: statusError, message: "provider overloaded",
		},
	}
}

func TestEncodeTracesMatchesGolden(t *testing.T) {
	body, err := encodeTraces(newResource("tempest", "1.2.3", map[string]string{"deployment.environment": "test"}), goldenSpans())
	if err != nil {
		t.Fatal(err)
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, body, "", "  "); err != nil {
		t.Fatal(err)
	}
	indented.WriteByte('\n')
	path := filepath.Join("testdata", "traces.golden.json")
	if *updateGolden {
		if err := os.WriteFile(path, indented.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(indented.Bytes(), want) {
		t.Fatalf("OTLP JSON differs from %s:\n%s", path, indented.Bytes())
	}
}

// TestEncodeTracesProto3Mapping checks the mapping rules the golden file
// encodes, so a regenerated golden cannot silently break them.
func TestEncodeTracesProto3Mapping(t *testing.T) {
	body, err := encodeTraces(newResource("tempest", "", nil), goldenSpans())
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []struct {
					TraceID           string          `json:"traceId"`
					SpanID            string          `json:"spanId"`
					ParentSpanID      *string         `json:"parentSpanId"`
					Kind              json.RawMessage `json:"kind"`
					StartTimeUnixNano json.RawMessage `json:"startTimeUnixNano"`
					Attributes        []struct {
						Key   string                     `json:"key"`
						Value map[string]json.RawMessage `json:"value"`
					} `json:"attributes"`
				} `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	spans := decoded.ResourceSpans[0].ScopeSpans[0].Spans
	if spans[0].TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || spans[0].SpanID != "00f067aa0ba902b7" {
		t.Fatalf("ids are not lowercase hex: %q %q", spans[0].TraceID, spans[0].SpanID)
	}
	if spans[0].ParentSpanID != nil || spans[1].ParentSpanID == nil || *spans[1].ParentSpanID != spans[0].SpanID {
		t.Fatal("root must omit parentSpanId and the child must name the root")
	}
	if string(spans[1].Kind) != "3" {
		t.Fatalf("kind = %s, want the integer enum 3", spans[1].Kind)
	}
	if string(spans[0].StartTimeUnixNano) != `"1700000000123456789"` {
		t.Fatalf("startTimeUnixNano = %s, want a decimal string", spans[0].StartTimeUnixNano)
	}
	if got := string(spans[0].Attributes[1].Value["intValue"]); got != `"9007199254740993"` {
		t.Fatalf("intValue = %s, want a string that keeps 64-bit precision", got)
	}
}
