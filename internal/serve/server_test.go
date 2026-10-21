package serve

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"go.harness.dev/harness/internal/acp"
	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/rpc"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/tempest"
)

type wireEvent struct {
	method string
	params json.RawMessage
}
type wireClient struct {
	conn   *rpc.Conn
	events chan wireEvent
}

func connect(t *testing.T, s *Server) *wireClient {
	t.Helper()
	a, b := net.Pipe()
	client := &wireClient{conn: rpc.NewStream(b), events: make(chan wireEvent, 20000)}
	go s.ServeStdio(a)
	go client.conn.Serve(func(ctx context.Context, m string, p json.RawMessage) (any, *rpc.Error) {
		client.events <- wireEvent{m, p}
		return nil, nil
	})
	t.Cleanup(func() { _ = client.conn.Close() })
	call(t, client, "initialize", map[string]int{"protocolVersion": 1}, nil)
	return client
}
func call(t *testing.T, c *wireClient, method string, p, result any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.conn.Call(ctx, method, p, result); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
}
func newSession(t *testing.T, c *wireClient, cwd string) string {
	t.Helper()
	var result struct {
		SessionID string `json:"sessionId"`
	}
	call(t, c, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}}, &result)
	return result.SessionID
}
func watchSession(t *testing.T, c *wireClient, id string, approver bool) tempest.Snapshot {
	t.Helper()
	var result struct {
		Snapshot tempest.Snapshot `json:"snapshot"`
	}
	call(t, c, "tempest/watch", map[string]any{"sessionId": id, "approver": approver}, &result)
	return result.Snapshot
}
func newServer(t *testing.T, provider *faux.Faux, tools []tempest.Tool, mode permission.Mode, clock acp.Clock) *Server {
	t.Helper()
	opts := Options{SessionDir: t.TempDir(), DefaultNew: tempest.NewSession{PermissionMode: mode}, Stack: &tempest.StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple, ConfiguredTools: tools}}
	if clock != nil {
		opts.Clock = clock
	}
	s, err := New(opts)
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

func TestStdioToolPromptAndCancel(t *testing.T) {
	for _, cancelRun := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelRun), func(t *testing.T) {
			f := faux.New(faux.Options{})
			f.SetResponses(faux.Respond(faux.Text("before"), faux.ToolCall("work", json.RawMessage(`{}`), "tool-1")), faux.Respond(faux.Text("after")))
			entered := make(chan struct{})
			release := make(chan struct{})
			tool := tempest.Tool{Tool: types.Tool{Name: "work", Description: "work", Parameters: json.RawMessage(`{"type":"object"}`)}, Execute: func(ctx context.Context, id string, p json.RawMessage, cb agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
				close(entered)
				select {
				case <-release:
					return agent.AgentToolResult{Content: []types.ContentBlock{types.NewText("ok")}}, nil
				case <-ctx.Done():
					return agent.AgentToolResult{}, ctx.Err()
				}
			}}
			s := newServer(t, f, []tempest.Tool{tool}, permission.ModeBypass, nil)
			c := connect(t, s)
			id := newSession(t, c, t.TempDir())
			watchSession(t, c, id, false)
			result := make(chan string, 1)
			go func() {
				var reply struct {
					StopReason string `json:"stopReason"`
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				err := c.conn.Call(ctx, "session/prompt", map[string]any{"sessionId": id, "prompt": []any{map[string]string{"type": "text", "text": "go"}}}, &reply)
				if err != nil {
					result <- err.Error()
				} else {
					result <- reply.StopReason
				}
			}()
			<-entered
			busyCtx, busyCancel := context.WithTimeout(context.Background(), 5*time.Second)
			busyErr := c.conn.Call(busyCtx, "session/prompt", map[string]any{"sessionId": id, "prompt": []any{map[string]string{"type": "text", "text": "second"}}}, nil)
			busyCancel()
			var busy *rpc.Error
			if !errors.As(busyErr, &busy) || busy.Code != -32004 {
				t.Fatalf("concurrent prompt = %v", busyErr)
			}
			if cancelRun {
				call(t, c, "session/cancel", map[string]string{"sessionId": id}, nil)
			} else {
				close(release)
			}
			want := "end_turn"
			if cancelRun {
				want = "cancelled"
			}
			if got := <-result; got != want {
				t.Fatalf("stopReason = %q, want %q", got, want)
			}
			snapshot := s.sessions[id].session.Snapshot()
			if snapshot.LastRun == nil || !snapshot.LastRun.Finished {
				t.Fatalf("unfinished snapshot: %+v", snapshot.LastRun)
			}
		})
	}
}

func TestReconnectTranscriptEqualsJournal(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("first text"), faux.ToolCall("work", json.RawMessage(`{}`), "c1")), faux.Respond(faux.Text("last text")))
	entered, release := make(chan struct{}), make(chan struct{})
	tool := tempest.Tool{Tool: types.Tool{Name: "work", Description: "work", Parameters: json.RawMessage(`{"type":"object"}`)}, Execute: func(ctx context.Context, id string, p json.RawMessage, cb agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
		close(entered)
		select {
		case <-release:
			return agent.AgentToolResult{Content: []types.ContentBlock{types.NewText("ok")}}, nil
		case <-ctx.Done():
			return agent.AgentToolResult{}, ctx.Err()
		}
	}}
	s := newServer(t, f, []tempest.Tool{tool}, permission.ModeBypass, nil)
	a := connect(t, s)
	cwd := t.TempDir()
	id := newSession(t, a, cwd)
	watchSession(t, a, id, false)
	promptDone := make(chan error, 1)
	go func() {
		promptDone <- a.conn.Call(context.Background(), "session/prompt", map[string]any{"sessionId": id, "prompt": []any{map[string]string{"type": "text", "text": "go"}}}, nil)
	}()
	<-entered
	_ = a.conn.Close()
	<-promptDone
	b := connect(t, s)
	call(t, b, "session/resume", map[string]string{"sessionId": id, "cwd": cwd}, nil)
	snapshot := watchSession(t, b, id, true)
	if snapshot.Run.Status != "running" {
		t.Fatalf("run after reconnect = %q", snapshot.Run.Status)
	}
	close(release)
	events := collectFinished(t, b, id, snapshot.Seq)
	var got strings.Builder
	for _, u := range snapshot.History {
		appendText(&got, u.Update)
	}
	if snapshot.Streaming != nil {
		got.WriteString(snapshot.Streaming.Text)
	}
	for _, e := range events {
		if e.method == "session/update" {
			var p struct {
				Update json.RawMessage `json:"update"`
			}
			_ = json.Unmarshal(e.params, &p)
			appendText(&got, p.Update)
		}
	}
	path, err := journal.Resolve(context.Background(), s.opts.SessionDir, cwd, id)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := journal.Load(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	for _, m := range loaded.Messages {
		if a, ok := m.(types.AssistantMessage); ok {
			for _, block := range a.Content {
				if block.Type == types.BlockText {
					want.WriteString(block.Text)
				}
			}
		}
	}
	if got.String() != want.String() {
		t.Fatalf("reconnected text %q != journal %q", got.String(), want.String())
	}
}
func appendText(out *strings.Builder, raw json.RawMessage) {
	var p struct {
		SessionUpdate string `json:"sessionUpdate"`
		Content       struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	_ = json.Unmarshal(raw, &p)
	if p.SessionUpdate == "agent_message_chunk" {
		out.WriteString(p.Content.Text)
	}
}
func eventSeq(e wireEvent) uint64 {
	var p struct {
		Seq    uint64 `json:"seq"`
		Update struct {
			Meta struct {
				Tempest struct {
					Seq uint64 `json:"seq"`
				} `json:"tempest"`
			} `json:"_meta"`
		} `json:"update"`
	}
	_ = json.Unmarshal(e.params, &p)
	if p.Seq != 0 {
		return p.Seq
	}
	return p.Update.Meta.Tempest.Seq
}
func collectFinished(t *testing.T, c *wireClient, id string, after uint64) []wireEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var events []wireEvent
	var final uint64
	for final == 0 || len(events) < int(final-after) {
		select {
		case e := <-c.events:
			seq := eventSeq(e)
			if seq <= after {
				continue
			}
			events = append(events, e)
			if e.method == "tempest/run" {
				var p struct {
					Status string `json:"status"`
				}
				_ = json.Unmarshal(e.params, &p)
				if p.Status == "finished" {
					final = seq
				}
			}
		case <-ctx.Done():
			t.Fatalf("missing updates: got %d, final %d, after %d", len(events), final, after)
		}
	}
	sort.Slice(events, func(i, j int) bool { return eventSeq(events[i]) < eventSeq(events[j]) })
	for i, e := range events {
		if eventSeq(e) != after+uint64(i)+1 {
			t.Fatalf("gap at %d: %s", i, e.params)
		}
	}
	return events
}

func TestFramingGateAndAuth(t *testing.T) {
	s, err := New(Options{SessionDir: t.TempDir(), Token: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	a, b := net.Pipe()
	defer b.Close()
	go s.ServeStdio(a)
	reader := bufio.NewReader(b)
	for _, test := range []struct {
		frame string
		code  int
	}{{strings.Repeat("x", 2<<20), -32600}, {"{", -32700}, {`{"jsonrpc":"2.0","id":1,"method":"session/new"}`, -32002}, {`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":1}}`, 0}, {`{"jsonrpc":"2.0","id":3,"method":"unknown"}`, -32601}} {
		_, _ = fmt.Fprintln(b, test.frame)
		line, e := reader.ReadBytes('\n')
		if e != nil {
			t.Fatal(e)
		}
		var response struct{ Error *rpc.Error }
		if e = json.Unmarshal(line, &response); e != nil {
			t.Fatal(e)
		}
		if test.code == 0 {
			if response.Error != nil {
				t.Fatal(string(line))
			}
		} else if response.Error == nil || response.Error.Code != test.code {
			t.Fatalf("response = %s", line)
		}
	}
	h := httptest.NewServer(s.Handler())
	defer h.Close()
	for _, token := range []string{"wrong", "secret"} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		ws, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.URL, "http")+"/acp?token="+token, nil)
		cancel()
		if token == "wrong" {
			if err == nil || response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("bad token: %v %+v", err, response)
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			_ = ws.CloseNow()
		}
	}
}

func TestRegistryLeaseReaperAndConcurrentResume(t *testing.T) {
	f := faux.New(faux.Options{})
	s := newServer(t, f, nil, permission.ModeBypass, nil)
	cwd := t.TempDir()
	session, err := s.New(context.Background(), cwd, &tempest.NewSession{})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 30 {
		wg.Go(func() {
			other, err := s.Resume(context.Background(), cwd, session.ID())
			if err != nil || other != session {
				t.Errorf("resume = %p, %v", other, err)
			}
		})
	}
	wg.Wait()
	_, release, err := s.Acquire(session.ID())
	if err != nil {
		t.Fatal(err)
	}
	s.reap(time.Now().Add(time.Hour))
	_, releaseSecond, err := s.Acquire(session.ID())
	if err != nil {
		t.Fatal("leased session reaped")
	}
	release()
	releaseSecond()
	s.reap(time.Now().Add(time.Hour))
	if _, _, err = s.Acquire(session.ID()); !errors.Is(err, journal.ErrSessionNotFound) {
		t.Fatalf("reaped Acquire = %v", err)
	}
	path, err := journal.Resolve(context.Background(), s.opts.SessionDir, cwd, session.ID())
	if err != nil {
		t.Fatal(err)
	}
	store, _, err := journal.OpenForResume(context.Background(), path, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	if filepath.Ext(path) != ".jsonl" {
		t.Fatal(path)
	}
}
