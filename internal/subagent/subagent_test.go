package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/stream"
	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
)

func TestTaskSchemaParity(t *testing.T) {
	tool := NewTool(Options{})

	var got, want any
	if err := json.Unmarshal(tool.Parameters, &got); err != nil {
		t.Fatalf("unmarshal task schema: %v (%s)", err, tool.Parameters)
	}
	if err := json.Unmarshal([]byte(`{"type":"object","properties":{"description":{"type":"string"},"prompt":{"type":"string"},"subagent_type":{"type":"string"}},"required":["prompt"]}`), &want); err != nil {
		t.Fatalf("unmarshal task schema fixture: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("task schema drift:\n got=%s\nwant=%v", tool.Parameters, want)
	}
}

func TestTaskToolRunsChildAgent(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("child done")))
	tool := NewTool(Options{Model: f.Model(), StreamFn: f.StreamSimple})
	result, err := tool.Execute(context.Background(), "task1", rawTask(t, map[string]any{"prompt": "do work", "subagent_type": "worker"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Content[0].Text; got != "child done" {
		t.Fatalf("result = %q", got)
	}
	details := map[string]any{}
	if err := json.Unmarshal(result.Details, &details); err != nil {
		t.Fatalf("details unmarshal: %v", err)
	}
	if details["subagent_type"] != "worker" {
		t.Fatalf("details = %+v", details)
	}
}

func TestTaskToolForwardsChildEventsToSink(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(
		faux.Respond(faux.ToolCall("create_workflow", rawTask(t, map[string]any{"name": "main"}), "cw1")),
		faux.Respond(faux.Text("done")),
	)
	createWorkflowTool := agent.AgentTool{
		Tool: ptypes.Tool{
			Name:       "create_workflow",
			Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		},
		Execute: func(context.Context, string, json.RawMessage, agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			return agent.AgentToolResult{Content: []ptypes.ContentBlock{ptypes.NewText(`{"success":true,"workflow":{"nodes":[{"id":"n1"}],"edges":[]}}`)}}, nil
		},
	}
	tool := NewTool(Options{Model: f.Model(), StreamFn: f.StreamSimple, Tools: []agent.AgentTool{createWorkflowTool}})

	var mu sync.Mutex
	var got []agent.AgentEvent
	ctx := WithEventSink(context.Background(), func(_ context.Context, ev agent.AgentEvent) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, ev)
		return nil
	})
	result, err := tool.Execute(ctx, "t1", rawTask(t, map[string]any{"prompt": "make workflow"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if gotText := result.Content[0].Text; gotText != "done" {
		t.Fatalf("result = %q", gotText)
	}

	mu.Lock()
	forwarded := append([]agent.AgentEvent(nil), got...)
	mu.Unlock()
	if len(forwarded) == 0 {
		t.Fatal("no events forwarded")
	}
	var sawWorkflowUse, sawWorkflowResult bool
	for _, ev := range forwarded {
		if ev.Type == agent.EventAgentStart || ev.Type == agent.EventAgentEnd {
			t.Fatalf("forwarded lifecycle event = %s", ev.Type)
		}
		if ev.Type != agent.EventMessageEnd {
			t.Fatalf("forwarded event = %s, want %s", ev.Type, agent.EventMessageEnd)
		}
		raw, err := json.Marshal(ev.Message)
		if err != nil {
			t.Fatal(err)
		}
		payload := string(raw)
		if ev.Message.Role() == "assistant" && strings.Contains(payload, "create_workflow") {
			sawWorkflowUse = true
		}
		if strings.Contains(payload, "workflow") && strings.Contains(payload, "n1") {
			sawWorkflowResult = true
		}
	}
	if !sawWorkflowUse {
		t.Fatal("missing forwarded create_workflow tool_use")
	}
	if !sawWorkflowResult {
		t.Fatal("missing forwarded workflow tool_result")
	}
}

func TestTaskToolRequiresPrompt(t *testing.T) {
	f := faux.New(faux.Options{})
	tool := NewTool(Options{Model: f.Model(), StreamFn: f.StreamSimple})
	_, err := tool.Execute(context.Background(), "task1", rawTask(t, map[string]any{"description": "missing"}), nil)
	if err == nil || !strings.Contains(err.Error(), "prompt is required") {
		t.Fatalf("err = %v", err)
	}
}

func TestTaskToolConcurrent(t *testing.T) {
	const (
		limit = 2
		tasks = 10
	)
	var inFlight int32
	var observedMax int32
	var calls int32
	streamFn := func(ctx context.Context, model ptypes.Model, c ptypes.Context, opts *ptypes.SimpleStreamOptions) *stream.AssistantStream {
		s := stream.NewAssistantStream("test", "test", model.ID)
		call := atomic.AddInt32(&calls, 1)
		go func() {
			current := atomic.AddInt32(&inFlight, 1)
			for {
				max := atomic.LoadInt32(&observedMax)
				if current <= max || atomic.CompareAndSwapInt32(&observedMax, max, current) {
					break
				}
			}
			defer atomic.AddInt32(&inFlight, -1)
			select {
			case <-ctx.Done():
				msg := ptypes.AssistantMessage{Content: []ptypes.ContentBlock{ptypes.NewText(ctx.Err().Error())}, StopReason: ptypes.StopError}
				s.Push(ptypes.StreamEvent{Type: ptypes.EvError, Err: &msg, Reason: ptypes.StopError})
			case <-time.After(20 * time.Millisecond):
				text := fmt.Sprintf("child-%d", call)
				msg := ptypes.AssistantMessage{Content: []ptypes.ContentBlock{ptypes.NewText(text)}, StopReason: ptypes.StopStop}
				s.Push(ptypes.StreamEvent{Type: ptypes.EvDone, Message: &msg, Reason: ptypes.StopStop})
			}
		}()
		return s
	}
	tool := NewTool(Options{Model: ptypes.Model{ID: "test"}, StreamFn: streamFn, MaxConcurrent: limit})
	var wg sync.WaitGroup
	errs := make(chan error, tasks)
	outputs := make(chan string, tasks)
	for i := 0; i < tasks; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := tool.Execute(context.Background(), "task", rawTask(t, map[string]any{"prompt": "work"}), nil)
			if err == nil {
				outputs <- result.Content[0].Text
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	close(outputs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for output := range outputs {
		seen[output] = true
	}
	if len(seen) != tasks {
		t.Fatalf("outputs = %v", seen)
	}
	if got := atomic.LoadInt32(&observedMax); got > limit || got < 2 {
		t.Fatalf("observed max concurrency = %d", got)
	}
}

func TestNewRunnerStripsRecursiveTaskTool(t *testing.T) {
	runner := NewRunner(Options{Tools: []agent.AgentTool{
		{Tool: ptypes.Tool{Name: "read"}},
		{Tool: ptypes.Tool{Name: "task"}},
	}})
	if len(runner.opts.Tools) != 1 || runner.opts.Tools[0].Name != "read" {
		t.Fatalf("child tools = %+v", runner.opts.Tools)
	}
}

func TestTaskToolUsesRegistryPromptAndTools(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(
		faux.Respond(faux.ToolCall("task", rawTask(t, map[string]any{"prompt": "create agents", "subagent_type": "agent_creator"}), "tc1")),
		func(c ptypes.Context, _ *ptypes.StreamOptions, _ faux.State, _ ptypes.Model) (ptypes.AssistantMessage, error) {
			if !strings.Contains(c.SystemPrompt, "# Agent Creator") || !strings.Contains(c.SystemPrompt, "create_widget") {
				t.Fatalf("child prompt = %q", c.SystemPrompt)
			}
			tools := contextToolNames(c.Tools)
			if strings.Join(tools, ",") != "read,create_widget" {
				t.Fatalf("child tools = %v", tools)
			}
			return ptypes.AssistantMessage{Content: []ptypes.ContentBlock{ptypes.NewText("creator child done")}, StopReason: ptypes.StopStop}, nil
		},
		faux.Respond(faux.Text("parent done")),
	)
	taskTool := NewTool(Options{
		Model:    f.Model(),
		StreamFn: f.StreamSimple,
		Registry: Registry{"agent_creator": {
			SystemPrompt: "# Agent Creator\nUse create_widget.",
			Tools: []agent.AgentTool{
				{Tool: ptypes.Tool{Name: "read"}},
				{Tool: ptypes.Tool{Name: "task"}},
				{Tool: ptypes.Tool{Name: "create_widget"}},
			},
		}},
	})
	parent := agent.NewAgent(agent.AgentOptions{
		InitialState: &agent.AgentState{SystemPrompt: "parent", Model: f.Model(), Tools: []agent.AgentTool{taskTool}},
		StreamFn:     f.StreamSimple,
	})
	if _, err := parent.PromptText(context.Background(), "build app"); err != nil {
		t.Fatal(err)
	}
	if got := f.CallCount(); got != 3 {
		t.Fatalf("faux calls = %d", got)
	}
	if text := finalText(parent.State().Messages); text != "parent done" {
		t.Fatalf("parent final text = %q", text)
	}
}

func TestTaskToolRegistryRejectsUnknownSubagent(t *testing.T) {
	f := faux.New(faux.Options{})
	tool := NewTool(Options{Model: f.Model(), StreamFn: f.StreamSimple, Registry: Registry{"agent_creator": {SystemPrompt: "creator"}}})
	_, err := tool.Execute(context.Background(), "task1", rawTask(t, map[string]any{"prompt": "work", "subagent_type": "missing"}), nil)
	if err == nil || !strings.Contains(err.Error(), "unknown subagent_type") {
		t.Fatalf("err = %v", err)
	}
}

func contextToolNames(tools []ptypes.Tool) []string {
	out := make([]string, len(tools))
	for i, tool := range tools {
		out[i] = tool.Name
	}
	return out
}

func rawTask(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestRegistryListing(t *testing.T) {
	reg := Registry{
		"ui_generator":  {Description: "UI/Frontend Specialist"},
		"agent_creator": {Description: "Agent & Workflow Specialist"},
	}
	got := RegistryListing(reg)
	for _, want := range []string{"<available_subagents>", "- agent_creator: Agent & Workflow Specialist", "- ui_generator: UI/Frontend Specialist", "general-purpose"} {
		if !strings.Contains(got, want) {
			t.Fatalf("listing missing %q:\n%s", want, got)
		}
	}
	if strings.Index(got, "agent_creator") > strings.Index(got, "ui_generator") {
		t.Fatal("subagent types should be sorted")
	}
	if RegistryListing(Registry{}) != "" {
		t.Fatal("empty registry should yield empty listing")
	}
}
