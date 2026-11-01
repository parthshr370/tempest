package telemetry

import (
	"context"
	"encoding/json"
	"net/url"
	"slices"
	"strconv"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/obs"
	"go.harness.dev/harness/internal/tracecontext"
)

// Span shape, after the Pydantic AI instrumentation (format 5) and the
// OpenTelemetry GenAI conventions:
//
//	invoke_agent <agent>            one per run; a child agent's run sits
//	├── chat <model>                under the parent's execute_tool span
//	│   └── chat <model>            a failed attempt that was retried, or
//	│                               whose model a backup replaced
//	├── execute_tool <tool>
//	└── chat <model>
//
// Requests and tools are siblings under the run: a tool is never the child
// of a request span that has already ended. Run totals go on the run span
// as gen_ai.aggregated_usage.*, a namespace of its own, so summing
// gen_ai.usage.* over a trace does not count a request twice.
//
// A request whose model is unavailable goes on to a backup model. The
// request span then describes the backup, which answers it, and carries a
// tempest.model.fallback event for the switch; the failed model's attempts
// stay its children under their own model.

const (
	defaultAgentName  = "tempest"
	maxErrorTextRunes = 1024
	// attemptRetry marks an attempt the retrying stream sent again to the
	// same model; attemptFallback one whose request went to a backup model.
	attemptRetry    = "retry"
	attemptFallback = "fallback"
	// fallbackEvent is the request span event for a switch to a backup.
	fallbackEvent = "tempest.model.fallback"
)

// AgentInfo identifies the agent an event listener reports on.
type AgentInfo struct {
	// Name is gen_ai.agent.name: the subagent type for a child, else "tempest".
	Name string
	// SessionID is gen_ai.conversation.id.
	SessionID string
}

// Attach subscribes a listener that turns agt's events into spans and
// metrics. It never fails the run: the listener only does bounded,
// in-memory work and returns nil.
func (t *Telemetry) Attach(agt *agent.Agent, info AgentInfo) (detach func()) {
	if t == nil {
		return func() {}
	}
	if info.Name == "" {
		info.Name = defaultAgentName
	}
	listener := &agentListener{telemetry: t, agent: agt, info: info}
	return agt.Subscribe(listener.handle)
}

// agentListener follows one agent's runs. The agent never calls a listener
// concurrently, so its state needs no lock.
type agentListener struct {
	telemetry *Telemetry
	agent     *agent.Agent
	info      AgentInfo
	run       *runTrace
}

type runTrace struct {
	span    *span
	sampled bool
	request *requestTrace
	tools   map[string]*toolTrace
}

type requestTrace struct {
	span *span
	// model is the model attempts go to: the request's, then the backup a
	// switch named.
	model types.Model
	// attemptStart is when the current attempt was sent: the request's
	// start, then each model_attempt's send time.
	attemptStart time.Time
	firstChunk   bool
	// messageOpen is set between an attempt's message_start and its
	// message_end; discardNextEnd marks that end as a failed attempt's.
	messageOpen    bool
	discardNextEnd bool
}

type toolTrace struct {
	span *span
}

// attemptFailure is one abandoned request attempt, announced by the retrying
// stream.
type attemptFailure struct {
	// at is when the attempt failed, on the retrying stream's side.
	at      time.Time
	kind    string
	attempt int
	message types.AssistantMessage
}

func (l *agentListener) handle(ctx context.Context, event agent.AgentEvent) error {
	switch event.Type {
	case agent.EventAgentStart:
		l.startRun(ctx, event)
	case agent.EventModelRequest:
		l.startRequest(ctx, event)
	case agent.EventMessageStart:
		if request := l.request(); request != nil && isAssistant(event.Message) {
			request.messageOpen = true
		}
	case agent.EventMessageUpdate:
		l.chunk(event)
	case agent.EventModelRetry:
		failed, _ := assistantMessage(event.Message)
		at := event.FailedAt
		if at.IsZero() {
			at = event.At
		}
		failure := attemptFailure{at: at, kind: attemptRetry, attempt: event.Attempt, message: failed}
		if event.Fallback != nil {
			failure.kind = attemptFallback
		}
		l.attemptFailed(failure)
	case agent.EventModelAttempt:
		l.attemptSent(event)
	case agent.EventModelFallback:
		l.switched(event)
	case agent.EventMessageEnd:
		l.messageEnd(event)
	case agent.EventToolPermission, agent.EventToolExecutionStart:
		l.tool(ctx, event)
	case agent.EventToolExecutionEnd:
		l.endTool(ctx, event)
	case agent.EventHook:
		if event.Hook != nil {
			l.telemetry.recordHook(event.Hook)
		}
	case agent.EventAgentEnd:
		l.endRun(event)
	}
	return nil
}

func (l *agentListener) request() *requestTrace {
	if l.run == nil {
		return nil
	}
	return l.run.request
}

func isAssistant(message types.Message) bool {
	_, ok := assistantMessage(message)
	return ok
}

func assistantMessage(message types.Message) (types.AssistantMessage, bool) {
	switch typed := message.(type) {
	case types.AssistantMessage:
		return typed, true
	case *types.AssistantMessage:
		if typed != nil {
			return *typed, true
		}
	}
	return types.AssistantMessage{}, false
}

// newSpan starts a span with the identity the agent put in ctx; an untraced
// ctx gives a span with zero ids, which is timed but never exported.
func newSpan(ctx context.Context, name string, kind spanKind, start time.Time) *span {
	current := tracecontext.FromContext(ctx)
	return &span{traceID: current.TraceID, spanID: current.SpanID, traceState: current.TraceState, parentID: tracecontext.ParentFromContext(ctx).SpanID, name: name, kind: kind, start: start}
}

func (l *agentListener) startRun(ctx context.Context, event agent.AgentEvent) {
	current := tracecontext.FromContext(ctx)
	runSpan := newSpan(ctx, "invoke_agent "+l.info.Name, spanKindInternal, event.At)
	runSpan.attributes = append(runSpan.attributes,
		stringAttribute("gen_ai.operation.name", "invoke_agent"),
		stringAttribute("gen_ai.agent.name", l.info.Name),
	)
	if l.info.SessionID != "" {
		runSpan.attributes = append(runSpan.attributes, stringAttribute("gen_ai.conversation.id", l.info.SessionID))
	}
	l.run = &runTrace{span: runSpan, sampled: current.IsValid() && current.Sampled, tools: map[string]*toolTrace{}}
}

func (l *agentListener) startRequest(ctx context.Context, event agent.AgentEvent) {
	if l.run == nil || event.Request == nil {
		return
	}
	l.abandonRequest(l.run, event.At)
	request := event.Request
	requestSpan := newSpan(ctx, "chat "+request.Model.ID, spanKindClient, event.At)
	requestSpan.attributes = append(requestSpan.attributes, stringAttribute("gen_ai.operation.name", "chat"))
	requestSpan.attributes = append(requestSpan.attributes, modelAttributes(request.Model)...)
	if request.MaxTokens > 0 {
		requestSpan.attributes = append(requestSpan.attributes, intAttribute("gen_ai.request.max_tokens", int64(request.MaxTokens)))
	}
	if request.Temperature != nil {
		requestSpan.attributes = append(requestSpan.attributes, doubleAttribute("gen_ai.request.temperature", *request.Temperature))
	}
	if request.Reasoning != "" {
		requestSpan.attributes = append(requestSpan.attributes, stringAttribute("tempest.request.reasoning", request.Reasoning))
	}
	if l.telemetry.settings.CaptureMessageContent {
		if request.SystemPrompt != "" {
			requestSpan.attributes = append(requestSpan.attributes, deferredAttribute("gen_ai.system_instructions", systemInstructions(request.SystemPrompt)))
		}
		requestSpan.attributes = append(requestSpan.attributes, deferredAttribute("gen_ai.input.messages", inputMessages(request.Messages)))
	}
	l.run.request = &requestTrace{span: requestSpan, model: request.Model, attemptStart: event.At}
}

// modelAttributes describe the model a request goes to: its provider, id
// and endpoint.
func modelAttributes(model types.Model) []attribute {
	attrs := []attribute{
		stringAttribute("gen_ai.provider.name", model.Provider),
		stringAttribute("gen_ai.request.model", model.ID),
	}
	return append(attrs, serverAttributes(model.BaseURL)...)
}

// isModelAttribute reports whether attr is one modelAttributes sets.
func isModelAttribute(attr attribute) bool {
	switch attr.key {
	case "gen_ai.provider.name", "gen_ai.request.model", "server.address", "server.port":
		return true
	}
	return false
}

// switched moves the open request to the backup model a switch named: the
// backup answers it, so the request span and the metrics of its response
// and later attempts are the backup's. The failed model's attempts already
// closed as child spans under their own model. The backup's first attempt is
// timed from its model_attempt, sent once the switch was admitted.
func (l *agentListener) switched(event agent.AgentEvent) {
	if l.run == nil || event.Fallback == nil {
		return
	}
	fallback := *event.Fallback
	target := l.run.span
	if request := l.run.request; request != nil {
		target = request.span
		request.model = fallback.To
		request.span.name = "chat " + fallback.To.ID
		request.span.attributes = append(slices.DeleteFunc(request.span.attributes, isModelAttribute), modelAttributes(fallback.To)...)
	}
	attrs := append(fallbackAttributes(fallback),
		intAttribute("tempest.fallback.attempts", int64(fallback.Attempts)),
		intAttribute("tempest.fallback.index", int64(fallback.Index)),
	)
	target.events = append(target.events, spanEvent{name: fallbackEvent, at: event.At, attributes: attrs})
	l.telemetry.recordFallback(fallback)
}

// serverAttributes reports the provider endpoint's host and port.
func serverAttributes(baseURL string) []attribute {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Hostname() == "" {
		return nil
	}
	attrs := []attribute{stringAttribute("server.address", parsed.Hostname())}
	port := parsed.Port()
	if port == "" {
		switch parsed.Scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	if number, err := strconv.Atoi(port); err == nil {
		attrs = append(attrs, intAttribute("server.port", int64(number)))
	}
	return attrs
}

// chunk notes the response's first output for time-to-first-chunk.
func (l *agentListener) chunk(event agent.AgentEvent) {
	request := l.request()
	if request == nil || request.firstChunk || !carriesOutput(event.AssistantMessageEvent) {
		return
	}
	request.firstChunk = true
	l.telemetry.recordFirstChunk(request.model, event.At.Sub(request.attemptStart))
}

// carriesOutput reports whether a stream event delivers response content:
// text, reasoning or a tool call's name or arguments. The GenAI
// time-to-first-chunk excludes events that only open or close an empty
// block, such as a tool call start that carries nothing but its ID.
func carriesOutput(event *types.StreamEvent) bool {
	if event == nil {
		return false
	}
	switch event.Type {
	case types.EvTextDelta, types.EvThinkingDelta, types.EvToolCallDelta:
		return event.Delta != ""
	case types.EvTextEnd, types.EvThinkingEnd:
		return event.Content != ""
	case types.EvToolCallStart, types.EvToolCallEnd:
		return event.ToolCall != nil && (event.ToolCall.Name != "" || len(event.ToolCall.Arguments) != 0)
	}
	return false
}

// attemptFailed closes one abandoned attempt as a child span of its request.
// The next attempt is timed from its model_attempt; until then nothing is in
// flight, so the failure is the earliest it can have been sent.
func (l *agentListener) attemptFailed(failure attemptFailure) {
	request := l.request()
	if request == nil {
		return
	}
	provider, model := failure.message.Provider, failure.message.Model
	if model == "" {
		provider, model = request.model.Provider, request.model.ID
	}
	attemptSpan := &span{
		traceID:    request.span.traceID,
		spanID:     tracecontext.NewSpanID(),
		traceState: request.span.traceState,
		parentID:   request.span.spanID,
		name:       "chat " + model,
		kind:       spanKindClient,
		start:      request.attemptStart,
		end:        failure.at,
		status:     statusError,
		message:    obs.RedactTruncate(failure.message.ErrorMessage, maxErrorTextRunes),
	}
	attemptSpan.attributes = append(attemptSpan.attributes,
		stringAttribute("gen_ai.operation.name", "chat"),
		stringAttribute("gen_ai.provider.name", provider),
		stringAttribute("gen_ai.request.model", model),
		stringAttribute("error.type", errorType(failure.message)),
		stringAttribute("tempest.attempt.outcome", failure.kind),
	)
	if failure.attempt > 0 {
		attemptSpan.attributes = append(attemptSpan.attributes, intAttribute("tempest.attempt", int64(failure.attempt)))
	}
	attemptSpan.attributes = append(attemptSpan.attributes, usageAttributes("gen_ai.usage.", failure.message.Usage)...)
	attemptSpan.attributes = append(attemptSpan.attributes, costAttributes(failure.message.Usage.Cost)...)
	l.telemetry.record(attemptSpan, l.run.sampled && request.span.spanID != tracecontext.SpanID{})
	l.telemetry.recordAttemptFailure(failure.kind, request.model, errorType(failure.message))
	l.telemetry.recordDiscardedUsage(request.model, failure.message)
	request.discardNextEnd = request.messageOpen
	request.messageOpen = false
	request.firstChunk = false
	request.attemptStart = failure.at
}

// attemptSent starts timing the attempt the retrying stream just sent, from
// when it was sent rather than when its event is handled.
func (l *agentListener) attemptSent(event agent.AgentEvent) {
	request := l.request()
	if request == nil {
		return
	}
	request.firstChunk = false
	request.attemptStart = event.SentAt
	if request.attemptStart.IsZero() {
		request.attemptStart = event.At
	}
}

func (l *agentListener) messageEnd(event agent.AgentEvent) {
	request := l.request()
	message, ok := assistantMessage(event.Message)
	if request == nil || !ok {
		return
	}
	if request.discardNextEnd {
		request.discardNextEnd = false
		return
	}
	l.run.request = nil
	requestSpan := request.span
	requestSpan.end = event.At
	responseModel := message.ResponseModel
	if responseModel == "" {
		responseModel = message.Model
	}
	requestSpan.attributes = append(requestSpan.attributes,
		stringAttribute("gen_ai.response.model", responseModel),
		stringsAttribute("gen_ai.response.finish_reasons", []string{finishReason(message.StopReason)}),
	)
	if message.ResponseID != "" {
		requestSpan.attributes = append(requestSpan.attributes, stringAttribute("gen_ai.response.id", message.ResponseID))
	}
	requestSpan.attributes = append(requestSpan.attributes, usageAttributes("gen_ai.usage.", message.Usage)...)
	requestSpan.attributes = append(requestSpan.attributes, costAttributes(message.Usage.Cost)...)
	failed := message.StopReason == types.StopError || message.StopReason == types.StopAborted
	if failed {
		requestSpan.status = statusError
		requestSpan.message = obs.RedactTruncate(message.ErrorMessage, maxErrorTextRunes)
		requestSpan.attributes = append(requestSpan.attributes, stringAttribute("error.type", errorType(message)))
	}
	if l.telemetry.settings.CaptureMessageContent && !failed {
		requestSpan.attributes = append(requestSpan.attributes, deferredAttribute("gen_ai.output.messages", outputMessages(message)))
	}
	l.telemetry.record(requestSpan, l.run.sampled)
	l.telemetry.recordRequest(request.model, responseModel, message, event.At.Sub(requestSpan.start))
}

// abandonRequest ends a run's request that never reported its response,
// which only happens when a listener failure cut the stream short.
func (l *agentListener) abandonRequest(run *runTrace, at time.Time) {
	if run == nil || run.request == nil {
		return
	}
	request := run.request
	run.request = nil
	request.span.end = at
	request.span.status = statusError
	request.span.attributes = append(request.span.attributes, stringAttribute("error.type", "incomplete"))
	l.telemetry.record(request.span, run.sampled)
}

// errorType is error.type for a failed response: the provider's error code
// when it sent one.
func errorType(message types.AssistantMessage) string {
	switch {
	case message.StopReason == types.StopAborted:
		return "aborted"
	case message.ErrorCode != "":
		return message.ErrorCode
	}
	return "error"
}

// usageAttributes reports usage in GenAI terms: input_tokens counts every
// input token, cached ones included, and the cache counts say how many of
// them were read from or written to the provider's cache.
func usageAttributes(prefix string, usage types.Usage) []attribute {
	return []attribute{
		intAttribute(prefix+"input_tokens", int64(inputTokens(usage))),
		intAttribute(prefix+"output_tokens", int64(usage.Output)),
		intAttribute(prefix+"cache_read.input_tokens", int64(usage.CacheRead)),
		intAttribute(prefix+"cache_creation.input_tokens", int64(usage.CacheWrite)),
	}
}

// costAttributes reports USD cost; an unknown price is flagged rather than
// reported as zero, which would read as free.
func costAttributes(cost types.Cost) []attribute {
	if cost.Unknown {
		return []attribute{boolAttribute("tempest.cost.unknown", true)}
	}
	return []attribute{doubleAttribute("tempest.cost.usd", cost.Total)}
}

func (l *agentListener) tool(ctx context.Context, event agent.AgentEvent) {
	if l.run == nil {
		return
	}
	call, ok := l.run.tools[event.ToolCallID]
	if !ok {
		toolSpan := newSpan(ctx, "execute_tool "+event.ToolName, spanKindInternal, event.At)
		toolSpan.attributes = append(toolSpan.attributes,
			stringAttribute("gen_ai.operation.name", "execute_tool"),
			stringAttribute("gen_ai.tool.name", event.ToolName),
			stringAttribute("gen_ai.tool.call.id", event.ToolCallID),
		)
		call = &toolTrace{span: toolSpan}
		l.run.tools[event.ToolCallID] = call
	}
	if event.Type == agent.EventToolPermission && event.Permission != nil {
		decision := permissionDecision(event.Permission)
		call.span.attributes = append(call.span.attributes,
			stringAttribute("tempest.permission.decision", decision),
			stringAttribute("tempest.permission.source", event.Permission.Source),
		)
		l.telemetry.recordPermission(event.ToolName, decision, event.Permission.Source)
	}
	if event.Type == agent.EventToolExecutionStart && !event.Preparing && l.telemetry.settings.CaptureMessageContent {
		if arguments := toolCallArguments(event.Args); len(arguments) != 0 {
			call.span.attributes = append(call.span.attributes, stringAttribute("gen_ai.tool.call.arguments", toolArguments(arguments)))
		}
	}
}

func permissionDecision(outcome *agent.PermissionOutcome) string {
	if outcome.Allow {
		return "allow"
	}
	return "deny"
}

// toolCallArguments reads a tool call's arguments: the loop passes the
// validated JSON bytes the tool receives as the event's untyped Args.
func toolCallArguments(args any) json.RawMessage {
	raw, _ := args.(json.RawMessage)
	return raw
}

func (l *agentListener) endTool(ctx context.Context, event agent.AgentEvent) {
	if l.run == nil {
		return
	}
	call, ok := l.run.tools[event.ToolCallID]
	if !ok {
		l.tool(ctx, event)
		call = l.run.tools[event.ToolCallID]
	}
	delete(l.run.tools, event.ToolCallID)
	call.span.end = event.At
	outcome := "success"
	if event.IsError {
		outcome = "error"
		call.span.status = statusError
		call.span.attributes = append(call.span.attributes, stringAttribute("error.type", "tool_error"))
	}
	call.span.attributes = append(call.span.attributes, stringAttribute("tempest.tool.outcome", outcome))
	if result, ok := event.Result.(agent.AgentToolResult); ok && l.telemetry.settings.CaptureMessageContent {
		call.span.attributes = append(call.span.attributes, stringAttribute("gen_ai.tool.call.result", toolResultText(result)))
	}
	l.telemetry.record(call.span, l.run.sampled)
	l.telemetry.recordToolCall(event.ToolName, outcome)
}

func (l *agentListener) endRun(event agent.AgentEvent) {
	if l.run == nil {
		return
	}
	run := l.run
	l.run = nil
	l.abandonRequest(run, event.At)
	for id, call := range run.tools {
		call.span.end = event.At
		call.span.status = statusError
		call.span.attributes = append(call.span.attributes, stringAttribute("error.type", "incomplete"))
		l.telemetry.record(call.span, run.sampled)
		delete(run.tools, id)
	}
	runSpan := run.span
	runSpan.end = event.At
	result := event.RunResult
	runState := l.agent.RunState()
	if runState.RunID != "" {
		runSpan.attributes = append(runSpan.attributes, stringAttribute("gen_ai.agent.call.id", runState.RunID))
	}
	runSpan.attributes = append(runSpan.attributes,
		stringAttribute("tempest.run.stop_reason", string(result.StopReason)),
		intAttribute("tempest.run.turns", int64(result.Turns)),
	)
	runSpan.attributes = append(runSpan.attributes, usageAttributes("gen_ai.aggregated_usage.", result.Usage)...)
	runSpan.attributes = append(runSpan.attributes, costAttributes(result.Usage.Cost)...)
	switch result.StopReason {
	case agent.StopError, agent.StopOutputInvalid, agent.StopRepeatedFailure:
		runSpan.status = statusError
		runSpan.attributes = append(runSpan.attributes, stringAttribute("error.type", string(result.StopReason)))
		if result.Err != nil {
			runSpan.message = obs.RedactTruncate(result.Err.Error(), maxErrorTextRunes)
		}
	}
	l.telemetry.record(runSpan, run.sampled)
	l.telemetry.recordRun(l.info.Name, result)
}
