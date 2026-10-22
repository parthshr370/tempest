package serve

import (
	"bufio"
	"bytes"
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

type a2aReply struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	} `json:"error"`
}

func interopServer(t *testing.T, f *faux.Faux, tools []tempest.Tool, mode permission.Mode) *Server {
	t.Helper()
	s, err := New(Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), A2ABaseURL: "http://127.0.0.1:9876", Token: "secret", DefaultNew: tempest.NewSession{PermissionMode: mode}, Stack: &tempest.StackConfig{Model: f.Model(), StreamFn: f.StreamSimple, ConfiguredTools: tools}})
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
func a2aRequest(method string, params any) *http.Request {
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	r := httptest.NewRequest("POST", "http://127.0.0.1:9876/a2a", bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("A2A-Version", "1.0")
	r.Header.Set("Authorization", "Bearer secret")
	return r
}
func a2aCall(t *testing.T, s *Server, method string, params any) a2aReply {
	t.Helper()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, a2aRequest(method, params))
	if w.Code != 200 {
		t.Fatalf("%s HTTP %d: %s", method, w.Code, w.Body.String())
	}
	var reply a2aReply
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	return reply
}
func a2aOK(t *testing.T, s *Server, method string, params, result any) {
	t.Helper()
	reply := a2aCall(t, s, method, params)
	if reply.Error != nil {
		t.Fatalf("%s: %s", method, reply.Error.Data)
	}
	if result != nil {
		if err := json.Unmarshal(reply.Result, result); err != nil {
			t.Fatal(err)
		}
	}
}
func sendParams(text, contextID string, immediate bool) map[string]any {
	return map[string]any{"message": map[string]any{"messageId": "client-id", "role": "ROLE_USER", "parts": []any{map[string]any{"text": text}}, "contextId": contextID}, "configuration": map[string]any{"returnImmediately": immediate}}
}
func admitted(t *testing.T, s *Server, params any) *model.Task {
	t.Helper()
	var out struct {
		Task *model.Task `json:"task"`
	}
	a2aOK(t, s, "SendMessage", params, &out)
	if out.Task == nil || out.Task.ID == "" || out.Task.ContextID == "" {
		t.Fatalf("not admitted: %+v", out)
	}
	return out.Task
}
func getTask(t *testing.T, s *Server, id model.TaskID) *model.Task {
	t.Helper()
	var out model.Task
	a2aOK(t, s, "GetTask", map[string]any{"id": id}, &out)
	return &out
}

func TestA2ACardAndEnvelope(t *testing.T) {
	f := faux.New(faux.Options{})
	s := interopServer(t, f, nil, permission.ModeBypass)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "http://127.0.0.1:9876/.well-known/agent-card.json", nil)
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 || w.Header().Get("ETag") == "" {
		t.Fatalf("card: %d %s", w.Code, w.Body.String())
	}
	var card map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &card); err != nil {
		t.Fatal(err)
	}
	if card["name"] != "Tempest" || strings.Contains(w.Body.String(), s.opts.Cwd) || !strings.Contains(w.Body.String(), `"httpAuthSecurityScheme":{"scheme":"Bearer"}`) || !strings.Contains(w.Body.String(), `"schemes":{"bearer":{}}`) {
		t.Fatalf("card shape: %s", w.Body.String())
	}
	iface := card["supportedInterfaces"].([]any)[0].(map[string]any)
	if iface["url"] != "http://127.0.0.1:9876/a2a" || iface["protocolVersion"] != "1.0" || iface["protocolBinding"] != "JSONRPC" {
		t.Fatal(iface)
	}
	r.Header.Set("If-None-Match", w.Header().Get("ETag"))
	cached := httptest.NewRecorder()
	s.Handler().ServeHTTP(cached, r)
	if cached.Code != 304 {
		t.Fatal(cached.Code)
	}
	for _, test := range []struct {
		name         string
		alter        func(*http.Request)
		status, code int
	}{
		{"token", func(r *http.Request) { r.Header.Set("Authorization", "Bearer wrong") }, 401, 0},
		{"query-token", func(r *http.Request) { r.Header.Del("Authorization"); r.URL.RawQuery = "token=secret" }, 401, 0},
		{"origin", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, 403, 0},
		{"version-missing", func(r *http.Request) { r.Header.Del("A2A-Version") }, 200, -32009},
		{"version-old", func(r *http.Request) { r.Header.Set("A2A-Version", "0.3") }, 200, -32009},
		{"method", func(r *http.Request) { r.Method = "GET" }, 405, 0},
		{"media", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 415, 0},
		{"optional-extension", func(r *http.Request) { r.Header.Set("A2A-Extensions", "https://unknown.example") }, 200, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := a2aRequest("ListTasks", map[string]any{})
			test.alter(r)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
			}
			if test.code != 0 {
				var reply a2aReply
				_ = json.Unmarshal(w.Body.Bytes(), &reply)
				if reply.Error == nil || reply.Error.Code != test.code {
					t.Fatal(w.Body.String())
				}
			}
		})
	}
	for _, test := range []struct {
		raw  string
		code int
	}{
		{`{`, -32700}, {`[]`, -32600}, {`{"jsonrpc":"2.0","id":null,"method":"ListTasks","params":{}}`, -32600}, {`{"jsonrpc":"2.0","id":1,"method":"ListTasks","params":{}} {}`, -32600},
		{`{"jsonrpc":"2.0","id":1,"method":"ListTasks","params":{"tenant":"other"}}`, -32004},
		{`{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m","role":"ROLE_USER","parts":[{"text":"x","data":true}]}}}`, -32602},
		{`{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m","role":"ROLE_USER","parts":[{"data":{"approve":true}}]}}}`, -32005},
		{`{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m","role":"ROLE_USER","parts":[{"text":"x","mediaType":"text/html"}]}}}`, -32005},
		{`{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"configuration":{"taskPushNotificationConfig":{}},"message":{"messageId":"m","role":"ROLE_USER","parts":[{"text":"x"}]}}}`, -32003},
	} {
		r := a2aRequest("ListTasks", map[string]any{})
		r.Body = io.NopCloser(strings.NewReader(test.raw))
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		var reply a2aReply
		_ = json.Unmarshal(w.Body.Bytes(), &reply)
		if reply.Error == nil || reply.Error.Code != test.code {
			t.Fatalf("%s => %s; want %d", test.raw, w.Body.String(), test.code)
		}
	}
	if len(s.sessions) != 0 {
		t.Fatal("validation opened a session")
	}
	oversized := a2aRequest("ListTasks", map[string]any{})
	oversized.Body = io.NopCloser(strings.NewReader(strings.Repeat(" ", (1<<20)+1)))
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, oversized)
	if w.Code != 413 {
		t.Fatal(w.Code)
	}
	health := httptest.NewRecorder()
	s.Handler().ServeHTTP(health, httptest.NewRequest("GET", "/healthz", nil))
	if health.Code != 200 {
		t.Fatal(health.Code)
	}
}

func TestA2AStateArtifactAndOldCancel(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("first answer")), faux.Respond(faux.Text("second answer")))
	s := interopServer(t, f, nil, permission.ModeBypass)
	first := admitted(t, s, sendParams("first", "", false))
	if first.Status.State != model.TaskStateCompleted || first.Artifacts[0].Parts[0].Text() != "first answer" {
		t.Fatalf("first=%+v", first)
	}
	session, release, err := s.Acquire(first.ContextID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if session.Snapshot().LastRun.RunID != string(first.ID) {
		t.Fatal("task id isn't durable run id")
	}
	second := admitted(t, s, sendParams("second", first.ContextID, false))
	if second.ID == first.ID || second.ContextID != first.ContextID {
		t.Fatal("run/context identity changed")
	}
	if got := getTask(t, s, first.ID).Artifacts[0].Parts[0].Text(); got != "first answer" {
		t.Fatalf("old text overwritten: %q", got)
	}
	if reply := a2aCall(t, s, "CancelTask", map[string]any{"id": first.ID}); reply.Error == nil || reply.Error.Code != -32002 {
		t.Fatal("old cancel accepted")
	}
	if reply := a2aCall(t, s, "SubscribeToTask", map[string]any{"id": first.ID}); reply.Error == nil || reply.Error.Code != -32004 {
		t.Fatal("terminal subscribe accepted")
	}
	var history model.Task
	a2aOK(t, s, "GetTask", map[string]any{"id": first.ID, "historyLength": 0}, &history)
	if len(history.History) != 0 {
		t.Fatal("historyLength=0 ignored")
	}
	if len(first.History) != 2 || first.History[0].ID != "client-id" || first.History[1].Role != model.MessageRoleAgent {
		t.Fatal("task history", first.History)
	}
}

func TestA2APermissionViaACPOnly(t *testing.T) {
	for _, choice := range []string{"allow_once", "reject_once", "cancel"} {
		t.Run(choice, func(t *testing.T) {
			f := faux.New(faux.Options{})
			f.SetResponses(faux.Respond(faux.Text("narration"), faux.Thinking("PRIVATE THOUGHT"), faux.ToolCall("mcp__fixture__work", json.RawMessage(`{"credential":"PRIVATE INPUT"}`), "call-one")), faux.Respond(faux.Text("final answer")))
			var ledger atomic.Int32
			tool := tempest.Tool{Tool: types.Tool{Name: "mcp__fixture__work", Parameters: json.RawMessage(`{"type":"object"}`)}, Execute: func(context.Context, string, json.RawMessage, agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
				ledger.Add(1)
				return agent.AgentToolResult{Content: []types.ContentBlock{types.NewText("PRIVATE OUTPUT")}}, nil
			}}
			s := interopServer(t, f, []tempest.Tool{tool}, permission.ModeDefault)
			task := admitted(t, s, sendParams("work", "", false))
			if task.Status.State != model.TaskStateInputRequired || ledger.Load() != 0 {
				t.Fatalf("state %s ledger %d", task.Status.State, ledger.Load())
			}
			if !strings.Contains(task.Status.Message.Parts[0].Text(), "tempest/permission") {
				t.Fatal("approval route missing")
			}
			for _, text := range []string{"yes", "approve=true"} {
				params := sendParams(text, task.ContextID, false)
				params["message"].(map[string]any)["taskId"] = task.ID
				reply := a2aCall(t, s, "SendMessage", params)
				if reply.Error == nil || reply.Error.Code != -32004 || !strings.Contains(string(reply.Error.Data), "permission_via_acp") {
					t.Fatalf("followup: %+v", reply)
				}
			}
			bytes, _ := json.Marshal(getTask(t, s, task.ID))
			for _, secret := range []string{"PRIVATE INPUT", "PRIVATE OUTPUT", "PRIVATE THOUGHT"} {
				if strings.Contains(string(bytes), secret) {
					t.Fatalf("leaked %s", secret)
				}
			}
			if choice == "cancel" {
				var canceled model.Task
				a2aOK(t, s, "CancelTask", map[string]any{"id": task.ID}, &canceled)
				if canceled.Status.State != model.TaskStateCanceled {
					t.Fatal(canceled.Status.State)
				}
			} else {
				c := connect(t, s)
				snapshot := watchSession(t, c, task.ContextID, true)
				if len(snapshot.PendingAsks) != 1 {
					t.Fatal(snapshot.PendingAsks)
				}
				call(t, c, "tempest/permission", map[string]any{"sessionId": task.ContextID, "toolCallId": "call-one", "optionId": choice}, nil)
				deadline := time.Now().Add(5 * time.Second)
				for {
					task = getTask(t, s, task.ID)
					if task.Status.State.Terminal() {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("permission never resolved")
					}
					time.Sleep(time.Millisecond)
				}
				if task.Status.State != model.TaskStateCompleted || task.Artifacts[0].Parts[0].Text() != "final answer" {
					t.Fatal(task)
				}
			}
			want := int32(0)
			if choice == "allow_once" {
				want = 1
			}
			if ledger.Load() != want {
				t.Fatalf("ledger %d want %d", ledger.Load(), want)
			}
		})
	}
}

func TestA2AStreamingIndependentAndCancel(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.ToolCall("work", json.RawMessage(`{}`), "block")), faux.Respond(faux.Text("answer")))
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
	httpServer := httptest.NewServer(s.Handler())
	defer httpServer.Close()
	task := admitted(t, s, sendParams("start", "", true))
	<-entered
	if task.Status.State != model.TaskStateWorking {
		t.Fatal(task.Status.State)
	}
	subscribe := func() (*http.Response, *bufio.Reader) {
		r := a2aRequest("SubscribeToTask", map[string]any{"id": task.ID})
		r.URL.Scheme = "http"
		r.URL.Host = strings.TrimPrefix(httpServer.URL, "http://")
		r.RequestURI = ""
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return res, bufio.NewReader(res.Body)
	}
	read := func(reader *bufio.Reader) string {
		t.Helper()
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		blank, err := reader.ReadString('\n')
		if err != nil || blank != "\n" {
			t.Fatalf("SSE separator %q %v", blank, err)
		}
		return line
	}
	a, ar := subscribe()
	b, br := subscribe()
	if !strings.Contains(read(ar), `"task":`) || !strings.Contains(read(br), `"task":`) {
		t.Fatal("first event isn't Task")
	}
	_ = a.Body.Close()
	close(release)
	var events []string
	for {
		line, err := br.ReadString('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "data: ") {
			events = append(events, line)
		}
	}
	_ = b.Body.Close()
	artifact, terminal := -1, -1
	for i, line := range events {
		if strings.Contains(line, `"artifactUpdate":`) {
			artifact = i
		}
		if strings.Contains(line, `TASK_STATE_COMPLETED`) {
			terminal = i
		}
		for _, bad := range []string{`"final":`, `"kind":`, "[DONE]"} {
			if strings.Contains(line, bad) {
				t.Fatal(line)
			}
		}
	}
	if artifact < 0 || terminal <= artifact {
		t.Fatalf("event order: %v", events)
	}
	if getTask(t, s, task.ID).Status.State != model.TaskStateCompleted {
		t.Fatal("disconnect canceled run")
	}
}

func TestA2AListPagingAndRestart(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("one")), faux.Respond(faux.Text("two")), faux.Respond(faux.Text("three")))
	s := interopServer(t, f, nil, permission.ModeBypass)
	one := admitted(t, s, sendParams("1", "", false))
	_ = admitted(t, s, sendParams("2", one.ContextID, false))
	other := admitted(t, s, sendParams("3", "", false))
	var first model.ListTasksResponse
	a2aOK(t, s, "ListTasks", map[string]any{"pageSize": 1, "historyLength": 0}, &first)
	if first.TotalSize != 3 || len(first.Tasks) != 1 || first.NextPageToken == "" || len(first.Tasks[0].Artifacts) > 0 || len(first.Tasks[0].History) > 0 {
		t.Fatalf("page=%+v", first)
	}
	token := first.NextPageToken
	seen := map[model.TaskID]bool{first.Tasks[0].ID: true}
	for token != "" {
		var page model.ListTasksResponse
		a2aOK(t, s, "ListTasks", map[string]any{"pageSize": 1, "historyLength": 0, "pageToken": token}, &page)
		for _, v := range page.Tasks {
			if seen[v.ID] {
				t.Fatal("overlap")
			}
			seen[v.ID] = true
		}
		token = page.NextPageToken
	}
	if len(seen) != 3 {
		t.Fatal(seen)
	}
	for _, p := range []map[string]any{{"pageSize": 1, "historyLength": 0, "pageToken": first.NextPageToken + "x"}, {"pageSize": 2, "historyLength": 0, "pageToken": first.NextPageToken}, {"pageSize": 0}, {"status": "working"}, {"statusTimestampAfter": "bad"}} {
		reply := a2aCall(t, s, "ListTasks", p)
		if reply.Error == nil || reply.Error.Code != -32602 {
			t.Fatalf("invalid cursor/filter accepted: %v", p)
		}
	}
	var filtered model.ListTasksResponse
	a2aOK(t, s, "ListTasks", map[string]any{"contextId": one.ContextID, "status": "TASK_STATE_COMPLETED", "includeArtifacts": true, "historyLength": 1}, &filtered)
	if filtered.TotalSize != 2 || len(filtered.Tasks[0].History) != 1 || len(filtered.Tasks[0].Artifacts) != 1 {
		t.Fatalf("filters %+v", filtered)
	}
	opts := s.opts
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.SetResponses(faux.Respond(faux.Text("resumed")))
	next, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close(context.Background())
	restored := getTask(t, next, other.ID)
	if restored.ID != other.ID || restored.ContextID != other.ContextID || restored.Status.State != model.TaskStateCompleted {
		t.Fatal("durable exact run didn't survive restart", restored)
	}
	var restartedList model.ListTasksResponse
	a2aOK(t, next, "ListTasks", map[string]any{"historyLength": 0}, &restartedList)
	if restartedList.TotalSize != 0 {
		t.Fatal("process-local list claimed durable history")
	}
	resumed := admitted(t, next, sendParams("after restart", one.ContextID, false))
	if resumed.ContextID != one.ContextID || resumed.ID == one.ID {
		t.Fatal("durable context not resumed")
	}
}
