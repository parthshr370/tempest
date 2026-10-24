package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/compaction"
	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
)

func cUser(text string) ptypes.UserMessage {
	return ptypes.UserMessage{Content: ptypes.StringContent(text)}
}

func cAssistant(text string) ptypes.AssistantMessage {
	return ptypes.AssistantMessage{Content: []ptypes.ContentBlock{ptypes.NewText(text)}}
}

// summaryMarker distinguishes summarizer output from ordinary turn output.
const summaryMarker = "SUMMARY-CHECKPOINT"

// smartStep returns a faux step that answers summarization requests (identified
// by the summarization system prompt) with a marker, and every other turn with a
// large body so history grows past the tuned window.
func smartStep() faux.ResponseStep {
	return func(c ptypes.Context, _ *ptypes.StreamOptions, _ faux.State, _ ptypes.Model) (ptypes.AssistantMessage, error) {
		if c.SystemPrompt == compaction.SummarizationSystemPrompt {
			return cAssistant(summaryMarker), nil
		}
		return cAssistant(strings.Repeat("X ", 300)), nil
	}
}

func smartSteps(n int) []faux.ResponseStep {
	steps := make([]faux.ResponseStep, n)
	for i := range steps {
		steps[i] = smartStep()
	}
	return steps
}

func noConsecutiveUsers(t *testing.T, msgs []ptypes.Message) {
	t.Helper()
	prev := ""
	for i, m := range msgs {
		role := m.Role()
		if role == "user" && prev == "user" {
			t.Fatalf("consecutive user messages at index %d", i)
		}
		prev = role
	}
}

func containsMarker(msgs []ptypes.Message, marker string) bool {
	for _, m := range msgs {
		switch v := m.(type) {
		case ptypes.UserMessage:
			if v.Content.IsBlocks() {
				for _, b := range v.Content.Blocks {
					if strings.Contains(b.Text, marker) {
						return true
					}
				}
			} else if strings.Contains(v.Content.Text, marker) {
				return true
			}
		case ptypes.AssistantMessage:
			for _, b := range v.Content {
				if strings.Contains(b.Text, marker) {
					return true
				}
			}
		}
	}
	return false
}

func TestMaybeCompactUnderBudgetNoop(t *testing.T) {
	f := faux.New(faux.Options{})
	r := &compactionRunner{contextWindow: 200000, model: f.Model(), streamFn: f.StreamSimple, settings: compaction.DefaultCompactionSettings}
	msgs := []ptypes.Message{cUser("hi"), cAssistant("hello")}

	out, ok := r.maybeCompact(msgs)
	if ok {
		t.Fatal("should not compact well under the window")
	}
	if len(out) != len(msgs) {
		t.Fatalf("len(out) = %d, want %d unchanged", len(out), len(msgs))
	}
}

func TestMaybeCompactSummarizesOverBudget(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(smartSteps(4)...)
	r := &compactionRunner{
		contextWindow: 100,
		model:         f.Model(),
		streamFn:      f.StreamSimple,
		settings:      compaction.CompactionSettings{Enabled: true, ReserveTokens: 10, KeepRecentTokens: 10},
	}
	msgs := []ptypes.Message{
		cUser(strings.Repeat("A", 400)),
		cAssistant(strings.Repeat("B", 400)),
		cUser(strings.Repeat("C", 400)),
		cAssistant(strings.Repeat("D", 400)),
		cUser("keep this recent ask"),
	}

	out, ok := r.maybeCompact(msgs)
	if !ok {
		t.Fatal("expected compaction over budget")
	}
	if len(out) >= len(msgs) {
		t.Fatalf("expected fewer messages after compaction, got %d >= %d", len(out), len(msgs))
	}
	noConsecutiveUsers(t, out)
	if !containsMarker(out, summaryMarker) {
		t.Fatal("compacted history missing the summary")
	}
}

// The hook must persist the compacted history to the agent so a later prompt
// starts from the summary, not the full transcript, and never yields two
// consecutive user turns.
func TestCompactionPersistsAcrossPrompts(t *testing.T) {
	f := faux.New(faux.Options{})
	model := f.Model()
	model.ContextWindow = 100

	// Record, for each ordinary (non-summary) turn request, whether its context
	// already carried the persisted summary. This proves prompt 2's provider
	// request started from the compacted history, not the full transcript.
	var mu sync.Mutex
	var turnSawMarker []bool
	step := func(c ptypes.Context, _ *ptypes.StreamOptions, _ faux.State, _ ptypes.Model) (ptypes.AssistantMessage, error) {
		if c.SystemPrompt == compaction.SummarizationSystemPrompt {
			return cAssistant(summaryMarker), nil
		}
		saw := false
		for _, m := range c.Messages {
			if containsMarker([]ptypes.Message{m}, summaryMarker) {
				saw = true
				break
			}
		}
		mu.Lock()
		turnSawMarker = append(turnSawMarker, saw)
		mu.Unlock()
		return cAssistant(strings.Repeat("X ", 300)), nil
	}
	steps := make([]faux.ResponseStep, 16)
	for i := range steps {
		steps[i] = step
	}
	f.SetResponses(steps...)

	r := &compactionRunner{
		contextWindow: model.ContextWindow,
		model:         model,
		streamFn:      f.StreamSimple,
		settings:      compaction.CompactionSettings{Enabled: true, ReserveTokens: 10, KeepRecentTokens: 10},
	}
	var agt *agent.Agent
	agt = agent.NewAgent(agent.AgentOptions{
		InitialState:               &agent.AgentState{Model: model},
		StreamFn:                   f.StreamSimple,
		PrepareNextTurnWithContext: r.hook(func(m []ptypes.Message) { agt.SetMessages(m) }),
	})

	ctx := context.Background()
	if _, err := agt.PromptText(ctx, strings.Repeat("first ", 200)); err != nil {
		t.Fatal(err)
	}
	if _, err := agt.PromptText(ctx, strings.Repeat("second ", 200)); err != nil {
		t.Fatal(err)
	}

	msgs := agt.State().Messages
	if !containsMarker(msgs, summaryMarker) {
		t.Fatalf("no persisted summary in agent state after prompts (%d msgs)", len(msgs))
	}
	noConsecutiveUsers(t, msgs)

	mu.Lock()
	defer mu.Unlock()
	if len(turnSawMarker) < 2 {
		t.Fatalf("expected at least 2 turn requests, got %d", len(turnSawMarker))
	}
	if turnSawMarker[0] {
		t.Fatal("first turn request should predate any summary")
	}
	if !turnSawMarker[len(turnSawMarker)-1] {
		t.Fatal("second prompt's request did not start from the persisted summary")
	}
}

func TestExtensionCompactionHookOnlyRunsAtActualBoundary(t *testing.T) {
	provider := faux.New(faux.Options{})
	refused := errors.New("compaction refused")
	calls := 0
	runner := &compactionRunner{contextWindow: 200000, model: provider.Model(), streamFn: provider.StreamSimple, settings: compaction.DefaultCompactionSettings, beforeCompaction: func(context.Context, string) error { calls++; return refused }}
	if _, changed, err := runner.compact(context.Background(), []ptypes.Message{cUser("short")}); err != nil || changed || calls != 0 {
		t.Fatalf("hook ran without compaction: %t %d %v", changed, calls, err)
	}
	runner.contextWindow = 100
	runner.settings = compaction.CompactionSettings{Enabled: true, ReserveTokens: 10, KeepRecentTokens: 10}
	messages := []ptypes.Message{cUser(strings.Repeat("A", 400)), cAssistant(strings.Repeat("B", 400)), cUser("recent")}
	if _, _, err := runner.compact(context.Background(), messages); !errors.Is(err, refused) || calls != 1 || provider.CallCount() != 0 {
		t.Fatalf("unapproved summary sent: calls=%d provider=%d err=%v", calls, provider.CallCount(), err)
	}
}
