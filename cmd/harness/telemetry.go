package main

import (
	"context"
	"log/slog"
	"time"

	"go.harness.dev/harness/internal/build"
	"go.harness.dev/harness/internal/config"
	"go.harness.dev/harness/internal/telemetry"
)

// telemetryShutdownTimeout bounds the final span and metric flush at exit so
// an unreachable collector cannot hold the process open.
const telemetryShutdownTimeout = 5 * time.Second

// startTelemetry starts OTLP export for this process. A one-shot run without
// an OTLP endpoint has nowhere to send telemetry and gets none; serve keeps
// metrics for its /metrics endpoint even without one.
func startTelemetry(cfg *config.ResolvedConfig, scraped bool) *telemetry.Telemetry {
	settings := cfg.OTel()
	if !scraped && settings.TracesEndpoint == "" && settings.MetricsEndpoint == "" {
		return nil
	}
	return telemetry.New(telemetry.Options{Settings: settings, Version: build.Current().Version})
}

// stopTelemetry flushes what is queued, within telemetryShutdownTimeout.
func stopTelemetry(tel *telemetry.Telemetry) {
	ctx, cancel := context.WithTimeout(context.Background(), telemetryShutdownTimeout)
	defer cancel()
	if err := tel.Shutdown(ctx); err != nil {
		slog.Warn("telemetry flush incomplete at exit", "error", err)
	}
}
