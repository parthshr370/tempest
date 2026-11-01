package mcpwire

import "context"

// Auth supplies OAuth bearer tokens to one HTTP server. It must not ask a
// user anything: interactive authorization belongs to the caller, which sees
// the returned error.
type Auth interface {
	// Token returns the bearer token for the next request, or "" for none.
	Token(ctx context.Context) (string, error)
	// Rejected decides what follows a 401 or 403. A nil error retries the
	// request once with a fresh Token.
	Rejected(ctx context.Context, rejection Rejection) error
}

// Rejection describes one 401 or 403 answer.
type Rejection struct {
	Status int
	// Challenges are the WWW-Authenticate header values.
	Challenges []string
	// Token is the bearer the rejected request carried, "" for none.
	Token string
	// Retried marks the rejection of the single retry; no further retry follows.
	Retried bool
}

// AuthError is an authorization failure. The server rejected the request
// before processing it, so sending it again after authorization is safe.
type AuthError struct{ Err error }

func (e *AuthError) Error() string { return e.Err.Error() }

// Unwrap exposes the cause, such as a login-required error from Auth.
func (e *AuthError) Unwrap() error { return e.Err }
