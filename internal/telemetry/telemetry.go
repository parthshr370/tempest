// Package telemetry exports OpenTelemetry traces and metrics for agent runs.
//
// Spans and metrics come from the agent event stream, the same stream that
// feeds the journal and ACP/A2A clients: a listener per agent turns events
// into finished spans and measurements. Export is OTLP/HTTP with JSON
// encoding, written with the standard library. Nothing here blocks the turn
// loop: spans go through a bounded queue that drops (and counts) on overflow,
// and network I/O happens on exporter goroutines.
package telemetry

import (
	"context"
	"errors"
	"net/http"
	"time"

	"go.harness.dev/harness/internal/config"
)

// Options configures one process's telemetry.
type Options struct {
	Settings config.OTelSettings
	// Version is reported as service.version.
	Version string
	// HTTPClient sends OTLP requests; nil uses a client on the default transport.
	HTTPClient *http.Client
	// SpanQueueSize, SpanBatchSize and SpanBatchInterval override the batch
	// span processor defaults; zero keeps the default.
	SpanQueueSize     int
	SpanBatchSize     int
	SpanBatchInterval time.Duration
}

// Telemetry is the process-wide exporter set. A nil *Telemetry is valid and
// records nothing, so callers need no enabled checks.
type Telemetry struct {
	settings config.OTelSettings
	resource resource
	client   *otlpClient
	spans    *spanBatcher
	health   *exportHealth

	metrics         *metricSet
	live            liveSessions
	metricsExporter *metricExporter
}

// New starts the exporters the settings ask for. It returns nil when
// telemetry is disabled (OTEL_SDK_DISABLED or otel.disabled).
func New(opts Options) *Telemetry {
	if opts.Settings.Disabled {
		return nil
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Transport: http.DefaultTransport}
	}
	userAgent := "tempest-otlp"
	if opts.Version != "" {
		userAgent += "/" + opts.Version
	}
	t := &Telemetry{
		settings: opts.Settings,
		resource: newResource(opts.Settings.ServiceName, opts.Version, opts.Settings.ResourceAttributes),
		client:   &otlpClient{http: httpClient, headers: opts.Settings.Headers, gzip: opts.Settings.Gzip, timeout: opts.Settings.Timeout, userAgent: userAgent},
		metrics:  newMetricSet(time.Now()),
		live:     liveSessions{sessions: map[int]func() int{}},
	}
	t.registerObservedInstruments()
	if opts.Settings.TracesEndpoint != "" {
		queueSize := orDefault(opts.SpanQueueSize, defaultSpanQueueSize)
		batchSize := orDefault(opts.SpanBatchSize, defaultSpanBatchSize)
		interval := opts.SpanBatchInterval
		if interval <= 0 {
			interval = defaultSpanBatchInterval
		}
		t.health = &exportHealth{signal: "traces"}
		t.spans = newSpanBatcher(queueSize, batchSize, interval, t.health, t.exportSpans)
	}
	if opts.Settings.MetricsEndpoint != "" {
		interval := opts.Settings.MetricExportInterval
		if interval <= 0 {
			interval = defaultMetricExportInterval
		}
		t.metricsExporter = newMetricExporter(interval, &exportHealth{signal: "metrics"}, t.exportMetrics)
	}
	return t
}

// defaultMetricExportInterval is the OpenTelemetry SDK default for
// OTEL_METRIC_EXPORT_INTERVAL, used when the settings leave it zero.
const defaultMetricExportInterval = 60 * time.Second

func orDefault(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}

// Tracing reports whether spans are exported, which is when agent runs
// should carry trace context at all.
func (t *Telemetry) Tracing() bool { return t != nil && t.spans != nil }

func (t *Telemetry) exportSpans(ctx context.Context, spans []*span) error {
	body, err := encodeTraces(t.resource, spans)
	if err != nil {
		return err
	}
	return t.client.post(ctx, t.settings.TracesEndpoint, body)
}

func (t *Telemetry) exportMetrics(ctx context.Context) error {
	body, err := encodeMetrics(t.resource, t.metrics.collect(time.Now()))
	if err != nil {
		return err
	}
	return t.client.post(ctx, t.settings.MetricsEndpoint, body)
}

// record hands a finished span to the batcher. Spans under a remote parent
// that was not sampled are propagated but never exported.
func (t *Telemetry) record(finished *span, sampled bool) {
	if !t.Tracing() || !sampled {
		return
	}
	t.spans.enqueue(finished)
}

// Shutdown flushes queued spans and the final metrics, waiting at most until
// ctx ends. Telemetry still queued at the deadline is dropped: exit is never
// held hostage by a collector.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	if t == nil {
		return nil
	}
	var errs []error
	if t.spans != nil {
		errs = append(errs, t.spans.shutdown(ctx))
	}
	if t.metricsExporter != nil {
		errs = append(errs, t.metricsExporter.shutdown(ctx))
	}
	return errors.Join(errs...)
}
