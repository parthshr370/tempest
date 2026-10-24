package openai

import (
	"bufio"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type authorizationFixture struct {
	server           *httptest.Server
	key              *rsa.PrivateKey
	mu               sync.Mutex
	pending          map[string]url.Values
	refreshes        int
	responses        int
	revocations      int
	revoked          bool
	identitySubject  string
	identityNonce    string
	missingClient    bool
	missingScope     bool
	access           string
	refresh          string
	revocationStatus int
	refreshStarted   chan struct{}
	continueRefresh  chan struct{}
	modifyIdentity   func(*identity)
	tamperSignature  bool
	refreshStatus    int
	jwksStatus       int
	refreshEarliest  any
	checkRefreshJWKS func()
}

func newAuthorizationFixture(t *testing.T) *authorizationFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &authorizationFixture{key: key, pending: make(map[string]url.Values), identitySubject: "account-subject", access: "fixture-access", refresh: "fixture-refresh"}
	fixture.mu.Lock()
	fixture.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(writer).Encode(map[string]string{"issuer": fixture.server.URL, "authorization_endpoint": fixture.server.URL + "/api/accounts/authorize", "token_endpoint": fixture.server.URL + "/api/accounts/oauth/token", "jwks_uri": fixture.server.URL + "/.well-known/jwks.json", "revocation_endpoint": fixture.server.URL + "/oauth/revoke"})
		case "/.well-known/jwks.json":
			if fixture.refreshes > 0 && fixture.checkRefreshJWKS != nil {
				fixture.checkRefreshJWKS()
			}
			if fixture.jwksStatus != 0 {
				writer.WriteHeader(fixture.jwksStatus)
				return
			}
			json.NewEncoder(writer).Encode(map[string]interface{}{"keys": []map[string]string{{"kty": "RSA", "kid": "fixture-key", "alg": "RS256", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
		case "/api/accounts/authorize":
			query := request.URL.Query()
			if query.Get("scope") != Scopes || query.Get("resource") != Resource || query.Get("response_type") != "code" || query.Get("code_challenge_method") != "S256" || query.Get("state") == "" || query.Get("nonce") == "" || !strings.HasPrefix(query.Get("ext_agent_host_id"), "urn:uuid:") {
				t.Error("authorization did not match the documented public-client contract")
			}
			redirect, err := url.Parse(query.Get("redirect_uri"))
			if err != nil || redirect.Scheme != "http" || redirect.Hostname() != "127.0.0.1" || redirect.Path != "/auth/callback" {
				t.Error("incorrect loopback callback")
				http.Error(writer, "bad redirect", 400)
				return
			}
			if query.Get("client_id") == "dynamic_agent_client" {
				if query.Get("agent_name_hint") != "Tempest" {
					t.Error("initial registration is missing truthful application identity")
				}
			} else if query.Get("client_id") != "oaiapp_fixture" || query.Get("agent_name_hint") != "" {
				t.Error("reauthorization did not reuse the issued client")
			}
			fixture.pending[query.Get("state")] = query
			callback := url.Values{"state": {query.Get("state")}, "code": {query.Get("state")}}
			if query.Get("client_id") == "dynamic_agent_client" && !fixture.missingClient {
				callback.Set("client_id", "oaiapp_fixture")
			}
			http.Redirect(writer, request, query.Get("redirect_uri")+"?"+callback.Encode(), http.StatusFound)
		case "/api/accounts/oauth/token":
			if request.Method != "POST" || request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
				t.Error("token grant must be form-encoded POST")
			}
			if err := request.ParseForm(); err != nil {
				t.Error(err)
				return
			}
			grant := request.PostForm
			if grant.Get("client_id") != "oaiapp_fixture" || grant.Get("resource") != Resource || grant.Has("client_secret") {
				t.Error("token grant used the wrong client, resource or client authentication")
			}
			switch grant.Get("grant_type") {
			case "authorization_code":
				pending, ok := fixture.pending[grant.Get("code")]
				delete(fixture.pending, grant.Get("code"))
				digest := sha256.Sum256([]byte(grant.Get("code_verifier")))
				if !ok || grant.Get("redirect_uri") != pending.Get("redirect_uri") || len(grant.Get("code_verifier")) < 43 || base64.RawURLEncoding.EncodeToString(digest[:]) != pending.Get("code_challenge") {
					t.Error("PKCE or exact callback URI mismatch")
					http.Error(writer, `{"error":"invalid_grant"}`, 400)
					return
				}
				nonce := pending.Get("nonce")
				if fixture.identityNonce != "" {
					nonce = fixture.identityNonce
				}
				scope := Scopes
				if fixture.missingScope {
					scope = "openid profile email offline_access resource.invoke"
				}
				json.NewEncoder(writer).Encode(map[string]interface{}{"access_token": fixture.access, "refresh_token": fixture.refresh, "id_token": fixture.sign(t, nonce), "token_type": "Bearer", "scope": scope, "expires_in": 3600})
			case "refresh_token":
				fixture.refreshes++
				if fixture.refreshStarted != nil {
					close(fixture.refreshStarted)
					<-fixture.continueRefresh
				}
				if fixture.refreshStatus != 0 {
					writer.WriteHeader(fixture.refreshStatus)
					io.WriteString(writer, `{"error":"temporarily_unavailable"}`)
					return
				}
				if grant.Has("scope") {
					t.Error("refresh must omit scope")
				}
				if fixture.revoked || grant.Get("refresh_token") != fixture.refresh {
					writer.WriteHeader(400)
					io.WriteString(writer, `{"error":"invalid_grant","error_description":"fixture-refresh must not leak"}`)
					return
				}
				fixture.access = "fixture-access-rotated"
				fixture.refresh = "fixture-refresh-rotated"
				json.NewEncoder(writer).Encode(map[string]interface{}{"access_token": fixture.access, "refresh_token": fixture.refresh, "id_token": fixture.sign(t, ""), "token_type": "Bearer", "expires_in": 3600, "earliest_refresh_at": fixture.refreshEarliest})
			default:
				t.Error("unknown token grant")
				writer.WriteHeader(400)
			}
		case "/oauth/revoke":
			if err := request.ParseForm(); err != nil {
				t.Error(err)
				return
			}
			if request.Method != "POST" || request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || request.PostForm.Get("client_id") != "oaiapp_fixture" || request.PostForm.Get("token") != fixture.refresh || request.PostForm.Get("token_type_hint") != "refresh_token" {
				t.Error("incorrect revocation grant")
			}
			fixture.revocations++
			if fixture.revocationStatus != 0 {
				writer.WriteHeader(fixture.revocationStatus)
				return
			}
			writer.WriteHeader(http.StatusOK)
		case "/v1/responses":
			fixture.responses++
			if request.Header.Get("Authorization") != "Bearer "+fixture.access || request.Header.Get("Content-Type") != "application/json" || request.Header.Get("Accept") != "text/event-stream" || request.Header.Get("ChatGPT-Account-ID") != "" || request.Header.Get("OpenAI-Beta") != "" {
				t.Error("incorrect direct-plan inference headers")
			}
			var payload map[string]json.RawMessage
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if string(payload["store"]) != "false" || string(payload["stream"]) != "true" || len(payload["input"]) == 0 || payload["input"][0] != '[' {
				t.Error("incorrect stateless inference profile")
			}
			for _, forbidden := range []string{"max_output_tokens", "temperature", "top_p", "previous_response_id", "conversation", "metadata"} {
				if _, ok := payload[forbidden]; ok {
					t.Errorf("forbidden plan field %s", forbidden)
				}
			}
			if tools := payload["tools"]; len(tools) > 0 && !strings.Contains(string(tools), `"type":"namespace"`) {
				t.Error("plan tools are not namespaced")
			}
			writer.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"signed-in response\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"response_fixture\",\"usage\":{\"input_tokens\":10,\"output_tokens\":4,\"total_tokens\":14}}}\n\n")
		default:
			http.NotFound(writer, request)
		}
	}))
	fixture.mu.Unlock()
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (fixture *authorizationFixture) counts() (refreshes, responses, revocations int) {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	return fixture.refreshes, fixture.responses, fixture.revocations
}

func (fixture *authorizationFixture) sign(t *testing.T, nonce string) string {
	t.Helper()
	claims := identity{Issuer: fixture.server.URL, Subject: fixture.identitySubject, Audience: json.RawMessage(`"oaiapp_fixture"`), Expires: time.Now().Add(time.Hour).Unix(), Issued: time.Now().Unix(), Nonce: nonce, Email: "fixture@example.test"}
	if fixture.modifyIdentity != nil {
		fixture.modifyIdentity(&claims)
	}
	token := signFixtureIdentity(t, fixture.key, claims)
	if fixture.tamperSignature && token != "" {
		parts := strings.Split(token, ".")
		signature, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil {
			t.Error(err)
			return ""
		}
		signature[0] ^= 1
		parts[2] = base64.RawURLEncoding.EncodeToString(signature)
		token = strings.Join(parts, ".")
	}
	return token
}

func signFixtureIdentity(t *testing.T, key *rsa.PrivateKey, claims identity) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"fixture-key"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Error(err)
		return ""
	}
	unsigned := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Error(err)
		return ""
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func fixtureManager(t *testing.T, fixture *authorizationFixture) *Manager {
	t.Helper()
	return &Manager{Dir: filepath.Join(t.TempDir(), "credentials"), Issuer: fixture.server.URL}
}

func fixtureLogin(t *testing.T, authManager *Manager) Status {
	t.Helper()
	status, err := authManager.Login(context.Background(), "default", false, func(authorizationURL string) error {
		response, err := http.Get(authorizationURL)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			return fmt.Errorf("callback HTTP %d", response.StatusCode)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return status
}

func expireFixture(t *testing.T, authManager *Manager) {
	t.Helper()
	saved, err := authManager.read("default")
	if err != nil {
		t.Fatal(err)
	}
	saved.ExpiresAt = time.Now().Add(-time.Minute)
	if err := authManager.save(saved); err != nil {
		t.Fatal(err)
	}
}

func TestRegistrationPKCEExchangeAndReauthorization(t *testing.T) {
	fixture := newAuthorizationFixture(t)
	authManager := fixtureManager(t, fixture)
	status := fixtureLogin(t, authManager)
	if !status.SignedIn || status.Account != "fixture@example.test" {
		t.Fatalf("status: %+v", status)
	}
	saved, err := authManager.read("default")
	if err != nil {
		t.Fatal(err)
	}
	if saved.ClientID != "oaiapp_fixture" || saved.IDToken == "" || saved.Subject != "account-subject" {
		t.Fatal("validated identity and issued registration not retained")
	}
	firstHost := saved.HostID
	for _, name := range []string{"default.json", "host", "active", "default.lock", "host.lock"} {
		info, err := os.Stat(filepath.Join(authManager.Dir, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("%s is not 0600: %v", name, err)
		}
	}
	info, err := os.Stat(authManager.Dir)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("credential directory is not 0700")
	}
	fixtureLogin(t, authManager)
	saved, err = authManager.read("default")
	if err != nil || saved.HostID != firstHost {
		t.Fatal("reauthorization changed host identity")
	}
	if err := authManager.Logout(context.Background(), "default"); err != nil {
		t.Fatal(err)
	}
	saved, err = authManager.read("default")
	if err != nil || saved.AccessToken != "" || saved.RefreshToken != "" || saved.IDToken != "" || saved.ClientID != "oaiapp_fixture" || saved.HostID != firstHost {
		t.Fatal("logout did not clear only tokens")
	}
	_, _, revocations := fixture.counts()
	if revocations != 1 {
		t.Fatal("logout did not revoke")
	}
	fixtureLogin(t, authManager)
}

func TestLoginRejectsInvalidIdentityOrGrant(t *testing.T) {
	for _, scenario := range []string{"nonce", "registration", "scope", "account"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newAuthorizationFixture(t)
			authManager := fixtureManager(t, fixture)
			if scenario == "account" {
				fixtureLogin(t, authManager)
				fixture.mu.Lock()
				fixture.identitySubject = "other-subject"
				fixture.mu.Unlock()
			}
			fixture.mu.Lock()
			if scenario == "nonce" {
				fixture.identityNonce = "wrong-nonce"
			}
			if scenario == "registration" {
				fixture.missingClient = true
			}
			if scenario == "scope" {
				fixture.missingScope = true
			}
			fixture.mu.Unlock()
			_, err := authManager.Login(context.Background(), "default", false, func(authorizationURL string) error {
				response, err := http.Get(authorizationURL)
				if err == nil {
					response.Body.Close()
				}
				return err
			})
			if err == nil {
				t.Fatal("invalid sign-in succeeded")
			}
			if scenario == "scope" {
				status, err := authManager.Status("default")
				if err != nil || !status.SignedIn || status.PlanEnabled {
					t.Fatal("identity-only sign-in was not retained separately from plan permission")
				}
				if _, err := authManager.Access(context.Background(), "default"); err == nil {
					t.Fatal("identity-only sign-in authorized inference")
				}
			}
			if scenario == "account" {
				saved, err := authManager.read("default")
				if err != nil || saved.Subject != "account-subject" {
					t.Fatal("failed sign-in replaced active identity")
				}
			}
		})
	}
}

func TestCallbackRejectsStateBeforeCodeExchange(t *testing.T) {
	fixture := newAuthorizationFixture(t)
	authManager := fixtureManager(t, fixture)
	_, err := authManager.Login(context.Background(), "default", false, func(authorizationURL string) error {
		authorization, err := url.Parse(authorizationURL)
		if err != nil {
			return err
		}
		callback, err := url.Parse(authorization.Query().Get("redirect_uri"))
		if err != nil {
			return err
		}
		invalid := url.Values{"state": {"wrong"}, "code": {"untrusted"}, "error": {"access_denied"}}
		response, err := http.Get(callback.String() + "?" + invalid.Encode())
		if err != nil {
			return err
		}
		response.Body.Close()
		if response.StatusCode != 400 {
			t.Error("callback accepted wrong state")
		}
		valid := url.Values{"state": {authorization.Query().Get("state")}, "error": {"access_denied"}}
		response, err = http.Get(callback.String() + "?" + valid.Encode())
		if err == nil {
			response.Body.Close()
		}
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatal("denial did not stop login")
	}
}

func TestRefreshConcurrentSessionsRotateOnce(t *testing.T) {
	fixture := newAuthorizationFixture(t)
	authManager := fixtureManager(t, fixture)
	fixtureLogin(t, authManager)
	expireFixture(t, authManager)
	var workers sync.WaitGroup
	for range 12 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			other := &Manager{Dir: authManager.Dir}
			credential, err := other.Access(context.Background(), "default")
			if err != nil || credential.AccessToken != "fixture-access-rotated" {
				t.Errorf("concurrent access: %v", err)
			}
		}()
	}
	workers.Wait()
	refreshes, _, _ := fixture.counts()
	if refreshes != 1 {
		t.Fatalf("refreshes=%d, want one", refreshes)
	}
}

func TestRefreshProcessHelper(t *testing.T) {
	directory := os.Getenv("TEMPEST_REFRESH_HELPER_DIR")
	if directory == "" {
		return
	}
	authManager := &Manager{Dir: directory}
	credential, err := authManager.Access(context.Background(), "default")
	if err != nil || credential.AccessToken != "fixture-access-rotated" {
		t.Fatalf("process access failed: %v", err)
	}
}

func TestRefreshConcurrentProcessesRotateOnce(t *testing.T) {
	fixture := newAuthorizationFixture(t)
	authManager := fixtureManager(t, fixture)
	fixtureLogin(t, authManager)
	expireFixture(t, authManager)
	var workers sync.WaitGroup
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			process := exec.Command(os.Args[0], "-test.run=^TestRefreshProcessHelper$")
			process.Env = append(os.Environ(), "TEMPEST_REFRESH_HELPER_DIR="+authManager.Dir)
			if output, err := process.CombinedOutput(); err != nil {
				t.Errorf("refresh helper: %v %s", err, output)
			}
		}()
	}
	workers.Wait()
	refreshes, _, _ := fixture.counts()
	if refreshes != 1 {
		t.Fatalf("process refreshes=%d, want one", refreshes)
	}
}

func TestRevokedTokenRequiresLogin(t *testing.T) {
	fixture := newAuthorizationFixture(t)
	authManager := fixtureManager(t, fixture)
	fixtureLogin(t, authManager)
	expireFixture(t, authManager)
	fixture.mu.Lock()
	fixture.revoked = true
	fixture.mu.Unlock()
	_, err := authManager.Access(context.Background(), "default")
	if err == nil || !strings.Contains(err.Error(), "tempest login openai") || strings.Contains(err.Error(), "fixture-refresh") {
		t.Fatalf("unsafe or unactionable revocation error: %v", err)
	}
	status, err := authManager.Status("default")
	if err != nil || status.SignedIn {
		t.Fatal("revoked credentials remained usable")
	}
	_, accessErr := authManager.Access(context.Background(), "default")
	refreshes, _, _ := fixture.counts()
	if accessErr == nil || refreshes != 1 {
		t.Fatal("revoked rotating token was retried")
	}
}

func TestLoginRealBinary(t *testing.T) {
	binary := os.Getenv("TEMPEST_TEST_BINARY")
	if binary == "" {
		t.Skip("set TEMPEST_TEST_BINARY to a built tempest binary for the curl-driven proof")
	}
	fixture := newAuthorizationFixture(t)
	root := t.TempDir()
	workingDirectory := filepath.Join(root, "workspace")
	if err := os.Mkdir(workingDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	environment := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "XDG_CONFIG_HOME=" + filepath.Join(root, "config")}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	login := exec.CommandContext(ctx, binary, "login", "openai", "--no-browser", "--issuer", fixture.server.URL)
	login.Env = environment
	login.Dir = workingDirectory
	stdout, err := login.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	login.Stderr = &stderr
	if err := login.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if login.ProcessState == nil {
			_ = login.Process.Kill()
			_ = login.Wait()
		}
	})
	scanner := bufio.NewScanner(stdout)
	var transcript strings.Builder
	callbackDriven := false
	for scanner.Scan() {
		line := scanner.Text()
		transcript.WriteString(line + "\n")
		if strings.HasPrefix(line, fixture.server.URL+"/api/accounts/authorize?") {
			curl := exec.CommandContext(ctx, "curl", "--fail", "--silent", "--show-error", "--location", line)
			if output, err := curl.CombinedOutput(); err != nil {
				t.Fatalf("curl callback: %v %s", err, output)
			}
			callbackDriven = true
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if err := login.Wait(); err != nil {
		t.Fatalf("login binary: %v %s", err, stderr.String())
	}
	if !callbackDriven || !strings.Contains(transcript.String(), "fixture@example.test") {
		t.Fatal("real login did not complete through curl callback")
	}
	t.Log("real binary: dynamic registration, loopback callback driven with curl, verified account fixture@example.test")
	session := exec.CommandContext(ctx, binary, "-model", "openai-chatgpt:fixture-model", "-p", "say hello", "-max-turns", "1", "-session-dir", filepath.Join(root, "sessions"))
	session.Env = environment
	session.Dir = workingDirectory
	output, err := session.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "signed-in response") {
		t.Fatalf("signed-in session: %v %s", err, output)
	}
	if strings.Contains(string(output), "fixture-access") || strings.Contains(string(output), "fixture-refresh") {
		t.Fatal("credential leaked to session output")
	}
	if err := filepath.WalkDir(filepath.Join(root, "sessions"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(contents), "fixture-access") || strings.Contains(string(contents), "fixture-refresh") {
			return fmt.Errorf("credential leaked to the durable session")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Log("real binary: persisted credential authorized /v1/responses; session returned signed-in response")
	doctor := exec.CommandContext(ctx, binary, "doctor", "-model", "openai-chatgpt:fixture-model")
	doctor.Env = environment
	doctor.Dir = workingDirectory
	output, err = doctor.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "fixture@example.test") || strings.Contains(string(output), "fixture-access") {
		t.Fatalf("doctor: %v %s", err, output)
	}
	for _, protocol := range []string{"serve", "mcp"} {
		proveSignedInProtocol(t, ctx, binary, protocol, environment, workingDirectory, filepath.Join(root, protocol+"-sessions"))
	}
	t.Log("real binary: ACP and MCP sessions used the same stored ChatGPT credential")
	logout := exec.CommandContext(ctx, binary, "logout", "openai")
	logout.Env = environment
	logout.Dir = workingDirectory
	if output, err := logout.CombinedOutput(); err != nil {
		t.Fatalf("logout binary: %v %s", err, output)
	}
	_, responses, revocations := fixture.counts()
	if responses != 3 || revocations != 1 {
		t.Fatalf("wire proofs: responses=%d revocations=%d", responses, revocations)
	}
	t.Log("real binary: doctor disclosed account and expiry only; logout revoked renewable session")
}

func TestLogoutAfterConcurrentRefreshDoesNotResurrectTokens(t *testing.T) {
	fixture := newAuthorizationFixture(t)
	authManager := fixtureManager(t, fixture)
	fixtureLogin(t, authManager)
	expireFixture(t, authManager)
	fixture.mu.Lock()
	fixture.refreshStarted = make(chan struct{})
	fixture.continueRefresh = make(chan struct{})
	fixture.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	refreshed := make(chan error, 1)
	go func() {
		_, err := authManager.Access(ctx, "default")
		refreshed <- err
	}()
	select {
	case <-fixture.refreshStarted:
	case <-ctx.Done():
		t.Fatal("refresh did not start")
	}
	loggedOut := make(chan error, 1)
	go func() { loggedOut <- authManager.Logout(ctx, "default") }()
	close(fixture.continueRefresh)
	if err := <-refreshed; err != nil {
		t.Fatal(err)
	}
	if err := <-loggedOut; err != nil {
		t.Fatal(err)
	}
	status, err := authManager.Status("default")
	if err != nil || status.SignedIn {
		t.Fatal("refresh resurrected a logged-out session")
	}
}

func TestLogoutClearsLocalTokensWhenRevocationUnavailable(t *testing.T) {
	fixture := newAuthorizationFixture(t)
	authManager := fixtureManager(t, fixture)
	fixtureLogin(t, authManager)
	fixture.mu.Lock()
	fixture.revocationStatus = http.StatusServiceUnavailable
	fixture.mu.Unlock()
	err := authManager.Logout(context.Background(), "default")
	if err == nil || !strings.Contains(err.Error(), "remote revocation was not confirmed") || strings.Contains(err.Error(), "fixture-refresh") {
		t.Fatalf("unconfirmed remote logout: %v", err)
	}
	status, err := authManager.Status("default")
	if err != nil || status.SignedIn {
		t.Fatal("local sign-out retained usable credentials")
	}
	_, _, revocations := fixture.counts()
	if revocations != 3 {
		t.Fatalf("revocation attempts=%d, want three", revocations)
	}
}

func proveSignedInProtocol(t *testing.T, parent context.Context, binary, protocol string, environment []string, workspace, sessions string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	process := exec.CommandContext(ctx, binary, protocol, "-model", "openai-chatgpt:fixture-model", "-session-dir", sessions, "-mcp-config", "")
	process.Env = environment
	process.Dir = workspace
	input, err := process.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := process.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var diagnostics strings.Builder
	process.Stderr = &diagnostics
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if process.ProcessState == nil {
			cancel()
			_ = process.Wait()
		}
	}()
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var transcript strings.Builder
	readReply := func(identifier int) json.RawMessage {
		t.Helper()
		for scanner.Scan() {
			transcript.WriteString(scanner.Text())
			var frame struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
				t.Fatal(err)
			}
			if frame.ID == identifier {
				if len(frame.Error) != 0 {
					t.Fatalf("%s RPC %d: %s", protocol, identifier, frame.Error)
				}
				return frame.Result
			}
		}
		t.Fatalf("%s stopped before RPC %d: %v", protocol, identifier, scanner.Err())
		return nil
	}
	send := func(request string) {
		t.Helper()
		if _, err := fmt.Fprintln(input, request); err != nil {
			t.Fatal(err)
		}
	}
	if protocol == "serve" {
		send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)
		readReply(1)
		send(fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":%q}}`, workspace))
		var created struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(readReply(2), &created); err != nil || created.SessionID == "" {
			t.Fatal("ACP did not create a session")
		}
		send(fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":%q,"prompt":[{"type":"text","text":"say hello"}]}}`, created.SessionID))
	} else {
		send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`)
		readReply(1)
		send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
		send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"tempest_run","arguments":{"prompt":"say hello"}}}`)
	}
	result := readReply(3)
	completed := strings.Contains(transcript.String(), "signed-in response")
	if protocol == "serve" {
		completed = strings.Contains(string(result), `"stopReason":"end_turn"`) && strings.Contains(string(result), `"totalTokens":14`)
	}
	if !completed || strings.Contains(transcript.String(), "fixture-access") {
		safeTranscript := strings.ReplaceAll(transcript.String(), "fixture-access", "REDACTED")
		safeTranscript = strings.ReplaceAll(safeTranscript, "fixture-refresh", "REDACTED")
		t.Fatalf("%s did not complete safely through the signed-in Responses route: %s", protocol, safeTranscript)
	}
	if err := process.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := process.Wait(); err != nil {
		t.Fatalf("%s shutdown: %v %s", protocol, err, diagnostics.String())
	}
}

func TestIDTokenValidation(t *testing.T) {
	for _, scenario := range []string{"signature", "issuer", "audience", "expiry", "nonce", "subject", "issued", "authorized-party"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newAuthorizationFixture(t)
			authManager := fixtureManager(t, fixture)
			fixture.mu.Lock()
			fixture.tamperSignature = scenario == "signature"
			fixture.modifyIdentity = func(claims *identity) {
				switch scenario {
				case "issuer":
					claims.Issuer = "https://untrusted.example"
				case "audience":
					claims.Audience = json.RawMessage(`"another-client"`)
				case "expiry":
					claims.Expires = time.Now().Add(-time.Minute).Unix()
				case "nonce":
					claims.Nonce = "other-nonce"
				case "subject":
					claims.Subject = ""
				case "issued":
					claims.Issued = time.Now().Add(time.Hour).Unix()
				case "authorized-party":
					claims.Audience = json.RawMessage(`["oaiapp_fixture","another-client"]`)
				}
			}
			fixture.mu.Unlock()
			_, err := authManager.Login(context.Background(), "default", false, func(authorizationURL string) error {
				response, err := http.Get(authorizationURL)
				if err == nil {
					response.Body.Close()
				}
				return err
			})
			if err == nil {
				t.Fatal("sign-in accepted an untrusted identity token")
			}
			status, err := authManager.Status("default")
			if err != nil || status.SignedIn {
				t.Fatal("rejected identity became an active account")
			}
		})
	}
}
