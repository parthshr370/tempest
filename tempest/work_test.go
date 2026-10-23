package tempest

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/stream"
	types "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/session/journal"
)

func TestSessionDurableIdleQueuesAndCancelRetention(t *testing.T) {
	ctx := t.Context()
	provider := faux.New(faux.Options{})
	owner := openFaux(t, provider, nil)
	first, err := owner.Steer(ctx, QueuedInput{Text: "retained steer", Key: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := owner.FollowUp(ctx, QueuedInput{Text: "retained follow up", Key: "second"})
	if err != nil {
		t.Fatal(err)
	}
	owner.Cancel()
	if snapshot := owner.Snapshot(); snapshot.Run.Status != "idle" || len(snapshot.Queues) != 2 {
		t.Fatalf("idle admission/cancel snapshot=%+v", snapshot)
	}
	retried, err := owner.Steer(ctx, QueuedInput{Text: "retained steer", Key: "first"})
	if err != nil || retried.ID != first.ID {
		t.Fatalf("key retry=%+v %v", retried, err)
	}
	if _, err = owner.FollowUp(ctx, QueuedInput{Text: "changed", Key: "first"}); !errors.Is(err, ErrQueueConflict) {
		t.Fatalf("conflict=%v", err)
	}
	if err = owner.CancelQueued(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	if err = owner.CancelQueued(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	path := owner.core.store.Path()
	cwd := owner.Snapshot().Cwd
	if err = owner.Close(ctx); err != nil {
		t.Fatal(err)
	}
	loaded, err := journal.Load(ctx, path)
	if err != nil || len(loaded.Queued) != 1 || loaded.Queued[0].ID != first.ID {
		t.Fatalf("durable queue=%+v err=%v", loaded, err)
	}
	resumed, err := Open(ctx, Options{SessionDir: filepath.Dir(path), Cwd: cwd, Resume: &ResumeSession{Path: path}, Stack: &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resumed.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	if snapshot := resumed.Snapshot(); len(snapshot.Queues) != 1 || snapshot.Queues[0].ID != first.ID || snapshot.Run.Status != "idle" {
		t.Fatalf("resumed queue snapshot=%+v", snapshot)
	}
	provider.SetResponses(faux.Respond(faux.Text("consumed")))
	result, err := resumed.Continue(ctx)
	if err != nil || result.StopReason != agent.StopCompleted {
		t.Fatalf("continue=%+v %v", result, err)
	}
	if len(resumed.Snapshot().Queues) != 0 {
		t.Fatal("committed queue message stayed pending")
	}
	if err = resumed.CancelQueued(ctx, first.ID); !errors.Is(err, ErrQueueTooLate) {
		t.Fatalf("consumed cancel=%v", err)
	}
}
func TestSessionTaskHandlePrecedesCompletionAndRootWaits(t *testing.T) {
	root := faux.New(faux.Options{})
	child := faux.New(faux.Options{})
	childStarted := make(chan struct{})
	rootContinued := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	root.SetResponses(
		faux.Respond(faux.ToolCall("task", json.RawMessage(`{"prompt":"delegated work","subagent_type":"general"}`), "task_raw")),
		func(types.Context, *types.StreamOptions, faux.State, types.Model) (types.AssistantMessage, error) {
			close(rootContinued)
			return types.AssistantMessage{Content: []types.ContentBlock{types.NewText("root waiting")}}, nil
		},
		faux.Respond(faux.Text("root final")),
	)
	child.SetResponses(func(types.Context, *types.StreamOptions, faux.State, types.Model) (types.AssistantMessage, error) {
		close(childStarted)
		<-release
		return types.AssistantMessage{Content: []types.ContentBlock{types.NewText("CHILD_FINAL_ONLY")}}, nil
	})
	dispatch := func(ctx context.Context, model types.Model, input types.Context, options *types.SimpleStreamOptions) *stream.AssistantStream {
		if strings.Contains(input.SystemPrompt, "You are a delegated child.") {
			return child.StreamSimple(ctx, model, input, options)
		}
		return root.StreamSimple(ctx, model, input, options)
	}
	owner, err := Open(t.Context(), Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &NewSession{PermissionMode: permission.ModeBypass}, Stack: &StackConfig{Model: root.Model(), StreamFn: dispatch, EnableTask: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		releaseOnce.Do(func() { close(release) })
		if err := owner.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	_, updates, stop := owner.Watch(t.Context(), WatchOptions{Buffer: 512})
	defer stop()
	run, err := owner.Begin(t.Context(), Prompt{Text: "root task"})
	if err != nil {
		t.Fatal(err)
	}
	awaitSignal(t, childStarted)
	awaitSignal(t, rootContinued)
	select {
	case <-run.done:
		t.Fatal("root ended while its child was blocked")
	default:
	}
	page, err := owner.Jobs(t.Context(), "", 20)
	if err != nil || len(page.Jobs) != 1 {
		t.Fatalf("jobs=%+v %v", page, err)
	}
	job := page.Jobs[0]
	if job.Cwd != "" || job.ChildSessionID == "" || job.Kind != "task" || job.State != "running" {
		t.Fatalf("job summary=%+v", job)
	}
	childPath := filepath.Join(owner.core.store.Path()+".d", "children", job.ChildSessionID, "session.jsonl")
	if _, err = os.Stat(childPath); err != nil {
		t.Fatal(err)
	}
	releaseOnce.Do(func() { close(release) })
	result, err := run.Wait()
	if err != nil || result.StopReason != agent.StopCompleted || run.Text() != "root final" {
		t.Fatalf("root result=%+v %v text=%q", result, err, run.Text())
	}
	details, err := owner.WaitJobs(t.Context(), []string{job.ID}, time.Second)
	if err != nil || len(details) != 1 || details[0].State != "completed" || details[0].Result == nil || details[0].Isolation != "shared" {
		t.Fatalf("job details=%+v %v", details, err)
	}
	sawJob, sawCompletion, sawTerminal := false, false, false
	for !sawTerminal {
		select {
		case update := <-updates:
			if update.Kind == "tempest/job" {
				sawJob = true
			}
			if update.Kind == "tempest/queue" && strings.Contains(string(update.Update), `"action":"consumed"`) {
				sawCompletion = true
			}
			if update.Kind == "agent_message_chunk" && strings.Contains(string(update.Update), "CHILD_FINAL_ONLY") {
				t.Fatal("child transcript forwarded as root assistant")
			}
			if update.Kind == "tempest/run" && strings.Contains(string(update.Update), `"status":"finished"`) {
				sawTerminal = true
			}
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		}
	}
	if !sawJob || !sawCompletion {
		t.Fatalf("job=%v completion=%v", sawJob, sawCompletion)
	}
}
func awaitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(10 * time.Second):
		t.Fatal("background synchronization timed out")
	}
}

func TestDefaultStackHonorsConfiguredTaskOptIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, configured := range []struct {
		value   string
		enabled bool
	}{{"false", false}, {"true", true}} {
		t.Run(configured.value, func(t *testing.T) {
			t.Setenv("HARNESS_ENABLE_TASK", configured.value)
			cwd := t.TempDir()
			script := filepath.Join(cwd, "faux.json")
			if err := os.WriteFile(script, []byte(`[{"text":"done"}]`), 0600); err != nil {
				t.Fatal(err)
			}
			owner, err := Open(t.Context(), Options{SessionDir: t.TempDir(), Cwd: cwd, New: &NewSession{MCPServers: []MCPServerConfig{}}, FauxScript: script})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := owner.Close(context.Background()); err != nil {
					t.Error(err)
				}
			}()
			hasTask := false
			for _, tool := range owner.core.agent.State().Tools {
				hasTask = hasTask || tool.Name == "task"
			}
			if hasTask != configured.enabled {
				t.Fatalf("HARNESS_ENABLE_TASK=%s registered task=%t", configured.value, hasTask)
			}
		})
	}
}

// TestNamedBashRunsInBackgroundWithoutAsync replays the benchmark's call
// shape: bash with a name and no async starts a background job the model
// then waits for by name.
func TestNamedBashRunsInBackgroundWithoutAsync(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	// The job's completion follow-up opens one more turn after the first DONE.
	script := writeScript(t, `[{"toolCalls":[{"id":"bg","name":"bash","arguments":{"command":"sleep 0.3; printf ready > marker.txt","name":"make_marker","timeout":60}}]},{"toolCalls":[{"id":"w","name":"wait","arguments":{"names":["make_marker"],"timeout":30}}]},{"text":"DONE"},{"text":"DONE"}]`)
	owner, err := Open(t.Context(), Options{SessionDir: t.TempDir(), Cwd: cwd, New: &NewSession{PermissionMode: permission.ModeBypass}, FauxScript: script})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := owner.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	result, err := owner.Prompt(t.Context(), Prompt{Text: "run make_marker in the background"})
	if err != nil || result.StopReason != agent.StopCompleted {
		t.Fatalf("run=%+v %v", result, err)
	}
	results := toolResults(owner.Messages())
	handle, waited := results["bg"], results["w"]
	if handle.IsError || !strings.Contains(handle.Content[0].Text, `"name":"make_marker"`) || !strings.Contains(handle.Content[0].Text, `"kind":"bash"`) || strings.Contains(handle.Content[0].Text, `"state":"completed"`) {
		t.Fatalf("bash result is not a background job handle: %#v", handle)
	}
	if waited.IsError || !strings.Contains(waited.Content[0].Text, `"name":"make_marker"`) || !strings.Contains(waited.Content[0].Text, `"state":"completed"`) {
		t.Fatalf("wait by name = %#v", waited)
	}
	if marker, err := os.ReadFile(filepath.Join(cwd, "marker.txt")); err != nil || string(marker) != "ready" {
		t.Fatalf("marker=%q %v", marker, err)
	}
}
