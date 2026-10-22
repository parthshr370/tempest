package serve

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	model "github.com/a2aproject/a2a-go/v2/a2a"
	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/tempest"
)

func TestA2APermissionSSEWorkingAndTerminal(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Thinking("PRIVATE THOUGHT"), faux.ToolCall("mcp__fixture__work", json.RawMessage(`{"credential":"PRIVATE INPUT"}`), "permission")), faux.Respond(faux.Text("finished")))
	var ledger atomic.Int32
	tool := tempest.Tool{Tool: types.Tool{Name: "mcp__fixture__work", Parameters: json.RawMessage(`{"type":"object"}`)}, Execute: func(context.Context, string, json.RawMessage, agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
		ledger.Add(1)
		return agent.AgentToolResult{Content: []types.ContentBlock{types.NewText("PRIVATE OUTPUT")}}, nil
	}}
	s := interopServer(t, f, []tempest.Tool{tool}, permission.ModeDefault)
	task := admitted(t, s, sendParams("run", "", false))
	host := httptest.NewServer(s.Handler())
	defer host.Close()
	request := a2aRequest("SubscribeToTask", map[string]any{"id": task.ID})
	request.URL.Host = strings.TrimPrefix(host.URL, "http://")
	request.RequestURI = ""
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	first, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(first, `"task":`) || !strings.Contains(first, "TASK_STATE_INPUT_REQUIRED") {
		t.Fatalf("first %s %v", first, err)
	}
	if response.Header.Get("Content-Type") != "text/event-stream" || response.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatal(response.Header)
	}
	c := connect(t, s)
	watchSession(t, c, task.ContextID, true)
	call(t, c, "tempest/permission", map[string]any{"sessionId": task.ContextID, "toolCallId": "permission", "optionId": "allow_once"}, nil)
	remaining, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	all := first + string(remaining)
	if !strings.Contains(all, "TASK_STATE_WORKING") || !strings.Contains(all, "TASK_STATE_COMPLETED") || !strings.Contains(all, `"artifactUpdate":`) {
		t.Fatal(all)
	}
	if ledger.Load() != 1 {
		t.Fatal(ledger.Load())
	}
	for _, secret := range []string{"PRIVATE INPUT", "PRIVATE OUTPUT", "PRIVATE THOUGHT"} {
		if strings.Contains(all, secret) {
			t.Fatal("SSE leaked " + secret)
		}
	}
}

func TestA2APermissionTimeoutDeniesWithoutACP(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.ToolCall("mcp__fixture__work", json.RawMessage(`{}`), "permission")), faux.Respond(faux.Text("denied safely")))
	var ledger atomic.Int32
	tool := tempest.Tool{Tool: types.Tool{Name: "mcp__fixture__work", Parameters: json.RawMessage(`{"type":"object"}`)}, Execute: func(context.Context, string, json.RawMessage, agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
		ledger.Add(1)
		return agent.AgentToolResult{}, nil
	}}
	clock := &testClock{created: make(chan *testTimer, 1)}
	s, err := New(Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), A2ABaseURL: "http://127.0.0.1:9876", Token: "secret", Clock: clock, DefaultNew: tempest.NewSession{PermissionMode: permission.ModeDefault}, Stack: &tempest.StackConfig{Model: f.Model(), StreamFn: f.StreamSimple, ConfiguredTools: []tempest.Tool{tool}}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	task := admitted(t, s, sendParams("run", "", false))
	timer := <-clock.created
	timer.ch <- time.Now()
	deadline := time.Now().Add(5 * time.Second)
	for {
		task = getTask(t, s, task.ID)
		if task.Status.State.Terminal() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("permission timeout didn't settle")
		}
		time.Sleep(time.Millisecond)
	}
	if ledger.Load() != 0 || task.Status.State != model.TaskStateCompleted {
		t.Fatal("timeout granted tool or rejected completed task")
	}
}
