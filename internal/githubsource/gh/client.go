// Package gh is the production model.GHClient: one `gh api -i` subprocess
// per REST call, run as the dedicated account selected by GH_CONFIG_DIR.
//
// The client never trusts gh's exit code for the outcome. `gh api` exits 1
// for every status >= 400, and also when it fails before any reply. The
// status line and headers that -i prints decide the result:
//
//   - 2xx and 304: a GHResponse and a nil error. A 304 has an empty Body.
//   - any other status: the parsed GHResponse (headers, rate limit) and a
//     *model.GHError with that Status, the reply's message and RetryAfter.
//   - no usable reply (gh failed before printing a status line, the call
//     timed out, stdout passed the output cap, the head or body is not
//     parseable): a *model.GHError with Status 0. For a write this means the
//     request may or may not have reached GitHub.
//   - the caller's context ended: the context's error, wrapped.
package gh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.harness.dev/harness/internal/githubsource/model"
)

// Defaults and caps of one call.
const (
	DefaultCallTimeout = 30 * time.Second
	// DefaultMaxOutput is the stdout cap of one call (head and body).
	DefaultMaxOutput = 16 << 20
	// maxStderr keeps gh's diagnostics bounded; gh writes one short line.
	maxStderr = 64 << 10
	// maxMessage bounds the error text copied into a GHError.
	maxMessage = 1024
	// waitDelay bounds how long a killed gh may hold its output pipes.
	waitDelay = 2 * time.Second
	// secondaryRetryAfter is GitHub's documented minimum wait after a
	// secondary rate limit that names no Retry-After.
	secondaryRetryAfter = time.Minute
	apiVersion          = "2022-11-28"
)

// tokenVariables would override GH_CONFIG_DIR's credentials; they never
// reach a gh subprocess.
var tokenVariables = []string{"GITHUB_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}

// Options configures the client. Path and ConfigDir come from the operator
// file's account section.
type Options struct {
	// Path is the gh executable.
	Path string
	// ConfigDir is the dedicated account's GH_CONFIG_DIR. It is required:
	// an empty value would fall back to the OS user's own gh login.
	ConfigDir string
	// Env is added to the subprocess environment after the inherited one.
	// GH_* and GitHub token variables in it are dropped like inherited ones.
	Env []string
	// CallTimeout bounds one subprocess. Zero means DefaultCallTimeout.
	CallTimeout time.Duration
	// MaxOutput caps stdout bytes of one call. Zero means DefaultMaxOutput.
	MaxOutput int
	// Clock converts X-RateLimit-Reset into RetryAfter. Nil means wall time.
	Clock model.Clock
}

// Client runs gh api subprocesses. It is safe for concurrent use.
type Client struct {
	path      string
	configDir string
	env       []string
	timeout   time.Duration
	maxOutput int
	clock     model.Clock
}

var _ model.GHClient = (*Client)(nil)

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

// New returns a client for options.
func New(options Options) *Client {
	client := &Client{
		path:      options.Path,
		configDir: options.ConfigDir,
		timeout:   options.CallTimeout,
		maxOutput: options.MaxOutput,
		clock:     options.Clock,
	}
	if client.timeout <= 0 {
		client.timeout = DefaultCallTimeout
	}
	if client.maxOutput <= 0 {
		client.maxOutput = DefaultMaxOutput
	}
	if client.clock == nil {
		client.clock = wallClock{}
	}
	client.env = environment(os.Environ(), options.Env, options.ConfigDir)
	return client
}

// environment builds the subprocess environment: inherited and extra
// variables without any GH_* or GitHub token variable, then the fixed
// credential selection, which therefore always wins.
func environment(inherited, extra []string, configDir string) []string {
	env := make([]string, 0, len(inherited)+len(extra)+4)
	for _, list := range [][]string{inherited, extra} {
		for _, entry := range list {
			key, _, _ := strings.Cut(entry, "=")
			if strings.HasPrefix(key, "GH_") || slices.Contains(tokenVariables, key) || key == "GIT_TERMINAL_PROMPT" {
				continue
			}
			env = append(env, entry)
		}
	}
	return append(env,
		"GH_CONFIG_DIR="+configDir,
		"GH_PROMPT_DISABLED=1",
		"GH_NO_UPDATE_NOTIFIER=1",
		"GIT_TERMINAL_PROMPT=0",
	)
}

// Do runs one request. See the package documentation for the error
// mapping.
func (c *Client) Do(ctx context.Context, request model.GHRequest) (model.GHResponse, error) {
	args, err := c.arguments(request)
	if err != nil {
		return model.GHResponse{}, err
	}
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	stdout := &cappedBuffer{limit: c.maxOutput, full: cancel}
	stderr := &cappedBuffer{limit: maxStderr}
	command := exec.CommandContext(callCtx, c.path, args...)
	command.Env = c.env
	command.Stdout = stdout
	command.Stderr = stderr
	command.WaitDelay = waitDelay
	if len(request.Body) > 0 {
		command.Stdin = bytes.NewReader(request.Body)
	}
	runErr := command.Run()
	endpoint := args[len(args)-1]
	switch {
	case ctx.Err() != nil:
		return model.GHResponse{}, fmt.Errorf("gh api %s %s: %w", request.Method, endpoint, ctx.Err())
	case stdout.overflow:
		return model.GHResponse{}, &model.GHError{Message: fmt.Sprintf("%s %s: output exceeds %d bytes", request.Method, endpoint, c.maxOutput)}
	case callCtx.Err() != nil:
		return model.GHResponse{}, &model.GHError{Message: fmt.Sprintf("%s %s: timed out after %s", request.Method, endpoint, c.timeout)}
	}
	response, ok, parseErr := parseOutput(stdout.Bytes())
	if !ok {
		message := lastLine(stderr.Bytes())
		if message == "" && runErr != nil {
			message = runErr.Error()
		}
		if message == "" {
			message = "no reply"
		}
		if parseErr != nil {
			message = parseErr.Error()
		}
		return model.GHResponse{}, &model.GHError{Message: bound(fmt.Sprintf("%s %s: %s", request.Method, endpoint, message))}
	}
	c.decorate(&response)
	if response.Status == http.StatusNotModified || (response.Status >= 200 && response.Status < 300) {
		if len(response.Body) > 0 && !json.Valid(response.Body) {
			return model.GHResponse{}, &model.GHError{Message: fmt.Sprintf("%s %s: HTTP %d reply is not JSON", request.Method, endpoint, response.Status)}
		}
		if response.Status == http.StatusNotModified {
			response.Body = nil
		}
		return response, nil
	}
	return response, &model.GHError{
		Status:     response.Status,
		Message:    replyMessage(response.Body, response.Status),
		RetryAfter: response.RateLimit.RetryAfter,
	}
}

// arguments renders the gh api argument vector; the endpoint is last.
func (c *Client) arguments(request model.GHRequest) ([]string, error) {
	if c.path == "" {
		return nil, errors.New("gh: no executable configured")
	}
	if c.configDir == "" {
		return nil, errors.New("gh: no GH_CONFIG_DIR configured for the account")
	}
	method := request.Method
	if method == "" {
		method = http.MethodGet
	}
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
	default:
		return nil, fmt.Errorf("gh: unsupported method %q", request.Method)
	}
	endpoint, err := Endpoint(request.Path, request.Query)
	if err != nil {
		return nil, err
	}
	args := []string{"api", "--method", method, "-i",
		"-H", "Accept: application/vnd.github+json",
		"-H", "X-GitHub-Api-Version: " + apiVersion}
	for _, header := range []struct{ name, value string }{{"If-None-Match", request.ETag}, {"If-Modified-Since", request.LastModified}} {
		if header.value == "" {
			continue
		}
		if strings.ContainsAny(header.value, "\r\n") {
			return nil, fmt.Errorf("gh: %s value contains a line break", header.name)
		}
		args = append(args, "-H", header.name+": "+header.value)
	}
	if len(request.Body) > 0 {
		if !json.Valid(request.Body) {
			return nil, errors.New("gh: request body is not JSON")
		}
		args = append(args, "--input", "-")
	}
	return append(args, endpoint), nil
}

// Endpoint joins a request path and query into gh's endpoint argument. A
// path may already carry a query (a NextPath does); query values replace
// keys of the same name. Keys are sorted, so the argument is deterministic.
func Endpoint(path string, query url.Values) (string, error) {
	path = strings.TrimPrefix(path, "/")
	base, rawQuery, _ := strings.Cut(path, "?")
	if base == "" || strings.Contains(base, "://") || strings.ContainsAny(base, " \t\r\n#") {
		return "", fmt.Errorf("gh: invalid API path %q", path)
	}
	for _, segment := range strings.Split(base, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", fmt.Errorf("gh: invalid API path %q", path)
		}
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", fmt.Errorf("gh: invalid query in %q: %w", path, err)
	}
	for key, list := range query {
		values[key] = list
	}
	if len(values) == 0 {
		return base, nil
	}
	return base + "?" + values.Encode(), nil
}

// parseOutput splits `gh api -i` output into status, headers and body. ok
// is false when no status line was printed; err explains a malformed head.
func parseOutput(out []byte) (model.GHResponse, bool, error) {
	statusLine, rest, found := bytes.Cut(out, []byte("\n"))
	if !found || !bytes.HasPrefix(statusLine, []byte("HTTP/")) {
		return model.GHResponse{}, false, nil
	}
	fields := strings.Fields(string(bytes.TrimRight(statusLine, "\r")))
	if len(fields) < 2 {
		return model.GHResponse{}, false, fmt.Errorf("malformed status line %q", statusLine)
	}
	status, err := strconv.Atoi(fields[1])
	if err != nil || status < 100 || status > 599 {
		return model.GHResponse{}, false, fmt.Errorf("malformed status line %q", statusLine)
	}
	response := model.GHResponse{Status: status, Header: http.Header{}}
	for {
		line, next, found := bytes.Cut(rest, []byte("\n"))
		if !found {
			return model.GHResponse{}, false, errors.New("reply head has no terminating blank line")
		}
		rest = next
		line = bytes.TrimRight(line, "\r")
		if len(line) == 0 {
			break
		}
		name, value, ok := strings.Cut(string(line), ":")
		if !ok {
			return model.GHResponse{}, false, fmt.Errorf("malformed header line %q", line)
		}
		response.Header.Add(strings.TrimSpace(name), strings.TrimSpace(value))
	}
	if body := bytes.TrimSpace(rest); len(body) > 0 {
		response.Body = json.RawMessage(body)
	}
	return response, true, nil
}

// decorate fills the derived response fields from the headers.
func (c *Client) decorate(response *model.GHResponse) {
	header := response.Header
	response.ETag = header.Get("ETag")
	response.LastModified = header.Get("Last-Modified")
	response.NextPath = NextPath(header.Get("Link"))
	if seconds, err := strconv.Atoi(header.Get("X-Poll-Interval")); err == nil && seconds > 0 {
		response.PollInterval = time.Duration(seconds) * time.Second
	}
	rate := model.RateLimit{Limit: -1, Remaining: -1}
	if value, err := strconv.Atoi(header.Get("X-RateLimit-Limit")); err == nil {
		rate.Limit = value
	}
	if value, err := strconv.Atoi(header.Get("X-RateLimit-Remaining")); err == nil {
		rate.Remaining = value
	}
	if value, err := strconv.ParseInt(header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		rate.Reset = time.Unix(value, 0).UTC()
	}
	now := c.clock.Now()
	if value := header.Get("Retry-After"); value != "" {
		rate.RetryAfter = retryAfter(value, now)
	}
	limited := response.Status == http.StatusForbidden || response.Status == http.StatusTooManyRequests
	if limited && rate.RetryAfter == 0 && rate.Remaining == 0 && !rate.Reset.IsZero() {
		rate.RetryAfter = max(rate.Reset.Sub(now), time.Second)
	}
	if limited && rate.RetryAfter == 0 && (response.Status == http.StatusTooManyRequests || secondaryLimit(response.Body)) {
		rate.RetryAfter = secondaryRetryAfter
	}
	if rate.Limit < 0 || rate.Remaining < 0 {
		// Unknown budget: report zeros so callers never read a fake
		// "nothing remaining" from a reply without rate headers.
		rate.Limit, rate.Remaining = 0, 0
	}
	response.RateLimit = rate
}

// retryAfter reads Retry-After as delay seconds or an HTTP date.
func retryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
		return max(time.Duration(seconds)*time.Second, 0)
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(at.Sub(now), 0)
	}
	return 0
}

// secondaryLimit reports GitHub's secondary rate limit reply, which is a
// 403 that may carry no rate headers at all.
func secondaryLimit(body []byte) bool {
	return strings.Contains(strings.ToLower(replyMessage(body, 0)), "secondary rate limit")
}

// NextPath converts a Link header's rel="next" target to request path form
// ("repos/o/r/issues?page=2"), dropping the host and a GitHub Enterprise
// "api/v3/" prefix. Empty when there is no next page.
func NextPath(link string) string {
	for part := range strings.SplitSeq(link, ",") {
		target, params, found := strings.Cut(strings.TrimSpace(part), ";")
		if !found || !strings.HasPrefix(target, "<") || !strings.HasSuffix(target, ">") {
			continue
		}
		next := false
		for param := range strings.SplitSeq(params, ";") {
			name, value, _ := strings.Cut(strings.TrimSpace(param), "=")
			if strings.TrimSpace(name) == "rel" && slices.Contains(strings.Fields(strings.Trim(value, `"`)), "next") {
				next = true
			}
		}
		if !next {
			continue
		}
		parsed, err := url.Parse(target[1 : len(target)-1])
		if err != nil {
			return ""
		}
		path := strings.TrimPrefix(parsed.Path, "/")
		path = strings.TrimPrefix(path, "api/v3/")
		if parsed.RawQuery != "" {
			path += "?" + parsed.RawQuery
		}
		return path
	}
	return ""
}

// replyMessage extracts GitHub's error message, with the first validation
// error appended when present.
func replyMessage(body []byte, status int) string {
	var reply struct {
		Message string `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
			Code    string `json:"code"`
			Field   string `json:"field"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &reply) == nil && reply.Message != "" {
		message := reply.Message
		if len(reply.Errors) > 0 {
			first := reply.Errors[0]
			detail := first.Message
			if detail == "" {
				detail = strings.TrimSpace(first.Field + " " + first.Code)
			}
			if detail != "" {
				message += ": " + detail
			}
		}
		return bound(message)
	}
	if status == 0 {
		return ""
	}
	return http.StatusText(status)
}

func lastLine(stderr []byte) string {
	lines := strings.Split(strings.TrimSpace(string(stderr)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func bound(text string) string {
	if len(text) <= maxMessage {
		return text
	}
	cut := maxMessage
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "…"
}

// cappedBuffer keeps at most limit bytes. On overflow it discards the rest
// and calls full, which kills the subprocess.
type cappedBuffer struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
	full     func()
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.overflow {
		return len(p), nil
	}
	if room := b.limit - b.buf.Len(); len(p) > room {
		b.buf.Write(p[:room])
		b.overflow = true
		if b.full != nil {
			b.full()
		}
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *cappedBuffer) Bytes() []byte { return b.buf.Bytes() }
