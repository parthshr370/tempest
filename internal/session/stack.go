package session

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/checkpoint"
	"go.harness.dev/harness/internal/config"
	"go.harness.dev/harness/internal/document"
	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/evalkernel"
	"go.harness.dev/harness/internal/extensionhost"
	"go.harness.dev/harness/internal/hooks"
	"go.harness.dev/harness/internal/irc"
	"go.harness.dev/harness/internal/lsp"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/progress"
	"go.harness.dev/harness/internal/prompt"
	"go.harness.dev/harness/internal/refs"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/skills"
	"go.harness.dev/harness/internal/subagent"
	"go.harness.dev/harness/internal/toolio"
	"go.harness.dev/harness/internal/tools"
	"go.harness.dev/harness/internal/work"
	"go.harness.dev/harness/tempest/extension"
)

// PermissionModeFunc is a live-mode accessor called from the permission gate
// on every tool call. It exists so callers can update the mode per request
// without rebuilding the agent stack.
type PermissionModeFunc func() permission.Mode

// StackConfig is the recipe for building a generic agent stack. Optional tools
// and prompt additions are supplied by the caller without coupling the core to
// a host integration.
type StackConfig struct {
	Cwd          string
	Model        ptypes.Model
	PlanModel    ptypes.Model
	SmolModel    ptypes.Model
	StreamFn     agent.StreamFn
	SmolStreamFn agent.StreamFn
	// Reasoning is the default role's reasoning effort ("", "off", minimal,
	// low, medium, high). PlanReasoning and SmolReasoning override it for
	// the plan stack and smol (explore) children; empty inherits Reasoning.
	Reasoning        string
	PlanReasoning    string
	SmolReasoning    string
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
	// OnMessagesChanged observes compaction's replacement transcript.
	OnMessagesChanged func([]ptypes.Message)

	PermissionMode     permission.Mode
	PermissionModeFunc PermissionModeFunc
	PermissionPolicy   permission.Policy
	// PermissionPolicyRef shares live interactive choices with child admission.
	PermissionPolicyRef *permission.Policy
	ExcludeTools        []string
	EnableTask          bool
	EnableWeb           bool
	WebSearchURL        string
	// SkillPaths feeds per-session discovery in tempest.Open; nil uses the resolved config's paths.
	SkillPaths      []string
	ConfiguredTools []agent.AgentTool
	PromptAdditions []string
	BashTimeout     time.Duration
	// AdmitProcess holds the host slot for each actual subprocess lifetime.
	AdmitProcess func(context.Context) (func(), error)
	// Hooks are operator settings. Resolved hooks are inherited by task children.
	Hooks         hooks.Settings
	HooksResolved bool
	HookAgentType string
	// Work and QueueSource bind the session-owned durable runtime.
	Work        work.RunHooks
	Controller  work.Controller
	QueueSource work.QueueSource
	Journal     *journal.Store
	Loaded      *journal.Loaded
	// RootJournal is the root session's journal for a child; internal URLs
	// (local://, job://, history://) resolve against it. Empty uses Journal.
	RootJournal string
	Child       bool
	// RewindGuard prevents a shared-workspace child from undoing a live parent's files.
	RewindGuard func(context.Context) error
	// Messenger is this agent's endpoint on its session tree's message bus;
	// non-nil registers the message tool.
	Messenger      *irc.Endpoint
	AllowedTools   []string
	BeforeToolCall func(context.Context, agent.BeforeToolCallContext) *agent.BeforeToolCallResult
	Extensions     *extensionhost.Runtime
	ResolvePreset  func(string) (extension.AgentPreset, error)

	// Plan selects a read-only stack. Zero PlanModel uses Model.
	Plan bool
	// AttachmentRegistry + AttachmentReader wire the read-only attachment tool
	// over the session's resolved attachments; Sanitize cleans returned text.
	AttachmentRegistry *document.AttachmentRegistry
	AttachmentReader   *document.CacheRootBlobReader
	Sanitize           func(string) string

	// LSP is the resolved language-server configuration. tempest.Open
	// creates, owns and closes one manager for an enabled configuration on
	// the root session only; BuildAgentStack never starts one itself.
	LSP *lsp.Config
	// LSPManager serves the lsp_* tools and receives edit/write change
	// notifications. An embedder calling BuildAgentStack directly may
	// supply its own manager and owns its closure.
	LSPManager *lsp.Manager
	// MutationQueue serializes file mutations per real path. The LSP
	// workspace must read through the same queue; nil selects the tools'
	// process-wide default queue.
	MutationQueue *toolio.FileMutationQueue
}

// StackConfigFromConfig keeps config-backed tool options together across hosts.
func StackConfigFromConfig(cfg *config.ResolvedConfig) StackConfig {
	lspConfig := cfg.LSP()
	return StackConfig{
		Plan:             cfg.Plan(),
		Reasoning:        cfg.Reasoning(),
		PlanReasoning:    cfg.PlanReasoning(),
		SmolReasoning:    cfg.SmolReasoning(),
		EnableTask:       cfg.EnableTask(),
		EnableWeb:        cfg.EnableWeb(),
		WebSearchURL:     cfg.WebSearchURL(),
		SkillPaths:       cfg.SkillPaths(),
		BashTimeout:      cfg.BashTimeout(),
		Hooks:            cfg.Hooks(),
		LSP:              &lspConfig,
		PermissionPolicy: permission.Policy{CloudRules: cfg.CloudRules()},
	}
}

// AgentStack is the assembled result: a configured agent ready for Prompt/
// Continue calls.
type AgentStack struct {
	Agent       *agent.Agent
	Model       ptypes.Model
	StreamFn    agent.StreamFn
	ProjectDir  string
	Checkpoints *checkpoint.Manager
	kernels     *evalkernel.Manager
}

// Close stops this stack's kernels and runs its session-end hooks.
func (s *AgentStack) Close() error {
	return errors.Join(s.kernels.Close(), s.Agent.EndSessionHooks(context.Background()))
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
	var err error
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return nil, err
	}
	if !cfg.HooksResolved && !cfg.Child {
		var skipped []string
		cfg.Hooks, skipped, err = hooks.ResolveProject(cfg.Hooks, cwd)
		if err != nil {
			return nil, err
		}
		for _, path := range skipped {
			slog.Warn("project hooks disabled: explicit trust required", "path", path)
		}
	}
	modeFunc := cfg.PermissionModeFunc
	if modeFunc == nil {
		mode := cfg.PermissionMode
		modeFunc = func() permission.Mode { return mode }
	}
	policy := cfg.PermissionPolicyRef
	if policy == nil {
		policy = &cfg.PermissionPolicy
	}

	beforeToolCall := func(ctx context.Context, c agent.BeforeToolCallContext) *agent.BeforeToolCallResult {
		rawArgs, err := json.Marshal(c.Args)
		if err != nil {
			rawArgs = nil
		}
		outcome := permission.GateDefinition(ctx, modeFunc(), c.ToolCall.Name, c.Definition.Identity, c.Definition.Effect, rawArgs, policy, cwd, c.ToolCall.ID)
		return &agent.BeforeToolCallResult{Block: !outcome.Allow, Reason: outcome.Reason, Permission: &agent.PermissionOutcome{Allow: outcome.Allow, Reason: outcome.Reason, Source: string(outcome.Source), Prompted: outcome.Prompted, Wait: outcome.Wait}}
	}
	if cfg.BeforeToolCall != nil {
		beforeToolCall = cfg.BeforeToolCall
	}

	// The session data directory holds tool-output spills so read can open
	// them. The journal id is the session id providers key their prompt
	// cache on.
	sessionDataDir, sessionID := "", ""
	if cfg.Journal != nil {
		sessionDataDir = cfg.Journal.Path() + ".d"
		sessionID = cfg.Journal.ID()
	}
	hookInput := hooks.Input{Cwd: cwd, SessionID: sessionID}
	if cfg.Journal != nil {
		hookInput.TranscriptPath = cfg.Journal.Path()
	}
	if cfg.Child {
		hookInput.AgentID, hookInput.AgentType = sessionID, cfg.HookAgentType
		hookInput.AgentTranscriptPath = hookInput.TranscriptPath
		if cfg.RootJournal != "" {
			hookInput.TranscriptPath = cfg.RootJournal
		}
	}
	hookRunner, err := hooks.New(cfg.Hooks, hookInput)
	if err != nil {
		return nil, err
	}
	if hookRunner != nil {
		hookRunner.AdmitProcess = cfg.AdmitProcess
	}
	if cfg.Loaded != nil {
		hookRunner.MarkResumed()
	}
	approveHookSettings := func(ctx context.Context, tool, callID string, paths []string) error {
		ctx = hooks.WithNotification(ctx, func(notifyCtx context.Context, reason string) error {
			_, err := hookRunner.Run(notifyCtx, hooks.Input{Event: "Notification", NotificationType: "permission_prompt", Message: reason, ToolName: tool, ToolUseID: callID})
			return err
		})
		outcome := permission.ApproveHookSettings(ctx, policy, tool, callID, cwd, paths)
		if !outcome.Allow {
			return errors.New(outcome.Reason)
		}
		return nil
	}
	var taskTool agent.AgentTool
	if cfg.EnableTask && !cfg.Plan && !cfg.Child {
		taskTool = subagent.NewTool(cfg.Controller)
		if cfg.Extensions != nil {
			taskTool = subagent.NewPresetTool(cfg.Controller, nil, cfg.Extensions.ResolvePresetContext)
		}
	}
	// Internal URLs resolve against the root session, so a child shares
	// its root's local:// directory, jobs and child transcripts.
	rootJournal := cfg.RootJournal
	if rootJournal == "" && cfg.Journal != nil {
		rootJournal = cfg.Journal.Path()
	}
	refsResolver, err := refs.New(refs.Config{Skills: cfg.Skills, RootJournal: rootJournal})
	if err != nil {
		return nil, err
	}
	if cfg.MutationQueue == nil {
		cfg.MutationQueue = tools.DefaultMutationQueue()
	}
	var checkpoints *checkpoint.Manager
	if cfg.Journal != nil {
		checkpoints, err = checkpoint.New(context.Background(), cwd, cfg.Journal, cfg.MutationQueue)
		if err != nil {
			return nil, err
		}
		if messages, recovered := checkpoints.Recovered(); recovered {
			cfg.InitialMessages = messages
		}
	}
	lostKernels := map[string]bool{}
	if cfg.Loaded != nil {
		lostKernels = cfg.Loaded.EvalLanguages
	}
	kernels := evalkernel.New(cwd, tools.ToolOutputDir(sessionDataDir), lostKernels, cfg.AdmitProcess)
	options := tools.ToolsOptions{
		MutationQueue:      cfg.MutationQueue,
		EnableWeb:          cfg.EnableWeb,
		BashTimeout:        cfg.BashTimeout,
		AdmitProcess:       cfg.AdmitProcess,
		SearchURL:          cfg.WebSearchURL,
		ConfiguredTools:    cfg.ConfiguredTools,
		TaskTool:           taskTool,
		AttachmentRegistry: cfg.AttachmentRegistry,
		AttachmentReader:   cfg.AttachmentReader,
		Sanitize:           cfg.Sanitize,
		Skills:             cfg.Skills,
		Refs:               refsResolver,
		Work:               cfg.Controller,
		Child:              cfg.Child,
		Messenger:          cfg.Messenger,
		SessionDataDir:     sessionDataDir,
		Checkpoints:        checkpoints,
		Kernels:            kernels,
		ApproveHookSettings: func(ctx context.Context, callID string, paths []string) error {
			return approveHookSettings(ctx, "integrate", callID, paths)
		},
	}
	// Language servers are root-session-only: a child edits another
	// worktree, whose changes must not reach the parent's documents.
	if cfg.LSPManager != nil && !cfg.Child {
		options.LSP = cfg.LSPManager
		options.OnFileChanged = cfg.LSPManager.Changed
	}
	model := cfg.Model
	reasoning := cfg.Reasoning
	if cfg.Plan {
		if cfg.PlanModel.ID != "" {
			model = cfg.PlanModel
		}
		if cfg.PlanReasoning != "" {
			reasoning = cfg.PlanReasoning
		}
	}
	options.ImageInput = model.SupportsVision()
	allTools := BuildToolRegistry(cwd, options)
	if checkpoints == nil {
		delete(allTools, tools.CheckpointTool)
		delete(allTools, tools.RewindTool)
	}
	if cfg.RewindGuard != nil {
		delete(allTools, tools.RewindTool)
	}
	var active []agent.AgentTool
	if cfg.Plan {
		active = SelectActive(allTools, PlanToolNames(), cfg.ExcludeTools, "")
		if cfg.AllowedTools != nil {
			active = slices.DeleteFunc(active, func(tool agent.AgentTool) bool {
				return !slices.Contains(cfg.AllowedTools, tool.Name)
			})
		}
	} else {
		active = SelectActive(allTools, cfg.AllowedTools, cfg.ExcludeTools, "")
	}
	if cfg.Child && cfg.AllowedTools == nil {
		active = nil
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
		ProgressPath:       progress.Path(sessionDataDir),
		ActiveTools:        active,
	})
	var agt *agent.Agent
	var compact func(context.Context, agent.ShouldStopAfterTurnContext) (*agent.AgentLoopTurnUpdate, error)
	if runner := newCompactionRunner(model, cfg.StreamFn); runner != nil {
		runner.hooks = hookRunner
		if cfg.Extensions != nil {
			runner.beforeCompaction = cfg.Extensions.BeforeCompactionSource
		}
		compact = runner.hookE(func(messages []ptypes.Message) {
			agt.SetMessages(messages)
			if cfg.OnMessagesChanged != nil {
				cfg.OnMessagesChanged(messages)
			}
		})
		defer func() { runner.configureJournal(cfg.Journal, cfg.Loaded, agt) }()
	}
	if checkpoints != nil {
		active = checkpoints.Wrap(active, func(name string) bool {
			return checkpoint.MutatesFiles(name, permission.ClassifyTool(name) == permission.TierRead)
		})
		previous := compact
		compact = func(ctx context.Context, c agent.ShouldStopAfterTurnContext) (*agent.AgentLoopTurnUpdate, error) {
			if update, err := checkpoints.Prepare(ctx, c); update != nil || err != nil {
				return update, err
			}
			if previous != nil {
				return previous(ctx, c)
			}
			return nil, nil
		}
	}
	var extensionRuntime agent.ExtensionRuntime
	if cfg.Extensions != nil {
		cfg.Extensions.BasePrompt = systemPrompt
		cfg.Extensions.BaseTools = append([]agent.AgentTool(nil), active...)
		cfg.Extensions.CommandHooks = len(cfg.Hooks.Hooks) != 0
		extensionRuntime = cfg.Extensions
		cfg.Extensions.AllowTool = func(name string, effect extension.EffectClass) bool {
			if slices.Contains(cfg.ExcludeTools, name) || cfg.Plan && effect != extension.EffectRead {
				return false
			}
			return !cfg.Child && cfg.AllowedTools == nil || slices.Contains(cfg.AllowedTools, name)
		}
		cfg.Extensions.ExecuteTool = func(ctx context.Context, tools []agent.AgentTool, call ptypes.ContentBlock) (agent.AgentToolResult, error) {
			return agent.ExecuteScopedTool(ctx, tools, call, agent.AgentLoopConfig{Hooks: hookRunner, Extensions: extensionRuntime, BeforeToolCall: beforeToolCall, Child: cfg.Child})
		}
	}
	agt = agent.NewAgent(agent.AgentOptions{
		InitialState:     &agent.AgentState{SystemPrompt: systemPrompt, Model: model, ThinkingLevel: thinkingLevel(reasoning), Tools: active, Messages: cfg.InitialMessages},
		StreamFn:         cfg.StreamFn,
		PrepareNextTurnE: compact,
		QueueSource:      cfg.QueueSource,
		Work:             cfg.Work,
		BeforeToolCall:   beforeToolCall,
		SessionID:        sessionID,
		Hooks:            hookRunner,
		Extensions:       extensionRuntime,
		Child:            cfg.Child,
	})
	if hookRunner != nil {
		hookRunner.PermissionMode = func() string { return string(modeFunc()) }
		hookRunner.Once = newHookCache(cfg.Journal, cfg.Loaded, agt).run
	}
	if checkpoints != nil {
		checkpoints.Bind(checkpoint.Runtime{
			Messages: func() []ptypes.Message { return agt.State().Messages },
			Changed: func(messages []ptypes.Message) {
				agt.SetMessages(messages)
				if cfg.OnMessagesChanged != nil {
					cfg.OnMessagesChanged(messages)
				}
			},
			Jobs:    cfg.Controller,
			Guard:   cfg.RewindGuard,
			Rewound: kernels.MarkStateLost,
			ApproveRestore: func(ctx context.Context, callID string, paths []string) error {
				return approveHookSettings(ctx, "rewind", callID, paths)
			},
		})
		agt.Subscribe(func(_ context.Context, event agent.AgentEvent) error {
			if event.Type == agent.EventAgentEnd {
				checkpoints.DiscardRequest()
			}
			return nil
		})
	}
	if checkpoints != nil {
		if messages, recovered := checkpoints.Recovered(); recovered {
			checkpoints.Install(messages)
		}
	}
	return &AgentStack{Agent: agt, Model: model, StreamFn: cfg.StreamFn, ProjectDir: cwd, Checkpoints: checkpoints, kernels: kernels}, nil
}

// thinkingLevel maps a configured reasoning effort to the agent's level;
// unset and "off" both mean no reasoning field.
func thinkingLevel(reasoning string) agent.ThinkingLevel {
	if reasoning == "" {
		return agent.ThinkingOff
	}
	return agent.ThinkingLevel(reasoning)
}

// PlanToolNames is the read-only plan stack's selection. Tools that are not
// registered (web access off, LSP disabled) are skipped by SelectActive.
func PlanToolNames() []string {
	names := []string{"read", "grep", "find", "ls", "web_search", "web_fetch", "attachment", string(tools.ViewImageTool), string(tools.ManifestDiffTool)}
	for _, name := range tools.LSPToolNames {
		names = append(names, string(name))
	}
	return names
}
