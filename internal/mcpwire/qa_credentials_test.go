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
	"testing"

	"go.harness.dev/harness/internal/mcpconfig"
)

func TestConfiguredHTTPHeaderSecretIsRedactedFromRemoteErrors(t *testing.T) {
	const secret = "qa-private-header-canary-483921"
	for _, phase := range []string{"initialize", "tools/call"} {
		t.Run(phase, func(t *testing.T) {
			peer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet {
					writer.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				var message httpMessage
				if err := json.NewDecoder(request.Body).Decode(&message); err != nil {
					t.Errorf("decode request: %v", err)
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
				if message.Method == phase {
					writer.Header().Set("Content-Type", "application/json")
					_, err := fmt.Fprintf(writer, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":%q}}`, message.ID, "upstream rejected X-Private-Key="+request.Header.Get("X-Private-Key"))
					if err != nil {
						t.Errorf("write error response: %v", err)
					}
					return
				}
				if message.Method == "initialize" {
					writeRPC(t, writer, message.ID, json.RawMessage(`{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"qa","version":"1"}}`))
					return
				}
				writer.WriteHeader(http.StatusAccepted)
			}))
			defer peer.Close()
			client, err := Dial(context.Background(), mcpconfig.Server{Name: "qa", Transport: "http", URL: peer.URL, Headers: map[string]string{"X-Private-Key": secret}}, t.TempDir(), Handlers{})
			if client != nil {
				defer closeWire(t, client)
			}
			if phase != "initialize" {
				if err != nil {
					t.Fatal(err)
				}
				_, err = client.Call(context.Background(), "tools/call", struct{}{})
			}
			if err == nil {
				t.Fatal("expected remote failure")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatal("configured HTTP header secret escaped through remote error diagnostics")
			}
		})
	}
}

// rotatingAuth hands out one token until set replaces it; a 401 refreshes
// to next, as a refresh-token grant would.
type rotatingAuth struct {
	mu          sync.Mutex
	token, next string
}

func (a *rotatingAuth) Token(context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.token, nil
}
func (a *rotatingAuth) set(token string) { a.mu.Lock(); a.token = token; a.mu.Unlock() }
func (a *rotatingAuth) Rejected(context.Context, Rejection) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.next == "" {
		return errors.New("no refresh available")
	}
	a.token, a.next = a.next, ""
	return nil
}
func (*rotatingAuth) Renewed(string) <-chan struct{} { return nil }

// A server that quotes the OAuth bearer it received, in an error or a
// result, gets it back redacted: the current token, one rotated away while
// its call was in flight, and both tokens of a refreshed retry.
func TestOAuthBearerIsRedactedFromRemoteAnswers(t *testing.T) {
	const (
		first   = "oauth-access-canary-first-2911"
		second  = "oauth-access-canary-second-7730"
		stale   = "oauth-access-canary-stale-1048"
		renewed = "oauth-access-canary-renewed-5516"
	)
	entered, release := make(chan struct{}), make(chan struct{})
	var seen sync.Map
	peer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		token := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		var message struct {
			httpMessage
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if err := json.NewDecoder(request.Body).Decode(&message); err != nil {
			t.Errorf("decode request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		switch message.Method {
		case "initialize":
			writeRPC(t, writer, message.ID, json.RawMessage(`{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"qa","version":"1"}}`))
		case "tools/call":
			if token == stale {
				seen.Store(stale, true)
				writer.WriteHeader(http.StatusUnauthorized)
				return
			}
			quoted := token
			if _, ok := seen.Load(stale); ok && token == renewed {
				quoted = stale + " then " + renewed
			}
			if message.Params.Name == "slow" {
				close(entered)
				<-release
			}
			writeRPC(t, writer, message.ID, json.RawMessage(fmt.Sprintf(`{"content":[{"type":"text","text":%q}]}`, "backend accepted "+quoted)))
		default:
			writer.WriteHeader(http.StatusAccepted)
		}
	}))
	defer peer.Close()
	auth := &rotatingAuth{token: first}
	client, err := Dial(context.Background(), mcpconfig.Server{Name: "qa", Transport: "http", URL: peer.URL}, t.TempDir(), Handlers{Auth: auth})
	if err != nil {
		t.Fatal(err)
	}
	defer closeWire(t, client)
	leaks := func(stage string, raw json.RawMessage, err error) {
		t.Helper()
		if err != nil {
			t.Errorf("%s: %v", stage, err)
			return
		}
		for _, token := range []string{first, second, stale, renewed} {
			if strings.Contains(string(raw), token) {
				t.Errorf("%s: OAuth bearer escaped in %s", stage, raw)
			}
		}
		if !strings.Contains(string(raw), "[redacted]") {
			t.Errorf("%s: no redaction in %s", stage, raw)
		}
	}
	slow := make(chan struct{})
	go func() {
		defer close(slow)
		raw, err := client.Call(context.Background(), "tools/call", map[string]string{"name": "slow"})
		leaks("rotated while in flight", raw, err)
	}()
	<-entered
	auth.set(second)
	raw, err := client.Call(context.Background(), "tools/call", map[string]string{"name": "fast"})
	leaks("current", raw, err)
	close(release)
	<-slow
	auth.mu.Lock()
	auth.token, auth.next = stale, renewed
	auth.mu.Unlock()
	raw, err = client.Call(context.Background(), "tools/call", map[string]string{"name": "fast"})
	leaks("refreshed retry", raw, err)
}
