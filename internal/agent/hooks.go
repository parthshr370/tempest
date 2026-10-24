package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/hooks"
	"go.harness.dev/harness/internal/work"
)

var errHookPromptBlocked = errors.New("prompt blocked by hook")
var errHookPromptConsumed = errors.New("interrupted queued prompt consumed")

func hookMessage(event, text string) types.Message {
	return types.UserMessage{Content: types.StringContent(event + " hook context: " + text), Timestamp: time.Now().UnixMilli()}
}

func (run *loopRun) startHooks(ctx context.Context, current *AgentContext) error {
	if run.config.Child {
		return nil
	}
	result, err := run.config.Hooks.Start(ctx, len(current.Messages) > 0)
	if err != nil {
		return err
	}
	for _, text := range result.Context {
		if err := run.appendMessages(ctx, current, []types.Message{hookMessage("SessionStart", text)}); err != nil {
			return err
		}
	}
	if result.Stop {
		return errHookPromptBlocked
	}
	return nil
}

func (run *loopRun) promptHooks(ctx context.Context, message types.Message, queueID string) (hooks.Result, error) {
	if !run.config.Hooks.Has("UserPromptSubmit") {
		return hooks.Result{}, nil
	}
	user, ok := message.(types.UserMessage)
	if !ok {
		return hooks.Result{}, nil
	}
	text := user.Content.Text
	if user.Content.IsBlocks() {
		var parts []string
		for _, block := range user.Content.Blocks {
			if block.Type == types.BlockText {
				parts = append(parts, block.Text)
			}
		}
		text = strings.Join(parts, "\n")
	}
	key := ""
	if queueID != "" {
		key = "prompt:" + queueID
	}
	return run.config.Hooks.Run(ctx, hooks.Input{Key: key, Event: "UserPromptSubmit", Prompt: text})
}

func (run *loopRun) appendPrompts(ctx context.Context, current *AgentContext, messages []types.Message) error {
	for _, message := range messages {
		run.stopHookActive = false
		run.stopHookContinuations = 0
		result, err := run.promptHooks(ctx, message, "")
		if err != nil {
			return err
		}
		if result.Decision == "deny" || result.Stop {
			return run.blockPrompt(ctx, result, "")
		}
		if err := run.appendMessages(ctx, current, []types.Message{message}); err != nil {
			return err
		}
		for _, text := range result.Context {
			if err := run.appendMessages(ctx, current, []types.Message{hookMessage("UserPromptSubmit", text)}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (run *loopRun) completeWithHooks(ctx context.Context) ([]work.QueuedMessage, error) {
	pending := run.complete()
	if len(pending) > 0 {
		run.hookFollowUp = true
	}
	if len(pending) > 0 || run.result.StopReason != StopCompleted || run.config.Hooks == nil || ctx.Err() != nil {
		return pending, nil
	}
	event := "Stop"
	if run.config.Child {
		event = "SubagentStop"
	}
	result, err := run.config.Hooks.Run(ctx, hooks.Input{Event: event, StopHookActive: run.stopHookActive, LastAssistantMessage: finalAnswerText(run.messages)})
	if err != nil {
		return nil, err
	}
	if result.Stop || hooks.Stopped(ctx) {
		return nil, nil
	}
	texts := result.Context
	if result.Decision == "deny" {
		texts = append([]string{result.Reason}, texts...)
	}
	if len(texts) == 0 || run.stopHookContinuations >= 8 {
		return nil, nil
	}
	run.stopHookContinuations++
	run.stopHookActive = true
	run.result.StopReason, run.result.Output = "", nil
	// Hook feedback isn't a new user submission and mustn't run UserPromptSubmit again.
	for _, text := range texts {
		pending = append(pending, work.QueuedMessage{Message: hookMessage(event, text)})
	}
	run.hookFollowUp = true
	return pending, nil
}

// EndSessionHooks runs cleanup hooks after work has joined, before the host closes its event stream.
func (a *Agent) EndSessionHooks(ctx context.Context) error {
	if a.Hooks == nil || a.Child {
		return nil
	}
	ctx = hooks.WithRun(ctx, func(ctx context.Context, report hooks.Report) error {
		a.emitMu.Lock()
		defer a.emitMu.Unlock()
		a.mu.Lock()
		a.seq++
		event := AgentEvent{Type: EventHook, Hook: &report, Seq: a.seq, At: time.Now()}
		listeners := append([]agentListener(nil), a.listeners...)
		a.mu.Unlock()
		for _, listener := range listeners {
			if err := listener.fn(ctx, event); err != nil {
				return err
			}
		}
		return nil
	})
	return a.Hooks.End(ctx)
}

func (run *loopRun) blockPrompt(ctx context.Context, result hooks.Result, queueID string) error {
	reason := result.Reason
	if result.Stop {
		reason = result.StopReason
	}
	if reason == "" {
		reason = "Prompt not submitted: blocked by UserPromptSubmit hook"
	}
	report := hooks.Report{Event: "UserPromptSubmit", Error: reason}
	if err := run.emit(context.WithoutCancel(ctx), AgentEvent{Type: EventHook, Hook: &report, QueueID: queueID}); err != nil {
		return err
	}
	if result.Interrupted {
		return errHookPromptConsumed
	}
	return fmt.Errorf("%w: %s", errHookPromptBlocked, reason)
}
