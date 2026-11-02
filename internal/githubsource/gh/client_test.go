package gh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/githubsource/ghfake"
	"go.harness.dev/harness/internal/githubsource/model"
)

func TestMain(m *testing.M) {
	ghfake.MainIfFake()
	os.Exit(m.Run())
}

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

var testNow = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func newClient(t *testing.T, server *ghfake.Server, options Options) *Client {
	t.Helper()
	options.Path = ghfake.Executable()
	options.ConfigDir = t.TempDir()
	options.Env = append(options.Env, server.Env()...)
	options.Clock = fixedClock{testNow}
	return New(options)
}

func ghError(t *testing.T, err error) *model.GHError {
	t.Helper()
	var target *model.GHError
	if !errors.As(err, &target) {
		t.Fatalf("error %v (%T) is not a *model.GHError", err, err)
	}
	return target
}

func TestDoParsesHeadersPaginationAndRateLimit(t *testing.T) {
	const pageSize = 2
	const resetUnix = 1793620800
	server := ghfake.NewServer(t)
	rows := []int{1, 2, 3}
	server.HandleFunc("GET /repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `W/"e1"`)
		w.Header().Set("Last-Modified", "Mon, 02 Nov 2026 11:00:00 GMT")
		w.Header().Set("X-Poll-Interval", "90")
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(resetUnix))
		ghfake.WritePage(w, r, rows)
	})
	client := newClient(t, server, Options{})
	response, err := client.Do(context.Background(), model.GHRequest{Path: "repos/o/r/issues", Query: url.Values{"per_page": {fmt.Sprint(pageSize)}, "state": {"all"}}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != http.StatusOK || string(response.Body) != "[1,2]" {
		t.Fatalf("response = %d %s", response.Status, response.Body)
	}
	if response.ETag != `W/"e1"` || response.LastModified != "Mon, 02 Nov 2026 11:00:00 GMT" || response.PollInterval != 90*time.Second {
		t.Fatalf("validators = %q %q %s", response.ETag, response.LastModified, response.PollInterval)
	}
	wantRate := model.RateLimit{Limit: 5000, Remaining: 4999, Reset: time.Unix(resetUnix, 0).UTC()}
	if response.RateLimit != wantRate {
		t.Fatalf("rate = %+v, want %+v", response.RateLimit, wantRate)
	}
	if response.NextPath != "repos/o/r/issues?page=2&per_page=2&state=all" {
		t.Fatalf("next = %q", response.NextPath)
	}
	next, err := client.Do(context.Background(), model.GHRequest{Path: response.NextPath})
	if err != nil {
		t.Fatal(err)
	}
	if string(next.Body) != "[3]" || next.NextPath != "" {
		t.Fatalf("page 2 = %s next %q", next.Body, next.NextPath)
	}
	requests := server.Requests()
	first := requests[0]
	if first.Header.Get("Accept") != "application/vnd.github+json" || first.Header.Get("X-GitHub-Api-Version") != apiVersion || first.Query.Get("state") != "all" {
		t.Fatalf("first request = %+v", first)
	}
}

func TestDoConditionalRequestReturnsNotModified(t *testing.T) {
	server := ghfake.NewServer(t)
	server.HandleFunc("GET /notifications", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` && r.Header.Get("If-Modified-Since") == "Mon, 02 Nov 2026 11:00:00 GMT" {
			w.Header().Set("ETag", `"v1"`)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		ghfake.WriteJSON(w, http.StatusOK, []int{})
	})
	client := newClient(t, server, Options{})
	response, err := client.Do(context.Background(), model.GHRequest{Path: "notifications", ETag: `"v1"`, LastModified: "Mon, 02 Nov 2026 11:00:00 GMT"})
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != http.StatusNotModified || response.Body != nil || response.ETag != `"v1"` {
		t.Fatalf("response = %+v", response)
	}
	if _, err := client.Do(context.Background(), model.GHRequest{Path: "notifications", ETag: "bad\r\nX-Injected: 1"}); err == nil || !strings.Contains(err.Error(), "line break") {
		t.Fatalf("header injection err = %v", err)
	}
}

func TestDoSendsBodyOnStdin(t *testing.T) {
	server := ghfake.NewServer(t)
	server.HandleFunc("POST /repos/o/r/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		ghfake.WriteJSON(w, http.StatusCreated, map[string]string{"echo": string(body)})
	})
	client := newClient(t, server, Options{})
	response, err := client.Do(context.Background(), model.GHRequest{Method: http.MethodPost, Path: "repos/o/r/issues/7/comments", Body: json.RawMessage(`{"body":"hi"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var reply struct{ Echo string }
	if err := json.Unmarshal(response.Body, &reply); err != nil || reply.Echo != `{"body":"hi"}` || response.Status != http.StatusCreated {
		t.Fatalf("reply = %d %s (%v)", response.Status, response.Body, err)
	}
}

func TestDoMapsErrorStatuses(t *testing.T) {
	const resetAfter = 45 * time.Second
	server := ghfake.NewServer(t)
	server.HandleFunc("GET /secondary", func(w http.ResponseWriter, r *http.Request) {
		ghfake.WriteJSON(w, http.StatusForbidden, map[string]string{"message": "You have exceeded a secondary rate limit"})
	})
	server.HandleFunc("GET /exhausted", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(testNow.Add(resetAfter).Unix()))
		ghfake.WriteJSON(w, http.StatusForbidden, map[string]string{"message": "API rate limit exceeded"})
	})
	server.HandleFunc("GET /denied", func(w http.ResponseWriter, r *http.Request) {
		ghfake.WriteJSON(w, http.StatusForbidden, map[string]string{"message": "Resource not accessible by integration"})
	})
	server.HandleFunc("GET /invalid", func(w http.ResponseWriter, r *http.Request) {
		type fieldError struct {
			Field string `json:"field"`
			Code  string `json:"code"`
		}
		type validationReply struct {
			Message string       `json:"message"`
			Errors  []fieldError `json:"errors"`
		}
		ghfake.WriteJSON(w, http.StatusUnprocessableEntity, validationReply{Message: "Validation Failed", Errors: []fieldError{{Field: "head", Code: "invalid"}}})
	})
	server.Fail(http.MethodGet, "/limited", http.StatusTooManyRequests, 1, http.Header{"Retry-After": {"7"}})
	server.Fail(http.MethodGet, "/gone", http.StatusGone, 1, nil)
	server.Fail(http.MethodGet, "/broken", http.StatusBadGateway, 1, nil)
	client := newClient(t, server, Options{})
	cases := []struct {
		path       string
		status     int
		message    string
		retryAfter time.Duration
		transient  bool
	}{
		{"limited", http.StatusTooManyRequests, "Too Many Requests", 7 * time.Second, true},
		{"secondary", http.StatusForbidden, "You have exceeded a secondary rate limit", secondaryRetryAfter, true},
		{"exhausted", http.StatusForbidden, "API rate limit exceeded", resetAfter, true},
		{"denied", http.StatusForbidden, "Resource not accessible by integration", 0, false},
		{"missing", http.StatusNotFound, "Not Found", 0, true},
		{"gone", http.StatusGone, "Gone", 0, false},
		{"invalid", http.StatusUnprocessableEntity, "Validation Failed: head invalid", 0, false},
		{"broken", http.StatusBadGateway, "Bad Gateway", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			response, err := client.Do(context.Background(), model.GHRequest{Path: tc.path})
			got := ghError(t, err)
			if got.Status != tc.status || got.Message != tc.message || got.RetryAfter != tc.retryAfter || got.Transient() != tc.transient {
				t.Fatalf("error = %+v transient %v", got, got.Transient())
			}
			if response.Status != tc.status {
				t.Fatalf("response status = %d", response.Status)
			}
		})
	}
}

// script writes an executable shell script standing in for gh.
func script(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDoRunsWithAccountEnvironmentOnly(t *testing.T) {
	dump := filepath.Join(t.TempDir(), "env")
	path := script(t, `env > "$DUMP"; printf 'HTTP/2.0 200 OK\n\r\n{}'`)
	t.Setenv("GH_TOKEN", "inherited-token")
	t.Setenv("GITHUB_TOKEN", "inherited-token")
	t.Setenv("GH_CONFIG_DIR", "/personal/gh")
	t.Setenv("GH_HOST", "example.com")
	configDir := t.TempDir()
	client := New(Options{Path: path, ConfigDir: configDir, Env: []string{"DUMP=" + dump, "GH_ENTERPRISE_TOKEN=extra", "GIT_TERMINAL_PROMPT=1"}})
	if _, err := client.Do(context.Background(), model.GHRequest{Path: "user"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for line := range strings.SplitSeq(string(raw), "\n") {
		key, value, _ := strings.Cut(line, "=")
		seen[key] = value
	}
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GH_HOST"} {
		if _, found := seen[key]; found {
			t.Errorf("%s reached gh", key)
		}
	}
	want := map[string]string{"GH_CONFIG_DIR": configDir, "GH_PROMPT_DISABLED": "1", "GH_NO_UPDATE_NOTIFIER": "1", "GIT_TERMINAL_PROMPT": "0", "DUMP": dump}
	for key, value := range want {
		if seen[key] != value {
			t.Errorf("%s = %q, want %q", key, seen[key], value)
		}
	}
	if _, err := New(Options{Path: path}).Do(context.Background(), model.GHRequest{Path: "user"}); err == nil || !strings.Contains(err.Error(), "GH_CONFIG_DIR") {
		t.Fatalf("missing config dir err = %v", err)
	}
}

func TestDoWithoutReplyIsStatusZero(t *testing.T) {
	const callTimeout = 200 * time.Millisecond
	const outputCap = 512
	failing := script(t, `echo "error connecting to api.github.com" >&2; exit 1`)
	client := New(Options{Path: failing, ConfigDir: t.TempDir()})
	_, err := client.Do(context.Background(), model.GHRequest{Path: "user"})
	if got := ghError(t, err); got.Status != 0 || !strings.Contains(got.Message, "error connecting to api.github.com") || !got.Transient() {
		t.Fatalf("no reply err = %+v", got)
	}

	slow := script(t, `exec sleep 5`)
	client = New(Options{Path: slow, ConfigDir: t.TempDir(), CallTimeout: callTimeout})
	started := time.Now()
	_, err = client.Do(context.Background(), model.GHRequest{Path: "user"})
	if got := ghError(t, err); got.Status != 0 || !strings.Contains(got.Message, "timed out after 200ms") {
		t.Fatalf("timeout err = %+v", got)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = client.Do(ctx, model.GHRequest{Path: "user"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled err = %v", err)
	}

	server := ghfake.NewServer(t)
	server.HandleFunc("GET /big", func(w http.ResponseWriter, r *http.Request) {
		ghfake.WriteJSON(w, http.StatusOK, strings.Repeat("x", outputCap*4))
	})
	client = newClient(t, server, Options{MaxOutput: outputCap})
	_, err = client.Do(context.Background(), model.GHRequest{Path: "big"})
	if got := ghError(t, err); got.Status != 0 || !strings.Contains(got.Message, fmt.Sprintf("output exceeds %d bytes", outputCap)) {
		t.Fatalf("cap err = %+v", got)
	}

	garbage := script(t, `printf 'HTTP/2.0 200 OK\n\r\nnot json'`)
	client = New(Options{Path: garbage, ConfigDir: t.TempDir()})
	if _, err = client.Do(context.Background(), model.GHRequest{Path: "user"}); ghError(t, err).Status != 0 {
		t.Fatalf("non-JSON body err = %v", err)
	}
}

func TestEndpointAndNextPath(t *testing.T) {
	endpoint, err := Endpoint("/repos/o/r/issues?page=2&since=a", url.Values{"since": {"2026-11-02T00:00:00Z"}, "per_page": {"100"}})
	if err != nil || endpoint != "repos/o/r/issues?page=2&per_page=100&since=2026-11-02T00%3A00%3A00Z" {
		t.Fatalf("endpoint = %q, %v", endpoint, err)
	}
	for _, bad := range []string{"", "https://api.github.com/user", "repos/../user", "repos//o", "repos/o r"} {
		if _, err := Endpoint(bad, nil); err == nil {
			t.Errorf("Endpoint(%q) accepted", bad)
		}
	}
	links := map[string]string{
		`<https://api.github.com/repositories/1/issues?page=3>; rel="next", <https://api.github.com/repositories/1/issues?page=9>; rel="last"`: "repositories/1/issues?page=3",
		`<https://ghe.example.com/api/v3/repos/o/r/pulls?page=2>; rel="next"`:                                                              "repos/o/r/pulls?page=2",
		`<https://api.github.com/x?page=1>; rel="prev"`:                                                                                    "",
		"": "",
	}
	for link, want := range links {
		if got := NextPath(link); got != want {
			t.Errorf("NextPath(%q) = %q, want %q", link, got, want)
		}
	}
}
