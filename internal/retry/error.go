package retry

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"go.harness.dev/harness/internal/engine/types"
)

// Code identifies a stable retry outcome.
type Code string

const (
	CodeRateLimited        Code = "rate_limited"
	CodeServerFailure      Code = "server_failure"
	CodeNetworkFailure     Code = "network_failure"
	CodeAttemptTimeout     Code = "attempt_timeout"
	CodeStreamInterrupted  Code = "stream_interrupted"
	CodeContextOverflow    Code = "context_overflow"
	CodeCanceled           Code = "canceled"
	CodeAuthentication     Code = "authentication"
	CodeInvalidRequest     Code = "invalid_request"
	CodeProviderFailure    Code = "provider_failure"
	CodeAttemptsExhausted  Code = "attempts_exhausted"
	CodeRetryDelayExceeded Code = "retry_delay_exceeded"
)

var (
	// ErrRetryable matches intrinsic transient provider failures.
	ErrRetryable = errors.New("retryable provider failure")
	// ErrTerminal matches non-retryable provider failures.
	ErrTerminal = errors.New("terminal provider failure")
	// ErrExhausted matches retry chains that used all attempts.
	ErrExhausted = errors.New("retry attempts exhausted")
	// ErrCanceled matches retry chains stopped by context cancellation.
	ErrCanceled = errors.New("retry canceled")
)

// Error carries the classified provider outcome without exposing request data.
type Error struct {
	Code       Code
	Message    string
	Retryable  bool
	RetryAfter time.Duration
	Attempts   int
	Cause      error
}

// Error returns safe, stable error text.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("provider failure: %s", e.Code)
}

// Unwrap returns the original provider failure when present.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// Is matches retry sentinels and nonzero typed error codes.
func (e *Error) Is(target error) bool {
	if e == nil {
		return false
	}
	switch target {
	case ErrRetryable:
		return e.Retryable
	case ErrTerminal:
		return !e.Retryable
	case ErrExhausted:
		return e.Code == CodeAttemptsExhausted
	case ErrCanceled:
		return e.Code == CodeCanceled
	}
	other, ok := target.(*Error)
	return ok && other.Code != "" && e.Code == other.Code
}

// Failure is the terminal attempt metadata consumed by [Classify].
type Failure struct {
	Message      string
	ProviderCode string
	StopReason   types.StopReason
	Status       int
	Headers      map[string]string
	ContextErr   error
}

// Classify maps provider terminal metadata to a stable retry category.
func Classify(f Failure, now time.Time) *Error {
	message := strings.TrimSpace(f.Message)
	providerCode := strings.TrimSpace(f.ProviderCode)
	causeText := message
	if providerCode != "" {
		if causeText != "" {
			causeText = providerCode + ": " + causeText
		} else {
			causeText = providerCode
		}
	}
	cause := error(nil)
	if causeText != "" {
		cause = errors.New(causeText)
	}
	result := func(code Code, retryable bool) *Error {
		return &Error{Code: code, Message: message, Retryable: retryable, RetryAfter: retryAfterOrZero(f.Headers, now), Cause: cause}
	}
	if f.ContextErr != nil || f.StopReason == types.StopAborted {
		return result(CodeCanceled, false)
	}
	combined := strings.ToLower(providerCode + " " + message)
	if isContextOverflow(combined) {
		return result(CodeContextOverflow, false)
	}
	if strings.HasPrefix(providerCode, "subscription_sharing_") && (f.Status == 429 || providerCode == "subscription_sharing_usage_limit_exceeded") {
		return result(Code(providerCode), false)
	}
	if f.Status == 429 {
		return result(CodeRateLimited, true)
	}
	if f.Status >= 500 && f.Status <= 599 {
		return result(CodeServerFailure, true)
	}
	// Only an error status classifies the failure. With a 2xx status the
	// provider accepted the request and the stream failed afterwards (a stall,
	// a dropped connection, an in-stream error event), so the code and message
	// below classify it.
	if f.Status >= 400 {
		switch f.Status {
		case 401, 403:
			return result(CodeAuthentication, false)
		case 400, 404, 409, 422:
			return result(CodeInvalidRequest, false)
		default:
			if f.Status >= 400 && f.Status <= 499 {
				return result(CodeInvalidRequest, false)
			}
			return result(CodeProviderFailure, false)
		}
	}
	if providerCode != "" {
		if code, retryable, ok := classifyProviderCode(providerCode); ok {
			return result(code, retryable)
		}
	}
	if containsAny(combined, "i/o timeout", "tls handshake timeout", "context deadline exceeded", "timed out") {
		return result(CodeAttemptTimeout, true)
	}
	if containsAny(combined, "connection reset", "connection refused", "connection closed", "broken pipe", "unexpected eof", "server closed idle connection", "socket hang up", "stream idle", "stream ended without finish_reason", "fetch failed") || strings.TrimSpace(strings.ToLower(message)) == "eof" || (strings.Contains(combined, "upstream") && strings.Contains(combined, "reset")) {
		if strings.Contains(combined, "stream") {
			return result(CodeStreamInterrupted, true)
		}
		return result(CodeNetworkFailure, true)
	}
	if containsAny(combined, "overloaded", "service unavailable", "server/internal error") {
		return result(CodeServerFailure, true)
	}
	if containsAny(combined, "too many requests", "rate limit", "retry your request") {
		return result(CodeRateLimited, true)
	}
	if strings.Contains(combined, "provider returned error") {
		return result(CodeServerFailure, true)
	}
	return result(CodeProviderFailure, false)
}

// classifyProviderCode maps a structured provider error code to a stable
// category. Per §3.5 a recognized code participates before message-compatibility
// patterns, so broad message wording cannot override it. Codes this package
// assigned (providers record them through Classify) map back to themselves;
// "canceled" does not, because only the caller's context decides cancellation.
func classifyProviderCode(code string) (Code, bool, bool) {
	switch strings.ReplaceAll(strings.ToLower(strings.TrimSpace(code)), "-", "_") {
	case "rate_limit_error", "rate_limited", "too_many_requests":
		return CodeRateLimited, true, true
	case "overloaded_error", "overloaded", "server_error", "internal_server_error", "service_unavailable", string(CodeServerFailure):
		return CodeServerFailure, true, true
	case string(CodeStreamInterrupted):
		return CodeStreamInterrupted, true, true
	case string(CodeNetworkFailure):
		return CodeNetworkFailure, true, true
	case string(CodeAttemptTimeout):
		return CodeAttemptTimeout, true, true
	case "authentication_error", "permission_error", "permission_denied", "invalid_api_key", string(CodeAuthentication):
		return CodeAuthentication, false, true
	case "invalid_request_error", "not_found_error", string(CodeInvalidRequest):
		return CodeInvalidRequest, false, true
	default:
		return "", false, false
	}
}

func retryAfterOrZero(headers map[string]string, now time.Time) time.Duration {
	delay, ok := RetryAfter(headers, now)
	if !ok {
		return 0
	}
	return delay
}

// isContextOverflow reports whether provider details name an input-limit failure, which must win before generic retryable failures.
func isContextOverflow(value string) bool {
	return containsAny(value, "context_length_exceeded", "prompt_too_long", "maximum context length", "too many input tokens") || (strings.Contains(value, "context window") && strings.Contains(value, "exceeded"))
}

func containsAny(value string, patterns ...string) bool {
	for _, pattern := range patterns {
		if strings.Contains(value, pattern) {
			return true
		}
	}
	return false
}
