package telemetry

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/config"
	"go.harness.dev/harness/internal/tracecontext"
)

// collector is a fake OTLP/HTTP receiver that keeps every decoded request.
type collector struct {
	mu       sync.Mutex
	requests []collected
	server   *httptest.Server
}

type collected struct {
	path    string
	headers http.Header
	body    []byte
}

func newCollector(t *testing.T) *collector {
	t.Helper()
	c := &collector{}
	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reader := io.Reader(r.Body)
		if r.Header.Get("Content-Encoding") == "gzip" {
			unzipped, err := gzip.NewReader(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			reader = unzipped
		}
		body, err := io.ReadAll(reader)
		if err != nil || !json.Valid(body) {
			http.Error(w, "invalid OTLP JSON", http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		c.requests = append(c.requests, collected{path: r.URL.Path, headers: r.Header.Clone(), body: body})
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{}")
	}))
	t.Cleanup(c.server.Close)
	return c
}

func (c *collector) snapshot() []collected {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]collected(nil), c.requests...)
}

// spanNames decodes every received trace request into span names.
func (c *collector) spanNames(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, request := range c.snapshot() {
		if request.path != "/v1/traces" {
			continue
		}
		for _, received := range decodeSpans(t, request.body) {
			names = append(names, received.Name)
		}
	}
	return names
}

func testSpan(name string) *span {
	now := time.Now()
	return &span{traceID: tracecontext.NewTraceID(), spanID: tracecontext.NewSpanID(), name: name, kind: spanKindInternal, start: now, end: now}
}

func TestExporterPostsGzipWithHeaders(t *testing.T) {
	c := newCollector(t)
	tel := New(Options{Settings: config.OTelSettings{
		TracesEndpoint: c.server.URL + "/v1/traces",
		Headers:        map[string]string{"x-api-key": "secret"},
		Gzip:           true,
		Timeout:        5 * time.Second,
		ServiceName:    "tempest",
	}, Version: "test"})
	tel.record(testSpan("one"), true)
	tel.record(testSpan("unsampled"), false)
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	requests := c.snapshot()
	if len(requests) != 1 {
		t.Fatalf("collector got %d requests, want 1", len(requests))
	}
	got := requests[0].headers
	if got.Get("x-api-key") != "secret" || got.Get("Content-Type") != "application/json" || got.Get("Content-Encoding") != "gzip" || got.Get(tracecontext.Header) != "" {
		t.Fatalf("request headers = %v", got)
	}
	if names := c.spanNames(t); len(names) != 1 || names[0] != "one" {
		t.Fatalf("spans = %v, want only the sampled span", names)
	}
}

func TestBatcherDropsWhenQueueIsFull(t *testing.T) {
	release := make(chan struct{})
	exported := make(chan int, 4)
	batcher := newSpanBatcher(2, 1, time.Hour, &exportHealth{signal: "traces"}, func(ctx context.Context, spans []*span) error {
		exported <- len(spans)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	batcher.enqueue(testSpan("exporting"))
	<-exported
	// The worker is stuck exporting; the queue holds two, the rest drop.
	start := time.Now()
	for range 5 {
		batcher.enqueue(testSpan("queued"))
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("enqueue blocked for %s", elapsed)
	}
	if dropped := batcher.dropped.Load(); dropped != 3 {
		t.Fatalf("dropped = %d, want 3", dropped)
	}
	close(release)
	if err := batcher.shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownFlushHonorsDeadline(t *testing.T) {
	// The blackhole reads the request, then never answers until the test ends.
	release := make(chan struct{})
	blackhole := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer blackhole.Close()
	defer close(release)
	tel := New(Options{Settings: config.OTelSettings{TracesEndpoint: blackhole.URL + "/v1/traces", Timeout: time.Minute, ServiceName: "tempest"}})
	for range 10 {
		tel.record(testSpan("stuck"), true)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := tel.Shutdown(ctx)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("shutdown took %s past a 200ms deadline", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error = %v, want the deadline", err)
	}
}
