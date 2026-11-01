package mcpwire

import (
	"context"
	"encoding/json"
	"errors"
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
			callback := make(chan struct{})
			var answered sync.Once
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
					// The server waits for the answer to its own request
					// before it answers the call.
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":\"root_request\",\"method\":\"roots/list\",\"params\":{}}\n\n")
					w.(http.Flusher).Flush()
					select {
					case <-callback:
					case <-time.After(3 * time.Second):
						t.Error("callback response not posted")
					case <-r.Context().Done():
						return
					}
					_, _ = fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}}\n\n", message.ID)
				case "":
					answered.Do(func() { close(callback) })
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
			if caps := client.Capabilities(); client.Version() != version || !caps.Tools || !caps.ToolsListChanged {
				t.Fatal("capabilities/version not negotiated")
			}
			if _, err = client.Call(context.Background(), "tools/list", struct{}{}); err != nil {
				t.Fatal(err)
			}
			if _, err = client.Call(context.Background(), "tools/call", struct{}{}); err != nil {
				t.Fatal(err)
			}
			// The call ended only after its callback response was posted.
			select {
			case <-callback:
			default:
				t.Fatal("callback response not posted")
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

// stubAuth hands out one token and refuses every rejection.
type stubAuth struct{}

func (stubAuth) Token(context.Context) (string, error) { return "token", nil }
func (stubAuth) Rejected(context.Context, Rejection) error {
	return errors.New("mcp_auth: login required")
}
func (stubAuth) Renewed(string) <-chan struct{} { return nil }

// An authorization failure on the GET that resumes a POST's response stream
// is not an AuthError: the server already processed the POST, so a caller
// must not log in and send it again.
func TestMCPSSEResumeRefusedForAuthorizationIsNotRetryable(t *testing.T) {
	var posts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
			return
		case r.Method == http.MethodGet && r.Header.Get("Last-Event-ID") == "":
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		case r.Method == http.MethodGet:
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var message httpMessage
		_ = json.NewDecoder(r.Body).Decode(&message)
		switch message.Method {
		case "initialize":
			writeRPC(t, w, message.ID, json.RawMessage(`{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"auth","version":"1"}}`))
		case "notifications/initialized", "notifications/cancelled":
			w.WriteHeader(http.StatusAccepted)
		default:
			posts.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "id: cursor\nretry: 0\ndata:\n\n")
		}
	}))
	defer server.Close()
	client, err := Dial(context.Background(), mcpconfig.Server{Name: "auth", Transport: "http", URL: server.URL}, t.TempDir(), Handlers{Auth: stubAuth{}})
	if err != nil {
		t.Fatal(err)
	}
	defer closeWire(t, client)
	_, err = client.Call(context.Background(), "tools/call", struct{}{})
	var authErr *AuthError
	if err == nil || errors.As(err, &authErr) || !strings.Contains(err.Error(), "remote outcome may be unknown") {
		t.Fatalf("refused resume = %v, want an unknown outcome that is not an AuthError", err)
	}
	if posts.Load() != 1 {
		t.Fatalf("POST sent %d times", posts.Load())
	}
}

// renewingAuth hands out its current token and refuses every rejection,
// as a token that needs a login does; renew stands in for that login.
type renewingAuth struct {
	mu      sync.Mutex
	token   string
	renewed chan struct{}
}

func (a *renewingAuth) Token(context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.token, nil
}
func (a *renewingAuth) Rejected(context.Context, Rejection) error {
	return errors.New("mcp_auth: login required")
}
func (a *renewingAuth) Renewed(refused string) <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != refused {
		renewed := make(chan struct{})
		close(renewed)
		return renewed
	}
	return a.renewed
}
func (a *renewingAuth) renew(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.token = token
	close(a.renewed)
	a.renewed = make(chan struct{})
}

// Tempest's answer to a server request that is refused for authorization
// (the token died while the user answered) keeps the connection, like the
// stream: the answer is posted once more after the next login, but only
// within the answer's call timeout and while the server's request lasts,
// which for a request on a call's stream is while that call does: not once
// the server has answered the call or Tempest has cancelled it.
func TestMCPHTTPReplyRefusedForAuthorizationKeepsTheConnection(t *testing.T) {
	const (
		callTimeout = time.Second
		// settle is how long a reply that should not come is waited for.
		settle = 100 * time.Millisecond
	)
	for _, ending := range []string{"renewed", "call answered", "call cancelled", "call timed out"} {
		t.Run(ending, func(t *testing.T) {
			server, replies, end := answerServer(t, "new")
			auth := &renewingAuth{token: "old", renewed: make(chan struct{})}
			client, err := Dial(context.Background(), mcpconfig.Server{Name: "auth", Transport: "http", URL: server.URL, CallTimeout: callTimeout.String()}, t.TempDir(), Handlers{Auth: auth})
			if err != nil {
				t.Fatal(err)
			}
			defer closeWire(t, client)
			defer end()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			called := make(chan error, 1)
			go func() {
				// Not tools/call: cancelling that closes the connection.
				_, err := client.Call(ctx, "prompts/get", struct{}{})
				called <- err
			}()
			if got := <-replies; got != "Bearer old" {
				t.Fatalf("first answer carried %q", got)
			}
			switch ending {
			case "renewed":
				auth.renew("new")
				select {
				case got := <-replies:
					if got != "Bearer new" {
						t.Fatalf("answer after the login carried %q", got)
					}
				case <-time.After(3 * callTimeout):
					t.Fatal("the answer was not posted again after the login")
				}
			case "call answered":
				// The server gives up on its request and answers the call.
				end()
			case "call cancelled":
				cancel()
			case "call timed out":
				// The call's timeout passes without a login.
			}
			if err := <-called; (err != nil) != (ending == "call cancelled" || ending == "call timed out") {
				t.Fatalf("call = %v", err)
			}
			auth.renew("new")
			if _, err := client.Call(context.Background(), "tools/list", struct{}{}); err != nil {
				t.Fatalf("the answer's authorization failure closed the connection: %v", err)
			}
			select {
			case got := <-replies:
				t.Fatalf("answer posted again (%q) after its request ended or a second time", got)
			case <-time.After(settle):
			}
		})
	}
}

// answerServer is an MCP server whose tools/call and prompts/get ask Tempest
// a ping on their response stream and, as a server waiting for the answer
// does, hold the call open until an answer carrying "Bearer "+accept is
// accepted or end is called. Every other answer is refused with 401. replies
// receives each answer's Authorization.
func answerServer(t *testing.T, accept string) (server *httptest.Server, replies <-chan string, end func()) {
	t.Helper()
	answers := make(chan string, 8)
	answered, ended := make(chan struct{}), make(chan struct{})
	var answerOnce, endOnce sync.Once
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
			return
		case http.MethodGet:
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var message httpMessage
		_ = json.NewDecoder(r.Body).Decode(&message)
		switch message.Method {
		case "initialize":
			writeRPC(t, w, message.ID, json.RawMessage(`{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"auth","version":"1"}}`))
		case "notifications/initialized", "notifications/cancelled":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call", "prompts/get":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"id\":\"ask\",\"method\":\"ping\",\"params\":{}}\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-answered:
			case <-ended:
			case <-r.Context().Done():
				return
			}
			_, _ = fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"content\":[]}}\n\n", message.ID)
		case "tools/list":
			writeRPC(t, w, message.ID, json.RawMessage(`{"tools":[]}`))
		case "":
			authorization := r.Header.Get("Authorization")
			answers <- authorization
			if authorization != "Bearer "+accept {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			answerOnce.Do(func() { close(answered) })
		}
	}))
	t.Cleanup(server.Close)
	return server, answers, func() { endOnce.Do(func() { close(ended) }) }
}

// refreshingAuth is a renewingAuth whose token "new", once rejected, is
// refreshed to "newer" without a user, as an OAuth refresh does; a
// rejection of the retry that follows is final.
type refreshingAuth struct{ renewingAuth }

func (a *refreshingAuth) Rejected(_ context.Context, rejection Rejection) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if rejection.Token == "new" && !rejection.Retried {
		a.token = "newer"
		return nil
	}
	return errors.New("mcp_auth: login required")
}

// The answer posted again after a login is that one POST: when the server
// refuses it too, Tempest does not refresh and post it a third time.
func TestMCPHTTPReplyRepostIsOnePost(t *testing.T) {
	const settle = 200 * time.Millisecond
	server, replies, end := answerServer(t, "newer")
	auth := &refreshingAuth{renewingAuth{token: "old", renewed: make(chan struct{})}}
	client, err := Dial(context.Background(), mcpconfig.Server{Name: "auth", Transport: "http", URL: server.URL, CallTimeout: "5s"}, t.TempDir(), Handlers{Auth: auth})
	if err != nil {
		t.Fatal(err)
	}
	defer closeWire(t, client)
	defer end()
	called := make(chan error, 1)
	go func() {
		_, err := client.Call(context.Background(), "tools/call", struct{}{})
		called <- err
	}()
	if got := <-replies; got != "Bearer old" {
		t.Fatalf("first answer carried %q", got)
	}
	auth.renew("new")
	if got := <-replies; got != "Bearer new" {
		t.Fatalf("answer after the login carried %q", got)
	}
	select {
	case got := <-replies:
		t.Fatalf("one repost sent a second POST, carrying %q", got)
	case <-time.After(settle):
	}
	end()
	if err := <-called; err != nil {
		t.Fatal(err)
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

// A 404 means an expired session only when the request named one (MCP
// Streamable HTTP, session management); a 404 to initialize is a wrong URL.
func TestMCPHTTPNotFoundWithoutSessionIsNotAnExpiredSession(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	_, err := Dial(context.Background(), mcpconfig.Server{Name: "missing", Transport: "http", URL: server.URL + "/wrong"}, t.TempDir(), Handlers{})
	if err == nil || strings.Contains(err.Error(), "session expired") || !strings.Contains(err.Error(), "404") {
		t.Fatalf("initialize answered 404 = %v, want an error naming the 404 and not a session", err)
	}
}
