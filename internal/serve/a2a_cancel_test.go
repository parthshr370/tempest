package serve

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	model "github.com/a2aproject/a2a-go/v2/a2a"
	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/tempest"
)

func TestA2AOldTaskCancelCannotReachNextRun(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("first")), faux.Respond(faux.ToolCall("work", json.RawMessage(`{}`), "second")))
	entered, aborted := make(chan struct{}), make(chan struct{})
	tool := tempest.Tool{Tool: types.Tool{Name: "work", Parameters: json.RawMessage(`{"type":"object"}`)}, Execute: func(ctx context.Context, _ string, _ json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
		close(entered)
		<-ctx.Done()
		close(aborted)
		return agent.AgentToolResult{}, ctx.Err()
	}}
	s := interopServer(t, f, []tempest.Tool{tool}, permission.ModeBypass)
	old := admitted(t, s, sendParams("one", "", false))
	current := admitted(t, s, sendParams("two", old.ContextID, true))
	<-entered
	reply := a2aCall(t, s, "CancelTask", map[string]any{"id": old.ID})
	if reply.Error == nil || reply.Error.Code != -32002 {
		t.Fatal("old task cancel accepted")
	}
	select {
	case <-aborted:
		t.Fatal("old task canceled newer run")
	default:
	}
	var terminal model.Task
	a2aOK(t, s, "CancelTask", map[string]any{"id": current.ID}, &terminal)
	if terminal.Status.State != model.TaskStateCanceled {
		t.Fatal(terminal.Status.State)
	}
	select {
	case <-aborted:
	default:
		t.Fatal("cancel returned before joining exact run")
	}
	if reply := a2aCall(t, s, "CancelTask", map[string]any{"id": current.ID}); reply.Error == nil || reply.Error.Code != -32002 {
		t.Fatal("terminal cancel accepted")
	}
}

func TestA2ADisconnectedUnaryKeepsAcceptedRun(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.ToolCall("work", json.RawMessage(`{}`), "block")), faux.Respond(faux.Text("survived disconnect")))
	entered, release := make(chan struct{}), make(chan struct{})
	tool := tempest.Tool{Tool: types.Tool{Name: "work", Parameters: json.RawMessage(`{"type":"object"}`)}, Execute: func(ctx context.Context, _ string, _ json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
		close(entered)
		select {
		case <-release:
			return agent.AgentToolResult{}, nil
		case <-ctx.Done():
			return agent.AgentToolResult{}, ctx.Err()
		}
	}}
	s := interopServer(t, f, []tempest.Tool{tool}, permission.ModeBypass)
	ctx, cancel := context.WithCancel(context.Background())
	request := a2aRequest("SendMessage", sendParams("work", "", false)).WithContext(ctx)
	returned := make(chan struct{})
	go func() { s.Handler().ServeHTTP(httptest.NewRecorder(), request); close(returned) }()
	<-entered
	cancel()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("disconnected unary still waiting")
	}
	var list model.ListTasksResponse
	a2aOK(t, s, "ListTasks", map[string]any{}, &list)
	if len(list.Tasks) != 1 || list.Tasks[0].Status.State != model.TaskStateWorking {
		t.Fatal("accepted task vanished")
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		task := getTask(t, s, list.Tasks[0].ID)
		if task.Status.State.Terminal() {
			if task.Status.State != model.TaskStateCompleted || !strings.Contains(task.Artifacts[0].Parts[0].Text(), "survived") {
				t.Fatal(task)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run didn't finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestA2AShutdownCapturesCanceledTask(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.ToolCall("work", json.RawMessage(`{}`), "block")))
	entered := make(chan struct{})
	tool := tempest.Tool{Tool: types.Tool{Name: "work", Parameters: json.RawMessage(`{"type":"object"}`)}, Execute: func(ctx context.Context, _ string, _ json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
		close(entered)
		<-ctx.Done()
		return agent.AgentToolResult{}, ctx.Err()
	}}
	s := interopServer(t, f, []tempest.Tool{tool}, permission.ModeBypass)
	task := admitted(t, s, sendParams("work", "", true))
	<-entered
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Inspect the adapter directly: the shared HTTP middleware has stopped admission.
	w := httptest.NewRecorder()
	s.a2a.Handler().ServeHTTP(w, a2aRequest("GetTask", map[string]any{"id": task.ID}))
	var reply struct {
		Result model.Task `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Result.Status.State != model.TaskStateCanceled {
		t.Fatal(w.Body.String())
	}
}
