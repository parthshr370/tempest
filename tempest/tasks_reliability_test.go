package tempest

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/extension"
)

var reliabilityObjectSchema = json.RawMessage(`{"type":"object"}`)

func foregroundTaskSession(t *testing.T, provider *faux.Faux, definition extension.TaskDefinition) *Session {
	t.Helper()
	registry := extension.NewRegistry()
	t.Cleanup(func() {
		if err := registry.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	var owner *Session
	start := extension.Tool{Name: "start", Effect: extension.EffectExecute, InputSchema: reliabilityObjectSchema, Execute: func(ctx context.Context, _ extension.Call, _ extension.Invocation) (extension.Result, error) {
		_, err := owner.CreateTask(ctx, TaskRequest{ID: "foreground", Definition: "tasks/" + definition.Name, Input: json.RawMessage(`{}`)})
		return extension.Result{Content: []extension.Content{{Type: "text", Text: "admitted"}}}, err
	}}
	if err := registry.Install(t.Context(), extension.Definition{ID: "tasks", Revision: "v1", Tasks: []extension.TaskDefinition{definition}, Tools: []extension.Tool{start}}); err != nil {
		t.Fatal(err)
	}
	var err error
	owner, err = Open(t.Context(), Options{Cwd: t.TempDir(), SessionDir: t.TempDir(), New: &NewSession{PermissionMode: permission.ModeBypass}, Registry: registry, Extensions: []extension.Selection{{ID: "tasks", Required: true}}, Stack: &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple, AllowedTools: []string{"ext__tasks__start"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := owner.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return owner
}

func waitTaskStatus(t *testing.T, owner *Session, id, status string) Task {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		task, err := owner.Task(t.Context(), id)
		if err == nil && task.Status == status {
			return task
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("task %s: want %s, got %+v err=%v", id, status, task, err)
		}
	}
}

func TestForegroundSleepDeliversQueuedSteer(t *testing.T) {
	provider := faux.New(faux.Options{})
	provider.SetResponses(faux.Respond(faux.ToolCall("ext__tasks__start", json.RawMessage(`{}`), "start")), faux.Respond(faux.Text("waiting")), faux.Respond(faux.Text("steered")))
	definition := extension.TaskDefinition{Name: "sleep", Version: 1, InputSchema: reliabilityObjectSchema, StateSchema: reliabilityObjectSchema, Initial: json.RawMessage(`{}`), Run: func(context.Context, extension.Task, extension.Invocation) (extension.TaskDecision, error) {
		deadline := time.Now().Add(time.Hour)
		return extension.TaskDecision{Kind: "sleep", WakeAt: &deadline}, nil
	}}
	owner := foregroundTaskSession(t, provider, definition)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		if _, err := owner.Prompt(t.Context(), Prompt{Text: "start"}); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("foreground prompt: %v", err)
		}
	}()
	waitTaskStatus(t, owner, "foreground", "sleeping")
	if _, err := owner.Steer(t.Context(), QueuedInput{Text: "are you there?"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for provider.CallCount() < 3 {
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("steer wasn't delivered while task slept")
		}
	}
	owner.Cancel()
	<-finished
}

func TestCloseCancelsAsyncForegroundCleanup(t *testing.T) {
	provider := faux.New(faux.Options{})
	provider.SetResponses(faux.Respond(faux.ToolCall("ext__tasks__start", json.RawMessage(`{}`), "start")), faux.Respond(faux.Text("waiting")))
	running, compensating := make(chan struct{}), make(chan struct{})
	var abortCalls atomic.Int32
	definition := extension.TaskDefinition{Name: "flow", Version: 1, InputSchema: reliabilityObjectSchema, StateSchema: reliabilityObjectSchema, Initial: json.RawMessage(`{}`), Run: func(ctx context.Context, _ extension.Task, _ extension.Invocation) (extension.TaskDecision, error) {
		close(running)
		<-ctx.Done()
		return extension.TaskDecision{}, ctx.Err()
	}, Abort: func(ctx context.Context, _ extension.Task, _ extension.Invocation) (extension.TaskDecision, error) {
		if abortCalls.Add(1) == 1 {
			close(compensating)
		}
		<-ctx.Done()
		return extension.TaskDecision{}, ctx.Err()
	}}
	owner := foregroundTaskSession(t, provider, definition)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		if _, err := owner.Prompt(t.Context(), Prompt{Text: "start"}); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("foreground prompt: %v", err)
		}
	}()
	select {
	case <-running:
	case <-time.After(5 * time.Second):
		t.Fatal("task didn't start")
	}
	owner.Cancel()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("Cancel waited for compensation")
	}
	select {
	case <-compensating:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup wasn't scheduled")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := owner.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if abortCalls.Load() != 1 {
		t.Fatalf("cleanup restarted %d times", abortCalls.Load())
	}
}

func TestRejectedCleanupDoesNotFaultLaterRunsOrClose(t *testing.T) {
	provider := faux.New(faux.Options{})
	provider.SetResponses(faux.Respond(faux.ToolCall("ext__tasks__start", json.RawMessage(`{}`), "start")), faux.Respond(faux.Text("waiting")), faux.Respond(faux.Text("unrelated")))
	running := make(chan struct{})
	definition := extension.TaskDefinition{Name: "flow", Version: 1, InputSchema: reliabilityObjectSchema, StateSchema: reliabilityObjectSchema, Initial: json.RawMessage(`{}`), Run: func(ctx context.Context, _ extension.Task, _ extension.Invocation) (extension.TaskDecision, error) {
		close(running)
		<-ctx.Done()
		return extension.TaskDecision{}, ctx.Err()
	}, Abort: func(context.Context, extension.Task, extension.Invocation) (extension.TaskDecision, error) {
		return extension.TaskDecision{Kind: "wait", Wait: []string{"missing-task"}}, nil
	}}
	owner := foregroundTaskSession(t, provider, definition)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		if _, err := owner.Prompt(t.Context(), Prompt{Text: "start"}); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("foreground prompt: %v", err)
		}
	}()
	select {
	case <-running:
	case <-time.After(5 * time.Second):
		t.Fatal("task didn't start")
	}
	owner.Cancel()
	<-finished
	task := waitTaskStatus(t, owner, "foreground", "parked")
	if task.Crashes != 0 || !strings.Contains(task.Reason, "missing wait") {
		t.Fatalf("task=%+v", task)
	}
	result, err := owner.Prompt(t.Context(), Prompt{Text: "unrelated"})
	if err != nil || result.StopReason == "error" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if err := owner.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestTaskCallbackCommitAdvancesRealSessionPhase(t *testing.T) {
	provider := faux.New(faux.Options{})
	registry := extension.NewRegistry()
	defer registry.Close(context.Background())
	calls := 0
	definition := extension.TaskDefinition{Name: "flow", Version: 1, InputSchema: reliabilityObjectSchema, StateSchema: reliabilityObjectSchema, Initial: json.RawMessage(`{}`), Run: func(ctx context.Context, task extension.Task, invocation extension.Invocation) (extension.TaskDecision, error) {
		calls++
		_, err := invocation.Commit(ctx, durable.Commit{ID: "saved", Changes: []durable.Change{{SessionID: task.SessionID, Kind: durable.DocumentKind, Namespace: "tasks.app", ID: "doc", Version: 1, Value: json.RawMessage(`{}`)}}})
		return extension.TaskDecision{Kind: "complete"}, err
	}}
	document := durable.DocumentDefinition{Namespace: "tasks.app", Version: 1, Schema: reliabilityObjectSchema, Initial: json.RawMessage(`{}`), History: durable.HistoryRewindable, Fork: durable.ForkInitial, Reset: durable.ResetPreserve}
	if err := registry.Install(t.Context(), extension.Definition{ID: "tasks", Revision: "v1", Tasks: []extension.TaskDefinition{definition}, Documents: []durable.DocumentDefinition{document}}); err != nil {
		t.Fatal(err)
	}
	owner, err := Open(t.Context(), Options{Cwd: t.TempDir(), SessionDir: t.TempDir(), New: &NewSession{}, Registry: registry, Extensions: []extension.Selection{{ID: "tasks", Required: true}}, Stack: &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(context.Background())
	if _, err := owner.CreateTask(t.Context(), TaskRequest{ID: "flow", Definition: "tasks/flow", Input: json.RawMessage(`{}`), Background: true}); err != nil {
		t.Fatal(err)
	}
	if err := owner.ResumePending(t.Context()); err != nil {
		t.Fatal(err)
	}
	task, err := owner.Task(t.Context(), "flow")
	if err != nil || task.Status != "completed" || calls != 1 {
		t.Fatalf("calls=%d task=%+v err=%v", calls, task, err)
	}
}

func TestHostAlarmFailureIsolatedAndTemporarySessionsClose(t *testing.T) {
	provider := faux.New(faux.Options{})
	registry := extension.NewRegistry()
	defer registry.Close(context.Background())
	var calls atomic.Int32
	definition := extension.TaskDefinition{Name: "flow", Version: 1, InputSchema: reliabilityObjectSchema, StateSchema: reliabilityObjectSchema, Initial: json.RawMessage(`{}`), Run: func(context.Context, extension.Task, extension.Invocation) (extension.TaskDecision, error) {
		calls.Add(1)
		return extension.TaskDecision{Kind: "complete"}, nil
	}}
	if err := registry.Install(t.Context(), extension.Definition{ID: "tasks", Revision: "v1", Tasks: []extension.TaskDefinition{definition}}); err != nil {
		t.Fatal(err)
	}
	store := durable.NewMemory()
	now := time.Unix(1000, 0).UTC()
	host, err := OpenHost(t.Context(), HostOptions{SessionDir: t.TempDir(), Store: store, Registry: registry, Extensions: []extension.Selection{{ID: "tasks", Required: true}}, Stack: &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}, Options: Options{Tasks: TaskOptions{Now: func() time.Time { return now }}}})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close(context.Background())
	var ids []string
	for range 3 {
		owner, err := host.Create(t.Context(), AgentOptions{Cwd: t.TempDir(), New: &NewSession{}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := owner.CreateTask(t.Context(), TaskRequest{ID: "flow", Definition: "tasks/flow", Input: json.RawMessage(`{}`), Background: true}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, owner.ID())
	}
	for _, id := range ids[:2] {
		if err := host.CloseSession(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	release, err := store.AcquireSession(t.Context(), ids[0])
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := host.ResumePending(t.Context()); err == nil {
		t.Fatal("locked archived session wasn't reported")
	}
	if calls.Load() != 2 {
		t.Fatalf("healthy sessions blocked: calls=%d", calls.Load())
	}
	host.mu.Lock()
	temporaryOpen := host.sessions[ids[1]] != nil
	host.mu.Unlock()
	if temporaryOpen {
		t.Fatal("alarm-reopened session leaked its writer")
	}
	if wake, present := host.NextWake(); !present || !wake.Equal(now.Add(taskAlarmRetryDelay)) {
		t.Fatalf("failed alarm hot-loops: wake=%v present=%v", wake, present)
	}
}

func TestCorruptArchiveDoesNotHideHealthyAlarm(t *testing.T) {
	provider := faux.New(faux.Options{})
	registry := extension.NewRegistry()
	defer registry.Close(context.Background())
	definition := extension.TaskDefinition{Name: "flow", Version: 1, InputSchema: reliabilityObjectSchema, StateSchema: reliabilityObjectSchema, Initial: json.RawMessage(`{}`), Run: func(context.Context, extension.Task, extension.Invocation) (extension.TaskDecision, error) {
		return extension.TaskDecision{Kind: "complete"}, nil
	}}
	if err := registry.Install(t.Context(), extension.Definition{ID: "tasks", Revision: "v1", Tasks: []extension.TaskDefinition{definition}}); err != nil {
		t.Fatal(err)
	}
	host, err := OpenHost(t.Context(), HostOptions{SessionDir: t.TempDir(), Registry: registry, Extensions: []extension.Selection{{ID: "tasks", Required: true}}, Stack: &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close(context.Background())
	var corruptPath string
	for index := range 2 {
		owner, err := host.Create(t.Context(), AgentOptions{Cwd: t.TempDir(), New: &NewSession{}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := owner.CreateTask(t.Context(), TaskRequest{ID: "flow", Definition: "tasks/flow", Input: json.RawMessage(`{}`), Background: true}); err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			corruptPath = owner.core.store.Path()
		}
		if err := host.CloseSession(t.Context(), owner.ID()); err != nil {
			t.Fatal(err)
		}
	}
	contents, err := os.ReadFile(corruptPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corruptPath, append(contents, []byte("not-json\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	alarms, err := host.archivedTaskAlarms(t.Context())
	if err == nil || len(alarms) != 1 {
		t.Fatalf("alarms=%v err=%v", alarms, err)
	}
	if _, present := host.NextWake(); !present {
		t.Fatal("one corrupt journal hid every alarm")
	}
}

func TestTaskPagesAndHubOmitLargePayloads(t *testing.T) {
	provider := faux.New(faux.Options{})
	registry := extension.NewRegistry()
	defer registry.Close(context.Background())
	input, err := json.Marshal(struct {
		Payload string `json:"payload"`
	}{strings.Repeat("x", 48<<10)})
	if err != nil {
		t.Fatal(err)
	}
	definition := extension.TaskDefinition{Name: "flow", Version: 1, InputSchema: reliabilityObjectSchema, StateSchema: reliabilityObjectSchema, Initial: json.RawMessage(`{}`), Run: func(context.Context, extension.Task, extension.Invocation) (extension.TaskDecision, error) {
		return extension.TaskDecision{Kind: "complete"}, nil
	}}
	definition.Initial = input
	if err := registry.Install(t.Context(), extension.Definition{ID: "tasks", Revision: "v1", Tasks: []extension.TaskDefinition{definition}}); err != nil {
		t.Fatal(err)
	}
	owner, err := Open(t.Context(), Options{Cwd: t.TempDir(), SessionDir: t.TempDir(), New: &NewSession{}, Registry: registry, Extensions: []extension.Selection{{ID: "tasks", Required: true}}, Stack: &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(context.Background())
	for _, id := range []string{"first", "second"} {
		if _, err := owner.CreateTask(t.Context(), TaskRequest{ID: id, Definition: "tasks/flow", Input: input, Background: true}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := owner.Tasks(t.Context(), TaskQuery{Limit: 1})
	if err != nil || len(page.Tasks) != 1 || page.NextCursor == "" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	next, err := owner.Tasks(t.Context(), TaskQuery{Limit: 1, Cursor: page.NextCursor})
	if err != nil || len(next.Tasks) != 1 || next.Tasks[0].ID == page.Tasks[0].ID || next.Seq != page.Seq {
		t.Fatalf("page=%+v err=%v", next, err)
	}
	owner.publishDurable(t.Context())
	snapshot, _, stop := owner.Watch(t.Context(), WatchOptions{})
	defer stop()
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"first"`) || !strings.Contains(string(raw), `"second"`) || strings.Contains(string(raw), strings.Repeat("x", 1024)) {
		t.Fatalf("unbounded or blank task projection: %s", raw)
	}
	if err := owner.ResumePending(t.Context()); err != nil {
		t.Fatal(err)
	}
	page, err = owner.Tasks(t.Context(), TaskQuery{})
	if err != nil || len(page.Tasks) != 2 || page.Tasks[0].Status != "completed" {
		t.Fatalf("history page=%+v err=%v", page, err)
	}
}

func TestDecisionDocumentSchemaRejectionIsTypedAndParked(t *testing.T) {
	provider := faux.New(faux.Options{})
	registry := extension.NewRegistry()
	defer registry.Close(context.Background())
	var sessionID string
	definition := extension.TaskDefinition{Name: "flow", Version: 1, InputSchema: reliabilityObjectSchema, StateSchema: reliabilityObjectSchema, Initial: json.RawMessage(`{}`), Run: func(context.Context, extension.Task, extension.Invocation) (extension.TaskDecision, error) {
		return extension.TaskDecision{Kind: "complete", Commit: durable.Commit{Changes: []durable.Change{{SessionID: sessionID, Kind: durable.DocumentKind, Namespace: "tasks.app", ID: "doc", Version: 1, Value: json.RawMessage(`false`)}}}}, nil
	}}
	document := durable.DocumentDefinition{Namespace: "tasks.app", Version: 1, Schema: reliabilityObjectSchema, Initial: json.RawMessage(`{}`), History: durable.HistoryRewindable, Fork: durable.ForkInitial, Reset: durable.ResetPreserve}
	if err := registry.Install(t.Context(), extension.Definition{ID: "tasks", Revision: "v1", Tasks: []extension.TaskDefinition{definition}, Documents: []durable.DocumentDefinition{document}}); err != nil {
		t.Fatal(err)
	}
	owner, err := Open(t.Context(), Options{Cwd: t.TempDir(), SessionDir: t.TempDir(), New: &NewSession{}, Registry: registry, Extensions: []extension.Selection{{ID: "tasks", Required: true}}, Stack: &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(context.Background())
	sessionID = owner.ID()
	if _, err := owner.CreateTask(t.Context(), TaskRequest{ID: "flow", Definition: "tasks/flow", Input: json.RawMessage(`{}`), Background: true}); err != nil {
		t.Fatal(err)
	}
	if err := owner.ResumePending(t.Context()); err != nil {
		t.Fatal(err)
	}
	task, err := owner.Task(t.Context(), "flow")
	if err != nil || task.Status != "parked" || task.Crashes != 0 || !strings.Contains(task.Reason, durable.ErrInvalidCommit.Error()) {
		t.Fatalf("task=%+v err=%v", task, err)
	}
	_, err = owner.core.store.Commit(t.Context(), durable.Commit{ID: "invalid-schema", Changes: []durable.Change{{SessionID: sessionID, Kind: durable.DocumentKind, Namespace: "tasks.app", ID: "doc", Version: 1, Value: json.RawMessage(`false`)}}})
	if !errors.Is(err, durable.ErrInvalidCommit) {
		t.Fatalf("schema rejection untyped: %v", err)
	}
}

func TestTaskSummaryPageFitsRPCFrameWithEscapedLabels(t *testing.T) {
	const taskPageSize = 100
	const rpcFrameBytes = 1 << 20
	label := strings.Repeat("\n", 256)
	task := Task{ID: label, Definition: label, Owner: label, Phase: label, Status: "cleanup_faulted", Reason: strings.Repeat("\n", 4096)}
	page := TaskPage{Tasks: make([]extension.TaskSummary, taskPageSize)}
	for index := range page.Tasks {
		page.Tasks[index] = task.Summary()
	}
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) >= rpcFrameBytes {
		t.Fatalf("summary page exceeded RPC frame: %d bytes", len(raw))
	}
}

func TestRejectedForegroundChildParksWithoutHoldingRun(t *testing.T) {
	provider := faux.New(faux.Options{})
	provider.SetResponses(faux.Respond(faux.ToolCall("ext__tasks__start", json.RawMessage(`{}`), "start")), faux.Respond(faux.Text("waiting")), faux.Respond(faux.Text("unrelated")))
	definition := extension.TaskDefinition{Name: "flow", Version: 1, InputSchema: reliabilityObjectSchema, StateSchema: reliabilityObjectSchema, Initial: json.RawMessage(`{}`), Run: func(_ context.Context, task extension.Task, _ extension.Invocation) (extension.TaskDecision, error) {
		if task.ID == "foreground" {
			return extension.TaskDecision{Kind: "wait", Wait: []string{"child"}, Children: []extension.TaskRequest{{ID: "child", Definition: "tasks/flow", Input: json.RawMessage(`{}`)}}}, nil
		}
		return extension.TaskDecision{Kind: "wait", Wait: []string{"missing-task"}}, nil
	}}
	owner := foregroundTaskSession(t, provider, definition)
	type promptResult struct {
		result RunResult
		err    error
	}
	finished := make(chan promptResult, 1)
	go func() {
		result, err := owner.Prompt(t.Context(), Prompt{Text: "start"})
		finished <- promptResult{result, err}
	}()
	select {
	case reply := <-finished:
		if reply.err != nil || reply.result.StopReason != "completed" {
			t.Fatalf("first run=%+v err=%v", reply.result, reply.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("parked child held its owner's run")
	}
	child, err := owner.Task(t.Context(), "child")
	if err != nil || child.Status != "parked" || child.AbortReason != "" || !strings.Contains(child.Reason, "missing wait") {
		t.Fatalf("child=%+v err=%v", child, err)
	}
	parent, err := owner.Task(t.Context(), "foreground")
	if err != nil || parent.Status != "waiting" || parent.AbortReason != "" {
		t.Fatalf("owner=%+v err=%v", parent, err)
	}
	result, err := owner.Prompt(t.Context(), Prompt{Text: "unrelated"})
	if err != nil || result.StopReason != "completed" {
		t.Fatalf("later run=%+v err=%v", result, err)
	}
}
