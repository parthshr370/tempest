package tempest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/tempest/extension"
)

// catalogMCP is a Streamable HTTP MCP server whose tool list a test
// replaces, announcing each change with tools/list_changed.
type catalogMCP struct {
	server *httptest.Server
	mu     sync.Mutex
	tools  []string
	calls  map[string]int
	notify chan struct{}
}

func newCatalogMCP(t *testing.T, tools ...string) *catalogMCP {
	t.Helper()
	fixture := &catalogMCP{tools: tools, calls: map[string]int{}, notify: make(chan struct{}, 1)}
	var initialized atomic.Bool
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
			return
		case http.MethodGet:
			if !initialized.Load() {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			for {
				select {
				case <-fixture.notify:
					_, _ = fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/tools/list_changed\"}\n\n")
					w.(http.Flusher).Flush()
				case <-r.Context().Done():
					return
				}
			}
		}
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var result string
		switch request.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "session")
			result = `{"protocolVersion":"2025-11-25","capabilities":{"tools":{"listChanged":true}},"serverInfo":{"name":"catalog","version":"1"}}`
		case "notifications/initialized":
			initialized.Store(true)
			w.WriteHeader(http.StatusAccepted)
			return
		case "notifications/cancelled":
			w.WriteHeader(http.StatusAccepted)
			return
		case "tools/list":
			fixture.mu.Lock()
			var listed []string
			for _, name := range fixture.tools {
				listed = append(listed, fmt.Sprintf(`{"name":%q,"description":"The %s operation of the catalog service.","inputSchema":{"type":"object","properties":{"id":{"type":"string"}}}}`, name, name))
			}
			fixture.mu.Unlock()
			result = `{"tools":[` + strings.Join(listed, ",") + `]}`
		case "tools/call":
			fixture.mu.Lock()
			fixture.calls[request.Params.Name]++
			fixture.mu.Unlock()
			result = `{"content":[{"type":"text","text":"ran ` + request.Params.Name + `"}]}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, request.ID, result)
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (f *catalogMCP) config(permission string) MCPServerConfig {
	return MCPServerConfig{Name: "cat", Transport: "http", URL: f.server.URL, Permission: permission}
}

func (f *catalogMCP) called(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

// replace swaps the server's tools and announces it.
func (f *catalogMCP) replace(tools ...string) {
	f.mu.Lock()
	f.tools = tools
	f.mu.Unlock()
	f.notify <- struct{}{}
}

var deferAll = &agent.ToolSearch{Mode: agent.ToolSearchAlways}

func call(name, args, id string) faux.ResponseStep {
	return faux.Respond(faux.ToolCall(name, json.RawMessage(args), id))
}

func openToolSearch(t *testing.T, options Options) *Session {
	t.Helper()
	session, err := Open(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = session.Close(ctx)
	})
	return session
}

func offeredNames(tools []types.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return names
}

func mcpNames(tools []types.Tool) []string {
	var names []string
	for _, tool := range tools {
		if strings.HasPrefix(tool.Name, "mcp__") || tool.Name == agent.ToolSearchName {
			names = append(names, tool.Name)
		}
	}
	return names
}

func lastToolResult(t *testing.T, s *Session) types.ToolResultMessage {
	t.Helper()
	messages := s.Messages()
	for i := len(messages) - 1; i >= 0; i-- {
		if result, ok := normalizeMessage(messages[i]).(types.ToolResultMessage); ok {
			return result
		}
	}
	t.Fatal("no tool result")
	return types.ToolResultMessage{}
}

// A deferred MCP tool loads through tool_search and keeps its permission:
// headless runs deny it like any MCP tool, and an explicit allow lets the
// call reach the server. Searching itself needs no approval.
func TestToolSearchMCPLoadKeepsPermission(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		t.Run(fmt.Sprint("allowed=", allowed), func(t *testing.T) {
			fixture := newCatalogMCP(t, "alpha", "beta", "gamma")
			provider := faux.New(faux.Options{})
			provider.SetResponses(
				call(agent.ToolSearchName, `{"query":"mcp__cat__beta"}`, "search"),
				call("mcp__cat__beta", `{"id":"1"}`, "use"),
				faux.Respond(faux.Text("done")),
			)
			requests := &providerRecorder{}
			policy := permission.Policy{}
			if allowed {
				policy.Overrides = map[string]permission.Decision{"mcp__cat__beta": permission.DecideAllow}
			}
			session := openToolSearch(t, Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &NewSession{MCPServers: []MCPServerConfig{fixture.config("")}}, Stack: &StackConfig{Model: provider.Model(), StreamFn: requests.wrap(provider), PermissionPolicy: policy, ToolSearch: deferAll}})
			result, err := session.Prompt(t.Context(), Prompt{Text: "use beta"})
			if err != nil || result.StopReason != agent.StopCompleted {
				t.Fatalf("run %+v %v", result, err)
			}
			sent := requests.all()
			if got := mcpNames(sent[0].input.Tools); !slices.Equal(got, []string{agent.ToolSearchName}) {
				t.Fatalf("first request MCP tools = %v", got)
			}
			if got := mcpNames(sent[1].input.Tools); !slices.Equal(got, []string{agent.ToolSearchName, "mcp__cat__beta"}) {
				t.Fatalf("after load MCP tools = %v", got)
			}
			use := lastToolResult(t, session)
			if allowed {
				if fixture.called("beta") != 1 || use.IsError {
					t.Fatalf("allowed call: server calls %d, result %+v", fixture.called("beta"), use)
				}
			} else if fixture.called("beta") != 0 || !use.IsError {
				t.Fatalf("denied call reached the server %d times, result %+v", fixture.called("beta"), use)
			}
		})
	}
}

// tools/list_changed updates the deferred catalog: a loaded tool the server
// removed leaves the tool list and a call to it fails without reaching the
// server, and a new tool is searchable and loads.
func TestToolSearchMCPListChanged(t *testing.T) {
	fixture := newCatalogMCP(t, "alpha", "beta")
	provider := faux.New(faux.Options{})
	provider.SetResponses(
		call(agent.ToolSearchName, `{"query":"mcp__cat__alpha mcp__cat__beta"}`, "load"),
		faux.Respond(faux.Text("loaded")),
	)
	requests := &providerRecorder{}
	session := openToolSearch(t, Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &NewSession{MCPServers: []MCPServerConfig{fixture.config("allow")}}, Stack: &StackConfig{Model: provider.Model(), StreamFn: requests.wrap(provider), ToolSearch: deferAll}})
	if _, err := session.Prompt(t.Context(), Prompt{Text: "load"}); err != nil {
		t.Fatal(err)
	}
	fixture.replace("beta", "delta")
	deadline := time.Now().Add(5 * time.Second)
	for !slices.ContainsFunc(session.core.mcp.Tools(), func(tool agent.AgentTool) bool { return tool.Name == "mcp__cat__delta" }) {
		if time.Now().After(deadline) {
			t.Fatal("tools/list_changed did not refresh the catalog")
		}
		time.Sleep(10 * time.Millisecond)
	}
	provider.SetResponses(
		call("mcp__cat__alpha", `{}`, "gone"),
		call(agent.ToolSearchName, `{"query":"delta operation"}`, "find"),
		call("mcp__cat__delta", `{}`, "new"),
		faux.Respond(faux.Text("done")),
	)
	before := len(requests.all())
	if result, err := session.Prompt(t.Context(), Prompt{Text: "use"}); err != nil || result.StopReason != agent.StopCompleted {
		t.Fatalf("run %+v %v", result, err)
	}
	sent := requests.all()[before:]
	if got := mcpNames(sent[0].input.Tools); !slices.Equal(got, []string{agent.ToolSearchName, "mcp__cat__beta"}) {
		t.Fatalf("after list_changed MCP tools = %v", got)
	}
	description := sent[0].input.Tools[slices.IndexFunc(sent[0].input.Tools, func(tool types.Tool) bool { return tool.Name == agent.ToolSearchName })].Description
	if !strings.Contains(description, "mcp__cat__delta") || strings.Contains(description, "mcp__cat__alpha") {
		t.Fatalf("catalog not refreshed: %s", description)
	}
	if got := mcpNames(sent[len(sent)-1].input.Tools); !slices.Equal(got, []string{agent.ToolSearchName, "mcp__cat__beta", "mcp__cat__delta"}) {
		t.Fatalf("after loading the new tool MCP tools = %v", got)
	}
	var gone types.ToolResultMessage
	for _, message := range session.Messages() {
		if result, ok := normalizeMessage(message).(types.ToolResultMessage); ok && result.ToolCallID == "gone" {
			gone = result
		}
	}
	if !gone.IsError || !strings.HasPrefix(gone.Content[0].Text, "Tool mcp__cat__alpha not found.") || fixture.called("alpha") != 0 {
		t.Fatalf("removed tool call: %+v, server calls %d", gone, fixture.called("alpha"))
	}
	if fixture.called("delta") != 1 {
		t.Fatalf("new tool calls = %d", fixture.called("delta"))
	}
}

// Session.Reset starts a new conversation: auto deferral stays on for new
// prompts after the catalog shrinks under the threshold, and after a reset
// the threshold decides again, as it would for a freshly opened session.
func TestToolSearchSessionResetDecidesAgain(t *testing.T) {
	fixture := newCatalogMCP(t, "alpha", "beta", "gamma", "delta")
	provider := faux.New(faux.Options{})
	provider.SetResponses(faux.Respond(faux.Text("one")), faux.Respond(faux.Text("two")), faux.Respond(faux.Text("three")))
	requests := &providerRecorder{}
	policy := &agent.ToolSearch{Mode: agent.ToolSearchAuto, ThresholdTokens: 60}
	session := openToolSearch(t, Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &NewSession{MCPServers: []MCPServerConfig{fixture.config("allow")}}, Stack: &StackConfig{Model: provider.Model(), StreamFn: requests.wrap(provider), ToolSearch: policy}})
	if _, err := session.Prompt(t.Context(), Prompt{Text: "first"}); err != nil {
		t.Fatal(err)
	}
	fixture.replace("alpha")
	deadline := time.Now().Add(5 * time.Second)
	for len(session.core.mcp.Tools()) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("tools/list_changed did not refresh the catalog")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := session.Prompt(t.Context(), Prompt{Text: "same conversation"}); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Reset(t.Context(), "", "reset"); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Prompt(t.Context(), Prompt{Text: "new conversation"}); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{agent.ToolSearchName}, {agent.ToolSearchName}, {"mcp__cat__alpha"}}
	sent := requests.all()
	if len(sent) != len(want) {
		t.Fatalf("sent %d requests, want %d", len(sent), len(want))
	}
	for i, request := range sent {
		if got := mcpNames(request.input.Tools); !slices.Equal(got, want[i]) {
			t.Fatalf("request %d MCP tools = %v, want %v", i, got, want[i])
		}
	}
}

// Resume rebuilds the loaded set from the journal: the first request after
// it offers the tool list the conversation ended with, byte for byte.
func TestToolSearchResumeReproducesToolList(t *testing.T) {
	fixture := newCatalogMCP(t, "alpha", "beta", "gamma")
	provider := faux.New(faux.Options{})
	provider.SetResponses(
		call(agent.ToolSearchName, `{"query":"mcp__cat__gamma"}`, "first"),
		call(agent.ToolSearchName, `{"query":"mcp__cat__alpha"}`, "second"),
		faux.Respond(faux.Text("loaded")),
	)
	requests := &providerRecorder{}
	dir, cwd := t.TempDir(), t.TempDir()
	server := fixture.config("allow")
	stack := StackConfig{Model: provider.Model(), StreamFn: requests.wrap(provider), ToolSearch: deferAll}
	session, err := Open(t.Context(), Options{SessionDir: dir, Cwd: cwd, New: &NewSession{MCPServers: []MCPServerConfig{server}}, Stack: &stack})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Prompt(t.Context(), Prompt{Text: "load"}); err != nil {
		t.Fatal(err)
	}
	path := session.core.store.Path()
	if err := session.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	before := requests.all()
	last := before[len(before)-1].input.Tools
	if got := mcpNames(last); !slices.Equal(got, []string{agent.ToolSearchName, "mcp__cat__gamma", "mcp__cat__alpha"}) {
		t.Fatalf("tools before resume = %v", got)
	}

	provider.SetResponses(faux.Respond(faux.Text("resumed")))
	resumed := openToolSearch(t, Options{SessionDir: dir, Cwd: cwd, Resume: &ResumeSession{Path: path, MCPServers: []MCPServerConfig{server}}, Stack: &stack})
	if _, err := resumed.Prompt(t.Context(), Prompt{Text: "next"}); err != nil {
		t.Fatal(err)
	}
	first := requests.all()[len(before)].input.Tools
	want, _ := json.Marshal(last)
	got, _ := json.Marshal(first)
	if !bytes.Equal(got, want) {
		t.Fatalf("resume changed the tool list:\n%v\n%v", offeredNames(last), offeredNames(first))
	}
}

// A task child runs its own loop over its own scoped tools: deferral in the
// parent never hands the child tool_search or an MCP tool.
func TestToolSearchChildScope(t *testing.T) {
	fixture := newCatalogMCP(t, "alpha", "beta")
	root, child := faux.New(faux.Options{}), faux.New(faux.Options{})
	root.SetResponses(
		call(agent.ToolSearchName, `{"query":"mcp__cat__alpha"}`, "load"),
		call("task", `{"prompt":"look around","subagent_type":"explore"}`, "delegate"),
		faux.Respond(faux.Text("waiting")),
		faux.Respond(faux.Text("root done")),
	)
	child.SetResponses(faux.Respond(faux.Text("child done")))
	rootRequests, childRequests := &providerRecorder{}, &providerRecorder{}
	rootStream, childStream := rootRequests.wrap(root), childRequests.wrap(child)
	dispatch := func(ctx context.Context, model types.Model, input types.Context, options *types.SimpleStreamOptions) *stream.AssistantStream {
		if strings.Contains(input.SystemPrompt, "You are a delegated child.") {
			return childStream(ctx, model, input, options)
		}
		return rootStream(ctx, model, input, options)
	}
	owner := openToolSearch(t, Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &NewSession{PermissionMode: permission.ModeBypass, MCPServers: []MCPServerConfig{fixture.config("allow")}}, Stack: &StackConfig{Model: root.Model(), StreamFn: dispatch, EnableTask: true, ToolSearch: deferAll}})
	result, err := owner.Prompt(t.Context(), Prompt{Text: "delegate"})
	if err != nil || result.StopReason != agent.StopCompleted {
		t.Fatalf("root result: %+v %v", result, err)
	}
	roots := rootRequests.all()
	if got := mcpNames(roots[1].input.Tools); !slices.Equal(got, []string{agent.ToolSearchName, "mcp__cat__alpha"}) {
		t.Fatalf("root MCP tools after load = %v", got)
	}
	children := childRequests.all()
	if len(children) == 0 {
		t.Fatal("child never ran")
	}
	for _, request := range children {
		if got := mcpNames(request.input.Tools); len(got) != 0 {
			t.Fatalf("child was offered %v", got)
		}
	}
}

// Extension tools defer like MCP tools and follow the registry's
// generations: a request offers the loaded tools of the generation it
// leased, so a tool the next revision drops leaves the list and fails as
// unknown, while a tool it adds is searchable.
func TestToolSearchExtensionGenerations(t *testing.T) {
	var calls sync.Map
	tool := func(name string) extension.Tool {
		return extension.Tool{Name: name, Description: "Kit " + name + " lookup.", Effect: extension.EffectRead, InputSchema: json.RawMessage(`{"type":"object"}`), Execute: func(context.Context, extension.Call, extension.Invocation) (extension.Result, error) {
			counter, _ := calls.LoadOrStore(name, new(atomic.Int32))
			counter.(*atomic.Int32).Add(1)
			return extension.Result{Content: []extension.Content{{Type: "text", Text: name + " ran"}}}, nil
		}}
	}
	called := func(name string) int32 {
		counter, ok := calls.Load(name)
		if !ok {
			return 0
		}
		return counter.(*atomic.Int32).Load()
	}
	registry := extension.NewRegistry()
	t.Cleanup(func() { _ = registry.Close(context.Background()) })
	if err := registry.Install(t.Context(), extension.Definition{ID: "kit", Revision: "v1", Tools: []extension.Tool{tool("alpha"), tool("beta")}}); err != nil {
		t.Fatal(err)
	}
	provider := faux.New(faux.Options{})
	provider.SetResponses(
		call(agent.ToolSearchName, `{"query":"ext__kit__alpha"}`, "load"),
		call("ext__kit__alpha", `{}`, "first"),
		faux.Respond(faux.Text("done")),
	)
	requests := &providerRecorder{}
	session := openToolSearch(t, Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &NewSession{}, Registry: registry, Extensions: []extension.Selection{{ID: "kit", Required: true}}, Stack: &StackConfig{Model: provider.Model(), StreamFn: requests.wrap(provider), ToolSearch: deferAll}})
	if result, err := session.Prompt(t.Context(), Prompt{Text: "use alpha"}); err != nil || result.StopReason != agent.StopCompleted {
		t.Fatalf("run %+v %v", result, err)
	}
	sent := requests.all()
	extNames := func(tools []types.Tool) []string {
		var names []string
		for _, tool := range tools {
			if strings.HasPrefix(tool.Name, "ext__") || tool.Name == agent.ToolSearchName {
				names = append(names, tool.Name)
			}
		}
		return names
	}
	if got := extNames(sent[0].input.Tools); !slices.Equal(got, []string{agent.ToolSearchName}) {
		t.Fatalf("first request extension tools = %v", got)
	}
	if got := extNames(sent[1].input.Tools); !slices.Equal(got, []string{agent.ToolSearchName, "ext__kit__alpha"}) || called("alpha") != 1 {
		t.Fatalf("after load extension tools = %v, alpha calls %d", got, called("alpha"))
	}

	if err := registry.Install(t.Context(), extension.Definition{ID: "kit", Revision: "v2", Tools: []extension.Tool{tool("beta"), tool("gamma")}}); err != nil {
		t.Fatal(err)
	}
	provider.SetResponses(
		call("ext__kit__alpha", `{}`, "gone"),
		call(agent.ToolSearchName, `{"query":"gamma"}`, "find"),
		call("ext__kit__gamma", `{}`, "new"),
		faux.Respond(faux.Text("done")),
	)
	before := len(sent)
	if result, err := session.Prompt(t.Context(), Prompt{Text: "again"}); err != nil || result.StopReason != agent.StopCompleted {
		t.Fatalf("run %+v %v", result, err)
	}
	sent = requests.all()[before:]
	if got := extNames(sent[0].input.Tools); !slices.Equal(got, []string{agent.ToolSearchName}) {
		t.Fatalf("after the new revision extension tools = %v", got)
	}
	if got := extNames(sent[len(sent)-1].input.Tools); !slices.Equal(got, []string{agent.ToolSearchName, "ext__kit__gamma"}) {
		t.Fatalf("after loading the new tool extension tools = %v", got)
	}
	var gone types.ToolResultMessage
	for _, message := range session.Messages() {
		if result, ok := normalizeMessage(message).(types.ToolResultMessage); ok && result.ToolCallID == "gone" {
			gone = result
		}
	}
	if !gone.IsError || !strings.HasPrefix(gone.Content[0].Text, "Tool ext__kit__alpha not found.") || called("alpha") != 1 || called("gamma") != 1 {
		t.Fatalf("removed tool call %+v, alpha calls %d, gamma calls %d", gone, called("alpha"), called("gamma"))
	}
}
