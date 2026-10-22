package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/rpc"
	"go.harness.dev/harness/tempest"
)

type client struct {
	conn     *rpc.Conn
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan error
	stopOnce sync.Once
}

func connect(t *testing.T, opts Options, revision string, capabilities any, handler rpc.Handler) *client {
	t.Helper()
	left, right := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	c := &client{conn: rpc.NewStream(left), ctx: ctx, cancel: cancel, done: make(chan error, 1)}
	go func() { c.done <- Serve(ctx, right, opts) }()
	go func() { _ = c.conn.Serve(handler) }()
	t.Cleanup(func() { c.stop(t) })
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := c.conn.Call(ctx, "initialize", map[string]any{"protocolVersion": revision, "capabilities": capabilities, "clientInfo": map[string]string{"name": "test", "version": "1"}}, &init); err != nil {
		t.Fatal(err)
	}
	want := revision
	if want != "2025-11-25" && want != "2025-06-18" {
		want = "2025-11-25"
	}
	if init.ProtocolVersion != want {
		t.Fatalf("revision=%s", init.ProtocolVersion)
	}
	if err := c.conn.Notify(ctx, "notifications/initialized", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	return c
}

func (c *client) stop(t *testing.T) {
	t.Helper()
	c.stopOnce.Do(func() {
		c.cancel()
		_ = c.conn.Close()
		select {
		case err := <-c.done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("MCP server didn't stop")
		}
	})
}
func options(t *testing.T, f *faux.Faux) Options {
	return Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), DefaultNew: tempest.NewSession{Model: f.Model().ID}, Stack: &tempest.StackConfig{Model: f.Model(), StreamFn: f.StreamSimple}, ShutdownTimeout: time.Second}
}
func (c *client) run(in any, token any) (toolResult, error) {
	params := map[string]any{"name": "tempest_run", "arguments": in}
	if token != nil {
		params["_meta"] = map[string]any{"progressToken": token}
	}
	var out toolResult
	err := c.conn.Call(c.ctx, "tools/call", params, &out)
	return out, err
}

func TestMCPServerRunResumeAndCaps(t *testing.T) {
	f := faux.New(faux.Options{})
	f.AppendResponses(faux.Respond(faux.ToolCall("ls", json.RawMessage(`{}`), "list")), faux.Respond(faux.Text("first")))
	f.AppendResponses(func(c types.Context, _ *types.StreamOptions, _ faux.State, _ types.Model) (types.AssistantMessage, error) {
		if len(c.Messages) < 5 {
			return types.AssistantMessage{}, fmt.Errorf("conversation didn't resume: %d messages", len(c.Messages))
		}
		return types.AssistantMessage{Content: []types.ContentBlock{types.NewText("resumed")}}, nil
	})
	opts := options(t, f)
	opts.DefaultNew.Budget.MaxTurns = 3
	var progress atomic.Int32
	c := connect(t, opts, "unsupported", map[string]any{}, func(_ context.Context, method string, _ json.RawMessage) (any, *rpc.Error) {
		if method == "notifications/progress" {
			progress.Add(1)
		}
		return nil, nil
	})
	var list struct {
		Tools      []json.RawMessage `json:"tools"`
		NextCursor string            `json:"nextCursor"`
	}
	if err := c.conn.Call(c.ctx, "tools/list", map[string]any{}, &list); err != nil || len(list.Tools) != 1 {
		t.Fatalf("tools=%+v err=%v", list, err)
	}
	first, err := c.run(RunInput{Prompt: "go"}, nil)
	if err != nil || first.IsError || first.StructuredContent == nil || first.StructuredContent.Text != "first" || first.StructuredContent.RunID == "" || first.StructuredContent.SessionID == "" || first.StructuredContent.Turns != 2 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := c.run(RunInput{Prompt: "again", SessionID: first.StructuredContent.SessionID}, nil)
	if err != nil || second.IsError || second.StructuredContent.Text != "resumed" || second.StructuredContent.SessionID != first.StructuredContent.SessionID {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	if progress.Load() != 0 {
		t.Fatal("tokenless call emitted progress")
	}
	for _, in := range []any{map[string]any{"prompt": " "}, map[string]any{"prompt": "go", "cwd": "relative"}, map[string]any{"prompt": "go", "budget": map[string]any{"maxTurns": 0}}, map[string]any{"prompt": "go", "budget": map[string]any{"maxTurns": 4}}, map[string]any{"prompt": "go", "budget": map[string]any{"maxWallSeconds": 1e100}}, map[string]any{"prompt": "go", "permissionMode": "bypass"}} {
		if _, err := c.run(in, nil); err == nil {
			t.Fatalf("invalid input accepted: %#v", in)
		}
	}
	for _, in := range []RunInput{{Prompt: "go", SessionID: "unknown"}, {Prompt: "go", SessionID: first.StructuredContent.SessionID, Cwd: t.TempDir()}, {Prompt: "go", SessionID: first.StructuredContent.SessionID, Model: "other"}} {
		out, err := c.run(in, nil)
		if err != nil || !out.IsError || out.StructuredContent != nil {
			t.Fatalf("setup failure=%+v err=%v", out, err)
		}
	}
}

func TestMCPServerElicitationCapabilitiesAndDecisions(t *testing.T) {
	for _, test := range []struct {
		name, revision string
		capability     any
		answer         any
		allow          bool
		asks           int
		timeout        bool
		override       permission.Decision
		local          bool
	}{
		{name: "november empty", revision: "2025-11-25", capability: map[string]any{"elicitation": map[string]any{}}, answer: map[string]any{"action": "accept", "content": map[string]bool{"allow": true}}, allow: true, asks: 1},
		{name: "november form", revision: "2025-11-25", capability: map[string]any{"elicitation": map[string]any{"form": map[string]any{}}}, answer: map[string]any{"action": "accept", "content": map[string]bool{"allow": true}}, allow: true, asks: 1},
		{name: "june", revision: "2025-06-18", capability: map[string]any{"elicitation": map[string]any{}}, answer: map[string]any{"action": "accept", "content": map[string]bool{"allow": true}}, allow: true, asks: 1},
		{name: "url only", revision: "2025-11-25", capability: map[string]any{"elicitation": map[string]any{"url": map[string]any{}}}},
		{name: "absent", revision: "2025-11-25", capability: map[string]any{}},
		{name: "decline", revision: "2025-11-25", capability: map[string]any{"elicitation": map[string]any{}}, answer: map[string]string{"action": "decline"}, asks: 1},
		{name: "cancel", revision: "2025-11-25", capability: map[string]any{"elicitation": map[string]any{}}, answer: map[string]string{"action": "cancel"}, asks: 1},
		{name: "malformed", revision: "2025-11-25", capability: map[string]any{"elicitation": map[string]any{}}, answer: map[string]any{"action": "accept", "content": map[string]string{"allow": "true"}}, asks: 1},
		{name: "false", revision: "2025-11-25", capability: map[string]any{"elicitation": map[string]any{}}, answer: map[string]any{"action": "accept", "content": map[string]bool{"allow": false}}, asks: 1},
		{name: "timeout", revision: "2025-11-25", capability: map[string]any{"elicitation": map[string]any{}}, asks: 1, timeout: true},
		{name: "local allow", revision: "2025-11-25", capability: map[string]any{}, override: permission.DecideAllow, allow: true, local: true},
		{name: "local deny", revision: "2025-11-25", capability: map[string]any{"elicitation": map[string]any{}}, override: permission.DecideDeny, local: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := faux.New(faux.Options{})
			opts := options(t, f)
			target := filepath.Join(opts.Cwd, "approved")
			const toolName = "mcp__fixture__mkdir"
			opts.DefaultNew.Tools = []tempest.Tool{{Tool: tempest.ToolDefinition{Name: toolName, Description: "Create the test directory", Parameters: json.RawMessage(`{"type":"object","additionalProperties":false}`)}, Execute: func(ctx context.Context, _ string, _ json.RawMessage, _ tempest.ToolUpdateCallback) (tempest.ToolResult, error) {
				if err := ctx.Err(); err != nil {
					return tempest.ToolResult{}, err
				}
				return tempest.ToolResult{Content: []tempest.ContentBlock{{Type: "text", Text: "created"}}}, os.Mkdir(target, 0700)
			}}}
			f.AppendResponses(faux.Respond(faux.ToolCall(toolName, json.RawMessage(`{}`), "permission-call")), faux.Respond(faux.Text("done")))
			if test.local {
				opts.Stack.PermissionPolicy.Overrides = map[string]permission.Decision{toolName: test.override}
			}
			opts.PermissionTimeout = 20 * time.Millisecond
			var asks atomic.Int32
			c := connect(t, opts, test.revision, test.capability, func(ctx context.Context, method string, raw json.RawMessage) (any, *rpc.Error) {
				if method != "elicitation/create" {
					return nil, nil
				}
				asks.Add(1)
				var params map[string]any
				_ = json.Unmarshal(raw, &params)
				if test.revision == "2025-06-18" {
					if _, ok := params["mode"]; ok {
						t.Error("June elicitation included mode")
					}
				} else if params["mode"] != "form" {
					t.Error("November form mode missing")
				}
				if test.timeout {
					<-ctx.Done()
					return nil, nil
				}
				return test.answer, nil
			})
			out, err := c.run(RunInput{Prompt: "run tool"}, nil)
			if err != nil || out.IsError {
				t.Fatalf("run=%+v err=%v", out, err)
			}
			_, statErr := os.Stat(target)
			if (statErr == nil) != test.allow {
				t.Fatalf("side effect=%v want allow=%v", statErr, test.allow)
			}
			if int(asks.Load()) != test.asks {
				t.Fatalf("asks=%d want %d", asks.Load(), test.asks)
			}
		})
	}
}

func TestMCPServerRawCancellationOrdering(t *testing.T) {
	f := faux.New(faux.Options{})
	f.AppendResponses(faux.Respond(faux.Text("later")))
	opts := options(t, f)
	left, right := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, right, opts) }()
	defer left.Close()
	scanner := bufio.NewScanner(left)
	write := func(data string) {
		t.Helper()
		if _, err := left.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	write("{\"jsonrpc\":\"2.0\",\"id\":0,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-11-25\",\"capabilities\":{}}}\n")
	if !scanner.Scan() {
		t.Fatal("initialize reply missing")
	}
	write("{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n" +
		"{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"tempest_run\",\"arguments\":{\"prompt\":\"cancel\"},\"_meta\":{\"progressToken\":\"cancel\"}}}\n" +
		"{\"jsonrpc\":\"2.0\",\"method\":\"notifications/cancelled\",\"params\":{\"requestId\":1}}\n" +
		"{\"jsonrpc\":\"2.0\",\"id\":\"1\",\"method\":\"ping\"}\n")
	if !scanner.Scan() {
		t.Fatal("ping missing")
	}
	var frame map[string]any
	_ = json.Unmarshal(scanner.Bytes(), &frame)
	if frame["id"] != "1" {
		t.Fatalf("cancelled numeric request replied or sent progress: %s", scanner.Bytes())
	}
	// A different JSON ID is independent, and an unknown cancellation can't poison it.
	write("{\"jsonrpc\":\"2.0\",\"method\":\"notifications/cancelled\",\"params\":{\"requestId\":999}}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"tempest_run\",\"arguments\":{\"prompt\":\"new\"}}}\n")
	if !scanner.Scan() {
		t.Fatal("new call missing")
	}
	if !strings.Contains(scanner.Text(), `"stopReason":"completed"`) {
		t.Fatalf("new call=%s", scanner.Text())
	}
	_ = left.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("EOF didn't join server")
	}
}

func TestMCPServerProgressPrecedesResponseAndBusy(t *testing.T) {
	f := faux.New(faux.Options{})
	opts := options(t, f)
	target := filepath.Join(opts.Cwd, "done")
	args, _ := json.Marshal(map[string]string{"command": "mkdir -p " + target})
	f.AppendResponses(faux.Respond(faux.ToolCall("bash", args, "ask")), faux.Respond(faux.Text("done")))
	asking := make(chan struct{})
	release := make(chan struct{})
	var eventsMu sync.Mutex
	var events []int
	c := connect(t, opts, "2025-11-25", map[string]any{"elicitation": map[string]any{}}, func(ctx context.Context, method string, raw json.RawMessage) (any, *rpc.Error) {
		if method == "notifications/progress" {
			var p struct {
				Progress int `json:"progress"`
			}
			_ = json.Unmarshal(raw, &p)
			eventsMu.Lock()
			events = append(events, p.Progress)
			eventsMu.Unlock()
		}
		if method == "elicitation/create" {
			close(asking)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return map[string]any{"action": "accept", "content": map[string]bool{"allow": true}}, nil
		}
		return nil, nil
	})
	done := make(chan toolResult, 1)
	go func() {
		out, err := c.run(RunInput{Prompt: "go"}, "progress")
		if err != nil {
			t.Error(err)
		}
		done <- out
	}()
	<-asking
	// The initial progress contains the live id; session lookup stays mutex-protected.
	var id string
	err := filepath.WalkDir(opts.SessionDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".jsonl") {
			base := strings.TrimSuffix(d.Name(), ".jsonl")
			id = base[strings.LastIndex(base, "_")+1:]
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	busy, err := c.run(RunInput{Prompt: "busy", SessionID: id}, nil)
	if err != nil || !busy.IsError {
		t.Fatalf("busy=%+v err=%v id=%s", busy, err, id)
	}
	close(release)
	out := <-done
	if out.IsError || out.StructuredContent == nil {
		t.Fatalf("result=%+v", out)
	}
	// Ping is ordered after the queued final response/progress frames.
	if err := c.conn.Call(c.ctx, "ping", nil, nil); err != nil {
		t.Fatal(err)
	}
	eventsMu.Lock()
	defer eventsMu.Unlock()
	if len(events) < 2 {
		t.Fatalf("progress=%v", events)
	}
	for i, value := range events {
		if value != i+1 {
			t.Fatalf("progress=%v", events)
		}
	}
}

func TestRunInputBudgetValidation(t *testing.T) {
	host := tempest.NewSession{Budget: tempest.Budget{MaxTurns: 4, MaxCostUSD: 1, MaxWallTime: time.Minute}}
	for _, raw := range []string{`{"prompt":"go","budget":{"maxTurns":0}}`, `{"prompt":"go","budget":{"maxCostUsd":2}}`, `{"prompt":"go","budget":{"maxWallSeconds":61}}`} {
		var in RunInput
		_ = json.Unmarshal([]byte(raw), &in)
		if _, err := in.settings(host); err == nil {
			t.Fatalf("relaxed host cap: %s", raw)
		}
	}
	var in RunInput
	_ = json.Unmarshal([]byte(`{"prompt":"go","budget":{"maxTurns":2,"maxCostUsd":0.5,"maxWallSeconds":1}}`), &in)
	got, err := in.settings(host)
	if err != nil || got.Budget.MaxTurns != 2 || got.Budget.MaxCostUSD != 0.5 || got.Budget.MaxWallTime != time.Second {
		t.Fatalf("tighten=%+v err=%v", got, err)
	}
}
