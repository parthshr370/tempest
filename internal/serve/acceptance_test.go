package serve

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/acp"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/rpc"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/tempest"
)

type testTimer struct{ ch chan time.Time }

func (t *testTimer) C() <-chan time.Time { return t.ch }
func (t *testTimer) Stop() bool          { return true }

type testClock struct{ created chan *testTimer }

func (c *testClock) NewTimer(time.Duration) acp.Timer {
	timer := &testTimer{make(chan time.Time, 1)}
	c.created <- timer
	return timer
}

func TestPermissionWireAndTimeoutJournal(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprint(timeout), func(t *testing.T) {
			f := faux.New(faux.Options{})
			f.SetResponses(faux.Respond(faux.ToolCall("bash", json.RawMessage(`{"command":"touch approved"}`), "permission-call")), faux.Respond(faux.Text("done")))
			clock := &testClock{created: make(chan *testTimer, 1)}
			s := newServer(t, f, nil, permission.ModeDefault, clock)
			a, b := net.Pipe()
			c := &wireClient{conn: rpc.NewStream(b), events: make(chan wireEvent, 1000)}
			requests := make(chan json.RawMessage, 1)
			go s.ServeStdio(a)
			go c.conn.Serve(func(ctx context.Context, method string, p json.RawMessage) (any, *rpc.Error) {
				if method == "session/request_permission" {
					requests <- p
					if timeout {
						<-ctx.Done()
						return nil, nil
					}
					return map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": "allow_once"}}, nil
				}
				c.events <- wireEvent{method, p}
				return nil, nil
			})
			defer c.conn.Close()
			call(t, c, "initialize", map[string]int{"protocolVersion": 1}, nil)
			cwd := t.TempDir()
			id := newSession(t, c, cwd)
			watchSession(t, c, id, false)
			done := make(chan error, 1)
			go func() {
				done <- c.conn.Call(context.Background(), "session/prompt", map[string]any{"sessionId": id, "prompt": []any{map[string]string{"type": "text", "text": "go"}}}, nil)
			}()
			timer := <-clock.created
			select {
			case request := <-requests:
				if !strings.Contains(string(request), "permission-call") {
					t.Fatalf("ask = %s", request)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("wire permission ask missing")
			}
			if timeout {
				timer.ch <- time.Now()
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("permission run hung")
			}
			path, err := journal.Resolve(context.Background(), s.opts.SessionDir, cwd, id)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			source := "prompt"
			if timeout {
				source = "prompt_timeout"
			}
			if !strings.Contains(string(data), `"source":"`+source+`"`) {
				t.Fatalf("missing %s permission record: %s", source, data)
			}
			_, fileErr := os.Stat(cwd + "/approved")
			if timeout {
				if !os.IsNotExist(fileErr) {
					t.Fatalf("timed-out tool ran: %v", fileErr)
				}
			} else {
				if fileErr != nil {
					t.Fatal(fileErr)
				}
				if strings.Index(string(data), `"type":"permission"`) > strings.Index(string(data), `"type":"tool_started"`) {
					t.Fatal("permission journaled after tool intent")
				}
			}
		})
	}
}

func TestFiftySessionsIsolateNonReadingWire(t *testing.T) {
	f := faux.New(faux.Options{MinTokenSize: 1, MaxTokenSize: 1})
	steps := make([]faux.ResponseStep, 50)
	for i := range steps {
		steps[i] = faux.Respond(faux.Text(strings.Repeat("x", 500)))
	}
	f.SetResponses(steps...)
	s := newServer(t, f, nil, permission.ModeBypass, nil)
	// The slow client reads its handshake and snapshot, then stops reading entirely.
	a, b := net.Pipe()
	defer b.Close()
	go s.ServeStdio(a)
	reader := bufio.NewReader(b)
	rawCall := func(id int, method string, params any) json.RawMessage {
		t.Helper()
		p, _ := json.Marshal(params)
		_, err := fmt.Fprintf(b, "{\"jsonrpc\":\"2.0\",\"id\":%d,\"method\":%q,\"params\":%s}\n", id, method, p)
		if err != nil {
			t.Fatal(err)
		}
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var response struct {
			Result json.RawMessage `json:"result"`
			Error  *rpc.Error      `json:"error"`
		}
		if err = json.Unmarshal(line, &response); err != nil || response.Error != nil {
			t.Fatalf("raw response %s, %v", line, err)
		}
		return response.Result
	}
	rawCall(1, "initialize", map[string]int{"protocolVersion": 1})
	var created struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(rawCall(2, "session/new", map[string]any{"cwd": t.TempDir()}), &created)
	rawCall(3, "tempest/watch", map[string]any{"sessionId": created.SessionID, "buffer": 1})
	clients := make([]*wireClient, 49)
	ids := make([]string, 49)
	for i := range clients {
		clients[i] = connect(t, s)
		ids[i] = newSession(t, clients[i], t.TempDir())
		call(t, clients[i], "tempest/watch", map[string]any{"sessionId": ids[i], "buffer": 4096}, nil)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	results := make(chan error, 50)
	var wg sync.WaitGroup
	slow, release, err := s.Acquire(created.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	wg.Go(func() { _, err := slow.Prompt(ctx, tempest.Prompt{Text: "slow"}); results <- err })
	for i, c := range clients {
		wg.Go(func() {
			var result struct {
				StopReason string `json:"stopReason"`
			}
			err := c.conn.Call(ctx, "session/prompt", map[string]any{"sessionId": ids[i], "prompt": []any{map[string]string{"type": "text", "text": "go"}}}, &result)
			if err == nil && result.StopReason != "end_turn" {
				err = fmt.Errorf("unexpected stop %s", result.StopReason)
			}
			results <- err
		})
	}
	for range 50 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("non-reading subscriber stalled other sessions")
		}
	}
	wg.Wait()
	// Draining resumes only after every run finished. Exactly one gap ends this watch.
	_ = b.SetReadDeadline(time.Now().Add(5 * time.Second))
	gaps := 0
	for gaps == 0 {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var event struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(line, &event)
		if event.Method == "tempest/gap" {
			gaps++
		}
	}
	_, _ = fmt.Fprintf(b, "{\"jsonrpc\":\"2.0\",\"id\":4,\"method\":\"tempest/get_state\",\"params\":{\"sessionId\":%q}}\n", created.SessionID)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var event struct {
			Method string `json:"method"`
			ID     int    `json:"id"`
		}
		_ = json.Unmarshal(line, &event)
		if event.Method == "tempest/gap" {
			gaps++
		}
		if event.ID == 4 {
			break
		}
	}
	if gaps != 1 {
		t.Fatalf("gap count = %d", gaps)
	}
}
