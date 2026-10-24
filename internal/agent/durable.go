package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/work"
)

// ErrNoActiveRun rejects publication outside the run's serialized event lifetime.
var ErrNoActiveRun = errors.New("agent: no active run")

// RunState binds durable identity and recovered logical accounting to the loop.
type RunState struct {
	SessionID      string
	RunID          string
	LogicalRunID   string
	ContinuationOf string
	StartedAt      time.Time
	Usage          types.Usage
	Turns          int
	UsageSources   map[string]types.Usage
}
type runtimeBindings struct {
	mu          sync.Mutex
	state       RunState
	receipt     string
	invocations []work.Invocation
	accounting  *runAccounting
}

func (b *runtimeBindings) snapshot() RunState {
	if b == nil {
		return RunState{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}
func (b *runtimeBindings) lastReceipt() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.receipt
}
func (b *runtimeBindings) invocation(i int) (work.Invocation, bool) {
	if b == nil {
		return work.Invocation{}, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if i < 0 || i >= len(b.invocations) {
		return work.Invocation{}, false
	}
	return b.invocations[i], true
}

// SetRunState is called by the durable recorder after run_started commits.
func (a *Agent) SetRunState(state RunState) {
	sources := make(map[string]types.Usage, len(state.UsageSources))
	for source, usage := range state.UsageSources {
		sources[source] = usage
	}
	state.UsageSources = sources
	a.bindings.mu.Lock()
	defer a.bindings.mu.Unlock()
	a.bindings.state = state
	a.bindings.receipt = ""
	a.bindings.invocations = nil
}

// RunState returns the durable identity of the current or most recent run.
func (a *Agent) RunState() RunState { return a.bindings.snapshot() }

// BindMessage records the receipt and canonical tool occurrences after message commit.
func (a *Agent) BindMessage(messageID, source string, message types.Message) {
	state := a.RunState()
	var invocations []work.Invocation
	if m, ok := message.(types.AssistantMessage); ok {
		for i, b := range m.Content {
			if b.Type == types.BlockToolCall {
				invocations = append(invocations, work.Invocation{RunID: state.RunID, CallID: fmt.Sprintf("%s:%d", messageID, i), ToolCallID: b.ID})
			}
		}
	}
	a.bindings.mu.Lock()
	defer a.bindings.mu.Unlock()
	a.bindings.receipt = source
	a.bindings.invocations = invocations
}

// PublishWork publishes an already committed fact through normal serialized delivery.
func (a *Agent) PublishWork(ctx context.Context, event work.Event) error {
	return a.processEvents(ctx, AgentEvent{Type: EventWork, Work: &event})
}

// ImportUsage durably imports a foreign receipt before charging shared run accounting.
func (a *Agent) ImportUsage(ctx context.Context, source, model string, usage types.Usage) error {
	accounting, _ := ctx.Value(accountingKey{}).(*runAccounting)
	if accounting == nil {
		return errors.New("usage import requires an accounting-bearing run context")
	}
	return accounting.record(ctx, source, model, usage, "")
}

// ImportHistoricalUsage imports a prior logical run without charging today's cap.
func (a *Agent) ImportHistoricalUsage(ctx context.Context, ownerRunID, source, model, childID string, usage types.Usage) error {
	accounting, _ := ctx.Value(accountingKey{}).(*runAccounting)
	if accounting == nil {
		return errors.New("usage import requires an accounting-bearing run context")
	}
	return accounting.recordForRun(ctx, source, model, usage, childID, ownerRunID, false)
}

type providerBudgetStop struct{ reason StopReason }

func (e *providerBudgetStop) Error() string { return "provider budget: " + string(e.reason) }

// AdmitProvider shares root cost admission with compaction calls. The caller must
// persist and account the response before releasing the returned token.
func AdmitProvider(ctx context.Context, model types.Model) (func(), error) {
	a, _ := ctx.Value(accountingKey{}).(*runAccounting)
	if a == nil {
		return func() {}, nil
	}
	release, stop, err := a.admit(ctx, model)
	if err != nil {
		return release, err
	}
	if stop != "" {
		return release, &providerBudgetStop{reason: stop}
	}
	return release, nil
}

// BackgroundProviderContext shares the live run's cost gate, not its cancellation.
// Its second result attributes the durable receipt to that run.
func (a *Agent) BackgroundProviderContext(ctx context.Context) (context.Context, string) {
	a.bindings.mu.Lock()
	accounting := a.bindings.accounting
	runID := a.bindings.state.RunID
	a.bindings.mu.Unlock()
	if accounting == nil {
		return ctx, ""
	}
	ctx = context.WithValue(ctx, accountingKey{}, accounting)
	return context.WithValue(ctx, durableProviderKey{}, true), runID
}

type durableProviderKey struct{}

// AccountProviderUsage forwards an already durable compaction receipt exactly once.
func AccountProviderUsage(ctx context.Context, source, model string, usage types.Usage) error {
	a, _ := ctx.Value(accountingKey{}).(*runAccounting)
	if a == nil {
		return nil
	}
	if native, _ := ctx.Value(durableProviderKey{}).(bool); native {
		a.mu.Lock()
		defer a.mu.Unlock()
		if prior, seen := a.sources[source]; seen {
			if !reflect.DeepEqual(prior, usage) {
				return fmt.Errorf("conflicting usage source %q", source)
			}
			return nil
		}
		a.sources[source] = usage
		a.usage.Add(usage)
		return nil
	}
	childID := ""
	if run, _ := ctx.Value(localRunKey{}).(*loopRun); run != nil && !run.root {
		run.result.Usage.Add(usage)
		childID = run.config.SessionID
	}
	return a.record(ctx, source, model, usage, childID)
}
func (a *runAccounting) record(ctx context.Context, source, model string, usage types.Usage, childID string) error {
	return a.recordForRun(ctx, source, model, usage, childID, "", true)
}
func (a *runAccounting) recordForRun(ctx context.Context, source, model string, usage types.Usage, childID, ownerRunID string, charge bool) error {
	a.receiptsMu.Lock()
	defer a.receiptsMu.Unlock()
	a.mu.Lock()
	prior, seen := a.sources[source]
	a.mu.Unlock()
	if source != "" && seen {
		if !reflect.DeepEqual(prior, usage) {
			return a.fail(fmt.Errorf("conflicting usage source %q", source))
		}
		return nil
	}
	if err := a.emit(context.WithoutCancel(ctx), AgentEvent{Type: EventUsage, Model: model, UsageSource: source, ChildSessionID: childID, UsageRunID: ownerRunID, RunResult: RunResult{Usage: usage}}); err != nil {
		return a.fail(err)
	}
	a.mu.Lock()
	if charge {
		a.usage.Add(usage)
	}
	if source != "" {
		a.sources[source] = usage
	}
	a.mu.Unlock()
	return nil
}
func invocationContext(ctx context.Context, config AgentLoopConfig, index int) context.Context {
	if invocation, ok := config.bindings.invocation(index); ok {
		return work.WithInvocation(ctx, invocation)
	}
	return ctx
}
