package mcpoauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.harness.dev/harness/internal/mcpconfig"
)

// refreshMargin refreshes an access token this long before it expires.
const refreshMargin = time.Minute

// loginTimeout bounds one interactive authorization, browser time included.
const loginTimeout = 10 * time.Minute

// LoginRequired means the server needs a user to authorize this client, for
// the first time or for more scope (step-up). Requests that end in it were
// rejected before the server processed them.
type LoginRequired struct {
	Server    string
	Challenge Challenge
	StepUp    bool
}

func (e *LoginRequired) Error() string {
	need := "needs authorization"
	if e.StepUp {
		need = fmt.Sprintf("needs authorization for more scope (%q)", e.Challenge.Scope)
	}
	return fmt.Sprintf("mcp_auth: server %s %s; run `tempest mcp login %s`", e.Server, need, e.Server)
}

// Client authorizes one configured HTTP MCP server.
type Client struct {
	server   string
	url      string
	resource string
	headers  map[string]string
	static   *mcpconfig.OAuthClient
	store    *Store
	dest     *destinations
	http     *http.Client
	now      func() time.Time
}

// New binds a client to a normalized HTTP server configuration. Requests
// time out after 30s, never follow redirects, and reach origins other than
// the server's only under the destination policy in destinations.go.
func New(cfg mcpconfig.Server, store *Store) (*Client, error) {
	resource, err := canonical(cfg.URL)
	if err != nil {
		return nil, err
	}
	parsed, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, err
	}
	dest := newDestinations(cfg.Name, parsed, http.DefaultTransport.(*http.Transport).Clone())
	client := &http.Client{Timeout: 30 * time.Second, Transport: dest.transport(),
		// Credentials and codes must never follow a redirect to another endpoint.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Client{server: cfg.Name, url: cfg.URL, resource: resource, headers: cfg.Headers, static: cfg.OAuth, store: store, dest: dest, http: client, now: time.Now}, nil
}

func (c *Client) bound(r Record) bool { return r.Resource == c.resource }

func (c *Client) expiring(r Record) bool {
	return !r.ExpiresAt.IsZero() && c.now().Add(refreshMargin).After(r.ExpiresAt)
}

// Token returns the access token for the next request, refreshing one that
// is about to expire; while it has not expired, a failed refresh keeps it.
// It returns "" when no usable token is stored.
func (c *Client) Token(ctx context.Context) (string, error) {
	r, err := c.store.Load(c.server)
	if missing(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !c.bound(r) || r.AccessToken == "" {
		return "", nil
	}
	if !c.expiring(r) || r.RefreshToken == "" {
		return r.AccessToken, nil
	}
	unlock, err := c.store.Lock(ctx, c.server)
	if err != nil {
		return "", err
	}
	defer unlock()
	r, err = c.store.Load(c.server)
	if missing(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !c.bound(r) || r.AccessToken == "" {
		return "", nil
	}
	if !c.expiring(r) {
		return r.AccessToken, nil
	}
	refreshed, err := c.refreshLocked(ctx, &r)
	if err != nil && r.AccessToken != "" && c.now().Before(r.ExpiresAt) {
		// The refresh runs refreshMargin early; until the token really
		// expires, an unreachable or failing authorization server leaves
		// it usable and the MCP server judges it.
		return r.AccessToken, nil
	}
	if err != nil || !refreshed {
		return "", err
	}
	return r.AccessToken, nil
}

// Refresh replaces an access token the server rejected. It reports true when
// a different token is now stored, including one another process refreshed.
func (c *Client) Refresh(ctx context.Context, rejected string) (bool, error) {
	unlock, err := c.store.Lock(ctx, c.server)
	if err != nil {
		return false, err
	}
	defer unlock()
	r, err := c.store.Load(c.server)
	if missing(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !c.bound(r) {
		return false, nil
	}
	if r.AccessToken != "" && r.AccessToken != rejected {
		return true, nil
	}
	if r.RefreshToken == "" {
		return false, nil
	}
	return c.refreshLocked(ctx, &r)
}

// refreshLocked runs the refresh grant. A rotated refresh token replaces the
// old one; a rejected refresh token clears the tokens so login is required,
// and a rejected dynamic client (invalid_client) is dropped with them.
func (c *Client) refreshLocked(ctx context.Context, r *Record) (bool, error) {
	ctx = within(ctx, parseNetClass(r.ServerNetwork))
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {r.RefreshToken}}
	if r.TokenResource != "" {
		form.Set("resource", r.TokenResource)
	}
	basic := authenticate(form, r.ClientID, r.ClientSecret, r.TokenAuthMethod)
	var token tokenResponse
	status, err := c.postForm(ctx, r.TokenEndpoint, form, basic, &token)
	if status == 0 && err != nil {
		return false, fmt.Errorf("mcp_auth: server %s token refresh: %w", c.server, err)
	}
	if status == http.StatusBadRequest || status == http.StatusUnauthorized {
		r.AccessToken, r.RefreshToken, r.ExpiresAt = "", "", time.Time{}
		if token.Error == "invalid_client" && r.Registered {
			forgetClient(r)
		}
		return false, c.store.Save(*r)
	}
	if err != nil || status != http.StatusOK || token.Error != "" {
		return false, fmt.Errorf("mcp_auth: server %s token refresh failed with HTTP %d %s", c.server, status, token.Error)
	}
	if err := token.validate(); err != nil {
		return false, fmt.Errorf("mcp_auth: server %s token refresh: %w", c.server, err)
	}
	r.AccessToken = token.AccessToken
	if token.RefreshToken != "" {
		r.RefreshToken = token.RefreshToken
	}
	r.ExpiresAt = token.expiry(c.now())
	if token.Scope != "" {
		r.Scope = token.Scope
	}
	return true, c.store.Save(*r)
}

// NoteScope remembers a step-up scope no user could authorize, so the next
// `tempest mcp login` requests it.
func (c *Client) NoteScope(ctx context.Context, scope string) error {
	if strings.TrimSpace(scope) == "" {
		return nil
	}
	unlock, err := c.store.Lock(ctx, c.server)
	if err != nil {
		return err
	}
	defer unlock()
	r, err := c.store.Load(c.server)
	if err != nil && !missing(err) {
		return err
	}
	if missing(err) || !c.bound(r) {
		r = Record{Server: c.server, Resource: c.resource}
	}
	r.PendingScope = mergeScopes(r.PendingScope, scope)
	return c.store.Save(r)
}

// Probe sends an unauthenticated initialize request and returns the Bearer
// challenge of a 401 or 403 answer. Whatever the answer, the challenge
// carries the address it came from.
func (c *Client) Probe(ctx context.Context) (Challenge, bool, error) {
	body := []byte(`{"jsonrpc":"2.0","id":"probe","method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"tempest","version":"login"}}}`)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return Challenge{}, false, err
	}
	for name, value := range c.headers {
		request.Header.Set(name, value)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	var remote net.Addr
	request = request.WithContext(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { remote = info.Conn.RemoteAddr() }}))
	response, err := c.http.Do(request)
	if err != nil {
		return Challenge{}, false, err
	}
	response.Body.Close()
	peer := c.dest.peer(request, remote)
	if session := response.Header.Get("Mcp-Session-Id"); session != "" {
		// The probe opened a session the client will not use.
		closing, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.url, nil)
		if err == nil {
			closing.Header.Set("Mcp-Session-Id", session)
			if done, err := c.http.Do(closing); err == nil {
				done.Body.Close()
			}
		}
	}
	if response.StatusCode != http.StatusUnauthorized && response.StatusCode != http.StatusForbidden {
		return Challenge{Peer: peer}, false, nil
	}
	challenge, ok := ParseBearer(response.Header.Values("WWW-Authenticate"))
	challenge.Peer = peer
	return challenge, ok, nil
}

// Status is display metadata; it never contains tokens.
type Status struct {
	SignedIn     bool
	Refreshable  bool
	ExpiresAt    time.Time
	Scope        string
	PendingScope string
	Issuer       string
}

// Status reads the stored record without contacting any server.
func (c *Client) Status() (Status, error) {
	r, err := c.store.Load(c.server)
	if missing(err) {
		return Status{}, nil
	}
	if err != nil {
		return Status{}, err
	}
	if !c.bound(r) {
		return Status{}, nil
	}
	return Status{SignedIn: r.AccessToken != "", Refreshable: r.RefreshToken != "", ExpiresAt: r.ExpiresAt, Scope: r.Scope, PendingScope: r.PendingScope, Issuer: r.Issuer}, nil
}

// Logout forgets the server's tokens. A dynamic client registration stays
// so the next login reuses it.
func (c *Client) Logout(ctx context.Context) error {
	unlock, err := c.store.Lock(ctx, c.server)
	if err != nil {
		return err
	}
	defer unlock()
	r, err := c.store.Load(c.server)
	if missing(err) {
		return nil
	}
	if err != nil || !r.Registered {
		return c.store.Delete(c.server)
	}
	r.AccessToken, r.RefreshToken, r.ExpiresAt, r.Scope, r.PendingScope = "", "", time.Time{}, "", ""
	return c.store.Save(r)
}

// registration is the client identity used for one authorization.
type registration struct {
	clientID, secret, method string
	registered               bool
	redirect                 string
}

// Login runs the authorization code flow with PKCE. present shows the
// authorization URL to the user (browser, ACP URL elicitation or a printed
// URL); the code returns to a loopback listener on 127.0.0.1.
func (c *Client) Login(ctx context.Context, challenge Challenge, present func(context.Context, string) error) (Record, error) {
	ctx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()
	network := serverNetwork(challenge.Peer)
	ctx = within(ctx, network)
	resource, err := c.resourceMetadata(ctx, challenge)
	if err != nil {
		return Record{}, fmt.Errorf("mcp_auth: %w", err)
	}
	tokenResource, err := canonical(resource.Resource)
	if err != nil {
		return Record{}, fmt.Errorf("mcp_auth: %w", err)
	}
	var metadata ServerMetadata
	issuer := ""
	for _, candidate := range resource.AuthorizationServers {
		metadata, err = c.serverMetadata(ctx, candidate)
		if err == nil {
			issuer = candidate
			break
		}
	}
	if issuer == "" {
		return Record{}, fmt.Errorf("mcp_auth: %w", err)
	}
	previous, err := c.loadLocked(ctx)
	if err != nil {
		return Record{}, err
	}
	listener, client, err := c.prepareClient(ctx, metadata, issuer, previous)
	if err != nil {
		return Record{}, fmt.Errorf("mcp_auth: %w", err)
	}
	defer listener.Close()
	scope := c.scope(challenge, resource, previous, issuer)
	verifier, err := randomValue()
	if err != nil {
		return Record{}, err
	}
	state, err := randomValue()
	if err != nil {
		return Record{}, err
	}
	digest := sha256.Sum256([]byte(verifier))
	authorization, err := url.Parse(metadata.AuthorizationEndpoint)
	if err != nil {
		return Record{}, err
	}
	query := authorization.Query()
	query.Set("response_type", "code")
	query.Set("client_id", client.clientID)
	query.Set("redirect_uri", client.redirect)
	query.Set("state", state)
	query.Set("code_challenge", base64.RawURLEncoding.EncodeToString(digest[:]))
	query.Set("code_challenge_method", "S256")
	query.Set("resource", tokenResource)
	if scope != "" {
		query.Set("scope", scope)
	}
	authorization.RawQuery = query.Encode()
	// The first callback decides the login: a valid one carries the code,
	// a refused one ends the wait with its reason at once. Each answer is
	// written before Login can return and close the listener.
	type outcome struct {
		values url.Values
		err    error
	}
	callbacks := make(chan outcome, 1)
	var decided atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		refuse := func(reason string) {
			http.Error(w, reason, http.StatusBadRequest)
			_ = http.NewResponseController(w).Flush()
			if decided.CompareAndSwap(false, true) {
				callbacks <- outcome{err: fmt.Errorf("mcp_auth: authorization callback for server %s refused: %s", c.server, reason)}
			}
		}
		values := r.URL.Query()
		if len(values["state"]) != 1 || subtle.ConstantTimeCompare([]byte(values.Get("state")), []byte(state)) != 1 {
			refuse("Authorization state did not match")
			return
		}
		// RFC 9207: a returned issuer must be the one this request went to,
		// and a server that advertises the parameter must return it.
		iss := values.Get("iss")
		if iss == "" && metadata.IssParameterSupported {
			refuse("Authorization response lacks the issuer its server promised")
			return
		}
		if iss != "" && strings.TrimSuffix(iss, "/") != strings.TrimSuffix(metadata.Issuer, "/") {
			refuse("Authorization issuer did not match")
			return
		}
		if !decided.CompareAndSwap(false, true) {
			http.Error(w, "Authorization callback already received", http.StatusConflict)
			return
		}
		fmt.Fprintln(w, "Authorization received. Return to Tempest.")
		_ = http.NewResponseController(w).Flush()
		callbacks <- outcome{values: values}
	})
	callbackServer := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	defer callbackServer.Close()
	go func() { _ = callbackServer.Serve(listener) }()
	if err := present(ctx, authorization.String()); err != nil {
		return Record{}, err
	}
	var values url.Values
	select {
	case callback := <-callbacks:
		if callback.err != nil {
			return Record{}, callback.err
		}
		values = callback.values
	case <-ctx.Done():
		return Record{}, fmt.Errorf("mcp_auth: authorization for server %s was not completed: %w", c.server, ctx.Err())
	}
	if failure := values.Get("error"); failure != "" {
		return Record{}, fmt.Errorf("mcp_auth: authorization server refused server %s: %s %s", c.server, failure, values.Get("error_description"))
	}
	code := values.Get("code")
	if code == "" || len(values["code"]) != 1 {
		return Record{}, fmt.Errorf("mcp_auth: authorization callback for server %s carried no code", c.server)
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {client.redirect}, "code_verifier": {verifier}, "resource": {tokenResource}}
	basic := authenticate(form, client.clientID, client.secret, client.method)
	var token tokenResponse
	status, err := c.postForm(ctx, metadata.TokenEndpoint, form, basic, &token)
	if err != nil || status != http.StatusOK || token.Error != "" {
		failure := fmt.Errorf("mcp_auth: token exchange for server %s failed with HTTP %d %s %s%s", c.server, status, token.Error, token.ErrorDescription, errorSuffix(err))
		if token.Error == "invalid_client" && client.registered {
			return Record{}, errors.Join(failure, c.forgetRegistration(ctx, client.clientID))
		}
		return Record{}, failure
	}
	if err := token.validate(); err != nil {
		return Record{}, fmt.Errorf("mcp_auth: token exchange for server %s: %w", c.server, err)
	}
	granted := token.Scope
	if granted == "" {
		granted = scope
	}
	record := Record{Server: c.server, Resource: c.resource, TokenResource: tokenResource, Issuer: issuer, AuthorizationEndpoint: metadata.AuthorizationEndpoint, TokenEndpoint: metadata.TokenEndpoint,
		ClientID: client.clientID, ClientSecret: client.secret, TokenAuthMethod: client.method, Registered: client.registered, AccessToken: token.AccessToken, RefreshToken: token.RefreshToken,
		ExpiresAt: token.expiry(c.now()), Scope: granted, ServerNetwork: network.String()}
	if client.registered {
		record.RedirectURI = client.redirect
	}
	unlock, err := c.store.Lock(ctx, c.server)
	if err != nil {
		return Record{}, err
	}
	defer unlock()
	return record, c.store.Save(record)
}

func errorSuffix(err error) string {
	if err == nil {
		return ""
	}
	return ": " + err.Error()
}

// forgetClient drops a dynamic registration the authorization server no
// longer knows (invalid_client) with the tokens issued to it, so the next
// login registers again instead of sending the user to a dead client.
func forgetClient(r *Record) {
	r.AccessToken, r.RefreshToken, r.ExpiresAt = "", "", time.Time{}
	r.ClientID, r.ClientSecret, r.TokenAuthMethod, r.Registered, r.RedirectURI = "", "", "", false, ""
}

func (c *Client) forgetRegistration(ctx context.Context, clientID string) error {
	unlock, err := c.store.Lock(ctx, c.server)
	if err != nil {
		return err
	}
	defer unlock()
	r, err := c.store.Load(c.server)
	if missing(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !c.bound(r) || !r.Registered || r.ClientID != clientID {
		return nil
	}
	forgetClient(&r)
	return c.store.Save(r)
}

func (c *Client) loadLocked(ctx context.Context) (Record, error) {
	unlock, err := c.store.Lock(ctx, c.server)
	if err != nil {
		return Record{}, err
	}
	defer unlock()
	r, err := c.store.Load(c.server)
	if missing(err) {
		return Record{}, nil
	}
	if err != nil {
		return Record{}, err
	}
	if !c.bound(r) {
		return Record{}, nil
	}
	return r, nil
}

// prepareClient opens the loopback listener and picks the client identity:
// the configured client, a stored dynamic registration whose redirect port
// is free, or a new dynamic registration.
func (c *Client) prepareClient(ctx context.Context, metadata ServerMetadata, issuer string, previous Record) (net.Listener, registration, error) {
	if c.static != nil && c.static.ClientID != "" {
		listener, redirect, err := loopback("127.0.0.1:0")
		if err != nil {
			return nil, registration{}, err
		}
		return listener, registration{clientID: c.static.ClientID, secret: c.static.ClientSecret, method: staticMethod(metadata, c.static.ClientSecret), redirect: redirect}, nil
	}
	if previous.Registered && previous.Issuer == issuer && previous.ClientID != "" {
		if prior, err := url.Parse(previous.RedirectURI); err == nil && prior.Hostname() == "127.0.0.1" && prior.Port() != "" {
			if listener, redirect, err := loopback(prior.Host); err == nil && redirect == previous.RedirectURI {
				return listener, registration{clientID: previous.ClientID, secret: previous.ClientSecret, method: previous.TokenAuthMethod, registered: true, redirect: redirect}, nil
			} else if err == nil {
				listener.Close()
			}
		}
	}
	if metadata.RegistrationEndpoint == "" {
		return nil, registration{}, fmt.Errorf("authorization server %s offers no dynamic client registration; set \"oauth\": {\"clientId\": \"...\"} on server %s", issuer, c.server)
	}
	listener, redirect, err := loopback("127.0.0.1:0")
	if err != nil {
		return nil, registration{}, err
	}
	client, err := c.register(ctx, metadata, redirect)
	if err != nil {
		listener.Close()
		return nil, registration{}, err
	}
	if err := c.keepRegistration(ctx, metadata, issuer, client); err != nil {
		listener.Close()
		return nil, registration{}, err
	}
	return listener, client, nil
}

// keepRegistration stores a new dynamic registration before the user is
// asked, so a login nobody finishes (declined, unattended, abandoned in the
// browser) does not register yet another client at the authorization server
// next time. A record that holds tokens keeps its client until a login
// replaces both: its refresh token belongs to that client.
func (c *Client) keepRegistration(ctx context.Context, metadata ServerMetadata, issuer string, client registration) error {
	unlock, err := c.store.Lock(ctx, c.server)
	if err != nil {
		return err
	}
	defer unlock()
	r, err := c.store.Load(c.server)
	if err != nil && !missing(err) {
		return err
	}
	if missing(err) || !c.bound(r) {
		r = Record{Server: c.server, Resource: c.resource}
	}
	if r.AccessToken != "" || r.RefreshToken != "" {
		return nil
	}
	if r.Issuer != issuer {
		r.Scope = ""
	}
	r.Issuer, r.AuthorizationEndpoint, r.TokenEndpoint = issuer, metadata.AuthorizationEndpoint, metadata.TokenEndpoint
	r.ClientID, r.ClientSecret, r.TokenAuthMethod, r.Registered, r.RedirectURI = client.clientID, client.secret, client.method, true, client.redirect
	return c.store.Save(r)
}

func loopback(address string) (net.Listener, string, error) {
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		return nil, "", fmt.Errorf("cannot start the OAuth loopback callback: %w", err)
	}
	return listener, "http://" + listener.Addr().String() + "/callback", nil
}

// registrationRequest is the RFC 7591 client metadata of a public client.
type registrationRequest struct {
	ClientName      string   `json:"client_name"`
	RedirectURIs    []string `json:"redirect_uris"`
	GrantTypes      []string `json:"grant_types"`
	ResponseTypes   []string `json:"response_types"`
	TokenAuthMethod string   `json:"token_endpoint_auth_method"`
}

// register performs RFC 7591 dynamic registration as a public client.
func (c *Client) register(ctx context.Context, metadata ServerMetadata, redirect string) (registration, error) {
	body, err := json.Marshal(registrationRequest{ClientName: "Tempest", RedirectURIs: []string{redirect}, GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"}, TokenAuthMethod: "none"})
	if err != nil {
		return registration{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, metadata.RegistrationEndpoint, bytes.NewReader(body))
	if err != nil {
		return registration{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	var registered struct {
		ClientID        string `json:"client_id"`
		ClientSecret    string `json:"client_secret"`
		TokenAuthMethod string `json:"token_endpoint_auth_method"`
		Error           string `json:"error"`
		Description     string `json:"error_description"`
	}
	status, err := c.doJSON(request, &registered)
	if err != nil || (status != http.StatusCreated && status != http.StatusOK) || registered.ClientID == "" {
		return registration{}, fmt.Errorf("dynamic client registration at %s failed with HTTP %d %s %s%s", metadata.RegistrationEndpoint, status, registered.Error, registered.Description, errorSuffix(err))
	}
	method := registered.TokenAuthMethod
	if method == "" {
		method = "none"
		if registered.ClientSecret != "" {
			method = "client_secret_basic"
		}
	}
	return registration{clientID: registered.ClientID, secret: registered.ClientSecret, method: method, registered: true, redirect: redirect}, nil
}

func staticMethod(metadata ServerMetadata, secret string) string {
	if secret == "" {
		return "none"
	}
	supported := metadata.TokenEndpointAuthMethodsSupported
	if len(supported) == 0 || slices.Contains(supported, "client_secret_basic") || !slices.Contains(supported, "client_secret_post") {
		return "client_secret_basic"
	}
	return "client_secret_post"
}

// authenticate applies the client's token endpoint authentication method and
// returns Basic credentials when that method uses them.
func authenticate(form url.Values, clientID, secret, method string) [2]string {
	switch method {
	case "client_secret_basic":
		return [2]string{clientID, secret}
	case "client_secret_post":
		form.Set("client_id", clientID)
		form.Set("client_secret", secret)
	default:
		form.Set("client_id", clientID)
	}
	return [2]string{}
}

// scope follows the specification's selection order: the challenged scope,
// then configured scopes, then scopes_supported. Previously granted and
// pending step-up scopes are kept so a step-up never loses permissions.
func (c *Client) scope(challenge Challenge, resource ResourceMetadata, previous Record, issuer string) string {
	base := resource.ScopesSupported
	switch {
	case challenge.Scope != "":
		base = strings.Fields(challenge.Scope)
	case c.static != nil && len(c.static.Scopes) > 0:
		base = c.static.Scopes
	}
	merged := strings.Join(base, " ")
	if previous.Issuer == issuer {
		merged = mergeScopes(merged, previous.Scope)
	}
	return mergeScopes(merged, previous.PendingScope)
}

func mergeScopes(first, second string) string {
	seen := map[string]bool{}
	var out []string
	for _, scope := range append(strings.Fields(first), strings.Fields(second)...) {
		if !seen[scope] {
			seen[scope] = true
			out = append(out, scope)
		}
	}
	return strings.Join(out, " ")
}

type tokenResponse struct {
	AccessToken      string          `json:"access_token"`
	TokenType        string          `json:"token_type"`
	RefreshToken     string          `json:"refresh_token"`
	Scope            string          `json:"scope"`
	ExpiresIn        json.RawMessage `json:"expires_in"`
	Error            string          `json:"error"`
	ErrorDescription string          `json:"error_description"`
}

func (t tokenResponse) validate() error {
	if t.AccessToken == "" {
		return errors.New("token response has no access_token")
	}
	if !strings.EqualFold(t.TokenType, "Bearer") {
		return fmt.Errorf("token type %q is not Bearer", t.TokenType)
	}
	return nil
}

// maxLifetime bounds a stated token lifetime; a longer one, which a
// time.Duration may not even hold, is treated as unknown.
const maxLifetime = 100 * 365 * 24 * time.Hour

// expiry converts expires_in, a number or a quoted number of seconds; none,
// or one that is not a positive number of at most maxLifetime, means the
// token's lifetime is unknown and it is used until the server rejects it.
func (t tokenResponse) expiry(now time.Time) time.Time {
	raw := strings.Trim(string(bytes.TrimSpace(t.ExpiresIn)), `"`)
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil || !(seconds > 0 && seconds <= maxLifetime.Seconds()) {
		return time.Time{}
	}
	return now.Add(time.Duration(seconds * float64(time.Second)))
}

func randomValue() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
