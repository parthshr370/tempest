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
			broker.setElicitation(server, json.RawMessage(capability))
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

func TestElicitForwardsOnlyDeclaredModes(t *testing.T) {
	urlRequest := question.Elicit{SessionID: "session", Server: "docs", Mode: "url", Message: "sign in", URL: "https://auth.example/authorize?x=1", ElicitationID: "e1"}
	formRequest := question.Elicit{SessionID: "session", Server: "docs", Mode: "form", Message: "pick", Schema: json.RawMessage(`{"type":"object","properties":{"color":{"type":"string","enum":["red","blue"]}},"required":["color"]}`)}
	for _, test := range []struct {
		capability string
		request    question.Elicit
		reply      string
		forwarded  bool
		action     string
	}{
		{`{"form":{}}`, urlRequest, `{"action":"accept"}`, false, ""},
		{`{"url":{}}`, urlRequest, `{"action":"accept"}`, true, "accept"},
		{`{"url":{}}`, formRequest, `{"action":"accept","content":{"color":"red"}}`, false, ""},
		{`{"form":{},"url":{}}`, formRequest, `{"action":"accept","content":{"color":"red"}}`, true, "accept"},
		{`{"form":{}}`, formRequest, `{"action":"decline"}`, true, "decline"},
	} {
		left, right := net.Pipe()
		server, client := rpc.NewStream(left), rpc.NewStream(right)
		received := make(chan question.ForwardParams, 1)
		go server.Serve(nil)
		go client.Serve(func(ctx context.Context, method string, raw json.RawMessage) (any, *rpc.Error) {
			var params question.ForwardParams
			if method != "elicitation/create" || json.Unmarshal(raw, &params) != nil {
				return nil, rpc.NewError(-32601, "wrong_method", method)
			}
			received <- params
			return json.RawMessage(test.reply), nil
		})
		broker := NewBroker(time.Second, nil)
		remove := broker.add("session", server)
		broker.setElicitation(server, json.RawMessage(test.capability))
		result, err := broker.Elicit(t.Context(), test.request)
		remove()
		_ = server.Close()
		_ = client.Close()
		if !test.forwarded {
			if !errors.Is(err, question.Unavailable) {
				t.Fatalf("%s %s: %v", test.capability, test.request.Mode, err)
			}
			continue
		}
		if err != nil || result.Action != test.action {
			t.Fatalf("%s %s: %#v %v", test.capability, test.request.Mode, result, err)
		}
		params := <-received
		if params.SessionID != "session" || params.Mode != test.request.Mode || params.Message != "MCP server docs: "+test.request.Message {
			t.Fatalf("params = %#v", params)
		}
		if test.request.Mode == "url" && (params.URL != test.request.URL || params.ElicitationID != "e1" || params.RequestedSchema != nil) {
			t.Fatalf("url params = %#v", params)
		}
	}
}
