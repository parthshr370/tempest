package telemetry

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/config"
	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/retry"
	"go.harness.dev/harness/internal/tracecontext"
)

// scriptedProvider answers each call with the next scripted attempt and
// records the trace context each request was sent under.
type scriptedProvider struct {
	mu           sync.Mutex
	attempts     []func(*stream.AssistantStream)
	calls        int
	traceparents []string
}

func (p *scriptedProvider) Stream(ctx context.Context, model types.Model, _ types.Context, _ *types.SimpleStreamOptions) *stream.AssistantStream {
	p.mu.Lock()
	attempt := p.attempts[p.calls]
	p.calls++
	p.traceparents = append(p.traceparents, tracecontext.FromContext(ctx).Traceparent())
	p.mu.Unlock()
	events := stream.NewAssistantStream(model.API, model.Provider, model.ID)
	go attempt(events)
	return events
}

// unavailableAttempt fails before any output, as an HTTP error does.
func unavailableAttempt(events *stream.AssistantStream) {
	events.Push(types.StreamEvent{Type: types.EvError, Reason: types.StopError, Err: &types.AssistantMessage{
		StopReason:   types.StopError,
		ErrorCode:    string(retry.CodeStreamInterrupted),
		ErrorMessage: "connection reset by peer",
	}})
}

func toolCallAttempt(name, arguments string) func(*stream.AssistantStream) {
	return func(events *stream.AssistantStream) {
		call := types.ContentBlock{Type: types.BlockToolCall, ID: "call-1", Name: name, Arguments: json.RawMessage(arguments)}
		events.Push(types.StreamEvent{Type: types.EvStart})
		events.Push(types.StreamEvent{Type: types.EvToolCallStart, ContentIndex: 0, ToolCall: &types.ContentBlock{Type: types.BlockToolCall, ID: "call-1", Name: name}})
		events.Push(types.StreamEvent{Type: types.EvToolCallEnd, ContentIndex: 0, ToolCall: &call})
		events.Push(types.StreamEvent{Type: types.EvDone, Reason: types.StopToolUse, Message: &types.AssistantMessage{
			Content: []types.ContentBlock{call}, StopReason: types.StopToolUse, ResponseID: "resp-1",
			Usage: types.Usage{Input: 120, Output: 8, CacheRead: 30, TotalTokens: 158, Cost: types.Cost{Total: 0.002}},
		}})
	}
}

func textAttempt(text string) func(*stream.AssistantStream) {
	return func(events *stream.AssistantStream) {
		events.Push(types.StreamEvent{Type: types.EvStart})
		events.Push(types.StreamEvent{Type: types.EvTextStart, ContentIndex: 0})
		events.Push(types.StreamEvent{Type: types.EvTextDelta, ContentIndex: 0, Delta: text})
		events.Push(types.StreamEvent{Type: types.EvDone, Reason: types.StopStop, Message: &types.AssistantMessage{
			Content: []types.ContentBlock{types.NewText(text)}, StopReason: types.StopStop, ResponseID: "resp-2",
			Usage: types.Usage{Input: 140, Output: 4, TotalTokens: 144, Cost: types.Cost{Total: 0.001}},
		}})
	}
}

type instantSleeper struct{}

func (instantSleeper) Sleep(context.Context, time.Duration) error { return nil }

func retrying(t *testing.T, provider *scriptedProvider) agent.StreamFn {
	t.Helper()
	cfg := retry.DefaultConfig()
	cfg.MaxAttempts = 3
	cfg.Sleeper = instantSleeper{}
	retrier, err := retry.New(provider, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return retrier.Stream
}

var testModel = types.Model{API: "anthropic-messages", Provider: "anthropic", ID: "claude-test", BaseURL: "https://api.anthropic.com"}

func echoTool(execute func(context.Context) error) agent.AgentTool {
	return agent.AgentTool{
		Tool: types.Tool{Name: "echo", Description: "echo", Parameters: json.RawMessage(`{"type":"object"}`)},
		Execute: func(ctx context.Context, _ string, _ json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			if execute != nil {
				if err := execute(ctx); err != nil {
					return agent.AgentToolResult{}, err
				}
			}
			return agent.AgentToolResult{Content: []types.ContentBlock{types.NewText("echoed")}}, nil
		},
	}
}

func newTestTelemetry(t *testing.T, c *collector, capture bool) *Telemetry {
	t.Helper()
	return New(Options{Settings: config.OTelSettings{
		TracesEndpoint:        c.server.URL + "/v1/traces",
		Timeout:               5 * time.Second,
		ServiceName:           "tempest",
		CaptureMessageContent: capture,
	}, SpanBatchInterval: 10 * time.Millisecond})
}

func spansByName(spans []receivedSpan) map[string][]receivedSpan {
	byName := map[string][]receivedSpan{}
	for _, received := range spans {
		byName[received.Name] = append(byName[received.Name], received)
	}
	return byName
}

func collectedSpans(t *testing.T, c *collector) []receivedSpan {
	t.Helper()
	var spans []receivedSpan
	for _, request := range c.snapshot() {
		spans = append(spans, decodeSpans(t, request.body)...)
	}
	return spans
}

// TestFauxRunSpanTree is the P3 acceptance run: one tool call and one retry
// reach a fake collector as invoke_agent with chat and execute_tool siblings
// and the failed attempt under its request.
func TestFauxRunSpanTree(t *testing.T) {
	c := newCollector(t)
	tel := newTestTelemetry(t, c, false)
	provider := &scriptedProvider{attempts: []func(*stream.AssistantStream){unavailableAttempt, toolCallAttempt("echo", `{"text":"secret argument"}`), textAttempt("done")}}
	agt := agent.NewAgent(agent.AgentOptions{
		InitialState: &agent.AgentState{Model: testModel, Tools: []agent.AgentTool{echoTool(nil)}},
		StreamFn:     retrying(t, provider),
		Trace:        tel.Tracing(),
	})
	detach := tel.Attach(agt, AgentInfo{SessionID: "session-1"})
	defer detach()
	result, err := agt.Prompt(context.Background(), types.UserMessage{Content: types.StringContent("say done")})
	if err != nil || result.StopReason != agent.StopCompleted {
		t.Fatalf("run = %+v, %v", result, err)
	}
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	spans := collectedSpans(t, c)
	byName := spansByName(spans)
	if len(spans) != 5 || len(byName["invoke_agent tempest"]) != 1 || len(byName["chat claude-test"]) != 3 || len(byName["execute_tool echo"]) != 1 {
		t.Fatalf("span names = %v", c.spanNames(t))
	}
	run := byName["invoke_agent tempest"][0]
	if run.ParentSpanID != "" || run.Kind != int(spanKindInternal) {
		t.Fatalf("run span is not a root: %+v", run)
	}
	for _, received := range spans {
		if received.TraceID != run.TraceID {
			t.Fatalf("%s is in trace %s, want %s", received.Name, received.TraceID, run.TraceID)
		}
	}
	var requests, attempts []receivedSpan
	for _, chat := range byName["chat claude-test"] {
		if chat.ParentSpanID == run.SpanID {
			requests = append(requests, chat)
		} else {
			attempts = append(attempts, chat)
		}
	}
	if len(requests) != 2 || len(attempts) != 1 {
		t.Fatalf("chat spans: %d under the run, %d elsewhere", len(requests), len(attempts))
	}
	failed := attempts[0]
	if failed.ParentSpanID != requests[0].SpanID && failed.ParentSpanID != requests[1].SpanID {
		t.Fatal("the failed attempt is not a child of a request span")
	}
	if failed.Status.Code != int(statusError) || failed.attr("error.type") != string(retry.CodeStreamInterrupted) || failed.attr("tempest.attempt") != "1" || failed.attr("tempest.attempt.outcome") != "retry" {
		t.Fatalf("failed attempt = %+v", failed)
	}
	tool := byName["execute_tool echo"][0]
	if tool.ParentSpanID != run.SpanID {
		t.Fatal("execute_tool must be a sibling of chat under the run span")
	}
	if tool.attr("gen_ai.tool.call.id") != "call-1" || tool.attr("tempest.tool.outcome") != "success" || tool.has("gen_ai.tool.call.arguments") {
		t.Fatalf("tool span = %+v", tool)
	}
	for _, request := range requests {
		if request.Kind != int(spanKindClient) || request.attr("gen_ai.operation.name") != "chat" || request.attr("gen_ai.provider.name") != "anthropic" || request.attr("server.address") != "api.anthropic.com" || request.attr("server.port") != "443" {
			t.Fatalf("request span = %+v", request)
		}
		if request.has("gen_ai.input.messages") || request.has("gen_ai.output.messages") {
			t.Fatal("message content captured without OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT")
		}
	}
	for _, request := range requests {
		// gen_ai.usage.input_tokens includes the cached input (120 + 30).
		if request.attr("gen_ai.response.id") == "resp-1" && (request.attr("gen_ai.usage.input_tokens") != "150" || request.attr("gen_ai.usage.cache_read.input_tokens") != "30") {
			t.Fatalf("tool call request usage = %+v", request.Attributes)
		}
	}
	if run.attr("gen_ai.aggregated_usage.input_tokens") != "290" || run.attr("gen_ai.aggregated_usage.output_tokens") != "12" || run.attr("gen_ai.aggregated_usage.cache_read.input_tokens") != "30" {
		t.Fatalf("run totals = %+v", run.Attributes)
	}
	if run.has("gen_ai.usage.input_tokens") || run.attr("gen_ai.conversation.id") != "session-1" || run.attr("tempest.cost.usd") != "0.003" {
		t.Fatalf("run attributes = %+v", run.Attributes)
	}

	// Every attempt went out under its request span, which providers send as
	// the traceparent header.
	for i, traceparent := range provider.traceparents {
		parsed, ok := tracecontext.Parse(traceparent)
		if !ok || parsed.TraceID.String() != run.TraceID {
			t.Fatalf("attempt %d traceparent %q is not in the run's trace", i, traceparent)
		}
	}
}

func TestCaptureMessageContentOptIn(t *testing.T) {
	c := newCollector(t)
	tel := newTestTelemetry(t, c, true)
	provider := &scriptedProvider{attempts: []func(*stream.AssistantStream){toolCallAttempt("echo", `{"text":"hello"}`), textAttempt("done")}}
	agt := agent.NewAgent(agent.AgentOptions{
		InitialState: &agent.AgentState{Model: testModel, SystemPrompt: "be brief", Tools: []agent.AgentTool{echoTool(nil)}},
		StreamFn:     provider.Stream,
		Trace:        true,
	})
	defer tel.Attach(agt, AgentInfo{})()
	if _, err := agt.Prompt(context.Background(), types.UserMessage{Content: types.StringContent("say hello")}); err != nil {
		t.Fatal(err)
	}
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	byName := spansByName(collectedSpans(t, c))
	tool := byName["execute_tool echo"][0]
	if tool.attr("gen_ai.tool.call.arguments") != `{"text":"hello"}` || tool.attr("gen_ai.tool.call.result") != "echoed" {
		t.Fatalf("tool content = %+v", tool.Attributes)
	}
	first := byName["chat claude-test"][0]
	if first.attr("gen_ai.input.messages") != `[{"role":"user","parts":[{"type":"text","content":"say hello"}]}]` {
		t.Fatalf("input messages = %s", first.attr("gen_ai.input.messages"))
	}
	if first.attr("gen_ai.system_instructions") == "" || first.attr("gen_ai.output.messages") == "" {
		t.Fatalf("request content = %+v", first.Attributes)
	}
}

// TestSubagentRunIsChildOfToolSpan runs a child agent inside a tool call: its
// invoke_agent span must sit under the parent's execute_tool span.
func TestSubagentRunIsChildOfToolSpan(t *testing.T) {
	c := newCollector(t)
	tel := newTestTelemetry(t, c, false)
	childProvider := &scriptedProvider{attempts: []func(*stream.AssistantStream){textAttempt("child done")}}
	child := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{Model: testModel}, StreamFn: childProvider.Stream, Trace: true, Child: true})
	defer tel.Attach(child, AgentInfo{Name: "explore"})()
	parentProvider := &scriptedProvider{attempts: []func(*stream.AssistantStream){toolCallAttempt("echo", `{}`), textAttempt("done")}}
	parent := agent.NewAgent(agent.AgentOptions{
		InitialState: &agent.AgentState{Model: testModel, Tools: []agent.AgentTool{echoTool(func(ctx context.Context) error {
			_, err := child.Prompt(ctx, types.UserMessage{Content: types.StringContent("explore")})
			return err
		})}},
		StreamFn: parentProvider.Stream,
		Trace:    true,
	})
	defer tel.Attach(parent, AgentInfo{})()
	if _, err := parent.Prompt(context.Background(), types.UserMessage{Content: types.StringContent("delegate")}); err != nil {
		t.Fatal(err)
	}
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	byName := spansByName(collectedSpans(t, c))
	tool := byName["execute_tool echo"][0]
	childRun := byName["invoke_agent explore"]
	if len(childRun) != 1 || childRun[0].ParentSpanID != tool.SpanID || childRun[0].TraceID != tool.TraceID {
		t.Fatalf("child run %+v is not under tool span %s", childRun, tool.SpanID)
	}
}

// TestUntracedAgentExportsNoSpans keeps a run without Trace out of the
// exporter even under a remote parent.
func TestUntracedAgentExportsNoSpans(t *testing.T) {
	c := newCollector(t)
	tel := newTestTelemetry(t, c, false)
	provider := &scriptedProvider{attempts: []func(*stream.AssistantStream){textAttempt("done")}}
	agt := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{Model: testModel}, StreamFn: provider.Stream})
	defer tel.Attach(agt, AgentInfo{})()
	remote := tracecontext.SpanContext{TraceID: tracecontext.NewTraceID(), SpanID: tracecontext.NewSpanID(), Sampled: true}
	if _, err := agt.Prompt(tracecontext.WithRemoteParent(context.Background(), remote), types.UserMessage{Content: types.StringContent("hi")}); err != nil {
		t.Fatal(err)
	}
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if names := c.spanNames(t); len(names) != 0 {
		t.Fatalf("untraced run exported %v", names)
	}
	if provider.traceparents[0] != (tracecontext.SpanContext{}).Traceparent() {
		t.Fatalf("untraced request carried trace context %q", provider.traceparents[0])
	}
}

// Every span of a run under a remote parent records the tracestate that
// came with that parent, as OTLP Span.trace_state.
func TestSpansCarryTheRemoteTracestate(t *testing.T) {
	c := newCollector(t)
	tel := newTestTelemetry(t, c, false)
	provider := &scriptedProvider{attempts: []func(*stream.AssistantStream){unavailableAttempt, toolCallAttempt("echo", `{}`), textAttempt("done")}}
	agt := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{Model: testModel, Tools: []agent.AgentTool{echoTool(nil)}}, StreamFn: retrying(t, provider), Trace: tel.Tracing()})
	defer tel.Attach(agt, AgentInfo{})()
	remote := tracecontext.SpanContext{TraceID: tracecontext.NewTraceID(), SpanID: tracecontext.NewSpanID(), Sampled: true, TraceState: "congo=t61rcWkgMzE"}
	if _, err := agt.Prompt(tracecontext.WithRemoteParent(context.Background(), remote), types.UserMessage{Content: types.StringContent("hi")}); err != nil {
		t.Fatal(err)
	}
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	spans := collectedSpans(t, c)
	if len(spans) != 5 {
		t.Fatalf("span names = %v", c.spanNames(t))
	}
	for _, received := range spans {
		if received.TraceState != remote.TraceState {
			t.Errorf("%s traceState = %q, want %q", received.Name, received.TraceState, remote.TraceState)
		}
	}
}

// TestLoopGuardReachesTheRunSpan: a run the loop guard nudged and then
// stopped carries the loop's pattern, the action and the nudge count, and
// fails with error.type loop_detected.
func TestLoopGuardReachesTheRunSpan(t *testing.T) {
	c := newCollector(t)
	tel := newTestTelemetry(t, c, false)
	attempts := make([]func(*stream.AssistantStream), 6)
	for index := range attempts {
		attempts[index] = toolCallAttempt("echo", `{"text":"same"}`)
	}
	agt := agent.NewAgent(agent.AgentOptions{
		InitialState:   &agent.AgentState{Model: testModel, Tools: []agent.AgentTool{echoTool(nil)}},
		StreamFn:       (&scriptedProvider{attempts: attempts}).Stream,
		Trace:          tel.Tracing(),
		LoopThresholds: &agent.LoopThresholds{RepeatedResult: 3},
	})
	detach := tel.Attach(agt, AgentInfo{SessionID: "session-1"})
	defer detach()
	result, err := agt.Prompt(context.Background(), types.UserMessage{Content: types.StringContent("echo")})
	if err != nil || result.StopReason != agent.StopLoopDetected {
		t.Fatalf("run = %+v, %v", result, err)
	}
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	run := spansByName(collectedSpans(t, c))["invoke_agent tempest"]
	if len(run) != 1 {
		t.Fatalf("span names = %v", c.spanNames(t))
	}
	span := run[0]
	if span.Status.Code != int(statusError) || span.attr("error.type") != "loop_detected" || span.attr("tempest.loop.pattern") != "repeated_result" || span.attr("tempest.loop.action") != "stop" || span.attr("tempest.loop.nudges") != "1" {
		t.Fatalf("run span = %+v", span)
	}
}
