package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"go.harness.dev/harness/internal/agent"
	types "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/work"
)

func queueStore(t *testing.T) *journal.Store {
	t.Helper()
	s, err := journal.Create(context.Background(), t.TempDir(), "/tmp/queues", journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func TestDurableQueueAckRetryCancelAndClaims(t *testing.T) {
	ctx := context.Background()
	s := queueStore(t)
	q := NewDurableQueues(s, nil, nil)
	first, err := q.Enqueue(ctx, work.QueueInput{Kind: work.Steer, Text: "first", Key: "stable"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := q.Enqueue(ctx, work.QueueInput{Kind: work.Steer, Text: "first", Key: "stable"})
	if err != nil || again != first {
		t.Fatalf("retry=%+v %v", again, err)
	}
	if _, err = q.Enqueue(ctx, work.QueueInput{Kind: work.FollowUp, Text: "changed", Key: "stable"}); !errors.Is(err, ErrQueueConflict) {
		t.Fatalf("conflict=%v", err)
	}
	claimCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	taken, err := q.Take(claimCtx, work.Steer)
	if err != nil || len(taken) != 1 || taken[0].ID != first.ID {
		t.Fatalf("take=%+v %v", taken, err)
	}
	if err = q.Cancel(ctx, first.ID); !errors.Is(err, ErrQueueTooLate) {
		t.Fatalf("claimed cancel=%v", err)
	}
	loaded, err := journal.Load(ctx, s.Path())
	if err != nil || len(loaded.Queued) != 1 {
		t.Fatalf("claim must survive crash=%+v %v", loaded, err)
	}
	restored := NewDurableQueues(s, loaded, nil)
	if err = restored.Cancel(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if err = restored.Cancel(ctx, first.ID); err != nil {
		t.Fatal("cancel retry", err)
	}
	receipt, err := restored.Enqueue(ctx, work.QueueInput{Kind: work.Steer, Text: "first", Key: "stable"})
	if err != nil || receipt != first || len(restored.Snapshot()) != 0 {
		t.Fatal("cancelled key lost", receipt, err)
	}
	if err = restored.Cancel(ctx, "unknown"); !errors.Is(err, ErrQueueNotFound) {
		t.Fatal(err)
	}
}
func TestDurableQueueConcurrentKeyAndBounds(t *testing.T) {
	s := queueStore(t)
	q := NewDurableQueues(s, nil, nil)
	ctx := context.Background()
	var wg sync.WaitGroup
	ids := make(chan string, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			item, err := q.Enqueue(ctx, work.QueueInput{Kind: work.Steer, Text: "same", Key: "key"})
			if err != nil {
				t.Error(err)
				return
			}
			ids <- item.ID
		}()
	}
	wg.Wait()
	close(ids)
	id := ""
	for next := range ids {
		if id != "" && id != next {
			t.Fatal("duplicate acceptance")
		}
		id = next
	}
	for i := 1; i < 32; i++ {
		if _, err := q.Enqueue(ctx, work.QueueInput{Kind: work.FollowUp, Text: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.Enqueue(ctx, work.QueueInput{Kind: work.Steer, Text: "full"}); !errors.Is(err, ErrQueueFull) {
		t.Fatal("32 item cap", err)
	}
	if _, err := q.Enqueue(ctx, work.QueueInput{Kind: work.FollowUp, Text: "job job_one completed; use wait", Key: "job:job_one:terminal", SourceJobID: "job_one"}); err != nil {
		t.Fatal("reserved completion credit", err)
	}
	if got := q.PendingJobIDs(); len(got) != 1 || got[0] != "job_one" {
		t.Fatalf("completion credits=%v", got)
	}
	for _, input := range []work.QueueInput{{Kind: work.Steer, Text: " "}, {Kind: work.Steer, Text: strings.Repeat("a", 16385)}, {Kind: work.Steer, Text: "okay", Key: "job:external"}, {Kind: work.Steer, Text: "\xff"}, {Kind: "other", Text: "text"}} {
		if _, err := q.Enqueue(ctx, input); !errors.Is(err, ErrInvalidQueueInput) {
			t.Fatalf("invalid input=%+v err=%v", input, err)
		}
	}
}
func TestDurableQueueEncodedByteCap(t *testing.T) {
	s := queueStore(t)
	q := NewDurableQueues(s, nil, nil)
	input := work.QueueInput{Kind: work.Steer, Text: strings.Repeat("\x01", 16<<10)}
	accepted := 0
	for range 32 {
		_, err := q.Enqueue(context.Background(), input)
		if errors.Is(err, ErrQueueFull) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		accepted++
	}
	if accepted == 0 || accepted >= 32 {
		t.Fatalf("encoded cap ignored: %d", accepted)
	}
}
func TestDurableQueueContinueEmptyConsumesOnce(t *testing.T) {
	ctx := context.Background()
	s := queueStore(t)
	q := NewDurableQueues(s, nil, nil)
	item, err := q.Enqueue(ctx, work.QueueInput{Kind: work.FollowUp, Text: "durable prompt", Key: "prompt"})
	if err != nil {
		t.Fatal(err)
	}
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("answer")))
	a := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{Model: f.Model()}, StreamFn: f.StreamSimple, QueueSource: q, SessionID: s.ID()})
	detach := AttachJournalWithOptions(a, s, RecordOptions{Queues: q})
	defer detach()
	a.Subscribe(func(ctx context.Context, event agent.AgentEvent) error {
		if event.Type == agent.EventMessageEnd && event.QueueID != "" {
			l, err := journal.Load(ctx, s.Path())
			if err != nil {
				return err
			}
			if !l.QueueConsumed[item.ID] {
				return errors.New("publication before consumption commit")
			}
		}
		return nil
	})
	result, err := a.Continue(ctx)
	if err != nil || result.Err != nil || result.StopReason != agent.StopCompleted {
		t.Fatalf("continue=%+v %v", result, err)
	}
	if len(q.Snapshot()) != 0 {
		t.Fatal("not acknowledged")
	}
	if err = q.Cancel(ctx, item.ID); !errors.Is(err, ErrQueueTooLate) {
		t.Fatal(err)
	}
	l, err := journal.Load(ctx, s.Path())
	if err != nil || len(l.Queued) != 0 || !l.QueueConsumed[item.ID] {
		t.Fatalf("reload=%+v %v", l, err)
	}
	if again, err := q.Enqueue(ctx, work.QueueInput{Kind: work.FollowUp, Text: "durable prompt", Key: "prompt"}); err != nil || again.ID != item.ID || len(q.Snapshot()) != 0 {
		t.Fatal("consumed key not retained", err)
	}
}
func TestCanonicalInvocationDuplicateRawIDs(t *testing.T) {
	s := queueStore(t)
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.ToolCall("work", json.RawMessage(`{}`), "duplicate"), faux.ToolCall("work", json.RawMessage(`{}`), "duplicate")), faux.Respond(faux.ToolCall("work", json.RawMessage(`{}`), "duplicate")), faux.Respond(faux.Text("done")))
	var mu sync.Mutex
	var invocations []work.Invocation
	tool := agent.AgentTool{Tool: types.Tool{Name: "work", Parameters: json.RawMessage(`{"type":"object"}`)}, Execute: func(ctx context.Context, id string, args json.RawMessage, update agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
		inv, ok := work.InvocationFromContext(ctx)
		if !ok {
			return agent.AgentToolResult{}, errors.New("missing durable invocation")
		}
		mu.Lock()
		invocations = append(invocations, inv)
		mu.Unlock()
		return agent.AgentToolResult{}, nil
	}}
	a := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{Model: f.Model(), Tools: []agent.AgentTool{tool}}, StreamFn: f.StreamSimple, SessionID: s.ID()})
	AttachJournal(a, s)
	result, err := a.PromptText(context.Background(), "go")
	if err != nil || result.Err != nil {
		t.Fatal(result, err)
	}
	if len(invocations) != 3 {
		t.Fatalf("invocations=%+v", invocations)
	}
	seen := map[string]bool{}
	for _, inv := range invocations {
		if inv.CallID == "" || seen[inv.CallID] || inv.ToolCallID != "duplicate" {
			t.Fatal("colliding invocation", inv)
		}
		seen[inv.CallID] = true
	}
}

func TestCancellationDoesNotUnclaimBeforeRunBoundary(t *testing.T) {
	ctx := context.Background()
	s := queueStore(t)
	q := NewDurableQueues(s, nil, nil)
	item, err := q.Enqueue(ctx, work.QueueInput{Kind: work.Steer, Text: "accepted"})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	taken, err := q.Take(runCtx, work.Steer)
	if err != nil || len(taken) != 1 {
		t.Fatal(taken, err)
	}
	cancel()
	if err := q.Cancel(ctx, item.ID); !errors.Is(err, ErrQueueTooLate) {
		t.Fatalf("cancel after claim=%v", err)
	}
	if err := s.Append(ctx, journal.MessageEntry{QueueID: item.ID, Message: taken[0].Message}); err != nil {
		t.Fatal(err)
	}
	if err := q.Acknowledge(item.ID); err != nil {
		t.Fatal(err)
	}
	q.ReleaseClaims()
	loaded, err := journal.Load(ctx, s.Path())
	if err != nil || !loaded.QueueConsumed[item.ID] {
		t.Fatal("claim cancellation corrupted consumption", err)
	}
}

func TestRecorderDeduplicatesEqualOptionalUsageValues(t *testing.T) {
	ctx := context.Background()
	s := queueStore(t)
	if err := s.Append(ctx, journal.RunStartedRecord{Meta: journal.Meta{RunID: "root"}, Model: "m"}); err != nil {
		t.Fatal(err)
	}
	r := &recorder{store: s, runID: "root", sources: map[string]types.Usage{}}
	first := types.Usage{Input: 4, TotalTokens: 4, CacheWrite1h: new(2), Reasoning: new(3)}
	second := types.Usage{Input: 4, TotalTokens: 4, CacheWrite1h: new(2), Reasoning: new(3)}
	for _, usage := range []types.Usage{first, second} {
		if err := r.record(ctx, agent.AgentEvent{Type: agent.EventUsage, Model: "child", UsageSource: "child:source", RunResult: agent.RunResult{Usage: usage}}); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := journal.Load(ctx, s.Path())
	if err != nil || loaded.UsageTotal.TotalTokens != 4 {
		t.Fatal("duplicate receipt charged", loaded, err)
	}
}
