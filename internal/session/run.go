// Package session wires a generic coding agent: the tool registry, prompt
// building, permission and compaction hooks ([BuildAgentStack]), and one run
// of the agent ([Run]).
package session

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.harness.dev/harness/internal/agent"
	ptypes "go.harness.dev/harness/internal/engine/types"
)

// RunOptions configures one run.
type RunOptions struct {
	// Budget caps the run; the zero value is unlimited. Run installs it on
	// the agent with [agent.Agent.SetBudget], so it also applies to later runs
	// started directly on that agent.
	Budget agent.Budget
}

// RunOutcome is how one run ended.
type RunOutcome struct {
	agent.RunResult
	// Elapsed is the wall time of the run.
	Elapsed time.Duration
	// LastAssistantText is the text of the run's last assistant message that
	// had any text; empty when the run produced none.
	LastAssistantText string
}

// Run sends prompt to agt as one run and blocks until the run ends. The run
// completes when the model replies with no tool calls; a budget, Abort or ctx
// cancellation, or a failure ends it earlier (see [agent.StopReason]).
//
// The error is non-nil only when no run started ([agent.ErrRunActive], a
// budget that cannot be enforced). A run that started and failed reports
// [agent.StopError] with the cause in RunOutcome.Err.
func Run(ctx context.Context, agt *agent.Agent, prompt InitialPrompt, opts RunOptions) (RunOutcome, error) {
	if agt == nil {
		return RunOutcome{}, errors.New("session run: agent is nil")
	}
	started := time.Now()
	// The agent serializes listener calls and Prompt returns after the last
	// one, so lastText needs no lock.
	var lastText string
	unsubscribe := agt.Subscribe(func(_ context.Context, ev agent.AgentEvent) error {
		if ev.Type == agent.EventMessageEnd {
			if text := assistantMessageText(ev.Message); text != "" {
				lastText = text
			}
		}
		return nil
	})
	defer unsubscribe()

	agt.SetBudget(opts.Budget)
	var result agent.RunResult
	var err error
	if len(prompt.Media) == 0 {
		result, err = agt.PromptText(ctx, prompt.Text)
	} else {
		result, err = agt.Prompt(ctx, initialUserMessage(prompt))
	}
	if err != nil {
		return RunOutcome{}, err
	}
	return RunOutcome{RunResult: result, Elapsed: time.Since(started), LastAssistantText: lastText}, nil
}

// assistantMessageText extracts the concatenated text blocks of an assistant
// message; it returns "" for any other message.
func assistantMessageText(message ptypes.Message) string {
	var msg ptypes.AssistantMessage
	switch value := message.(type) {
	case ptypes.AssistantMessage:
		msg = value
	case *ptypes.AssistantMessage:
		if value == nil {
			return ""
		}
		msg = *value
	default:
		return ""
	}
	parts := []string{}
	for _, block := range msg.Content {
		if block.Type == ptypes.BlockText {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}
