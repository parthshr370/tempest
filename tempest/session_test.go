package tempest

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/compaction"
	types "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/session/journal"
)

type promptFunc func(context.Context, Ask) (Choice, error)

func (f promptFunc) Approve(ctx context.Context, a Ask) (Choice, error) { return f(ctx, a) }
func openFaux(t *testing.T, f *faux.Faux, p Prompter) *Session {
	t.Helper()
	s, err := Open(context.Background(), Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &NewSession{}, Prompter: p, Stack: &StackConfig{Model: f.Model(), StreamFn: f.StreamSimple}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return s
}
func scriptBash(f *faux.Faux, command string) {
	raw, _ := json.Marshal(map[string]any{"command": command})
	f.AppendResponses(faux.Respond(faux.ToolCall("bash", raw, "call")), faux.Respond(faux.Text("finished")))
}
func TestSessionPermissionRoundTripAndJournalOrder(t *testing.T) {
	for _, choice := range []Choice{permission.AllowOnce, permission.RejectAlways} {
		t.Run(string(choice), func(t *testing.T) {
			f := faux.New(faux.Options{})
			var asks atomic.Int32
			p := promptFunc(func(ctx context.Context, a Ask) (Choice, error) {
				asks.Add(1)
				if a.ToolCallID != "call" || a.Tool != "bash" || a.Kind != "execute" || !json.Valid(a.RawInput) || len(a.Options) != 4 {
					t.Errorf("bad ask: %+v", a)
				}
				return choice, nil
			})
			s := openFaux(t, f, p)
			target := filepath.Join(s.Snapshot().Cwd, "approved-directory")
			command := "mkdir -p " + target
			scriptBash(f, command)
			result, err := s.Prompt(context.Background(), Prompt{Text: "use bash"})
			if err != nil || result.StopReason != agent.StopCompleted {
				t.Fatalf("run = %+v, %v", result, err)
			}
			if result.RunID == "" || result.RunID != s.Snapshot().LastRun.RunID {
				t.Fatalf("run ids result=%q snapshot=%+v", result.RunID, s.Snapshot().LastRun)
			}
			_, statErr := os.Stat(target)
			if (statErr == nil) != (choice == permission.AllowOnce) {
				t.Fatalf("tool side effect stat=%v choice=%s", statErr, choice)
			}
			if choice == permission.RejectAlways {
				scriptBash(f, command)
				if _, err := s.Prompt(context.Background(), Prompt{Text: "try again"}); err != nil {
					t.Fatal(err)
				}
				if asks.Load() != 1 {
					t.Fatalf("reject_always asked %d times", asks.Load())
				}
			}
			raw, err := os.ReadFile(s.core.store.Path())
			if err != nil {
				t.Fatal(err)
			}
			permissionIndex, startedIndex := -1, -1
			for i, line := range strings.Split(string(raw), "\n") {
				var entry struct {
					Type    string `json:"type"`
					Payload struct {
						Decision string `json:"decision"`
						Source   string `json:"source"`
					} `json:"payload"`
				}
				if json.Unmarshal([]byte(line), &entry) != nil {
					continue
				}
				if entry.Type == "permission" && permissionIndex < 0 {
					permissionIndex = i
					expected := "deny"
					if choice == permission.AllowOnce {
						expected = "allow"
					}
					if entry.Payload.Decision != expected || entry.Payload.Source != "prompt" {
						t.Fatalf("permission fact = %s", line)
					}
				}
				if entry.Type == "tool_started" && startedIndex < 0 {
					startedIndex = i
				}
			}
			if permissionIndex < 0 || startedIndex <= permissionIndex {
				t.Fatalf("permission index %d, intent index %d", permissionIndex, startedIndex)
			}
		})
	}
}
func TestSessionCancelWhilePermissionPending(t *testing.T) {
	f := faux.New(faux.Options{})
	asked := make(chan Ask, 1)
	s := openFaux(t, f, promptFunc(func(ctx context.Context, a Ask) (Choice, error) { asked <- a; <-ctx.Done(); return "", ctx.Err() }))
	scriptBash(f, "mkdir -p "+filepath.Join(s.Snapshot().Cwd, "never"))
	done := make(chan RunResult, 1)
	runErr := make(chan error, 1)
	go func() { r, err := s.Prompt(context.Background(), Prompt{Text: "wait"}); done <- r; runErr <- err }()
	ask := <-asked
	snapshot := s.Snapshot()
	if len(snapshot.PendingAsks) != 1 || snapshot.PendingAsks[0].ToolCallID != ask.ToolCallID {
		t.Fatalf("pending snapshot=%+v", snapshot.PendingAsks)
	}
	s.Cancel()
	if result := <-done; result.StopReason != agent.StopAborted {
		t.Fatalf("result=%+v", result)
	}
	if err := <-runErr; err != nil {
		t.Fatal(err)
	}
	if len(s.Snapshot().PendingAsks) != 0 {
		t.Fatal("cancel left pending ask")
	}
	if err := s.Answer(ask.ToolCallID, permission.AllowOnce); !errors.Is(err, ErrNoPendingAsk) {
		t.Fatalf("late answer = %v", err)
	}
}
func TestAnswerWinsAndCancelsSuppliedPrompter(t *testing.T) {
	f := faux.New(faux.Options{})
	asked := make(chan Ask, 1)
	settled := make(chan struct{})
	s := openFaux(t, f, promptFunc(func(ctx context.Context, a Ask) (Choice, error) {
		asked <- a
		<-ctx.Done()
		close(settled)
		return "", ctx.Err()
	}))
	target := filepath.Join(s.Snapshot().Cwd, "allowed")
	scriptBash(f, "mkdir -p "+target)
	done := make(chan error, 1)
	go func() { _, err := s.Prompt(context.Background(), Prompt{Text: "ask"}); done <- err }()
	a := <-asked
	if err := s.Answer(a.ToolCallID, permission.AllowOnce); err != nil {
		t.Fatal(err)
	}
	if err := s.Answer(a.ToolCallID, permission.RejectOnce); !errors.Is(err, ErrNoPendingAsk) {
		t.Fatalf("second answer=%v", err)
	}
	<-settled
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal(err)
	}
}
func TestPreCancelledSessionHasZeroCallsAndDurableAbortedRun(t *testing.T) {
	f := faux.New(faux.Options{})
	s := openFaux(t, f, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, err := s.Prompt(ctx, Prompt{Text: "accepted"})
	if err != nil || r.StopReason != agent.StopAborted || r.Turns != 0 || f.CallCount() != 0 {
		t.Fatalf("result=%+v err=%v provider calls=%d", r, err, f.CallCount())
	}
	loaded, err := journal.Load(context.Background(), s.core.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.LastRun == nil || !loaded.LastRun.Finished || loaded.LastRun.StopReason != "aborted" {
		t.Fatalf("durable run=%+v", loaded.LastRun)
	}
}
func TestResumeSeedsTranscriptAndDiagnostics(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("first")))
	s := openFaux(t, f, nil)
	if _, err := s.Prompt(context.Background(), Prompt{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	path := s.core.store.Path()
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	resumed, err := Open(context.Background(), Options{Resume: &ResumeSession{Path: path}, Cwd: t.TempDir(), SessionDir: t.TempDir(), Stack: &StackConfig{Model: f.Model(), StreamFn: f.StreamSimple}})
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close(context.Background())
	snap := resumed.Snapshot()
	if snap.LastRun == nil || !snap.LastRun.Finished || snap.Usage.Total.Input == 0 {
		t.Fatalf("resumed snapshot=%+v", snap)
	}
	history, err := resumed.History(snap.HistoryEpoch, 0, snap.HistoryLen)
	if err != nil {
		t.Fatal(err)
	}
	var transcript strings.Builder
	for _, u := range history {
		if u.Kind == "user_message_chunk" || u.Kind == "agent_message_chunk" {
			var p struct {
				Content struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			_ = json.Unmarshal(u.Update, &p)
			transcript.WriteString(p.Content.Text)
		}
	}
	if transcript.String() != "hellofirst" {
		t.Fatalf("transcript=%q", transcript.String())
	}
	f.SetResponses(faux.Respond(faux.Text("second")))
	if _, err := resumed.Prompt(context.Background(), Prompt{Text: "again"}); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, u := range resumed.Snapshot().History {
		if u.Kind != "user_message_chunk" {
			continue
		}
		var p struct {
			MessageID string `json:"messageId"`
		}
		_ = json.Unmarshal(u.Update, &p)
		if ids[p.MessageID] {
			t.Fatalf("message id reused %q", p.MessageID)
		}
		ids[p.MessageID] = true
	}
}
func TestOpenWithoutResolvedStackUsesDefaultDiscovery(t *testing.T) {
	script := filepath.Join(t.TempDir(), "script.json")
	if err := os.WriteFile(script, []byte(`[{"text":"hello"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &NewSession{}, FauxScript: script})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	if _, err := s.Prompt(context.Background(), Prompt{Text: "test"}); err != nil {
		t.Fatal(err)
	}
	if s.LastAssistantText() != "hello" {
		t.Fatalf("text=%q", s.LastAssistantText())
	}
}
func TestBudgetAndPermissionValidation(t *testing.T) {
	f := faux.New(faux.Options{})
	s := openFaux(t, f, nil)
	if err := s.SetBudget(Budget{MaxTurns: -1}); err == nil {
		t.Fatal("negative budget accepted")
	}
	if err := s.SetPermissionMode("invalid"); err == nil {
		t.Fatal("invalid mode accepted")
	}
	if err := s.SetPermissionMode(permission.ModePlan); err != nil {
		t.Fatal(err)
	}
	if err := s.SetBudget(Budget{MaxTurns: 2}); err != nil {
		t.Fatal(err)
	}
	if got := s.Snapshot(); got.Budget.MaxTurns != 2 || got.PermissionMode != "plan" {
		t.Fatalf("settings=%+v", got)
	}
}

func TestLateAttachAtEveryEventMatchesSixToolJournalTranscript(t *testing.T) {
	run := func(t *testing.T, boundary int) int {
		t.Helper()
		f := faux.New(faux.Options{})
		blocks := []types.ContentBlock{faux.Text("checking")}
		for _, id := range []string{"a", "b", "c", "d", "e", "f"} {
			blocks = append(blocks, faux.ToolCall("probe", json.RawMessage(`{}`), id))
		}
		f.SetResponses(faux.Respond(blocks...), faux.Respond(faux.Text("all done")))
		tool := Tool{Tool: types.Tool{Name: "probe", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}, ExecutionMode: agent.ExecSequential, Execute: func(_ context.Context, id string, _ json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			return agent.AgentToolResult{Content: []types.ContentBlock{types.NewText("result " + id)}}, nil
		}}
		s, err := Open(context.Background(), Options{Cwd: t.TempDir(), SessionDir: t.TempDir(), New: &NewSession{Tools: []Tool{tool}}, Stack: &StackConfig{Model: f.Model(), StreamFn: f.StreamSimple}})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close(context.Background())
		count := 0
		var snap Snapshot
		var events <-chan Update
		var stop func()
		unsubscribe := s.core.agent.Subscribe(func(context.Context, agent.AgentEvent) error {
			if count == boundary {
				snap, events, stop = s.Watch(context.Background(), WatchOptions{Buffer: 10000})
			}
			count++
			return nil
		})
		defer unsubscribe()
		result, err := s.Prompt(context.Background(), Prompt{Text: "six tools"})
		if err != nil || result.StopReason != agent.StopCompleted {
			t.Fatalf("run=%+v err=%v", result, err)
		}
		if boundary < 0 {
			return count
		}
		if events == nil {
			t.Fatalf("boundary %d not reached (%d events)", boundary, count)
		}
		prefix, err := s.History(snap.HistoryEpoch, 0, snap.HistoryLen)
		if err != nil {
			t.Fatal(err)
		}
		var reconstructed strings.Builder
		appendText := func(u Update) {
			var p struct {
				Content   json.RawMessage `json:"content"`
				RawOutput json.RawMessage `json:"rawOutput"`
			}
			_ = json.Unmarshal(u.Update, &p)
			switch u.Kind {
			case "user_message_chunk", "agent_message_chunk":
				var c struct {
					Text string `json:"text"`
				}
				_ = json.Unmarshal(p.Content, &c)
				reconstructed.WriteString(c.Text)
			case "tool_call_update":
				if len(p.RawOutput) == 0 {
					return
				}
				var content []struct {
					Content struct {
						Text string `json:"text"`
					} `json:"content"`
				}
				_ = json.Unmarshal(p.Content, &content)
				for _, c := range content {
					reconstructed.WriteString(c.Content.Text)
				}
			}
		}
		for _, u := range prefix {
			appendText(u)
		}
		if snap.Streaming != nil {
			reconstructed.WriteString(snap.Streaming.Text)
		}
		stop()
		expectedSeq := snap.Seq + 1
		for u := range events {
			if u.Seq != expectedSeq {
				t.Fatalf("seq=%d want %d", u.Seq, expectedSeq)
			}
			expectedSeq++
			appendText(u)
		}
		loaded, err := journal.Load(context.Background(), s.core.store.Path())
		if err != nil {
			t.Fatal(err)
		}
		var expected strings.Builder
		for _, msg := range loaded.Messages {
			switch m := msg.(type) {
			case types.UserMessage:
				if m.Content.Blocks == nil {
					expected.WriteString(m.Content.Text)
				} else {
					for _, b := range m.Content.Blocks {
						if b.Type == types.BlockText {
							expected.WriteString(b.Text)
						}
					}
				}
			case types.AssistantMessage:
				for _, b := range m.Content {
					if b.Type == types.BlockText {
						expected.WriteString(b.Text)
					}
				}
			case types.ToolResultMessage:
				for _, b := range m.Content {
					if b.Type == types.BlockText {
						expected.WriteString(b.Text)
					}
				}
			}
		}
		if reconstructed.String() != expected.String() {
			t.Fatalf("boundary %d transcript=%q journal=%q", boundary, reconstructed.String(), expected.String())
		}
		return count
	}
	count := run(t, -1)
	for boundary := range count {
		t.Run(string(rune('A'+boundary)), func(t *testing.T) { run(t, boundary) })
	}
}

func TestCloseJoinsRunRejectsAdmissionAndReleasesJournal(t *testing.T) {
	f := faux.New(faux.Options{})
	asked := make(chan struct{}, 1)
	s := openFaux(t, f, promptFunc(func(ctx context.Context, _ Ask) (Choice, error) {
		asked <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	}))
	scriptBash(f, "mkdir -p "+filepath.Join(s.Snapshot().Cwd, "never"))
	done := make(chan RunResult, 1)
	go func() { result, _ := s.Prompt(context.Background(), Prompt{Text: "wait"}); done <- result }()
	<-asked
	if _, err := s.Prompt(context.Background(), Prompt{Text: "busy"}); !errors.Is(err, ErrRunActive) {
		t.Fatalf("concurrent prompt=%v", err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if result := <-done; result.StopReason != agent.StopAborted {
		t.Fatalf("closed run=%+v", result)
	}
	if _, err := s.Continue(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed admission=%v", err)
	}
	store, loaded, err := journal.OpenForResume(context.Background(), s.core.store.Path(), journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if loaded.LastRun == nil || loaded.LastRun.StopReason != "aborted" || !loaded.LastRun.Finished {
		t.Fatalf("closed journal=%+v", loaded.LastRun)
	}
}

func TestCompactionChangesPublicHistoryEpochWithoutRecountingUsage(t *testing.T) {
	f := faux.New(faux.Options{})
	model := f.Model()
	model.ContextWindow = 100
	var summaries atomic.Int32
	step := func(c types.Context, _ *types.StreamOptions, _ faux.State, _ types.Model) (types.AssistantMessage, error) {
		if c.SystemPrompt == compaction.SummarizationSystemPrompt {
			summaries.Add(1)
			return types.AssistantMessage{Content: []types.ContentBlock{types.NewText("summary")}}, nil
		}
		return types.AssistantMessage{Content: []types.ContentBlock{types.NewText(strings.Repeat("response ", 300))}}, nil
	}
	for range 16 {
		f.AppendResponses(step)
	}
	s, err := Open(context.Background(), Options{Cwd: t.TempDir(), SessionDir: t.TempDir(), New: &NewSession{}, Stack: &StackConfig{Model: model, StreamFn: f.StreamSimple, GenericRules: []string{"rule"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	before := s.Snapshot()
	for _, text := range []string{strings.Repeat("first ", 50000), strings.Repeat("second ", 50000)} {
		if _, err := s.Prompt(context.Background(), Prompt{Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	after := s.Snapshot()
	if summaries.Load() == 0 || after.HistoryEpoch <= before.HistoryEpoch {
		t.Fatalf("summaries=%d epoch before=%d after=%d", summaries.Load(), before.HistoryEpoch, after.HistoryEpoch)
	}
	if _, err := s.History(before.HistoryEpoch, 0, 1); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("old epoch=%v", err)
	}
	loaded, err := journal.Load(context.Background(), s.core.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if after.Usage.Total.TotalTokens != loaded.UsageTotal.TotalTokens {
		t.Fatalf("usage recounted after compaction: snapshot=%+v journal=%+v", after.Usage.Total, loaded.UsageTotal)
	}
}
