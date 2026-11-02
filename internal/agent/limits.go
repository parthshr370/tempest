package agent

import (
	"context"
	"fmt"
	"strings"

	"go.harness.dev/harness/internal/engine/types"
)

// promptTokens counts every token a model call was sent: uncached input plus
// cache reads and cache writes.
func promptTokens(u types.Usage) int { return u.Input + u.CacheRead + u.CacheWrite }

// tokenCapped reports whether b sets any token cap.
func tokenCapped(b Budget) bool {
	return b.MaxInputTokens > 0 || b.MaxOutputTokens > 0 || b.MaxTotalTokens > 0
}

// tokenStop names the first token cap usage has reached, or "". A cap of N
// admits no request once N tokens are used.
func tokenStop(b Budget, u types.Usage) StopReason {
	input, output := promptTokens(u), u.Output
	switch {
	case b.MaxInputTokens > 0 && input >= b.MaxInputTokens:
		return StopMaxInputTokens
	case b.MaxOutputTokens > 0 && output >= b.MaxOutputTokens:
		return StopMaxOutputTokens
	case b.MaxTotalTokens > 0 && input+output >= b.MaxTotalTokens:
		return StopMaxTotalTokens
	}
	return ""
}

// checkMeasured fails a token-capped run on a completed response that
// reported no tokens, which the caps could otherwise never count.
func (run *loopRun) checkMeasured(message types.AssistantMessage) error {
	if message.StopReason == types.StopError || message.StopReason == types.StopAborted {
		return nil
	}
	if promptTokens(message.Usage)+message.Usage.Output > 0 {
		return nil
	}
	if !tokenCapped(run.accounting.limits) && !tokenCapped(run.config.Budget) {
		return nil
	}
	return fmt.Errorf("token budget cannot be enforced for %s/%s: %w", message.Provider, message.Model, ErrUnknownUsage)
}

// admitToolCalls charges a response's tool calls to the run before any of
// them runs, or refuses the whole batch when it does not fit a tool-call
// cap: the root run's shared one, or a child run's own. The check and the
// charge happen under the receipt lock, so parallel child runs cannot both
// take the last calls. The charge is a usage receipt journaled before the
// tools run, which resume sums like token receipts; a batch already charged
// under the same source is not charged again. refusal is the text each
// refused call gets as its error result.
func (run *loopRun) admitToolCalls(ctx context.Context, message types.AssistantMessage) (refusal string, err error) {
	calls := 0
	for _, block := range message.Content {
		if block.Type == types.BlockToolCall {
			calls++
		}
	}
	source := ""
	if receipt := run.config.bindings.lastReceipt(); receipt != "" {
		source = "tools:" + strings.TrimPrefix(receipt, "message:")
	}
	a := run.accounting
	a.receiptsMu.Lock()
	defer a.receiptsMu.Unlock()
	a.mu.Lock()
	used := a.usage.ToolCalls
	_, charged := a.sources[source]
	a.mu.Unlock()
	if source != "" && charged {
		return "", nil
	}
	if limit := a.limits.MaxToolCalls; limit > 0 && used+calls > limit {
		return toolCallRefusal(calls, used, limit), nil
	}
	if limit, own := run.config.MaxToolCalls, run.result.Usage.ToolCalls; !run.root && limit > 0 && own+calls > limit {
		return toolCallRefusal(calls, own, limit), nil
	}
	usage := types.Usage{ToolCalls: calls}
	childID := ""
	if !run.root {
		childID = run.config.SessionID
	}
	if err := a.recordLocked(ctx, source, "", usage, childID, "", true); err != nil {
		return "", err
	}
	run.result.Usage.Add(usage)
	return "", nil
}

// toolCallRefusal tells the model why none of its calls ran.
func toolCallRefusal(requested, used, limit int) string {
	left := max(limit-used, 0)
	return fmt.Sprintf("Not run: this response asked for %d tool calls, but the run's tool-call limit of %d allows %d more (%d used). None of this response's calls ran, and the run stopped.", requested, limit, left, used)
}
