package agent

import (
	"log/slog"
	"strings"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/hooks"
)

// modelSpec is the "provider:model" form a fallback switch is recorded in.
func modelSpec(model types.Model) string { return model.Provider + ":" + model.ID }

// restoreFallback keeps a resumed run on the backup model its journal
// recorded, so a crash does not send the run back to an unavailable primary
// mid-way. A recorded backup that is no longer in the chain (the settings
// changed) leaves the run on its primary.
func (run *loopRun) restoreFallback(choice *FallbackChoice) {
	if choice == nil {
		return
	}
	backups := run.config.FallbackModels
	if choice.Index < 1 || choice.Index > len(backups) || modelSpec(backups[choice.Index-1]) != choice.Model {
		slog.Warn("recorded backup model is not in the fallback chain; resuming on the primary model", "backup", choice.Model, "fallback_index", choice.Index)
		return
	}
	run.config.Model = backups[choice.Index-1]
	run.fallbackIndex = choice.Index
	slog.Info("resuming on recorded backup model", "model", choice.Model, "fallback_index", choice.Index)
}

// Model-request hook statuses, as in Codex.
const (
	requestCompleted = "completed"
	requestFailed    = "failed"
	requestCancelled = "cancelled"
)

// modelRequest follows one logical model request across its retry attempts
// and fallback switches, and reports each attempt to the observation-only
// PreModelRequest and PostModelRequest hooks. Observe never waits, so the
// hooks cannot delay the request.
type modelRequest struct {
	hooks   *hooks.Runner
	turnID  string
	role    string
	model   types.Model
	index   int
	attempt int
	// open is set between an attempt's PreModelRequest and its PostModelRequest.
	open bool
}

func newModelRequest(config AgentLoopConfig, turnID string, fallbackIndex int) *modelRequest {
	role := config.ModelRole
	if role == "" {
		role = "default"
	}
	return &modelRequest{hooks: config.Hooks, turnID: turnID, role: role, model: config.Model, index: fallbackIndex}
}

// begin reports the start of the next attempt on the current model.
func (r *modelRequest) begin() {
	r.attempt++
	r.open = true
	r.observe("PreModelRequest", &hooks.ModelRequest{})
}

// end reports how the open attempt finished. message is the attempt's final
// or discarded message, nil when there is none.
func (r *modelRequest) end(status string, message *types.AssistantMessage) {
	if !r.open {
		return
	}
	r.open = false
	request := &hooks.ModelRequest{Status: status}
	if message != nil {
		if status == requestFailed {
			request.Error = strings.TrimSpace(message.ErrorMessage)
			if request.Error == "" {
				request.Error = message.ErrorCode
			}
		}
		if status == requestCompleted {
			request.TokenUsage = hookTokenUsage(message.Usage)
		}
	}
	r.observe("PostModelRequest", request)
}

// finish reports the request's terminal message.
func (r *modelRequest) finish(message types.AssistantMessage) {
	switch message.StopReason {
	case types.StopError:
		r.end(requestFailed, &message)
	case types.StopAborted:
		r.end(requestCancelled, &message)
	default:
		r.end(requestCompleted, &message)
	}
}

// retry closes a discarded attempt and opens the next one on the same model.
func (r *modelRequest) retry(failed *types.AssistantMessage) {
	r.end(requestFailed, failed)
	r.begin()
}

// switchModel closes the failed model's last attempt and opens the first
// attempt on the backup model.
func (r *modelRequest) switchModel(fallback types.ModelFallback, failed *types.AssistantMessage) {
	r.end(requestFailed, failed)
	r.model, r.index, r.attempt = fallback.To, fallback.Index, 0
	r.begin()
}

func (r *modelRequest) observe(event string, request *hooks.ModelRequest) {
	if !r.hooks.Has(event) {
		return
	}
	request.Role, request.Attempt, request.FallbackIndex = r.role, r.attempt, r.index
	r.hooks.Observe(hooks.Input{Event: event, TurnID: r.turnID, Model: r.model.ID, ModelRequest: request})
}

func hookTokenUsage(usage types.Usage) *hooks.TokenUsage {
	reasoning := 0
	if usage.Reasoning != nil {
		reasoning = *usage.Reasoning
	}
	total := usage.TotalTokens
	if total == 0 {
		total = usage.Input + usage.Output + usage.CacheRead + usage.CacheWrite
	}
	return &hooks.TokenUsage{
		InputTokens:           usage.Input + usage.CacheRead + usage.CacheWrite,
		CachedInputTokens:     usage.CacheRead,
		CacheWriteInputTokens: usage.CacheWrite,
		OutputTokens:          usage.Output,
		ReasoningOutputTokens: reasoning,
		TotalTokens:           total,
	}
}
