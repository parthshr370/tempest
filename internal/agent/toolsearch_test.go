package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
)

// sentRequest is one provider request as it was sent, serialized at send
// time so later transcript appends cannot change it.
type sentRequest struct {
	system   string
	tools    []types.Tool
	toolJSON []byte
	messages []byte
}

func (r sentRequest) names() []string {
	names := make([]string, 0, len(r.tools))
	for _, tool := range r.tools {
		names = append(names, tool.Name)
	}
	return names
}

// sentRequests answers each request with the next scripted message and
// keeps a serialized copy of every request.
type sentRequests struct {
	mu       sync.Mutex
	script   []types.AssistantMessage
	requests []sentRequest
}

func (s *sentRequests) stream(ctx context.Context, model types.Model, c types.Context, _ *types.SimpleStreamOptions) *stream.AssistantStream {
	toolJSON, _ := json.Marshal(c.Tools)
	messages, _ := json.Marshal(c.Messages)
	s.mu.Lock()
	s.requests = append(s.requests, sentRequest{system: c.SystemPrompt, tools: slices.Clone(c.Tools), toolJSON: toolJSON, messages: messages})
	message := types.AssistantMessage{Content: []types.ContentBlock{types.NewText("done")}, StopReason: types.StopStop}
	if len(s.script) > 0 {
		message, s.script = s.script[0], s.script[1:]
	}
	s.mu.Unlock()
	message.API, message.Provider, message.Model = model.API, model.Provider, model.ID
	out := stream.NewAssistantStream(model.API, model.Provider, model.ID)
	go func() {
		out.Push(types.StreamEvent{Type: types.EvStart, Partial: &message})
		out.Push(types.StreamEvent{Type: types.EvDone, Reason: message.StopReason, Message: &message})
	}()
	return out
}

func (s *sentRequests) sent() []sentRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// callMessage is one assistant response calling name with args.
func callMessage(id, name, args string) types.AssistantMessage {
	return types.AssistantMessage{StopReason: types.StopToolUse, Content: []types.ContentBlock{types.NewToolCall(id, name, json.RawMessage(args))}}
}

func doneMessage() types.AssistantMessage {
	return types.AssistantMessage{Content: []types.ContentBlock{types.NewText("done")}, StopReason: types.StopStop}
}

// deferredCatalog is a set of deferrable tools that count their calls.
type deferredCatalog struct {
	calls sync.Map // name -> *atomic.Int32
}

func (c *deferredCatalog) tool(name, description string) AgentTool {
	counter := &atomic.Int32{}
	c.calls.Store(name, counter)
	return AgentTool{
		Tool:       types.Tool{Name: name, Description: description, Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)},
		Deferrable: true,
		Execute: func(context.Context, string, json.RawMessage, AgentToolUpdateCallback) (AgentToolResult, error) {
			counter.Add(1)
			return AgentToolResult{Content: []types.ContentBlock{types.NewText(name + " ran")}}, nil
		},
	}
}

func (c *deferredCatalog) count(name string) int32 {
	counter, ok := c.calls.Load(name)
	if !ok {
		return 0
	}
	return counter.(*atomic.Int32).Load()
}

func (c *deferredCatalog) tools() []AgentTool {
	return []AgentTool{
		okTool("read"),
		c.tool("mcp__gh__create_issue", "Create a new issue in a GitHub repository."),
		c.tool("mcp__gh__list_pulls", "List pull requests.\nSupports filters."),
		c.tool("mcp__db__query", "Run a read-only SQL query; returns issue rows when asked."),
	}
}

func newSearchAgent(tools []AgentTool, script *sentRequests, policy *ToolSearch, messages []types.Message) *Agent {
	return NewAgent(AgentOptions{
		InitialState: &AgentState{SystemPrompt: "system", Model: reminderModel, Tools: tools, Messages: messages},
		ConvertToLlm: func(messages []types.Message) []types.Message { return messages },
		StreamFn:     script.stream,
		ToolSearch:   policy,
	})
}

var alwaysDefer = &ToolSearch{Mode: ToolSearchAlways}

// Exact names select just those tools; otherwise tools not loaded yet come
// first, then words in the name before words only in the description, and
// the limit cuts the list.
func TestToolSearchRanking(t *testing.T) {
	var catalog deferredCatalog
	tools := catalog.tools()[1:]
	names := func(ranked []AgentTool) []string {
		var out []string
		for _, tool := range ranked {
			out = append(out, tool.Name)
		}
		return out
	}
	for _, tc := range []struct {
		name   string
		query  string
		loaded map[string]bool
		limit  int
		want   []string
	}{
		{"name beats description", "issue", nil, 5, []string{"mcp__gh__create_issue", "mcp__db__query"}},
		{"exact names select only those", "issue mcp__db__query", nil, 5, []string{"mcp__db__query"}},
		{"case and separators", "Create-Issue", nil, 5, []string{"mcp__gh__create_issue", "mcp__db__query"}},
		{"unloaded before loaded", "issue", map[string]bool{"mcp__gh__create_issue": true}, 5, []string{"mcp__db__query", "mcp__gh__create_issue"}},
		{"limit", "gh", nil, 1, []string{"mcp__gh__create_issue"}},
		{"namespace marker matches nothing", "mcp", nil, 5, nil},
		{"word boundaries", "requ", nil, 5, nil},
		{"no match", "kubernetes", nil, 5, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := names(rankTools(tools, tc.loaded, tc.query, tc.limit)); !slices.Equal(got, tc.want) {
				t.Fatalf("rank(%q) = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}

// The model searches, the match loads, and the next request offers it at
// the end of the tool list, where the model calls it. The load only appends
// to the tool list: the earlier tools, the system prompt and the earlier
// messages are byte-identical, and the request after keeps the same list.
func TestToolSearchLoadThenCallAppendsOnly(t *testing.T) {
	var catalog deferredCatalog
	script := &sentRequests{script: []types.AssistantMessage{
		callMessage("s1", ToolSearchName, `{"query":"create issue"}`),
		callMessage("c1", "mcp__gh__create_issue", `{"q":"bug"}`),
		callMessage("c2", "read", `{}`),
		doneMessage(),
	}}
	a := newSearchAgent(catalog.tools(), script, alwaysDefer, nil)
	if _, err := a.PromptText(context.Background(), "file a bug"); err != nil {
		t.Fatal(err)
	}
	sent := script.sent()
	if len(sent) != 4 {
		t.Fatalf("requests = %d, want 4", len(sent))
	}
	if got, want := sent[0].names(), []string{"read", ToolSearchName}; !slices.Equal(got, want) {
		t.Fatalf("first request tools = %v, want %v", got, want)
	}
	description := sent[0].tools[1].Description
	for _, name := range []string{"mcp__gh__create_issue", "mcp__gh__list_pulls", "mcp__db__query"} {
		if !strings.Contains(description, name) {
			t.Fatalf("tool_search description does not list %s: %s", name, description)
		}
	}
	results := toolResults(a.State().Messages)
	if len(results) != 3 || results[0].IsError || !strings.Contains(results[0].Content[0].Text, "mcp__gh__create_issue: Create a new issue") {
		t.Fatalf("search result = %+v", results)
	}
	if got, want := sent[1].names(), []string{"read", ToolSearchName, "mcp__gh__create_issue", "mcp__db__query"}; !slices.Equal(got, want) {
		t.Fatalf("second request tools = %v, want %v", got, want)
	}
	if catalog.count("mcp__gh__create_issue") != 1 || results[1].IsError {
		t.Fatalf("loaded tool calls = %d, result %+v", catalog.count("mcp__gh__create_issue"), results[1])
	}
	// The cached prefix: the first request's tool list is a byte prefix of
	// the second's, minus the closing bracket, and nothing else before the
	// new messages changed.
	before := bytes.TrimSuffix(sent[0].toolJSON, []byte("]"))
	if !bytes.HasPrefix(sent[1].toolJSON, before) || sent[0].system != sent[1].system {
		t.Fatalf("load rewrote the prefix:\n%s\n%s", sent[0].toolJSON, sent[1].toolJSON)
	}
	if !bytes.HasPrefix(sent[1].messages, bytes.TrimSuffix(sent[0].messages, []byte("]"))) {
		t.Fatalf("load rewrote earlier messages")
	}
	for i := 2; i < len(sent); i++ {
		if !bytes.Equal(sent[i].toolJSON, sent[1].toolJSON) || sent[i].system != sent[1].system {
			t.Fatalf("request %d tools changed after the load:\n%s\n%s", i+1, sent[1].toolJSON, sent[i].toolJSON)
		}
	}
}

// A deferred tool the model has not loaded is neither offered nor run; the
// answer tells it how to load the tool.
func TestToolSearchUnloadedCallFails(t *testing.T) {
	var catalog deferredCatalog
	script := &sentRequests{script: []types.AssistantMessage{
		callMessage("c1", "mcp__gh__list_pulls", `{}`),
		callMessage("c2", "mcp__missing__tool", `{}`),
		doneMessage(),
	}}
	a := newSearchAgent(catalog.tools(), script, alwaysDefer, nil)
	if _, err := a.PromptText(context.Background(), "list pulls"); err != nil {
		t.Fatal(err)
	}
	results := toolResults(a.State().Messages)
	if catalog.count("mcp__gh__list_pulls") != 0 || len(results) != 2 {
		t.Fatalf("unloaded tool ran %d times, results %+v", catalog.count("mcp__gh__list_pulls"), results)
	}
	if text := results[0].Content[0].Text; !results[0].IsError || text != notLoadedMessage("mcp__gh__list_pulls") {
		t.Fatalf("unloaded call result = %q", text)
	}
	// An unknown tool lists only what the request offered.
	if text := results[1].Content[0].Text; !results[1].IsError || text != "Tool mcp__missing__tool not found. The tools in this session are: read, tool_search." {
		t.Fatalf("unknown call result = %q", text)
	}
	// A failed call does not load the tool.
	for _, request := range script.sent() {
		if got := request.names(); !slices.Equal(got, []string{"read", ToolSearchName}) {
			t.Fatalf("request tools = %v", got)
		}
	}
}

// Auto mode defers only once the deferrable schemas reach the threshold;
// below it, and in never mode, the request is exactly the eager list.
func TestToolSearchThreshold(t *testing.T) {
	var catalog deferredCatalog
	tools := catalog.tools()
	chars := 0
	for _, tool := range tools[1:] {
		chars += len(tool.Name) + len(tool.Description) + len(tool.Parameters)
	}
	size := (chars + 3) / 4
	for _, tc := range []struct {
		name   string
		policy *ToolSearch
		defer_ bool
	}{
		{"auto default threshold", nil, false},
		{"auto below", &ToolSearch{ThresholdTokens: size + 1}, false},
		{"auto at threshold", &ToolSearch{Mode: ToolSearchAuto, ThresholdTokens: size}, true},
		{"never", &ToolSearch{Mode: ToolSearchNever, ThresholdTokens: 1}, false},
		{"always", alwaysDefer, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := &sentRequests{}
			a := newSearchAgent(tools, script, tc.policy, nil)
			if _, err := a.PromptText(context.Background(), "hi"); err != nil {
				t.Fatal(err)
			}
			got := script.sent()[0].names()
			want := []string{"read", "mcp__gh__create_issue", "mcp__gh__list_pulls", "mcp__db__query"}
			if tc.defer_ {
				want = []string{"read", ToolSearchName}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("tools = %v, want %v", got, want)
			}
		})
	}
}

// The search covers only the agent's own tools, so a scoped agent (a
// child, or one whose tool list a preset narrowed) cannot reveal or load a
// tool outside its list, even by its exact name.
func TestToolSearchScope(t *testing.T) {
	var catalog deferredCatalog
	scoped := catalog.tools()[:2]
	script := &sentRequests{script: []types.AssistantMessage{
		callMessage("s1", ToolSearchName, `{"query":"mcp__db__query pull"}`),
		callMessage("c1", "mcp__db__query", `{}`),
		doneMessage(),
	}}
	a := newSearchAgent(scoped, script, alwaysDefer, nil)
	if _, err := a.PromptText(context.Background(), "query"); err != nil {
		t.Fatal(err)
	}
	sent := script.sent()
	if strings.Contains(sent[0].tools[1].Description, "mcp__db__query") {
		t.Fatalf("description lists a tool outside the scope: %s", sent[0].tools[1].Description)
	}
	results := toolResults(a.State().Messages)
	if !strings.HasPrefix(results[0].Content[0].Text, "No deferred tool matches") || string(results[0].Details) != `{"loaded":[]}` {
		t.Fatalf("search result = %+v", results[0])
	}
	if !results[1].IsError || catalog.count("mcp__db__query") != 0 {
		t.Fatalf("out-of-scope call = %+v", results[1])
	}
	for _, request := range sent {
		if got := request.names(); !slices.Equal(got, []string{"read", ToolSearchName}) {
			t.Fatalf("request tools = %v", got)
		}
	}
}

// A loaded tool keeps its place across runs; when the catalog drops it (an
// MCP server's tools/list_changed, an extension generation) it leaves the
// list, later loads keep their order, and a call to it fails as unknown.
func TestToolSearchRemovedToolFailsCleanly(t *testing.T) {
	var catalog deferredCatalog
	tools := catalog.tools()
	script := &sentRequests{script: []types.AssistantMessage{
		callMessage("s1", ToolSearchName, `{"query":"mcp__db__query mcp__gh__list_pulls"}`),
		doneMessage(),
	}}
	a := newSearchAgent(tools, script, alwaysDefer, nil)
	if _, err := a.PromptText(context.Background(), "load"); err != nil {
		t.Fatal(err)
	}
	a.SetTools(slices.DeleteFunc(slices.Clone(tools), func(tool AgentTool) bool { return tool.Name == "mcp__db__query" }))
	script.script = []types.AssistantMessage{callMessage("c1", "mcp__db__query", `{}`), doneMessage()}
	if _, err := a.PromptText(context.Background(), "query"); err != nil {
		t.Fatal(err)
	}
	sent := script.sent()
	if got, want := sent[1].names(), []string{"read", ToolSearchName, "mcp__db__query", "mcp__gh__list_pulls"}; !slices.Equal(got, want) {
		t.Fatalf("after load tools = %v, want %v", got, want)
	}
	if got, want := sent[2].names(), []string{"read", ToolSearchName, "mcp__gh__list_pulls"}; !slices.Equal(got, want) {
		t.Fatalf("after removal tools = %v, want %v", got, want)
	}
	results := toolResults(a.State().Messages)
	last := results[len(results)-1]
	if !last.IsError || !strings.HasPrefix(last.Content[0].Text, "Tool mcp__db__query not found.") || catalog.count("mcp__db__query") != 0 {
		t.Fatalf("removed tool call = %+v", last)
	}
}

// Resume rebuilds the agent from the journaled transcript: the first request
// after it offers the tool list the conversation ended with, byte for byte.
func TestToolSearchResumeReproducesToolList(t *testing.T) {
	var catalog deferredCatalog
	script := &sentRequests{script: []types.AssistantMessage{
		callMessage("s1", ToolSearchName, `{"query":"query"}`),
		callMessage("c1", "mcp__db__query", `{}`),
		callMessage("s2", ToolSearchName, `{"query":"mcp__gh__list_pulls"}`),
		doneMessage(),
	}}
	a := newSearchAgent(catalog.tools(), script, alwaysDefer, nil)
	if _, err := a.PromptText(context.Background(), "work"); err != nil {
		t.Fatal(err)
	}
	// The journal stores messages as JSON; resume decodes them.
	raw, err := json.Marshal(a.State().Messages)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	messages := make([]types.Message, 0, len(decoded))
	for _, item := range decoded {
		message, err := types.UnmarshalMessage(item)
		if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, message)
	}
	before := script.sent()
	resumed := &sentRequests{}
	if _, err := newSearchAgent(catalog.tools(), resumed, alwaysDefer, messages).PromptText(context.Background(), "next"); err != nil {
		t.Fatal(err)
	}
	last, first := before[len(before)-1], resumed.sent()[0]
	if got, want := last.names(), []string{"read", ToolSearchName, "mcp__db__query", "mcp__gh__list_pulls"}; !slices.Equal(got, want) {
		t.Fatalf("tools before resume = %v, want %v", got, want)
	}
	if !bytes.Equal(first.toolJSON, last.toolJSON) {
		t.Fatalf("resume changed the tool list:\n%s\n%s", last.toolJSON, first.toolJSON)
	}
}

// Permission gates see a loaded tool under its own name, exactly as when it
// is eager: a blocked tool stays blocked after it loads.
func TestToolSearchKeepsPermissionGate(t *testing.T) {
	var catalog deferredCatalog
	script := &sentRequests{script: []types.AssistantMessage{
		callMessage("s1", ToolSearchName, `{"query":"mcp__gh__create_issue"}`),
		callMessage("c1", "mcp__gh__create_issue", `{}`),
		doneMessage(),
	}}
	var gated []string
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{SystemPrompt: "system", Model: reminderModel, Tools: catalog.tools()},
		ConvertToLlm: func(messages []types.Message) []types.Message { return messages },
		StreamFn:     script.stream,
		ToolSearch:   alwaysDefer,
		BeforeToolCall: func(_ context.Context, call BeforeToolCallContext) *BeforeToolCallResult {
			gated = append(gated, call.ToolCall.Name)
			if call.ToolCall.Name == "mcp__gh__create_issue" {
				return &BeforeToolCallResult{Block: true, Reason: "denied by policy"}
			}
			return nil
		},
	})
	if _, err := a.PromptText(context.Background(), "file"); err != nil {
		t.Fatal(err)
	}
	results := toolResults(a.State().Messages)
	if !slices.Equal(gated, []string{ToolSearchName, "mcp__gh__create_issue"}) || catalog.count("mcp__gh__create_issue") != 0 || !results[1].IsError || results[1].Content[0].Text != "denied by policy" {
		t.Fatalf("gate saw %v, calls %d, result %+v", gated, catalog.count("mcp__gh__create_issue"), results[1])
	}
}

// Auto deferral is sticky: once a conversation defers, a catalog change
// that takes the deferrable schemas back under the threshold (an MCP
// tools/list_changed, an extension generation) does not flip the request
// back to the eager layout. Reset starts a new conversation, which decides
// by the threshold again.
func TestToolSearchAutoStaysDeferred(t *testing.T) {
	var catalog deferredCatalog
	full := catalog.tools()
	chars := 0
	for _, tool := range full[1:] {
		chars += len(tool.Name) + len(tool.Description) + len(tool.Parameters)
	}
	policy := &ToolSearch{Mode: ToolSearchAuto, ThresholdTokens: (chars + 3) / 4}
	small := full[:2]
	script := &sentRequests{}
	a := newSearchAgent(full, script, policy, nil)
	for _, tools := range [][]AgentTool{full, small, full, small} {
		a.SetTools(tools)
		if _, err := a.PromptText(context.Background(), "hi"); err != nil {
			t.Fatal(err)
		}
	}
	for i, request := range script.sent() {
		if got := request.names(); !slices.Equal(got, []string{"read", ToolSearchName}) {
			t.Fatalf("request %d tools = %v, want the deferred layout", i, got)
		}
	}
	a.Reset()
	if _, err := a.PromptText(context.Background(), "new conversation"); err != nil {
		t.Fatal(err)
	}
	sent := script.sent()
	if got, want := sent[len(sent)-1].names(), []string{"read", "mcp__gh__create_issue"}; !slices.Equal(got, want) {
		t.Fatalf("after Reset tools = %v, want %v", got, want)
	}
}

// SetMessages with a non-empty transcript (a compaction, a rewind) keeps
// the conversation and its deferral; an empty one (a context reset or
// handoff) starts a new conversation, which decides by the threshold again
// like [Agent.Reset].
func TestToolSearchAutoEmptyTranscriptDecidesAgain(t *testing.T) {
	var catalog deferredCatalog
	full := catalog.tools()
	chars := 0
	for _, tool := range full[1:] {
		chars += len(tool.Name) + len(tool.Description) + len(tool.Parameters)
	}
	policy := &ToolSearch{Mode: ToolSearchAuto, ThresholdTokens: (chars + 3) / 4}
	script := &sentRequests{}
	a := newSearchAgent(full, script, policy, nil)
	if _, err := a.PromptText(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	a.SetTools(full[:2])
	a.SetMessages([]types.Message{types.UserMessage{Content: types.StringContent("summary")}})
	if _, err := a.PromptText(context.Background(), "after compaction"); err != nil {
		t.Fatal(err)
	}
	a.SetMessages(nil)
	if _, err := a.PromptText(context.Background(), "after reset"); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"read", ToolSearchName}, {"read", ToolSearchName}, {"read", "mcp__gh__create_issue"}}
	sent := script.sent()
	if len(sent) != len(want) {
		t.Fatalf("sent %d requests, want %d", len(sent), len(want))
	}
	for i, request := range sent {
		if got := request.names(); !slices.Equal(got, want[i]) {
			t.Fatalf("request %d tools = %v, want %v", i, got, want[i])
		}
	}
}

// A conversation reopened in a new Agent (resume, fork) under the
// threshold keeps deferring when its transcript shows a tool_search call,
// so its loaded tools keep their places.
func TestToolSearchAutoReopenedAfterSearchStaysDeferred(t *testing.T) {
	var catalog deferredCatalog
	small := catalog.tools()[:3]
	searched := []types.Message{
		types.UserMessage{Content: types.StringContent("load")},
		callMessage("s1", ToolSearchName, `{"query":"mcp__gh__list_pulls"}`),
		types.ToolResultMessage{ToolCallID: "s1", ToolName: ToolSearchName, Content: []types.ContentBlock{types.NewText("loaded")}, Details: json.RawMessage(`{"loaded":["mcp__gh__list_pulls"]}`)},
		doneMessage(),
	}
	for _, tc := range []struct {
		name     string
		messages []types.Message
		want     []string
	}{
		{"searched", searched, []string{"read", ToolSearchName, "mcp__gh__list_pulls"}},
		{"never searched", nil, []string{"read", "mcp__gh__create_issue", "mcp__gh__list_pulls"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := &sentRequests{}
			if _, err := newSearchAgent(small, script, nil, tc.messages).PromptText(context.Background(), "next"); err != nil {
				t.Fatal(err)
			}
			if got := script.sent()[0].names(); !slices.Equal(got, tc.want) {
				t.Fatalf("tools = %v, want %v", got, tc.want)
			}
		})
	}
}
