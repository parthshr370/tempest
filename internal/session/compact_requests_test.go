package session

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/compaction"
	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/retry"
	"go.harness.dev/harness/internal/session/journal"
)

// costTolerance absorbs float rounding in summed dollar costs.
const costTolerance = 1e-9

// pricedSummaries answers every summary request with "summary" at a billed
// cost per request, and any other request with "done" at no cost. It counts
// the summary requests.
type pricedSummaries struct {
	mu       sync.Mutex
	requests int
	cost     float64
}

func (p *pricedSummaries) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests
}

func (p *pricedSummaries) Stream(_ context.Context, model types.Model, c types.Context, _ *types.SimpleStreamOptions) *stream.AssistantStream {
	text, cost := "done", 0.0
	if c.SystemPrompt == compaction.SummarizationSystemPrompt {
		p.mu.Lock()
		p.requests++
		p.mu.Unlock()
		text, cost = "summary", p.cost
	}
	response := stream.NewAssistantStream(model.API, model.Provider, model.ID)
	message := types.AssistantMessage{API: model.API, Provider: model.Provider, Model: model.ID, StopReason: types.StopStop, Content: []types.ContentBlock{types.NewText(text)}, Usage: types.Usage{Input: 1, TotalTokens: 1, Cost: types.Cost{Total: cost}}}
	response.Push(types.StreamEvent{Type: types.EvDone, Reason: types.StopStop, Message: &message})
	response.End()
	return response
}

// A summary written in many requests is held to the run's cost cap request
// by request: each request is admitted against what the earlier ones cost,
// so the cap stops the summary before the request that would spend past it,
// the run ends at max_cost, and every request it paid for is journaled.
func TestCompactionRequestsAreEachAdmittedAgainstTheCostCap(t *testing.T) {
	const (
		window, output = 4000, 1600
		historyBytes   = 800_000
		// The recent tail outweighs the window's keep-recent budget, so the
		// cut falls after the long message and only it is summarized.
		recentBytes = 4 * window
		costCap     = 0.50
		requestCost = 0.30
		// Admitted at $0.00 and $0.30; at $0.60 the cap is spent.
		admittedRequests = 2
	)
	ctx := context.Background()
	s := queueStore(t)
	provider := &pricedSummaries{cost: requestCost}
	model := types.Model{API: "priced-api", Provider: "priced", ID: "m", ContextWindow: window, MaxTokens: output}
	a := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{Model: model}, StreamFn: provider.Stream, SessionID: s.ID(), Budget: agent.Budget{MaxCostUSD: costCap}})
	r := newCompactionRunner(model, provider.Stream)
	r.configureJournal(s, nil, a)
	a.PrepareNextTurnE = r.hookE(a.SetMessages)
	AttachJournal(a, s)
	a.SetMessages([]types.Message{cUser(strings.Repeat("u", historyBytes)), cAssistant("ok"), cUser(strings.Repeat("r", recentBytes)), cAssistant("ok")})
	result, err := a.PromptText(ctx, "go")
	if err != nil {
		t.Fatal(err)
	}
	if requests := provider.count(); requests != admittedRequests {
		t.Fatalf("%d summary requests under a $%g cap at $%g each, want %d", requests, costCap, requestCost, admittedRequests)
	}
	charged := admittedRequests * requestCost
	if result.StopReason != agent.StopMaxCost || math.Abs(result.Usage.Cost.Total-charged) > costTolerance {
		t.Fatalf("run stopped %q having charged $%g, want max_cost at $%g (err %v)", result.StopReason, result.Usage.Cost.Total, charged, result.Err)
	}
	loaded, err := journal.Load(ctx, s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(loaded.UsageTotal.Cost.Total-charged) > costTolerance {
		t.Fatalf("journal charged $%g, want the $%g the requests cost", loaded.UsageTotal.Cost.Total, charged)
	}
	if len(loaded.CompactionParts) != 1 {
		t.Fatalf("%d compaction phases journaled, want 1", len(loaded.CompactionParts))
	}
	for _, part := range loaded.CompactionParts {
		if part.Finished == nil || part.Finished.Status != "error" || !strings.Contains(part.Finished.Error, string(agent.StopMaxCost)) {
			t.Fatalf("stopped phase recorded as %+v, want an error naming the cost cap", part.Finished)
		}
	}
}

// runCappedCompaction runs a prompt whose history must be summarized in
// several requests, under budget, with send answering every request.
func runCappedCompaction(t *testing.T, send agent.StreamFn, budget agent.Budget) agent.RunResult {
	t.Helper()
	s := queueStore(t)
	model := types.Model{API: "priced-api", Provider: "priced", ID: "m", ContextWindow: 4000, MaxTokens: 1600}
	a := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{Model: model}, StreamFn: send, SessionID: s.ID(), Budget: budget})
	r := newCompactionRunner(model, send)
	r.configureJournal(s, nil, a)
	a.PrepareNextTurnE = r.hookE(a.SetMessages)
	AttachJournal(a, s)
	a.SetMessages([]types.Message{cUser(strings.Repeat("u", 800_000)), cAssistant("ok"), cUser(strings.Repeat("r", 16_000)), cAssistant("ok")})
	result, err := a.PromptText(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// summaryUsage answers like pricedSummaries but gives each summary the usage
// usage(n) for the n-th summary request.
func summaryUsage(p *pricedSummaries, usage func(n int) types.Usage) agent.StreamFn {
	return func(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream {
		answer := p.Stream(ctx, model, c, opts)
		if c.SystemPrompt != compaction.SummarizationSystemPrompt {
			return answer
		}
		message, err := answer.Result(ctx)
		if err != nil {
			panic(err)
		}
		message.Usage = usage(p.count())
		out := stream.NewAssistantStream(model.API, model.Provider, model.ID)
		out.Push(types.StreamEvent{Type: types.EvDone, Reason: message.StopReason, Message: message})
		out.End()
		return out
	}
}

// Summary requests count against token caps like turns: each is admitted
// against the tokens the earlier ones used, and one that reports no tokens
// fails a token-capped run before another request is sent. Under a cost cap,
// a summary of unknown price fails the run the same way.
func TestCompactionRequestsAreHeldToTokenCapsAndMeasured(t *testing.T) {
	t.Run("measured", func(t *testing.T) {
		p := &pricedSummaries{}
		result := runCappedCompaction(t, p.Stream, agent.Budget{MaxInputTokens: 2})
		if result.StopReason != agent.StopMaxInputTokens || p.count() != 2 || result.Usage.Input != 2 {
			t.Fatalf("%d summary requests, result %+v; want max_input_tokens after 2", p.count(), result)
		}
	})
	t.Run("no usage under a token cap", func(t *testing.T) {
		p := &pricedSummaries{}
		result := runCappedCompaction(t, summaryUsage(p, func(int) types.Usage { return types.Usage{} }), agent.Budget{MaxInputTokens: 100})
		if !errors.Is(result.Err, agent.ErrUnknownUsage) || p.count() != 1 {
			t.Fatalf("%d summary requests, result %+v; want ErrUnknownUsage after 1", p.count(), result)
		}
	})
	t.Run("unknown price under a cost cap", func(t *testing.T) {
		p := &pricedSummaries{}
		unpriced := func(int) types.Usage { return types.Usage{Input: 1, TotalTokens: 1, Cost: types.Cost{Unknown: true}} }
		result := runCappedCompaction(t, summaryUsage(p, unpriced), agent.Budget{MaxCostUSD: 1})
		if !errors.Is(result.Err, agent.ErrUnknownPrice) || p.count() != 1 {
			t.Fatalf("%d summary requests, result %+v; want ErrUnknownPrice after 1", p.count(), result)
		}
	})
}

// failingSummaries fails the first summary attempt after it reported 100
// prompt tokens, the way a dropped stream is billed, and answers the rest.
type failingSummaries struct{ pricedSummaries }

func (f *failingSummaries) Stream(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream {
	answer := f.pricedSummaries.Stream(ctx, model, c, opts)
	if c.SystemPrompt != compaction.SummarizationSystemPrompt || f.count() > 1 {
		return answer
	}
	out := stream.NewAssistantStream(model.API, model.Provider, model.ID)
	out.Push(types.StreamEvent{Type: types.EvStart})
	out.Push(types.StreamEvent{Type: types.EvError, Reason: types.StopError, Err: &types.AssistantMessage{API: model.API, Provider: model.Provider, Model: model.ID, StopReason: types.StopError, ErrorCode: string(retry.CodeStreamInterrupted), ErrorMessage: "stream dropped", Usage: types.Usage{Input: 100, TotalTokens: 100}}})
	out.End()
	return out
}

// A summary request is retried only while the run is under its caps: when
// the failed attempt's usage reached a cap, the run stops there and the
// attempt's usage is journaled.
func TestCompactionRetryIsReadmittedAfterAFailedAttempt(t *testing.T) {
	for _, test := range []struct {
		name     string
		cap      int
		wantStop agent.StopReason
		wantSent int
	}{
		{"cap reached", 100, agent.StopMaxInputTokens, 1},
		{"under the cap", 102, agent.StopMaxInputTokens, 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := &failingSummaries{}
			policy := retry.DefaultConfig()
			policy.Sleeper = noRetryDelay{}
			retrier, err := retry.New(retry.StreamFunc(f.Stream), policy)
			if err != nil {
				t.Fatal(err)
			}
			result := runCappedCompaction(t, retrier.Stream, agent.Budget{MaxInputTokens: test.cap})
			if result.StopReason != test.wantStop || f.count() != test.wantSent {
				t.Fatalf("%d summary attempts, result %+v; want %q after %d", f.count(), result, test.wantStop, test.wantSent)
			}
			if want := 100 + test.wantSent - 1; result.Usage.Input != want {
				t.Fatalf("input = %d, want %d", result.Usage.Input, want)
			}
		})
	}
}

// A summary request that falls back to a backup with a smaller window is
// sent to the backup sized for its window, and the summary's later requests
// stay on the backup instead of trying the primary again.
func TestCompactionFallbackMidSummaryStaysOnTheBackupAndFitsIt(t *testing.T) {
	const (
		primaryWindow, backupWindow = 8000, 4000
		backupOutput                = 500
		turns, turnChars            = 12, 4000
		// More answers than the summary can ask for.
		replies = 64
	)
	primary, backup := newStatusProvider("primary", "p1", http.StatusServiceUnavailable), newStatusProvider("backup", "b1", 0)
	policy := retry.DefaultConfig()
	policy.MaxAttempts = fallbackAttempts
	policy.Sleeper = noRetryDelay{}
	primaryRetrier, err := retry.New(primary, policy)
	if err != nil {
		t.Fatal(err)
	}
	backupRetrier, err := retry.New(backup, policy)
	if err != nil {
		t.Fatal(err)
	}
	primaryModel, backupModel := primary.faux.Model(), backup.faux.Model()
	primaryModel.ContextWindow = primaryWindow
	backupModel.ContextWindow, backupModel.MaxTokens = backupWindow, backupOutput
	chain, err := retry.NewFallback([]retry.Candidate{{Model: primaryModel, Stream: primaryRetrier}, {Model: backupModel, Stream: backupRetrier}})
	if err != nil {
		t.Fatal(err)
	}
	var history []types.Message
	for turn := range turns {
		history = append(history, cUser(fmt.Sprintf("ask %d %s", turn, strings.Repeat("u", turnChars))), cAssistant(strings.Repeat("a", turnChars)))
	}
	history = append(history, cUser("keep recent"))
	var mu sync.Mutex
	var sizes []int
	step := func(c types.Context, opts *types.StreamOptions, state faux.State, model types.Model) (types.AssistantMessage, error) {
		mu.Lock()
		defer mu.Unlock()
		size := compaction.EstimateContextTokens(c.Messages).Tokens + (len(c.SystemPrompt)+3)/4
		if opts != nil {
			size += opts.MaxTokens
		}
		sizes = append(sizes, size)
		return faux.Respond(faux.Text("summary"))(c, opts, state, model)
	}
	steps := make([]faux.ResponseStep, replies)
	for i := range steps {
		steps[i] = step
	}
	backup.faux.SetResponses(steps...)
	r := newCompactionRunner(primaryModel, chain.Stream)
	r.backups = []types.Model{backupModel}
	if _, changed, err := r.compact(context.Background(), history, primaryModel); err != nil || !changed {
		t.Fatalf("compaction changed=%t err=%v", changed, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sizes) < 2 {
		t.Fatalf("backup answered %d summary requests, want the summary written on it in several", len(sizes))
	}
	for _, size := range sizes {
		if size > backupWindow {
			t.Fatalf("backup summary requests of %v estimated tokens, want each within its %d-token window", sizes, backupWindow)
		}
	}
	if calls := primary.callCount(); calls != fallbackAttempts {
		t.Fatalf("primary called %d times, want only the %d attempts before the summary switched", calls, fallbackAttempts)
	}
}
