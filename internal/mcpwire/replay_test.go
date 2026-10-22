package mcpwire

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.harness.dev/harness/internal/mcpconfig"
)

func TestMCPHTTPNeverReplaysAmbiguousPOST(t *testing.T) {
	var dispatched atomic.Int64
	endpoint := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var message httpMessage
		if err := json.NewDecoder(request.Body).Decode(&message); err != nil {
			t.Error(err)
			return
		}
		if message.Method == "initialize" {
			writeRPC(t, response, message.ID, json.RawMessage(`{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"replay","version":"1"}}`))
			return
		}
		dispatched.Add(1)
		connection, _, err := response.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		// The server accepted the side effect, then lost its reply on a reused connection.
		if err = connection.Close(); err != nil {
			t.Error(err)
		}
	}))
	defer endpoint.Close()
	configs, err := mcpconfig.Normalize([]mcpconfig.Server{{Name: "replay", Transport: "http", URL: endpoint.URL, Headers: map[string]string{"Idempotency-Key": "not-permission-to-retry"}}})
	if err != nil {
		t.Fatal(err)
	}
	wire, err := dialHTTP(configs[0], Handlers{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := wire.close(ctx); err != nil {
			t.Error(err)
		}
	}()
	if _, err = wire.call(context.Background(), "initialize", "initialize", struct{}{}); err != nil {
		t.Fatal(err)
	}
	if _, err = wire.call(context.Background(), "side-effect", "tools/call", struct{}{}); err == nil {
		t.Fatal("lost side-effect reply succeeded")
	}
	if dispatched.Load() != 1 {
		t.Fatalf("ambiguous tools/call replayed %d times", dispatched.Load())
	}
}
