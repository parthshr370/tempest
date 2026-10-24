package acp

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"go.harness.dev/harness/internal/question"
	"go.harness.dev/harness/internal/rpc"
)

func TestAskACPFormNegotiationAndScope(t *testing.T) {
	for _, capability := range []string{`null`, `{}`, `{"form":null}`, `{"url":{}}`, `{"form":{}}`} {
		t.Run(capability, func(t *testing.T) {
			left, right := net.Pipe()
			server, client := rpc.NewStream(left), rpc.NewStream(right)
			t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
			go server.Serve(nil)
			received := make(chan question.Elicitation, 2)
			go client.Serve(func(ctx context.Context, method string, raw json.RawMessage) (interface{}, *rpc.Error) {
				if method != "elicitation/create" {
					return nil, rpc.NewError(-32601, "wrong_method", method)
				}
				var elicitation question.Elicitation
				if err := json.Unmarshal(raw, &elicitation); err != nil {
					return nil, rpc.NewError(-32602, "bad_request", err.Error())
				}
				received <- elicitation
				return json.RawMessage(`{"action":"accept","content":{"selected":["one","two"],"text":"context"}}`), nil
			})
			broker := NewBroker(time.Second, nil)
			defer broker.add("session", server)()
			broker.setForm(server, json.RawMessage(capability))
			request := question.Request{SessionID: "session", ToolCallID: "call", Questions: []question.Question{{ID: "q", Question: "Which?", Options: []question.Option{{Label: "one", Description: "First"}, {Label: "two"}}, Multi: true, AllowText: true}}}
			answers, err := broker.Question(t.Context(), request)
			if capability != `{"form":{}}` {
				if !errors.Is(err, question.Unavailable) {
					t.Fatalf("unsupported capability: %v", err)
				}
				return
			}
			if err != nil || len(answers) != 1 || len(answers[0].Selected) != 2 {
				t.Fatalf("answers=%+v err=%v", answers, err)
			}
			elicitation := <-received
			if elicitation.SessionID != "session" || elicitation.ToolCallID != "call" || elicitation.Mode != "form" {
				t.Fatalf("scope=%+v", elicitation)
			}
			request.SessionID = "another"
			if _, err := broker.Question(t.Context(), request); !errors.Is(err, question.Unavailable) {
				t.Fatalf("cross-session routing: %v", err)
			}
		})
	}
}
