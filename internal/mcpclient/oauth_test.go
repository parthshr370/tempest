package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/mcpconfig"
	"go.harness.dev/harness/internal/mcpoauth"
	"go.harness.dev/harness/internal/mcpoauth/mcpoauthtest"
	"go.harness.dev/harness/internal/question"
)

type oauthSetup struct {
	fixture *mcpoauthtest.Server
	server  mcpconfig.Server
	dir     string
}

func newOAuthSetup(t *testing.T) oauthSetup {
	t.Helper()
	fixture := mcpoauthtest.New(t, mcpoauthtest.Options{})
	return oauthSetup{fixture: fixture, server: mcpconfig.Server{Name: "secure", Transport: "http", URL: fixture.URL, StartupTimeout: "10s", CallTimeout: "10s"}, dir: filepath.Join(t.TempDir(), "mcp-oauth")}
}

func (o oauthSetup) open(t *testing.T, elicit question.Elicitor) *Manager {
	t.Helper()
	manager, err := Open(t.Context(), []mcpconfig.Server{o.server}, Options{Cwd: t.TempDir(), SpillDir: t.TempDir(), CredentialDir: o.dir, Elicit: elicit})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return manager
}

// browser accepts every URL elicitation by completing the authorization.
type browser struct {
	fixture *mcpoauthtest.Server
	mu      sync.Mutex
	asked   []question.Elicit
}

func (b *browser) elicit(ctx context.Context, request question.Elicit) (question.ElicitResult, error) {
	b.mu.Lock()
	b.asked = append(b.asked, request)
	b.mu.Unlock()
	if request.Mode != "url" || request.Server != "secure" || request.ElicitationID == "" {
		return question.ElicitResult{}, errors.New("unexpected elicitation")
	}
	if err := b.fixture.Browse(ctx, request.URL); err != nil {
		return question.ElicitResult{}, err
	}
	return question.ElicitResult{Action: "accept"}, nil
}

func secureTool(t *testing.T, manager *Manager, name string) agent.AgentTool {
	t.Helper()
	for _, tool := range manager.Tools() {
		if tool.Name == "mcp__secure__"+name {
			return tool
		}
	}
	t.Fatalf("tool %s absent from %d tools", name, len(manager.Tools()))
	return agent.AgentTool{}
}

func TestMCPOAuthWithoutUserFailsTheRunNamingLogin(t *testing.T) {
	setup := newOAuthSetup(t)
	manager := setup.open(t, nil)
	if len(manager.Tools()) != 0 {
		t.Fatal("tools offered before authorization")
	}
	err := manager.Prepare(t.Context())
	var startup *StartupError
	var login *mcpoauth.LoginRequired
	if !errors.As(err, &startup) || !errors.As(err, &login) || !strings.Contains(err.Error(), "run `tempest mcp login secure`") {
		t.Fatalf("prepare without a user: %v", err)
	}
}

func TestMCPOAuthAuthorizesThroughURLElicitationAndStepsUp(t *testing.T) {
	setup := newOAuthSetup(t)
	user := &browser{fixture: setup.fixture}
	manager := setup.open(t, user.elicit)
	if err := manager.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	read, err := secureTool(t, manager, "read").Execute(t.Context(), "call-read", json.RawMessage(`{}`), nil)
	if err != nil || read.IsError {
		t.Fatalf("read: %#v %v", read, err)
	}
	write, err := secureTool(t, manager, "write").Execute(t.Context(), "call-write", json.RawMessage(`{}`), nil)
	if err != nil || write.IsError {
		t.Fatalf("write after step-up: %#v %v", write, err)
	}
	if calls := setup.fixture.Calls(); !slices.Equal(calls, []string{"read", "write"}) {
		t.Fatalf("server executed %v", calls)
	}
	authorizations := setup.fixture.Authorizations()
	if len(user.asked) != 2 || len(authorizations) != 2 || authorizations[1].Get("scope") != "mcp:read mcp:write" {
		t.Fatalf("asked=%d authorizations=%v", len(user.asked), authorizations)
	}
}

func TestMCPOAuthRefreshesARejectedTokenWithoutTheUser(t *testing.T) {
	setup := newOAuthSetup(t)
	user := &browser{fixture: setup.fixture}
	manager := setup.open(t, user.elicit)
	if err := manager.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	setup.fixture.ExpireAccessTokens()
	result, err := secureTool(t, manager, "read").Execute(t.Context(), "call", json.RawMessage(`{}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("read after token expiry: %#v %v", result, err)
	}
	if len(user.asked) != 1 {
		t.Fatalf("user asked %d times; a refresh needs no user", len(user.asked))
	}
}

func TestMCPOAuthStepUpWithoutUserFailsTheCallAndKeepsTheScope(t *testing.T) {
	setup := newOAuthSetup(t)
	store := mcpoauth.NewStore(setup.dir)
	servers, err := mcpconfig.Normalize([]mcpconfig.Server{setup.server})
	if err != nil {
		t.Fatal(err)
	}
	flow, err := mcpoauth.New(servers[0], store)
	if err != nil {
		t.Fatal(err)
	}
	challenge, _, err := flow.Probe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := flow.Login(t.Context(), challenge, setup.fixture.Browse); err != nil {
		t.Fatal(err)
	}
	manager := setup.open(t, func(context.Context, question.Elicit) (question.ElicitResult, error) {
		return question.ElicitResult{}, question.Unavailable
	})
	if err := manager.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	result, err := secureTool(t, manager, "write").Execute(t.Context(), "call", json.RawMessage(`{}`), nil)
	if err != nil || !result.IsError || !strings.Contains(result.Content[0].Text, "run `tempest mcp login secure`") {
		t.Fatalf("step-up without a user: %#v %v", result, err)
	}
	status, err := flow.Status()
	if err != nil || status.PendingScope != "mcp:read mcp:write" {
		t.Fatalf("pending scope = %#v %v", status, err)
	}
	if read, err := secureTool(t, manager, "read").Execute(t.Context(), "call-read", json.RawMessage(`{}`), nil); err != nil || read.IsError {
		t.Fatalf("connection retired by an authorization refusal: %#v %v", read, err)
	}
}

// serverLogs collects the data of MCP server log messages.
type serverLogs chan string

func (serverLogs) Enabled(context.Context, slog.Level) bool { return true }
func (l serverLogs) Handle(_ context.Context, record slog.Record) error {
	if record.Message == "MCP server log" {
		record.Attrs(func(attr slog.Attr) bool {
			if attr.Key == "data" {
				l <- attr.Value.String()
			}
			return true
		})
	}
	return nil
}
func (l serverLogs) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l serverLogs) WithGroup(string) slog.Handler      { return l }

func (l serverLogs) expect(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-l:
		if got != want {
			t.Fatalf("server log %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("server log %q never arrived", want)
	}
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !condition(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// The GET stream is optional. When its reconnect is refused for
// authorization, only the stream stops: a call in flight, authorized when
// sent, still gets its answer, nothing polls the server meanwhile, and
// server notifications flow again after the next login.
func TestMCPOAuthStreamRefusalKeepsCallsAndResumesAfterLogin(t *testing.T) {
	fixture := mcpoauthtest.New(t, mcpoauthtest.Options{Stream: true})
	setup := oauthSetup{fixture: fixture, server: mcpconfig.Server{Name: "secure", Transport: "http", URL: fixture.URL, StartupTimeout: "10s", CallTimeout: "10s"}, dir: filepath.Join(t.TempDir(), "mcp-oauth")}
	logs := make(serverLogs, 16)
	user := &browser{fixture: fixture}
	manager, err := Open(t.Context(), []mcpconfig.Server{setup.server}, Options{Cwd: t.TempDir(), SpillDir: t.TempDir(), CredentialDir: setup.dir, Elicit: user.elicit, Logger: slog.New(logs)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	if err := manager.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the stream", func() bool { return fixture.OpenStreams() == 1 })
	fixture.Notify(`{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info","data":"before"}}`)
	logs.expect(t, "before")

	held, abandoned, release := fixture.Hold()
	defer release()
	type outcome struct {
		result agent.AgentToolResult
		err    error
	}
	inFlight := make(chan outcome, 1)
	read := secureTool(t, manager, "read")
	go func() {
		result, err := read.Execute(t.Context(), "call-held", json.RawMessage(`{}`), nil)
		inFlight <- outcome{result, err}
	}()
	<-held
	// The server forgets every token, then its stream drops; the reconnect
	// is refused and its refresh token too, so only a login can help.
	fixture.ExpireAccessTokens()
	fixture.RevokeRefreshTokens()
	refusedGET := fixture.StreamRequests() + 1
	fixture.DropStreams()
	eventually(t, "the refused refresh", func() bool {
		return slices.ContainsFunc(fixture.TokenRequests(), func(form url.Values) bool { return form.Get("grant_type") == "refresh_token" })
	})
	select {
	case <-abandoned:
		t.Fatal("the stream's authorization failure cancelled a call in flight")
	case <-time.After(300 * time.Millisecond):
	}
	release()
	if got := <-inFlight; got.err != nil || got.result.IsError || got.result.Content[0].Text != "did read" {
		t.Fatalf("call in flight: %#v %v", got.result, got.err)
	}
	if got := fixture.StreamRequests(); got != refusedGET {
		t.Fatalf("%d GET requests after the refusal, want %d: the stream must wait for a login, not poll", got, refusedGET)
	}

	// The next call needs a login; the stream follows it.
	result, err := secureTool(t, manager, "read").Execute(t.Context(), "call-after", json.RawMessage(`{}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("call after the refusal: %#v %v", result, err)
	}
	if len(user.asked) != 2 {
		t.Fatalf("user asked %d times, want the initial login and one more", len(user.asked))
	}
	eventually(t, "the stream after login", func() bool { return fixture.OpenStreams() == 1 })
	fixture.Notify(`{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info","data":"after"}}`)
	logs.expect(t, "after")
}

// callTimeout bounds each request, not the user: a login that a resource
// read needs may take longer than one call, as it may for a tool call.
func TestMCPOAuthLoginForAResourceReadOutlastsTheCallTimeout(t *testing.T) {
	fixture := mcpoauthtest.New(t, mcpoauthtest.Options{Resources: true})
	setup := oauthSetup{fixture: fixture, server: mcpconfig.Server{Name: "secure", Transport: "http", URL: fixture.URL, StartupTimeout: "10s", CallTimeout: "1s"}, dir: filepath.Join(t.TempDir(), "mcp-oauth")}
	user := &browser{fixture: fixture}
	slowUser := func(ctx context.Context, request question.Elicit) (question.ElicitResult, error) {
		if len(user.asked) > 0 {
			// The user takes longer than one call to sign in again.
			time.Sleep(1500 * time.Millisecond)
		}
		return user.elicit(ctx, request)
	}
	manager := setup.open(t, slowUser)
	if err := manager.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	fixture.ExpireAccessTokens()
	fixture.RevokeRefreshTokens()
	resource, _, err := manager.ReadResource(t.Context(), "mcp://secure/fixture://note")
	if err != nil || string(resource.Data) != "note text" {
		t.Fatalf("read needing a slow login: %q %v", resource.Data, err)
	}
}
