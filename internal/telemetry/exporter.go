package telemetry

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// Batch defaults follow the OpenTelemetry batch span processor
	// (OTEL_BSP_MAX_QUEUE_SIZE, OTEL_BSP_MAX_EXPORT_BATCH_SIZE,
	// OTEL_BSP_SCHEDULE_DELAY).
	defaultSpanQueueSize     = 2048
	defaultSpanBatchSize     = 512
	defaultSpanBatchInterval = 5 * time.Second
	// maxResponseDrain bounds how much of a collector's reply is read so the
	// connection can be reused; OTLP partial-success bodies are small.
	maxResponseDrain = 64 << 10
)

// otlpClient posts OTLP/HTTP JSON requests to one collector.
type otlpClient struct {
	http      *http.Client
	headers   map[string]string
	gzip      bool
	timeout   time.Duration
	userAgent string
}

// post sends one export request. The request carries no traceparent: the
// exporter's own traffic is not part of any run's trace.
func (c *otlpClient) post(ctx context.Context, endpoint string, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	payload := body
	if c.gzip {
		var compressed bytes.Buffer
		writer := gzip.NewWriter(&compressed)
		if _, err := writer.Write(body); err != nil {
			return fmt.Errorf("gzip OTLP body: %w", err)
		}
		if err := writer.Close(); err != nil {
			return fmt.Errorf("gzip OTLP body: %w", err)
		}
		payload = compressed.Bytes()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("OTLP endpoint %s: invalid URL", redactEndpoint(endpoint))
	}
	for key, value := range c.headers {
		request.Header.Set(key, value)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", c.userAgent)
	if c.gzip {
		request.Header.Set("Content-Encoding", "gzip")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return redactRequestError(err, endpoint)
	}
	defer response.Body.Close()
	// Draining lets the transport reuse the connection; a short read only
	// costs that reuse, so its error does not change the export outcome.
	_, drainErr := io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseDrain))
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("collector answered %s", response.Status)
	}
	if drainErr != nil {
		slog.Debug("otlp response drain failed", "error", drainErr)
	}
	return nil
}

// redactEndpoint drops userinfo, query and fragment from an endpoint shown in
// an error or log line: collectors take credentials in any of them, under
// names no generic redactor knows.
func redactEndpoint(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "[unparsable endpoint]"
	}
	return redactURL(parsed)
}

func redactURL(parsed *url.URL) string {
	redacted := *parsed
	redacted.User, redacted.RawQuery, redacted.ForceQuery, redacted.Fragment, redacted.RawFragment = nil, "", false, "", ""
	return redacted.String()
}

// redactRequestError replaces the URL net/http puts in a request error,
// which keeps the endpoint's query (and a redirect target's), with the
// redacted endpoint.
func redactRequestError(err error, endpoint string) error {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return err
	}
	return &url.Error{Op: urlErr.Op, URL: redactEndpoint(endpoint), Err: urlErr.Err}
}

// maxCollectorRedirects is net/http's default redirect limit.
const maxCollectorRedirects = 10

// sameOriginRedirect is the OTLP client's redirect policy: a collector may
// redirect within the configured endpoint's origin only. net/http copies
// custom headers, such as a collector API key, to any origin a redirect
// names, and the operator configured them for this origin alone.
func sameOriginRedirect(request *http.Request, via []*http.Request) error {
	if len(via) >= maxCollectorRedirects {
		return fmt.Errorf("stopped after %d redirects", maxCollectorRedirects)
	}
	if origin(request.URL) != origin(via[0].URL) {
		return fmt.Errorf("collector redirected to another origin, %s; OTLP export follows redirects within the endpoint's origin only", redactURL(request.URL))
	}
	return nil
}

// origin is a URL's scheme, host and port, with the scheme's default port
// filled in.
func origin(target *url.URL) string {
	port := target.Port()
	if port == "" {
		switch target.Scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	return strings.ToLower(target.Scheme) + "://" + strings.ToLower(target.Hostname()) + ":" + port
}

// exportHealth logs an exporter's first failure as a warning and later ones
// at debug level, so an unreachable collector produces one visible line, not
// one per batch. A success re-arms the warning.
type exportHealth struct {
	signal  string
	mu      sync.Mutex
	failing bool
	failed  atomic.Uint64
}

func (h *exportHealth) record(err error, items int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err == nil {
		if h.failing {
			slog.Info("otlp export recovered", "signal", h.signal)
		}
		h.failing = false
		return
	}
	h.failed.Add(uint64(items))
	if h.failing {
		slog.Debug("otlp export failed", "signal", h.signal, "items", items, "error", err)
		return
	}
	h.failing = true
	slog.Warn("otlp export failed; telemetry is dropped until the collector answers", "signal", h.signal, "items", items, "error", err)
}

// spanBatcher is the batch span processor. enqueue never blocks: when the
// queue is full the span is dropped and counted, so a slow or unreachable
// collector costs the turn loop nothing. One worker goroutine exports
// batches when one is full or the interval passes, and on shutdown drains the
// queue under the caller's deadline.
type spanBatcher struct {
	queue    chan *span
	export   func(context.Context, []*span) error
	maxBatch int
	interval time.Duration
	dropped  atomic.Uint64
	health   *exportHealth

	closing   atomic.Bool
	closeOnce sync.Once
	stop      chan struct{}
	done      chan struct{}
	// finalCtx is written before stop is closed and read by the worker after,
	// so the channel close orders the accesses.
	finalCtx context.Context
	finalErr error
}

func newSpanBatcher(queueSize, maxBatch int, interval time.Duration, health *exportHealth, export func(context.Context, []*span) error) *spanBatcher {
	batcher := &spanBatcher{
		queue:    make(chan *span, queueSize),
		export:   export,
		maxBatch: maxBatch,
		interval: interval,
		health:   health,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go batcher.run()
	return batcher
}

func (b *spanBatcher) enqueue(finished *span) {
	if b.closing.Load() {
		b.dropped.Add(1)
		return
	}
	select {
	case b.queue <- finished:
	default:
		b.dropped.Add(1)
	}
}

func (b *spanBatcher) run() {
	defer close(b.done)
	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()
	batch := make([]*span, 0, b.maxBatch)
	for {
		select {
		case finished := <-b.queue:
			batch = append(batch, finished)
			if len(batch) >= b.maxBatch {
				b.send(context.Background(), batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				b.send(context.Background(), batch)
				batch = batch[:0]
			}
		case <-b.stop:
			b.finalErr = b.drain(b.finalCtx, batch)
			return
		}
	}
}

// drain exports what is batched and queued at shutdown, stopping at the
// deadline; spans still queued then are counted as dropped.
func (b *spanBatcher) drain(ctx context.Context, batch []*span) error {
	for {
		for len(batch) < b.maxBatch {
			select {
			case finished := <-b.queue:
				batch = append(batch, finished)
				continue
			default:
			}
			break
		}
		if len(batch) == 0 {
			return nil
		}
		if ctx.Err() != nil {
			b.dropped.Add(uint64(len(batch) + len(b.queue)))
			return ctx.Err()
		}
		if err := b.send(ctx, batch); err != nil {
			return err
		}
		batch = batch[:0]
	}
}

// send exports batch and then clears it, so the reused batch array does not
// keep exported spans, and the content they captured, reachable.
func (b *spanBatcher) send(ctx context.Context, batch []*span) error {
	err := b.export(ctx, batch)
	clear(batch)
	b.health.record(err, len(batch))
	return err
}

// shutdown stops accepting spans, exports the rest and waits until that is
// done or ctx ends. Later calls wait for the same flush.
func (b *spanBatcher) shutdown(ctx context.Context) error {
	b.closeOnce.Do(func() {
		b.finalCtx = ctx
		b.closing.Store(true)
		close(b.stop)
	})
	select {
	case <-b.done:
		return b.finalErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
