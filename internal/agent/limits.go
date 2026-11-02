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

// spendStop names the cost or token cap of b that usage u has reached. A
// cost cap the usage cannot be priced against is an error, not free.
func spendStop(b Budget, u types.Usage) (StopReason, error) {
	if b.MaxCostUSD > 0 {
		if u.Cost.Unknown {
			return "", fmt.Errorf("cost budget $%g cannot be enforced: %w", b.MaxCostUSD, ErrUnknownPrice)
		}
		if u.Cost.Total >= b.MaxCostUSD {
			return StopMaxCost, nil
		}
	}
	return tokenStop(b, u), nil
}

func (run *loopRun) checkMeasured(message types.AssistantMessage) error {
	return run.accounting.checkMeasured(run.config.Budget, message)
}

// CheckMeasured holds a provider response made for ctx's run outside its
// loop, such as a compaction summary, to the same rule as the run's own
// responses (see runAccounting.checkMeasured).
func CheckMeasured(ctx context.Context, message types.AssistantMessage) error {
	a, _ := ctx.Value(accountingKey{}).(*runAccounting)
	if a == nil {
		return nil
	}
	var own Budget
	if run, _ := ctx.Value(localRunKey{}).(*loopRun); run != nil {
		own = run.config.Budget
	}
	return a.checkMeasured(own, message)
}

// checkMeasured fails a run on a completed response that reported no tokens
// while a token cap applies, as the cap could otherwise never count it.
// The root run's caps bind its children, so under them the failure is
// latched on the shared accounting: the root and every child stop before
// their next request, whatever a delegating tool does with a child's error.
// A cap only the run itself has (own) fails only that run.
func (a *runAccounting) checkMeasured(own Budget, message types.AssistantMessage) error {
	if message.StopReason == types.StopError || message.StopReason == types.StopAborted {
		return nil
	}
	if promptTokens(message.Usage)+message.Usage.Output > 0 {
		return nil
	}
	err := fmt.Errorf("token budget cannot be enforced for %s/%s: %w", message.Provider, message.Model, ErrUnknownUsage)
	switch {
	case tokenCapped(a.limits):
		return a.fail(err)
	case tokenCapped(own):
		return err
	}
	return nil
}

// attemptStop decides whether a request may be sent again after attempts
// that failed, as a retry or to a backup: not once the spend, with pending
// (those attempts' usage not yet charged), has reached a cost or token cap,
// the root run's shared ones or run's own (run may be nil). A reached cap
// is a *providerBudgetStop, which ends the run at that cap.
func (a *runAccounting) attemptStop(run *loopRun, pending types.Usage) error {
	if err := a.failure(); err != nil {
		return err
	}
	shared := a.total()
	shared.Add(pending)
	stop, err := spendStop(a.limits, shared)
	if stop == "" && err == nil && run != nil {
		own := run.result.Usage
		own.Add(pending)
		stop, err = spendStop(run.config.Budget, own)
	}
	if stop != "" {
		return &providerBudgetStop{reason: stop}
	}
	return err
}

// attemptGate makes a stream's retry and fallback layers wait, before they
// send a request again, for its consumer to answer the EvRetry or
// EvFallback event that announced the failed attempt.
type attemptGate struct{ retry, fallback chan error }

// gateAttempts installs a gate in opts. A fallback hook already in opts runs
// once the gate admits the switch.
func gateAttempts(opts *types.SimpleStreamOptions) *attemptGate {
	g := &attemptGate{retry: make(chan error, 1), fallback: make(chan error, 1)}
	switched := opts.AdmitFallback
	opts.AdmitRetry = func(ctx context.Context, _ int) error { return g.wait(ctx, g.retry) }
	opts.AdmitFallback = func(ctx context.Context, fallback types.ModelFallback) error {
		if err := g.wait(ctx, g.fallback); err != nil || switched == nil {
			return err
		}
		return switched(ctx, fallback)
	}
	return g
}

func (g *attemptGate) wait(ctx context.Context, answer chan error) error {
	select {
	case err := <-answer:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// answer admits (nil) or refuses the attempt announced by event. An answer
// nobody waited for, from a stream that announces without asking, is
// replaced by the next one.
func (g *attemptGate) answer(event types.StreamEvent, err error) {
	answer := g.retry
	if event.Type == types.EvFallback && event.Fallback != nil {
		answer = g.fallback
	}
	select {
	case <-answer:
	default:
	}
	answer <- err
}

// GateAttempts holds provider work admitted with AdmitProvider to the caps
// of ctx's run between its attempts. opts's retry and fallback layers then
// send the request again, as a retry or to a backup, only once observe has
// seen the EvRetry or EvFallback event announcing the failed attempt and the
// usage of the attempts failed so far leaves the run under its cost and
// token caps. The caller passes observe every event it reads; observe
// returns the refusal, a *providerBudgetStop or the run's failure, which
// the caller returns once the stream has ended. A fallback hook already in
// opts runs once the gate admits the switch.
func GateAttempts(ctx context.Context, opts *types.SimpleStreamOptions) (observe func(types.StreamEvent) error) {
	a, _ := ctx.Value(accountingKey{}).(*runAccounting)
	if a == nil {
		return func(types.StreamEvent) error { return nil }
	}
	run, _ := ctx.Value(localRunKey{}).(*loopRun)
	gate := gateAttempts(opts)
	var pending types.Usage
	return func(event types.StreamEvent) error {
		if event.Type != types.EvRetry && event.Type != types.EvFallback {
			return nil
		}
		if event.Err != nil {
			pending.Add(event.Err.Usage)
		}
		err := a.attemptStop(run, pending)
		gate.answer(event, err)
		return err
	}
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
