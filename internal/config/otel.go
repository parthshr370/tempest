package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// OTLPProtocol is the only OTLP transport tempest speaks: HTTP with the
// proto3 JSON encoding, written with the standard library.
const OTLPProtocol = "http/json"

const (
	// defaultOTLPTimeout and defaultMetricExportInterval are the
	// OpenTelemetry SDK defaults for OTEL_EXPORTER_OTLP_TIMEOUT and
	// OTEL_METRIC_EXPORT_INTERVAL.
	defaultOTLPTimeout          = 10 * time.Second
	defaultMetricExportInterval = 60 * time.Second
	defaultOTelServiceName      = "tempest"
	tracesPath                  = "/v1/traces"
	metricsPath                 = "/v1/metrics"
)

// otelFileSettings is the `otel` settings object. Each key has the standard
// OTEL_* variable as its environment input (settingInputs).
type otelFileSettings struct {
	Disabled               *bool             `json:"disabled"`
	Endpoint               *string           `json:"endpoint"`
	TracesEndpoint         *string           `json:"traces_endpoint"`
	MetricsEndpoint        *string           `json:"metrics_endpoint"`
	Protocol               *string           `json:"protocol"`
	TracesProtocol         *string           `json:"traces_protocol"`
	MetricsProtocol        *string           `json:"metrics_protocol"`
	Headers                map[string]string `json:"headers"`
	Compression            *string           `json:"compression"`
	TimeoutMS              *int              `json:"timeout_ms"`
	ServiceName            *string           `json:"service_name"`
	ResourceAttributes     map[string]string `json:"resource_attributes"`
	CaptureMessageContent  *bool             `json:"capture_message_content"`
	MetricExportIntervalMS *int              `json:"metric_export_interval_ms"`
}

// OTelSettings is the resolved telemetry export configuration. Empty
// endpoints mean that signal is not exported over OTLP; metrics can still be
// scraped from `serve -listen` at /metrics unless Disabled.
type OTelSettings struct {
	Disabled              bool
	TracesEndpoint        string
	MetricsEndpoint       string
	Headers               map[string]string
	Gzip                  bool
	Timeout               time.Duration
	ServiceName           string
	ResourceAttributes    map[string]string
	CaptureMessageContent bool
	MetricExportInterval  time.Duration
}

// OTel returns the telemetry settings. Headers carry credentials; callers
// must not log them.
func (c *ResolvedConfig) OTel() OTelSettings {
	settings := c.otel
	settings.Headers = cloneStringMap(settings.Headers)
	settings.ResourceAttributes = cloneStringMap(settings.ResourceAttributes)
	return settings
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

// resolveOTel validates the merged `otel` object. A protocol other than
// http/json is an error naming the supported value, so a collector expecting
// gRPC or protobuf never silently receives JSON.
func resolveOTel(file *otelFileSettings) (OTelSettings, error) {
	if file == nil {
		file = &otelFileSettings{}
	}
	for field, protocol := range map[string]*string{"otel.protocol": file.Protocol, "otel.traces_protocol": file.TracesProtocol, "otel.metrics_protocol": file.MetricsProtocol} {
		if value := settingString(protocol); value != "" && value != OTLPProtocol {
			return OTelSettings{}, &Error{Field: field, Err: fmt.Errorf("OTLP protocol %q is not supported; tempest exports %s (set OTEL_EXPORTER_OTLP_PROTOCOL=%s)", value, OTLPProtocol, OTLPProtocol)}
		}
	}
	settings := OTelSettings{
		Disabled:              settingValue(file.Disabled),
		Headers:               cloneStringMap(file.Headers),
		Timeout:               defaultOTLPTimeout,
		ServiceName:           defaultOTelServiceName,
		ResourceAttributes:    cloneStringMap(file.ResourceAttributes),
		CaptureMessageContent: settingValue(file.CaptureMessageContent),
		MetricExportInterval:  defaultMetricExportInterval,
	}
	if name := settingString(file.ServiceName); name != "" {
		settings.ServiceName = name
	}
	switch compression := strings.ToLower(settingString(file.Compression)); compression {
	case "", "none":
	case "gzip":
		settings.Gzip = true
	default:
		return OTelSettings{}, &Error{Field: "otel.compression", Err: fmt.Errorf("unknown compression %q (valid: gzip, none)", compression)}
	}
	if file.TimeoutMS != nil {
		if *file.TimeoutMS <= 0 || *file.TimeoutMS > maxRetryDurationMS {
			return OTelSettings{}, &Error{Field: "otel.timeout_ms", Err: errors.New("must be a positive number of milliseconds")}
		}
		settings.Timeout = time.Duration(*file.TimeoutMS) * time.Millisecond
	}
	if file.MetricExportIntervalMS != nil {
		if *file.MetricExportIntervalMS <= 0 || *file.MetricExportIntervalMS > maxRetryDurationMS {
			return OTelSettings{}, &Error{Field: "otel.metric_export_interval_ms", Err: errors.New("must be a positive number of milliseconds")}
		}
		settings.MetricExportInterval = time.Duration(*file.MetricExportIntervalMS) * time.Millisecond
	}
	base := settingString(file.Endpoint)
	var err error
	if settings.TracesEndpoint, err = otlpSignalURL("otel.traces_endpoint", settingString(file.TracesEndpoint), base, tracesPath); err != nil {
		return OTelSettings{}, err
	}
	if settings.MetricsEndpoint, err = otlpSignalURL("otel.metrics_endpoint", settingString(file.MetricsEndpoint), base, metricsPath); err != nil {
		return OTelSettings{}, err
	}
	return settings, nil
}

// otlpSignalURL applies the OTLP/HTTP endpoint rules: a signal-specific
// endpoint is used as given, while the base endpoint gets the signal path
// appended.
func otlpSignalURL(field, signal, base, path string) (string, error) {
	endpoint := signal
	if endpoint == "" && base != "" {
		field = "otel.endpoint"
		endpoint = strings.TrimSuffix(base, "/") + path
	}
	if endpoint == "" {
		return "", nil
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", &Error{Field: field, Err: fmt.Errorf("must be an absolute http or https URL, got %q", redactURL(endpoint))}
	}
	return endpoint, nil
}

// redactURL drops userinfo and query from an endpoint shown in an error.
func redactURL(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "[unparseable URL]"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	return parsed.String()
}

// parseKeyValuePairs reads the OTEL_EXPORTER_OTLP_HEADERS and
// OTEL_RESOURCE_ATTRIBUTES format: comma-separated key=value pairs with
// percent-encoded values.
func parseKeyValuePairs(text string) (map[string]string, error) {
	pairs := map[string]string{}
	for _, item := range strings.Split(text, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		key, value, found := strings.Cut(item, "=")
		key = strings.TrimSpace(key)
		if !found || key == "" {
			return nil, errors.New("want comma-separated key=value pairs")
		}
		decoded, err := url.PathUnescape(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("value for %q: %w", key, err)
		}
		pairs[key] = decoded
	}
	return pairs, nil
}
