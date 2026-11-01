package telemetry

import (
	"slices"
	"sync"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/hooks"
)

// hookExitBlock is the Claude Code hook exit code that blocks the action.
const hookExitBlock = 2

// liveSessions is the state behind the tempest.sessions and tempest.jobs
// gauges: each open session registers a function that counts its
// unfinished background jobs.
type liveSessions struct {
	mu       sync.Mutex
	next     int
	sessions map[int]func() int
}

// TrackSession counts a session as live until release is called. jobs
// reports how many of its background jobs have not finished; it is called
// at collection time, never on the turn loop.
func (t *Telemetry) TrackSession(jobs func() int) (release func()) {
	if t == nil {
		return func() {}
	}
	t.live.mu.Lock()
	defer t.live.mu.Unlock()
	t.live.next++
	id := t.live.next
	t.live.sessions[id] = jobs
	return func() {
		t.live.mu.Lock()
		defer t.live.mu.Unlock()
		delete(t.live.sessions, id)
	}
}

func (t *Telemetry) observeSessions() []observation {
	t.live.mu.Lock()
	defer t.live.mu.Unlock()
	return []observation{{value: int64(len(t.live.sessions))}}
}

func (t *Telemetry) observeJobs() []observation {
	t.live.mu.Lock()
	counters := make([]func() int, 0, len(t.live.sessions))
	for _, jobs := range t.live.sessions {
		counters = append(counters, jobs)
	}
	t.live.mu.Unlock()
	var total int64
	for _, jobs := range counters {
		if jobs != nil {
			total += int64(jobs())
		}
	}
	return []observation{{value: total}}
}

func (t *Telemetry) observeSpanLoss() []observation {
	if t.spans == nil {
		return nil
	}
	return []observation{
		{attributes: []attribute{stringAttribute("reason", "queue_full")}, value: int64(t.spans.dropped.Load())},
		{attributes: []attribute{stringAttribute("reason", "export_failed")}, value: int64(t.health.failed.Load())},
	}
}

func (t *Telemetry) registerObservedInstruments() {
	t.metrics.observed("tempest.sessions", "{session}", "Open sessions.", kindGauge, t.observeSessions)
	t.metrics.observed("tempest.jobs", "{job}", "Background jobs that have not finished, across open sessions.", kindGauge, t.observeJobs)
	t.metrics.observed("tempest.telemetry.spans.lost", "{span}", "Spans not exported, by reason.", kindCounter, t.observeSpanLoss)
}

func requestAttributes(model types.Model) []attribute {
	return []attribute{
		stringAttribute("gen_ai.operation.name", "chat"),
		stringAttribute("gen_ai.provider.name", model.Provider),
		stringAttribute("gen_ai.request.model", model.ID),
	}
}

// inputTokens is gen_ai.usage.input_tokens: every input token, cached ones
// included, as the GenAI conventions define it. Tempest's Usage.Input counts
// only the uncached part, with cache reads and writes beside it.
func inputTokens(usage types.Usage) int {
	return usage.Input + usage.CacheRead + usage.CacheWrite
}

// recordRequest measures one finished model request: its tokens, duration
// (retries included) and cost.
func (t *Telemetry) recordRequest(model types.Model, responseModel string, message types.AssistantMessage, duration time.Duration) {
	attrs := append(requestAttributes(model), stringAttribute("gen_ai.response.model", responseModel))
	durationAttrs := attrs
	if message.StopReason == types.StopError || message.StopReason == types.StopAborted {
		durationAttrs = append(slices.Clone(attrs), stringAttribute("error.type", errorType(message)))
	}
	t.metrics.observe(t.metrics.duration, duration.Seconds(), durationAttrs...)
	t.recordUsage(attrs, message.Usage)
}

// recordDiscardedUsage measures what a failed attempt that was sent again
// consumed, as run accounting counts it. An attempt that failed before the
// provider reported usage consumed nothing measurable and is not recorded.
func (t *Telemetry) recordDiscardedUsage(model types.Model, message types.AssistantMessage) {
	usage := message.Usage
	if inputTokens(usage) == 0 && usage.Output == 0 {
		return
	}
	responseModel := message.ResponseModel
	if responseModel == "" {
		responseModel = message.Model
	}
	t.recordUsage(append(requestAttributes(model), stringAttribute("gen_ai.response.model", responseModel)), usage)
}

func (t *Telemetry) recordUsage(attrs []attribute, usage types.Usage) {
	t.metrics.observe(t.metrics.tokenUsage, float64(inputTokens(usage)), append(slices.Clone(attrs), stringAttribute("gen_ai.token.type", "input"))...)
	t.metrics.observe(t.metrics.tokenUsage, float64(usage.Output), append(slices.Clone(attrs), stringAttribute("gen_ai.token.type", "output"))...)
	if !usage.Cost.Unknown {
		t.metrics.observe(t.metrics.cost, usage.Cost.Total, attrs...)
	}
}

func (t *Telemetry) recordFirstChunk(model types.Model, elapsed time.Duration) {
	t.metrics.observe(t.metrics.timeToFirstChunk, elapsed.Seconds(), requestAttributes(model)...)
}

func (t *Telemetry) recordAttemptFailure(kind string, model types.Model, errorType string) {
	t.metrics.increment(t.metrics.failedAttempts,
		stringAttribute("gen_ai.provider.name", model.Provider),
		stringAttribute("gen_ai.request.model", model.ID),
		stringAttribute("error.type", errorType),
		stringAttribute("tempest.attempt.outcome", kind),
	)
}

func (t *Telemetry) recordPermission(tool, decision, source string) {
	t.metrics.increment(t.metrics.permissions,
		stringAttribute("gen_ai.tool.name", tool),
		stringAttribute("tempest.permission.decision", decision),
		stringAttribute("tempest.permission.source", source),
	)
}

func (t *Telemetry) recordToolCall(tool, outcome string) {
	t.metrics.increment(t.metrics.toolCalls, stringAttribute("gen_ai.tool.name", tool), stringAttribute("tempest.tool.outcome", outcome))
}

func (t *Telemetry) recordRun(agentName string, result agent.RunResult) {
	t.metrics.increment(t.metrics.runs, stringAttribute("gen_ai.agent.name", agentName), stringAttribute("tempest.run.stop_reason", string(result.StopReason)))
}

// recordHook counts one hook command run. Exit code 2 is the hook blocking
// the action, a decision rather than a failure.
func (t *Telemetry) recordHook(report *hooks.Report) {
	outcome := "success"
	switch {
	case report.TimedOut:
		outcome = "timeout"
	case report.Error != "":
		outcome = "error"
	case report.ExitCode == hookExitBlock:
		outcome = "blocked"
	case report.ExitCode != 0:
		outcome = "failed"
	}
	t.metrics.increment(t.metrics.hookRuns, stringAttribute("tempest.hook.event", report.Event), stringAttribute("tempest.hook.outcome", outcome))
}
