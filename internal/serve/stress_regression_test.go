package serve

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/tempest"
)

// rawPipeClient writes exact frames and reads replies in wire order.
type rawPipeClient struct {
	t      *testing.T
	conn   net.Conn
	reader *bufio.Reader
}

func rawConnect(t *testing.T, s *Server) *rawPipeClient {
	t.Helper()
	a, b := net.Pipe()
	go func() {
		if err := s.ServeStdio(a); err != nil && !errors.Is(err, tempest.ErrClosed) {
			t.Logf("serve stdio: %v", err)
		}
	}()
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Logf("close pipe: %v", err)
		}
	})
	c := &rawPipeClient{t: t, conn: b, reader: bufio.NewReader(b)}
	c.write(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)
	c.result(1)
	return c
}

func (c *rawPipeClient) write(frames ...string) {
	c.t.Helper()
	if _, err := c.conn.Write([]byte(strings.Join(frames, "\n") + "\n")); err != nil {
		c.t.Fatal(err)
	}
}

// result reads frames until the response with id, skipping notifications.
func (c *rawPipeClient) result(id int) json.RawMessage {
	c.t.Helper()
	if err := c.conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		c.t.Fatal(err)
	}
	for {
		line, err := c.reader.ReadBytes('\n')
		if err != nil {
			c.t.Fatalf("waiting for response %d: %v", id, err)
		}
		var frame struct {
			ID     *int            `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(line, &frame); err != nil {
			c.t.Fatal(err)
		}
		if frame.ID == nil || *frame.ID != id {
			continue
		}
		if frame.Error != nil {
			c.t.Fatalf("response %d error: %s", id, frame.Error)
		}
		return frame.Result
	}
}

func (c *rawPipeClient) newSession(cwd string) string {
	c.t.Helper()
	c.write(fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":%q,"mcpServers":[]}}`, cwd))
	var created struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(c.result(2), &created); err != nil {
		c.t.Fatal(err)
	}
	return created.SessionID
}

func promptFrame(id int, sessionID string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/prompt","params":{"sessionId":%q,"prompt":[{"type":"text","text":"go"}]}}`, id, sessionID)
}

func cancelFrame(sessionID string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":%q}}`, sessionID)
}

func stopReason(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var reply struct {
		StopReason string `json:"stopReason"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatal(err)
	}
	return reply.StopReason
}

// slowTool runs until cancelled or until its duration passes.
func slowTool(duration time.Duration, entered chan<- struct{}) tempest.Tool {
	return tempest.Tool{Tool: types.Tool{Name: "work", Description: "work", Parameters: json.RawMessage(`{"type":"object"}`)}, Execute: func(ctx context.Context, id string, p json.RawMessage, cb agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
		if entered != nil {
			select {
			case entered <- struct{}{}:
			default:
			}
		}
		select {
		case <-ctx.Done():
			return agent.AgentToolResult{}, ctx.Err()
		case <-time.After(duration):
			return agent.AgentToolResult{Content: []types.ContentBlock{types.NewText("ran to completion")}}, nil
		}
	}}
}

// ACP clients send session/cancel after session/prompt on the same connection;
// the server must not reorder them and drop the cancel.
func TestCancelSentRightAfterPromptOnSameConnectionCancelsIt(t *testing.T) {
	const trials = 10
	for trial := range trials {
		f := faux.New(faux.Options{})
		f.SetResponses(faux.Respond(faux.ToolCall("work", json.RawMessage(`{}`), "w")), faux.Respond(faux.Text("finished without cancel")))
		s := newServer(t, f, []tempest.Tool{slowTool(2*time.Second, nil)}, permission.ModeBypass, nil)
		c := rawConnect(t, s)
		id := c.newSession(t.TempDir())
		started := time.Now()
		c.write(promptFrame(3, id), cancelFrame(id))
		got := stopReason(t, c.result(3))
		if got != "cancelled" {
			t.Fatalf("trial %d: session/cancel written right after session/prompt was ignored: stopReason %q after %s", trial, got, time.Since(started))
		}
	}
}

// A cancel that reaches the server while the session is idle must stay a no-op,
// not cancel the prompt the client sends next. The reorder is a scheduling race,
// so the test repeats it; on 9575291 it hit 2 to 11 times per 300 trials.
func TestCancelSentWhileIdleDoesNotCancelNextPrompt(t *testing.T) {
	const trials = 1000
	f := faux.New(faux.Options{})
	steps := make([]faux.ResponseStep, trials)
	for i := range steps {
		steps[i] = faux.Respond(faux.Text("answered"))
	}
	f.SetResponses(steps...)
	s := newServer(t, f, nil, permission.ModeBypass, nil)
	c := rawConnect(t, s)
	id := c.newSession(t.TempDir())
	cancelled := 0
	for trial := range trials {
		c.write(cancelFrame(id), promptFrame(3+trial, id))
		if got := stopReason(t, c.result(3+trial)); got == "cancelled" {
			cancelled++
		}
	}
	if cancelled > 0 {
		t.Fatalf("%d of %d prompts were cancelled by a session/cancel sent before them while the session was idle", cancelled, trials)
	}
}

func agentText(updates []wireEvent) string {
	sort.SliceStable(updates, func(i, j int) bool { return eventSeq(updates[i]) < eventSeq(updates[j]) })
	var text strings.Builder
	seen := map[uint64]bool{}
	for _, e := range updates {
		if e.method != "session/update" || seen[eventSeq(e)] {
			continue
		}
		seen[eventSeq(e)] = true
		var p struct {
			Update json.RawMessage `json:"update"`
		}
		if err := json.Unmarshal(e.params, &p); err == nil {
			appendText(&text, p.Update)
		}
	}
	return text.String()
}

func journalAgentText(t *testing.T, s *Server, cwd, id string) string {
	t.Helper()
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
	return want.String()
}

// collectUntilFinished gathers notifications until tempest/run finished.
func collectUntilFinished(t *testing.T, c *wireClient) []wireEvent {
	t.Helper()
	var events []wireEvent
	deadline := time.After(20 * time.Second)
	for {
		select {
		case e := <-c.events:
			events = append(events, e)
			if e.method == "tempest/run" && strings.Contains(string(e.params), `"status":"finished"`) {
				// Notifications are dispatched concurrently; let stragglers land.
				time.Sleep(100 * time.Millisecond)
				for {
					select {
					case late := <-c.events:
						events = append(events, late)
					default:
						return events
					}
				}
			}
		case <-deadline:
			t.Fatalf("run never finished; %d events", len(events))
		}
	}
}

// session/load replays only completed history; the part of the assistant
// message already streamed when the client loads must not be lost.
func TestSessionLoadMidMessageKeepsStreamedPrefix(t *testing.T) {
	f := faux.New(faux.Options{TokensPerSecond: 200, MinTokenSize: 1, MaxTokenSize: 1})
	var words strings.Builder
	for i := range 60 {
		fmt.Fprintf(&words, "w%03d", i)
	}
	f.SetResponses(faux.Respond(faux.Text(words.String())))
	s := newServer(t, f, nil, permission.ModeBypass, nil)
	a := connect(t, s)
	cwd := t.TempDir()
	id := newSession(t, a, cwd)
	watchSession(t, a, id, false)
	go func() {
		if err := a.conn.Call(context.Background(), "session/prompt", map[string]any{"sessionId": id, "prompt": []any{map[string]string{"type": "text", "text": "go"}}}, nil); err != nil {
			t.Logf("prompt: %v", err)
		}
	}()
	for e := range a.events {
		if e.method == "session/update" && strings.Contains(string(e.params), "agent_message_chunk") {
			break
		}
	}
	time.Sleep(50 * time.Millisecond)
	b := connect(t, s)
	call(t, b, "session/load", map[string]any{"sessionId": id, "cwd": cwd, "mcpServers": []any{}}, nil)
	got := agentText(collectUntilFinished(t, b))
	if want := journalAgentText(t, s, cwd, id); got != want {
		t.Fatalf("session/load during streaming reconstructed %d bytes, journal has %d: %s", len(got), len(want), firstDiff(got, want))
	}
}

func firstDiff(got, want string) string {
	for i := range min(len(got), len(want)) {
		if got[i] != want[i] {
			return fmt.Sprintf("first difference at byte %d: got %q, want %q", i, got[i:min(len(got), i+16)], want[i:min(len(want), i+16)])
		}
	}
	return "one is a prefix of the other"
}

// A client that reads promptly must not be gapped by the server's own replay
// when it loads a long session while a run is streaming.
func TestSessionLoadDuringRunIsNotGappedByItsOwnReplay(t *testing.T) {
	fast := faux.New(faux.Options{})
	fast.SetResponses(faux.Respond(faux.Text(strings.Repeat("history words ", 12000))))
	slow := faux.New(faux.Options{TokensPerSecond: 8000, MinTokenSize: 1, MaxTokenSize: 1})
	slow.SetResponses(faux.Respond(faux.Text(strings.Repeat("live ", 16000))))
	calls := 0
	streamFn := func(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream {
		calls++
		if calls == 1 {
			return fast.StreamSimple(ctx, model, c, opts)
		}
		return slow.StreamSimple(ctx, model, c, opts)
	}
	opts := Options{SessionDir: t.TempDir(), DefaultNew: tempest.NewSession{PermissionMode: permission.ModeBypass}, Stack: &tempest.StackConfig{Model: fast.Model(), StreamFn: streamFn}}
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	a := connect(t, s)
	cwd := t.TempDir()
	id := newSession(t, a, cwd)
	call(t, a, "session/prompt", map[string]any{"sessionId": id, "prompt": []any{map[string]string{"type": "text", "text": "build history"}}}, nil)
	watchSession(t, a, id, false)
	go func() {
		if err := a.conn.Call(context.Background(), "session/prompt", map[string]any{"sessionId": id, "prompt": []any{map[string]string{"type": "text", "text": "stream"}}}, nil); err != nil {
			t.Logf("prompt: %v", err)
		}
	}()
	for e := range a.events {
		if e.method == "session/update" && strings.Contains(string(e.params), `"text":"live`) {
			break
		}
	}
	b := connect(t, s)
	call(t, b, "session/load", map[string]any{"sessionId": id, "cwd": cwd, "mcpServers": []any{}}, nil)
	for _, e := range collectUntilFinishedOrGap(t, b) {
		if e.method == "tempest/gap" {
			t.Fatalf("session/load subscriber was gapped while it was reading: %s", e.params)
		}
	}
}

func collectUntilFinishedOrGap(t *testing.T, c *wireClient) []wireEvent {
	t.Helper()
	var events []wireEvent
	deadline := time.After(30 * time.Second)
	for {
		select {
		case e := <-c.events:
			events = append(events, e)
			if e.method == "tempest/gap" || (e.method == "tempest/run" && strings.Contains(string(e.params), `"status":"finished"`)) {
				return events
			}
		case <-deadline:
			t.Fatalf("no finish or gap; %d events", len(events))
		}
	}
}

// Shutdown aborts runs; the client that sent session/prompt is still connected
// and must get its cancelled response before the transport closes.
func TestShutdownAnswersPendingPromptBeforeClosingConnection(t *testing.T) {
	const trials = 5
	for trial := range trials {
		f := faux.New(faux.Options{})
		f.SetResponses(faux.Respond(faux.ToolCall("work", json.RawMessage(`{}`), "w")), faux.Respond(faux.Text("unreachable")))
		entered := make(chan struct{}, 1)
		s := newServer(t, f, []tempest.Tool{slowTool(time.Minute, entered)}, permission.ModeBypass, nil)
		c := connect(t, s)
		id := newSession(t, c, t.TempDir())
		result := make(chan error, 1)
		var reply struct {
			StopReason string `json:"stopReason"`
		}
		go func() {
			result <- c.conn.Call(context.Background(), "session/prompt", map[string]any{"sessionId": id, "prompt": []any{map[string]string{"type": "text", "text": "go"}}}, &reply)
		}()
		<-entered
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := s.Close(ctx); err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel()
		if err := <-result; err != nil {
			t.Fatalf("trial %d: pending session/prompt got %v during shutdown, not a cancelled response", trial, err)
		}
		if reply.StopReason != "cancelled" {
			t.Fatalf("trial %d: stopReason %q", trial, reply.StopReason)
		}
	}
}

// A session/close that gives up waiting (its connection went away) has already
// closed the session; the registry must not keep handing that closed session out.
func TestFailedCloseDoesNotLeaveClosedSessionRegistered(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.ToolCall("stubborn", json.RawMessage(`{}`), "s")), faux.Respond(faux.Text("first done")), faux.Respond(faux.Text("second")))
	entered := make(chan struct{}, 1)
	stubborn := tempest.Tool{Tool: types.Tool{Name: "stubborn", Description: "ignores cancellation briefly", Parameters: json.RawMessage(`{"type":"object"}`)}, Execute: func(ctx context.Context, id string, p json.RawMessage, cb agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
		entered <- struct{}{}
		time.Sleep(300 * time.Millisecond)
		return agent.AgentToolResult{Content: []types.ContentBlock{types.NewText("done")}}, nil
	}}
	s := newServer(t, f, []tempest.Tool{stubborn}, permission.ModeBypass, nil)
	cwd := t.TempDir()
	session, err := s.New(context.Background(), cwd, &tempest.NewSession{})
	if err != nil {
		t.Fatal(err)
	}
	runDone := make(chan error, 1)
	go func() {
		_, err := session.Prompt(context.Background(), tempest.Prompt{Text: "go"})
		runDone <- err
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := s.CloseSession(ctx, session.ID()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CloseSession = %v, want deadline", err)
	}
	if err := <-runDone; err != nil {
		t.Fatalf("run: %v", err)
	}
	resumed, err := s.Resume(context.Background(), cwd, session.ID(), nil)
	if err != nil {
		t.Fatalf("resume after failed close: %v", err)
	}
	if _, err := resumed.Prompt(context.Background(), tempest.Prompt{Text: "again"}); errors.Is(err, tempest.ErrClosed) {
		t.Fatalf("registry still returns the closed session after a failed session/close: %v", err)
	}
}
