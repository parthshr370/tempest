package session

import (
	"context"
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
