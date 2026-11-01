package telemetry

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/config"
	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/hooks"
)

// goldenStart and goldenAt fix the metric timestamps so the encoding is
// byte-stable.
var (
	goldenStart = time.Unix(1_700_000_000, 0)
	goldenAt    = goldenStart.Add(90 * time.Second)
)

// goldenTelemetry builds a Telemetry with no exporters and a fixed start,
// then records one point of each instrument kind.
func goldenTelemetry() *Telemetry {
	tel := &Telemetry{
		resource: newResource("tempest", "1.2.3", map[string]string{"deployment.environment": "test"}),
		metrics:  newMetricSet(goldenStart),
		live:     liveSessions{sessions: map[int]func() int{}},
	}
	tel.registerObservedInstruments()
	const backgroundJobs = 2
	tel.TrackSession(func() int { return backgroundJobs })
	message := types.AssistantMessage{StopReason: types.StopStop, Usage: types.Usage{Input: 120, Output: 8, Cost: types.Cost{Total: 0.002}}}
	tel.recordRequest(testModel, "claude-test-20260101", message, 1500*time.Millisecond)
	tel.recordToolCall("echo", "success")
	return tel
}

func TestEncodeMetricsMatchesGolden(t *testing.T) {
	tel := goldenTelemetry()
	body, err := encodeMetrics(tel.resource, tel.metrics.collect(goldenAt))
	if err != nil {
		t.Fatal(err)
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, body, "", "  "); err != nil {
		t.Fatal(err)
	}
	indented.WriteByte('\n')
	path := filepath.Join("testdata", "metrics.golden.json")
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
		t.Fatalf("OTLP metrics JSON differs from %s:\n%s", path, indented.Bytes())
	}
}

// TestEncodeMetricsProto3Mapping checks the mapping rules the golden file
// encodes: 64-bit integers and timestamps as decimal strings, cumulative
// temporality as the enum integer, and bucket counts one longer than bounds.
func TestEncodeMetricsProto3Mapping(t *testing.T) {
	tel := goldenTelemetry()
	body, err := encodeMetrics(tel.resource, tel.metrics.collect(goldenAt))
	if err != nil {
		t.Fatal(err)
	}
	metrics := decodeMetrics(t, body)
	sessions := metrics["tempest.sessions"]
	if sessions.Gauge == nil || string(sessions.Gauge.DataPoints[0]["asInt"]) != `"1"` || sessions.Gauge.DataPoints[0]["startTimeUnixNano"] != nil {
		t.Fatalf("tempest.sessions = %+v", sessions)
	}
	if got := string(metrics["tempest.jobs"].Gauge.DataPoints[0]["asInt"]); got != `"2"` {
		t.Fatalf("tempest.jobs asInt = %s", got)
	}
	calls := metrics["tempest.tool.calls"]
	if calls.Sum == nil || string(calls.Sum.AggregationTemporality) != "2" || !calls.Sum.IsMonotonic || string(calls.Sum.DataPoints[0]["timeUnixNano"]) != `"1700000090000000000"` {
		t.Fatalf("tempest.tool.calls = %+v", calls)
	}
	tokens := metrics["gen_ai.client.token.usage"]
	if tokens.Histogram == nil || len(tokens.Histogram.DataPoints) != 2 {
		t.Fatalf("token usage = %+v", tokens)
	}
	var point struct {
		Count          string    `json:"count"`
		BucketCounts   []string  `json:"bucketCounts"`
		ExplicitBounds []float64 `json:"explicitBounds"`
	}
	raw, _ := json.Marshal(tokens.Histogram.DataPoints[0])
	if err := json.Unmarshal(raw, &point); err != nil {
		t.Fatal(err)
	}
	if point.Count != "1" || len(point.BucketCounts) != len(point.ExplicitBounds)+1 {
		t.Fatalf("histogram point = %+v", point)
	}
	if _, lost := metrics["tempest.telemetry.spans.lost"]; lost {
		t.Fatal("span loss reported without a trace exporter")
	}
}

type receivedMetric struct {
	Name  string `json:"name"`
	Gauge *struct {
		DataPoints []map[string]json.RawMessage `json:"dataPoints"`
	} `json:"gauge"`
	Sum *struct {
		DataPoints             []map[string]json.RawMessage `json:"dataPoints"`
		AggregationTemporality json.RawMessage              `json:"aggregationTemporality"`
		IsMonotonic            bool                         `json:"isMonotonic"`
	} `json:"sum"`
	Histogram *struct {
		DataPoints []map[string]json.RawMessage `json:"dataPoints"`
	} `json:"histogram"`
}

// decodeMetrics indexes one OTLP metrics request by metric name.
func decodeMetrics(t *testing.T, body []byte) map[string]receivedMetric {
	t.Helper()
	var request struct {
		ResourceMetrics []struct {
			ScopeMetrics []struct {
				Metrics []receivedMetric `json:"metrics"`
			} `json:"scopeMetrics"`
		} `json:"resourceMetrics"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatalf("decode OTLP metrics: %v\n%s", err, body)
	}
	byName := map[string]receivedMetric{}
	for _, resource := range request.ResourceMetrics {
		for _, scope := range resource.ScopeMetrics {
			for _, metric := range scope.Metrics {
				byName[metric.Name] = metric
			}
		}
	}
	return byName
}

// promSample is one parsed Prometheus sample line.
type promSample struct {
	name   string
	labels map[string]string
	value  float64
}

var (
	promMetricName = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	promLabelName  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	promTypes      = map[string]bool{"counter": true, "gauge": true, "histogram": true, "summary": true, "untyped": true}
)

// parsePrometheus is a strict reader for the text exposition format 0.0.4:
// HELP and TYPE lines precede their family's samples and appear once, names
// and labels use the allowed alphabets, label values are quoted and escaped,
// and every sample belongs to a declared family. It returns the samples and
// each family's type.
func parsePrometheus(t *testing.T, text string) ([]promSample, map[string]string) {
	t.Helper()
	families := map[string]string{}
	helped := map[string]bool{}
	var samples []promSample
	scanner := bufio.NewScanner(strings.NewReader(text))
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := scanner.Text()
		fail := func(format string, args ...any) {
			t.Helper()
			t.Fatalf("line %d %q: %s", lineNumber, line, fmt.Sprintf(format, args...))
		}
		if line == "" {
			continue
		}
		if comment, ok := strings.CutPrefix(line, "# "); ok {
			fields := strings.SplitN(comment, " ", 3)
			if len(fields) < 3 || !promMetricName.MatchString(fields[1]) {
				fail("malformed comment")
			}
			switch fields[0] {
			case "HELP":
				if helped[fields[1]] {
					fail("second HELP")
				}
				helped[fields[1]] = true
			case "TYPE":
				if _, seen := families[fields[1]]; seen || !promTypes[fields[2]] {
					fail("duplicate or unknown TYPE")
				}
				families[fields[1]] = fields[2]
			}
			continue
		}
		sample := parsePromSample(line, fail)
		if _, declared := families[promFamily(sample.name, families)]; !declared {
			fail("sample before its TYPE line")
		}
		samples = append(samples, sample)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return samples, families
}

func parsePromSample(line string, fail func(string, ...any)) promSample {
	sample := promSample{labels: map[string]string{}}
	rest := line
	end := strings.IndexAny(rest, "{ ")
	if end <= 0 {
		fail("no metric name")
	}
	sample.name = rest[:end]
	if !promMetricName.MatchString(sample.name) {
		fail("invalid metric name")
	}
	rest = rest[end:]
	if strings.HasPrefix(rest, "{") {
		rest = rest[1:]
		for !strings.HasPrefix(rest, "}") {
			name, after, ok := strings.Cut(rest, `="`)
			if !ok || !promLabelName.MatchString(name) {
				fail("invalid label name in %q", rest)
			}
			var value strings.Builder
			index := 0
			for ; index < len(after) && after[index] != '"'; index++ {
				if after[index] != '\\' {
					value.WriteByte(after[index])
					continue
				}
				index++
				switch {
				case index >= len(after):
					fail("dangling escape")
				case after[index] == 'n':
					value.WriteByte('\n')
				case after[index] == '\\' || after[index] == '"':
					value.WriteByte(after[index])
				default:
					fail("invalid escape")
				}
			}
			if index >= len(after) {
				fail("unterminated label value")
			}
			if _, duplicate := sample.labels[name]; duplicate {
				fail("duplicate label %s", name)
			}
			sample.labels[name] = value.String()
			rest = strings.TrimPrefix(after[index+1:], ",")
		}
		rest = rest[1:]
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 || len(fields) > 2 {
		fail("want a value and an optional timestamp")
	}
	value, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		fail("value: %v", err)
	}
	sample.value = value
	return sample
}

// promFamily maps a histogram's _bucket, _sum and _count samples to the
// family they belong to.
func promFamily(name string, families map[string]string) string {
	for _, suffix := range []string{"_bucket", "_sum", "_count"} {
		if base, ok := strings.CutSuffix(name, suffix); ok && families[base] == "histogram" {
			return base
		}
	}
	return name
}

func scrape(t *testing.T, tel *Telemetry) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	tel.MetricsHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != prometheusContentType {
		t.Fatalf("scrape = %d %q", recorder.Code, recorder.Header().Get("Content-Type"))
	}
	return recorder.Body.String()
}

func TestMetricsHandlerServesValidPrometheusText(t *testing.T) {
	tel := goldenTelemetry()
	tel.recordHook(&hooks.Report{Event: "PreToolUse", ExitCode: hookExitBlock})
	tel.recordPermission("echo", "allow", "rule")
	// A label value with every character the format escapes.
	tel.recordToolCall("quote\"back\\slash\nnewline", "error")
	samples, families := parsePrometheus(t, scrape(t, tel))

	wantTypes := map[string]string{
		"gen_ai_client_token_usage":                "histogram",
		"gen_ai_client_operation_duration_seconds": "histogram",
		"gen_ai_client_operation_cost_usd":         "histogram",
		"tempest_tool_calls_total":                 "counter",
		"tempest_hook_runs_total":                  "counter",
		"tempest_permission_decisions_total":       "counter",
		"tempest_sessions":                         "gauge",
		"tempest_jobs":                             "gauge",
	}
	for name, kind := range wantTypes {
		if families[name] != kind {
			t.Errorf("family %s has type %q, want %q", name, families[name], kind)
		}
	}
	values := map[string]float64{}
	for _, sample := range samples {
		switch {
		case sample.name == "tempest_hook_runs_total":
			if sample.labels["tempest_hook_outcome"] != "blocked" {
				t.Errorf("hook outcome = %v", sample.labels)
			}
		case sample.name == "tempest_tool_calls_total" && sample.labels["tempest_tool_outcome"] == "error":
			if sample.labels["gen_ai_tool_name"] != "quote\"back\\slash\nnewline" {
				t.Errorf("escaped label round-trip = %q", sample.labels["gen_ai_tool_name"])
			}
		}
		values[sample.name] = sample.value
	}
	if values["tempest_sessions"] != 1 || values["tempest_jobs"] != 2 {
		t.Fatalf("gauges = sessions %v, jobs %v", values["tempest_sessions"], values["tempest_jobs"])
	}
	assertCumulativeHistograms(t, samples, families)
}

// assertCumulativeHistograms checks that each histogram series has
// non-decreasing buckets ending in le="+Inf" equal to its _count.
func assertCumulativeHistograms(t *testing.T, samples []promSample, families map[string]string) {
	t.Helper()
	type seriesState struct {
		last     float64
		infinity float64
		sawInf   bool
	}
	states := map[string]*seriesState{}
	counts := map[string]float64{}
	key := func(sample promSample, family string) string {
		parts := []string{family}
		for name, value := range sample.labels {
			if name != "le" {
				parts = append(parts, name+"="+value)
			}
		}
		slices.Sort(parts[1:])
		return strings.Join(parts, ",")
	}
	for _, sample := range samples {
		family := promFamily(sample.name, families)
		if families[family] != "histogram" {
			continue
		}
		id := key(sample, family)
		switch {
		case strings.HasSuffix(sample.name, "_bucket"):
			state := states[id]
			if state == nil {
				state = &seriesState{}
				states[id] = state
			}
			if sample.value < state.last {
				t.Fatalf("%s buckets decrease", id)
			}
			state.last = sample.value
			if sample.labels["le"] == "+Inf" {
				state.sawInf, state.infinity = true, sample.value
			}
		case strings.HasSuffix(sample.name, "_count"):
			counts[id] = sample.value
		}
	}
	if len(states) == 0 {
		t.Fatal("no histogram series")
	}
	for id, state := range states {
		if !state.sawInf || state.infinity != counts[id] {
			t.Fatalf("%s: +Inf bucket %v, count %v", id, state.infinity, counts[id])
		}
	}
}

func TestMetricsHandlerRejectsWrites(t *testing.T) {
	recorder := httptest.NewRecorder()
	goldenTelemetry().MetricsHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST /metrics = %d", recorder.Code)
	}
}

func TestHookOutcomes(t *testing.T) {
	tel := goldenTelemetry()
	for _, report := range []hooks.Report{
		{Event: "Stop"},
		{Event: "Stop", ExitCode: hookExitBlock},
		{Event: "Stop", ExitCode: 1},
		{Event: "Stop", Error: "exec: not found"},
		{Event: "Stop", TimedOut: true, Error: "deadline"},
	} {
		tel.recordHook(&report)
	}
	outcomes := map[string]int64{}
	for _, entry := range tel.metrics.collect(goldenAt).instruments {
		if entry.name != "tempest.hook.runs" {
			continue
		}
		for _, point := range entry.series {
			outcomes[point.attributes[1].text] = point.value
		}
	}
	want := map[string]int64{"success": 1, "blocked": 1, "failed": 1, "error": 1, "timeout": 1}
	if fmt.Sprint(outcomes) != fmt.Sprint(want) {
		t.Fatalf("hook outcomes = %v, want %v", outcomes, want)
	}
}

// TestFauxRunExportsMetrics runs the acceptance faux run (one retry, one
// tool call) and reads the metrics the collector receives at shutdown.
func TestFauxRunExportsMetrics(t *testing.T) {
	c := newCollector(t)
	tel := New(Options{Settings: config.OTelSettings{
		MetricsEndpoint:      c.server.URL + "/v1/metrics",
		Timeout:              5 * time.Second,
		ServiceName:          "tempest",
		MetricExportInterval: time.Hour,
	}})
	release := tel.TrackSession(func() int { return 0 })
	defer release()
	provider := &scriptedProvider{attempts: []func(*stream.AssistantStream){unavailableAttempt, toolCallAttempt("echo", `{}`), textAttempt("done")}}
	agt := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{Model: testModel, Tools: []agent.AgentTool{echoTool(nil)}}, StreamFn: retrying(t, provider)})
	defer tel.Attach(agt, AgentInfo{})()
	if result, err := agt.Prompt(context.Background(), types.UserMessage{Content: types.StringContent("say done")}); err != nil || result.StopReason != agent.StopCompleted {
		t.Fatalf("run = %+v, %v", result, err)
	}
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	requests := c.snapshot()
	if len(requests) != 1 || requests[0].path != "/v1/metrics" {
		t.Fatalf("collector received %d requests", len(requests))
	}
	metrics := decodeMetrics(t, requests[0].body)
	for _, name := range []string{
		"gen_ai.client.token.usage", "gen_ai.client.operation.duration", "gen_ai.client.operation.time_to_first_chunk",
		"gen_ai.client.operation.cost", "tempest.agent.runs", "tempest.tool.calls", "tempest.model.failed_attempts", "tempest.sessions", "tempest.jobs",
	} {
		if _, ok := metrics[name]; !ok {
			t.Errorf("metric %s missing", name)
		}
	}
	retries := metrics["tempest.model.failed_attempts"].Sum
	if retries == nil || len(retries.DataPoints) != 1 || string(retries.DataPoints[0]["asInt"]) != `"1"` || !strings.Contains(string(retries.DataPoints[0]["attributes"]), `"retry"`) {
		t.Fatalf("failed attempts = %+v", metrics["tempest.model.failed_attempts"])
	}
	if requests := metrics["gen_ai.client.operation.duration"].Histogram; requests == nil || len(requests.DataPoints) != 1 || string(requests.DataPoints[0]["count"]) != `"2"` {
		t.Fatalf("duration = %+v", requests)
	}
}

// blackhole accepts OTLP connections and never answers, the worst collector
// outage: no refusal for the client to fail fast on.
func blackhole(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, conn)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range held {
			_ = conn.Close()
		}
	})
	return "http://" + listener.Addr().String()
}

// TestBlackholeCollectorDoesNotSlowTurns runs the faux run with traces and
// metrics pointed at a collector that never answers. Exports run every
// millisecond and wait up to a minute, so any export on the turn loop would
// blow the per-turn bound by orders of magnitude; shutdown gives up at its
// own deadline.
func TestBlackholeCollectorDoesNotSlowTurns(t *testing.T) {
	const (
		exportTimeout    = time.Minute
		turnBound        = time.Second
		shutdownDeadline = 100 * time.Millisecond
		shutdownSlack    = time.Second
		tinyQueue        = 2
	)
	endpoint := blackhole(t)
	tel := New(Options{
		Settings: config.OTelSettings{
			TracesEndpoint:       endpoint + "/v1/traces",
			MetricsEndpoint:      endpoint + "/v1/metrics",
			Timeout:              exportTimeout,
			ServiceName:          "tempest",
			MetricExportInterval: time.Millisecond,
		},
		SpanQueueSize:     tinyQueue,
		SpanBatchSize:     1,
		SpanBatchInterval: time.Millisecond,
	})
	provider := &scriptedProvider{attempts: []func(*stream.AssistantStream){unavailableAttempt, toolCallAttempt("echo", `{}`), textAttempt("done")}}
	agt := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{Model: testModel, Tools: []agent.AgentTool{echoTool(nil)}}, StreamFn: retrying(t, provider), Trace: tel.Tracing()})
	defer tel.Attach(agt, AgentInfo{})()
	var turnStart time.Time
	var slowest time.Duration
	turns := 0
	agt.Subscribe(func(_ context.Context, event agent.AgentEvent) error {
		switch event.Type {
		case agent.EventTurnStart:
			turnStart = time.Now()
		case agent.EventTurnEnd:
			turns++
			slowest = max(slowest, time.Since(turnStart))
		}
		return nil
	})
	if result, err := agt.Prompt(context.Background(), types.UserMessage{Content: types.StringContent("say done")}); err != nil || result.StopReason != agent.StopCompleted {
		t.Fatalf("run = %+v, %v", result, err)
	}
	if turns != 2 || slowest > turnBound {
		t.Fatalf("%d turns, slowest %v; want 2 turns each under %v", turns, slowest, turnBound)
	}

	ctx, cancel := context.WithTimeout(context.Background(), shutdownDeadline)
	defer cancel()
	began := time.Now()
	err := tel.Shutdown(ctx)
	if elapsed := time.Since(began); elapsed > shutdownDeadline+shutdownSlack {
		t.Fatalf("shutdown took %v past its %v deadline", elapsed, shutdownDeadline)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown against a blackhole = %v, want the deadline error", err)
	}
}
