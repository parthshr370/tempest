package tempest

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.harness.dev/harness/internal/agent"
	types "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/hub"
	"go.harness.dev/harness/internal/work"
)

type eventMapper struct {
	mu             sync.Mutex
	logicalRunID   string
	continuationOf string
	sessionID      string
	model          types.Model
	counter        int
	messageID      string
	text           string
	thought        string
	runID          string
	started        time.Time
	turn           int
	usage          UsageState
	// announced holds calls that already emitted tool_call; denied holds calls the gate refused.
	announced map[string]bool
	denied    map[string]bool
	childJobs map[string]string
}

func newMapper(id string, model types.Model) *eventMapper {
	return &eventMapper{sessionID: id, model: model, announced: make(map[string]bool), denied: make(map[string]bool), childJobs: make(map[string]string), usage: UsageState{ContextSize: model.ContextWindow}}
}
func (m *eventMapper) nextMessage() string { m.counter++; return fmt.Sprintf("msg_%d", m.counter) }

// announce emits the tool_call that must precede every update for the call.
func (m *eventMapper) announce(ev agent.AgentEvent, status string, at time.Time) Update {
	input := rawInput(ev.Args)
	title, kind := toolDescription(ev.ToolName, input)
	return update("tool_call", map[string]any{"toolCallId": ev.ToolCallID, "status": status, "title": title, "kind": kind, "rawInput": input}, at)
}
func update(kind string, p map[string]any, at time.Time) Update {
	if !strings.HasPrefix(kind, "tempest/") {
		p["sessionUpdate"] = kind
	}
	raw, err := json.Marshal(p)
	if err != nil {
		panic(fmt.Sprintf("invalid mapped %s event: %v", kind, err))
	}
	return Update{At: at, Kind: kind, Update: raw}
}
func (m *eventMapper) chunk(kind, text string, at time.Time) Update {
	return update(kind, map[string]any{"messageId": m.messageID, "content": map[string]any{"type": "text", "text": text}}, at)
}

// mapEvent is the only agent-to-public-event conversion, shared by all surfaces.
func (m *eventMapper) mapEvent(ev agent.AgentEvent) []Update {
	m.mu.Lock()
	defer m.mu.Unlock()
	at := ev.At
	switch ev.Type {
	case agent.EventWork:
		if ev.Work == nil {
			return nil
		}
		if ev.Work.Job != nil && ev.Work.Job.ChildSessionID != "" {
			m.childJobs[ev.Work.Job.ChildSessionID] = ev.Work.Job.ID
		}
		return []Update{mapWorkUpdate(*ev.Work, at)}
	case agent.EventAgentStart:
		m.started = at
		m.turn = 0
		clear(m.announced)
		clear(m.denied)
		return []Update{update("tempest/run", map[string]any{"status": "started", "runId": m.runID, "logicalRunId": m.logicalRunID, "continuationOf": m.continuationOf, "model": m.model.ID}, at)}
	case agent.EventTurnStart, agent.EventTurnEnd:
		phase := "end"
		if ev.Type == agent.EventTurnStart {
			phase = "start"
			m.turn++
		}
		return []Update{update("tempest/turn", map[string]any{"index": m.turn, "phase": phase}, at)}
	case agent.EventMessageStart:
		if ev.Message != nil && ev.Message.Role() == "assistant" {
			m.messageID = m.nextMessage()
			m.text = ""
			m.thought = ""
		}
	case agent.EventMessageUpdate:
		if ev.AssistantMessageEvent == nil {
			return nil
		}
		if m.messageID == "" {
			m.messageID = m.nextMessage()
		}
		e := ev.AssistantMessageEvent
		switch e.Type {
		case types.EvTextDelta:
			m.text += e.Delta
			return []Update{m.chunk("agent_message_chunk", e.Delta, at)}
		case types.EvThinkingDelta:
			m.thought += e.Delta
			return []Update{m.chunk("agent_thought_chunk", e.Delta, at)}
		}
	case agent.EventMessageEnd:
		switch msg := normalizeMessage(ev.Message).(type) {
		case types.UserMessage:
			id := m.nextMessage()
			var out []Update
			blocks := msg.Content.Blocks
			if blocks == nil {
				blocks = []types.ContentBlock{types.NewText(msg.Content.Text)}
			}
			for _, b := range blocks {
				out = append(out, update("user_message_chunk", map[string]any{"messageId": id, "content": b}, at))
			}
			if ev.QueueID != "" {
				// The consumed text is already the user message above; send only the id.
				out = append(out, update("tempest/queue", map[string]any{"action": "consumed", "item": map[string]any{"id": ev.QueueID}}, at))
			}
			return out
		case types.AssistantMessage:
			if m.messageID == "" {
				m.messageID = m.nextMessage()
			}
			var out []Update
			var text, thought strings.Builder
			for _, b := range msg.Content {
				switch b.Type {
				case types.BlockText:
					text.WriteString(b.Text)
				case types.BlockThinking:
					thought.WriteString(b.Thinking)
				}
			}
			// Non-streaming providers and resumed messages still use the same chunks.
			if strings.HasPrefix(text.String(), m.text) && len(text.String()) > len(m.text) {
				out = append(out, m.chunk("agent_message_chunk", text.String()[len(m.text):], at))
			}
			if strings.HasPrefix(thought.String(), m.thought) && len(thought.String()) > len(m.thought) {
				out = append(out, m.chunk("agent_thought_chunk", thought.String()[len(m.thought):], at))
			}
			out = append(out, update("tempest/message_end", map[string]any{"messageId": m.messageID, "stopReason": msg.StopReason}, at))
			m.messageID = ""
			m.text = ""
			m.thought = ""
			m.usage.Total.Add(msg.Usage)
			m.usage.LastCall = msg.Usage
			m.usage.ContextUsed = msg.Usage.Input + msg.Usage.CacheRead + msg.Usage.CacheWrite
			m.usage.CostUnknown = m.usage.Total.Cost.Unknown
			p := map[string]any{"used": m.usage.ContextUsed, "size": m.model.ContextWindow, "_meta": map[string]any{"tempest": map[string]any{"usageState": m.usage}}}
			if !m.usage.Total.Cost.Unknown {
				p["cost"] = map[string]any{"amount": m.usage.Total.Cost.Total, "currency": "USD"}
			}
			out = append(out, update("usage_update", p, at))
			return out
		}
	case agent.EventUsage:
		m.usage.Total.Add(ev.Usage)
		m.usage.CostUnknown = m.usage.Total.Cost.Unknown
		payload := struct {
			Model          string     `json:"model"`
			Usage          Usage      `json:"usage"`
			Total          Usage      `json:"total"`
			UsageState     UsageState `json:"usageState"`
			SourceRef      string     `json:"sourceRef,omitempty"`
			ChildSessionID string     `json:"childSessionId,omitempty"`
			JobID          string     `json:"jobId,omitempty"`
		}{ev.Model, ev.Usage, m.usage.Total, m.usage, ev.UsageSource, ev.ChildSessionID, m.childJobs[ev.ChildSessionID]}
		raw, err := json.Marshal(payload)
		if err != nil {
			panic(fmt.Sprintf("invalid usage event: %v", err))
		}
		return []Update{{At: at, Kind: "tempest/usage", Update: raw}}
	case agent.EventToolPermission:
		if ev.Permission == nil {
			return nil
		}
		p := ev.Permission
		var out []Update
		// A prompted call was already announced by the hub's pending ask.
		if !p.Prompted && !m.announced[ev.ToolCallID] {
			out = append(out, m.announce(ev, "pending", at))
		}
		m.announced[ev.ToolCallID] = true
		decision := "deny"
		status := "failed"
		if p.Allow {
			decision = "allow"
			status = "pending"
		} else {
			m.denied[ev.ToolCallID] = true
		}
		return append(out, update("tool_call_update", map[string]any{"toolCallId": ev.ToolCallID, "status": status, "_meta": map[string]any{"tempest": map[string]any{"permission": map[string]any{"decision": decision, "source": p.Source, "waitMs": p.Wait.Milliseconds()}}}}, at))
	case agent.EventToolExecutionStart:
		if m.denied[ev.ToolCallID] {
			// A denied call never runs; its end update carries the refusal.
			return nil
		}
		if !m.announced[ev.ToolCallID] {
			m.announced[ev.ToolCallID] = true
			return []Update{m.announce(ev, "in_progress", at)}
		}
		return []Update{update("tool_call_update", map[string]any{"toolCallId": ev.ToolCallID, "status": "in_progress"}, at)}
	case agent.EventToolExecutionUpdate:
		p := map[string]any{"toolCallId": ev.ToolCallID}
		if content := toolContent(ev.PartialResult); len(content) > 0 {
			p["content"] = content
		}
		return []Update{update("tool_call_update", p, at)}
	case agent.EventToolExecutionEnd:
		status := "completed"
		if ev.IsError {
			status = "failed"
		}
		delete(m.announced, ev.ToolCallID)
		delete(m.denied, ev.ToolCallID)
		return []Update{update("tool_call_update", map[string]any{"toolCallId": ev.ToolCallID, "status": status, "rawOutput": rawInput(ev.Result), "content": toolContent(ev.Result)}, at)}
	case agent.EventAgentEnd:
		var cost *float64
		if !ev.Usage.Cost.Unknown {
			v := ev.Usage.Cost.Total
			cost = &v
		}
		p := map[string]any{"status": "finished", "runId": m.runID, "stopReason": ev.StopReason, "turns": ev.Turns, "usage": ev.Usage, "costUsd": cost, "elapsedMs": at.Sub(m.started).Milliseconds()}
		if ev.Err != nil {
			p["error"] = ev.Err.Error()
		}
		return []Update{update("tempest/run", p, at)}
	}
	return nil
}
func normalizeMessage(msg Message) Message {
	switch v := msg.(type) {
	case *types.UserMessage:
		if v != nil {
			return *v
		}
	case *types.AssistantMessage:
		if v != nil {
			return *v
		}
	case *types.ToolResultMessage:
		if v != nil {
			return *v
		}
	}
	return msg
}
func rawInput(v any) json.RawMessage {
	switch r := v.(type) {
	case json.RawMessage:
		if json.Valid(r) {
			return append(json.RawMessage(nil), r...)
		}
		raw, _ := json.Marshal(string(r))
		return raw
	case []byte:
		if json.Valid(r) {
			return append(json.RawMessage(nil), r...)
		}
		raw, _ := json.Marshal(string(r))
		return raw
	}
	raw, err := json.Marshal(v)
	if err != nil {
		raw, _ = json.Marshal(fmt.Sprint(v))
	}
	return raw
}
func toolDescription(name string, raw json.RawMessage) (string, ToolKind) {
	kind := ToolKind("other")
	switch name {
	case "read":
		kind = "read"
	case "write", "edit":
		kind = "edit"
	case "bash":
		kind = "execute"
	case "grep", "find", "ls", "lsp_definition", "lsp_references":
		kind = "search"
	case "lsp_rename", "lsp_diagnostics", "manifest_diff":
		// rename returns a plan (applied:false); applying it is a later edit.
		kind = "read"
	case "todo_write":
		kind = "think"
	default:
		if strings.HasPrefix(name, "web_") {
			kind = "fetch"
		}
	}
	var args map[string]any
	_ = json.Unmarshal(raw, &args)
	if name == "bash" {
		if cmd, ok := args["command"].(string); ok {
			return "$ " + cmd, kind
		}
	}
	if name == "manifest_diff" {
		before, _ := args["before"].(string)
		after, _ := args["after"].(string)
		if before != "" && after != "" {
			return name + ": " + before + " -> " + after, kind
		}
	}
	if path, ok := args["path"].(string); ok && path != "" && strings.HasPrefix(name, "lsp_") {
		line, hasLine := args["line"].(float64)
		column, hasColumn := args["column"].(float64)
		if hasLine && hasColumn {
			return fmt.Sprintf("%s: %s:%d:%d", name, path, int(line), int(column)), kind
		}
	}
	for _, key := range []string{"path", "pattern", "query"} {
		if value, ok := args[key].(string); ok && value != "" {
			return name + ": " + value, kind
		}
	}
	return name, kind
}
func toolContent(v any) []map[string]any {
	var blocks []types.ContentBlock
	switch r := v.(type) {
	case agent.AgentToolResult:
		blocks = r.Content
	case *agent.AgentToolResult:
		if r != nil {
			blocks = r.Content
		}
	case types.ToolResultMessage:
		blocks = r.Content
	}
	out := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, map[string]any{"type": "content", "content": b})
	}
	return out
}

type workPayload struct {
	Action          string          `json:"action,omitempty"`
	Item            *work.QueueItem `json:"item,omitempty"`
	Job             *work.Job       `json:"job,omitempty"`
	JobID           string          `json:"jobId,omitempty"`
	RunID           string          `json:"runId,omitempty"`
	Bytes           int64           `json:"bytes,omitempty"`
	CompactionID    string          `json:"compactionId,omitempty"`
	SourceHash      string          `json:"sourceHash,omitempty"`
	Reason          string          `json:"reason,omitempty"`
	ChildSessionID  string          `json:"childSessionId,omitempty"`
	ChildToolCallID string          `json:"childToolCallId,omitempty"`
	Title           string          `json:"title,omitempty"`
}

func mapWorkUpdate(event work.Event, at time.Time) Update {
	payload := workPayload{Action: event.Action, RunID: event.RunID, ChildSessionID: event.ChildSessionID, ChildToolCallID: event.ChildToolCallID, Title: event.ChildTitle}
	switch event.Kind {
	case "queue":
		payload.Item = event.Queue
	case "job":
		if event.Job != nil {
			summary := hub.SummarizeJob(*event.Job)
			payload.Job = &summary
		}
	case "job_output":
		if event.Job != nil {
			payload.JobID = event.Job.ID
		}
		payload.Bytes = event.Bytes
	case "compaction":
		payload.CompactionID = event.CompactionID
		payload.SourceHash = event.SourceHash
		payload.Reason = event.Reason
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(fmt.Sprintf("invalid work event: %v", err))
	}
	return Update{At: at, Kind: "tempest/" + event.Kind, Update: raw}
}
