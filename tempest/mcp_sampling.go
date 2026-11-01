package tempest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.harness.dev/harness/internal/agent"
	types "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/mcpclient"
	"go.harness.dev/harness/internal/permission"
)

// mcpSamplingEnabled reports whether any selected server opted into
// sampling, the only case in which the capability is declared.
func mcpSamplingEnabled(servers []MCPServerConfig) bool {
	for _, server := range servers {
		if server.Sampling {
			return true
		}
	}
	return false
}

// mcpSampling answers sampling/createMessage for the session's opted-in
// MCP servers: one model call on the sampling model (the smol role unless
// mcp_sampling_model overrides it), approved by the user on first use per
// server, charged to the live run and journaled as
// mcp-sampling:<server>:<id>.
type mcpSampling struct {
	session       *Session
	model         types.Model
	stream        agent.StreamFn
	reasoning     string
	maxTokens     int
	allowHeadless bool
	// agent is set once the session's agent exists; earlier requests are
	// refused.
	agent atomic.Pointer[agent.Agent]
	mu    sync.Mutex
	gates map[string]*samplingGate
}

// samplingGate serializes one server's approvals, so concurrent first
// requests ask once, and remembers an always-choice for the session.
type samplingGate struct {
	mu      sync.Mutex
	decided bool
	allow   bool
}

func newMCPSampling(s *Session, cfg StackConfig) *mcpSampling {
	reasoning := cfg.SmolReasoning
	if reasoning == "" {
		reasoning = cfg.Reasoning
	}
	if reasoning == string(agent.ThinkingOff) {
		reasoning = ""
	}
	maxTokens := cfg.MCP.Sampling.MaxTokens
	if maxTokens <= 0 {
		maxTokens = cfg.MCP.Sampling.Model.MaxTokens
	}
	return &mcpSampling{session: s, model: cfg.MCP.Sampling.Model, stream: cfg.MCP.Sampling.StreamFn, reasoning: reasoning, maxTokens: maxTokens, allowHeadless: cfg.MCP.Sampling.AllowHeadless, gates: map[string]*samplingGate{}}
}

func (m *mcpSampling) sample(ctx context.Context, request mcpclient.SamplingRequest) (types.AssistantMessage, error) {
	agt := m.agent.Load()
	if agt == nil {
		return types.AssistantMessage{}, fmt.Errorf("%w: the session is still starting", mcpclient.ErrSamplingRefused)
	}
	// The spend belongs to the live run. The request lives no longer than
	// that run, which waits for its receipt before it ends, so the spend
	// is journaled and charged (and held to -max-cost) under it even when
	// the server asked without awaiting the answer in its tool call.
	runCtx, _, done, ok := agt.JoinRun(ctx)
	defer done()
	if !ok {
		return types.AssistantMessage{}, fmt.Errorf("%w: MCP sampling is answered only while a run is in progress", mcpclient.ErrSamplingRefused)
	}
	maxTokens := request.MaxTokens
	if m.maxTokens > 0 && maxTokens > m.maxTokens {
		maxTokens = m.maxTokens
	}
	if err := m.approve(runCtx, request, maxTokens); err != nil {
		return types.AssistantMessage{}, err
	}
	if !m.model.SupportsVision() && hasImage(request.Messages) {
		return types.AssistantMessage{}, fmt.Errorf("sampling model %s does not accept images", m.model.ID)
	}
	release, err := agent.AdmitProvider(runCtx, m.model)
	defer release()
	if err != nil {
		return types.AssistantMessage{}, err
	}
	options := &types.SimpleStreamOptions{Reasoning: m.reasoning}
	options.MaxTokens = maxTokens
	streamCtx, cancel := context.WithCancel(runCtx)
	defer cancel()
	response := m.stream(streamCtx, m.model, types.Context{SystemPrompt: request.SystemPrompt, Messages: request.Messages}, options)
	if response == nil {
		return types.AssistantMessage{}, errors.New("sampling model returned no stream")
	}
	// Attempts the retry layer or a fallback switch abandoned can still be
	// billed. The receipt names the model that answered: a backup after a
	// switch.
	var usage types.Usage
	answered := m.model.ID
	for event := range response.Events() {
		if (event.Type == types.EvRetry || event.Type == types.EvFallback) && event.Err != nil {
			usage.Add(event.Err.Usage)
		}
		if event.Type == types.EvFallback && event.Fallback != nil {
			answered = event.Fallback.To.ID
		}
	}
	final, err := response.Result(context.WithoutCancel(runCtx))
	if final != nil {
		usage.Add(final.Usage)
	}
	// The receipt is journaled and charged even when the call failed.
	if importErr := agt.ImportUsage(context.WithoutCancel(runCtx), request.Source(), answered, usage); importErr != nil {
		return types.AssistantMessage{}, importErr
	}
	if err != nil {
		return types.AssistantMessage{}, err
	}
	if final == nil {
		return types.AssistantMessage{}, errors.New("sampling model returned no message")
	}
	if final.StopReason == types.StopError || final.StopReason == types.StopAborted {
		reason := final.ErrorMessage
		if reason == "" {
			reason = string(final.StopReason)
		}
		return types.AssistantMessage{}, errors.New("sampling model failed: " + reason)
	}
	if final.Model == "" {
		final.Model = answered
	}
	return *final, nil
}

// approve asks the user on a server's first request. Allow always and
// reject always hold for the rest of the session; once-choices ask again
// next time. Without a user (no prompter, or serve with no approving client)
// the request is refused at once unless mcp_sampling_allow_headless is set.
func (m *mcpSampling) approve(ctx context.Context, request mcpclient.SamplingRequest, maxTokens int) error {
	m.mu.Lock()
	gate := m.gates[request.Server]
	if gate == nil {
		gate = &samplingGate{}
		m.gates[request.Server] = gate
	}
	m.mu.Unlock()
	gate.mu.Lock()
	defer gate.mu.Unlock()
	refused := fmt.Errorf("%w: the user declined sampling for MCP server %q", mcpclient.ErrSamplingRefused, request.Server)
	if gate.decided {
		if gate.allow {
			return nil
		}
		return refused
	}
	core := m.session.core
	if !attended(core.prompter, core.id) {
		if m.allowHeadless {
			return nil
		}
		return fmt.Errorf("%w: MCP server %q asked to use the model and no user can approve it; set mcp_sampling_allow_headless to allow it", mcpclient.ErrSamplingRefused, request.Server)
	}
	input, err := json.Marshal(samplingInput(request, m.model.ID, maxTokens))
	if err != nil {
		return err
	}
	title := fmt.Sprintf("MCP server %s asks to use %s (sampling, up to %d tokens)", request.Server, m.model.ID, maxTokens)
	choice, err := m.session.ask(ctx, Ask{SessionID: core.id, ToolCallID: request.Source(), Tool: "mcp_sampling", Title: title, Kind: "other", RawInput: input, Options: PermissionChoices()})
	allowed := err == nil && (choice == permission.AllowOnce || choice == permission.AllowAlways)
	// Close the approval card the pending ask opened.
	status := "failed"
	if allowed {
		status = "completed"
	}
	core.hub.Emit(update("tool_call_update", map[string]any{"toolCallId": request.Source(), "status": status}, time.Now()))
	if err != nil {
		return err
	}
	switch choice {
	case permission.AllowAlways:
		gate.decided, gate.allow = true, true
	case permission.RejectAlways:
		gate.decided = true
	}
	if !allowed {
		return refused
	}
	return nil
}

// attended reports whether a user can answer an approval now: a prompter
// exists and, when it can tell, someone who approves is attached.
func attended(prompter Prompter, sessionID string) bool {
	if prompter == nil {
		return false
	}
	if attendance, ok := prompter.(Attendance); ok {
		return attendance.Attended(sessionID)
	}
	return true
}

// samplingInput is what the approval shows: the server, the model, the
// capped limit and the messages the model would receive.
func samplingInput(request mcpclient.SamplingRequest, model string, maxTokens int) map[string]any {
	type message struct {
		Role string `json:"role"`
		Text string `json:"text"`
	}
	messages := make([]message, 0, len(request.Messages))
	for _, raw := range request.Messages {
		var blocks []types.ContentBlock
		role := "user"
		switch m := raw.(type) {
		case types.UserMessage:
			blocks = m.Content.Blocks
		case types.AssistantMessage:
			role, blocks = "assistant", m.Content
		}
		text := ""
		for _, block := range blocks {
			if block.Type == types.BlockImage {
				text += "[image]"
				continue
			}
			text += block.Text
		}
		messages = append(messages, message{Role: role, Text: text})
	}
	return map[string]any{"server": request.Server, "model": model, "maxTokens": maxTokens, "systemPrompt": request.SystemPrompt, "messages": messages}
}

func hasImage(messages []types.Message) bool {
	for _, raw := range messages {
		if user, ok := raw.(types.UserMessage); ok {
			for _, block := range user.Content.Blocks {
				if block.Type == types.BlockImage {
					return true
				}
			}
		}
	}
	return false
}
