package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/work"
)

type testRunHooks struct {
	start  func(context.Context, work.RunInfo) error
	idle   func(context.Context) (bool, error)
	finish func(context.Context, string) error
}

func (h testRunHooks) Start(ctx context.Context, info work.RunInfo) error {
	if h.start != nil {
		return h.start(ctx, info)
	}
	return nil
}
func (h testRunHooks) Idle(ctx context.Context) (bool, error) {
	if h.idle != nil {
		return h.idle(ctx)
	}
	return false, nil
}
func (h testRunHooks) Finish(ctx context.Context, reason string) error {
	if h.finish != nil {
		return h.finish(ctx, reason)
	}
	return nil
}

func TestWorkIdleWallDeadlineJoinsBeforeAgentEnd(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("done")))
	var a *Agent
	started, joined := false, false
	var stop string
	hooks := testRunHooks{start: func(ctx context.Context, _ work.RunInfo) error {
		if !started {
			return errors.New("work before durable start")
		}
		if _, ok := ctx.Value(accountingKey{}).(*runAccounting); !ok {
			return errors.New("lost root accounting")
		}
		return nil
	}, idle: func(ctx context.Context) (bool, error) { <-ctx.Done(); return true, nil }, finish: func(ctx context.Context, reason string) error {
		if ctx.Err() != nil {
			return errors.New("finish must persist after cancellation")
		}
		joined = true
		stop = reason
		return a.PublishWork(ctx, work.Event{Kind: "job", Action: "joined"})
	}}
	a = newRuntimeAgent(f, func(o *AgentOptions) { o.Work = hooks; o.Budget.MaxWallTime = 20 * time.Millisecond })
	a.Subscribe(func(ctx context.Context, e AgentEvent) error {
		if e.Type == EventAgentStart {
			started = true
		}
		if e.Type == EventAgentEnd && !joined {
			return errors.New("terminal before joined work")
		}
		return nil
	})
	result, err := a.PromptText(context.Background(), "go")
	if err != nil || result.Err != nil || result.StopReason != StopMaxWallTime || stop != string(StopMaxWallTime) || f.CallCount() != 1 {
		t.Fatalf("result=%+v err=%v stop=%s calls=%d", result, err, stop, f.CallCount())
	}
	if err := a.PublishWork(context.Background(), work.Event{Kind: "job"}); !errors.Is(err, ErrNoActiveRun) {
		t.Fatalf("late publication=%v", err)
	}
}
func TestRecoveredBudgetAndWallPermitNoProvider(t *testing.T) {
	for _, test := range []struct {
		name   string
		budget Budget
		state  RunState
		want   StopReason
	}{
		{"cost", Budget{MaxCostUSD: 1}, RunState{Usage: types.Usage{Cost: types.Cost{Total: 1.1}}}, StopMaxCost},
		{"turns", Budget{MaxTurns: 2}, RunState{Turns: 2}, StopMaxTurns},
		{"wall", Budget{MaxWallTime: time.Second}, RunState{StartedAt: time.Now().Add(-time.Hour)}, StopMaxWallTime},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := faux.New(faux.Options{})
			a := newRuntimeAgent(f, func(o *AgentOptions) { o.Budget = test.budget })
			a.Subscribe(func(ctx context.Context, e AgentEvent) error {
				if e.Type == EventAgentStart {
					a.SetRunState(test.state)
				}
				return nil
			})
			r, err := a.PromptText(context.Background(), "go")
			if err != nil || r.StopReason != test.want || f.CallCount() != 0 {
				t.Fatalf("result=%+v err=%v calls=%d", r, err, f.CallCount())
			}
		})
	}
}
func TestUsageImportsAndCompactorShareSingleAdmission(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("never")))
	var a *Agent
	forwarded := 0
	usage := types.Usage{Input: 5, TotalTokens: 5, Cost: types.Cost{Total: 1.1}}
	hooks := testRunHooks{start: func(ctx context.Context, _ work.RunInfo) error {
		release, err := AdmitProvider(ctx, f.Model())
		if err != nil {
			return err
		}
		blocked := make(chan struct{})
		admitted := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			close(blocked)
			next, err := AdmitProvider(ctx, f.Model())
			defer next()
			if err == nil {
				close(admitted)
			}
		}()
		<-blocked
		if err := a.ImportUsage(ctx, "child:receipt", "child", usage); err != nil {
			release()
			return err
		}
		if err := a.ImportUsage(ctx, "child:receipt", "child", usage); err != nil {
			release()
			return err
		}
		if err := a.ImportHistoricalUsage(ctx, "old", "historical:receipt", "old-model", "old-child", types.Usage{Input: 100, TotalTokens: 100}); err != nil {
			release()
			return err
		}
		release()
		wg.Wait()
		select {
		case <-admitted:
			return errors.New("second admission escaped exhausted shared cost")
		default:
		}
		return nil
	}}
	a = newRuntimeAgent(f, func(o *AgentOptions) { o.Work = hooks; o.Budget.MaxCostUSD = 1 })
	a.Subscribe(func(ctx context.Context, e AgentEvent) error {
		if e.Type == EventUsage {
			forwarded++
		}
		return nil
	})
	result, err := a.PromptText(context.Background(), "go")
	if err != nil || result.StopReason != StopMaxCost || result.Usage != usage || forwarded != 2 || f.CallCount() != 0 {
		t.Fatalf("result=%+v err=%v receipts=%d calls=%d", result, err, forwarded, f.CallCount())
	}
}
func TestWorkFinishFailureChangesOutcome(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("done")))
	failure := errors.New("failed terminal persistence")
	a := newRuntimeAgent(f, func(o *AgentOptions) {
		o.Work = testRunHooks{finish: func(context.Context, string) error { return failure }}
	})
	result, err := a.PromptText(context.Background(), "go")
	if err != nil || result.StopReason != StopError || !errors.Is(result.Err, failure) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestCompactorAdmissionPreservesBudgetStopReason(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("done")))
	a := newRuntimeAgent(f, func(o *AgentOptions) {
		o.Budget.MaxCostUSD = 1
		o.PrepareNextTurnE = func(ctx context.Context, _ ShouldStopAfterTurnContext) (*AgentLoopTurnUpdate, error) {
			if err := AccountProviderUsage(ctx, "compaction:receipt", "m", types.Usage{Cost: types.Cost{Total: 1.1}}); err != nil {
				return nil, err
			}
			release, err := AdmitProvider(ctx, f.Model())
			defer release()
			return nil, err
		}
	})
	result, err := a.PromptText(context.Background(), "go")
	if err != nil || result.StopReason != StopMaxCost || result.Err != nil || f.CallCount() != 1 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, f.CallCount())
	}
}
