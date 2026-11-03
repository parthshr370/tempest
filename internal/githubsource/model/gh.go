package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// GHClient performs one GitHub REST request as the dedicated account.
//
// The production client runs `<ghPath> api --method M -i [-H ...]
// [--input -] <path>` with GH_CONFIG_DIR set to the account's directory,
// GH_PROMPT_DISABLED=1 and no inherited GitHub token variables. It parses
// the status line and headers that `-i` prints before the body and trusts
// that status over gh's exit code, which is 1 for every status >= 400.
// One call is bounded by Config.Poll.CallTimeout and a 16 MiB stdout cap.
type GHClient interface {
	Do(ctx context.Context, request GHRequest) (GHResponse, error)
}

// GHRequest is one REST call relative to the API root.
type GHRequest struct {
	Method string `json:"method"`
	// Path has no host and no leading slash, e.g.
	// "repos/o/r/issues/42/comments".
	Path  string     `json:"path"`
	Query url.Values `json:"query,omitempty"`
	// Body is sent on stdin with --input - when non-empty.
	Body json.RawMessage `json:"body,omitempty"`
	// ETag and LastModified become If-None-Match and If-Modified-Since.
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"lastModified,omitempty"`
}

// GHResponse is a parsed reply. Status 304 has an empty Body.
type GHResponse struct {
	Status int             `json:"status"`
	Header http.Header     `json:"header"`
	Body   json.RawMessage `json:"body,omitempty"`
	// NextPath is the Link rel="next" target in GHRequest.Path form.
	NextPath     string        `json:"nextPath,omitempty"`
	ETag         string        `json:"etag,omitempty"`
	LastModified string        `json:"lastModified,omitempty"`
	PollInterval time.Duration `json:"pollInterval,omitempty"`
	RateLimit    RateLimit     `json:"rateLimit"`
}

// RateLimit is the account's REST budget as last reported.
type RateLimit struct {
	Limit     int       `json:"limit"`
	Remaining int       `json:"remaining"`
	Reset     time.Time `json:"reset,omitzero"`
	// RetryAfter is set by a secondary rate limit or abuse response.
	RetryAfter time.Duration `json:"retryAfter,omitempty"`
}

// GHError is a non-2xx, non-304 reply, or a gh failure with no reply. The
// client sets RetryAfter from Retry-After, or from X-RateLimit-Reset when a
// 403 or 429 reports no remaining requests.
type GHError struct {
	// Status is zero when gh failed before printing a status line.
	Status     int           `json:"status"`
	Message    string        `json:"message"`
	RetryAfter time.Duration `json:"retryAfter,omitempty"`
}

func (e *GHError) Error() string {
	if e.Status == 0 {
		return "gh: " + e.Message
	}
	return fmt.Sprintf("gh: %s (HTTP %d)", e.Message, e.Status)
}

// Transient reports whether the same request may succeed later: rate
// limits, server errors, and 404, which can mean lost private access. 410
// (gone) and other client errors are permanent for that resource.
func (e *GHError) Transient() bool {
	switch {
	case e.Status == 0, e.Status == http.StatusNotFound, e.Status == http.StatusTooManyRequests:
		return true
	case e.Status == http.StatusForbidden:
		return e.RetryAfter > 0
	default:
		return e.Status >= http.StatusInternalServerError
	}
}
