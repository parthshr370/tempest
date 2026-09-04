package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/compaction"
	"go.harness.dev/harness/internal/document"
	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/prompt"
	"go.harness.dev/harness/internal/skills"
	"go.harness.dev/harness/internal/subagent"
	"go.harness.dev/harness/internal/tools"
)

// PermissionModeFunc is a live-mode accessor called from the permission gate
// on every tool call. It exists so callers can update the mode per request
// without rebuilding the agent stack.
type PermissionModeFunc func() permission.Mode

// StackConfig is the recipe for building a generic agent stack. Optional tools
// and prompt additions are supplied by the caller without coupling the core to
// a host integration.
type StackConfig struct {
	Cwd              string
	SpillDir         string
	Model            ptypes.Model
	PlanModel        ptypes.Model
	SmolModel        ptypes.Model
	StreamFn         agent.StreamFn
	SmolStreamFn     agent.StreamFn
	ProductName      string
	CustomPrompt     string
	PromptGuidelines []string
	ContextFiles     []prompt.ContextFile
	Skills           []skills.Skill
	Rules            []prompt.PromptRule
	GenericRules     []string
	Personality      string

	// InitialMessages seeds the agent transcript when resuming a session. Nil
	// starts a fresh conversation.
	InitialMessages []ptypes.Message

	PermissionMode     permission.Mode
	PermissionModeFunc PermissionModeFunc
	PermissionPolicy   permission.Policy
	ExcludeTools       []string
	EnableTask         bool
	EnableWeb          bool
	WebSearchURL       string
	ConfiguredTools    []agent.AgentTool
	PromptAdditions    []string

	// Plan selects a read-only stack. Zero PlanModel uses Model.
	Plan bool
	// AttachmentRegistry + AttachmentReader wire the read-only attachment tool
	// over the session's resolved attachments; Sanitize cleans returned text.
	AttachmentRegistry *document.AttachmentRegistry
	AttachmentReader   *document.CacheRootBlobReader
	Sanitize           func(string) string
}

// AgentStack is the assembled result: a configured agent ready for Prompt/
// Continue calls.
type AgentStack struct {
	Agent      *agent.Agent
	Model      ptypes.Model
	StreamFn   agent.StreamFn
	ProjectDir string
}

// BuildAgentStack wires a generic coding agent: local tools, permission gating,
// compaction, and the neutral prompt builder. Callers may extend it through the
// data seams in StackConfig.
func BuildAgentStack(cfg StackConfig) (*AgentStack, error) {
	if cfg.StreamFn == nil {
		return nil, errors.New("agent stack streamFn is nil")
	}
	cwd := cfg.Cwd
	if strings.TrimSpace(cwd) == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	modeFunc := cfg.PermissionModeFunc
	if modeFunc == nil {
		mode := cfg.PermissionMode
		modeFunc = func() permission.Mode { return mode }
	}

	beforeToolCall := func(ctx context.Context, c agent.BeforeToolCallContext) *agent.BeforeToolCallResult {
		rawArgs, err := json.Marshal(c.Args)
		if err != nil {
			rawArgs = nil
		}
		allow, reason := permission.Gate(ctx, modeFunc(), c.ToolCall.Name, rawArgs, cfg.PermissionPolicy)
		if allow {
			return nil
		}
		return &agent.BeforeToolCallResult{Block: true, Reason: reason}
	}

	if cfg.SpillDir == "" {
		dir, err := os.MkdirTemp("", "harness-tool-outputs-")
		if err != nil {
			return nil, err
		}
		cfg.SpillDir = dir
	}
	transformContext := compaction.Transform(compaction.TransformOptions{Cwd: cwd, SpillToolResults: true, SpillDir: cfg.SpillDir})
	// Children compact against the default role; the parent keeps its own
	// plan-adjusted runner further below.
	childCompact := func(agent.ShouldStopAfterTurnContext) *agent.AgentLoopTurnUpdate { return nil }
	if runner := newCompactionRunner(cfg.Model, cfg.StreamFn); runner != nil {
		childCompact = runner.hook(nil)
	}
	var taskTool agent.AgentTool
	if cfg.EnableTask && !cfg.Plan {
		// The read-only plan stack never delegates: every registered child type
		// would be able to write, which Plan exists to prevent.
		registry := subagent.Registry{
			"explore": subagent.Definition{
				Description: "Read-only investigation: read files, grep, find, and list without changing anything.",
				Tools:       tools.ReadOnlyTools(cwd, tools.ToolsOptions{EnableWeb: cfg.EnableWeb, SearchURL: cfg.WebSearchURL}),
				Model:       cfg.SmolModel,
				StreamFn:    cfg.SmolStreamFn,
			},
			"general": subagent.Definition{
				Description: "Full coding task with the complete tool set.",
				Tools:       append(tools.CodingTools(cwd, tools.ToolsOptions{}), tools.ReadOnlyTools(cwd, tools.ToolsOptions{})...),
			},
		}
		taskTool = subagent.NewTool(subagent.Options{
			Model:            cfg.Model,
			StreamFn:         cfg.StreamFn,
			Registry:         registry,
			TransformContext: transformContext,
			PrepareNextTurn:  childCompact,
			BeforeToolCall:   beforeToolCall,
		})
	}
	var readResolver tools.ReadResourceResolver
	if len(cfg.Skills) > 0 {
		resolver, err := skills.NewResolver(cfg.Skills)
		if err != nil {
			return nil, err
		}
		readResolver = resolver
	}
	options := tools.ToolsOptions{
		EnableWeb:          cfg.EnableWeb,
		SearchURL:          cfg.WebSearchURL,
		ConfiguredTools:    cfg.ConfiguredTools,
		TaskTool:           taskTool,
		AttachmentRegistry: cfg.AttachmentRegistry,
		AttachmentReader:   cfg.AttachmentReader,
		Sanitize:           cfg.Sanitize,
		Skills:             cfg.Skills,
		ReadResolver:       readResolver,
	}
	allTools := BuildToolRegistry(cwd, options)
	var active []agent.AgentTool
	model := cfg.Model
	if cfg.Plan {
		if cfg.PlanModel.ID != "" {
			model = cfg.PlanModel
		}
		active = SelectActive(allTools, []string{"read", "grep", "find", "ls", "web_search", "web_fetch", "attachment"}, cfg.ExcludeTools, "")
	} else {
		active = SelectActive(allTools, nil, cfg.ExcludeTools, "")
	}

	appendPrompt := strings.Join(cfg.PromptAdditions, "\n\n")
	systemPrompt := RebuildSystemPrompt(RebuildSystemPromptOptions{
		ProductName:        cfg.ProductName,
		AppendSystemPrompt: appendPrompt,
		CustomPrompt:       cfg.CustomPrompt,
		PromptGuidelines:   cfg.PromptGuidelines,
		ContextFiles:       cfg.ContextFiles,
		Skills:             cfg.Skills,
		Rules:              cfg.Rules,
		GenericRules:       cfg.GenericRules,
		Personality:        cfg.Personality,
		Cwd:                cwd,
		ActiveTools:        active,
	})
	var agt *agent.Agent
	var compact func(agent.ShouldStopAfterTurnContext) *agent.AgentLoopTurnUpdate
	if runner := newCompactionRunner(model, cfg.StreamFn); runner != nil {
		compact = runner.hook(func(messages []ptypes.Message) { agt.SetMessages(messages) })
	}
	agt = agent.NewAgent(agent.AgentOptions{
		InitialState:               &agent.AgentState{SystemPrompt: systemPrompt, Model: model, Tools: active, Messages: cfg.InitialMessages},
		StreamFn:                   cfg.StreamFn,
		TransformContext:           transformContext,
		PrepareNextTurnWithContext: compact,
		BeforeToolCall:             beforeToolCall,
	})
	return &AgentStack{Agent: agt, Model: model, StreamFn: cfg.StreamFn, ProjectDir: cwd}, nil
}
