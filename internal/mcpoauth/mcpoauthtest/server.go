// Package mcpoauthtest runs an in-process OAuth 2.1 authorization server and
// a Streamable HTTP MCP server that accepts only its tokens. Tests use it to
// exercise MCP authorization without a network or accounts.
package mcpoauthtest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Options shapes the fixture's metadata and token policy.
type Options struct {
	// TokenLifetime is expires_in for access tokens; zero means one hour.
	TokenLifetime time.Duration
	// NoPKCE omits S256 from the authorization server metadata.
	NoPKCE bool
	// NoChallengeMetadata sends 401s without resource_metadata, so clients
	// must probe the well-known locations.
	NoChallengeMetadata bool
	// IssuerPath gives the issuer a path, whose metadata then lives only at
	// the OpenID Connect path-inserted location.
	IssuerPath string
	// StaticClient is accepted without registration; Register stays open.
	StaticClient string
}

// Server is one authorization server and its protected MCP endpoint. Tool
// "read" needs scope mcp:read and tool "write" needs mcp:write.
type Server struct {
	Options Options
	AS      *httptest.Server
	MCP     *httptest.Server
	// URL is the MCP endpoint and Issuer the authorization server.
	URL    string
	Issuer string

	mu             sync.Mutex
	clients        map[string][]string
	codes          map[string]grant
	access         map[string]issued
	refresh        map[string]issued
	registrations  int
	authorizations []url.Values
	tokenRequests  []url.Values
	verified       int
	calls          []string
}

type grant struct {
	clientID, redirect, challenge, scope, resource string
}

type issued struct {
	clientID, scope, resource string
	expires                   time.Time
}

// New starts both servers; they stop with the test.
func New(t testing.TB, options Options) *Server {
	t.Helper()
	s := &Server{Options: options, clients: map[string][]string{}, codes: map[string]grant{}, access: map[string]issued{}, refresh: map[string]issued{}}
	if options.TokenLifetime == 0 {
		s.Options.TokenLifetime = time.Hour
	}
	s.AS = httptest.NewServer(http.HandlerFunc(s.authorization))
	s.MCP = httptest.NewServer(http.HandlerFunc(s.resource))
	t.Cleanup(s.AS.Close)
	t.Cleanup(s.MCP.Close)
	s.Issuer = s.AS.URL + options.IssuerPath
	s.URL = s.MCP.URL + "/mcp"
	return s
}

// Browse plays the user's browser: it opens the authorization URL, which
// this fixture approves at once, and follows the redirect to the client's
// loopback callback.
func (s *Server) Browse(ctx context.Context, authorizationURL string) error {
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, authorizationURL, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusFound {
		return fmt.Errorf("authorization refused: %d %s", response.StatusCode, body)
	}
	callback, err := http.NewRequestWithContext(ctx, http.MethodGet, response.Header.Get("Location"), nil)
	if err != nil {
		return err
	}
	answer, err := client.Do(callback)
	if err != nil {
		return err
	}
	answer.Body.Close()
	if answer.StatusCode != http.StatusOK {
		return fmt.Errorf("callback answered %d", answer.StatusCode)
	}
	return nil
}

// Registrations counts dynamic client registrations.
func (s *Server) Registrations() int { s.mu.Lock(); defer s.mu.Unlock(); return s.registrations }

// Authorizations returns the authorization request parameters seen.
func (s *Server) Authorizations() []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.authorizations)
}

// TokenRequests returns the token endpoint form parameters seen.
func (s *Server) TokenRequests() []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.tokenRequests)
}

// VerifiedCodes counts code exchanges whose PKCE verifier matched.
func (s *Server) VerifiedCodes() int { s.mu.Lock(); defer s.mu.Unlock(); return s.verified }

// Calls lists the tools the MCP endpoint executed.
func (s *Server) Calls() []string { s.mu.Lock(); defer s.mu.Unlock(); return slices.Clone(s.calls) }

// RevokeRefreshTokens makes every refresh token invalid.
func (s *Server) RevokeRefreshTokens() { s.mu.Lock(); s.refresh = map[string]issued{}; s.mu.Unlock() }

// ExpireAccessTokens makes every access token invalid.
func (s *Server) ExpireAccessTokens() { s.mu.Lock(); s.access = map[string]issued{}; s.mu.Unlock() }

func (s *Server) authorization(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/.well-known/oauth-authorization-server" + s.Options.IssuerPath, "/.well-known/openid-configuration" + s.Options.IssuerPath:
		if s.Options.IssuerPath != "" && strings.HasPrefix(r.URL.Path, "/.well-known/oauth-authorization-server") {
			http.NotFound(w, r)
			return
		}
		methods := []string{"S256"}
		if s.Options.NoPKCE {
			methods = nil
		}
		writeJSON(w, http.StatusOK, map[string]any{"issuer": s.Issuer, "authorization_endpoint": s.AS.URL + "/authorize", "token_endpoint": s.AS.URL + "/token", "registration_endpoint": s.AS.URL + "/register", "code_challenge_methods_supported": methods, "scopes_supported": []string{"mcp:read", "mcp:write"}})
	case "/register":
		var request struct {
			RedirectURIs []string `json:"redirect_uris"`
			AuthMethod   string   `json:"token_endpoint_auth_method"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || len(request.RedirectURIs) == 0 || request.AuthMethod != "none" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_client_metadata"})
			return
		}
		id := random("client")
		s.mu.Lock()
		s.clients[id] = request.RedirectURIs
		s.registrations++
		s.mu.Unlock()
		writeJSON(w, http.StatusCreated, map[string]any{"client_id": id, "redirect_uris": request.RedirectURIs, "token_endpoint_auth_method": "none"})
	case "/authorize":
		s.authorize(w, r)
	case "/token":
		s.token(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	s.mu.Lock()
	s.authorizations = append(s.authorizations, query)
	redirects, known := s.clients[query.Get("client_id")]
	s.mu.Unlock()
	redirect := query.Get("redirect_uri")
	static := s.Options.StaticClient != "" && query.Get("client_id") == s.Options.StaticClient && strings.HasPrefix(redirect, "http://127.0.0.1:")
	switch {
	case !known && !static:
		http.Error(w, "unknown client", http.StatusBadRequest)
	case !static && !slices.Contains(redirects, redirect):
		http.Error(w, "unregistered redirect_uri", http.StatusBadRequest)
	case query.Get("response_type") != "code" || query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") == "" || query.Get("state") == "":
		http.Error(w, "PKCE S256, state and response_type=code are required", http.StatusBadRequest)
	case query.Get("resource") != s.URL:
		http.Error(w, "resource must name the MCP server", http.StatusBadRequest)
	default:
		code := random("code")
		s.mu.Lock()
		s.codes[code] = grant{clientID: query.Get("client_id"), redirect: redirect, challenge: query.Get("code_challenge"), scope: query.Get("scope"), resource: query.Get("resource")}
		s.mu.Unlock()
		target, _ := url.Parse(redirect)
		values := target.Query()
		values.Set("code", code)
		values.Set("state", query.Get("state"))
		values.Set("iss", s.Issuer)
		target.RawQuery = values.Encode()
		http.Redirect(w, r, target.String(), http.StatusFound)
	}
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if r.ParseForm() != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	form := r.PostForm
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenRequests = append(s.tokenRequests, form)
	invalid := func() { writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"}) }
	if form.Get("resource") != s.URL {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_target"})
		return
	}
	var subject issued
	switch form.Get("grant_type") {
	case "authorization_code":
		code, ok := s.codes[form.Get("code")]
		delete(s.codes, form.Get("code"))
		digest := sha256.Sum256([]byte(form.Get("code_verifier")))
		if !ok || code.clientID != form.Get("client_id") || code.redirect != form.Get("redirect_uri") || code.resource != form.Get("resource") || base64.RawURLEncoding.EncodeToString(digest[:]) != code.challenge {
			invalid()
			return
		}
		s.verified++
		subject = issued{clientID: code.clientID, scope: code.scope, resource: code.resource}
	case "refresh_token":
		prior, ok := s.refresh[form.Get("refresh_token")]
		// Rotation: a refresh token is good for one use.
		delete(s.refresh, form.Get("refresh_token"))
		if !ok || prior.clientID != form.Get("client_id") {
			invalid()
			return
		}
		subject = prior
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
		return
	}
	access, refresh := random("access"), random("refresh")
	subject.expires = time.Now().Add(s.Options.TokenLifetime)
	s.access[access] = subject
	s.refresh[refresh] = subject
	writeJSON(w, http.StatusOK, map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": int(s.Options.TokenLifetime / time.Second), "refresh_token": refresh, "scope": subject.scope})
}

func (s *Server) resource(w http.ResponseWriter, r *http.Request) {
	metadata := s.MCP.URL + "/.well-known/oauth-protected-resource/mcp"
	if r.URL.Path == "/.well-known/oauth-protected-resource/mcp" {
		writeJSON(w, http.StatusOK, map[string]any{"resource": s.URL, "authorization_servers": []string{s.Issuer}, "scopes_supported": []string{"mcp:read"}})
		return
	}
	if r.URL.Path != "/mcp" {
		http.NotFound(w, r)
		return
	}
	challenge := func(status int, parameters string) {
		header := "Bearer " + parameters
		if !s.Options.NoChallengeMetadata {
			header += fmt.Sprintf(`, resource_metadata="%s"`, metadata)
		}
		w.Header().Set("WWW-Authenticate", header)
		w.WriteHeader(status)
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	grant, ok := s.access[token]
	s.mu.Unlock()
	if !ok || time.Now().After(grant.expires) || grant.resource != s.URL || !hasScope(grant.scope, "mcp:read") {
		challenge(http.StatusUnauthorized, `error="invalid_token", scope="mcp:read"`)
		return
	}
	switch r.Method {
	case http.MethodGet:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	case http.MethodDelete:
		w.WriteHeader(http.StatusOK)
		return
	}
	var message struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if json.NewDecoder(r.Body).Decode(&message) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if len(message.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch message.Method {
	case "initialize":
		w.Header().Set("Mcp-Session-Id", "oauth-session")
		result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "oauth-fixture", "version": "1"}}
	case "tools/list":
		schema := json.RawMessage(`{"type":"object"}`)
		result = map[string]any{"tools": []map[string]any{{"name": "read", "inputSchema": schema}, {"name": "write", "inputSchema": schema}}}
	case "tools/call":
		if message.Params.Name == "write" && !hasScope(grant.scope, "mcp:write") {
			challenge(http.StatusForbidden, `error="insufficient_scope", scope="mcp:read mcp:write"`)
			return
		}
		s.mu.Lock()
		s.calls = append(s.calls, message.Params.Name)
		s.mu.Unlock()
		result = map[string]any{"content": []map[string]string{{"type": "text", "text": "did " + message.Params.Name}}}
	default:
		writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": message.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": result})
}

func hasScope(granted, want string) bool { return slices.Contains(strings.Fields(granted), want) }

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func random(prefix string) string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(errors.New("random source failed"))
	}
	return prefix + "-" + hex.EncodeToString(raw[:])
}
