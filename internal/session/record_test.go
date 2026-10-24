package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/tempest/durable"
)

func TestChildUsageIsJournaledWithoutChildMessages(t *testing.T) {
	ctx := context.Background()
	store, err := journal.Create(ctx, t.TempDir(), "/workspace/project", journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runID := store.NewRunID()
	if err := store.Append(ctx, journal.RunStartedRecord{Meta: journal.Meta{RunID: runID}, Model: "parent"}); err != nil {
		t.Fatal(err)
	}
	r := recorder{store: store, runID: runID}
	usage := types.Usage{Input: 3, TotalTokens: 3, Cost: types.Cost{Unknown: true}}
	result := agent.RunResult{Usage: usage, StopReason: agent.StopCompleted}
	if err := r.record(ctx, agent.AgentEvent{Type: agent.EventUsage, Model: "child", RunResult: result}); err != nil {
		t.Fatal(err)
	}
	if err := r.record(ctx, agent.AgentEvent{Type: agent.EventAgentEnd, RunResult: result}); err != nil {
		t.Fatal(err)
	}
	loaded, err := journal.Load(ctx, store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.UsageTotal != usage || len(loaded.Messages) != 0 {
		t.Fatalf("delegated usage = %#v, messages = %#v; want usage counted once and no child transcript", loaded.UsageTotal, loaded.Messages)
	}
}

// crashCalls is the scripted three-tool turn: one tool that is safe to re-run
// and two that are not.
var crashCalls = []types.ContentBlock{
	types.NewToolCall("call-read", "read", json.RawMessage(`{"path":"a.txt"}`)),
	types.NewToolCall("call-write", "write", json.RawMessage(`{"path":"b.txt"}`)),
	types.NewToolCall("call-bash", "bash", json.RawMessage(`{"command":"make"}`)),
}

var errCrashed = errors.New("simulated process death")

// toolLedger counts executions per tool call across a crashed run and its
// resume, and checks inside each execution that the call's tool_started line
// is already on disk.
type toolLedger struct {
	mu         sync.Mutex
	path       string
	executions map[string]int
	violations []string
}

func newToolLedger(path string) *toolLedger {
	return &toolLedger{path: path, executions: map[string]int{}}
}

func (l *toolLedger) tools() []agent.AgentTool {
	tools := make([]agent.AgentTool, 0, len(crashCalls))
	for _, call := range crashCalls {
		tools = append(tools, agent.AgentTool{
			Tool: types.Tool{Name: call.Name, Description: call.Name, Parameters: json.RawMessage(`{"type":"object"}`)},
			Execute: func(ctx context.Context, toolCallID string, params json.RawMessage, onUpdate agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
				l.record(toolCallID)
				return agent.AgentToolResult{Content: []types.ContentBlock{types.NewText("ok " + toolCallID)}}, nil
			},
		})
	}
	return tools
}

func (l *toolLedger) record(toolCallID string) {
	contents, err := os.ReadFile(l.path)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.executions[toolCallID]++
	found := false
	for _, line := range strings.Split(string(contents), "\n") {
		if strings.Contains(line, `"type":"tool_started"`) && strings.Contains(line, `"tool_call_id":"`+toolCallID+`"`) {
			found = true
			break
		}
	}
	if err != nil || !found {
		l.violations = append(l.violations, fmt.Sprintf("%s executed before its tool_started was on disk (read err %v)", toolCallID, err))
	}
}

func (l *toolLedger) snapshot() (map[string]int, []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	copied := make(map[string]int, len(l.executions))
	for id, count := range l.executions {
		copied[id] = count
	}
	return copied, append([]string(nil), l.violations...)
}

func newCrashAgent(f *faux.Faux, ledger *toolLedger, mode agent.ToolExecutionMode, messages []types.Message) *agent.Agent {
	return agent.NewAgent(agent.AgentOptions{
		InitialState: &agent.AgentState{
			SystemPrompt: "test",
			Model:        f.Model(),
			Tools:        ledger.tools(),
			Messages:     messages,
		},
		StreamFn:      f.StreamSimple,
		ToolExecution: mode,
	})
}

// journalLineCount counts lines past the header. It runs inside a listener,
// possibly on a tool goroutine, so it reports errors instead of failing t.
func journalLineCount(path string) (int, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return bytes.Count(contents, []byte{'\n'}) - 1, nil
}

// crashRun runs the scripted turn and simulates process death once the
// journal holds crashAfter lines past the header: a listener registered after
// the journal's closes the store and fails the run, and the file is cut back
// to exactly crashAfter lines (plus a torn half line when tornTail is set).
func crashRun(t *testing.T, mode agent.ToolExecutionMode, crashAfter int, tornTail []byte) (string, *toolLedger) {
	t.Helper()
	ctx := context.Background()
	store, err := journal.Create(ctx, t.TempDir(), "/workspace/project", journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	path := store.Path()
	ledger := newToolLedger(path)
	f := faux.New(faux.Options{})
	f.SetResponses(
		faux.RespondMessage(types.AssistantMessage{Content: crashCalls, StopReason: types.StopToolUse}),
		faux.Respond(faux.Text("all three done")),
	)
	agt := newCrashAgent(f, ledger, mode, nil)
	detach := AttachJournal(agt, store)
	defer detach()
	if crashAfter >= 0 {
		var crashed atomic.Bool
		agt.Subscribe(func(ctx context.Context, ev agent.AgentEvent) error {
			if crashed.Load() {
				return errCrashed
			}
			lines, err := journalLineCount(path)
			if err != nil {
				return err
			}
			if lines < crashAfter {
				return nil
			}
			crashed.Store(true)
			if err := store.Close(); err != nil {
				t.Errorf("close store at crash: %v", err)
			}
			return errCrashed
		})
	}
	if _, err := agt.PromptText(ctx, "do the three things"); err != nil {
		t.Fatalf("prompt did not start: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if crashAfter >= 0 {
		cutJournal(t, path, crashAfter, tornTail)
	}
	return path, ledger
}

// cutJournal keeps the header plus the first keep lines, then appends tail.
func cutJournal(t *testing.T, path string, keep int, tail []byte) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	end := 0
	for line := 0; line <= keep; line++ {
		next := bytes.IndexByte(contents[end:], '\n')
		if next < 0 {
			t.Fatalf("journal has fewer than %d lines", keep)
		}
		end += next + 1
	}
	if err := os.WriteFile(path, append(contents[:end:end], tail...), 0o600); err != nil {
		t.Fatal(err)
	}
}

// rawLine is the envelope of one journal line, enough to check record
// invariants straight from the file.
type rawLine struct {
	Type    string `json:"type"`
	RunID   string `json:"run_id"`
	Payload struct {
		ToolCallID string `json:"tool_call_id"`
	} `json:"payload"`
	Message struct {
		Role       string `json:"role"`
		ToolCallID string `json:"toolCallId"`
	} `json:"message"`
}

func readRawLines(t *testing.T, path string) []rawLine {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var lines []rawLine
	for _, text := range strings.Split(strings.TrimRight(string(contents), "\n"), "\n")[1:] {
		var line rawLine
		if err := json.Unmarshal([]byte(entryLine(text)), &line); err != nil {
			t.Fatalf("journal line after resume repair does not decode: %v", err)
		}
		lines = append(lines, line)
	}
	return lines
}

// checkTranscript is what a provider demands of a transcript: every tool call
// of a kept assistant message has exactly one result in the block right after
// it, and no result is an orphan or a duplicate. Assistant messages that
// stopped with error or aborted are dropped before the provider sees them, so
// their calls need no results.
func checkTranscript(messages []types.Message) error {
	seen := map[string]bool{}
	open := map[string]bool{}
	closeTurn := func() error {
		for id := range open {
			return fmt.Errorf("tool call %s has no result", id)
		}
		return nil
	}
	for index, message := range messages {
		switch m := message.(type) {
		case types.ToolResultMessage:
			if seen[m.ToolCallID] {
				return fmt.Errorf("message %d: duplicate result for %s", index, m.ToolCallID)
			}
			if !open[m.ToolCallID] {
				return fmt.Errorf("message %d: orphan result for %s", index, m.ToolCallID)
			}
			seen[m.ToolCallID] = true
			delete(open, m.ToolCallID)
		case types.AssistantMessage:
			if err := closeTurn(); err != nil {
				return fmt.Errorf("before message %d: %w", index, err)
			}
			if m.StopReason == types.StopError || m.StopReason == types.StopAborted {
				continue
			}
			for _, block := range m.Content {
				if block.Type == types.BlockToolCall {
					open[block.ID] = true
				}
			}
		default:
			if err := closeTurn(); err != nil {
				return fmt.Errorf("before message %d: %w", index, err)
			}
		}
	}
	return closeTurn()
}

// checkResumeInvariants asserts (a) through (d) on a reopened journal.
func checkResumeInvariants(lines []rawLine, loaded *journal.Loaded) error {
	type key struct{ run, call string }
	started := map[key]bool{}
	finished := map[key]bool{}
	resultEntries := map[string]int{}
	for index, line := range lines {
		k := key{line.RunID, line.Payload.ToolCallID}
		switch line.Type {
		case "tool_started":
			started[k] = true
		case "tool_finished":
			if !started[k] {
				return fmt.Errorf("(a) line %d: tool_finished for %s without an earlier tool_started", index+2, k.call)
			}
			finished[k] = true
		case "message":
			if line.Message.Role == "toolResult" {
				resultEntries[line.Message.ToolCallID]++
			}
		}
	}
	for id, count := range resultEntries {
		if count > 1 {
			return fmt.Errorf("(c) journal holds %d results for %s", count, id)
		}
	}

	interrupted := map[string]bool{}
	for _, tool := range loaded.Interrupted {
		interrupted[tool.ToolCallID] = true
		if !started[key{tool.RunID, tool.ToolCallID}] || finished[key{tool.RunID, tool.ToolCallID}] {
			return fmt.Errorf("(b) %s reported interrupted but the records disagree", tool.ToolCallID)
		}
	}
	for k := range started {
		if !finished[k] && !interrupted[k.call] {
			return fmt.Errorf("(b) %s started without finishing but is not in Interrupted", k.call)
		}
	}
	results := map[string][]types.ToolResultMessage{}
	for _, message := range loaded.Messages {
		if result, ok := message.(types.ToolResultMessage); ok {
			results[result.ToolCallID] = append(results[result.ToolCallID], result)
		}
	}
	for id := range interrupted {
		got := results[id]
		if len(got) != 1 || !got[0].IsError || !strings.HasPrefix(got[0].Content[0].Text, "outcome unknown") {
			texts := make([]string, 0, len(got))
			for _, result := range got {
				texts = append(texts, result.Content[0].Text)
			}
			return fmt.Errorf("(b) interrupted %s has results %q, want one unknown-outcome error", id, texts)
		}
	}
	for id, got := range results {
		if len(got) > 1 {
			return fmt.Errorf("(c) %d results for %s in Messages", len(got), id)
		}
	}
	if err := checkTranscript(loaded.Messages); err != nil {
		return fmt.Errorf("(d) %w", err)
	}
	return nil
}

// resume reopens the crashed journal, continues on a fresh faux provider, and
// returns the reduction it resumed from. The provider checks the transcript it
// is sent, which is the real acceptance test for (d).
func resume(t *testing.T, path string, mode agent.ToolExecutionMode, ledger *toolLedger) (*journal.Loaded, error) {
	t.Helper()
	ctx := context.Background()
	store, loaded, err := journal.OpenForResume(ctx, path, journal.Options{})
	if err != nil {
		return nil, fmt.Errorf("reopen: %w", err)
	}
	defer store.Close()
	lines := readRawLines(t, path)
	if err := checkResumeInvariants(lines, loaded); err != nil {
		return loaded, err
	}

	var sentErr error
	f := faux.New(faux.Options{})
	f.SetResponses(func(c types.Context, opts *types.StreamOptions, state faux.State, model types.Model) (types.AssistantMessage, error) {
		sentErr = checkTranscript(c.Messages)
		return faux.Respond(faux.Text("resumed"))(c, opts, state, model)
	})
	agt := newCrashAgent(f, ledger, mode, loaded.Messages)
	detach := AttachJournal(agt, store)
	defer detach()

	var result agent.RunResult
	if len(loaded.Messages) == 0 || loaded.Messages[len(loaded.Messages)-1].Role() == "assistant" {
		result, err = agt.PromptText(ctx, "carry on")
	} else {
		result, err = agt.Continue(ctx)
	}
	if err != nil {
		return loaded, fmt.Errorf("(e) resume did not start: %w", err)
	}
	if result.StopReason != agent.StopCompleted {
		return loaded, fmt.Errorf("(e) resumed run ended %s: %v", result.StopReason, result.Err)
	}
	if sentErr != nil {
		return loaded, fmt.Errorf("(d) provider was sent an invalid transcript: %w", sentErr)
	}
	return loaded, nil
}

func TestCrashAtEveryJournalBoundaryResumesHonestly(t *testing.T) {
	// seen proves the enumeration reached every reconciliation outcome and a
	// torn tail, so a passing run cannot mean the crashes missed the hard cases.
	seen := map[string]int{}
	for _, mode := range []agent.ToolExecutionMode{agent.ExecSequential, agent.ExecParallel} {
		// A clean reference run fixes how many lines there are to crash after
		// and supplies realistic torn tails.
		referencePath, _ := crashRun(t, mode, -1, nil)
		reference, err := os.ReadFile(referencePath)
		if err != nil {
			t.Fatal(err)
		}
		referenceLines := strings.Split(strings.TrimRight(string(reference), "\n"), "\n")[1:]
		if len(referenceLines) < 10 {
			t.Fatalf("%s reference journal has only %d lines", mode, len(referenceLines))
		}

		for crashAfter := 0; crashAfter <= len(referenceLines); crashAfter++ {
			var tails [][]byte
			tails = append(tails, nil)
			if crashAfter < len(referenceLines) {
				next := referenceLines[crashAfter]
				tails = append(tails, []byte(next[:len(next)/2]))
			}
			for _, tail := range tails {
				boundary := fmt.Sprintf("%s/after line %d", mode, crashAfter)
				if crashAfter > 0 {
					boundary += " " + lineKind(referenceLines[crashAfter-1])
				}
				if tail != nil {
					boundary += " + torn " + lineKind(referenceLines[crashAfter])
				}
				t.Run(boundary, func(t *testing.T) {
					path, ledger := crashRun(t, mode, crashAfter, tail)
					before, _ := ledger.snapshot()
					loaded, err := resume(t, path, mode, ledger)
					if err != nil {
						t.Fatalf("boundary %s: %v", boundary, err)
					}
					after, violations := ledger.snapshot()
					for _, call := range crashCalls {
						if after[call.ID] != before[call.ID] {
							t.Fatalf("boundary %s: (e) resume executed %s again (%d -> %d)", boundary, call.ID, before[call.ID], after[call.ID])
						}
						if after[call.ID] > 1 {
							t.Fatalf("boundary %s: (e) %s executed %d times", boundary, call.ID, after[call.ID])
						}
					}
					if len(violations) > 0 {
						t.Fatalf("boundary %s: intent after effect: %v", boundary, violations)
					}

					final, err := journal.Load(context.Background(), path)
					if err != nil {
						t.Fatalf("boundary %s: reload after resume: %v", boundary, err)
					}
					if final.LastRun == nil || !final.LastRun.Finished || final.LastRun.StopReason != string(agent.StopCompleted) {
						t.Fatalf("boundary %s: resumed run not recorded as completed: %#v", boundary, final.LastRun)
					}
					if len(final.Interrupted) != 0 {
						t.Fatalf("boundary %s: completed resume still reports interrupted tools: %#v", boundary, final.Interrupted)
					}
					// Only the warning is scoped to LastRun. Historical tool
					// results must keep exactly the same unknown-outcome text.
					if len(final.Messages) < len(loaded.Messages) || !reflect.DeepEqual(final.Messages[:len(loaded.Messages)], loaded.Messages) {
						t.Fatalf("boundary %s: completed resume changed the historical transcript", boundary)
					}
					if err := checkTranscript(final.Messages); err != nil {
						t.Fatalf("boundary %s: (d) after resume: %v", boundary, err)
					}
					for _, message := range loaded.Messages {
						if result, ok := message.(types.ToolResultMessage); ok && result.IsError {
							seen[strings.SplitN(result.Content[0].Text, ":", 2)[0]]++
						}
					}
					for _, diagnostic := range loaded.Diagnostics {
						if strings.Contains(diagnostic.Message, "torn final line") {
							seen["torn"]++
						}
						if strings.Contains(diagnostic.Message, "was interrupted") {
							seen["interrupted run"]++
						}
					}
				})
			}
		}
	}
	for _, outcome := range []string{"outcome unknown", "result lost", "not executed", "torn", "interrupted run"} {
		if seen[outcome] == 0 {
			t.Errorf("no boundary produced %q; the enumeration is not reaching it (seen %v)", outcome, seen)
		}
	}
}

func lineKind(line string) string {
	var raw rawLine
	if err := json.Unmarshal([]byte(entryLine(line)), &raw); err != nil {
		return "?"
	}
	switch {
	case raw.Payload.ToolCallID != "":
		return raw.Type + " " + raw.Payload.ToolCallID
	case raw.Message.Role != "":
		return raw.Type + " " + raw.Message.Role
	default:
		return raw.Type
	}
}

func TestAttachJournalRecordsAFullRunInOrder(t *testing.T) {
	path, ledger := crashRun(t, agent.ExecSequential, -1, nil)
	if _, violations := ledger.snapshot(); len(violations) > 0 {
		t.Fatal(violations)
	}
	var kinds []string
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range bytes.Split(contents, []byte{'\n'})[1:] {
		if len(raw) == 0 {
			continue
		}
		envelope, err := durable.DecodeEnvelope(path, raw)
		if err != nil {
			t.Fatal(err)
		}
		for _, change := range envelope.Commit.Changes {
			if change.Kind == durable.EntryKind && change.Namespace == "_tempest/journal" {
				var line rawLine
				if err := json.Unmarshal(change.Value, &line); err != nil {
					t.Fatal(err)
				}
				kinds = append(kinds, line.Type)
			}
		}
	}
	want := []string{
		"run_started", "message", "message", "usage",
		"tool_started", "tool_finished", "message",
		"tool_started", "tool_finished", "message",
		"tool_started", "tool_finished", "message",
		"message", "usage", "run_finished",
	}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("record order =\n%v\nwant\n%v", kinds, want)
	}
	loaded, err := journal.Load(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Diagnostics) != 0 || len(loaded.Interrupted) != 0 || loaded.LastRun == nil || loaded.LastRun.StopReason != string(agent.StopCompleted) {
		t.Fatalf("loaded = %#v", loaded)
	}
	if len(loaded.Messages) != 6 || loaded.UsageTotal.TotalTokens == 0 {
		t.Fatalf("messages = %d, usage = %#v", len(loaded.Messages), loaded.UsageTotal)
	}
}

type archiveReadBarrier struct {
	*durable.Log
	block   bool
	entered chan struct{}
	release chan struct{}
}

func (b *archiveReadBarrier) Read(ctx context.Context, q durable.Query) (durable.Page, error) {
	if b.block && q.Kind == durable.EntryKind && q.Namespace == "_tempest/journal" && q.History {
		close(b.entered)
		<-b.release
	}
	return b.Log.Read(ctx, q)
}

func TestFreshPromptDoesNotWaitForArchiveReplay(t *testing.T) {
	ctx := context.Background()
	backend := &archiveReadBarrier{Log: durable.NewMemory(), entered: make(chan struct{}), release: make(chan struct{})}
	store, err := journal.CreateBackend(ctx, backend, t.TempDir(), "/workspace/project", journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("first")), faux.Respond(faux.Text("second")))
	a := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{Model: f.Model()}, StreamFn: f.StreamSimple})
	defer AttachJournal(a, store)()
	if result, err := a.PromptText(ctx, "first"); err != nil || result.Err != nil {
		t.Fatalf("first prompt: %+v, %v", result, err)
	}
	backend.block = true
	done := make(chan error, 1)
	go func() {
		result, err := a.PromptText(ctx, "second")
		done <- errors.Join(err, result.Err)
	}()
	select {
	case <-backend.entered:
		close(backend.release)
		<-done
		t.Fatal("fresh prompt waited for the complete archive instead of the writer's committed usage receipts")
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	}
	if f.CallCount() != 2 {
		t.Fatalf("provider calls = %d, want 2", f.CallCount())
	}
	if len(a.RunState().UsageSources) == 0 {
		t.Fatal("fresh prompt lost the previous run's usage receipts")
	}
}

func TestAttachJournalStopsTheRunWhenTheJournalIsDead(t *testing.T) {
	ctx := context.Background()
	store, err := journal.Create(ctx, t.TempDir(), "/workspace/project", journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ledger := newToolLedger(store.Path())
	f := faux.New(faux.Options{})
	f.SetResponses(faux.RespondMessage(types.AssistantMessage{Content: crashCalls, StopReason: types.StopToolUse}))
	agt := newCrashAgent(f, ledger, agent.ExecParallel, nil)
	defer AttachJournal(agt, store)()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := agt.PromptText(ctx, "go")
	if err != nil {
		t.Fatalf("prompt did not start: %v", err)
	}
	if result.StopReason != agent.StopError || result.Err == nil {
		t.Fatalf("result = %#v, want a StopError from the journal", result)
	}
	if executions, _ := ledger.snapshot(); f.CallCount() != 0 || len(executions) != 0 {
		t.Fatalf("run continued on a dead journal: %d model calls, executions %v", f.CallCount(), executions)
	}
}

func entryLine(line string) string {
	var envelope durable.Envelope
	if json.Unmarshal([]byte(line), &envelope) == nil && envelope.Type == "commit" {
		for _, change := range envelope.Commit.Changes {
			if change.Kind == durable.EntryKind && change.Namespace == "_tempest/journal" {
				return string(change.Value)
			}
		}
	}
	return line
}
