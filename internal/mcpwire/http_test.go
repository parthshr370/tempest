package mcpwire

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.harness.dev/harness/internal/mcpconfig"
)

func writeRPC(t *testing.T, w http.ResponseWriter, id json.RawMessage, result json.RawMessage) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if _, err := fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, id, result); err != nil {
		t.Errorf("response: %v", err)
	}
}
func closeWire(t *testing.T, client *Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Close(ctx); err != nil {
		t.Error(err)
	}
}

func TestMCPHTTPHeadersAndModes(t *testing.T) {
	for _, version := range SupportedVersions {
		t.Run(version, func(t *testing.T) {
			var initialized atomic.Bool
			var deleted atomic.Bool
			var callback atomic.Bool
			var notices atomic.Int64
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer operator-secret" {
					t.Error("configured authorization missing")
				}
				if r.Method == http.MethodDelete {
					deleted.Store(true)
					if r.Header.Get("Mcp-Session-Id") != "session" {
						t.Error("DELETE missing session")
					}
					w.WriteHeader(http.StatusNoContent)
					return
				}
				if r.Method == http.MethodGet {
					if !initialized.Load() {
						t.Error("GET before initialized")
					}
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				if r.Header.Get("Accept") != "application/json, text/event-stream" || r.Header.Get("Content-Type") != "application/json" {
					t.Error("POST content negotiation")
				}
				var message httpMessage
				if json.NewDecoder(r.Body).Decode(&message) != nil {
					t.Error("invalid request")
					w.WriteHeader(400)
					return
				}
				if message.Method == "initialize" {
					if r.Header.Get("Mcp-Session-Id") != "" || r.Header.Get("MCP-Protocol-Version") != "" {
						t.Error("stale initialization headers")
					}
					var proposed struct {
						Version string `json:"protocolVersion"`
					}
					_ = json.Unmarshal(message.Params, &proposed)
					if proposed.Version != PreferredVersion {
						t.Error("didn't prefer November")
					}
					w.Header().Set("Mcp-Session-Id", "session")
					writeRPC(t, w, message.ID, json.RawMessage(fmt.Sprintf(`{"protocolVersion":%q,"capabilities":{"tools":{"listChanged":true}},"serverInfo":{"name":"http","version":"1"}}`, version)))
					return
				}
				if r.Header.Get("Mcp-Session-Id") != "session" || r.Header.Get("MCP-Protocol-Version") != version {
					t.Errorf("negotiated headers: %#v", r.Header)
				}
				switch message.Method {
				case "notifications/initialized":
					initialized.Store(true)
					w.WriteHeader(http.StatusAccepted)
				case "tools/list":
					if !initialized.Load() {
						t.Error("list before initialized")
					}
					writeRPC(t, w, message.ID, json.RawMessage(`{"tools":[]}`))
				case "tools/call":
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":\"root_request\",\"method\":\"roots/list\",\"params\":{}}\n\n")
					_, _ = fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}}\n\n", message.ID)
				case "":
					callback.Store(true)
					w.WriteHeader(http.StatusAccepted)
				default:
					w.WriteHeader(http.StatusAccepted)
				}
			}))
			defer server.Close()
			client, err := Dial(context.Background(), mcpconfig.Server{Name: "http", Transport: "http", URL: server.URL, Headers: map[string]string{"Authorization": "Bearer operator-secret"}}, t.TempDir(), Handlers{Notify: func(string, json.RawMessage) { notices.Add(1) }})
			if err != nil {
				t.Fatal(err)
			}
			if client.Version() != version || !client.HasTools() || !client.ListChanged() {
				t.Fatal("capabilities/version not negotiated")
			}
			if _, err = client.Call(context.Background(), "tools/list", struct{}{}); err != nil {
				t.Fatal(err)
			}
			if _, err = client.Call(context.Background(), "tools/call", struct{}{}); err != nil {
				t.Fatal(err)
			}
			// Close joins any callback response in flight.
			deadline := time.After(3 * time.Second)
			for !callback.Load() {
				select {
				case <-deadline:
					t.Fatal("callback response not posted")
				default:
					time.Sleep(time.Millisecond)
				}
			}
			closeWire(t, client)
			if !deleted.Load() {
				t.Fatal("assigned HTTP session wasn't deleted")
			}
		})
	}
}

func TestMCPSSELogicalResume(t *testing.T) {
	var mu sync.Mutex
	streams := map[string]json.RawMessage{}
	postCalls := 0
	resumeCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodGet {
			cursor := r.Header.Get("Last-Event-ID")
			if cursor == "" {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			mu.Lock()
			id, ok := streams[cursor]
			resumeCalls++
			mu.Unlock()
			if !ok {
				t.Error("wrong logical cursor")
				w.WriteHeader(400)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, ": resumed\r\nid: %s-done\r\ndata: {\"jsonrpc\":\"2.0\",\r\ndata: \"id\":%s,\"result\":{\"cursor\":%q}}\r\n\r\n", cursor, id, cursor)
			return
		}
		var message httpMessage
		_ = json.NewDecoder(r.Body).Decode(&message)
		switch message.Method {
		case "initialize":
			writeRPC(t, w, message.ID, json.RawMessage(`{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"resume","version":"1"}}`))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			var arguments struct {
				Stream string `json:"stream"`
			}
			_ = json.Unmarshal(message.Params, &arguments)
			mu.Lock()
			streams[arguments.Stream] = append(json.RawMessage(nil), message.ID...)
			postCalls++
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, ": priming\r\nid: %s\r\nretry: 0\r\ndata:\r\n\r\n", arguments.Stream)
		}
	}))
	defer server.Close()
	client, err := Dial(context.Background(), mcpconfig.Server{Name: "resume", Transport: "http", URL: server.URL}, t.TempDir(), Handlers{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeWire(t, client)
	var workers sync.WaitGroup
	for _, stream := range []string{"cursor-a", "cursor-b"} {
		workers.Add(1)
		go func() {
			defer workers.Done()
			raw, err := client.Call(context.Background(), "tools/call", struct {
				Stream string `json:"stream"`
			}{stream})
			if err != nil {
				t.Error(err)
				return
			}
			if !strings.Contains(string(raw), stream) {
				t.Errorf("cross-stream result: %s", raw)
			}
		}()
	}
	workers.Wait()
	mu.Lock()
	defer mu.Unlock()
	if postCalls != 2 || resumeCalls != 2 {
		t.Fatalf("POST replay/resume: %d %d", postCalls, resumeCalls)
	}
}
func TestMCPSSEFailureNeverReplaysPOST(t *testing.T) {
	for _, mode := range []string{"no-cursor", "wrong-id", "malformed", "bad-resume", "deadline", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			var posts atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				if r.Method == http.MethodGet {
					if r.Header.Get("Last-Event-ID") == "" {
						w.WriteHeader(http.StatusMethodNotAllowed)
						return
					}
					if mode == "bad-resume" {
						w.Header().Set("Content-Type", "application/json")
						_, _ = fmt.Fprint(w, `{}`)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprint(w, "id: cursor\nretry: 1000\ndata:\n\n")
					return
				}
				var message httpMessage
				_ = json.NewDecoder(r.Body).Decode(&message)
				if message.Method == "initialize" {
					writeRPC(t, w, message.ID, json.RawMessage(`{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"bad","version":"1"}}`))
					return
				}
				if message.Method == "notifications/initialized" || message.Method == "notifications/cancelled" {
					w.WriteHeader(http.StatusAccepted)
					return
				}
				posts.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				switch mode {
				case "wrong-id":
					_, _ = fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"id\":\"wrong\",\"result\":{}}\n\n")
				case "malformed":
					_, _ = fmt.Fprint(w, "data: not-json\n\n")
				case "bad-resume", "deadline":
					_, _ = fmt.Fprint(w, "id: cursor\nretry: 0\ndata:\n\n")
				case "oversize":
					_, _ = fmt.Fprintf(w, "data: %s\n\n", strings.Repeat("x", MaxResponse+1))
				}
			}))
			defer server.Close()
			client, err := Dial(context.Background(), mcpconfig.Server{Name: "bad", Transport: "http", URL: server.URL, CallTimeout: "100ms"}, t.TempDir(), Handlers{})
			if err != nil {
				t.Fatal(err)
			}
			defer closeWire(t, client)
			if _, err = client.Call(context.Background(), "tools/call", struct{}{}); err == nil {
				t.Fatal("malformed/disconnected call succeeded")
			}
			if posts.Load() != 1 {
				t.Fatalf("POST replayed %d times", posts.Load())
			}
		})
	}
}
func TestMCPHTTPNoCredentialRetry(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTemporaryRedirect} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var requests atomic.Int64
			var leaked atomic.Bool
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
			defer target.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Location", target.URL)
				w.WriteHeader(status)
			}))
			defer server.Close()
			_, err := Dial(context.Background(), mcpconfig.Server{Name: "auth", Transport: "http", URL: server.URL, Headers: map[string]string{"Authorization": "Bearer secret"}}, t.TempDir(), Handlers{})
			if err == nil || requests.Load() != 1 || leaked.Load() {
				t.Fatalf("credential retry/redirect: %v %d %v", err, requests.Load(), leaked.Load())
			}
		})
	}
}
