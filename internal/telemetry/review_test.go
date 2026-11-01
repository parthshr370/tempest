package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/config"
	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/retry"
)

// cardinalityLimit is the OpenTelemetry SDK's default per-instrument limit.
const cardinalityLimit = 2000

// collectedSeries returns the series one instrument holds now.
func collectedSeries(t *testing.T, tel *Telemetry, name string) []series {
	t.Helper()
	for _, inst := range tel.metrics.collect(time.Now()).instruments {
		if inst.name == name {
			return inst.series
		}
	}
	t.Fatalf("no instrument %s", name)
	return nil
}

// seriesWith returns the series whose attributes include key=value.
func seriesWith(all []series, key, value string) (series, bool) {
	for _, entry := range all {
		for _, attr := range entry.attributes {
			if attr.key == key && attr.text == value {
				return entry, true
			}
		}
	}
	return series{}, false
}

// A long-lived process that sees ever new tool names keeps at most the
// cardinality limit of series per instrument; the rest aggregate into one
// overflow series, and the totals stay right.
func TestMetricSeriesAreBounded(t *testing.T) {
	tel := New(Options{Settings: config.OTelSettings{ServiceName: "tempest"}})
	const distinct = cardinalityLimit + 500
	for i := range distinct {
		tel.recordToolCall(fmt.Sprintf("invented-tool-%d", i), "error")
		tel.recordFirstChunk(types.Model{Provider: "p", ID: fmt.Sprintf("model-%d", i)}, time.Second)
	}
	calls := collectedSeries(t, tel, "tempest.tool.calls")
	if len(calls) != cardinalityLimit+1 {
		t.Fatalf("tool call series = %d, want %d plus one overflow series", len(calls), cardinalityLimit)
	}
	overflow, ok := seriesWith(calls, "otel.metric.overflow", "true")
	if !ok || len(overflow.attributes) != 1 || overflow.value != distinct-cardinalityLimit {
		t.Fatalf("overflow series = %+v, want %d calls", overflow, distinct-cardinalityLimit)
	}
	var total int64
	for _, entry := range calls {
		total += entry.value
	}
	if total != distinct {
		t.Fatalf("tool calls total %d, want %d", total, distinct)
	}
	if chunks := collectedSeries(t, tel, "gen_ai.client.operation.time_to_first_chunk"); len(chunks) != cardinalityLimit+1 {
		t.Fatalf("histogram series = %d", len(chunks))
	}
	samples, families := parsePrometheus(t, scrape(t, tel))
	assertCumulativeHistograms(t, samples, families)
	found := false
	for _, sample := range samples {
		found = found || strings.HasPrefix(sample.name, "tempest_tool_calls") && sample.labels["otel_metric_overflow"] == "true"
	}
	if !found {
		t.Fatal("the scrape does not label the overflow series")
	}
}

// Captured content keeps at most the capture limit of text, never pins the
// history it was copied from, and always encodes to valid JSON.
func TestCapturedContentIsBoundedValidJSON(t *testing.T) {
	huge := strings.Repeat("é", maxCapturedContentBytes)
	hugeArguments := json.RawMessage(`{"text":"` + strings.Repeat("a", 2*maxCapturedContentBytes) + `"}`)
	tiny := make([]types.Message, 0, 100_000)
	for range cap(tiny) {
		tiny = append(tiny, types.UserMessage{Content: types.StringContent("hi")})
	}
	call := types.ContentBlock{Type: types.BlockToolCall, ID: "call-1", Name: "write", Arguments: hugeArguments}
	captures := map[string]func() string{
		"input text":       inputMessages([]types.Message{types.UserMessage{Content: types.StringContent(huge)}}),
		"input arguments":  inputMessages([]types.Message{types.AssistantMessage{Content: []types.ContentBlock{call}, StopReason: types.StopToolUse}}),
		"tool arguments":   func() string { return toolArguments(hugeArguments) },
		"many messages":    inputMessages(tiny),
		"output":           outputMessages(types.AssistantMessage{Content: []types.ContentBlock{types.NewText(huge)}, StopReason: types.StopStop}),
		"system":           systemInstructions(huge),
		"short is unmoved": inputMessages([]types.Message{types.UserMessage{Content: types.StringContent("say hello")}}),
	}
	// The structure the budget charges per part roughly matches its JSON, so
	// an encoded capture stays near the limit.
	const slack = 1024
	for name, capture := range captures {
		encoded := capture()
		if !json.Valid([]byte(encoded)) {
			t.Errorf("%s: captured content is not valid JSON: ...%s", name, encoded[max(0, len(encoded)-80):])
		}
		if len(encoded) > maxCapturedContentBytes+slack {
			t.Errorf("%s: captured %d bytes, limit %d", name, len(encoded), maxCapturedContentBytes)
		}
		if truncated := strings.Contains(encoded, truncatedMarker); truncated == (name == "short is unmoved") {
			t.Errorf("%s: truncation marker present = %v", name, truncated)
		}
	}

	// The capture copies what it keeps: the original text can be freed while
	// the span waits for export.
	pinned := strings.Repeat("x", 4*maxCapturedContentBytes)
	freed := make(chan struct{})
	runtime.AddCleanup(unsafe.StringData(pinned), func(chan struct{}) { close(freed) }, freed)
	capture := inputMessages([]types.Message{types.UserMessage{Content: types.StringContent(pinned)}})
	pinned = ""
	waitFreed(t, freed, "the captured history")
	if !strings.Contains(capture(), truncatedMarker) {
		t.Fatal("capture lost its content")
	}
}

func waitFreed(t *testing.T, freed <-chan struct{}, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		select {
		case <-freed:
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatalf("%s is still reachable", what)
}

// The batch worker reuses its batch array; exported spans must not stay
// reachable through it until the next batch overwrites them.
func TestBatcherReleasesExportedSpans(t *testing.T) {
	exported := make(chan struct{}, 1)
	batcher := newSpanBatcher(4, 1, time.Hour, &exportHealth{signal: "traces"}, func(context.Context, []*span) error {
		exported <- struct{}{}
		return nil
	})
	defer func() {
		if err := batcher.shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	finished := testSpan("exported")
	freed := make(chan struct{})
	runtime.AddCleanup(finished, func(chan struct{}) { close(freed) }, freed)
	batcher.enqueue(finished)
	finished = nil
	<-exported
	waitFreed(t, freed, "an exported span")
}

// A transport error names the collector without the endpoint's userinfo or
// query, where collectors take credentials.
func TestExportErrorsRedactTheEndpoint(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	const secret = "P3_QUERY_SECRET_6349"
	client := &otlpClient{http: http.DefaultClient, timeout: 5 * time.Second, userAgent: "test"}
	err = client.post(context.Background(), "http://user:"+secret+"@"+address+"/v1/traces?sig="+secret, []byte("{}"))
	if err == nil {
		t.Fatal("export to a closed port succeeded")
	}
	if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), address+"/v1/traces") {
		t.Fatalf("export error = %q", err)
	}
}

// A collector that redirects to another origin gets neither the payload nor
// the configured headers there; a redirect within its origin is followed.
func TestCollectorRedirectsStayInTheEndpointOrigin(t *testing.T) {
	const secret = "P3_REDIRECT_SECRET_2728"
	var mu sync.Mutex
	var received []string
	record := func(where string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			mu.Lock()
			received = append(received, where+" "+r.Header.Get("x-collector-key"))
			mu.Unlock()
		}
	}
	other := httptest.NewServer(record("other origin"))
	defer other.Close()
	otherOrigin := strings.Replace(other.URL, "127.0.0.1", "localhost", 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/cross/v1/traces", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, otherOrigin+"/v1/traces", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/same/v1/traces", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/moved/v1/traces", http.StatusPermanentRedirect)
	})
	mux.HandleFunc("/moved/v1/traces", record("same origin"))
	first := httptest.NewServer(mux)
	defer first.Close()

	for _, path := range []string{"/cross", "/same"} {
		tel := New(Options{Settings: config.OTelSettings{TracesEndpoint: first.URL + path + "/v1/traces", Headers: map[string]string{"x-collector-key": secret}, Timeout: 5 * time.Second, ServiceName: "tempest"}})
		tel.record(testSpan("one"), true)
		if err := tel.Shutdown(context.Background()); (err != nil) != (path == "/cross") {
			t.Fatalf("%s: shutdown = %v", path, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 || received[0] != "same origin "+secret {
		t.Fatalf("received = %q", received)
	}
}

// partialFailedAttempt streams some output, reports usage, then fails with a
// retryable error, as a connection reset mid-response does.
func partialFailedAttempt(events *stream.AssistantStream) {
	events.Push(types.StreamEvent{Type: types.EvStart})
	events.Push(types.StreamEvent{Type: types.EvTextStart, ContentIndex: 0})
	events.Push(types.StreamEvent{Type: types.EvTextDelta, ContentIndex: 0, Delta: "partial"})
	events.Push(types.StreamEvent{Type: types.EvError, Reason: types.StopError, Err: &types.AssistantMessage{
		StopReason:   types.StopError,
		ErrorCode:    string(retry.CodeStreamInterrupted),
		ErrorMessage: "connection reset by peer",
		Usage:        types.Usage{Input: 50, CacheRead: 10, Output: 20, Cost: types.Cost{Total: 0.01}},
	}})
}

// Token and cost metrics follow GenAI usage semantics: input counts cached
// input, and a discarded attempt's reported usage and cost are measured as
// run accounting counts them.
func TestTokenMetricsCountCachedInputAndDiscardedAttempts(t *testing.T) {
	tel := New(Options{Settings: config.OTelSettings{ServiceName: "tempest"}})
	provider := &scriptedProvider{attempts: []func(*stream.AssistantStream){partialFailedAttempt, toolCallAttempt("echo", `{}`), textAttempt("done")}}
	agt := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{Model: testModel, Tools: []agent.AgentTool{echoTool(nil)}}, StreamFn: retrying(t, provider)})
	defer tel.Attach(agt, AgentInfo{})()
	result, err := agt.Prompt(context.Background(), types.UserMessage{Content: types.StringContent("say done")})
	if err != nil || result.StopReason != agent.StopCompleted {
		t.Fatalf("run = %+v, %v", result, err)
	}
	// totals sums the count and sum of every series with key=value ("" for all).
	totals := func(all []series, key, value string) (count uint64, sum float64) {
		for _, entry := range all {
			if _, ok := seriesWith([]series{entry}, key, value); ok || key == "" {
				count += entry.count
				sum += entry.sum
			}
		}
		return count, sum
	}
	tokens := collectedSeries(t, tel, "gen_ai.client.token.usage")
	inputCount, inputSum := totals(tokens, "gen_ai.token.type", "input")
	_, outputSum := totals(tokens, "gen_ai.token.type", "output")
	// Attempts: discarded 50+10 in / 20 out, tool call 120+30 / 8, text 140 / 4.
	if inputCount != 3 || inputSum != 350 || outputSum != 32 {
		t.Fatalf("token usage: %d input points summing to %v, output %v", inputCount, inputSum, outputSum)
	}
	wantCost := result.Usage.Cost.Total
	costCount, costSum := totals(collectedSeries(t, tel, "gen_ai.client.operation.cost"), "", "")
	if costCount != 3 || math.Abs(costSum-wantCost) > 1e-9 {
		t.Fatalf("cost: %d points summing to %v, want 3 summing to the run's %v", costCount, costSum, wantCost)
	}
}

// Time to first chunk runs from the attempt's send to its first output, not
// to an empty block start or a tool call start carrying only its ID. A
// retried or backup attempt's send time is the one its model_attempt event
// carries, however late that event, or the switch before it, is handled.
func TestTimeToFirstChunkMeasuresOutputFromTheRealSend(t *testing.T) {
	tel := New(Options{Settings: config.OTelSettings{ServiceName: "tempest"}})
	agt := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{Model: testModel}})
	listener := &agentListener{telemetry: tel, agent: agt, info: AgentInfo{Name: defaultAgentName}}
	ctx := context.Background()
	base := time.Unix(1_700_000_000, 0)
	at := func(offset time.Duration) time.Time { return base.Add(offset) }
	update := func(offset time.Duration, event types.StreamEvent) agent.AgentEvent {
		return agent.AgentEvent{Type: agent.EventMessageUpdate, At: at(offset), Message: types.AssistantMessage{}, AssistantMessageEvent: &event}
	}
	for _, event := range []agent.AgentEvent{
		{Type: agent.EventAgentStart, At: at(0)},
		{Type: agent.EventModelRequest, At: at(0), Request: &agent.ModelRequest{Model: testModel}},
		{Type: agent.EventMessageStart, At: at(time.Millisecond), Message: types.AssistantMessage{}},
		update(100*time.Millisecond, types.StreamEvent{Type: types.EvTextStart}),
		update(200*time.Millisecond, types.StreamEvent{Type: types.EvTextDelta}),
		update(300*time.Millisecond, types.StreamEvent{Type: types.EvToolCallStart, ToolCall: &types.ContentBlock{Type: types.BlockToolCall, ID: "call-early"}}),
		update(700*time.Millisecond, types.StreamEvent{Type: types.EvTextDelta, Delta: "hi"}),
		// The attempt fails at 1s and is sent again at 3s; the loop only
		// handles the retry event at 10s.
		{Type: agent.EventModelRetry, At: at(10 * time.Second), FailedAt: at(time.Second), RetryDelay: time.Second, Attempt: 1, Message: types.AssistantMessage{StopReason: types.StopError}},
		{Type: agent.EventMessageEnd, At: at(10 * time.Second), Message: types.AssistantMessage{StopReason: types.StopError}},
		{Type: agent.EventModelAttempt, At: at(10 * time.Second), SentAt: at(3 * time.Second), Attempt: 2},
		{Type: agent.EventMessageStart, At: at(10500 * time.Millisecond), Message: types.AssistantMessage{}},
		update(10500*time.Millisecond, types.StreamEvent{Type: types.EvTextStart}),
		update(10500*time.Millisecond, types.StreamEvent{Type: types.EvToolCallStart, ToolCall: &types.ContentBlock{Type: types.BlockToolCall, ID: "call", Name: "read"}}),
		// That attempt fails at 11s too; the backup it switches to at 12s is
		// sent only at 14s, once the switch was admitted, and answers at 14.25s.
		{Type: agent.EventModelRetry, At: at(11 * time.Second), FailedAt: at(11 * time.Second), Attempt: 2, Fallback: &types.ModelFallback{From: testModel, To: testModel}, Message: types.AssistantMessage{StopReason: types.StopError}},
		{Type: agent.EventMessageEnd, At: at(11 * time.Second), Message: types.AssistantMessage{StopReason: types.StopError}},
		{Type: agent.EventModelFallback, At: at(12 * time.Second), Fallback: &types.ModelFallback{From: testModel, To: testModel}},
		{Type: agent.EventModelAttempt, At: at(14 * time.Second), SentAt: at(14 * time.Second), Attempt: 1},
		update(14250*time.Millisecond, types.StreamEvent{Type: types.EvTextDelta, Delta: "backup"}),
	} {
		if err := listener.handle(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	chunks := collectedSeries(t, tel, "gen_ai.client.operation.time_to_first_chunk")
	if len(chunks) != 1 || chunks[0].count != 3 || chunks[0].min != 0.25 || chunks[0].max != 7.5 {
		t.Fatalf("time to first chunk = %+v, want 0.7s, 7.5s and 0.25s", chunks)
	}
}

// lateSleeper oversleeps every backoff, as a loaded scheduler can: the retry
// is sent well after its planned delay.
type lateSleeper struct{ overrun time.Duration }

func (s lateSleeper) Sleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay + s.overrun)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// A retry's time to first chunk runs from when the retrying stream actually
// sent it, not from its failure plus the planned backoff.
func TestRetryTimeToFirstChunkStartsAtTheActualSend(t *testing.T) {
	const overrun = 300 * time.Millisecond
	tel := New(Options{Settings: config.OTelSettings{ServiceName: "tempest"}})
	provider := &scriptedProvider{attempts: []func(*stream.AssistantStream){partialFailedAttempt, textAttempt("done")}}
	cfg := retry.DefaultConfig()
	cfg.BaseDelay, cfg.BackoffCap = time.Millisecond, time.Millisecond
	cfg.Jitter = retry.Jitter{Min: 1, Max: 1}
	cfg.Sleeper = lateSleeper{overrun: overrun}
	retrier, err := retry.New(provider, cfg)
	if err != nil {
		t.Fatal(err)
	}
	agt := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{Model: testModel}, StreamFn: retrier.Stream})
	defer tel.Attach(agt, AgentInfo{})()
	if result, err := agt.Prompt(context.Background(), types.UserMessage{Content: types.StringContent("run")}); err != nil || result.StopReason != agent.StopCompleted {
		t.Fatalf("run = %+v, %v", result, err)
	}
	chunks := collectedSeries(t, tel, "gen_ai.client.operation.time_to_first_chunk")
	if len(chunks) != 1 || chunks[0].count != 2 || chunks[0].max >= overrun.Seconds()/2 {
		t.Fatalf("time to first chunk = %+v; the retry's backoff overrun of %s was counted as waiting for output", chunks, overrun)
	}
}
