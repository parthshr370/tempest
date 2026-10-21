package tempest

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.harness.dev/harness/internal/agent"
	types "go.harness.dev/harness/internal/engine/types"
)

type eventMapper struct {
	sessionID string
	model     types.Model
	counter   int
	messageID string
	text      string
	thought   string
	runID     string
	started   time.Time
	turn      int
	usage     UsageState
	announced map[string]bool
}

func newMapper(id string, model types.Model) *eventMapper {
	return &eventMapper{sessionID: id, model: model, announced: make(map[string]bool), usage: UsageState{ContextSize: model.ContextWindow}}
}
func (m *eventMapper) nextMessage() string { m.counter++; return fmt.Sprintf("msg_%d", m.counter) }
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
	at := ev.At
	switch ev.Type {
	case agent.EventAgentStart:
		m.started = at
		m.turn = 0
		clear(m.announced)
		return []Update{update("tempest/run", map[string]any{"status": "started", "runId": m.runID, "model": m.model.ID}, at)}
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
		return []Update{update("tempest/usage", map[string]any{"model": ev.Model, "usage": ev.Usage, "total": m.usage.Total, "usageState": m.usage}, at)}
	case agent.EventToolPermission:
		if ev.Permission == nil {
			return nil
		}
		p := ev.Permission
		if p.Prompted {
			m.announced[ev.ToolCallID] = true
		}
		decision := "deny"
		status := "failed"
		if p.Allow {
			decision = "allow"
			status = "pending"
		}
		return []Update{update("tool_call_update", map[string]any{"toolCallId": ev.ToolCallID, "status": status, "_meta": map[string]any{"tempest": map[string]any{"permission": map[string]any{"decision": decision, "source": p.Source, "waitMs": p.Wait.Milliseconds()}}}}, at)}
	case agent.EventToolExecutionStart:
		kind := "tool_call"
		p := map[string]any{"toolCallId": ev.ToolCallID, "status": "in_progress"}
		if m.announced[ev.ToolCallID] {
			kind = "tool_call_update"
		} else {
			title, k := toolDescription(ev.ToolName, rawInput(ev.Args))
			p["title"] = title
			p["kind"] = k
			p["rawInput"] = rawInput(ev.Args)
			m.announced[ev.ToolCallID] = true
		}
		return []Update{update(kind, p, at)}
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
	case "grep", "find", "ls":
		kind = "search"
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
