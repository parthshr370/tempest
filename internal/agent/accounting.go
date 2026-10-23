package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.harness.dev/harness/internal/engine/types"
)

type accountingKey struct{}
type localRunKey struct{}

// runAccounting follows the run context into child agents. Cost-capped calls
// are serialized through admission so parallel children cannot each spend the
// same remaining budget. A single admitted call may still overrun the soft cap.
type runAccounting struct {
	mu         sync.Mutex
	usage      types.Usage
	err        error
	maxCost    float64
	admission  chan struct{}
	emit       AgentEventSink
	sources    map[string]types.Usage
	receiptsMu sync.Mutex
}

func newLoopRun(ctx context.Context, config AgentLoopConfig, emit AgentEventSink, streamFn StreamFn) (context.Context, *loopRun) {
	accounting, _ := ctx.Value(accountingKey{}).(*runAccounting)
	root := accounting == nil
	if root {
		accounting = &runAccounting{maxCost: config.MaxCostUSD, emit: emit, sources: map[string]types.Usage{}}
		if config.MaxCostUSD > 0 {
			accounting.admission = make(chan struct{}, 1)
		}
		ctx = context.WithValue(ctx, accountingKey{}, accounting)
	}
	if config.OutputSchema != nil {
		config.SimpleStreamOptions.OutputSchema = config.OutputSchema.Raw()
	}
	run := &loopRun{config: config, emit: emit, streamFn: streamFn, accounting: accounting, root: root, started: time.Now()}
	return context.WithValue(ctx, localRunKey{}, run), run
}

func (a *runAccounting) total() types.Usage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.usage
}

func (a *runAccounting) failure() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.err
}

func (a *runAccounting) fail(err error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err == nil {
		a.err = err
	}
	return err
}

func (a *runAccounting) costStop() (StopReason, error) {
	if err := a.failure(); err != nil {
		return "", err
	}
	if a.maxCost == 0 {
		return "", nil
	}
	usage := a.total()
	if usage.Cost.Unknown {
		return "", fmt.Errorf("cost budget $%g cannot be enforced: %w", a.maxCost, ErrUnknownPrice)
	}
	if usage.Cost.Total >= a.maxCost {
		return StopMaxCost, nil
	}
	return "", nil
}

// admit holds the cost gate until the caller has accounted for the response.
func (a *runAccounting) admit(ctx context.Context, model types.Model) (release func(), stop StopReason, err error) {
	release = func() {}
	if ctx.Err() != nil {
		return release, StopAborted, nil
	}
	if a.admission == nil {
		return release, "", a.failure()
	}
	select {
	case a.admission <- struct{}{}:
		release = func() { <-a.admission }
	case <-ctx.Done():
		return release, StopAborted, nil
	}
	if stop, err = a.costStop(); stop != "" || err != nil {
		return release, stop, err
	}
	return release, "", a.fail((Budget{MaxCostUSD: a.maxCost}).Validate(model))
}

func (run *loopRun) accountUsage(ctx context.Context, message types.AssistantMessage) error {
	run.result.Usage.Add(message.Usage)
	source := run.config.bindings.lastReceipt()
	if !run.root {
		return run.accounting.record(ctx, source, message.Model, message.Usage, run.config.SessionID)
	}
	a := run.accounting
	a.mu.Lock()
	defer a.mu.Unlock()
	if source != "" {
		if _, ok := a.sources[source]; ok {
			return nil
		}
		a.sources[source] = message.Usage
	}
	a.usage.Add(message.Usage)
	return nil
}
