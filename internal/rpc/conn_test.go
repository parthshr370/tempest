package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFramingRecovers(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	conn := NewStream(server)
	defer conn.Close()
	go conn.Serve(nil)
	reader := bufio.NewReader(client)
	cases := []struct {
		frame string
		code  int
		name  string
	}{
		{strings.Repeat("x", 2<<20), -32600, "frame_too_large"},
		{`{"bad":`, -32700, "parse_error"},
		{"{\"jsonrpc\":\"2.0\",\"id\":7,\"method\":\"\xff\"}", -32700, "parse_error"},
		{`{"jsonrpc":"2.0","id":7,"method":"missing"}`, -32601, "method_not_found"},
		{`[]`, -32600, "invalid_request"},
	}
	for _, test := range cases {
		if _, err := fmt.Fprintln(client, test.frame); err != nil {
			t.Fatal(err)
		}
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var reply struct {
			ID    json.RawMessage
			Error Error
		}
		if err = json.Unmarshal(line, &reply); err != nil {
			t.Fatal(err)
		}
		data := reply.Error.Data.(map[string]any)
		if reply.Error.Code != test.code || data["code"] != test.name {
			t.Fatalf("reply = %s", line)
		}
		if test.code == -32700 && string(reply.ID) != "null" {
			t.Fatalf("parse error must discard the request ID: %s", line)
		}
	}
}

func TestBidirectionalCallsAndPendingDisconnect(t *testing.T) {
	a, b := net.Pipe()
	left, right := NewStream(a), NewStream(b)
	defer left.Close()
	defer right.Close()
	entered := make(chan struct{})
	go left.Serve(func(ctx context.Context, method string, p json.RawMessage) (any, *Error) {
		return map[string]string{"reply": "client"}, nil
	})
	go right.Serve(func(ctx context.Context, method string, p json.RawMessage) (any, *Error) {
		if method == "wait" {
			close(entered)
			<-ctx.Done()
			return nil, nil
		}
		var response map[string]string
		if err := right.Call(ctx, "callback", nil, &response); err != nil {
			return nil, NewError(-32603, "internal_error", err.Error())
		}
		return response, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for range 40 {
		wg.Go(func() {
			var result map[string]string
			if err := left.Call(ctx, "echo", map[string]int{"a": 1}, &result); err != nil {
				t.Error(err)
			} else if result["reply"] != "client" {
				t.Error(result)
			}
		})
	}
	wg.Wait()
	result := make(chan error, 1)
	go func() { result <- left.Call(ctx, "wait", nil, nil) }()
	<-entered
	_ = right.Close()
	if err := <-result; !errors.Is(err, ErrClosed) {
		t.Fatalf("pending call = %v", err)
	}
}

func TestOutboundFrameReplacement(t *testing.T) {
	a, b := net.Pipe()
	server := NewStream(a)
	defer server.Close()
	defer b.Close()
	go server.Serve(func(ctx context.Context, method string, p json.RawMessage) (any, *Error) {
		return strings.Repeat("x", 2<<20), nil
	})
	reader := bufio.NewReader(b)
	if err := server.Notify(context.Background(), "large", strings.Repeat("x", 2<<20)); err != nil {
		t.Fatal(err)
	}
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var notice message
	if err = json.Unmarshal(line, &notice); err != nil {
		t.Fatal(err)
	}
	if notice.Method != "tempest/frame_dropped" {
		t.Fatalf("notification = %s", line)
	}
	if _, err = fmt.Fprintln(b, `{"jsonrpc":"2.0","id":1,"method":"large"}`); err != nil {
		t.Fatal(err)
	}
	line, err = reader.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var response message
	if err = json.Unmarshal(line, &response); err != nil {
		t.Fatal(err)
	}
	if response.Error == nil || response.Error.Code != -32000 {
		t.Fatalf("response = %s", line)
	}
}

func TestCloseDrainsQueuedFrames(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	conn := NewStream(a)
	const frames = 20
	for range frames {
		if err := conn.Notify(context.Background(), "queued", nil); err != nil {
			t.Fatal(err)
		}
	}
	received := make(chan int, 1)
	go func() {
		scanner := bufio.NewScanner(b)
		count := 0
		for scanner.Scan() {
			count++
		}
		if err := scanner.Err(); err != nil {
			t.Error(err)
		}
		received <- count
	}()
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if count := <-received; count != frames {
		t.Fatalf("received %d of %d queued frames", count, frames)
	}
}

func TestCloseBoundsBlockedWriter(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	conn := NewStream(a)
	if err := conn.Notify(context.Background(), "blocked", nil); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- conn.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close hung on a non-reading peer")
	}
	select {
	case <-conn.writerDone:
	case <-time.After(time.Second):
		t.Fatal("writer survived transport close")
	}
}
func TestLateRepliesAfterManyCancellationsKeepConnectionAlive(t *testing.T) {
	local, peer := net.Pipe()
	defer peer.Close()
	conn := NewStream(local)
	conn.SetProtocolErrorHandler(func(error) { go conn.Close() })
	defer conn.Close()
	go conn.Serve(func(context.Context, string, json.RawMessage) (any, *Error) { return "ok", nil })
	reader := bufio.NewReader(peer)
	const cancellations = 130
	ids := make([]string, cancellations)
	for i := range cancellations {
		ctx, cancel := context.WithCancel(context.Background())
		finished := make(chan error, 1)
		id := fmt.Sprintf("late-%d", i)
		go func() {
			_, err := conn.CallWithID(ctx, id, "work", nil)
			finished <- err
		}()
		line, err := reader.ReadString('\n')
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		var request struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(line), &request); err != nil {
			cancel()
			t.Fatal(err)
		}
		ids[i] = request.ID
		cancel()
		if err := <-finished; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled call %d = %v", i, err)
		}
	}
	for _, id := range ids {
		if _, err := fmt.Fprintf(peer, "{\"jsonrpc\":\"2.0\",\"id\":%q,\"result\":null}\n", id); err != nil {
			t.Fatalf("late reply %q: %v", id, err)
		}
	}
	if _, err := fmt.Fprintln(peer, `{"jsonrpc":"2.0","id":"probe","method":"ping"}`); err != nil {
		t.Fatal(err)
	}
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("connection closed after late replies: %v", err)
	}
	var response struct {
		ID     string `json:"id"`
		Result string `json:"result"`
	}
	if err := json.Unmarshal([]byte(line), &response); err != nil || response.ID != "probe" || response.Result != "ok" {
		t.Fatalf("probe reply = %s, err=%v", line, err)
	}
}
