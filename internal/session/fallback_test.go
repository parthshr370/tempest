package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/hooks"
	"go.harness.dev/harness/internal/provider/base"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/retry"
	"go.harness.dev/harness/internal/session/journal"
)

// httpStatusProvider is a faux provider behind an HTTP status. While status
// is an error, every call reports it through OnResponse and fails the way a
// provider's HTTP error path does; otherwise the faux provider answers.
// onCall, when set, runs at the start of every call.
type httpStatusProvider struct {
	faux   *faux.Faux
	mu     sync.Mutex
	status int
	calls  int
	onCall func()
}

func newStatusProvider(provider, model string, status int) *httpStatusProvider {
	return &httpStatusProvider{faux: faux.New(faux.Options{API: provider + "-api", Provider: provider, Models: []faux.ModelDef{{ID: model}}}), status: status}
}

func (p *httpStatusProvider) setStatus(status int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status = status
}

func (p *httpStatusProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *httpStatusProvider) Stream(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream {
	p.mu.Lock()
	status := p.status
	p.calls++
	onCall := p.onCall
	p.mu.Unlock()
	if onCall != nil {
		onCall()
	}
	if status == 0 {
		return p.faux.StreamSimple(ctx, model, c, opts)
	}
	if opts != nil && opts.OnResponse != nil {
		if err := opts.OnResponse(types.ProviderResponse{Status: status}, model); err != nil {
			panic(err)
		}
	}
	failure := base.ResponseError(&http.Response{StatusCode: status, Header: http.Header{}}, []byte(`{"error":{"message":"fixture failure"}}`))
	message := types.AssistantMessage{API: model.API, Provider: model.Provider, Model: model.ID, Content: []types.ContentBlock{}, StopReason: types.StopError}
	base.ApplyError(&message, failure)
	failed := stream.NewAssistantStream(model.API, model.Provider, model.ID)
	failed.Push(types.StreamEvent{Type: types.EvError, Reason: types.StopError, Err: &message})
	failed.End()
	return failed
}

type noRetryDelay struct{}

func (noRetryDelay) Sleep(context.Context, time.Duration) error { return nil }

// fallbackRig is an agent whose model chain is primary then backup, each
// behind its own three-attempt retry policy, recorded into a journal.
type fallbackRig struct {
	store   *journal.Store
	primary *httpStatusProvider
	backup  *httpStatusProvider
	agent   *agent.Agent
	mu      sync.Mutex
	events  []agent.AgentEvent
	toolRun int
}

const fallbackAttempts = 3

func newFallbackRig(t *testing.T, store *journal.Store, primaryStatus int, messages []types.Message, runner *hooks.Runner) *fallbackRig {
	t.Helper()
	rig := &fallbackRig{store: store, primary: newStatusProvider("primary", "p1", primaryStatus), backup: newStatusProvider("backup", "b1", 0)}
	policy := retry.DefaultConfig()
	policy.MaxAttempts = fallbackAttempts
	policy.Sleeper = noRetryDelay{}
	primaryRetrier, err := retry.New(rig.primary, policy)
	if err != nil {
		t.Fatal(err)
	}
	backupRetrier, err := retry.New(rig.backup, policy)
	if err != nil {
		t.Fatal(err)
	}
	primaryModel, backupModel := rig.primary.faux.Model(), rig.backup.faux.Model()
	chain, err := retry.NewFallback([]retry.Candidate{{Model: primaryModel, Stream: primaryRetrier}, {Model: backupModel, Stream: backupRetrier}})
	if err != nil {
		t.Fatal(err)
	}
	note := agent.AgentTool{
		Tool: types.Tool{Name: "note", Description: "records a note", Parameters: json.RawMessage(`{"type":"object"}`)},
		Execute: func(context.Context, string, json.RawMessage, agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			rig.mu.Lock()
			rig.toolRun++
			rig.mu.Unlock()
			return agent.AgentToolResult{Content: []types.ContentBlock{types.NewText("noted")}}, nil
		},
	}
	rig.agent = agent.NewAgent(agent.AgentOptions{
		InitialState:   &agent.AgentState{SystemPrompt: "test", Model: primaryModel, Tools: []agent.AgentTool{note}, Messages: messages},
		StreamFn:       chain.Stream,
		FallbackModels: []types.Model{backupModel},
		Hooks:          runner,
	})
	t.Cleanup(AttachJournal(rig.agent, store))
	rig.agent.Subscribe(func(_ context.Context, ev agent.AgentEvent) error {
		rig.mu.Lock()
		defer rig.mu.Unlock()
		rig.events = append(rig.events, ev)
		return nil
	})
	return rig
}

func (rig *fallbackRig) fallbacks() []types.ModelFallback {
	rig.mu.Lock()
	defer rig.mu.Unlock()
	var switches []types.ModelFallback
	for _, ev := range rig.events {
		if ev.Type == agent.EventModelFallback && ev.Fallback != nil {
			switches = append(switches, *ev.Fallback)
		}
	}
	return switches
}

func (rig *fallbackRig) toolRuns() int {
	rig.mu.Lock()
	defer rig.mu.Unlock()
	return rig.toolRun
}

func newFallbackStore(t *testing.T) *journal.Store {
	t.Helper()
	store, err := journal.Create(context.Background(), t.TempDir(), "/workspace/project", journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func loadJournal(t *testing.T, path string) *journal.Loaded {
	t.Helper()
	loaded, err := journal.Load(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func lastAssistant(messages []types.Message) types.AssistantMessage {
	for i := len(messages) - 1; i >= 0; i-- {
		if message, ok := messages[i].(types.AssistantMessage); ok {
			return message
		}
	}
	return types.AssistantMessage{}
}

// The primary returns 503 until its retry budget is spent; the backup then
// answers every turn of the run, the switch is journaled once, and the next
// prompt goes to the primary again.
func TestFallbackAnswersAfterPrimaryRetriesAndSticksForTheRun(t *testing.T) {
	ctx := context.Background()
	store := newFallbackStore(t)
	rig := newFallbackRig(t, store, http.StatusServiceUnavailable, nil, nil)
	rig.backup.faux.SetResponses(
		faux.RespondMessage(types.AssistantMessage{Content: []types.ContentBlock{faux.ToolCall("note", json.RawMessage(`{}`), "call-1")}, StopReason: types.StopToolUse}),
		faux.Respond(faux.Text("answered by backup")),
	)

	result, err := rig.agent.PromptText(ctx, "do it")
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != agent.StopCompleted || result.Turns != 2 || rig.toolRuns() != 1 {
		t.Fatalf("run = %+v err=%v tools=%d", result, result.Err, rig.toolRuns())
	}
	if rig.primary.callCount() != fallbackAttempts {
		t.Fatalf("primary calls = %d, want %d (second turn must stay on the backup)", rig.primary.callCount(), fallbackAttempts)
	}
	if rig.backup.callCount() != 2 {
		t.Fatalf("backup calls = %d, want 2", rig.backup.callCount())
	}
	switches := rig.fallbacks()
	if len(switches) != 1 || switches[0].From.ID != "p1" || switches[0].To.ID != "b1" || switches[0].Reason != string(retry.CodeServerFailure) || switches[0].Attempts != fallbackAttempts || switches[0].Index != 1 {
		t.Fatalf("fallback events = %+v", switches)
	}
	final := lastAssistant(rig.agent.State().Messages)
	if final.Provider != "backup" || final.Model != "b1" {
		t.Fatalf("final answer accounted to %s:%s", final.Provider, final.Model)
	}

	loaded := loadJournal(t, store.Path())
	if len(loaded.Runs) != 1 || loaded.Runs[0].Fallback == nil {
		t.Fatalf("journal runs = %+v", loaded.Runs)
	}
	record := loaded.Runs[0].Fallback
	if record.From != "primary:p1" || record.To != "backup:b1" || record.Reason != string(retry.CodeServerFailure) || record.Attempts != fallbackAttempts || record.FallbackIndex != 1 {
		t.Fatalf("model_fallback record = %+v", record)
	}
	answered := 0
	for _, message := range loaded.Messages {
		if assistant, ok := message.(types.AssistantMessage); ok && assistant.StopReason != types.StopError {
			if assistant.Provider != "backup" {
				t.Fatalf("answer journaled against %s:%s", assistant.Provider, assistant.Model)
			}
			answered++
		}
	}
	if answered != 2 {
		t.Fatalf("journaled answers = %d", answered)
	}

	rig.primary.setStatus(0)
	rig.primary.faux.SetResponses(faux.Respond(faux.Text("primary is back")))
	result, err = rig.agent.PromptText(ctx, "again")
	if err != nil || result.StopReason != agent.StopCompleted {
		t.Fatalf("second prompt = %+v err=%v", result, err)
	}
	if rig.primary.callCount() != fallbackAttempts+1 || rig.backup.callCount() != 2 {
		t.Fatalf("second prompt calls: primary=%d backup=%d", rig.primary.callCount(), rig.backup.callCount())
	}
	if final := lastAssistant(rig.agent.State().Messages); final.Provider != "primary" {
		t.Fatalf("second prompt answered by %s", final.Provider)
	}
	if len(rig.fallbacks()) != 1 {
		t.Fatalf("second prompt switched models: %+v", rig.fallbacks())
	}
}

// A rejected request or a credential failure is a configuration error: it
// surfaces from the primary and the backup is never called.
func TestFallbackNeverMasksConfigurationErrors(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			store := newFallbackStore(t)
			rig := newFallbackRig(t, store, status, nil, nil)
			rig.backup.faux.SetResponses(faux.Respond(faux.Text("must not answer")))
			result, err := rig.agent.PromptText(context.Background(), "do it")
			if err != nil {
				t.Fatal(err)
			}
			if result.StopReason != agent.StopError || result.Err == nil {
				t.Fatalf("run = %+v", result)
			}
			if rig.primary.callCount() != 1 || rig.backup.callCount() != 0 || len(rig.fallbacks()) != 0 {
				t.Fatalf("calls primary=%d backup=%d switches=%+v", rig.primary.callCount(), rig.backup.callCount(), rig.fallbacks())
			}
			if loaded := loadJournal(t, store.Path()); loaded.Runs[0].Fallback != nil {
				t.Fatalf("journaled a fallback: %+v", loaded.Runs[0].Fallback)
			}
		})
	}
}

// Cancelling while the backup is answering ends the run as aborted, without
// another attempt or another model.
func TestFallbackCancelDuringBackupAborts(t *testing.T) {
	store := newFallbackStore(t)
	rig := newFallbackRig(t, store, http.StatusServiceUnavailable, nil, nil)
	rig.backup.faux.SetResponses(func(c types.Context, opts *types.StreamOptions, state faux.State, model types.Model) (types.AssistantMessage, error) {
		rig.agent.Abort()
		<-state.Context.Done()
		return faux.Respond(faux.Text("too late"))(c, opts, state, model)
	})
	result, err := rig.agent.PromptText(context.Background(), "do it")
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != agent.StopAborted {
		t.Fatalf("run = %+v err=%v", result, result.Err)
	}
	if rig.backup.callCount() != 1 || rig.primary.callCount() != fallbackAttempts {
		t.Fatalf("calls primary=%d backup=%d", rig.primary.callCount(), rig.backup.callCount())
	}
	loaded := loadJournal(t, store.Path())
	if !loaded.Runs[0].Finished || loaded.Runs[0].StopReason != string(agent.StopAborted) || loaded.Runs[0].Fallback == nil {
		t.Fatalf("journal run = %+v", loaded.Runs[0])
	}
}

// Cancelling after the switch was admitted but before the backup was called
// never calls the backup and reports no backup attempt to the hooks.
func TestFallbackCancelAtTheSwitchNeverCallsTheBackup(t *testing.T) {
	runner, path := hookRecorder(t, 0)
	store := newFallbackStore(t)
	rig := newFallbackRig(t, store, http.StatusServiceUnavailable, nil, runner)
	rig.agent.Subscribe(func(_ context.Context, ev agent.AgentEvent) error {
		if ev.Type == agent.EventModelFallback {
			rig.agent.Abort()
		}
		return nil
	})
	result, err := rig.agent.PromptText(context.Background(), "do it")
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != agent.StopAborted || rig.backup.callCount() != 0 || rig.primary.callCount() != fallbackAttempts {
		t.Fatalf("run = %+v calls primary=%d backup=%d", result, rig.primary.callCount(), rig.backup.callCount())
	}
	runner.FlushObservations(context.Background())
	inputs := readHookInputs(t, path)
	if len(inputs) != 2*fallbackAttempts {
		t.Fatalf("hook inputs = %d, want %d (the primary's attempts only): %v", len(inputs), 2*fallbackAttempts, inputs)
	}
	for _, input := range inputs {
		if input["model"] != "p1" {
			t.Fatalf("hook reported a backup attempt that was never sent: %v", input)
		}
	}
}

// The switch to the backup is in the journal before the backup is called, so
// a crash at any point after the backup starts resumes on the backup.
func TestFallbackSwitchIsJournaledBeforeTheBackupIsCalled(t *testing.T) {
	store := newFallbackStore(t)
	rig := newFallbackRig(t, store, http.StatusServiceUnavailable, nil, nil)
	rig.backup.faux.SetResponses(faux.Respond(faux.Text("answered by backup")))
	var recordedAtCall []*journal.ModelFallbackRecord
	rig.backup.onCall = func() {
		loaded, err := journal.Load(context.Background(), store.Path())
		if err != nil {
			t.Errorf("load at backup call: %v", err)
			return
		}
		recordedAtCall = append(recordedAtCall, loaded.Runs[0].Fallback)
	}
	// A listener that is slow to take the switch holds the backup back.
	const slowListener = 50 * time.Millisecond
	rig.agent.Subscribe(func(_ context.Context, ev agent.AgentEvent) error {
		if ev.Type == agent.EventModelFallback {
			time.Sleep(slowListener)
			if rig.backup.callCount() != 0 {
				t.Error("backup called while a listener was still taking the switch")
			}
		}
		return nil
	})
	result, err := rig.agent.PromptText(context.Background(), "do it")
	if err != nil || result.StopReason != agent.StopCompleted {
		t.Fatalf("run = %+v err=%v", result, err)
	}
	if len(recordedAtCall) != 1 || recordedAtCall[0] == nil || recordedAtCall[0].To != "backup:b1" {
		t.Fatalf("switch recorded when the backup was called = %+v", recordedAtCall)
	}
}

// A switch that cannot be journaled never reaches the backup: the run stops
// with the journal's error instead of spending on an unrecorded model.
func TestFallbackSwitchThatCannotBeJournaledNeverCallsTheBackup(t *testing.T) {
	store := newFallbackStore(t)
	rig := newFallbackRig(t, store, http.StatusServiceUnavailable, nil, nil)
	rig.backup.faux.SetResponses(faux.Respond(faux.Text("must not answer")))
	// The journal fails right after the primary's last attempt, so its next
	// append is the switch.
	rig.primary.onCall = func() {
		if rig.primary.callCount() == fallbackAttempts {
			if err := store.Close(); err != nil {
				t.Errorf("close journal: %v", err)
			}
		}
	}
	result, _ := rig.agent.PromptText(context.Background(), "do it")
	if result.Err == nil || rig.backup.callCount() != 0 {
		t.Fatalf("run = %+v backup calls=%d", result, rig.backup.callCount())
	}
	if loaded := loadJournal(t, store.Path()); loaded.Runs[0].Fallback != nil {
		t.Fatalf("journal has a switch the store refused: %+v", loaded.Runs[0].Fallback)
	}
}

// A listener that fails on the switch refuses its admission like a journal
// failure: the run returns that error, the backup is never called, and the
// waiting fallback chain is released instead of deadlocking.
func TestFallbackSwitchAListenerRefusesNeverCallsTheBackup(t *testing.T) {
	rig := newFallbackRig(t, newFallbackStore(t), http.StatusServiceUnavailable, nil, nil)
	refused := errors.New("listener refused the switch")
	rig.agent.Subscribe(func(_ context.Context, ev agent.AgentEvent) error {
		if ev.Type == agent.EventModelFallback {
			return refused
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := rig.agent.PromptText(ctx, "do it")
	if ctx.Err() != nil {
		t.Fatal("a refused switch deadlocked the run")
	}
	if !errors.Is(errors.Join(err, result.Err), refused) || rig.backup.callCount() != 0 {
		t.Fatalf("run = %+v err=%v backup calls=%d", result, err, rig.backup.callCount())
	}
}

// A crash while the switch is being taken, after the journal append, leaves
// a run that resumes on the backup; the crashed run never called it.
func TestFallbackCrashAtTheSwitchResumesOnTheBackup(t *testing.T) {
	ctx := context.Background()
	store := newFallbackStore(t)
	path := store.Path()
	rig := newFallbackRig(t, store, http.StatusServiceUnavailable, nil, nil)
	rig.backup.faux.SetResponses(faux.Respond(faux.Text("must not answer")))
	rig.agent.Subscribe(func(_ context.Context, ev agent.AgentEvent) error {
		if ev.Type == agent.EventModelFallback {
			if err := store.Close(); err != nil {
				t.Errorf("close at crash: %v", err)
			}
			return errFallbackCrash
		}
		return nil
	})
	_, _ = rig.agent.PromptText(ctx, "do it")
	if rig.backup.callCount() != 0 {
		t.Fatalf("crashed run called the backup %d times", rig.backup.callCount())
	}

	resumed, loaded, err := journal.OpenForResume(ctx, path, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resumed.Close() })
	after := newFallbackRig(t, resumed, 0, loaded.Messages, nil)
	after.primary.faux.SetResponses(faux.Respond(faux.Text("primary must not answer")))
	after.backup.faux.SetResponses(faux.Respond(faux.Text("resumed on backup")))
	result, err := after.agent.Continue(ctx)
	if err != nil || result.StopReason != agent.StopCompleted {
		t.Fatalf("resumed run = %+v err=%v", result, err)
	}
	if after.primary.callCount() != 0 || after.backup.callCount() != 1 {
		t.Fatalf("resumed calls primary=%d backup=%d", after.primary.callCount(), after.backup.callCount())
	}
}

var errFallbackCrash = errors.New("simulated crash")

// A crash after the run switched to the backup resumes on the backup: the
// resumed logical run never sends the primary another request.
func TestFallbackChoiceSurvivesCrashResume(t *testing.T) {
	ctx := context.Background()
	store := newFallbackStore(t)
	path := store.Path()
	rig := newFallbackRig(t, store, http.StatusServiceUnavailable, nil, nil)
	rig.backup.faux.SetResponses(
		faux.RespondMessage(types.AssistantMessage{Content: []types.ContentBlock{faux.ToolCall("note", json.RawMessage(`{}`), "call-1")}, StopReason: types.StopToolUse}),
		faux.Respond(faux.Text("not reached")),
	)
	crashed := false
	rig.agent.Subscribe(func(_ context.Context, ev agent.AgentEvent) error {
		if crashed {
			return errFallbackCrash
		}
		if ev.Type == agent.EventTurnEnd {
			crashed = true
			if err := store.Close(); err != nil {
				t.Errorf("close at crash: %v", err)
			}
			return errFallbackCrash
		}
		return nil
	})
	if _, err := rig.agent.PromptText(ctx, "do it"); err != nil {
		t.Fatal(err)
	}
	if !crashed || rig.toolRuns() != 1 {
		t.Fatalf("crash point not reached: crashed=%v tools=%d", crashed, rig.toolRuns())
	}

	resumed, loaded, err := journal.OpenForResume(ctx, path, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resumed.Close() })
	if loaded.LastRun == nil || loaded.LastRun.Finished {
		t.Fatalf("crashed run looks finished: %+v", loaded.LastRun)
	}
	// The primary is healthy now; the resumed run must still not use it.
	after := newFallbackRig(t, resumed, 0, loaded.Messages, nil)
	after.primary.faux.SetResponses(faux.Respond(faux.Text("primary must not answer")))
	after.backup.faux.SetResponses(faux.Respond(faux.Text("resumed on backup")))
	result, err := after.agent.Continue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != agent.StopCompleted {
		t.Fatalf("resumed run = %+v err=%v", result, result.Err)
	}
	if after.primary.callCount() != 0 || after.backup.callCount() != 1 {
		t.Fatalf("resumed calls primary=%d backup=%d", after.primary.callCount(), after.backup.callCount())
	}
	if final := lastAssistant(after.agent.State().Messages); final.Provider != "backup" {
		t.Fatalf("resumed answer from %s", final.Provider)
	}
	if state := after.agent.RunState(); state.ContinuationOf == "" || state.Fallback == nil || state.Fallback.Model != "backup:b1" {
		t.Fatalf("resumed run state = %+v", state)
	}
}

// hookRecorder configures PreModelRequest and PostModelRequest commands that
// append their stdin to a file, one JSON object per line.
func hookRecorder(t *testing.T, sleepSeconds int) (*hooks.Runner, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "requests.jsonl")
	record := fmt.Sprintf("cat >> %q; echo >> %q", path, path)
	pre := record
	if sleepSeconds > 0 {
		pre = fmt.Sprintf("sleep %d; %s", sleepSeconds, record)
	}
	runner, err := hooks.New(hooks.Settings{Hooks: map[string][]hooks.Group{
		"PreModelRequest":  {{Hooks: []hooks.Command{{Type: "command", Command: pre}}}},
		"PostModelRequest": {{Hooks: []hooks.Command{{Type: "command", Command: record}}}},
	}}, hooks.Input{SessionID: "session-1", Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	return runner, path
}

func readHookInputs(t *testing.T, path string) []map[string]any {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var inputs []map[string]any
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if len(scanner.Bytes()) == 0 {
			continue
		}
		var input map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &input); err != nil {
			t.Fatalf("hook input %q: %v", scanner.Text(), err)
		}
		inputs = append(inputs, input)
	}
	return inputs
}

// Every attempt, across retries and the switch to the backup, gets exactly
// one PreModelRequest and one PostModelRequest with the documented fields.
func TestModelRequestHooksReportEveryAttempt(t *testing.T) {
	runner, path := hookRecorder(t, 0)
	store := newFallbackStore(t)
	rig := newFallbackRig(t, store, http.StatusServiceUnavailable, nil, runner)
	rig.backup.faux.SetResponses(faux.Respond(faux.Text("answered by backup")))
	result, err := rig.agent.PromptText(context.Background(), "do it")
	if err != nil || result.StopReason != agent.StopCompleted {
		t.Fatalf("run = %+v err=%v", result, err)
	}
	runID := rig.agent.RunState().RunID
	runner.FlushObservations(context.Background())

	inputs := readHookInputs(t, path)
	type attempt struct {
		model         string
		attempt       float64
		fallbackIndex float64
		status        string
	}
	want := []attempt{}
	for n := 1; n <= fallbackAttempts; n++ {
		want = append(want, attempt{"p1", float64(n), 0, ""}, attempt{"p1", float64(n), 0, "failed"})
	}
	want = append(want, attempt{"b1", 1, 1, ""}, attempt{"b1", 1, 1, "completed"})
	if len(inputs) != len(want) {
		t.Fatalf("hook inputs = %d, want %d: %v", len(inputs), len(want), inputs)
	}
	for i, input := range inputs {
		got := attempt{model: fmt.Sprint(input["model"]), attempt: input["attempt"].(float64), fallbackIndex: input["fallback_index"].(float64)}
		event := "PreModelRequest"
		if want[i].status != "" {
			event = "PostModelRequest"
			got.status = fmt.Sprint(input["status"])
		}
		if got != want[i] || input["hook_event_name"] != event {
			t.Fatalf("input %d = %v, want %+v %s", i, input, want[i], event)
		}
		if input["session_id"] != "session-1" || input["cwd"] == "" || input["turn_id"] != runID || input["role"] != "default" {
			t.Fatalf("input %d common fields = %v", i, input)
		}
		if event == "PreModelRequest" {
			continue
		}
		errorText, hasError := input["error"]
		usage, hasUsage := input["token_usage"]
		if !hasError || !hasUsage {
			t.Fatalf("post input %d lacks error or token_usage: %v", i, input)
		}
		if want[i].status == "failed" && (errorText == nil || usage != nil) {
			t.Fatalf("failed post %d = %v", i, input)
		}
		if want[i].status == "completed" {
			counts, ok := usage.(map[string]any)
			if errorText != nil || !ok || counts["total_tokens"] == nil {
				t.Fatalf("completed post %d = %v", i, input)
			}
		}
	}
}

// A model-request hook that sleeps runs beside the request, never before it.
func TestModelRequestHooksNeverDelayTheRequest(t *testing.T) {
	const hookSleepSeconds = 10
	runner, _ := hookRecorder(t, hookSleepSeconds)
	store := newFallbackStore(t)
	rig := newFallbackRig(t, store, 0, nil, runner)
	rig.primary.faux.SetResponses(faux.Respond(faux.Text("fast answer")))
	started := time.Now()
	result, err := rig.agent.PromptText(context.Background(), "do it")
	elapsed := time.Since(started)
	if err != nil || result.StopReason != agent.StopCompleted {
		t.Fatalf("run = %+v err=%v", result, err)
	}
	if elapsed >= hookSleepSeconds*time.Second/2 {
		t.Fatalf("run took %s behind a %ds observation hook", elapsed, hookSleepSeconds)
	}
	// Session end flushes the queue; a cancelled flush kills the sleeper.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	flushStarted := time.Now()
	runner.FlushObservations(cancelled)
	if time.Since(flushStarted) >= hookSleepSeconds*time.Second/2 {
		t.Fatal("cancelled flush waited for the sleeping hook")
	}
}

// blockingSleeper signals when a retry backoff starts and waits for the
// request's cancellation.
type blockingSleeper struct{ entered chan struct{} }

func (s blockingSleeper) Sleep(ctx context.Context, _ time.Duration) error {
	close(s.entered)
	<-ctx.Done()
	return ctx.Err()
}

// A retry that is cancelled during its backoff was never sent, so the hooks
// see only the attempt that was: one PreModelRequest and its failed
// PostModelRequest.
func TestModelRequestHooksSkipARetryCancelledInItsBackoff(t *testing.T) {
	runner, path := hookRecorder(t, 0)
	rig := newFallbackRig(t, newFallbackStore(t), http.StatusServiceUnavailable, nil, runner)
	sleeper := blockingSleeper{entered: make(chan struct{})}
	policy := retry.DefaultConfig()
	policy.Sleeper = sleeper
	retrier, err := retry.New(rig.primary, policy)
	if err != nil {
		t.Fatal(err)
	}
	rig.agent.StreamFn = retrier.Stream
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = rig.agent.PromptText(ctx, "cancel the backoff")
	}()
	<-sleeper.entered
	cancel()
	<-done
	runner.FlushObservations(context.Background())
	inputs := readHookInputs(t, path)
	if rig.primary.callCount() != 1 || len(inputs) != 2 || inputs[0]["hook_event_name"] != "PreModelRequest" || inputs[1]["status"] != "failed" {
		t.Fatalf("provider calls = %d, hook inputs = %v", rig.primary.callCount(), inputs)
	}
}
