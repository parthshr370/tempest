package serve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/rpc"
	"go.harness.dev/harness/tempest"
)

func TestLoadReplayPagingAndErrors(t *testing.T) {
	f := faux.New(faux.Options{MinTokenSize: 1, MaxTokenSize: 1})
	f.SetResponses(faux.Respond(faux.Text(strings.Repeat("history ", 300))))
	s := newServer(t, f, nil, permission.ModeBypass, nil)
	a := connect(t, s)
	cwd := t.TempDir()
	id := newSession(t, a, cwd)
	var prompt struct {
		StopReason string `json:"stopReason"`
		Meta       struct {
			Tempest struct {
				RunID     string `json:"runId"`
				ElapsedMS int64  `json:"elapsedMs"`
			} `json:"tempest"`
		} `json:"_meta"`
	}
	call(t, a, "session/prompt", map[string]any{"sessionId": id, "prompt": []any{map[string]string{"type": "text", "text": "go"}}}, &prompt)
	if prompt.StopReason != "end_turn" || prompt.Meta.Tempest.RunID == "" {
		t.Fatalf("prompt response = %+v", prompt)
	}
	snapshot := s.sessions[id].session.Snapshot()
	if snapshot.HistoryLen <= 200 {
		t.Fatal("test must exceed the snapshot tail")
	}
	b := connect(t, s)
	call(t, b, "session/load", map[string]any{"sessionId": id, "cwd": cwd}, nil)
	var text strings.Builder
	deadline := time.After(5 * time.Second)
	for range snapshot.HistoryLen {
		select {
		case e := <-b.events:
			if e.method == "session/update" {
				var p struct {
					Update json.RawMessage `json:"update"`
				}
				_ = json.Unmarshal(e.params, &p)
				appendText(&text, p.Update)
			}
		case <-deadline:
			t.Fatal("load failed to replay complete paged history")
		}
	}
	if text.Len() != len(strings.Repeat("history ", 300)) {
		t.Fatalf("replay length = %d", text.Len())
	}
	var history struct {
		Updates []tempest.Update `json:"updates"`
		Epoch   int              `json:"epoch"`
		Total   int              `json:"total"`
	}
	call(t, b, "tempest/history", map[string]any{"sessionId": id, "epoch": snapshot.HistoryEpoch, "offset": 0, "limit": 3}, &history)
	if len(history.Updates) != 3 || history.Total != snapshot.HistoryLen {
		t.Fatalf("history = %+v", history)
	}
	for _, test := range []struct {
		method string
		params any
		code   int
	}{
		{"tempest/history", map[string]any{"sessionId": id, "epoch": snapshot.HistoryEpoch + 1, "limit": 1}, -32010},
		{"tempest/set_budget", map[string]any{"sessionId": id, "maxTurns": -1}, -32005},
		{"session/new", map[string]any{"cwd": cwd, "mcpServers": []string{"unsupported"}}, -32602},
		{"session/prompt", map[string]any{"sessionId": id, "prompt": []any{map[string]string{"type": "audio"}}}, -32602},
		{"tempest/get_state", map[string]any{"sessionId": "missing"}, -32001},
		{"tempest/permission", map[string]any{"sessionId": id, "toolCallId": "missing", "optionId": "allow_once"}, -32006},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := b.conn.Call(ctx, test.method, test.params, nil)
		cancel()
		var wire *rpc.Error
		if !errors.As(err, &wire) || wire.Code != test.code {
			t.Fatalf("%s = %v, want %d", test.method, err, test.code)
		}
	}
	watchSession(t, b, id, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := b.conn.Call(ctx, "tempest/permission", map[string]any{"sessionId": id, "toolCallId": "missing", "optionId": "allow_once"}, nil)
	var wire *rpc.Error
	if !errors.As(err, &wire) || wire.Code != -32011 {
		t.Fatalf("permission = %v", err)
	}
	call(t, b, "tempest/set_budget", map[string]any{"sessionId": id, "maxTurns": 5, "maxWallSeconds": 2}, nil)
	call(t, b, "tempest/set_permission_mode", map[string]any{"sessionId": id, "mode": "plan"}, nil)
	call(t, b, "tempest/unwatch", map[string]any{"sessionId": id}, nil)
	var list struct {
		Sessions []struct {
			SessionID string `json:"sessionId"`
		} `json:"sessions"`
	}
	call(t, b, "session/list", map[string]any{"cwd": cwd}, &list)
	if len(list.Sessions) != 1 || list.Sessions[0].SessionID != id {
		t.Fatalf("list = %+v", list)
	}
	call(t, b, "session/close", map[string]any{"sessionId": id}, nil)
	call(t, b, "session/resume", map[string]any{"sessionId": id, "cwd": cwd}, nil)
}

func TestWebSocketOversizeSurvives(t *testing.T) {
	s, err := New(Options{SessionDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	h := httptest.NewServer(s.Handler())
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.URL, "http")+"/acp", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	for _, test := range []struct {
		frame string
		code  int
	}{{strings.Repeat("x", 2<<20), -32600}, {`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`, 0}} {
		if err = ws.Write(ctx, websocket.MessageText, []byte(test.frame)); err != nil {
			t.Fatal(err)
		}
		_, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var response struct{ Error *rpc.Error }
		if err = json.Unmarshal(data, &response); err != nil {
			t.Fatal(err)
		}
		if test.code == 0 {
			if response.Error != nil {
				t.Fatal(string(data))
			}
		} else if response.Error == nil || response.Error.Code != test.code {
			t.Fatalf("response = %s", data)
		}
	}
}

func TestRunErrorAndBudgetStopReasons(t *testing.T) {
	for _, budgetStop := range []bool{false, true} {
		t.Run(map[bool]string{false: "run_error", true: "max_turns"}[budgetStop], func(t *testing.T) {
			f := faux.New(faux.Options{})
			if budgetStop {
				f.SetResponses(faux.Respond(faux.ToolCall("read", json.RawMessage(`{"path":"missing"}`), "read-call")))
			}
			s := newServer(t, f, nil, permission.ModeBypass, nil)
			c := connect(t, s)
			var created struct {
				SessionID string `json:"sessionId"`
			}
			call(t, c, "session/new", map[string]any{"cwd": t.TempDir(), "_meta": map[string]any{"tempest": map[string]any{"budget": map[string]any{"maxTurns": 1}}}}, &created)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var reply struct {
				StopReason string `json:"stopReason"`
				Meta       struct {
					Tempest struct {
						StopReason string `json:"stopReason"`
					} `json:"tempest"`
				} `json:"_meta"`
			}
			err := c.conn.Call(ctx, "session/prompt", map[string]any{"sessionId": created.SessionID, "prompt": []any{map[string]string{"type": "text", "text": "go"}}}, &reply)
			if budgetStop {
				if err != nil || reply.StopReason != "max_turn_requests" || reply.Meta.Tempest.StopReason != "max_turns" {
					t.Fatalf("budget result = %+v, %v", reply, err)
				}
			} else {
				var wire *rpc.Error
				if !errors.As(err, &wire) || wire.Code != -32009 {
					t.Fatalf("run error = %v", err)
				}
			}
		})
	}
}
