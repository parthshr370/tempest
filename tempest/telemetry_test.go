package tempest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/config"
	"go.harness.dev/harness/internal/engine/stream"
	types "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/prompt"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/telemetry"
)

func scrapeGauge(t *testing.T, tel *telemetry.Telemetry, name string) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	tel.MetricsHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		if value, ok := strings.CutPrefix(line, name+" "); ok {
			return value
		}
	}
	t.Fatalf("%s missing from scrape:\n%s", name, recorder.Body.String())
	return ""
}

// TestSessionGaugesFollowOpenAndClose: an open session counts in
// tempest.sessions with its unfinished jobs in tempest.jobs, and closing it
// releases both.
func TestSessionGaugesFollowOpenAndClose(t *testing.T) {
	tel := telemetry.New(telemetry.Options{Settings: config.OTelSettings{ServiceName: "tempest"}})
	f := faux.New(faux.Options{})
	s, err := Open(context.Background(), Options{Cwd: t.TempDir(), SessionDir: t.TempDir(), New: &NewSession{PermissionMode: PermissionMode("bypass")}, Stack: &StackConfig{Model: f.Model(), StreamFn: f.StreamSimple, ContextFiles: []prompt.ContextFile{}, Telemetry: tel}})
	if err != nil {
		t.Fatal(err)
	}
	if sessions, jobs := scrapeGauge(t, tel, "tempest_sessions"), scrapeGauge(t, tel, "tempest_jobs"); sessions != "1" || jobs != "0" {
		t.Fatalf("open session: sessions %s, jobs %s", sessions, jobs)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sessions := scrapeGauge(t, tel, "tempest_sessions"); sessions != "0" {
		t.Fatalf("closed session still counted: sessions %s", sessions)
	}
}

// exportedSpan is the part of an OTLP/JSON span the trace tests read.
type exportedSpan struct {
	TraceID      string `json:"traceId"`
	SpanID       string `json:"spanId"`
	ParentSpanID string `json:"parentSpanId"`
	Name         string `json:"name"`
}

// spanCollector is an OTLP/HTTP trace receiver that keeps every span.
type spanCollector struct {
	mu    sync.Mutex
	spans []exportedSpan
}

func (c *spanCollector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []exportedSpan `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, resource := range request.ResourceSpans {
		for _, scope := range resource.ScopeSpans {
			c.spans = append(c.spans, scope.Spans...)
		}
	}
}

// A real task tool call and a real background bash call, both launched
// through the session's job manager, run under their execute_tool spans:
// the child agent's invoke_agent span is a child of the task call, and the
// background command's TRACEPARENT names the bash call.
func TestTraceTaskChildAndBackgroundBashUnderTheirToolCalls(t *testing.T) {
	collector := &spanCollector{}
	server := httptest.NewServer(collector)
	defer server.Close()
	tel := telemetry.New(telemetry.Options{Settings: config.OTelSettings{TracesEndpoint: server.URL + "/v1/traces", Timeout: 5 * time.Second, ServiceName: "tempest"}})
	root := faux.New(faux.Options{})
	child := faux.New(faux.Options{})
	dispatch := func(ctx context.Context, model types.Model, input types.Context, options *types.SimpleStreamOptions) *stream.AssistantStream {
		if strings.Contains(input.SystemPrompt, "You are a delegated child.") {
			return child.StreamSimple(ctx, model, input, options)
		}
		return root.StreamSimple(ctx, model, input, options)
	}
	cwd := t.TempDir()
	traceFile := filepath.Join(cwd, "traceparent.txt")
	bash, err := json.Marshal(map[string]any{"command": `printf %s "$TRACEPARENT" > ` + traceFile, "async": true})
	if err != nil {
		t.Fatal(err)
	}
	child.SetResponses(faux.Respond(faux.Text("child done")))
	root.SetResponses(
		faux.Respond(faux.ToolCall("task", json.RawMessage(`{"prompt":"work","subagent_type":"explore"}`), "root_task"), faux.ToolCall("bash", bash, "root_bash")),
		faux.Respond(faux.Text("waiting")), faux.Respond(faux.Text("root done")), faux.Respond(faux.Text("root done")), faux.Respond(faux.Text("root done")),
	)
	owner, err := Open(t.Context(), Options{SessionDir: t.TempDir(), Cwd: cwd, New: &NewSession{PermissionMode: permission.ModeBypass}, Stack: &StackConfig{Model: root.Model(), StreamFn: dispatch, EnableTask: true, ContextFiles: []prompt.ContextFile{}, Telemetry: tel}})
	if err != nil {
		t.Fatal(err)
	}
	run, err := owner.Begin(t.Context(), Prompt{Text: "delegate"})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := run.Wait(); err != nil || result.StopReason != agent.StopCompleted {
		t.Fatalf("run=%+v %v", result, err)
	}
	if err := owner.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	collector.mu.Lock()
	spans := slices.Clone(collector.spans)
	collector.mu.Unlock()
	byName := map[string][]exportedSpan{}
	for _, span := range spans {
		byName[span.Name] = append(byName[span.Name], span)
	}
	if len(byName["execute_tool task"]) != 1 || len(byName["execute_tool bash"]) != 1 || len(byName["invoke_agent explore"]) != 1 {
		t.Fatalf("spans = %+v", spans)
	}
	taskCall, bashCall, childRun := byName["execute_tool task"][0], byName["execute_tool bash"][0], byName["invoke_agent explore"][0]
	if childRun.ParentSpanID != taskCall.SpanID || childRun.TraceID != taskCall.TraceID {
		t.Fatalf("child run %+v is not under the task call %+v", childRun, taskCall)
	}
	written, err := os.ReadFile(traceFile)
	if err != nil {
		t.Fatal(err)
	}
	if want := "00-" + bashCall.TraceID + "-" + bashCall.SpanID + "-01"; string(written) != want {
		t.Fatalf("background TRACEPARENT = %q, want the bash call's %q", written, want)
	}
}
