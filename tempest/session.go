package tempest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.harness.dev/harness/internal/adapter"
	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/config"
	"go.harness.dev/harness/internal/discovery"
	types "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/hub"
	"go.harness.dev/harness/internal/jobs"
	"go.harness.dev/harness/internal/mcpclient"
	"go.harness.dev/harness/internal/mcpconfig"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/session"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/work"
)

// Run is an admitted run. Wait may be called more than once.
type Run struct {
	cancel     context.CancelFunc
	done       chan struct{}
	started    time.Time
	result     RunResult
	err        error
	ready      chan struct{}
	readyOnce  sync.Once
	startID    string
	text       string
	last       Message
	finishedAt time.Time
}

// Wait returns the run's terminal result.
func (r *Run) Wait() (RunResult, error) {
	<-r.done
	return r.result, r.err
}

// Cancel cancels only this run, even after the session starts another one.
func (r *Run) Cancel() { r.cancel() }

// Text returns this run's last nonempty assistant text after completion.
func (r *Run) Text() string { <-r.done; return r.text }

// LastMessage returns this run's final transcript message after completion.
func (r *Run) LastMessage() Message { <-r.done; return r.last }

// FinishedAt returns this run's terminal event time, or zero if no terminal event published.
func (r *Run) FinishedAt() time.Time { <-r.done; return r.finishedAt }

// Started waits for the durable start record. Cancelling the wait doesn't cancel the run.
func (r *Run) Started(ctx context.Context) (string, error) {
	select {
	case <-r.ready:
		if r.startID != "" {
			return r.startID, nil
		}
		return "", r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

type approval struct {
	result chan approvalResult
	child  bool
}
type approvalResult struct {
	choice Choice
	err    error
}
type sessionCore struct {
	mu               sync.Mutex
	id               string
	agent            *agent.Agent
	store            *journal.Store
	hub              *hub.Hub
	model            types.Model
	budget           Budget
	mode             PermissionMode
	prompter         Prompter
	pending          map[string]*approval
	active           *Run
	closed           bool
	closeOnce        sync.Once
	closeErr         error
	detachJournal    func()
	detachHub        func()
	lastText         string
	runID            string
	delivery         *session.MediaDelivery
	mcp              *mcpclient.Manager
	mcpEnabled       bool
	permissionPolicy *permission.Policy
	baseTools        []Tool
	excludeTools     []string
	plan             bool
	baseMCPDefaults  map[string]permission.Decision
	queues           *session.DurableQueues
	jobs             *jobs.Manager
}

func open(ctx context.Context, o Options) (_ *Session, err error) {
	if (o.New == nil) == (o.Resume == nil) {
		return nil, errors.New("exactly one of New or Resume is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cwd := o.Cwd
	if cwd == "" {
		cwd, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return nil, err
	}
	var selectedMCP []MCPServerConfig
	if o.New != nil {
		selectedMCP = o.New.MCPServers
	} else {
		selectedMCP = o.Resume.MCPServers
	}
	var cfg StackConfig
	if o.Stack != nil {
		cfg = *o.Stack
	}
	cfg.Cwd = cwd
	needsDiscovery := cfg.ContextFiles == nil && cfg.Skills == nil && cfg.GenericRules == nil && cfg.Rules == nil
	dir := o.SessionDir
	var defaults *config.ResolvedConfig
	if dir == "" || o.Stack == nil || needsDiscovery || selectedMCP == nil {
		defaults, err = (config.Config{Flags: config.FlagValues{MCPConfig: config.StringInput{Set: selectedMCP != nil, Value: ""}}}).Resolve()
		if err != nil {
			return nil, err
		}
		if dir == "" {
			dir = defaults.SessionDir()
		}
		if o.Stack == nil {
			cfg = session.StackConfigFromConfig(defaults)
			cfg.Cwd = cwd
		}
	}
	if selectedMCP == nil && defaults != nil {
		selectedMCP = defaults.MCPServers()
	}
	selectedMCP, err = mcpconfig.Normalize(selectedMCP)
	if err != nil {
		return nil, err
	}
	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	var store *journal.Store
	var loaded *journal.Loaded
	if o.Resume != nil {
		path := o.Resume.Path
		if path == "" {
			if o.Resume.SessionID == "" {
				return nil, errors.New("resume needs SessionID or Path")
			}
			path, err = journal.Resolve(ctx, dir, cwd, o.Resume.SessionID)
			if err != nil {
				return nil, err
			}
		}
		store, loaded, err = journal.OpenForResume(ctx, path, journal.Options{})
		if err != nil {
			return nil, err
		}
		defer func() {
			if err != nil {
				_ = store.Close()
			}
		}()
		cwd = loaded.Header.Cwd
		cfg.Cwd = cwd
		cfg.InitialMessages = loaded.Messages
	}
	modelSpec := ""
	var budget Budget
	mode := cfg.PermissionMode
	if o.New != nil {
		modelSpec = o.New.Model
		budget = o.New.Budget
		if o.New.PermissionMode != "" {
			mode = o.New.PermissionMode
		}
		if o.New.SystemPrompt != "" {
			cfg.PromptAdditions = append(append([]string(nil), cfg.PromptAdditions...), o.New.SystemPrompt)
		}
		cfg.ConfiguredTools = append(append([]Tool(nil), cfg.ConfiguredTools...), o.New.Tools...)
	} else {
		modelSpec = o.Resume.Model
		budget = o.Resume.Budget
		if o.Resume.PermissionMode != "" {
			mode = o.Resume.PermissionMode
		}
		if modelSpec == "" && loaded != nil {
			modelSpec = loaded.Model
		}
	}
	if mode == "" && defaults != nil {
		mode = PermissionMode(defaults.PermissionModeDefault())
	}
	mode, err = permission.ParseMode(string(mode))
	if err != nil {
		return nil, err
	}
	if o.FauxScript != "" {
		cfg.Model, cfg.StreamFn, err = loadScript(o.FauxScript)
		if err != nil {
			return nil, err
		}
		cfg.PlanModel = cfg.Model
	}
	if cfg.StreamFn == nil {
		provider := o.Provider
		if defaults != nil {
			if provider.ModelID == "" {
				provider.ModelID = defaults.Model()
			}
			if provider.AnthropicBaseURL == "" {
				provider.AnthropicBaseURL = defaults.AnthropicBaseURL()
			}
			if provider.OpenAIBaseURL == "" {
				provider.OpenAIBaseURL = defaults.OpenAIBaseURL()
			}
			if provider.MaxTokens == 0 {
				provider.MaxTokens = defaults.MaxTokens()
			}
		}
		if modelSpec != "" {
			provider.ModelID = modelSpec
		}
		router, e := adapter.NewProviderRouter(provider)
		if e != nil {
			return nil, e
		}
		cfg.Model = router.Model
		cfg.StreamFn = router.StreamFn
	}
	runtimeModel := cfg.Model
	if cfg.Plan && cfg.PlanModel.ID != "" {
		runtimeModel = cfg.PlanModel
	}
	if err = budget.Validate(runtimeModel); err != nil {
		return nil, err
	}
	if cfg.SmolStreamFn == nil {
		cfg.SmolStreamFn = cfg.StreamFn
	}
	if cfg.SmolModel.ID == "" {
		cfg.SmolModel = cfg.Model
	}
	if needsDiscovery {
		skillPaths := cfg.SkillPaths
		if skillPaths == nil {
			skillPaths = defaults.SkillPaths()
		}
		inputs, e := discovery.Discover(ctx, discovery.Options{Cwd: cwd, AgentDir: defaults.AgentDir(), SkillPaths: skillPaths, IncludeDefaultSkills: true})
		if e != nil {
			return nil, e
		}
		cfg.ContextFiles = inputs.ContextFiles
		cfg.Skills = inputs.Skills
		cfg.GenericRules = inputs.GenericRules
		for _, d := range inputs.Diagnostics {
			logger.Warn("discovery diagnostic", "code", d.Code, "path", d.Path, "message", d.Message)
		}
	}
	if store == nil {
		store, err = journal.Create(ctx, dir, cwd, journal.Options{})
		if err != nil {
			return nil, err
		}
		defer func() {
			if err != nil {
				_ = store.Close()
			}
		}()
	}
	// Explicit model metadata can differ from the plan stack's runtime model.
	metadataModel := modelSpec
	if metadataModel == "" {
		metadataModel = runtimeModel.ID
	}
	if loaded == nil || loaded.Model != metadataModel || (loaded.Role != "" && loaded.Role != "default") {
		if err = store.AppendModelChange(ctx, metadataModel, "default"); err != nil {
			return nil, err
		}
	}
	c := &sessionCore{id: store.ID(), store: store, model: runtimeModel, budget: budget, mode: mode, prompter: o.Prompter, pending: make(map[string]*approval)}
	s := &Session{core: c}
	mapper := newMapper(c.id, runtimeModel)
	seed := Snapshot{SessionID: c.id, Cwd: cwd, Model: runtimeModel.ID, PermissionMode: string(mode), Budget: budget, Usage: mapper.usage}
	if loaded != nil {
		seed.Interrupted = loaded.Interrupted
		seed.Queues = append([]work.QueueItem(nil), loaded.Queued...)
		seed.Jobs = append([]work.Job(nil), loaded.Jobs...)
		if loaded.LastRun != nil {
			r := loaded.LastRun
			seed.LastRun = &RunSummary{RunID: r.RunID, StopReason: r.StopReason, Finished: r.Finished, Error: r.Error, FinishedAt: r.FinishedAt}
		}
		for _, d := range loaded.Diagnostics {
			seed.Diagnostics = append(seed.Diagnostics, d.Message)
			logger.Warn("session journal diagnostic", "message", d.Message)
		}
		for _, i := range loaded.Interrupted {
			logger.Warn("previous run left a tool call unfinished; its outcome is unknown", "tool", i.Tool, "tool_call_id", i.ToolCallID)
		}
	}
	c.hub = hub.New(seed, mapper.mapEvent)
	if loaded != nil {
		// Replay reconciled messages through the same mapping before any watcher exists.
		listener := c.hub.Listener()
		for _, msg := range loaded.Messages {
			_ = listener(ctx, agent.MessageEnd(msg))
			switch m := normalizeMessage(msg).(type) {
			case types.AssistantMessage:
				for _, b := range m.Content {
					if b.Type == types.BlockToolCall {
						_ = listener(ctx, agent.ToolExecutionStart(b.ID, b.Name, b.Arguments))
					}
				}
			case types.ToolResultMessage:
				_ = listener(ctx, agent.ToolExecutionEnd(m.ToolCallID, m.ToolName, m, m.IsError))
			}
		}
		mapper.usage.Total = loaded.UsageTotal
		mapper.usage.CostUnknown = loaded.UsageTotal.Cost.Unknown
		c.hub.Emit(update("tempest/usage", map[string]any{"model": runtimeModel.ID, "usage": Usage{}, "total": loaded.UsageTotal, "usageState": mapper.usage}, time.Time{}))
	}
	onMessagesChanged := cfg.OnMessagesChanged
	cfg.OnMessagesChanged = func(messages []types.Message) {
		c.hub.ReplaceHistory(func() []Update {
			savedUsage := mapper.usage
			var updates []Update
			for _, msg := range messages {
				updates = append(updates, mapper.mapEvent(agent.MessageEnd(msg))...)
			}
			mapper.usage = savedUsage
			updates = append(updates, update("tempest/usage", map[string]any{"usageState": savedUsage}, time.Time{}))
			return updates
		})
		if onMessagesChanged != nil {
			onMessagesChanged(messages)
		}
	}
	cfg.PermissionMode = mode
	cfg.PermissionModeFunc = func() permission.Mode { c.mu.Lock(); defer c.mu.Unlock(); return c.mode }
	// Copy caller-owned overrides: always choices belong only to this session.
	overrides := make(map[string]permission.Decision, len(cfg.PermissionPolicy.Overrides))
	for k, v := range cfg.PermissionPolicy.Overrides {
		overrides[k] = v
	}
	cfg.PermissionPolicy.Overrides = overrides
	if o.Prompter != nil {
		cfg.PermissionPolicy.Prompter = gatePrompter{s}
	}
	c.baseMCPDefaults = make(map[string]permission.Decision, len(cfg.PermissionPolicy.MCPDefaults))
	for name, decision := range cfg.PermissionPolicy.MCPDefaults {
		c.baseMCPDefaults[name] = decision
	}
	cfg.PermissionPolicy.MCPDefaults = make(map[string]permission.Decision)
	c.permissionPolicy = &cfg.PermissionPolicy
	c.mcp, err = mcpclient.Open(ctx, cwd, filepath.Join(dir, "mcp-output", c.id), selectedMCP, logger)
	if err != nil {
		return nil, err
	}
	c.mcpEnabled = len(selectedMCP) != 0
	defer func() {
		if err != nil {
			closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = c.mcp.Close(closeCtx)
		}
	}()
	c.installMCPDefaults()
	cfg.ConfiguredTools = append(append([]Tool(nil), cfg.ConfiguredTools...), c.mcp.Tools()...)
	cfg.PermissionPolicyRef = c.permissionPolicy
	c.excludeTools = append([]string(nil), cfg.ExcludeTools...)
	c.plan = cfg.Plan
	cfg.Journal, cfg.Loaded = store, loaded
	c.queues = session.NewDurableQueues(store, loaded, c.publishWork)
	cfg.QueueSource = c.queues
	var recoveries []work.Recovery
	if loaded != nil {
		recoveries = loaded.Recoveries
	}
	c.jobs, err = jobs.New(work.Host{
		SessionID: c.id, JournalPath: store.Path(), Cwd: cwd, Queues: c.queues,
		Append:  func(ctx context.Context, f work.Fact) error { return session.AppendWorkFact(ctx, store, f) },
		Enqueue: c.queues.Enqueue, Publish: c.publishWork,
		PrepareTask: func(ctx context.Context, spec work.TaskSpec) (work.TaskEnvironment, error) {
			return c.prepareTask(ctx, cfg, spec)
		},
		RunTask: func(ctx context.Context, launch work.TaskLaunch) (work.TaskResult, error) {
			return c.runTask(ctx, cfg, launch)
		},
	}, jobs.Options{BashTimeout: cfg.BashTimeout, OnError: func(error) { s.Cancel() }}, recoveries)
	if err != nil {
		return nil, err
	}
	cfg.Work = &sessionWork{core: c, recoveries: recoveries}
	cfg.Controller = c.jobs
	stack, e := session.BuildAgentStack(cfg)
	if e != nil {
		return nil, e
	}
	c.agent = stack.Agent
	mcpNames := make(map[string]bool)
	for _, tool := range c.mcp.Tools() {
		mcpNames[tool.Name] = true
	}
	for _, tool := range c.agent.State().Tools {
		if !mcpNames[tool.Name] {
			c.baseTools = append(c.baseTools, tool)
		}
	}
	c.agent.SetBudget(budget)
	if cfg.AttachmentReader != nil || cfg.AttachmentRegistry != nil {
		c.delivery = &session.MediaDelivery{Reader: cfg.AttachmentReader, Registry: cfg.AttachmentRegistry, ContextWindow: runtimeModel.ContextWindow, Sanitize: cfg.Sanitize}
	}
	c.detachJournal = session.AttachJournalWithOptions(c.agent, store, session.RecordOptions{Queues: c.queues, Started: func(id string) {
		state := c.agent.RunState()
		mapper.mu.Lock()
		mapper.runID = id
		mapper.logicalRunID = state.LogicalRunID
		mapper.continuationOf = state.ContinuationOf
		mapper.mu.Unlock()
		c.mu.Lock()
		c.runID = id
		c.mu.Unlock()
	}})
	c.detachHub = c.agent.Subscribe(func(ctx context.Context, ev agent.AgentEvent) error {
		if ev.Type == agent.EventAgentStart {
			// The journal listener runs first. Its append must succeed before readiness.
			c.mu.Lock()
			if r := c.active; r != nil {
				r.startID = c.runID
				r.readyOnce.Do(func() { close(r.ready) })
			}
			c.mu.Unlock()
		}
		if ev.Type == agent.EventAgentEnd {
			c.mu.Lock()
			if c.active != nil {
				c.active.finishedAt = ev.At
			}
			c.mu.Unlock()
		}
		if ev.Type == agent.EventMessageEnd {
			c.mu.Lock()
			if c.active != nil {
				c.active.last = ev.Message
			}
			c.mu.Unlock()
			if msg, ok := normalizeMessage(ev.Message).(types.AssistantMessage); ok {
				var parts []string
				for _, b := range msg.Content {
					if b.Type == types.BlockText {
						parts = append(parts, b.Text)
					}
				}
				text := strings.Join(parts, "\n")
				if text != "" {
					c.mu.Lock()
					c.lastText = text
					c.mu.Unlock()
				}
			}
		}
		return c.hub.Listener()(ctx, ev)
	})
	return s, nil
}
func (s *Session) begin(ctx context.Context, p *Prompt) (*Run, error) {
	c := s.core
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if c.active != nil {
		c.mu.Unlock()
		return nil, ErrRunActive
	}
	if p != nil && c.jobs.RecoveryRequired() {
		c.mu.Unlock()
		return nil, ErrRecoveryRequired
	}
	runCtx, cancel := context.WithCancel(ctx)
	r := &Run{cancel: cancel, done: make(chan struct{}), ready: make(chan struct{}), started: time.Now()}
	c.active = r
	c.lastText = ""
	c.runID = ""
	c.mu.Unlock()
	go s.execute(runCtx, p, r)
	return r, nil
}

func (s *Session) execute(runCtx context.Context, p *Prompt, r *Run) {
	c := s.core
	defer func() {
		r.cancel()
		c.mu.Lock()
		r.result.RunID = r.startID
		r.result.ElapsedMS = time.Since(r.started).Milliseconds()
		r.text = c.lastText
		if r.startID == "" && r.err == nil {
			r.err = errors.New("run ended before durable start")
		}
		r.readyOnce.Do(func() { close(r.ready) })
		c.active = nil
		closed := c.closed
		c.mu.Unlock()
		if closed {
			c.finishClose()
		}
		close(r.done)
	}()
	if c.mcpEnabled {
		if err := c.mcp.Prepare(runCtx); err != nil {
			r.err = err
			return
		}
		if err := runCtx.Err(); err != nil {
			r.err = err
			return
		}
		c.installMCPDefaults()
		c.agent.SetTools(c.selectedMCPTools())
	}
	if p == nil {
		r.result, r.err = c.agent.Continue(runCtx)
		return
	}
	if len(p.Media) == 0 {
		r.result, r.err = c.agent.PromptText(runCtx, p.Text)
		return
	}
	var delivery *session.MediaDelivery
	if c.delivery != nil {
		v := *c.delivery
		delivery = &v
	}
	r.result, r.err = c.agent.Prompt(runCtx, session.BuildUserMessage(p.Text, p.Media, delivery))
}
func (s *Session) cancel() {
	c := s.core
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != nil {
		c.active.cancel()
	}
}
func (s *Session) watch(ctx context.Context, o WatchOptions) (Snapshot, <-chan Update, func()) {
	var snap Snapshot
	var ch <-chan Update
	var stop func()
	if o.Replay {
		snap, ch, stop = s.core.hub.WatchReplay(o.Buffer)
	} else {
		snap, ch, stop = s.core.hub.Watch(o.Buffer)
	}
	stopContext := context.AfterFunc(ctx, stop)
	return snap, ch, func() { stopContext(); stop() }
}
func (s *Session) setBudget(b Budget) error {
	c := s.core
	if err := b.Validate(c.model); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	c.budget = b
	c.agent.SetBudget(b)
	c.hub.Configure(b, string(c.mode))
	return nil
}
func (s *Session) setPermissionMode(m PermissionMode) error {
	parsed, err := permission.ParseMode(string(m))
	if err != nil {
		return err
	}
	c := s.core
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	c.mode = parsed
	c.hub.Configure(c.budget, string(parsed))
	return nil
}
func (s *Session) close(ctx context.Context) error {
	c := s.core
	c.mu.Lock()
	c.closed = true
	r := c.active
	if r != nil {
		r.cancel()
	}
	c.mu.Unlock()
	if r != nil {
		select {
		case <-r.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.finishClose()
	return c.closeErr
}
func (c *sessionCore) finishClose() {
	c.closeOnce.Do(func() {
		c.closeErr = errors.Join(c.closeErr, c.jobs.Close(context.Background()))
		if c.detachHub != nil {
			c.detachHub()
		}
		if c.detachJournal != nil {
			c.detachJournal()
		}
		c.hub.Close()
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c.closeErr = errors.Join(c.closeErr, c.mcp.Close(closeCtx), c.store.Close())
	})
}

type gatePrompter struct{ session *Session }

func (g gatePrompter) Approve(ctx context.Context, req permission.PromptRequest) (permission.Choice, error) {
	s := g.session
	c := s.core
	title, kind := toolDescription(req.Tool, req.Args)
	ask := Ask{SessionID: c.id, ToolCallID: req.ToolCallID, Tool: req.Tool, Title: title, Kind: kind, RawInput: req.Args, Reason: req.Reason, Options: PermissionChoices()}
	ask.JobID, ask.ChildSessionID, ask.ChildToolCallID, ask.Cwd = req.JobID, req.ChildSessionID, req.ChildToolCallID, req.Cwd
	if req.ChildSessionID != "" {
		ask.Options = []Option{{OptionID: "allow_once", Name: "Allow once", Kind: "allow_once"}, {OptionID: "reject_once", Name: "Reject once", Kind: "reject_once"}}
		ask.Title = "Child " + req.ChildSessionID + " (" + req.Cwd + "): " + title
	}
	a := &approval{result: make(chan approvalResult, 1), child: req.ChildSessionID != ""}
	c.mu.Lock()
	if _, exists := c.pending[req.ToolCallID]; exists {
		c.mu.Unlock()
		return "", fmt.Errorf("duplicate pending tool call %q", req.ToolCallID)
	}
	c.pending[req.ToolCallID] = a
	c.hub.AddAsk(ask)
	c.mu.Unlock()
	promptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		choice, err := c.prompter.Approve(promptCtx, ask)
		s.resolveApproval(req.ToolCallID, a, approvalResult{choice: choice, err: err})
	}()
	select {
	case result := <-a.result:
		return result.choice, result.err
	case <-ctx.Done():
		s.resolveApproval(req.ToolCallID, a, approvalResult{err: ctx.Err()})
		result := <-a.result
		return result.choice, result.err
	}
}
func (s *Session) resolveApproval(id string, a *approval, result approvalResult) bool {
	c := s.core
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending[id] != a {
		return false
	}
	delete(c.pending, id)
	c.hub.RemoveAsk(id)
	a.result <- result
	return true
}
func (s *Session) answer(id string, choice Choice) error {
	switch choice {
	case "", permission.AllowOnce, permission.AllowAlways, permission.RejectOnce, permission.RejectAlways:
	default:
		return fmt.Errorf("unknown permission choice %q", choice)
	}
	c := s.core
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.pending[id]
	if a == nil {
		return ErrNoPendingAsk
	}
	if a.child && choice != permission.AllowOnce && choice != permission.RejectOnce && choice != "" {
		return errors.New("child approvals only allow once or reject once")
	}
	delete(c.pending, id)
	c.hub.RemoveAsk(id)
	a.result <- approvalResult{choice: choice}
	return nil
}
