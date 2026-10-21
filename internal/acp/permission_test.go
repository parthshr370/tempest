package acp

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/rpc"
	"go.harness.dev/harness/tempest"
)

type fakeTimer struct{ ch chan time.Time }

func (t *fakeTimer) C() <-chan time.Time { return t.ch }
func (t *fakeTimer) Stop() bool          { return true }

type fakeClock struct{ created chan *fakeTimer }

func (c *fakeClock) NewTimer(time.Duration) Timer {
	timer := &fakeTimer{make(chan time.Time, 1)}
	c.created <- timer
	return timer
}

type permissionRequest struct {
	params json.RawMessage
	answer chan string
}

func approver(t *testing.T) (*rpc.Conn, <-chan permissionRequest) {
	t.Helper()
	a, b := net.Pipe()
	server, client := rpc.NewStream(a), rpc.NewStream(b)
	requests := make(chan permissionRequest, 4)
	go server.Serve(nil)
	go client.Serve(func(ctx context.Context, method string, p json.RawMessage) (any, *rpc.Error) {
		answer := make(chan string, 1)
		requests <- permissionRequest{p, answer}
		select {
		case option := <-answer:
			return map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": option}}, nil
		case <-ctx.Done():
			return nil, nil
		}
	})
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	return server, requests
}
func receiveAsk(t *testing.T, requests <-chan permissionRequest) permissionRequest {
	t.Helper()
	select {
	case p := <-requests:
		return p
	case <-time.After(5 * time.Second):
		t.Fatal("permission request never arrived")
		return permissionRequest{}
	}
}

func TestPermissionReconnectAndFirstAnswer(t *testing.T) {
	clock := &fakeClock{created: make(chan *fakeTimer, 4)}
	broker := NewBroker(time.Minute, clock)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ask := tempest.Ask{SessionID: "s", ToolCallID: "tool", Tool: "bash", Title: "$ rm file", Kind: "execute", RawInput: json.RawMessage(`{"command":"rm file"}`), Options: tempest.PermissionChoices()}
	result := make(chan tempest.Choice, 1)
	go func() { choice, _ := broker.Approve(ctx, ask); result <- choice }()
	<-clock.created
	first, requests := approver(t)
	remove := broker.add("s", first)
	_ = receiveAsk(t, requests)
	remove()
	_ = first.Close()
	select {
	case choice := <-result:
		t.Fatalf("disconnect resolved ask: %q", choice)
	default:
	}
	second, next := approver(t)
	defer broker.add("s", second)()
	request := receiveAsk(t, next)
	var wire struct {
		SessionID string `json:"sessionId"`
		ToolCall  struct {
			ID string `json:"toolCallId"`
		} `json:"toolCall"`
		Options []tempest.Option `json:"options"`
	}
	if err := json.Unmarshal(request.params, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.SessionID != "s" || wire.ToolCall.ID != "tool" || len(wire.Options) != 4 {
		t.Fatalf("request = %s", request.params)
	}
	third, last := approver(t)
	defer broker.add("s", third)()
	thirdRequest := receiveAsk(t, last)
	var wg sync.WaitGroup
	wg.Go(func() { request.answer <- "allow_once" })
	wg.Go(func() { thirdRequest.answer <- "reject_always" })
	wg.Wait()
	select {
	case choice := <-result:
		if choice != "allow_once" && choice != "reject_always" {
			t.Fatal(choice)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("answer not delivered")
	}
}

func TestPermissionTimeoutAndCancellation(t *testing.T) {
	for _, timeout := range []bool{true, false} {
		t.Run(map[bool]string{true: "timeout", false: "cancel"}[timeout], func(t *testing.T) {
			clock := &fakeClock{created: make(chan *fakeTimer, 1)}
			broker := NewBroker(time.Minute, clock)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { _, err := broker.Approve(ctx, tempest.Ask{SessionID: "s", ToolCallID: "c"}); result <- err }()
			timer := <-clock.created
			want := context.Canceled
			if timeout {
				timer.ch <- time.Now()
				want = context.DeadlineExceeded
			} else {
				cancel()
			}
			if err := <-result; !errors.Is(err, want) {
				t.Fatalf("Approve = %v, want %v", err, want)
			}
		})
	}
}

func TestPromptContentConversion(t *testing.T) {
	text := "resource text"
	blocks := []contentBlock{{Type: "text", Text: "one"}, {Type: "text", Text: "two"}, {Type: "image", Data: "YWJj", MimeType: "image/png"}, {Type: "resource", Resource: &struct {
		URI  string  `json:"uri"`
		Text *string `json:"text"`
	}{URI: "file:///a", Text: &text}}}
	prompt, err := convertPrompt(blocks)
	if err != nil {
		t.Fatal(err)
	}
	if prompt.Text != "one\n\ntwo\n\n```file:///a\nresource text\n```" || len(prompt.Media) != 1 {
		t.Fatalf("prompt = %+v", prompt)
	}
	for _, kind := range []string{"audio", "resource_link", "invalid"} {
		if _, err := convertPrompt([]contentBlock{{Type: kind}}); err == nil || err.Code != -32602 {
			t.Fatalf("%s accepted", kind)
		}
	}
}
