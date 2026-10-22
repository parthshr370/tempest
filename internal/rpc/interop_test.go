package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
)

func TestRequestIDAndNoResponse(t *testing.T) {
	local, remote := net.Pipe()
	conn := NewStream(local)
	defer conn.Close()
	defer remote.Close()
	received := make(chan string, 3)
	go func() {
		_ = conn.ServeOrdered(func(ctx context.Context, method string, params json.RawMessage) (any, *Error) {
			received <- string(RequestID(ctx))
			if method == "cancelled" {
				return Deferred(func() (any, *Error) { return NoResponse{}, nil }), nil
			}
			return struct{}{}, nil
		})
	}()
	go func() {
		_, _ = remote.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"cancelled\"}\n{\"jsonrpc\":\"2.0\",\"id\":\"1\",\"method\":\"reply\"}\n"))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, want := range []string{"1", `"1"`} {
		select {
		case got := <-received:
			if got != want {
				t.Fatalf("ID %s, want %s", got, want)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	_ = remote.SetReadDeadline(time.Now().Add(time.Second))
	line, err := bufio.NewReader(remote).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal([]byte(line), &response) != nil || string(response.ID) != `"1"` {
		t.Fatalf("suppressed wrong reply: %s", line)
	}
}
func TestCallWithIDRejectsPendingDuplicate(t *testing.T) {
	local, remote := net.Pipe()
	conn := NewStream(local)
	defer conn.Close()
	defer remote.Close()
	go func() {
		_ = conn.ServeOrdered(func(context.Context, string, json.RawMessage) (any, *Error) { return nil, nil })
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settled := make(chan error, 1)
	go func() { _, err := conn.CallWithID(ctx, "same", "ping", struct{}{}); settled <- err }()
	if _, err := bufio.NewReader(remote).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.CallWithID(ctx, "same", "ping", struct{}{}); err == nil || !strings.Contains(err.Error(), "already pending") {
		t.Fatalf("duplicate: %v", err)
	}
	cancel()
	select {
	case <-settled:
	case <-time.After(time.Second):
		t.Fatal("pending caller stuck")
	}
}
func TestConfigurableFrameLimitAndStrictPolicy(t *testing.T) {
	local, remote := net.Pipe()
	conn := NewStreamWithLimit(local, 128)
	defer conn.Close()
	defer remote.Close()
	rejected := make(chan error, 1)
	conn.SetProtocolErrorHandler(func(err error) { rejected <- err })
	go func() {
		_ = conn.ServeOrdered(func(context.Context, string, json.RawMessage) (any, *Error) { return nil, nil })
	}()
	go func() { _, _ = remote.Write([]byte(strings.Repeat("x", 129) + "\n")) }()
	select {
	case <-rejected:
	case <-time.After(time.Second):
		t.Fatal("strict oversize policy wasn't invoked")
	}
}
