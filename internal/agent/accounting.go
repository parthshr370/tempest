package agent

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.harness.dev/harness/internal/engine/types"
)

type accountingKey struct{}
type localRunKey struct{}

// runAccounting follows the run context into child agents. Cost- and
// token-capped calls are serialized through admission so parallel children
// cannot each spend the same remaining budget. A single admitted call may
// still overrun the soft cap.
type runAccounting struct {
	mu    sync.Mutex
	usage types.Usage
	err   error
	// limits are the root run's caps shared with its children: cost, tokens
	// and tool calls. Turn and wall-time caps stay per run.
	limits     Budget
	admission  chan struct{}
	emit       AgentEventSink
	sources    map[string]types.Usage
	receiptsMu sync.Mutex
	// Provider work started outside the loop joins the run until it ends.
	joinMu sync.Mutex
	ending bool
	joined sync.WaitGroup
	end    context.Context
	endRun context.CancelFunc
}

func newLoopRun(ctx context.Context, config AgentLoopConfig, emit AgentEventSink, streamFn StreamFn) (context.Context, *loopRun) {
	accounting, _ := ctx.Value(accountingKey{}).(*runAccounting)
	root := accounting == nil
	if root {
		accounting = &runAccounting{limits: config.Budget, emit: emit, sources: map[string]types.Usage{}}
		if config.MaxCostUSD > 0 || tokenCapped(config.Budget) {
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

// join admits provider work started outside the loop until the run starts
// ending. Its context is cancelled when the run ends; done must be called
// once the work has accounted for its spend.
func (a *runAccounting) join(ctx context.Context) (context.Context, func(), bool) {
	a.joinMu.Lock()
	defer a.joinMu.Unlock()
	if a.ending {
		return ctx, func() {}, false
	}
	if a.end == nil {
		a.end, a.endRun = context.WithCancel(context.Background())
	}
	a.joined.Add(1)
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(a.end, cancel)
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			stop()
			cancel()
			a.joined.Done()
		})
	}, true
}

// settle stops admitting outside work, cancels what is in flight, and waits
// until each piece has journaled and charged its spend to this run.
func (a *runAccounting) settle() {
	a.joinMu.Lock()
	a.ending = true
	endRun := a.endRun
	a.joinMu.Unlock()
	if endRun != nil {
		endRun()
	}
	a.joined.Wait()
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

// withFailure joins the shared accounting's failure to err unless err
// already carries it, as when the failing call returned it too.
func withFailure(err, failure error) error {
	if failure == nil || errors.Is(err, failure) {
		return err
	}
	return errors.Join(err, failure)
}

// capStop names the shared cost or token cap the run's usage has reached.
func (a *runAccounting) capStop() (StopReason, error) {
	if err := a.failure(); err != nil {
		return "", err
	}
	return spendStop(a.limits, a.total())
}

// admit holds the cost and token gate until the caller has accounted for
// the response.
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
	if stop, err = a.capStop(); stop != "" || err != nil {
		return release, stop, err
	}
	return release, "", a.fail((Budget{MaxCostUSD: a.limits.MaxCostUSD}).Validate(model))
}

func (run *loopRun) accountUsage(ctx context.Context, message types.AssistantMessage) error {
	if err := run.chargeUsage(ctx, message); err != nil {
		return err
	}
	return run.checkMeasured(message)
}

func (run *loopRun) chargeUsage(ctx context.Context, message types.AssistantMessage) error {
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
