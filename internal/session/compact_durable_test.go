package session

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/compaction"
	types "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/work"
)

func TestCompactionPaidPhaseReusedAndUnknownNeverRetried(t *testing.T) {
	ctx := context.Background()
	s := queueStore(t)
	a := agent.NewAgent(agent.AgentOptions{})
	a.SetRunState(agent.RunState{RunID: "root", LogicalRunID: "root"})
	if err := s.Append(ctx, journal.RunStartedRecord{Meta: journal.Meta{RunID: "root"}, Model: "m"}); err != nil {
		t.Fatal(err)
	}
	f := faux.New(faux.Options{})
	r := newCompactionRunner(f.Model(), f.StreamSimple)
	r.configureJournal(s, nil, a)
	paid := 0
	call := func(context.Context) (compaction.SummaryResult, error) {
		paid++
		return compaction.SummaryResult{Text: "cached paid history", Usage: types.Usage{Input: 9, TotalTokens: 9}}, nil
	}
	source, preparation := strings.Repeat("a", 64), strings.Repeat("b", 64)
	text, err := r.phase(ctx, "cmp_reuse", source, preparation, "history", call)
	if err != nil || text != "cached paid history" {
		t.Fatal(text, err)
	}
	loaded, err := journal.Load(ctx, s.Path())
	if err != nil {
		t.Fatal(err)
	}
	resumed := newCompactionRunner(f.Model(), f.StreamSimple)
	resumed.configureJournal(s, loaded, a)
	if _, err := resumed.phase(ctx, "cmp_reuse", source, preparation, "history", call); err != nil || paid != 1 {
		t.Fatal("paid phase requested twice", paid, err)
	}
	if _, err := resumed.phase(ctx, "cmp_reuse", source, preparation, "prefix", call); err != nil || paid != 2 {
		t.Fatal("resume did not pay only missing prefix", paid, err)
	}
	source = strings.Repeat("c", 64)
	started := journal.CompactionCall{CompactionID: "cmp_unknown", SourceHash: source, Phase: "prefix", Model: f.Model().ID, PreparationHash: preparation, Settings: json.RawMessage(`{}`)}
	raw, _ := json.Marshal(started)
	if err := s.AppendWorkFact(ctx, work.Fact{Kind: "compaction_call_started", RunID: "root", Payload: raw}); err != nil {
		t.Fatal(err)
	}
	loaded, err = journal.Load(ctx, s.Path())
	if err != nil {
		t.Fatal(err)
	}
	resumed.configureJournal(s, loaded, a)
	if _, err := resumed.phase(ctx, "cmp_unknown", source, preparation, "prefix", call); err == nil || !strings.Contains(err.Error(), "outcome_unknown") || paid != 2 {
		t.Fatal("unknown phase retried", paid, err)
	}
	if loaded.UsageTotal.TotalTokens != 18 {
		t.Fatal("phase usage not durable", loaded.UsageTotal)
	}
}
func TestCompactionSnapshotMatchesLiveProjection(t *testing.T) {
	s := queueStore(t)
	f := faux.New(faux.Options{})
	f.SetResponses(smartSteps(8)...)
	model := f.Model()
	model.ContextWindow = 100
	r := newCompactionRunner(model, f.StreamSimple)
	r.settings = compaction.CompactionSettings{Enabled: true, ReserveTokens: 10, KeepRecentTokens: 10}
	a := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{Model: model}, StreamFn: f.StreamSimple, SessionID: s.ID()})
	r.configureJournal(s, nil, a)
	commits := 0
	a.PrepareNextTurnE = r.hookE(func(messages []types.Message) { commits++; a.SetMessages(messages) })
	AttachJournal(a, s)
	result, err := a.PromptText(context.Background(), strings.Repeat("first ", 200))
	if err != nil || result.Err != nil {
		t.Fatal(result, err)
	}
	loaded, err := journal.Load(context.Background(), s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if commits != 1 || !reflect.DeepEqual(a.State().Messages, loaded.Messages) {
		live, _ := json.Marshal(a.State().Messages)
		replay, _ := json.Marshal(loaded.Messages)
		t.Fatalf("commits=%d live=%s replay=%s", commits, live, replay)
	}
	if len(loaded.CompactionParts) == 0 || loaded.UsageTotal.TotalTokens != result.Usage.TotalTokens {
		t.Fatalf("compaction unmetered loaded=%+v result=%+v", loaded.UsageTotal, result.Usage)
	}
}

func TestResumeUnknownCompactionStopsBeforeProvider(t *testing.T) {
	ctx := context.Background()
	s := queueStore(t)
	f := faux.New(faux.Options{})
	model := f.Model()
	model.ContextWindow = 100
	r := newCompactionRunner(model, f.StreamSimple)
	r.settings = compaction.CompactionSettings{Enabled: true, ReserveTokens: 10, KeepRecentTokens: 10}
	messages := []types.Message{cUser(strings.Repeat("a", 400)), cAssistant(strings.Repeat("b", 400)), cUser(strings.Repeat("c", 400)), cAssistant(strings.Repeat("d", 400)), cUser("keep recent")}
	prep := compaction.PrepareCompaction(messagesToEntries(messages), r.settings)
	if prep == nil || len(prep.MessagesToSummarize) == 0 {
		t.Fatal("fixture does not prepare history")
	}
	source, err := stableHash(struct {
		Messages []types.Message
		Model    string
		Settings compaction.CompactionSettings
	}{messages, model.ID, r.settings})
	if err != nil {
		t.Fatal(err)
	}
	preparation, err := stableHash(prep)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(ctx, journal.RunStartedRecord{Meta: journal.Meta{RunID: "interrupted"}, Model: model.ID}); err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if err := s.Append(ctx, journal.MessageEntry{Meta: journal.Meta{RunID: "interrupted"}, Message: message}); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := json.Marshal(journal.CompactionCall{CompactionID: "cmp_" + source[:32], SourceHash: source, Phase: "history", Model: model.ID, Settings: json.RawMessage(`{}`), PreparationHash: preparation})
	if err := s.AppendWorkFact(ctx, work.Fact{Kind: "compaction_call_started", RunID: "interrupted", Payload: raw}); err != nil {
		t.Fatal(err)
	}
	loaded, err := journal.Load(ctx, s.Path())
	if err != nil {
		t.Fatal(err)
	}
	a := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{Model: model, Messages: loaded.Messages}, StreamFn: f.StreamSimple, SessionID: s.ID()})
	r.configureJournal(s, loaded, a)
	a.PrepareNextTurnE = r.hookE(a.SetMessages)
	AttachJournal(a, s)
	result, err := a.Continue(ctx)
	if err != nil || result.StopReason != agent.StopError || result.Err == nil || !strings.Contains(result.Err.Error(), "outcome_unknown") || result.Turns != 0 || f.CallCount() != 0 {
		t.Fatalf("resume=%+v err=%v calls=%d", result, err, f.CallCount())
	}
}
