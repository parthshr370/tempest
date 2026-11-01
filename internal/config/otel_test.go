package config

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestOTelStandardVariablesResolve(t *testing.T) {
	w := newLayerWorkspace(t)
	w.write(w.user, `{"otel": {"service_name": "from-settings", "timeout_ms": 2500, "headers": {"x-team": "settings"}}}`)
	w.env["OTEL_EXPORTER_OTLP_ENDPOINT"] = "http://collector:4318/"
	w.env["OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"] = "https://metrics.example/custom"
	w.env["OTEL_EXPORTER_OTLP_HEADERS"] = "api-key=s%3Dcret, x-team=env"
	w.env["OTEL_SERVICE_NAME"] = "from-env"
	w.env["OTEL_RESOURCE_ATTRIBUTES"] = "deployment.environment=ci,team=core"
	w.env["OTEL_EXPORTER_OTLP_COMPRESSION"] = "gzip"
	w.env["OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT"] = "true"
	w.env["OTEL_METRIC_EXPORT_INTERVAL"] = "15000"
	w.env["OTEL_EXPORTER_OTLP_PROTOCOL"] = "http/json"

	got := w.mustResolve().OTel()
	want := OTelSettings{
		TracesEndpoint:        "http://collector:4318/v1/traces",
		MetricsEndpoint:       "https://metrics.example/custom",
		Headers:               map[string]string{"api-key": "s=cret", "x-team": "env"},
		Gzip:                  true,
		Timeout:               2500 * time.Millisecond,
		ServiceName:           "from-env",
		ResourceAttributes:    map[string]string{"deployment.environment": "ci", "team": "core"},
		CaptureMessageContent: true,
		MetricExportInterval:  15 * time.Second,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("OTel() = %+v\nwant     %+v", got, want)
	}
}

func TestOTelDefaultsAndDisabled(t *testing.T) {
	w := newLayerWorkspace(t)
	got := w.mustResolve().OTel()
	if got.TracesEndpoint != "" || got.MetricsEndpoint != "" || got.Disabled || got.CaptureMessageContent {
		t.Fatalf("unset otel exported something: %+v", got)
	}
	if got.ServiceName != "tempest" || got.Timeout != 10*time.Second || got.MetricExportInterval != time.Minute {
		t.Fatalf("defaults = %+v", got)
	}
	w.env["OTEL_SDK_DISABLED"] = "true"
	if !w.mustResolve().OTel().Disabled {
		t.Fatal("OTEL_SDK_DISABLED=true did not disable telemetry")
	}
}

func TestOTelProtocolOtherThanJSONFailsStartup(t *testing.T) {
	for _, variable := range []string{"OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL"} {
		t.Run(variable, func(t *testing.T) {
			w := newLayerWorkspace(t)
			w.env[variable] = "grpc"
			_, err := w.resolve()
			if err == nil || !strings.Contains(err.Error(), `"grpc" is not supported`) || !strings.Contains(err.Error(), "OTEL_EXPORTER_OTLP_PROTOCOL=http/json") {
				t.Fatalf("err = %v, want an error naming http/json", err)
			}
		})
	}
}

func TestOTelInvalidValuesFail(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"relative endpoint": {"OTEL_EXPORTER_OTLP_ENDPOINT": "collector:4318"},
		"compression":       {"OTEL_EXPORTER_OTLP_COMPRESSION": "zstd"},
		"timeout":           {"OTEL_EXPORTER_OTLP_TIMEOUT": "0"},
		"headers":           {"OTEL_EXPORTER_OTLP_HEADERS": "no-equals-sign"},
		"boolean":           {"OTEL_SDK_DISABLED": "maybe"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newLayerWorkspace(t)
			for key, value := range env {
				w.env[key] = value
			}
			if _, err := w.resolve(); err == nil || !strings.Contains(err.Error(), "otel.") {
				t.Fatalf("err = %v, want an otel.* error", err)
			}
		})
	}
}

func TestOTelProjectSettingsNeedProviderTrust(t *testing.T) {
	w := newLayerWorkspace(t)
	w.write(w.projectPath(), `{"otel": {"endpoint": "https://repo.example", "headers": {"api-key": "x"}, "capture_message_content": true}}`)
	resolved := w.mustResolve()
	if got := resolved.OTel(); got.TracesEndpoint != "" || got.CaptureMessageContent || len(got.Headers) != 0 {
		t.Fatalf("untrusted project configured telemetry: %+v", got)
	}
	if !strings.Contains(strings.Join(resolved.Warnings(), "\n"), "otel") {
		t.Fatalf("warnings %v do not name the dropped otel key", resolved.Warnings())
	}

	w.flags.TrustProjectProviders = BoolInput{Value: true, Set: true}
	if got := w.mustResolve().OTel(); got.TracesEndpoint != "https://repo.example/v1/traces" {
		t.Fatalf("trusted project endpoint = %q", got.TracesEndpoint)
	}
}

func TestOTelHeadersRedactedInConfigShow(t *testing.T) {
	w := newLayerWorkspace(t)
	w.env["OTEL_EXPORTER_OTLP_HEADERS"] = "x-honeycomb-team=abc123"
	setting := settingFor(t, w.mustResolve(), "otel.headers.x-honeycomb-team")
	if strings.Contains(string(setting.Value), "abc123") {
		t.Fatalf("config show leaked an OTLP header: %s", setting.Value)
	}
	if setting.Source != "OTEL_EXPORTER_OTLP_HEADERS" {
		t.Fatalf("source = %q", setting.Source)
	}
}

// otelExports reports whether settings send anything anywhere or capture
// content.
func otelExports(settings OTelSettings) bool {
	return settings.TracesEndpoint != "" || settings.MetricsEndpoint != "" || settings.CaptureMessageContent || len(settings.Headers) != 0
}

// A repository cannot configure export through a spelling encoding/json would
// fold onto otel or one of its members, trusted or not: the schema drops the
// spelling before the trust gate and decoding see it, and config show never
// lists it.
func TestOTelCaseVariantsNeverConfigureExport(t *testing.T) {
	const marker = "P3_HEADER_SECRET_6134"
	for _, project := range []string{
		`{"OTel":{"endpoint":"http://127.0.0.1:4318","capture_message_content":true}}`,
		`{"OTEL":{"ENDPOINT":"http://127.0.0.1:4318","Headers":{"x-team":"` + marker + `"}}}`,
		`{"otel":{"Endpoint":"http://127.0.0.1:4318","Traces_Endpoint":"http://127.0.0.1:4318/v1/traces","CAPTURE_MESSAGE_CONTENT":true,"Headers":{"x-team":"` + marker + `"}}}`,
		`{"profiles":{"review":{"OTel":{"endpoint":"http://127.0.0.1:4318","capture_message_content":true}}}}`,
	} {
		for _, trusted := range []bool{false, true} {
			t.Run(fmt.Sprintf("trusted=%v/%s", trusted, project), func(t *testing.T) {
				w := newLayerWorkspace(t)
				w.flags.Profile = StringInput{Value: "review", Set: true}
				w.write(w.user, `{"profiles":{"review":{}}}`)
				w.flags.TrustProjectProviders = BoolInput{Value: trusted, Set: true}
				w.flags.TrustProjectHooks = BoolInput{Value: trusted, Set: true}
				w.write(w.projectPath(), project)
				resolved := w.mustResolve()
				if got := resolved.OTel(); otelExports(got) {
					t.Fatalf("a case variant configured telemetry: %+v", got)
				}
				for _, setting := range resolved.Settings() {
					// A trusted project's otel object whose members were all
					// unknown spellings is left empty: {} and nothing else.
					top, _, _ := strings.Cut(setting.Key, ".")
					if strings.Contains(string(setting.Value), marker) || strings.EqualFold(top, "otel") && (setting.Key != "otel" || string(setting.Value) != "{}") {
						t.Fatalf("config show lists %+v", setting)
					}
				}
				if !strings.Contains(strings.Join(resolved.Warnings(), "\n"), "unknown") {
					t.Fatalf("no unknown-setting warning: %q", resolved.Warnings())
				}
			})
		}
	}
}

// A header set through any spelling of otel.headers either applies and is
// masked by config show, or is unknown and dropped; its value never prints.
func TestOTelHeaderSpellingsNeverShowTheValue(t *testing.T) {
	const marker = "P3_HEADER_SECRET_6134"
	for _, override := range []string{
		`otel.Headers={"x-collector-team":"` + marker + `"}`,
		`otel.HEADERS.x-collector-team=` + marker,
		`OTel.headers={"x-collector-team":"` + marker + `"}`,
		`otel.headers={"x-collector-team":"` + marker + `"}`,
		`otel.headers.x-collector-team=` + marker,
	} {
		t.Run(override, func(t *testing.T) {
			w := newLayerWorkspace(t)
			w.flags.Overrides = StringsInput{Set: true, Values: []string{override}}
			resolved := w.mustResolve()
			exact := strings.HasPrefix(override, "otel.headers")
			if got := resolved.OTel().Headers["x-collector-team"]; (got == marker) != exact {
				t.Fatalf("header = %q; the exact spelling alone applies", got)
			}
			for _, setting := range resolved.Settings() {
				if strings.Contains(string(setting.Value), marker) {
					t.Fatalf("config show printed the header value: %+v", setting)
				}
			}
		})
	}
}
