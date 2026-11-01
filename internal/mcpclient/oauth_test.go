package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
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
