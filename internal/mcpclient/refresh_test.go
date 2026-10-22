package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.harness.dev/harness/internal/mcpconfig"
)

func TestMCPListChangedDirtyRefresh(t *testing.T) {
	var generation atomic.Int64
	var active atomic.Int64
	var initialized atomic.Bool
	notifications := make(chan struct{}, 2)
	refreshStarted := make(chan struct{})
	release := make(chan struct{})
	trailing := make(chan struct{})
	listenerReady := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodGet {
			if !initialized.Load() {
				t.Error("listener before initialized")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			close(listenerReady)
			for {
				select {
				case <-notifications:
					_, _ = fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/tools/list_changed\"}\n\n")
					w.(http.Flusher).Flush()
				case <-r.Context().Done():
					return
				}
			}
		}
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&message)
		result := ""
		switch message.Method {
		case "initialize":
			result = `{"protocolVersion":"2025-11-25","capabilities":{"tools":{"listChanged":true}},"serverInfo":{"name":"refresh","version":"1"}}`
		case "notifications/initialized":
			initialized.Store(true)
			w.WriteHeader(http.StatusAccepted)
			return
		case "tools/list":
			if active.Add(1) != 1 {
				t.Error("concurrent full discovery")
			}
			defer active.Add(-1)
			current := generation.Load()
			if current == 1 {
				close(refreshStarted)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			if current == 2 {
				select {
				case <-trailing:
				default:
					close(trailing)
				}
			}
			result = fmt.Sprintf(`{"tools":[{"name":"lookup","inputSchema":{"type":"object","properties":{"generation%d":{"type":"string"}}}}]}`, current)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, message.ID, result)
	}))
	defer server.Close()
	manager, err := Open(context.Background(), t.TempDir(), t.TempDir(), []mcpconfig.Server{{Name: "refresh", Transport: "http", URL: server.URL}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-listenerReady:
	case <-time.After(3 * time.Second):
		t.Fatal("listener didn't open")
	}
	old := manager.Tools()[0]
	generation.Store(1)
	notifications <- struct{}{}
	select {
	case <-refreshStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("refresh didn't start")
	}
	generation.Store(2)
	notifications <- struct{}{}
	// The second notification must be admitted while the first discovery is blocked.
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		manager.servers[0].mu.Lock()
		dirty := manager.servers[0].dirty
		manager.servers[0].mu.Unlock()
		if dirty {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("dirty notification was lost")
		case <-time.After(time.Millisecond):
		}
	}
	close(release)
	select {
	case <-trailing:
	case <-time.After(3 * time.Second):
		t.Fatal("missing trailing discovery")
	}
	if err := manager.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	latest := manager.Tools()[0]
	if !strings.Contains(string(latest.Parameters), "generation2") || !strings.Contains(string(old.Parameters), "generation0") {
		t.Fatalf("catalog mutation old=%s latest=%s", old.Parameters, latest.Parameters)
	}
}
