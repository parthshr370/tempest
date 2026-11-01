package mcpoauth_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.harness.dev/harness/internal/mcpconfig"
	"go.harness.dev/harness/internal/mcpoauth"
	"go.harness.dev/harness/internal/mcpoauth/mcpoauthtest"
)

func TestParseBearerFindsChallengeAmongSchemes(t *testing.T) {
	values := []string{`Basic realm="x"`, `Newauth realm="apps", type=1, Bearer error="insufficient_scope", scope="files:read files:write", resource_metadata="https://mcp.example/.well-known/oauth-protected-resource", error_description="needs \"write\""`}
	challenge, ok := mcpoauth.ParseBearer(values)
	if !ok {
		t.Fatal("no Bearer challenge found")
	}
	want := mcpoauth.Challenge{ResourceMetadata: "https://mcp.example/.well-known/oauth-protected-resource", Scope: "files:read files:write", Error: "insufficient_scope", Description: `needs "write"`}
	if challenge != want {
		t.Fatalf("challenge = %#v, want %#v", challenge, want)
	}
	if _, ok := mcpoauth.ParseBearer([]string{`Basic realm="x"`}); ok {
		t.Fatal("Basic-only header parsed as Bearer")
	}
}

type harness struct {
	fixture *mcpoauthtest.Server
	store   *mcpoauth.Store
	dir     string
	flow    *mcpoauth.Client
}

func newHarness(t *testing.T, options mcpoauthtest.Options, client *mcpconfig.OAuthClient) *harness {
	t.Helper()
	fixture := mcpoauthtest.New(t, options)
	dir := filepath.Join(t.TempDir(), "agent", "mcp-oauth")
	store := mcpoauth.NewStore(dir)
	servers, err := mcpconfig.Normalize([]mcpconfig.Server{{Name: "secure", Transport: "http", URL: fixture.URL, OAuth: client}})
	if err != nil {
		t.Fatal(err)
	}
	flow, err := mcpoauth.New(servers[0], store)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{fixture: fixture, store: store, dir: dir, flow: flow}
}

// remotePeer stands for a server that answered from a public address, so
// its loopback fixtures count as someone else's internal services.
var remotePeer = netip.MustParseAddr("93.184.216.34")

func (h *harness) login(t *testing.T) mcpoauth.Record {
	t.Helper()
	challenge, challenged, err := h.flow.Probe(t.Context())
	if err != nil || !challenged {
		t.Fatalf("probe: challenged=%v err=%v", challenged, err)
	}
	if !challenge.Peer.IsLoopback() {
		t.Fatalf("probe reports the challenge came from %v, want the fixture's loopback address", challenge.Peer)
	}
	record, err := h.flow.Login(t.Context(), challenge, h.fixture.Browse)
	if err != nil {
		t.Fatal(err)
	}
	if record.ServerNetwork != "loopback" {
		t.Fatalf("record keeps server network %q, want loopback for refresh", record.ServerNetwork)
	}
	return record
}

func TestLoginDiscoversRegistersWithPKCEAndStoresOwnerOnly(t *testing.T) {
	h := newHarness(t, mcpoauthtest.Options{}, nil)
	record := h.login(t)
	if h.fixture.Registrations() != 1 || h.fixture.VerifiedCodes() != 1 {
		t.Fatalf("registrations=%d verified=%d", h.fixture.Registrations(), h.fixture.VerifiedCodes())
	}
	authorization := h.fixture.Authorizations()[0]
	if authorization.Get("resource") != h.fixture.URL || authorization.Get("scope") != "mcp:read" || authorization.Get("code_challenge_method") != "S256" || !strings.HasPrefix(authorization.Get("redirect_uri"), "http://127.0.0.1:") {
		t.Fatalf("authorization request = %v", authorization)
	}
	if token := h.fixture.TokenRequests()[0]; token.Get("resource") != h.fixture.URL || token.Get("code_verifier") == "" {
		t.Fatalf("token request = %v", token)
	}
	if !record.Registered || record.Issuer != h.fixture.Issuer || record.AccessToken == "" || record.RefreshToken == "" {
		t.Fatalf("record = %#v", record)
	}
	info, err := os.Stat(filepath.Join(h.dir, "secure.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode: %v %v", info, err)
	}
	if info, err := os.Stat(h.dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("store directory mode: %v %v", info, err)
	}
	token, err := h.flow.Token(t.Context())
	if err != nil || token != record.AccessToken {
		t.Fatalf("token = %q %v", token, err)
	}
}

func TestLoginProbesWellKnownMetadataInSpecifiedOrder(t *testing.T) {
	h := newHarness(t, mcpoauthtest.Options{NoChallengeMetadata: true, IssuerPath: "/tenant"}, nil)
	record := h.login(t)
	if record.Issuer != h.fixture.Issuer || record.TokenEndpoint != h.fixture.AS.URL+"/token" {
		t.Fatalf("record = %#v", record)
	}
}

func TestLoginRefusesAuthorizationServerWithoutPKCE(t *testing.T) {
	h := newHarness(t, mcpoauthtest.Options{NoPKCE: true}, nil)
	challenge, _, err := h.flow.Probe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.flow.Login(t.Context(), challenge, h.fixture.Browse)
	if err == nil || !strings.Contains(err.Error(), "S256") {
		t.Fatalf("login without PKCE support: %v", err)
	}
	if len(h.fixture.Authorizations()) != 0 {
		t.Fatal("authorization request sent despite missing PKCE support")
	}
}

// RFC 9207 section 2.4: an authorization server that advertises the iss
// response parameter must send it, so a response without it is refused.
func TestLoginRefusesResponseWithoutAdvertisedIssuer(t *testing.T) {
	h := newHarness(t, mcpoauthtest.Options{OmitIss: true}, nil)
	challenge, _, err := h.flow.Probe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := h.flow.Login(ctx, challenge, h.fixture.Browse); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("login with the advertised iss missing = %v, want the callback refused", err)
	}
	if h.fixture.VerifiedCodes() != 0 {
		t.Fatal("the code was exchanged although the response lacked iss")
	}
}

// A refused callback ends the login at once with its reason, also when
// present returned before the browser came back, as a terminal does once
// the user pressed Enter; the user does not wait out the login timeout.
func TestRefusedCallbackEndsTheLogin(t *testing.T) {
	for name, test := range map[string]struct {
		options mcpoauthtest.Options
		tamper  func(url.Values)
		want    string
	}{
		"missing iss": {options: mcpoauthtest.Options{OmitIss: true}, want: "lacks the issuer"},
		"wrong state": {tamper: func(values url.Values) { values.Set("state", "forged") }, want: "state did not match"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, test.options, nil)
			challenge, _, err := h.flow.Probe(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			browsed := make(chan struct{})
			present := func(ctx context.Context, authorizationURL string) error {
				go func() { defer close(browsed); _ = browseWith(ctx, authorizationURL, test.tamper) }()
				return nil
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			_, err = h.flow.Login(ctx, challenge, present)
			<-browsed
			if err == nil || ctx.Err() != nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("login after a refused callback = %v (deadline: %v), want it ended with %q", err, ctx.Err(), test.want)
			}
			if h.fixture.VerifiedCodes() != 0 {
				t.Fatal("a refused callback's code was exchanged")
			}
		})
	}
}

// browseWith approves the authorization URL like the fixture's Browse and
// lets tamper change the callback's query before the browser follows it.
func browseWith(ctx context.Context, authorizationURL string, tamper func(url.Values)) error {
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, authorizationURL, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	response.Body.Close()
	callback, err := url.Parse(response.Header.Get("Location"))
	if err != nil {
		return err
	}
	if tamper != nil {
		query := callback.Query()
		tamper(query)
		callback.RawQuery = query.Encode()
	}
	request, err = http.NewRequestWithContext(ctx, http.MethodGet, callback.String(), nil)
	if err != nil {
		return err
	}
	answer, err := client.Do(request)
	if err != nil {
		return err
	}
	return answer.Body.Close()
}

func TestLoginUsesConfiguredClientWithoutRegistering(t *testing.T) {
	h := newHarness(t, mcpoauthtest.Options{StaticClient: "preregistered"}, &mcpconfig.OAuthClient{ClientID: "preregistered"})
	record := h.login(t)
	if h.fixture.Registrations() != 0 || record.ClientID != "preregistered" || record.Registered {
		t.Fatalf("registrations=%d record=%#v", h.fixture.Registrations(), record)
	}
}

func TestSecondLoginReusesDynamicRegistration(t *testing.T) {
	h := newHarness(t, mcpoauthtest.Options{}, nil)
	first := h.login(t)
	if err := h.flow.Logout(t.Context()); err != nil {
		t.Fatal(err)
	}
	if token, err := h.flow.Token(t.Context()); err != nil || token != "" {
		t.Fatalf("token after logout = %q %v", token, err)
	}
	second := h.login(t)
	if h.fixture.Registrations() != 1 || second.ClientID != first.ClientID {
		t.Fatalf("registrations=%d first=%s second=%s", h.fixture.Registrations(), first.ClientID, second.ClientID)
	}
}

// A login the user never finishes (declined, no user, closed browser) keeps
// its registration, so the next attempt does not register another client.
func TestAbandonedLoginKeepsItsDynamicRegistration(t *testing.T) {
	h := newHarness(t, mcpoauthtest.Options{}, nil)
	challenge, _, err := h.flow.Probe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	declined := errors.New("declined")
	for range 2 {
		if _, err := h.flow.Login(t.Context(), challenge, func(context.Context, string) error { return declined }); !errors.Is(err, declined) {
			t.Fatalf("abandoned login = %v", err)
		}
	}
	if status, err := h.flow.Status(); err != nil || status.SignedIn {
		t.Fatalf("status after abandoned logins = %#v %v", status, err)
	}
	h.login(t)
	if h.fixture.Registrations() != 1 {
		t.Fatalf("registrations = %d after two abandoned logins and one finished, want 1", h.fixture.Registrations())
	}
}

// A step-up whose stored redirect port is taken registers a new client, but
// an abandoned step-up must not swap it in under the stored refresh token,
// which belongs to the old client.
func TestAbandonedStepUpKeepsTheSignedInClient(t *testing.T) {
	h := newHarness(t, mcpoauthtest.Options{TokenLifetime: 30 * time.Second}, nil)
	first := h.login(t)
	redirect, err := url.Parse(first.RedirectURI)
	if err != nil {
		t.Fatal(err)
	}
	busy, err := net.Listen("tcp4", redirect.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	challenge, _, err := h.flow.Probe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	declined := errors.New("declined")
	if _, err := h.flow.Login(t.Context(), challenge, func(context.Context, string) error { return declined }); !errors.Is(err, declined) {
		t.Fatalf("abandoned step-up = %v", err)
	}
	stored, err := h.store.Load("secure")
	if err != nil || stored.ClientID != first.ClientID || stored.RefreshToken != first.RefreshToken {
		t.Fatalf("stored client %q after abandoned step-up, want %q (%v)", stored.ClientID, first.ClientID, err)
	}
	if token, err := h.flow.Token(t.Context()); err != nil || token == "" || token == first.AccessToken {
		t.Fatalf("refresh with the signed-in client = %q %v", token, err)
	}
}

// An authorization server that forgot the dynamic client answers the
// refresh with invalid_client. The registration is dropped with the tokens,
// so the next login registers again instead of reusing the dead client.
func TestForgottenClientIsRegisteredAgainAfterRefresh(t *testing.T) {
	h := newHarness(t, mcpoauthtest.Options{TokenLifetime: 30 * time.Second}, nil)
	first := h.login(t)
	h.fixture.ForgetClients()
	if token, err := h.flow.Token(t.Context()); err != nil || token != "" {
		t.Fatalf("token after the server forgot the client = %q %v", token, err)
	}
	second := h.login(t)
	if h.fixture.Registrations() != 2 || second.ClientID == first.ClientID {
		t.Fatalf("registrations=%d first=%s second=%s", h.fixture.Registrations(), first.ClientID, second.ClientID)
	}
}

// The same holds when the code exchange is what answers invalid_client.
func TestForgottenClientIsRegisteredAgainAfterCodeExchange(t *testing.T) {
	h := newHarness(t, mcpoauthtest.Options{}, nil)
	challenge, _, err := h.flow.Probe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	forget := func(ctx context.Context, authorizationURL string) error {
		err := h.fixture.Browse(ctx, authorizationURL)
		h.fixture.ForgetClients()
		return err
	}
	if _, err := h.flow.Login(t.Context(), challenge, forget); err == nil || !strings.Contains(err.Error(), "invalid_client") {
		t.Fatalf("exchange with a forgotten client = %v", err)
	}
	h.login(t)
	if h.fixture.Registrations() != 2 {
		t.Fatalf("registrations = %d, want a new one after invalid_client", h.fixture.Registrations())
	}
}

func TestRefreshRotatesTokensAndRejectionRequiresLogin(t *testing.T) {
	// A 30s lifetime is inside the refresh margin, so every Token call refreshes.
	h := newHarness(t, mcpoauthtest.Options{TokenLifetime: 30 * time.Second}, nil)
	first := h.login(t)
	second, err := h.flow.Token(t.Context())
	if err != nil || second == "" || second == first.AccessToken {
		t.Fatalf("refreshed token = %q %v", second, err)
	}
	stored, err := h.store.Load("secure")
	if err != nil || stored.RefreshToken == first.RefreshToken {
		t.Fatalf("refresh token not rotated: %v", err)
	}
	third, err := h.flow.Token(t.Context())
	if err != nil || third == "" || third == second {
		t.Fatalf("second rotation = %q %v", third, err)
	}
	refreshes := 0
	for _, request := range h.fixture.TokenRequests() {
		if request.Get("grant_type") == "refresh_token" {
			refreshes++
			if request.Get("resource") != h.fixture.URL {
				t.Fatalf("refresh without resource: %v", request)
			}
		}
	}
	if refreshes != 2 {
		t.Fatalf("refresh requests = %d", refreshes)
	}
	h.fixture.RevokeRefreshTokens()
	token, err := h.flow.Token(t.Context())
	if err != nil || token != "" {
		t.Fatalf("token after revoked refresh = %q %v", token, err)
	}
	if status, err := h.flow.Status(); err != nil || status.SignedIn {
		t.Fatalf("status after revoked refresh = %#v %v", status, err)
	}
}

// Refreshing a minute early must not turn an unreachable authorization
// server into an outage while the access token is still valid.
func TestUnreachableRefreshKeepsTheUnexpiredToken(t *testing.T) {
	h := newHarness(t, mcpoauthtest.Options{TokenLifetime: 30 * time.Second}, nil)
	record := h.login(t)
	h.fixture.AS.Close()
	token, err := h.flow.Token(t.Context())
	if err != nil || token != record.AccessToken {
		t.Fatalf("token while the authorization server is down = %q %v, want the unexpired token", token, err)
	}
}

func TestRefreshReusesTokenAnotherProcessRotated(t *testing.T) {
	h := newHarness(t, mcpoauthtest.Options{}, nil)
	record := h.login(t)
	refreshed, err := h.flow.Refresh(t.Context(), "an-older-token")
	if err != nil || !refreshed {
		t.Fatalf("refresh with a newer stored token = %v %v", refreshed, err)
	}
	for _, request := range h.fixture.TokenRequests() {
		if request.Get("grant_type") == "refresh_token" {
			t.Fatal("refresh grant sent although a newer token was stored")
		}
	}
	refreshed, err = h.flow.Refresh(t.Context(), record.AccessToken)
	if err != nil || !refreshed {
		t.Fatalf("refresh of the rejected token = %v %v", refreshed, err)
	}
}

func TestTokensAreNotSentToAReconfiguredURL(t *testing.T) {
	h := newHarness(t, mcpoauthtest.Options{}, nil)
	h.login(t)
	servers, err := mcpconfig.Normalize([]mcpconfig.Server{{Name: "secure", Transport: "http", URL: h.fixture.MCP.URL + "/other"}})
	if err != nil {
		t.Fatal(err)
	}
	moved, err := mcpoauth.New(servers[0], h.store)
	if err != nil {
		t.Fatal(err)
	}
	if token, err := moved.Token(context.Background()); err != nil || token != "" {
		t.Fatalf("token for another URL = %q %v", token, err)
	}
}

func TestStepUpScopeIsKeptForTheNextLogin(t *testing.T) {
	h := newHarness(t, mcpoauthtest.Options{}, nil)
	h.login(t)
	if err := h.flow.NoteScope(t.Context(), "mcp:read mcp:write"); err != nil {
		t.Fatal(err)
	}
	h.login(t)
	authorizations := h.fixture.Authorizations()
	if scope := authorizations[len(authorizations)-1].Get("scope"); scope != "mcp:read mcp:write" {
		t.Fatalf("step-up scope = %q", scope)
	}
	if status, err := h.flow.Status(); err != nil || status.PendingScope != "" || status.Scope != "mcp:read mcp:write" {
		t.Fatalf("status = %#v %v", status, err)
	}
}

func TestLoginUsesCrossOriginResourceMetadataThePolicyAllows(t *testing.T) {
	h := newHarness(t, mcpoauthtest.Options{}, nil)
	var probes atomic.Int32
	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"resource": h.fixture.URL, "authorization_servers": []string{h.fixture.Issuer}})
	}))
	t.Cleanup(metadata.Close)
	challenge, _, err := h.flow.Probe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	challenge.ResourceMetadata = metadata.URL + "/resource-metadata"
	if _, err := h.flow.Login(t.Context(), challenge, h.fixture.Browse); err != nil {
		t.Fatalf("login with metadata on another loopback origin: %v", err)
	}
	if n := probes.Load(); n != 1 {
		t.Fatalf("the challenge's resource_metadata URL was fetched %d times, want 1", n)
	}
}

func TestLoginProbesWellKnownWhenPolicyRefusesResourceMetadata(t *testing.T) {
	var probes atomic.Int32
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(probe.Close)
	for _, hint := range []string{probe.URL + "/metadata-probe", "http://metadata.example/metadata-probe"} {
		h := newHarness(t, mcpoauthtest.Options{}, nil)
		challenge, _, err := h.flow.Probe(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		challenge.ResourceMetadata, challenge.Peer = hint, remotePeer
		// The well-known metadata on the server's own origin is read, so
		// discovery gets as far as the (loopback, thus refused) issuer.
		_, err = h.flow.Login(t.Context(), challenge, h.fixture.Browse)
		if want := "fetching " + h.fixture.Issuer + "/.well-known/oauth-authorization-server"; err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "refusing to reach") {
			t.Fatalf("hint %s: login = %v, want the refused issuer after well-known fallback", hint, err)
		}
	}
	if n := probes.Load(); n != 0 {
		t.Fatalf("Tempest reached the refused resource_metadata URL %d times", n)
	}
}

func TestLoginRefusesToReachInternalAuthorizationServerOfRemoteServer(t *testing.T) {
	h := newHarness(t, mcpoauthtest.Options{}, nil)
	challenge, _, err := h.flow.Probe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	challenge.Peer = remotePeer
	_, err = h.flow.Login(t.Context(), challenge, h.fixture.Browse)
	if err == nil || !strings.Contains(err.Error(), "refusing to reach 127.0.0.1, a loopback address") {
		t.Fatalf("login via an internal authorization server = %v", err)
	}
	if h.fixture.Registrations() != 0 || len(h.fixture.Authorizations()) != 0 {
		t.Fatal("the internal authorization server was contacted")
	}
}

func TestDiscoveryDoesNotFollowRedirects(t *testing.T) {
	var probes atomic.Int32
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { probes.Add(1) }))
	t.Cleanup(probe.Close)
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, probe.URL+"/internal", http.StatusFound)
	}))
	t.Cleanup(mcp.Close)
	servers, err := mcpconfig.Normalize([]mcpconfig.Server{{Name: "redirecting", Transport: "http", URL: mcp.URL + "/mcp"}})
	if err != nil {
		t.Fatal(err)
	}
	flow, err := mcpoauth.New(servers[0], mcpoauth.NewStore(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := flow.Login(t.Context(), mcpoauth.Challenge{}, nil); err == nil {
		t.Fatal("login succeeded through redirects")
	}
	if probes.Load() != 0 {
		t.Fatal("a discovery redirect was followed")
	}
}
