package serve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/tempest"
)

func TestMCPRegistryDefaultsReservationsAndConflicts(t *testing.T) {
	var starts atomic.Int32
	var block atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			w.WriteHeader(400)
			return
		}
		if request.Method == "notifications/initialized" {
			w.WriteHeader(202)
			return
		}
		var result any
		switch request.Method {
		case "initialize":
			starts.Add(1)
			if block.Load() {
				once.Do(func() { close(entered) })
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fixture", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{}}
		default:
			t.Errorf("unexpected fixture method %s", request.Method)
			result = map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	defer fixture.Close()
	f := faux.New(faux.Options{})
	cfg := []tempest.MCPServerConfig{{Name: "fixture", Transport: "http", URL: fixture.URL, Headers: map[string]string{"Authorization": "Bearer fixture-secret"}}}
	s, err := New(Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), DefaultNew: tempest.NewSession{MCPServers: cfg}, Stack: &tempest.StackConfig{Model: f.Model(), StreamFn: f.StreamSimple}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	// New copied secrets and runtime defaults before callers can mutate them.
	cfg[0].Headers["Authorization"] = "changed"
	defaults := s.Defaults()
	if defaults.MCPServers[0].Headers["Authorization"] != "Bearer fixture-secret" {
		t.Fatal("host defaults aliased caller")
	}
	defaults.MCPServers[0].Headers["Authorization"] = "changed again"
	session, err := s.New(context.Background(), s.opts.Cwd, &tempest.NewSession{})
	if err != nil {
		t.Fatal(err)
	}
	live := s.Defaults().MCPServers
	live[0].StartupTimeout = "0.5m"
	live[0].Headers = map[string]string{"authorization": "Bearer fixture-secret"}
	reused, err := s.Resume(context.Background(), s.opts.Cwd, session.ID(), live)
	if err != nil || reused != session || starts.Load() != 1 {
		t.Fatalf("reuse %p %v starts %d", reused, err, starts.Load())
	}
	if _, err := s.Resume(context.Background(), s.opts.Cwd, session.ID(), []tempest.MCPServerConfig{}); !errors.Is(err, tempest.ErrMCPConfigConflict) {
		t.Fatalf("disable live config: %v", err)
	}
	if err := s.CloseSession(context.Background(), session.ID()); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	results := make(chan *tempest.Session, 30)
	for range 30 {
		workers.Go(func() {
			value, err := s.Resume(context.Background(), s.opts.Cwd, session.ID(), nil)
			if err != nil {
				t.Error(err)
				return
			}
			results <- value
		})
	}
	workers.Wait()
	close(results)
	var same *tempest.Session
	for value := range results {
		if same != nil && same != value {
			t.Fatal("duplicate live writer")
		}
		same = value
	}
	if starts.Load() != 2 {
		t.Fatalf("concurrent resume initialized %d times", starts.Load())
	}
	block.Store(true)
	opened := make(chan error, 1)
	go func() { _, err := s.New(context.Background(), s.opts.Cwd, &tempest.NewSession{}); opened <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("startup didn't enter fixture")
	}
	unrelated := make(chan error, 1)
	go func() {
		other, err := s.New(context.Background(), s.opts.Cwd, &tempest.NewSession{MCPServers: []tempest.MCPServerConfig{}})
		if err == nil {
			_, err = s.Resume(context.Background(), s.opts.Cwd, other.ID(), []tempest.MCPServerConfig{})
		}
		unrelated <- err
	}()
	select {
	case err := <-unrelated:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		close(release)
		t.Fatal("network startup held the registry mutex")
	}
	close(release)
	if err := <-opened; err != nil {
		t.Fatal(err)
	}
}
