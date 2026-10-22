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
	pending bool
	start   func(context.Context, work.RunInfo) error
	idle    func(context.Context) (bool, error)
	finish  func(context.Context, string) error
}

func (h testRunHooks) Pending() bool { return h.pending }

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

func TestWorkMaxTurnsDistinguishesQuiescenceFromPendingWork(t *testing.T) {
	for _, test := range []struct {
		name      string
		steps     []faux.ResponseStep
		maxTurns  int
		followUp  bool
		liveWork  bool
		idleError bool
		want      StopReason
	}{
		{name: "final text", steps: []faux.ResponseStep{faux.Respond(faux.Text("OK"))}, maxTurns: 1, want: StopCompleted},
		{name: "tool then final text", steps: []faux.ResponseStep{toolCallStep("one"), faux.Respond(faux.Text("OK"))}, maxTurns: 2, want: StopCompleted},
		{name: "tool needs another turn", steps: []faux.ResponseStep{toolCallStep("one")}, maxTurns: 1, want: StopMaxTurns},
		{name: "follow-up needs another turn", steps: []faux.ResponseStep{faux.Respond(faux.Text("OK"))}, maxTurns: 1, followUp: true, want: StopMaxTurns},
		{name: "live work at cap", steps: []faux.ResponseStep{faux.Respond(faux.Text("OK"))}, maxTurns: 1, liveWork: true, want: StopMaxTurns},
		{name: "idle failure at cap", steps: []faux.ResponseStep{faux.Respond(faux.Text("OK"))}, maxTurns: 1, idleError: true, want: StopError},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := faux.New(faux.Options{})
			f.SetResponses(test.steps...)
			queue := &testWorkQueue{}
			idleCalls := 0
			failure := errors.New("idle failed")
			hooks := testRunHooks{idle: func(ctx context.Context) (bool, error) {
				idleCalls++
				if test.idleError {
					return false, failure
				}
				if test.liveWork {
					if ctx.Err() == nil {
						return false, errors.New("waited for work beyond the turn cap")
					}
					return false, ctx.Err()
				}
				if test.followUp {
					queue.messages = append(queue.messages, work.QueuedMessage{ID: "next", Message: userMessage("next")})
					return true, nil
				}
				return false, nil
			}}
			a := newRuntimeAgent(f, func(o *AgentOptions) {
				o.Work, o.QueueSource = hooks, queue
				o.Budget.MaxTurns = test.maxTurns
				o.InitialState.Tools = []AgentTool{okTool("work")}
			})
			result, err := a.PromptText(context.Background(), "go")
			if err != nil || result.StopReason != test.want || result.Turns != test.maxTurns || f.CallCount() != test.maxTurns {
				t.Fatalf("result=%+v err=%v calls=%d", result, err, f.CallCount())
			}
			if test.want == StopCompleted && idleCalls != 1 {
				t.Fatalf("idle calls=%d, want atomic quiescence check", idleCalls)
			}
			if test.idleError && !errors.Is(result.Err, failure) {
				t.Fatalf("idle error=%v, want %v", result.Err, failure)
			}
		})
	}
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

func TestContinueQuiescentWorkWaitsForCompletion(t *testing.T) {
	for _, delayed := range []bool{false, true} {
		t.Run(map[bool]string{false: "undelivered", true: "recovering"}[delayed], func(t *testing.T) {
			f := faux.New(faux.Options{})
			f.SetResponses(faux.Respond(faux.Text("recovered")))
			var a *Agent
			queue := &testWorkQueue{}
			started, delivered := false, false
			deliver := func() {
				delivered = true
				queue.messages = append(queue.messages, work.QueuedMessage{ID: "completion", Message: userMessage("job completion")})
			}
			hooks := testRunHooks{
				pending: true,
				start: func(_ context.Context, info work.RunInfo) error {
					if !info.Resume {
						return errors.New("work recovery requires Continue")
					}
					started = true
					if !delayed {
						deliver()
					}
					return nil
				},
				idle: func(context.Context) (bool, error) {
					if !delivered {
						if f.CallCount() != 0 {
							return false, errors.New("provider called before recovered completion")
						}
						deliver()
						return true, nil
					}
					return false, nil
				},
			}
			a = newRuntimeAgent(f, func(o *AgentOptions) { o.Work = hooks; o.QueueSource = queue })
			a.SetMessages([]types.Message{types.AssistantMessage{Content: []types.ContentBlock{types.NewText("root quiescent")}}})
			result, err := a.Continue(context.Background())
			if err != nil || result.Err != nil || result.StopReason != StopCompleted || !started || !delivered || f.CallCount() != 1 {
				t.Fatalf("result=%+v err=%v started=%v delivered=%v calls=%d", result, err, started, delivered, f.CallCount())
			}
			if got := runtimeRoles(a.State().Messages); got != "assistant,user,assistant" {
				t.Fatalf("roles=%s", got)
			}
		})
	}
}

func TestContinueDurableQueueWithoutWorkStillRejectsQuiescence(t *testing.T) {
	f := faux.New(faux.Options{})
	a := newRuntimeAgent(f, func(o *AgentOptions) {
		o.QueueSource = &testWorkQueue{}
		o.Work = testRunHooks{}
	})
	a.SetMessages([]types.Message{types.AssistantMessage{}})
	if _, err := a.Continue(context.Background()); err == nil || err.Error() != "Cannot continue without pending input" {
		t.Fatalf("Continue error=%v", err)
	}
	if f.CallCount() != 0 {
		t.Fatal("idle Continue called the provider")
	}
}

type testWorkQueue struct {
	messages []work.QueuedMessage
}

func (q *testWorkQueue) Pending() (bool, <-chan struct{}) { return len(q.messages) > 0, nil }
func (q *testWorkQueue) Take(_ context.Context, kind work.QueueKind) ([]work.QueuedMessage, error) {
	if kind != work.FollowUp {
		return nil, nil
	}
	messages := q.messages
	q.messages = nil
	return messages, nil
}
