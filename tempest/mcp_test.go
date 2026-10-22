package tempest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
)

const publicMCPSchema = `{"type":"object","$defs":{"text":{"type":"string"}},"properties":{"value":{"$ref":"#/$defs/text","default":"ok"}},"required":["value"],"additionalProperties":false}`

type publicMCPFixture struct {
	server  *httptest.Server
	calls   atomic.Int64
	inits   atomic.Int64
	deletes atomic.Int64
	expire  atomic.Bool
	block   atomic.Bool
	blocked chan struct{}
}

func newPublicMCP(t *testing.T) *publicMCPFixture {
	t.Helper()
	fixture := &publicMCPFixture{blocked: make(chan struct{}, 1)}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.Method == http.MethodDelete {
			fixture.deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			w.WriteHeader(400)
			return
		}
		var result string
		switch request.Method {
		case "initialize":
			fixture.inits.Add(1)
			if fixture.block.Load() {
				select {
				case fixture.blocked <- struct{}{}:
				default:
				}
				<-r.Context().Done()
				return
			}
			w.Header().Set("Mcp-Session-Id", "session")
			result = `{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"public","version":"1"}}`
		case "notifications/initialized", "notifications/cancelled":
			w.WriteHeader(http.StatusAccepted)
			return
		case "tools/list":
			result = `{"tools":[{"name":"read","inputSchema":` + publicMCPSchema + `,"annotations":{"readOnlyHint":true}}]}`
		case "tools/call":
			fixture.calls.Add(1)
			if fixture.expire.Load() {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			result = `{"content":[{"type":"text","text":"preserved remote failure"}],"structuredContent":{"reason":"remote"},"isError":true}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, request.ID, result)
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}
func (f *publicMCPFixture) config() MCPServerConfig {
	return MCPServerConfig{Name: "public", Transport: "http", URL: f.server.URL}
}
func openPublicMCP(t *testing.T, fixture *publicMCPFixture, provider *faux.Faux, prompter Prompter, policy permission.Policy) *Session {
	t.Helper()
	session, err := Open(context.Background(), Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &NewSession{MCPServers: []MCPServerConfig{fixture.config()}}, Prompter: prompter, Stack: &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple, PermissionPolicy: policy}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := session.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return session
}
func TestMCPPermissionAndPreservedJournalContent(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		t.Run(fmt.Sprint(allowed), func(t *testing.T) {
			fixture := newPublicMCP(t)
			provider := faux.New(faux.Options{})
			provider.SetResponses(func(input types.Context, options *types.StreamOptions, state faux.State, model types.Model) (types.AssistantMessage, error) {
				found := false
				for _, tool := range input.Tools {
					if tool.Name == "mcp__public__read" {
						found = true
						if string(tool.Parameters) != publicMCPSchema {
							t.Errorf("provider schema changed: %s", tool.Parameters)
						}
					}
				}
				if !found {
					t.Error("MCP definition absent from actual provider request")
				}
				return faux.Respond(faux.ToolCall("mcp__public__read", json.RawMessage(`{"value":"lookup"}`), "mcp-call"))(input, options, state, model)
			}, faux.Respond(faux.Text("completed despite remote error")))
			policy := permission.Policy{}
			if allowed {
				policy.Overrides = map[string]permission.Decision{"mcp__public__read": permission.DecideAllow}
			}
			session := openPublicMCP(t, fixture, provider, nil, policy)
			run, err := session.Begin(context.Background(), Prompt{Text: "lookup"})
			if err != nil {
				t.Fatal(err)
			}
			id, err := run.Started(context.Background())
			if err != nil || id == "" {
				t.Fatalf("durable start %q %v", id, err)
			}
			result, err := run.Wait()
			if err != nil || result.RunID != id || result.StopReason != agent.StopCompleted {
				t.Fatalf("run %#v %v", result, err)
			}
			expectedCalls := int64(0)
			if allowed {
				expectedCalls = 1
			}
			if fixture.calls.Load() != expectedCalls {
				t.Fatal("permission dispatched denied tool")
			}
			raw, err := os.ReadFile(session.core.store.Path())
			if err != nil {
				t.Fatal(err)
			}
			if allowed {
				if !strings.Contains(string(raw), "preserved remote failure") {
					t.Fatalf("journal lost error content: %s", raw)
				}
				var found bool
				for _, message := range session.Messages() {
					if tool, ok := normalizeMessage(message).(types.ToolResultMessage); ok && tool.ToolName == "mcp__public__read" {
						found = tool.IsError && tool.Content[0].Text == "preserved remote failure" && strings.Contains(string(tool.Details), `"reason":"remote"`)
					}
				}
				if !found {
					t.Fatalf("error content lost: %s", raw)
				}
				permissionIndex, startedIndex := strings.Index(string(raw), `"type":"permission"`), strings.Index(string(raw), `"type":"tool_started"`)
				if permissionIndex < 0 || startedIndex <= permissionIndex {
					t.Fatalf("permission/intent order: %d %d", permissionIndex, startedIndex)
				}
			}
		})
	}
}
func TestRunTerminalCaptureAndOwnedCancel(t *testing.T) {
	provider := faux.New(faux.Options{})
	entered, release := make(chan struct{}), make(chan struct{})
	provider.SetResponses(faux.Respond(faux.Text("first answer")), func(input types.Context, options *types.StreamOptions, state faux.State, model types.Model) (types.AssistantMessage, error) {
		close(entered)
		<-release
		return faux.Respond(faux.Text("second answer"))(input, options, state, model)
	})
	session := openFaux(t, provider, nil)
	first, err := session.Begin(context.Background(), Prompt{Text: "first"})
	if err != nil {
		t.Fatal(err)
	}
	firstResult, err := first.Wait()
	if err != nil {
		t.Fatal(err)
	}
	firstFinished := first.FinishedAt()
	if firstFinished.IsZero() || !firstFinished.Equal(session.Snapshot().LastRun.FinishedAt) {
		t.Fatal("retained terminal timestamp differs from run update")
	}
	second, err := session.Begin(context.Background(), Prompt{Text: "second"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("second run didn't enter provider")
	}
	first.Cancel()
	close(release)
	secondResult, err := second.Wait()
	if err != nil || secondResult.StopReason != agent.StopCompleted {
		t.Fatalf("old cancel reached new run: %#v %v", secondResult, err)
	}
	id, err := first.Started(context.Background())
	if err != nil || id != firstResult.RunID || first.Text() != "first answer" || second.Text() != "second answer" {
		t.Fatalf("terminal capture first=%q second=%q id=%q err=%v", first.Text(), second.Text(), id, err)
	}
	last, ok := normalizeMessage(first.LastMessage()).(types.AssistantMessage)
	if !ok || last.Content[0].Text != "first answer" {
		t.Fatalf("run message overwritten: %#v", first.LastMessage())
	}
	if !first.FinishedAt().Equal(firstFinished) || second.FinishedAt().IsZero() {
		t.Fatal("run-scoped terminal timestamp overwritten")
	}
}
func TestRunSetupCancellation(t *testing.T) {
	fixture := newPublicMCP(t)
	provider := faux.New(faux.Options{})
	provider.SetResponses(faux.Respond(faux.ToolCall("mcp__public__read", json.RawMessage(`{"value":"lookup"}`), "call")), faux.Respond(faux.Text("first complete")))
	session := openPublicMCP(t, fixture, provider, nil, permission.Policy{Overrides: map[string]permission.Decision{"mcp__public__read": permission.DecideAllow}})
	fixture.expire.Store(true)
	if _, err := session.Prompt(context.Background(), Prompt{Text: "expire remote session"}); err != nil {
		t.Fatal(err)
	}
	fixture.block.Store(true)
	run, err := session.Begin(context.Background(), Prompt{Text: "must not reach provider"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-fixture.blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("reconnect setup didn't start")
	}
	run.Cancel()
	result, err := run.Wait()
	if !errors.Is(err, context.Canceled) || result.RunID != "" {
		t.Fatalf("setup cancellation %#v %v", result, err)
	}
	id, startErr := run.Started(context.Background())
	if id != "" || !errors.Is(startErr, context.Canceled) {
		t.Fatalf("invented durable start %q %v", id, startErr)
	}
	if run.LastMessage() != nil || run.Text() != "" {
		t.Fatal("setup failure captured preceding run output")
	}
	if fixture.calls.Load() != 1 {
		t.Fatal("replayed expired tool call")
	}
}

func TestExplicitMCPDisableDoesNotReloadAmbientFile(t *testing.T) {
	t.Setenv("HARNESS_MCP_CONFIG", "/nonexistent/must-not-load.json")
	provider := faux.New(faux.Options{})
	session, err := Open(context.Background(), Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &NewSession{MCPServers: []MCPServerConfig{}}, Stack: &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}})
	if err != nil {
		t.Fatal(err)
	}
	if err = session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRunStartedRejectsUndurableID(t *testing.T) {
	provider := faux.New(faux.Options{})
	session := openFaux(t, provider, nil)
	if err := session.core.store.Close(); err != nil {
		t.Fatal(err)
	}
	run, err := session.Begin(context.Background(), Prompt{Text: "journal cannot record the start"})
	if err != nil {
		t.Fatal(err)
	}
	id, startErr := run.Started(context.Background())
	result, waitErr := run.Wait()
	if id != "" || result.RunID != "" || startErr == nil || waitErr == nil {
		t.Fatalf("undurable ID published: %q %#v %v %v", id, result, startErr, waitErr)
	}
}
