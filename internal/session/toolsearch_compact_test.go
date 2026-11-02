package session

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/compaction"
	types "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/session/journal"
)

// scriptedToolRequests answers agent requests from a script and summary
// requests with a fixed summary, and keeps the tool list of every agent
// request as the provider received it.
type scriptedToolRequests struct {
	mu        sync.Mutex
	responses []types.AssistantMessage
	toolLists [][]byte
}

func (s *scriptedToolRequests) step(c types.Context, _ *types.StreamOptions, _ faux.State, _ types.Model) (types.AssistantMessage, error) {
	if c.SystemPrompt == compaction.SummarizationSystemPrompt {
		return types.AssistantMessage{Content: []types.ContentBlock{types.NewText("Summary: the user loaded mcp__s__beta, mcp__s__gamma and mcp__s__alpha.")}, StopReason: types.StopStop}, nil
	}
	toolJSON, err := json.Marshal(c.Tools)
	if err != nil {
		return types.AssistantMessage{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.toolLists = append(s.toolLists, toolJSON)
	response := types.AssistantMessage{Content: []types.ContentBlock{types.NewText("done")}, StopReason: types.StopStop}
	if len(s.responses) > 0 {
		response, s.responses = s.responses[0], s.responses[1:]
	}
	return response, nil
}

func (s *scriptedToolRequests) sent() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.toolLists)
}

func toolCallResponse(id, name, args string) types.AssistantMessage {
	return types.AssistantMessage{Content: []types.ContentBlock{types.NewToolCall(id, name, json.RawMessage(args))}, StopReason: types.StopToolUse}
}

func offeredNames(t *testing.T, toolJSON []byte) []string {
	t.Helper()
	var tools []types.Tool
	if err := json.Unmarshal(toolJSON, &tools); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return names
}

func deferredTestTools() []agent.AgentTool {
	tools := []agent.AgentTool{{
		Tool: types.Tool{Name: "read", Description: "Read a file.", Parameters: json.RawMessage(`{"type":"object"}`)},
		Execute: func(context.Context, string, json.RawMessage, agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			return agent.AgentToolResult{Content: []types.ContentBlock{types.NewText("read ran")}}, nil
		},
	}}
	for _, name := range []string{"mcp__s__alpha", "mcp__s__beta", "mcp__s__gamma"} {
		tools = append(tools, agent.AgentTool{
			Tool:       types.Tool{Name: name, Description: "Operation " + name + ".", Parameters: json.RawMessage(`{"type":"object"}`)},
			Deferrable: true,
			Execute: func(context.Context, string, json.RawMessage, agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
				return agent.AgentToolResult{Content: []types.ContentBlock{types.NewText(name + " ran")}}, nil
			},
		})
	}
	return tools
}

// A summarizing compaction replaces the tool_search results that loaded
// tools, and the calls of some of them, with summary prose. The tools a
// request offers must not change at the checkpoint: a tool loaded but never
// called (gamma) stays loaded, the order stays first-load order (beta,
// gamma, alpha, though the kept tail calls only beta), and a resume from
// the checkpoint's journal offers the same bytes.
func TestCompactionKeepsLoadedToolsAndTheirOrder(t *testing.T) {
	ctx := context.Background()
	store := queueStore(t)
	script := &scriptedToolRequests{responses: []types.AssistantMessage{
		toolCallResponse("s1", agent.ToolSearchName, `{"query":"mcp__s__beta mcp__s__gamma"}`),
		toolCallResponse("s2", agent.ToolSearchName, `{"query":"mcp__s__alpha"}`),
		toolCallResponse("c1", "mcp__s__alpha", `{}`),
		toolCallResponse("c2", "mcp__s__beta", `{}`),
		// The checkpoint lands here, before the next request.
		toolCallResponse("c3", "mcp__s__gamma", `{}`),
	}}
	steps := make([]faux.ResponseStep, 32)
	for i := range steps {
		steps[i] = script.step
	}
	provider := faux.New(faux.Options{})
	provider.SetResponses(steps...)
	model := provider.Model()
	model.ContextWindow = 1_000_000
	policy := &agent.ToolSearch{Mode: agent.ToolSearchAlways}
	newAgent := func(messages []types.Message) *agent.Agent {
		return agent.NewAgent(agent.AgentOptions{
			InitialState: &agent.AgentState{SystemPrompt: "system", Model: model, Tools: deferredTestTools(), Messages: messages},
			StreamFn:     provider.StreamSimple,
			SessionID:    store.ID(),
			ToolSearch:   policy,
		})
	}
	live := newAgent(nil)
	runner := newCompactionRunner(model, provider.StreamSimple)
	runner.settings = compaction.CompactionSettings{Enabled: true, ReserveTokens: 10, KeepRecentTokens: 1}
	runner.configureJournal(store, nil, live)
	compactions := 0
	live.PrepareNextTurnE = func(ctx context.Context, c agent.ShouldStopAfterTurnContext) (*agent.AgentLoopTurnUpdate, error) {
		if len(c.ToolResults) == 0 || c.ToolResults[0].ToolName != "mcp__s__beta" {
			return nil, nil
		}
		// Keep only the call of beta and its result verbatim: the cut
		// lands on that assistant message, mid-turn.
		runner.settings.KeepRecentTokens = compaction.EstimateTokens(c.Context.Messages[len(c.Context.Messages)-1]) + 1
		replacement, changed, err := runner.summarize(ctx, c.Context.Messages, true, true, c.Model)
		if err != nil || !changed {
			t.Fatalf("compaction changed=%v err=%v", changed, err)
		}
		compactions++
		live.SetMessages(replacement)
		next := c.Context
		next.Messages = replacement
		return &agent.AgentLoopTurnUpdate{Context: &next}, nil
	}
	AttachJournal(live, store)
	if result, err := live.PromptText(ctx, "load the tools"); err != nil || result.Err != nil {
		t.Fatal(result, err)
	}
	if compactions != 1 {
		t.Fatalf("compactions = %d, want 1", compactions)
	}
	for _, message := range live.State().Messages {
		if result, ok := message.(types.ToolResultMessage); ok && result.ToolName == agent.ToolSearchName {
			t.Fatal("fixture: a tool_search result survived the compaction")
		}
	}

	sent := script.sent()
	if len(sent) != 6 {
		t.Fatalf("agent requests = %d, want 6", len(sent))
	}
	// Requests: search, search, call alpha, call beta, checkpoint, call
	// gamma, done.
	beforeCheckpoint, afterCheckpoint := sent[3], sent[4]
	want := []string{"read", agent.ToolSearchName, "mcp__s__beta", "mcp__s__gamma", "mcp__s__alpha"}
	if got := offeredNames(t, beforeCheckpoint); !slices.Equal(got, want) {
		t.Fatalf("tools before the checkpoint = %v, want %v", got, want)
	}
	if !bytes.Equal(afterCheckpoint, beforeCheckpoint) {
		t.Fatalf("the checkpoint changed the tool list:\nbefore %v\nafter  %v", offeredNames(t, beforeCheckpoint), offeredNames(t, afterCheckpoint))
	}
	var gamma *types.ToolResultMessage
	for _, message := range live.State().Messages {
		if result, ok := message.(types.ToolResultMessage); ok && result.ToolName == "mcp__s__gamma" {
			gamma = &result
		}
	}
	if gamma == nil || gamma.IsError {
		t.Fatalf("call of the tool loaded before the checkpoint and never called = %+v", gamma)
	}

	loaded, err := journal.Load(ctx, store.Path())
	if err != nil {
		t.Fatal(err)
	}
	resumedScript := &scriptedToolRequests{}
	provider.SetResponses(resumedScript.step)
	resumed := newAgent(loaded.Messages)
	if result, err := resumed.PromptText(ctx, "again"); err != nil || result.Err != nil {
		t.Fatal(result, err)
	}
	if first := resumedScript.sent()[0]; !bytes.Equal(first, sent[len(sent)-1]) {
		t.Fatalf("resume from the checkpoint changed the tool list:\nlive    %v\nresumed %v", offeredNames(t, sent[len(sent)-1]), offeredNames(t, first))
	}
}
