// Package tracecontext carries W3C Trace Context through a context.Context.
//
// A serve request's traceparent becomes the parent of the run it starts; the
// agent loop derives one span per run, model request and tool call from it;
// provider and MCP HTTP requests send the current span as their traceparent
// header and subprocesses receive it as TRACEPARENT. A tracestate that came
// with the remote parent goes along unchanged, as tracestate and TRACESTATE.
// The package only moves identifiers: spans are built and exported by
// internal/telemetry from the agent event stream, which is delivered under
// the same contexts.
package tracecontext

import (
	"context"
	"encoding/hex"
	"math/rand/v2"
	"net/http"
	"os"
	"slices"
	"strings"
)

// Header is the W3C Trace Context request header.
const Header = "traceparent"

// StateHeader is the W3C Trace Context vendor-state header.
const StateHeader = "tracestate"

// EnvVar is the variable a subprocess reads its parent span from, and
// StateEnvVar its tracestate, as proposed for OpenTelemetry environment
// carriers.
const (
	EnvVar      = "TRACEPARENT"
	StateEnvVar = "TRACESTATE"
)

const (
	traceparentVersion = "00"
	// traceparentLength is the length of a version 00 traceparent:
	// 2 + 1 + 32 + 1 + 16 + 1 + 2.
	traceparentLength = 55
	flagSampled       = 0x01
	// maxTraceStateMembers is the W3C limit on tracestate list members.
	maxTraceStateMembers = 32
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
// TraceState is the tracestate that arrived with the remote parent; tempest
// adds no entry of its own, so every span below that parent carries it
// unchanged.
type SpanContext struct {
	TraceID    TraceID
	SpanID     SpanID
	Sampled    bool
	TraceState string
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

// Inherit gives ctx the span of from, including from's own parent or an
// untraced scope that shadows a span further up. Work that runs under a
// longer-lived context than the call that started it, such as a background
// job under its session's scope, uses it to stay under that call's span. A
// from that never carried trace context leaves ctx unchanged.
func Inherit(ctx, from context.Context) context.Context {
	current, ok := from.Value(contextKey{}).(scope)
	if !ok {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, current)
}

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
	span := childOf(parent)
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
	return context.WithValue(ctx, contextKey{}, scope{span: childOf(parent), parent: parent})
}

// childOf is a new span in parent's trace, with its sampled flag and
// tracestate.
func childOf(parent SpanContext) SpanContext {
	return SpanContext{TraceID: parent.TraceID, SpanID: NewSpanID(), Sampled: parent.Sampled, TraceState: parent.TraceState}
}

// Extract reads the traceparent and tracestate headers of an incoming
// request. A tracestate is read only beside a valid traceparent, and a
// malformed one is dropped without affecting it.
func Extract(header http.Header) (SpanContext, bool) {
	values := header.Values(Header)
	if len(values) != 1 {
		// The specification treats a repeated traceparent as invalid.
		return SpanContext{}, false
	}
	parent, ok := Parse(values[0])
	if ok {
		parent.TraceState = parseTraceState(header.Values(StateHeader))
	}
	return parent, ok
}

// ParseWithState parses trace context carried as two plain values rather
// than headers, as in ACP's _meta: tracestate is read only beside a valid
// traceparent, and a malformed one is dropped without affecting it.
func ParseWithState(traceparent, tracestate string) (SpanContext, bool) {
	parent, ok := Parse(traceparent)
	if ok {
		parent.TraceState = parseTraceState([]string{tracestate})
	}
	return parent, ok
}

// parseTraceState combines tracestate header fields into one list, as
// RFC 7230 combines repeated fields, leaving out empty members. It returns
// "" when a member is malformed, a key repeats or there are more than 32
// members.
func parseTraceState(values []string) string {
	var members []string
	for _, value := range values {
		for rest, more := value, true; more; {
			var member string
			member, rest, more = strings.Cut(rest, ",")
			member = strings.Trim(member, " \t")
			if member == "" {
				continue
			}
			key, entry, ok := strings.Cut(member, "=")
			if !ok || !validStateKey(key) || !validStateValue(entry) || len(members) == maxTraceStateMembers ||
				slices.ContainsFunc(members, func(kept string) bool { return strings.HasPrefix(kept, key+"=") }) {
				return ""
			}
			members = append(members, member)
		}
	}
	return strings.Join(members, ",")
}

// validStateKey reports whether key is a W3C tracestate key: a simple key of
// up to 256 characters, or tenant@system.
func validStateKey(key string) bool {
	tenant, system, multiTenant := strings.Cut(key, "@")
	if !multiTenant {
		return len(key) <= 256 && key != "" && isLowerAlpha(key[0]) && keyChars(key[1:])
	}
	return tenant != "" && len(tenant) <= 241 && (isLowerAlpha(tenant[0]) || isDigit(tenant[0])) && keyChars(tenant[1:]) &&
		system != "" && len(system) <= 14 && isLowerAlpha(system[0]) && keyChars(system[1:])
}

func keyChars(text string) bool {
	for _, character := range []byte(text) {
		if !isLowerAlpha(character) && !isDigit(character) && character != '_' && character != '-' && character != '*' && character != '/' {
			return false
		}
	}
	return true
}

func isLowerAlpha(character byte) bool { return character >= 'a' && character <= 'z' }

func isDigit(character byte) bool { return character >= '0' && character <= '9' }

// validStateValue reports whether value is a W3C tracestate value: 1 to 256
// printable ASCII characters other than ',' and '='. Surrounding whitespace
// was trimmed with the member.
func validStateValue(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for _, character := range []byte(value) {
		if character < 0x20 || character > 0x7e || character == ',' || character == '=' {
			return false
		}
	}
	return true
}

// Inject sets the traceparent and tracestate headers of an outgoing request
// to the current span's. An untraced ctx leaves both as the caller set them.
func Inject(ctx context.Context, header http.Header) {
	span := FromContext(ctx)
	if !span.IsValid() {
		return
	}
	header.Set(Header, span.Traceparent())
	if span.TraceState != "" {
		header.Set(StateHeader, span.TraceState)
	} else {
		header.Del(StateHeader)
	}
}

// Environ returns env with TRACEPARENT and TRACESTATE set to the current
// span's, replacing any inherited values; TRACESTATE is left out when the
// trace has none. A nil env stands for the process environment, as with
// exec.Cmd.Env. An untraced ctx returns env unchanged.
func Environ(ctx context.Context, env []string) []string {
	span := FromContext(ctx)
	if !span.IsValid() {
		return env
	}
	if env == nil {
		env = os.Environ()
	}
	parentPrefix, statePrefix := EnvVar+"=", StateEnvVar+"="
	result := make([]string, 0, len(env)+2)
	for _, entry := range env {
		if !strings.HasPrefix(entry, parentPrefix) && !strings.HasPrefix(entry, statePrefix) {
			result = append(result, entry)
		}
	}
	result = append(result, parentPrefix+span.Traceparent())
	if span.TraceState != "" {
		result = append(result, statePrefix+span.TraceState)
	}
	return result
}
