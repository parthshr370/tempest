package telemetry

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
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
	// An export the collector answers with a retryable status is sent again
	// after about this delay, doubled each time up to exportRetryMaxDelay,
	// and never sooner than the answer's Retry-After; all within the
	// export's timeout.
	exportRetryInitialDelay = time.Second
	exportRetryMaxDelay     = 5 * time.Second
)

// otlpClient posts OTLP/HTTP JSON requests to one collector.
type otlpClient struct {
	http      *http.Client
	headers   map[string]string
	gzip      bool
	timeout   time.Duration
	userAgent string
}

// post sends one export request, again after a retryable answer while its
// timeout allows (OTLP/HTTP: 429, 502, 503 and 504 are retryable, and a
// Retry-After header names the delay). The request carries no traceparent:
// the exporter's own traffic is not part of any run's trace. A collector
// that accepted the request but rejected part of it returns a
// *partialSuccess error naming how much.
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
	backoff := exportRetryInitialDelay
	for {
		retryAfter, retryable, err := c.send(ctx, endpoint, payload)
		if !retryable {
			return err
		}
		// Full jitter in the upper half keeps clients that failed together
		// from retrying together; a Retry-After the collector named is the
		// least it waits.
		delay := max(backoff/2+rand.N(backoff/2+1), retryAfter)
		backoff = min(2*backoff, exportRetryMaxDelay)
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < delay {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return err
		}
	}
}

// send makes one export request. retryable reports a status OTLP/HTTP lets
// a client retry, and retryAfter the delay its Retry-After header names.
func (c *otlpClient) send(ctx context.Context, endpoint string, payload []byte) (retryAfter time.Duration, retryable bool, err error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return 0, false, fmt.Errorf("OTLP endpoint %s: invalid URL", redactEndpoint(endpoint))
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
		return 0, false, redactRequestError(err, endpoint)
	}
	defer response.Body.Close()
	// Reading the reply to its end lets the transport reuse the connection;
	// a short read only costs that reuse and a partial-success count.
	reply, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseDrain))
	switch response.StatusCode {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return parseRetryAfter(response.Header.Get("Retry-After"), time.Now(), c.timeout), true, fmt.Errorf("collector answered %s", response.Status)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return 0, false, fmt.Errorf("collector answered %s", response.Status)
	}
	if readErr != nil {
		slog.Debug("otlp response drain failed", "error", readErr)
	}
	return 0, false, parsePartialSuccess(reply)
}

// parseRetryAfter reads a Retry-After value, delay seconds or an HTTP date,
// as a delay from now, at most limit (the export's timeout: a longer delay
// cannot be waited out either way); 0 when it is absent or unreadable.
// Seconds past what a time.Duration holds are clamped too, not wrapped.
func parseRetryAfter(value string, now time.Time, limit time.Duration) time.Duration {
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil || errors.Is(err, strconv.ErrRange) {
		if seconds <= 0 {
			return 0
		}
		if seconds > int64(limit/time.Second) {
			return limit
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		return min(max(0, at.Sub(now)), limit)
	}
	return 0
}

// partialSuccess is a collector's answer that it accepted an export but
// rejected part of it (OTLP "Partial Success"). Rejected counts the spans,
// data points or log records it dropped; they must not be sent again.
type partialSuccess struct {
	Rejected int64
	Message  string
}

func (p *partialSuccess) Error() string {
	if p.Message == "" {
		return fmt.Sprintf("collector rejected %d items", p.Rejected)
	}
	return fmt.Sprintf("collector rejected %d items: %s", p.Rejected, p.Message)
}

// otlpCount is an OTLP/JSON int64: a decimal string, or a number from a
// lenient collector.
type otlpCount int64

func (c *otlpCount) UnmarshalJSON(raw []byte) error {
	value, err := strconv.ParseInt(strings.Trim(string(raw), `"`), 10, 64)
	*c = otlpCount(value)
	return err
}

// parsePartialSuccess returns a *partialSuccess for a success reply that
// rejected items, nil otherwise. An empty or unreadable reply is a full
// success: the body is optional.
func parsePartialSuccess(reply []byte) error {
	var answer struct {
		PartialSuccess struct {
			RejectedSpans      otlpCount `json:"rejectedSpans"`
			RejectedDataPoints otlpCount `json:"rejectedDataPoints"`
			ErrorMessage       string    `json:"errorMessage"`
		} `json:"partialSuccess"`
	}
	if len(reply) == 0 || json.Unmarshal(reply, &answer) != nil {
		return nil
	}
	partial := answer.PartialSuccess
	if rejected := int64(partial.RejectedSpans + partial.RejectedDataPoints); rejected > 0 {
		return &partialSuccess{Rejected: rejected, Message: partial.ErrorMessage}
	}
	if partial.ErrorMessage != "" {
		slog.Warn("otlp collector warned about an export", "message", partial.ErrorMessage)
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
// one per batch. A success re-arms the warning. failed counts the items of
// exports that failed, rejected the items a collector refused in an export
// it otherwise accepted.
type exportHealth struct {
	signal   string
	mu       sync.Mutex
	failing  bool
	failed   atomic.Uint64
	rejected atomic.Uint64
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
	var partial *partialSuccess
	if errors.As(err, &partial) {
		h.rejected.Add(uint64(partial.Rejected))
	} else {
		h.failed.Add(uint64(items))
	}
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
// deadline; spans still queued then are counted as dropped. A batch the
// collector rejects is counted as failed and the next one is still sent, so
// one bad batch does not strand the rest. It returns the first export error
// and the deadline, if either happened.
func (b *spanBatcher) drain(ctx context.Context, batch []*span) error {
	var failed error
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
			return failed
		}
		if ctx.Err() != nil {
			b.dropped.Add(uint64(len(batch) + len(b.queue)))
			return errors.Join(failed, ctx.Err())
		}
		if err := b.send(ctx, batch); err != nil && failed == nil {
			failed = err
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
