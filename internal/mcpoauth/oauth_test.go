package mcpoauth_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
	flow, err := mcpoauth.New(servers[0], store, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{fixture: fixture, store: store, dir: dir, flow: flow}
}

func (h *harness) login(t *testing.T) mcpoauth.Record {
	t.Helper()
	challenge, challenged, err := h.flow.Probe(t.Context())
	if err != nil || !challenged {
		t.Fatalf("probe: challenged=%v err=%v", challenged, err)
	}
	record, err := h.flow.Login(t.Context(), challenge, h.fixture.Browse)
	if err != nil {
		t.Fatal(err)
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
	moved, err := mcpoauth.New(servers[0], h.store, nil)
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
