package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync/atomic"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/question"
	"go.harness.dev/harness/internal/rpc"
	"go.harness.dev/harness/tempest"
)

func TestAskACPPlanSessionEndToEnd(t *testing.T) {
	provider := faux.New(faux.Options{})
	provider.AppendResponses(faux.Respond(faux.ToolCall("ask", json.RawMessage(`{"questions":[{"id":"choice","question":"Choose","options":[{"label":"one","description":"First"}],"allow_text":true}]}`), "ask-call")))
	provider.AppendResponses(func(conversation types.Context, _ *types.StreamOptions, _ faux.State, _ types.Model) (types.AssistantMessage, error) {
		for _, message := range conversation.Messages {
			if result, ok := message.(types.ToolResultMessage); ok && result.ToolName == "ask" {
				encoded, err := json.Marshal(result)
				if err != nil {
					return types.AssistantMessage{}, err
				}
				if !result.IsError && bytes.Contains(encoded, []byte("user context")) {
					return types.AssistantMessage{Content: []types.ContentBlock{types.NewText("answered")}}, nil
				}
			}
		}
		return types.AssistantMessage{}, fmt.Errorf("ask answer missing")
	})
	server, err := New(Options{SessionDir: t.TempDir(), DefaultNew: tempest.NewSession{PermissionMode: permission.ModePlan}, Stack: &tempest.StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple, Plan: true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	left, right := net.Pipe()
	wire := &wireClient{conn: rpc.NewStream(right), events: make(chan wireEvent, 100)}
	t.Cleanup(func() { _ = wire.conn.Close() })
	go server.ServeStdio(left)
	var received atomic.Int32
	go wire.conn.Serve(func(_ context.Context, method string, raw json.RawMessage) (interface{}, *rpc.Error) {
		if method != "elicitation/create" {
			return nil, nil
		}
		var form question.Elicitation
		if err := json.Unmarshal(raw, &form); err != nil {
			return nil, rpc.NewError(-32602, "invalid", err.Error())
		}
		if form.Mode != "form" || form.SessionID == "" || form.ToolCallID != "ask-call" {
			return nil, rpc.NewError(-32602, "invalid", "wrong elicitation scope")
		}
		received.Add(1)
		return json.RawMessage(`{"action":"accept","content":{"selected":"one","text":"user context"}}`), nil
	})
	call(t, wire, "initialize", json.RawMessage(`{"protocolVersion":1,"clientCapabilities":{"elicitation":{"form":{}}}}`), nil)
	id := newSession(t, wire, t.TempDir())
	var reply struct {
		StopReason string `json:"stopReason"`
	}
	call(t, wire, "session/prompt", struct {
		SessionID string `json:"sessionId"`
		Prompt    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"prompt"`
	}{id, []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{{"text", "ask"}}}, &reply)
	if reply.StopReason != "end_turn" || received.Load() != 1 {
		t.Fatalf("reply=%+v asks=%d", reply, received.Load())
	}
}
