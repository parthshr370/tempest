package telemetry

import (
	"encoding/json"
	"maps"
	"math"
	"slices"
	"strconv"
	"time"

	"go.harness.dev/harness/internal/tracecontext"
)

// This file is the OTLP/HTTP JSON wire format, written by hand: the proto3
// JSON mapping of opentelemetry-proto's ExportTraceServiceRequest and
// ExportMetricsServiceRequest. OTLP departs from plain proto3 JSON in two
// places that matter here: trace and span ids are lowercase hex, not base64,
// and enums are integers. 64-bit integers (timestamps, counts, intValue) are
// decimal strings, as proto3 JSON requires.

// instrumentationScope names tempest as the producer of every span and metric.
const instrumentationScope = "go.harness.dev/harness"

type valueKind int

const (
	valueString valueKind = iota
	valueInt
	valueDouble
	valueBool
	valueStrings
	// valueDeferred is a string computed at export time, off the turn loop,
	// for captured message content that is expensive to encode.
	valueDeferred
)

// attribute is one span, resource or data point attribute.
type attribute struct {
	key      string
	kind     valueKind
	text     string
	integer  int64
	number   float64
	flag     bool
	texts    []string
	deferred func() string
}

func stringAttribute(key, value string) attribute {
	return attribute{key: key, kind: valueString, text: value}
}

func intAttribute(key string, value int64) attribute {
	return attribute{key: key, kind: valueInt, integer: value}
}

func doubleAttribute(key string, value float64) attribute {
	return attribute{key: key, kind: valueDouble, number: value}
}

func boolAttribute(key string, value bool) attribute {
	return attribute{key: key, kind: valueBool, flag: value}
}

func stringsAttribute(key string, values []string) attribute {
	return attribute{key: key, kind: valueStrings, texts: values}
}

func deferredAttribute(key string, value func() string) attribute {
	return attribute{key: key, kind: valueDeferred, deferred: value}
}

// spanKind is the OTLP Span.SpanKind enum.
type spanKind int

const (
	spanKindInternal spanKind = 1
	spanKindClient   spanKind = 3
)

// statusCode is the OTLP Status.StatusCode enum; unset is omitted.
type statusCode int

const statusError statusCode = 2

// span is one finished span. Spans are immutable once handed to the batcher.
type span struct {
	traceID    tracecontext.TraceID
	spanID     tracecontext.SpanID
	parentID   tracecontext.SpanID
	name       string
	kind       spanKind
	start      time.Time
	end        time.Time
	attributes []attribute
	events     []spanEvent
	status     statusCode
	message    string
}

// spanEvent is something that happened at one point during a span.
type spanEvent struct {
	name       string
	at         time.Time
	attributes []attribute
}

type wireAnyValue struct {
	StringValue *string         `json:"stringValue,omitempty"`
	BoolValue   *bool           `json:"boolValue,omitempty"`
	IntValue    string          `json:"intValue,omitempty"`
	DoubleValue *float64        `json:"doubleValue,omitempty"`
	ArrayValue  *wireArrayValue `json:"arrayValue,omitempty"`
}

type wireArrayValue struct {
	Values []wireAnyValue `json:"values"`
}

type wireKeyValue struct {
	Key   string       `json:"key"`
	Value wireAnyValue `json:"value"`
}

type wireResource struct {
	Attributes []wireKeyValue `json:"attributes"`
}

type wireScope struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type wireStatus struct {
	Code    statusCode `json:"code,omitempty"`
	Message string     `json:"message,omitempty"`
}

type wireSpanEvent struct {
	TimeUnixNano string         `json:"timeUnixNano"`
	Name         string         `json:"name"`
	Attributes   []wireKeyValue `json:"attributes,omitempty"`
}

type wireSpan struct {
	TraceID           string          `json:"traceId"`
	SpanID            string          `json:"spanId"`
	ParentSpanID      string          `json:"parentSpanId,omitempty"`
	Name              string          `json:"name"`
	Kind              spanKind        `json:"kind"`
	StartTimeUnixNano string          `json:"startTimeUnixNano"`
	EndTimeUnixNano   string          `json:"endTimeUnixNano"`
	Attributes        []wireKeyValue  `json:"attributes,omitempty"`
	Events            []wireSpanEvent `json:"events,omitempty"`
	Status            wireStatus      `json:"status"`
}

type wireScopeSpans struct {
	Scope wireScope  `json:"scope"`
	Spans []wireSpan `json:"spans"`
}

type wireResourceSpans struct {
	Resource   wireResource     `json:"resource"`
	ScopeSpans []wireScopeSpans `json:"scopeSpans"`
}

type wireTracesRequest struct {
	ResourceSpans []wireResourceSpans `json:"resourceSpans"`
}

func wireValue(attr attribute) (wireAnyValue, bool) {
	switch attr.kind {
	case valueString:
		text := attr.text
		return wireAnyValue{StringValue: &text}, true
	case valueDeferred:
		text := attr.deferred()
		return wireAnyValue{StringValue: &text}, true
	case valueInt:
		return wireAnyValue{IntValue: strconv.FormatInt(attr.integer, 10)}, true
	case valueDouble:
		if math.IsNaN(attr.number) || math.IsInf(attr.number, 0) {
			// JSON has no literal for these; such a value means nothing to a backend.
			return wireAnyValue{}, false
		}
		number := attr.number
		return wireAnyValue{DoubleValue: &number}, true
	case valueBool:
		flag := attr.flag
		return wireAnyValue{BoolValue: &flag}, true
	case valueStrings:
		values := make([]wireAnyValue, len(attr.texts))
		for i := range attr.texts {
			values[i] = wireAnyValue{StringValue: &attr.texts[i]}
		}
		return wireAnyValue{ArrayValue: &wireArrayValue{Values: values}}, true
	}
	return wireAnyValue{}, false
}

func wireAttributes(attrs []attribute) []wireKeyValue {
	if len(attrs) == 0 {
		return nil
	}
	result := make([]wireKeyValue, 0, len(attrs))
	for _, attr := range attrs {
		if value, ok := wireValue(attr); ok {
			result = append(result, wireKeyValue{Key: attr.key, Value: value})
		}
	}
	return result
}

func unixNano(at time.Time) string { return strconv.FormatInt(at.UnixNano(), 10) }

// resource is the process identity attached to every export.
type resource struct {
	attributes []attribute
	version    string
}

// newResource merges the configured resource attributes with service and SDK
// identity. service.name from OTEL_SERVICE_NAME wins over the same key in
// OTEL_RESOURCE_ATTRIBUTES, as the specification requires. Attributes are
// sorted so an export is byte-stable.
func newResource(serviceName, version string, configured map[string]string) resource {
	values := map[string]string{}
	for key, value := range configured {
		values[key] = value
	}
	values["service.name"] = serviceName
	values["telemetry.sdk.name"] = "tempest"
	values["telemetry.sdk.language"] = "go"
	if version != "" {
		values["service.version"] = version
		values["telemetry.sdk.version"] = version
	}
	attrs := make([]attribute, 0, len(values))
	for _, key := range slices.Sorted(maps.Keys(values)) {
		attrs = append(attrs, stringAttribute(key, values[key]))
	}
	return resource{attributes: attrs, version: version}
}

// encodeTraces renders one ExportTraceServiceRequest.
func encodeTraces(res resource, spans []*span) ([]byte, error) {
	wire := make([]wireSpan, len(spans))
	for i, finished := range spans {
		entry := wireSpan{
			TraceID:           finished.traceID.String(),
			SpanID:            finished.spanID.String(),
			Name:              finished.name,
			Kind:              finished.kind,
			StartTimeUnixNano: unixNano(finished.start),
			EndTimeUnixNano:   unixNano(finished.end),
			Attributes:        wireAttributes(finished.attributes),
			Status:            wireStatus{Code: finished.status, Message: finished.message},
		}
		if finished.parentID != (tracecontext.SpanID{}) {
			entry.ParentSpanID = finished.parentID.String()
		}
		if len(finished.events) > 0 {
			entry.Events = make([]wireSpanEvent, len(finished.events))
			for j, event := range finished.events {
				entry.Events[j] = wireSpanEvent{TimeUnixNano: unixNano(event.at), Name: event.name, Attributes: wireAttributes(event.attributes)}
			}
		}
		wire[i] = entry
	}
	request := wireTracesRequest{ResourceSpans: []wireResourceSpans{{
		Resource:   wireResource{Attributes: wireAttributes(res.attributes)},
		ScopeSpans: []wireScopeSpans{{Scope: wireScope{Name: instrumentationScope, Version: res.version}, Spans: wire}},
	}}}
	return json.Marshal(request)
}
