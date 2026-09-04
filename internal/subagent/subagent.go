// Package subagent exposes the built-in `task` tool, which delegates a prompt to
// a fresh child agent sharing the same provider seam.
package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"go.harness.dev/harness/internal/agent"
	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/schema"
)

const defaultMaxConcurrent = 4

// Options holds the configuration for a subagent runner and its task tool.
type Options struct {
	Model            ptypes.Model
	Tools            []agent.AgentTool
	SystemPrompt     string
	Registry         Registry
	StreamFn         agent.StreamFn
	TransformContext func(context.Context, []ptypes.Message) []ptypes.Message
	// PrepareNextTurn, when set, is installed on every child agent so long,
	// tool-heavy delegated builds (agent_creator/ui_generator) compact their
	// transcript between turns just like the root agent.
	PrepareNextTurn func(agent.ShouldStopAfterTurnContext) *agent.AgentLoopTurnUpdate
	BeforeToolCall  func(context.Context, agent.BeforeToolCallContext) *agent.BeforeToolCallResult
	AfterToolCall   func(context.Context, agent.AfterToolCallContext) *agent.AfterToolCallResult
	MaxConcurrent   int
}

// Definition describes a registered subagent type with its own system prompt and tools.
// Model and StreamFn, when set, override the runner's defaults for this type
// only; explore-type read-only helpers can run on the small smol role while
// general-purpose children keep the default role without splitting the task
// tool in two.
type Definition struct {
	Description  string
	SystemPrompt string
	Tools        []agent.AgentTool
	Model        ptypes.Model
	StreamFn     agent.StreamFn
}

// Registry maps subagent type names to their definitions.
type Registry map[string]Definition

// RegistryListing renders the delegatable subagent types for the task tool so
// the orchestrator prompt names the exact child-agent types it may use and does
// not invent one such as "general-purpose". It returns "" for an empty registry.
func RegistryListing(reg Registry) string {
	if len(reg) == 0 {
		return ""
	}
	names := make([]string, 0, len(reg))
	for name := range reg {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("<available_subagents>\n")
	b.WriteString("Delegate with the task tool using ONLY these subagent_type values. There is no \"general-purpose\" type; never invent one.\n")
	for _, name := range names {
		if desc := strings.TrimSpace(reg[name].Description); desc != "" {
			fmt.Fprintf(&b, "- %s: %s\n", name, desc)
		} else {
			fmt.Fprintf(&b, "- %s\n", name)
		}
	}
	b.WriteString("</available_subagents>")
	return b.String()
}

// eventSinkKey is the context key for a subagent's forwarding sink. A subagent
// runs its own child agent loop, so its child tool calls never reach the parent
// stream by default. When a sink rides the context we hand the child's finished
// messages back to it.
type eventSinkKey struct{}

// WithEventSink hands a subagent run a sink for its child agent's finished
// messages. The server points sink at the same stream-json writer it feeds the
// parent agent, so a delegated build's tool calls show up on the wire the FE
// reads. A nil sink is a no-op.
func WithEventSink(ctx context.Context, sink agent.AgentEventSink) context.Context {
	if sink == nil {
		return ctx
	}
	return context.WithValue(ctx, eventSinkKey{}, sink)
}

// eventSinkFromContext returns the sink stashed by [WithEventSink], or nil when
// nobody set one.
func eventSinkFromContext(ctx context.Context) agent.AgentEventSink {
	sink, _ := ctx.Value(eventSinkKey{}).(agent.AgentEventSink)
	return sink
}

// Runner executes subagent tasks with concurrency control.
type Runner struct {
	opts Options
	sem  chan struct{}
}

// taskInput mirrors the task tool's wire payload before Runner validates and normalizes it.
type taskInput struct {
	Description  string `json:"description"`
	Prompt       string `json:"prompt"`
	SubagentType string `json:"subagent_type"`
}

// NewTool creates the task AgentTool from the given options.
func NewTool(opts Options) agent.AgentTool {
	runner := NewRunner(opts)
	return agent.AgentTool{
		Tool: ptypes.Tool{
			Name:        "task",
			Description: "Run a delegated task in a fresh child agent with an isolated context window.",
			Parameters:  schema.Object(schema.JSON{"description": schema.String(), "prompt": schema.String(), "subagent_type": schema.String()}, "prompt"),
		},
		Label: "task",
		Execute: func(ctx context.Context, toolCallID string, params json.RawMessage, onUpdate agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			return runner.Execute(ctx, toolCallID, params, onUpdate)
		},
	}
}

// NewRunner starts a concurrency-limited runner from opts.
func NewRunner(opts Options) *Runner {
	maxConcurrent := opts.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = defaultMaxConcurrent
	}
	opts.Tools = stripTaskTools(opts.Tools)
	return &Runner{opts: opts, sem: make(chan struct{}, maxConcurrent)}
}

// stripTaskTools keeps child agents from recursively spawning more child agents.
func stripTaskTools(tools []agent.AgentTool) []agent.AgentTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]agent.AgentTool, 0, len(tools))
	for _, tool := range tools {
		if tool.Name == "task" {
			continue
		}
		out = append(out, tool)
	}
	return out
}

// Execute runs a delegated subagent task and returns its final output.
func (r *Runner) Execute(ctx context.Context, _ string, params json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
	if r.opts.StreamFn == nil {
		return agent.AgentToolResult{}, errors.New("task tool streamFn is nil")
	}
	var input taskInput
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(params, &input); err != nil {
		return agent.AgentToolResult{}, err
	}
	input.Prompt = strings.TrimSpace(input.Prompt)
	if input.Prompt == "" {
		return agent.AgentToolResult{}, errors.New("task prompt is required")
	}

	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return agent.AgentToolResult{}, ctx.Err()
	}

	systemPrompt := r.opts.SystemPrompt
	childTools := r.opts.Tools
	childModel := r.opts.Model
	childStreamFn := r.opts.StreamFn
	if len(r.opts.Registry) > 0 {
		subagentType := strings.TrimSpace(input.SubagentType)
		if subagentType == "" {
			return agent.AgentToolResult{}, errors.New("task subagent_type is required")
		}
		definition, ok := r.opts.Registry[subagentType]
		if !ok {
			return agent.AgentToolResult{}, fmt.Errorf("unknown subagent_type %q", subagentType)
		}
		if definition.SystemPrompt != "" {
			systemPrompt = definition.SystemPrompt
		}
		if len(definition.Tools) > 0 {
			childTools = stripTaskTools(definition.Tools)
		}
		if definition.Model.ID != "" {
			childModel = definition.Model
		}
		if definition.StreamFn != nil {
			childStreamFn = definition.StreamFn
		}
	}
	child := agent.NewAgent(agent.AgentOptions{
		InitialState: &agent.AgentState{
			SystemPrompt: systemPrompt,
			Model:        childModel,
			Tools:        append([]agent.AgentTool(nil), childTools...),
		},
		StreamFn:                   childStreamFn,
		TransformContext:           r.opts.TransformContext,
		PrepareNextTurnWithContext: r.opts.PrepareNextTurn,
		BeforeToolCall:             r.opts.BeforeToolCall,
		AfterToolCall:              r.opts.AfterToolCall,
	})
	events := 0
	forward := eventSinkFromContext(ctx)
	unsubscribe := child.Subscribe(func(ectx context.Context, ev agent.AgentEvent) error {
		events++
		// We forward only finished messages, and skip lifecycle events: an
		// EventAgentStart would emit a second init and EventAgentEnd maps to a
		// result envelope that would end the parent turn.
		if forward != nil && ev.Type == agent.EventMessageEnd {
			return forward(ectx, ev)
		}
		return nil
	})
	defer unsubscribe()

	if err := child.PromptText(ctx, input.Prompt); err != nil {
		return agent.AgentToolResult{}, err
	}
	child.WaitForIdle()
	state := child.State()
	if state.ErrorMessage != "" {
		return agent.AgentToolResult{}, errors.New(state.ErrorMessage)
	}
	output := finalText(state.Messages)
	if output == "" {
		output = "(no output)"
	}
	details, _ := json.Marshal(map[string]any{"subagent_type": input.SubagentType, "description": input.Description, "messages": len(state.Messages), "events": events})
	return agent.AgentToolResult{
		Content: []ptypes.ContentBlock{ptypes.NewText(output)},
		Details: details,
	}, nil
}

// defaultSystemPrompt keeps unregistered subagents focused when no caller prompt was configured.
func defaultSystemPrompt(subagentType, description string) string {
	parts := []string{"You are a focused subagent. Complete only the delegated task and report the result concisely."}
	if subagentType != "" {
		parts = append(parts, "Subagent type: "+subagentType+".")
	}
	if description != "" {
		parts = append(parts, "Task description: "+description+".")
	}
	return strings.Join(parts, "\n")
}

// finalText walks backward so trailing tool or lifecycle messages don't hide the child's reply.
func finalText(messages []ptypes.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		assistant, ok := asAssistant(messages[i])
		if !ok {
			continue
		}
		parts := []string{}
		for _, block := range assistant.Content {
			if block.Type == ptypes.BlockText {
				parts = append(parts, block.Text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	return ""
}

// asAssistant accepts both message forms the agent state can retain.
func asAssistant(message ptypes.Message) (ptypes.AssistantMessage, bool) {
	switch value := message.(type) {
	case ptypes.AssistantMessage:
		return value, true
	case *ptypes.AssistantMessage:
		if value != nil {
			return *value, true
		}
	}
	return ptypes.AssistantMessage{}, false
}

// FormatTaskSummary prepends "Task: " before the input when non-empty.
func FormatTaskSummary(input string, output string) string {
	input = strings.TrimSpace(input)
	output = strings.TrimSpace(output)
	if input == "" {
		return output
	}
	return fmt.Sprintf("Task: %s\n\n%s", input, output)
}
