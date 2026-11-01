package mcpclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"

	"go.harness.dev/harness/internal/mcpconfig"
	"go.harness.dev/harness/internal/mcpoauth"
	"go.harness.dev/harness/internal/mcpwire"
	"go.harness.dev/harness/internal/question"
)

// serverAuth answers one HTTP server's bearer challenges. It refreshes
// tokens without a user; authorization that needs a user returns
// mcpoauth.LoginRequired to the caller, which may run server.authorize.
type serverAuth struct {
	name string
	flow *mcpoauth.Client
	// login serializes interactive authorizations of this server.
	login sync.Mutex
	// generation counts completed logins so a caller that waited behind
	// another login does not ask the user a second time.
	generation atomic.Uint64
	// mu guards current and renewed.
	mu sync.Mutex
	// current is the token last handed out or obtained by a login, "" when
	// the last Token found none or failed.
	current string
	// renewed is closed, and replaced, when current becomes a new token.
	renewed chan struct{}
}

func newServerAuth(cfg mcpconfig.Server, store *mcpoauth.Store) (*serverAuth, error) {
	flow, err := mcpoauth.New(cfg, store)
	if err != nil {
		return nil, fmt.Errorf("mcp_config: server %s: %w", cfg.Name, err)
	}
	return &serverAuth{name: cfg.Name, flow: flow, renewed: make(chan struct{})}, nil
}

// Token sees every token the server gets, including one another process
// stored, so Renewed learns of each refresh and login.
func (a *serverAuth) Token(ctx context.Context) (string, error) {
	token, err := a.flow.Token(ctx)
	a.observe(token)
	return token, err
}

func (a *serverAuth) observe(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if token != "" && token != a.current {
		close(a.renewed)
		a.renewed = make(chan struct{})
	}
	a.current = token
}

// Renewed implements mcpwire.Auth.
func (a *serverAuth) Renewed(refused string) <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.current != "" && a.current != refused {
		renewed := make(chan struct{})
		close(renewed)
		return renewed
	}
	return a.renewed
}

// Rejected refreshes a rejected token once; otherwise a 401 needs a login
// and a 403 insufficient_scope needs a step-up login (MCP authorization,
// "Scope Challenge Handling").
func (a *serverAuth) Rejected(ctx context.Context, rejection mcpwire.Rejection) error {
	challenge, _ := mcpoauth.ParseBearer(rejection.Challenges)
	challenge.Peer = rejection.Peer
	if rejection.Status == http.StatusForbidden {
		if challenge.Error == "insufficient_scope" {
			return &mcpoauth.LoginRequired{Server: a.name, Challenge: challenge, StepUp: true}
		}
		return fmt.Errorf("mcp_auth: server %s forbade the request (HTTP 403 %s)", a.name, challenge.Error)
	}
	if rejection.Token != "" && !rejection.Retried {
		refreshed, err := a.flow.Refresh(ctx, rejection.Token)
		if err != nil {
			return err
		}
		if refreshed {
			return nil
		}
	}
	return &mcpoauth.LoginRequired{Server: a.name, Challenge: challenge}
}

// handlers wires this server's callbacks into a new connection.
func (s *server) handlers() mcpwire.Handlers {
	handlers := mcpwire.Handlers{Notify: s.notification, Request: s.request, AdmitProcess: s.manager.admitProcess, Elicitation: true, Sampling: s.config.Sampling && s.manager.sample != nil}
	if s.auth != nil {
		handlers.Auth = s.auth
	}
	return handlers
}

// authorize runs the interactive login a LoginRequired asked for. seen is
// the login generation the failed request used: a login that finished in
// the meantime is reused instead of asking again.
func (s *server) authorize(ctx context.Context, required *mcpoauth.LoginRequired, seen uint64) error {
	auth := s.auth
	auth.login.Lock()
	defer auth.login.Unlock()
	if auth.generation.Load() != seen {
		return nil
	}
	if s.manager.elicit == nil {
		return s.loginUnavailable(ctx, required)
	}
	present := func(ctx context.Context, authorizationURL string) error {
		id, err := randomID()
		if err != nil {
			return err
		}
		reply, err := s.manager.elicit(ctx, question.Elicit{Server: s.config.Name, Mode: "url", Message: "sign in to let Tempest use this server.", URL: authorizationURL, ElicitationID: id})
		if err != nil {
			return err
		}
		if reply.Action != "accept" {
			return fmt.Errorf("mcp_auth: authorization of server %s was not accepted (%s); run `tempest mcp login %s` to sign in later", s.config.Name, reply.Action, s.config.Name)
		}
		return nil
	}
	record, err := auth.flow.Login(ctx, required.Challenge, present)
	if err != nil {
		if errors.Is(err, question.Unavailable) {
			return s.loginUnavailable(ctx, required)
		}
		return err
	}
	auth.generation.Add(1)
	auth.observe(record.AccessToken)
	return nil
}

// loginUnavailable keeps a step-up scope for the next `tempest mcp login`
// and returns the error that names that command.
func (s *server) loginUnavailable(ctx context.Context, required *mcpoauth.LoginRequired) error {
	if required.StepUp {
		if err := s.auth.flow.NoteScope(ctx, required.Challenge.Scope); err != nil {
			s.manager.logger.Warn("recording MCP step-up scope failed", "server", s.config.Name, "error", err)
		}
	}
	return required
}

// randomID returns 128 random bits in hex: elicitation and sampling ids.
func randomID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
