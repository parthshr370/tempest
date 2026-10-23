package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

func frame(body string) string {
	return fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
}

// serveCL starts a strict Content-Length connection and records notifications.
func serveCL(t *testing.T, maxBody int) (conn *Conn, remote net.Conn, methods chan string, faults chan error, served chan error) {
	t.Helper()
	local, remote := net.Pipe()
	conn = NewContentLengthStream(local, maxBody)
	methods = make(chan string, 16)
	faults = make(chan error, 4)
	served = make(chan error, 1)
	conn.SetProtocolErrorHandler(func(err error) { faults <- err })
	go func() {
		served <- conn.ServeOrdered(func(ctx context.Context, method string, params json.RawMessage) (any, *Error) {
			methods <- method + " " + string(params)
			return nil, nil
		})
	}()
	t.Cleanup(func() { conn.Close(); remote.Close() })
	return conn, remote, methods, faults, served
}

func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
	var zero T
	return zero
}

func TestContentLengthFrames(t *testing.T) {
	first := `{"jsonrpc":"2.0","method":"a","params":{"text":"line1\nline2 😀"}}`
	multiline := "{\n  \"jsonrpc\": \"2.0\",\n  \"method\": \"b\",\n  \"params\": [\"é\"]\n}"
	stream := frame(first) + "Content-Type: application/vscode-jsonrpc; charset=UTF8\r\nX-Unknown: ignored\r\n" + frame(multiline)
	t.Run("fragment at every byte", func(t *testing.T) {
		for split := 1; split < len(stream); split++ {
			_, remote, methods, faults, _ := serveCL(t, 0)
			go func() {
				_, _ = io.WriteString(remote, stream[:split])
				_, _ = io.WriteString(remote, stream[split:])
			}()
			if got := recv(t, methods); got != `a {"text":"line1\nline2 😀"}` {
				t.Fatalf("split %d: first = %q", split, got)
			}
			if got := recv(t, methods); !strings.HasPrefix(got, "b [") || !strings.Contains(got, "é") {
				t.Fatalf("split %d: second = %q", split, got)
			}
			select {
			case err := <-faults:
				t.Fatalf("split %d: fault %v", split, err)
			default:
			}
		}
	})
	t.Run("two frames in one write and emoji byte lengths", func(t *testing.T) {
		_, remote, methods, _, _ := serveCL(t, 0)
		go func() { _, _ = io.WriteString(remote, frame(first)+frame(first)) }()
		for range 2 {
			if got := recv(t, methods); !strings.Contains(got, "😀") {
				t.Fatalf("got %q", got)
			}
		}
	})
	t.Run("outbound frames and exact correlation", func(t *testing.T) {
		conn, remote, _, _, _ := serveCL(t, 0)
		reader := bufio.NewReader(remote)
		results := make(chan string, 2)
		for _, id := range []string{"one", "two"} {
			go func() {
				raw, err := conn.CallWithID(context.Background(), id, "m", map[string]string{"q": id + " 😀"})
				if err != nil {
					results <- id + " error " + err.Error()
					return
				}
				results <- id + " " + string(raw)
			}()
		}
		var ids []string
		for range 2 {
			header, err := reader.ReadString('\n')
			if err != nil || !strings.HasPrefix(header, "Content-Length: ") || !strings.HasSuffix(header, "\r\n") {
				t.Fatalf("header %q %v", header, err)
			}
			n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(header, "Content-Length: "), "\r\n"))
			if blank, _ := reader.ReadString('\n'); blank != "\r\n" {
				t.Fatalf("blank %q", blank)
			}
			body := make([]byte, n)
			if _, err = io.ReadFull(reader, body); err != nil || !json.Valid(body) {
				t.Fatalf("body %q %v", body, err)
			}
			var m struct{ ID string }
			_ = json.Unmarshal(body, &m)
			ids = append(ids, m.ID)
		}
		// Reply in reverse order; each caller must get its own result.
		for i := len(ids) - 1; i >= 0; i-- {
			_, _ = io.WriteString(remote, frame(fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"result":"for-%s"}`, ids[i], ids[i])))
		}
		got := map[string]bool{recv(t, results): true, recv(t, results): true}
		if !got[`one "for-one"`] || !got[`two "for-two"`] {
			t.Fatalf("correlation: %v", got)
		}
	})
	t.Run("accepted content types", func(t *testing.T) {
		for _, header := range []string{"", "Content-Type: application/vscode-jsonrpc\r\n", "content-type: Application/VSCode-JSONRPC; charset=utf-8\r\n", "Content-Type: application/vscode-jsonrpc; charset=\"UTF-8\"\r\n"} {
			_, remote, methods, _, _ := serveCL(t, 0)
			go func() { _, _ = io.WriteString(remote, header+frame(`{"jsonrpc":"2.0","method":"ok"}`)) }()
			if got := recv(t, methods); got != "ok " {
				t.Fatalf("%q: %q", header, got)
			}
		}
	})
}

func TestContentLengthStrictFailures(t *testing.T) {
	valid := `{"jsonrpc":"2.0","method":"x"}`
	cases := []struct {
		name, input, code string
		closeAfter        bool
	}{
		{"missing length", "Content-Type: application/vscode-jsonrpc\r\n\r\n" + valid, "lsp_protocol: missing Content-Length", false},
		{"duplicate length", "Content-Length: 30\r\nContent-Length: 30\r\n\r\n" + valid, "lsp_protocol: duplicate Content-Length", false},
		{"negative length", "Content-Length: -1\r\n\r\n", "lsp_protocol: invalid Content-Length", false},
		{"plus sign", "Content-Length: +30\r\n\r\n" + valid, "lsp_protocol: invalid Content-Length", false},
		{"zero length", "Content-Length: 0\r\n\r\n", "lsp_protocol: invalid Content-Length", false},
		{"overflow length", "Content-Length: 99999999999999999999\r\n\r\n", "lsp_protocol: invalid Content-Length", false},
		{"over 8 MiB body", fmt.Sprintf("Content-Length: %d\r\n\r\n", MaxContentLength+1), "lsp_output_limit: LSP frame exceeds response cap", false},
		{"over configured cap", "Content-Length: 65\r\n\r\n", "lsp_output_limit:", false},
		{"header over 8 KiB", "X-Pad: " + strings.Repeat("a", 8<<10) + "\r\n", "lsp_protocol: header section exceeds 8 KiB", false},
		{"too many headers", strings.Repeat("X-A: b\r\n", 17) + "\r\n", "lsp_protocol: more than 16 headers", false},
		{"bad charset", "Content-Type: application/vscode-jsonrpc; charset=latin1\r\n" + frame(valid), "lsp_protocol: unsupported Content-Type", false},
		{"other media type", "Content-Type: text/plain\r\n" + frame(valid), "lsp_protocol: unsupported Content-Type", false},
		{"non-ASCII header", "X-Name: é\r\n" + frame(valid), "lsp_protocol: header contains non-ASCII", false},
		{"bare LF", "Content-Length: 30\n\n" + valid, "lsp_protocol: header line must end with CRLF", false},
		{"invalid JSON", frame(`{"jsonrpc":`), "lsp_protocol: invalid JSON or UTF-8", false},
		{"truncated body", "Content-Length: 30\r\n\r\n{\"js", "lsp_protocol: truncated body", true},
		{"truncated header", "Content-Len", "lsp_protocol: truncated header", true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			maxBody := 0
			if test.name == "over configured cap" {
				maxBody = 64
			}
			conn, remote, methods, faults, served := serveCL(t, maxBody)
			pending := make(chan error, 1)
			go func() {
				_, err := conn.CallWithID(context.Background(), "p", "wait", nil)
				pending <- err
			}()
			requested := make(chan struct{})
			go func() {
				reader := bufio.NewReader(remote)
				_, _ = reader.ReadString('}')
				close(requested)
				_, _ = io.Copy(io.Discard, reader)
			}()
			go func() {
				// Let the call's frame leave the writer first, so closing the
				// pipe below is observed as a truncated read, not a write failure.
				<-requested
				_, _ = io.WriteString(remote, test.input)
				if test.closeAfter {
					remote.Close()
				}
			}()
			fault := recv(t, faults)
			if !strings.HasPrefix(fault.Error(), test.code) {
				t.Fatalf("fault %q, want prefix %q", fault, test.code)
			}
			recv(t, served)
			if err := recv(t, pending); !errors.Is(err, ErrClosed) {
				t.Fatalf("pending call settled with %v", err)
			}
			select {
			case m := <-methods:
				t.Fatalf("dispatched %q after corrupt framing", m)
			default:
			}
		})
	}
}

func TestContentLengthOversizeWithoutStrictHandlerStops(t *testing.T) {
	local, remote := net.Pipe()
	conn := NewContentLengthStream(local, 32)
	defer conn.Close()
	defer remote.Close()
	served := make(chan error, 1)
	go func() {
		served <- conn.ServeOrdered(func(context.Context, string, json.RawMessage) (any, *Error) { return nil, nil })
	}()
	replies := make(chan string, 1)
	go func() {
		reader := bufio.NewReader(remote)
		_, _ = reader.ReadString('\n')
		_, _ = reader.ReadString('\n')
		line, _ := reader.ReadString('}')
		replies <- line
		_, _ = io.Copy(io.Discard, reader)
	}()
	// A trailing valid frame must not be parsed: the stream can't resynchronize.
	go func() {
		_, _ = io.WriteString(remote, "Content-Length: 33\r\n\r\n"+strings.Repeat(" ", 33)+frame(`{"jsonrpc":"2.0","method":"x"}`))
	}()
	if got := recv(t, replies); !strings.Contains(got, "frame_too_large") {
		t.Fatalf("reply %q", got)
	}
	if err := recv(t, served); err == nil {
		t.Fatal("serve continued after oversized frame")
	}
}

func TestStrictLineFaultTextUnchanged(t *testing.T) {
	local, remote := net.Pipe()
	conn := NewStream(local)
	defer conn.Close()
	defer remote.Close()
	faults := make(chan error, 1)
	conn.SetProtocolErrorHandler(func(err error) { faults <- err })
	go func() {
		_ = conn.ServeOrdered(func(context.Context, string, json.RawMessage) (any, *Error) { return nil, nil })
	}()
	go func() { _, _ = io.WriteString(remote, "{\"bad\":\n") }()
	if got := recv(t, faults).Error(); got != "mcp_protocol: invalid JSON or UTF-8" {
		t.Fatalf("MCP strict text changed: %q", got)
	}
}
