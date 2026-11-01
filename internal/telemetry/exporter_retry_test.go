package telemetry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// scriptedCollector answers each export with the next of its answers, then
// with 200 and an empty body.
func scriptedCollector(t *testing.T, answers ...func(http.ResponseWriter)) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if index := requests.Add(1) - 1; int(index) < len(answers) {
			answers[index](w)
		}
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

func status(code int, retryAfter string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(code)
	}
}

// OTLP/HTTP: 429, 502, 503 and 504 are retryable, with backoff and the
// answer's Retry-After honoured, within the export's timeout; other
// failures are not retried.
func TestExportRetriesRetryableAnswersWithinItsTimeout(t *testing.T) {
	const retryAfterSeconds = 1
	retryAfter := strconv.Itoa(retryAfterSeconds)
	for _, test := range []struct {
		name     string
		answers  []func(http.ResponseWriter)
		timeout  time.Duration
		requests int64
		minWait  time.Duration
		fails    bool
	}{
		{"429 then accepted, after Retry-After", []func(http.ResponseWriter){status(http.StatusTooManyRequests, retryAfter)}, 5 * time.Second, 2, retryAfterSeconds * time.Second, false},
		{"503 then accepted", []func(http.ResponseWriter){status(http.StatusServiceUnavailable, "")}, 5 * time.Second, 2, 0, false},
		{"502 and 504 then accepted", []func(http.ResponseWriter){status(http.StatusBadGateway, ""), status(http.StatusGatewayTimeout, "")}, 5 * time.Second, 3, 0, false},
		{"Retry-After past the timeout", []func(http.ResponseWriter){status(http.StatusServiceUnavailable, "60")}, 2 * time.Second, 1, 0, true},
		{"400 is not retried", []func(http.ResponseWriter){status(http.StatusBadRequest, "")}, 5 * time.Second, 1, 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, requests := scriptedCollector(t, test.answers...)
			client := &otlpClient{http: server.Client(), timeout: test.timeout, userAgent: "test"}
			start := time.Now()
			err := client.post(context.Background(), server.URL+"/v1/traces", []byte("{}"))
			elapsed := time.Since(start)
			if (err != nil) != test.fails || requests.Load() != test.requests {
				t.Fatalf("err=%v after %d requests, want failure=%v after %d", err, requests.Load(), test.fails, test.requests)
			}
			if elapsed < test.minWait || elapsed >= test.timeout {
				t.Fatalf("took %s, want at least %s and under the %s timeout", elapsed, test.minWait, test.timeout)
			}
		})
	}
}

// A collector that accepts an export but rejects some spans in it (OTLP
// partial success) has lost them: they count as rejected, and the batch is
// not sent again.
func TestExportCountsPartialSuccessAsLoss(t *testing.T) {
	server, requests := scriptedCollector(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"partialSuccess":{"rejectedSpans":"2","errorMessage":"spans too large"}}`))
	})
	client := &otlpClient{http: server.Client(), timeout: 5 * time.Second, userAgent: "test"}
	err := client.post(context.Background(), server.URL+"/v1/traces", []byte("{}"))
	var partial *partialSuccess
	if !errors.As(err, &partial) || partial.Rejected != 2 || requests.Load() != 1 {
		t.Fatalf("partial success = %v after %d requests, want 2 rejected after one", err, requests.Load())
	}
	health := &exportHealth{signal: "traces"}
	health.record(err, 3)
	if health.rejected.Load() != 2 || health.failed.Load() != 0 {
		t.Fatalf("rejected %d failed %d, want 2 and 0", health.rejected.Load(), health.failed.Load())
	}
	// A full success may carry an empty partialSuccess, or nothing.
	for _, reply := range []string{``, `{}`, `{"partialSuccess":{}}`, `{"partialSuccess":{"rejectedSpans":"0"}}`} {
		if err := parsePartialSuccess([]byte(reply)); err != nil {
			t.Fatalf("reply %q = %v, want a full success", reply, err)
		}
	}
}
