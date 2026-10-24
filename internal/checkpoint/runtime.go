package checkpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/work"
)

// Runtime supplies the session's current transcript and replacement notification.
type Runtime struct {
	Messages       func() []types.Message
	Changed        func([]types.Message)
	Jobs           work.Controller
	Guard          func(context.Context) error
	Rewound        func()
	ApproveRestore func(context.Context, string, []string) error
}

// Bind attaches runtime access after the agent has been constructed.
func (m *Manager) Bind(runtime Runtime) { m.runtime = runtime }

func safeMessages(messages []types.Message) ([]types.Message, string) {
	for i := len(messages) - 1; i >= 0; i-- {
		var assistant types.AssistantMessage
		switch message := messages[i].(type) {
		case types.AssistantMessage:
			assistant = message
		case *types.AssistantMessage:
			if message != nil {
				assistant = *message
			}
		}
		for _, block := range assistant.Content {
			if block.Type == types.BlockToolCall {
				return messages[:i], fmt.Sprintf("%d:%s", assistant.Timestamp, block.ID)
			}
		}
		if messages[i].Role() == "user" {
			break
		}
	}
	return messages, ""
}

// CheckIdle refuses rewind while any owned job could change files or emit input.
func (m *Manager) CheckIdle(ctx context.Context) error {
	if m.runtime.Guard != nil {
		if err := m.runtime.Guard(ctx); err != nil {
			return err
		}
	}
	if m.runtime.Jobs == nil {
		return nil
	}
	cursor := ""
	for {
		page, err := m.runtime.Jobs.Jobs(ctx, cursor, 50)
		if err != nil {
			return err
		}
		for _, job := range page.Jobs {
			switch job.State {
			case "starting", "running", "recovering":
				return fmt.Errorf("rewind requires settled jobs and children; cancel and wait for %s first", job.ID)
			}
		}
		if page.NextCursor == "" {
			return nil
		}
		cursor = page.NextCursor
	}
}

// Tool builds the model-facing checkpoint or rewind definition.
func (m *Manager) Tool(name string) agent.AgentTool {
	schema := `{"type":"object","properties":{"goal":{"type":"string"}},"required":["goal"],"additionalProperties":false}`
	description := "Save files and conversation before an investigation. Returns an explicit checkpoint id and notes for skipped paths."
	if name == "rewind" {
		schema = `{"type":"object","properties":{"report":{"type":"string"},"checkpoint":{"type":"string"}},"required":["report"],"additionalProperties":false}`
		description = "Restore an explicit checkpoint and retain findings. Defaults to the newest explicit checkpoint (or automatic checkpoint if none). Refuses outside edits and capture gaps; settle jobs first. Only an operator can force outside edits."
	}
	return agent.AgentTool{Tool: types.Tool{Name: name, Description: description, Parameters: json.RawMessage(schema)}, Label: name, ExecutionMode: agent.ExecSequential, Execute: func(ctx context.Context, id string, args json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
		if m == nil || m.runtime.Messages == nil {
			return agent.AgentToolResult{}, fmt.Errorf("%s requires a durable session", name)
		}
		var parameters struct {
			Goal       string `json:"goal"`
			Report     string `json:"report"`
			Checkpoint string `json:"checkpoint"`
		}
		decoder := json.NewDecoder(strings.NewReader(string(args)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&parameters); err != nil {
			return agent.AgentToolResult{}, err
		}
		if name == "checkpoint" {
			if strings.TrimSpace(parameters.Goal) == "" {
				return agent.AgentToolResult{}, fmt.Errorf("goal cannot be empty")
			}
			messages, _ := safeMessages(m.runtime.Messages())
			info, err := m.Create(ctx, parameters.Goal, messages)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			encoded, err := json.Marshal(info)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			return agent.AgentToolResult{Content: []types.ContentBlock{types.NewText(string(encoded))}}, nil
		}
		if strings.TrimSpace(parameters.Report) == "" {
			return agent.AgentToolResult{}, fmt.Errorf("report cannot be empty")
		}
		if err := m.CheckIdle(ctx); err != nil {
			return agent.AgentToolResult{}, err
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.request != nil {
			return agent.AgentToolResult{}, fmt.Errorf("rewind already requested")
		}
		target, err := m.target(parameters.Checkpoint)
		if err != nil {
			return agent.AgentToolResult{}, err
		}
		if err := m.lock(func() error { _, err := m.validate(ctx, target, false); return err }); err != nil {
			return agent.AgentToolResult{}, err
		}
		m.state.RewindRequest = m.store.NewRunID()
		if err := persist(m, "state.json", m.state); err != nil {
			m.state.RewindRequest = ""
			return agent.AgentToolResult{}, err
		}
		m.request = &rewindRequest{id: target.ID, report: parameters.Report, callID: id}
		return agent.AgentToolResult{Content: []types.ContentBlock{types.NewText("Rewind requested; findings retained at the next turn boundary.")}}, nil
	}}
}

type rewindRequest struct{ id, report, callID string }

// MutatesFiles excludes orchestration and read tools from workspace capture.
// Unknown tools remain conservative: their implementation may write files.
func MutatesFiles(name string, readOnly bool) bool {
	if readOnly {
		return false
	}
	switch name {
	case "checkpoint", "rewind", "jobs", "cancel", "task", "message", "todo_write":
		return false
	default:
		return true
	}
}

// Wrap captures around tool calls without changing their results or holding a
// workspace lock while they run. Missing captures invalidate older rewind targets.
func (m *Manager) Wrap(tools []agent.AgentTool, mutates func(string) bool) []agent.AgentTool {
	out := append([]agent.AgentTool(nil), tools...)
	for i, tool := range out {
		if tool.Name == "checkpoint" || tool.Name == "rewind" || !mutates(tool.Name) {
			continue
		}
		execute := tool.Execute
		out[i].Execute = func(ctx context.Context, id string, args json.RawMessage, update agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			m.beforeCall(ctx, tool.Name, id)
			// A panic still runs afterCall. Keep Uncertain durable until we know
			// the tool returned; the executor owns panic-to-result conversion.
			returned := false
			defer func() { m.afterCall(ctx, returned) }()
			result, err := execute(ctx, id, args, update)
			returned = true
			return result, err
		}
	}
	return out
}

func (m *Manager) beforeCall(ctx context.Context, name, id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	captureContext, cancel := context.WithTimeout(ctx, captureTimeout)
	defer cancel()
	err := m.lock(func() error {
		messages, turn := safeMessages(m.runtime.Messages())
		if turn == "" {
			turn = id
		}
		if m.active == 0 {
			if m.turn != turn {
				if _, err := m.capture(captureContext, "before "+name, messages, true); err != nil {
					return err
				}
				m.turn = turn
			} else {
				before, err := m.scan(captureContext, m.state.Expected)
				if err != nil {
					return err
				}
				m.observeOutside(before)
			}
		}
		m.state.Uncertain = true
		return persist(m, "state.json", m.state)
	})
	if err != nil {
		m.gap(err.Error())
	}
	m.active++
}

func (m *Manager) afterCall(ctx context.Context, returned bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.active--
	if !returned {
		m.gap("mutating tool panicked before its outcome was captured")
		return
	}
	// A cancelled shell result still matters. Bound the bookkeeping independently
	// so cancellation doesn't strand the session or hold it indefinitely.
	captureContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), captureTimeout)
	defer cancel()
	err := m.lock(func() error {
		expected, err := m.scan(captureContext, m.state.Expected)
		if err != nil {
			return err
		}
		m.state.Expected = expected
		m.state.Uncertain = m.active > 0
		return persist(m, "state.json", m.state)
	})
	if err != nil {
		m.gap(err.Error())
	}
}

const captureTimeout = 10 * time.Second

// DiscardRequest abandons a deferred request when a run ends before Prepare.
func (m *Manager) DiscardRequest() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.request = nil
	if m.state.RewindRequest == "" {
		return
	}
	m.state.RewindRequest = ""
	if err := persist(m, "state.json", m.state); err != nil {
		slog.Warn("clearing abandoned rewind request", "error", err)
	}
}

// Prepare commits a rewind after the assistant batch's results are durable.
func (m *Manager) Prepare(ctx context.Context, c agent.ShouldStopAfterTurnContext) (*agent.AgentLoopTurnUpdate, error) {
	m.mu.Lock()
	request := m.request
	m.request = nil
	m.mu.Unlock()
	if request == nil {
		return nil, nil
	}
	err := m.CheckIdle(ctx)
	var messages []types.Message
	if err == nil && m.runtime.ApproveRestore != nil {
		var paths []string
		paths, err = m.restoreApprovalPaths(ctx, request.id)
		if err == nil {
			err = m.runtime.ApproveRestore(ctx, request.callID, paths)
		}
	}
	if err == nil {
		messages, err = m.Rewind(ctx, request.id, request.report, false)
	}
	if err != nil {
		m.DiscardRequest()
		// A refusal is input for the model, not a fatal run error or a phantom
		// interrupted request on the next resume.
		notice := types.UserMessage{Content: types.StringContent("Rewind refused; no investigation was discarded: " + err.Error()), Timestamp: time.Now().UnixMilli()}
		if appendErr := m.store.AppendMessage(context.WithoutCancel(ctx), notice); appendErr != nil {
			return nil, appendErr
		}
		messages = append(append([]types.Message(nil), c.Context.Messages...), notice)
	}
	m.runtime.Changed(messages)
	c.Context.Messages = messages
	return &agent.AgentLoopTurnUpdate{Context: &c.Context}, nil
}

func (m *Manager) restoreApprovalPaths(ctx context.Context, id string) (paths []string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	err = m.lock(func() error {
		target, err := m.target(id)
		if err != nil {
			return err
		}
		current, err := m.validate(ctx, target, false)
		if err != nil {
			return err
		}
		desired, err := m.desiredFiles(target, current)
		if err != nil {
			return err
		}
		paths = changedPaths(current, desired)
		return nil
	})
	// Approval can wait for a person. Never hold workspace locks while asking.
	return
}

// Install publishes an already committed rewind to the in-memory session.
func (m *Manager) Install(messages []types.Message) { m.runtime.Changed(messages) }
