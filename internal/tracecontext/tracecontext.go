// Package tracecontext carries W3C Trace Context through a context.Context.
//
// A serve request's traceparent becomes the parent of the run it starts; the
// agent loop derives one span per run, model request and tool call from it;
// provider and MCP HTTP requests send the current span as their traceparent
// header and subprocesses receive it as TRACEPARENT. The package only moves
// identifiers: spans are built and exported by internal/telemetry from the
// agent event stream, which is delivered under the same contexts.
package tracecontext

import (
	"context"
	"encoding/hex"
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
)

// Header is the W3C Trace Context request header.
const Header = "traceparent"

// EnvVar is the variable a subprocess reads its parent span from, as
// proposed for OpenTelemetry environment carriers.
const EnvVar = "TRACEPARENT"

const (
	traceparentVersion = "00"
	// traceparentLength is the length of a version 00 traceparent:
	// 2 + 1 + 32 + 1 + 16 + 1 + 2.
	traceparentLength = 55
	flagSampled       = 0x01
)

// TraceID is a W3C trace id; the all-zero value is invalid.
type TraceID [16]byte

// SpanID is a W3C parent (span) id; the all-zero value is invalid.
type SpanID [8]byte

// String returns the lowercase hex form used on the wire and in OTLP/JSON.
func (id TraceID) String() string { return hex.EncodeToString(id[:]) }

// String returns the lowercase hex form used on the wire and in OTLP/JSON.
func (id SpanID) String() string { return hex.EncodeToString(id[:]) }

// SpanContext identifies one span. Sampled carries the W3C sampled flag: a
// span whose remote parent was not sampled is propagated but not exported.
type SpanContext struct {
	TraceID TraceID
	SpanID  SpanID
	Sampled bool
}

// IsValid reports whether both identifiers are set.
func (s SpanContext) IsValid() bool { return s.TraceID != TraceID{} && s.SpanID != SpanID{} }

// Traceparent formats the span as a version 00 traceparent value.
func (s SpanContext) Traceparent() string {
	flags := "00"
	if s.Sampled {
		flags = "01"
	}
	return traceparentVersion + "-" + s.TraceID.String() + "-" + s.SpanID.String() + "-" + flags
}

// Parse reads a traceparent value. It accepts version 00 exactly and a later
// version's leading fields, rejects version ff, uppercase hex and all-zero
// identifiers, as the W3C Trace Context specification requires.
func Parse(value string) (SpanContext, bool) {
	value = strings.TrimSpace(value)
	if len(value) < traceparentLength {
		return SpanContext{}, false
	}
	version := value[:2]
	if !isLowerHex(version) || version == "ff" {
		return SpanContext{}, false
	}
	if version == traceparentVersion && len(value) != traceparentLength {
		return SpanContext{}, false
	}
	if len(value) > traceparentLength && value[traceparentLength] != '-' {
		return SpanContext{}, false
	}
	if value[2] != '-' || value[35] != '-' || value[52] != '-' {
		return SpanContext{}, false
	}
	traceHex, spanHex, flagsHex := value[3:35], value[36:52], value[53:55]
	if !isLowerHex(traceHex) || !isLowerHex(spanHex) || !isLowerHex(flagsHex) {
		return SpanContext{}, false
	}
	var parsed SpanContext
	if _, err := hex.Decode(parsed.TraceID[:], []byte(traceHex)); err != nil {
		return SpanContext{}, false
	}
	if _, err := hex.Decode(parsed.SpanID[:], []byte(spanHex)); err != nil {
		return SpanContext{}, false
	}
	var flags [1]byte
	if _, err := hex.Decode(flags[:], []byte(flagsHex)); err != nil {
		return SpanContext{}, false
	}
	parsed.Sampled = flags[0]&flagSampled != 0
	return parsed, parsed.IsValid()
}

func isLowerHex(text string) bool {
	for _, character := range []byte(text) {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

// NewTraceID returns a random nonzero trace id. Identifiers need uniqueness,
// not secrecy, so the runtime's fast generator is enough.
func NewTraceID() TraceID {
	var id TraceID
	for id == (TraceID{}) {
		high, low := rand.Uint64(), rand.Uint64()
		for i := range 8 {
			id[i] = byte(high >> (56 - 8*i))
			id[8+i] = byte(low >> (56 - 8*i))
		}
	}
	return id
}

// NewSpanID returns a random nonzero span id.
func NewSpanID() SpanID {
	var id SpanID
	for id == (SpanID{}) {
		value := rand.Uint64()
		for i := range 8 {
			id[i] = byte(value >> (56 - 8*i))
		}
	}
	return id
}

// scope is the current span and its parent. A zero span means tracing is off
// below this point, which shadows any span set further up.
type scope struct {
	span   SpanContext
	parent SpanContext
}

type contextKey struct{}

func scopeFrom(ctx context.Context) scope {
	current, _ := ctx.Value(contextKey{}).(scope)
	return current
}

// WithRemoteParent makes a span received from another process the current
// span, so the next run started under ctx becomes its child.
func WithRemoteParent(ctx context.Context, parent SpanContext) context.Context {
	if !parent.IsValid() {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, scope{span: parent})
}

// FromContext returns the current span, or the zero value when ctx is not traced.
func FromContext(ctx context.Context) SpanContext { return scopeFrom(ctx).span }

// ParentFromContext returns the current span's parent, or the zero value for
// a root or remote span.
func ParentFromContext(ctx context.Context) SpanContext { return scopeFrom(ctx).parent }

// StartRun begins an agent run's span. When traced is false the run and
// everything below it is untraced, even under a remote parent. Otherwise the
// run is a child of the current span (another run's tool call, or a serve
// request's traceparent) or the root of a new sampled trace.
func StartRun(ctx context.Context, traced bool) context.Context {
	parent := FromContext(ctx)
	if !traced {
		if parent.IsValid() {
			return context.WithValue(ctx, contextKey{}, scope{})
		}
		return ctx
	}
	span := SpanContext{TraceID: parent.TraceID, SpanID: NewSpanID(), Sampled: parent.Sampled}
	if !parent.IsValid() {
		span = SpanContext{TraceID: NewTraceID(), SpanID: span.SpanID, Sampled: true}
	}
	return context.WithValue(ctx, contextKey{}, scope{span: span, parent: parent})
}

// StartChild begins a span below the current one, or returns ctx unchanged
// when it is not traced.
func StartChild(ctx context.Context) context.Context {
	parent := FromContext(ctx)
	if !parent.IsValid() {
		return ctx
	}
	span := SpanContext{TraceID: parent.TraceID, SpanID: NewSpanID(), Sampled: parent.Sampled}
	return context.WithValue(ctx, contextKey{}, scope{span: span, parent: parent})
}

// Extract reads the traceparent header of an incoming request.
func Extract(header http.Header) (SpanContext, bool) {
	values := header.Values(Header)
	if len(values) != 1 {
		// The specification treats a repeated traceparent as invalid.
		return SpanContext{}, false
	}
	return Parse(values[0])
}

// Inject sets the traceparent header of an outgoing request to the current
// span. An untraced ctx leaves the header as the caller set it.
func Inject(ctx context.Context, header http.Header) {
	if span := FromContext(ctx); span.IsValid() {
		header.Set(Header, span.Traceparent())
	}
}

// Environ returns env with TRACEPARENT set to the current span, replacing any
// inherited value. A nil env stands for the process environment, as with
// exec.Cmd.Env. An untraced ctx returns env unchanged.
func Environ(ctx context.Context, env []string) []string {
	span := FromContext(ctx)
	if !span.IsValid() {
		return env
	}
	if env == nil {
		env = os.Environ()
	}
	prefix := EnvVar + "="
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return append(result, prefix+span.Traceparent())
}
