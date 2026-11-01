package tempest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	types "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/session"
	"go.harness.dev/harness/internal/session/journal"
)

// samplingCall is what one sampling request delivered to a model.
type samplingCall struct {
	model     string
	context   types.Context
	maxTokens int
}

// samplingHarness is a faux session whose root model calls the fixture's
// sample tool; the smol model (or an override) answers the server.
type samplingHarness struct {
	t       *testing.T
	session *Session
	root    *faux.Faux
	mu      sync.Mutex
	calls   []samplingCall
	prompts int
}

func samplingModel(id string) *faux.Faux {
	return faux.New(faux.Options{Models: []faux.ModelDef{{ID: id, Input: []string{"text"}, ContextWindow: 128000, MaxTokens: 50}}})
}

// answer queues one sampling reply on model and records what reached it.
func (h *samplingHarness) answer(model *faux.Faux) {
	model.AppendResponses(func(c types.Context, opts *types.StreamOptions, _ faux.State, m types.Model) (types.AssistantMessage, error) {
		h.mu.Lock()
		h.calls = append(h.calls, samplingCall{model: m.ID, context: c, maxTokens: opts.MaxTokens})
		h.mu.Unlock()
		return types.AssistantMessage{Content: []types.ContentBlock{faux.Text("fixture summary")}, StopReason: types.StopStop}, nil
	})
}

// sample runs one prompt in which the root model calls the sample tool,
// and returns the tool's text: the server's view of the sampling reply.
func (h *samplingHarness) sample() string {
	h.t.Helper()
	h.prompts++
	id := fmt.Sprintf("sample-%d", h.prompts)
	h.root.AppendResponses(faux.Respond(faux.ToolCall("mcp__fixture__sample", json.RawMessage(`{}`), id)), faux.Respond(faux.Text("done")))
	if _, err := h.session.Prompt(h.t.Context(), Prompt{Text: "sample the fixture"}); err != nil {
		h.t.Fatal(err)
	}
	result := toolResults(h.session.Messages())[id]
	if result.IsError || len(result.Content) == 0 {
		h.t.Fatalf("sample tool result = %#v", result)
	}
	return result.Content[0].Text
}

func (h *samplingHarness) recorded() []samplingCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]samplingCall(nil), h.calls...)
}

func openSampling(t *testing.T, smol *faux.Faux, opted bool, prompter Prompter, configure func(*StackConfig)) *samplingHarness {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	h := &samplingHarness{t: t, root: faux.New(faux.Options{})}
	stack := &StackConfig{Model: h.root.Model(), StreamFn: h.root.StreamSimple, SmolModel: smol.Model(), SmolStreamFn: smol.StreamSimple}
	if configure != nil {
		configure(stack)
	}
	server := MCPServerConfig{Name: "fixture", Transport: "stdio", Command: mcpFixtureServer, Permission: "allow", Sampling: opted}
	s, err := Open(t.Context(), Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &NewSession{MCPServers: []MCPServerConfig{server}}, Prompter: prompter, Stack: stack})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	h.session = s
	return h
}

func TestSessionMCPSamplingRoutesToSmolAndJournalsSpend(t *testing.T) {
	smol := samplingModel("smol-model")
	h := openSampling(t, smol, true, nil, func(stack *StackConfig) {
		stack.MCP.Sampling = session.MCPSamplingSettings{MaxTokens: 40, AllowHeadless: true}
	})
	h.answer(smol)
	reply := h.sample()
	if !strings.Contains(reply, `"text":"fixture summary"`) || !strings.Contains(reply, `"model":"smol-model"`) {
		t.Fatalf("server got %q", reply)
	}
	calls := h.recorded()
	if len(calls) != 1 {
		t.Fatalf("sampling calls = %d", len(calls))
	}
	call := calls[0]
	// Only the server's own message reaches the model: includeContext is
	// treated as none, no tools are offered, and the server's 100-token
	// request is capped at 40.
	if call.model != "smol-model" || call.maxTokens != 40 || call.context.SystemPrompt != "You are terse." || len(call.context.Tools) != 0 || len(call.context.Messages) != 1 {
		t.Fatalf("smol received %+v", call)
	}
	if user, ok := call.context.Messages[0].(types.UserMessage); !ok || user.Content.Blocks[0].Text != "Summarize the fixture" {
		t.Fatalf("smol messages = %#v", call.context.Messages)
	}

	// The spend is one receipt named for the server, charged to the
	// session total alongside the root model's turns.
	var source string
	var spent types.Usage
	for name, usage := range h.session.core.store.UsageSources() {
		if strings.HasPrefix(name, "mcp-sampling:fixture:") {
			source, spent = name, usage
		}
	}
	if source == "" || spent.Output == 0 {
		t.Fatalf("no sampling receipt in %v", h.session.core.store.UsageSources())
	}
	var expected types.Usage
	for _, message := range h.session.Messages() {
		if assistant, ok := normalizeMessage(message).(types.AssistantMessage); ok {
			expected.Add(assistant.Usage)
		}
	}
	expected.Add(spent)
	if total := h.session.Snapshot().Usage.Total; total.Input != expected.Input || total.Output != expected.Output {
		t.Fatalf("session usage %+v, want %+v", total, expected)
	}
	path := h.session.core.store.Path()
	if err := h.session.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	loaded, err := journal.Load(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.UsageSources[source] != spent || loaded.UsageTotal.Output != expected.Output {
		t.Fatalf("journal sources %v total %+v", loaded.UsageSources, loaded.UsageTotal)
	}
}

func TestSessionMCPSamplingModelOverride(t *testing.T) {
	smol, override := samplingModel("smol-model"), samplingModel("sampling-model")
	h := openSampling(t, smol, true, nil, func(stack *StackConfig) {
		// No MaxTokens: the cap is the model's own output limit (50).
		stack.MCP.Sampling = session.MCPSamplingSettings{Model: override.Model(), StreamFn: override.StreamSimple, AllowHeadless: true}
	})
	h.answer(override)
	if reply := h.sample(); !strings.Contains(reply, `"model":"sampling-model"`) {
		t.Fatalf("server got %q", reply)
	}
	if calls := h.recorded(); len(calls) != 1 || calls[0].model != "sampling-model" || calls[0].maxTokens != 50 || smol.CallCount() != 0 {
		t.Fatalf("calls %+v, smol calls %d", calls, smol.CallCount())
	}
}

func TestSessionMCPSamplingAsksOnFirstUse(t *testing.T) {
	smol := samplingModel("smol-model")
	var mu sync.Mutex
	var asks []Ask
	choices := []Choice{permission.RejectOnce, permission.AllowAlways}
	prompter := promptFunc(func(_ context.Context, ask Ask) (Choice, error) {
		mu.Lock()
		defer mu.Unlock()
		if ask.Tool != "mcp_sampling" {
			return permission.AllowOnce, nil
		}
		asks = append(asks, ask)
		choice := choices[0]
		choices = choices[1:]
		return choice, nil
	})
	h := openSampling(t, smol, true, prompter, func(stack *StackConfig) {
		stack.MCP.Sampling = session.MCPSamplingSettings{MaxTokens: 40}
	})
	if reply := h.sample(); !strings.Contains(reply, "sampling error: -1") || !strings.Contains(reply, "declined") {
		t.Fatalf("rejected reply = %q", reply)
	}
	if len(h.recorded()) != 0 {
		t.Fatal("a rejected request reached the model")
	}
	h.answer(smol)
	if reply := h.sample(); !strings.Contains(reply, "fixture summary") {
		t.Fatalf("allowed reply = %q", reply)
	}
	// Allow always holds for the rest of the session: no third ask.
	h.answer(smol)
	if reply := h.sample(); !strings.Contains(reply, "fixture summary") {
		t.Fatalf("remembered reply = %q", reply)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asks) != 2 || len(h.recorded()) != 2 {
		t.Fatalf("asks %d, model calls %d", len(asks), len(h.recorded()))
	}
	ask := asks[0]
	if !strings.HasPrefix(ask.ToolCallID, "mcp-sampling:fixture:") || !strings.Contains(ask.Title, "MCP server fixture asks to use smol-model") || !strings.Contains(ask.Title, "up to 40 tokens") || !strings.Contains(string(ask.RawInput), "Summarize the fixture") {
		t.Fatalf("ask = %+v", ask)
	}
	if pending := h.session.Snapshot().PendingAsks; len(pending) != 0 {
		t.Fatalf("asks left pending: %+v", pending)
	}
}

func TestSessionMCPSamplingHeadless(t *testing.T) {
	t.Run("denied by default", func(t *testing.T) {
		smol := samplingModel("smol-model")
		h := openSampling(t, smol, true, nil, nil)
		if reply := h.sample(); !strings.Contains(reply, "sampling error: -1") || !strings.Contains(reply, "mcp_sampling_allow_headless") {
			t.Fatalf("headless reply = %q", reply)
		}
		if smol.CallCount() != 0 {
			t.Fatal("a refused request reached the model")
		}
	})
	t.Run("allowed by setting", func(t *testing.T) {
		smol := samplingModel("smol-model")
		h := openSampling(t, smol, true, nil, func(stack *StackConfig) { stack.MCP.Sampling.AllowHeadless = true })
		h.answer(smol)
		if reply := h.sample(); !strings.Contains(reply, "fixture summary") {
			t.Fatalf("headless reply = %q", reply)
		}
	})
}

func TestSessionMCPSamplingNotDeclaredWithoutOptIn(t *testing.T) {
	smol := samplingModel("smol-model")
	h := openSampling(t, smol, false, nil, func(stack *StackConfig) { stack.MCP.Sampling.AllowHeadless = true })
	if reply := h.sample(); reply != "sampling not declared" {
		t.Fatalf("reply = %q", reply)
	}
	if h.session.core.sampling != nil || smol.CallCount() != 0 {
		t.Fatal("sampling wired for a server that did not opt in")
	}
}
