package telemetry

import (
	"encoding/json"
	"strconv"
	"testing"
)

// receivedAttribute is one OTLP/JSON key-value as a collector decodes it.
type receivedAttribute struct {
	Key   string `json:"key"`
	Value struct {
		StringValue *string  `json:"stringValue"`
		IntValue    string   `json:"intValue"`
		DoubleValue *float64 `json:"doubleValue"`
		BoolValue   *bool    `json:"boolValue"`
		ArrayValue  *struct {
			Values []struct {
				StringValue string `json:"stringValue"`
			} `json:"values"`
		} `json:"arrayValue"`
	} `json:"value"`
}

// receivedSpan is a span as a collector decodes it from OTLP/JSON.
type receivedSpan struct {
	TraceID      string              `json:"traceId"`
	SpanID       string              `json:"spanId"`
	ParentSpanID string              `json:"parentSpanId"`
	Name         string              `json:"name"`
	Kind         int                 `json:"kind"`
	Attributes   []receivedAttribute `json:"attributes"`
	Events       []receivedEvent     `json:"events"`
	Status       struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"status"`
}

// receivedEvent is a span event as a collector decodes it.
type receivedEvent struct {
	TimeUnixNano string              `json:"timeUnixNano"`
	Name         string              `json:"name"`
	Attributes   []receivedAttribute `json:"attributes"`
}

func decodeSpans(t *testing.T, body []byte) []receivedSpan {
	t.Helper()
	var request struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []receivedSpan `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatalf("decode OTLP traces: %v\n%s", err, body)
	}
	var spans []receivedSpan
	for _, resource := range request.ResourceSpans {
		for _, scope := range resource.ScopeSpans {
			spans = append(spans, scope.Spans...)
		}
	}
	return spans
}

// attr renders one attribute value as text, or "" when it is absent.
func (s receivedSpan) attr(key string) string { return attributeText(s.Attributes, key) }

// attr renders one event attribute value as text, or "" when it is absent.
func (e receivedEvent) attr(key string) string { return attributeText(e.Attributes, key) }

func attributeText(attributes []receivedAttribute, key string) string {
	for _, attribute := range attributes {
		if attribute.Key != key {
			continue
		}
		value := attribute.Value
		switch {
		case value.StringValue != nil:
			return *value.StringValue
		case value.IntValue != "":
			return value.IntValue
		case value.DoubleValue != nil:
			return strconv.FormatFloat(*value.DoubleValue, 'g', -1, 64)
		case value.BoolValue != nil:
			return strconv.FormatBool(*value.BoolValue)
		case value.ArrayValue != nil:
			text := ""
			for i, item := range value.ArrayValue.Values {
				if i > 0 {
					text += ","
				}
				text += item.StringValue
			}
			return text
		}
	}
	return ""
}

func (s receivedSpan) has(key string) bool {
	for _, attribute := range s.Attributes {
		if attribute.Key == key {
			return true
		}
	}
	return false
}
