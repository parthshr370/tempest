package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/stream"
	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/openai"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/tools"
)

// toolTurns is more tool results than any count-based history window would
// keep, so a window that rewrites old results would show up as a changed
// message in a later request.
const toolTurns = 24

// A real stack talking to an OpenRouter-shaped endpoint must send requests in
// which every turn extends the previous one byte for byte: the same tools, the
// same session cache key, and every earlier message unchanged. Anything that
// rewrites history between turns costs the provider's prompt cache.
func TestStackRequestsKeepByteStablePrefix(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "a.go"), []byte("package a\n\nfunc A() int { return 1 }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := journal.Create(context.Background(), t.TempDir(), cwd, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	var mu sync.Mutex
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Errorf("read request: %v", readErr)
		}
		mu.Lock()
		bodies = append(bodies, body)
		turn := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if turn <= toolTurns {
			call := fmt.Sprintf(`{"index":0,"id":"call_%02d","type":"function","function":{"name":"read","arguments":"{\"path\":\"a.go\"}"}}`, turn)
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[%s]}}]}\n\n", call)
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"cost\":0.001}}\n\n")
		} else {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"cost\":0.001}}\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	model := ptypes.Model{ID: "deepseek/deepseek-v4.1-flash", API: "openai-completions", Provider: "openrouter", BaseURL: server.URL, ContextWindow: 200000, MaxTokens: 1024, Cost: ptypes.ModelCost{Unknown: true}}
	streamFn := func(ctx context.Context, m ptypes.Model, c ptypes.Context, opts *ptypes.SimpleStreamOptions) *stream.AssistantStream {
		withKey := *opts
		withKey.APIKey = "test-key"
		return openai.StreamSimple(ctx, m, c, &withKey)
	}
	stack, err := BuildAgentStack(StackConfig{Cwd: cwd, Model: model, StreamFn: streamFn, Journal: store})
	if err != nil {
		t.Fatal(err)
	}
	result, err := stack.Agent.PromptText(context.Background(), "read a.go until told to stop")
	if err != nil || result.Err != nil {
		t.Fatalf("run err=%v result=%v", err, result.Err)
	}
	if result.Usage.Cost.Unknown || result.Usage.Cost.Total < 0.001*toolTurns {
		t.Fatalf("billed cost not accounted: %+v", result.Usage.Cost)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != toolTurns+1 {
		t.Fatalf("requests = %d, want %d", len(bodies), toolTurns+1)
	}
	for index := 1; index < len(bodies); index++ {
		assertRequestExtends(t, index, bodies[index-1], bodies[index], store.ID())
	}
}

type cachedRequest struct {
	Messages       []json.RawMessage `json:"messages"`
	Tools          json.RawMessage   `json:"tools"`
	PromptCacheKey string            `json:"prompt_cache_key"`
	SessionID      string            `json:"session_id"`
}

func assertRequestExtends(t *testing.T, turn int, previous, next []byte, sessionID string) {
	t.Helper()
	var before, after cachedRequest
	if err := json.Unmarshal(previous, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(next, &after); err != nil {
		t.Fatal(err)
	}
	if after.PromptCacheKey != sessionID || after.SessionID != sessionID {
		t.Fatalf("request %d cache key %q session %q, want journal id %q", turn, after.PromptCacheKey, after.SessionID, sessionID)
	}
	if !bytes.Equal(before.Tools, after.Tools) {
		t.Fatalf("request %d changed the tool list", turn)
	}
	if len(after.Messages) <= len(before.Messages) {
		t.Fatalf("request %d has %d messages, previous %d", turn, len(after.Messages), len(before.Messages))
	}
	for index, message := range before.Messages {
		if !bytes.Equal(message, after.Messages[index]) {
			t.Fatalf("request %d rewrote message %d:\nbefore %s\nafter  %s", turn, index, message, after.Messages[index])
		}
	}
}

// Every selection of the same registry yields the same order, so the tool list
// and the system prompt that names it are identical across sessions and runs.
func TestSelectActiveOrderIsDeterministic(t *testing.T) {
	registry := map[tools.ToolName]agent.AgentTool{}
	for _, name := range []string{"write", "bash", "read", "grep", "edit", "find", "ls", "todo_write", "mcp_zeta", "mcp_alpha"} {
		registry[tools.ToolName(name)] = agent.AgentTool{Tool: ptypes.Tool{Name: name}}
	}
	first := ToolNames(SelectActive(registry, nil, []string{"ls"}, ""))
	if !slices.IsSorted(first) || slices.Contains(first, "ls") {
		t.Fatalf("selection = %v, want sorted names without ls", first)
	}
	for range 20 {
		if again := ToolNames(SelectActive(registry, nil, []string{"ls"}, "")); strings.Join(again, ",") != strings.Join(first, ",") {
			t.Fatalf("selection order changed: %v then %v", first, again)
		}
	}
}
