package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"go.harness.dev/harness/internal/agent"
	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/session/journal"
)

// summaryRunes bounds the prompt summary and tool error text kept in records;
// the full text lives in message entries.
const summaryRunes = 200

// AttachJournal records agt's runs into store as they happen and returns a
// function that stops recording. Each record is written inside the event
// listener, before the agent continues, so tool_started is on disk before the
// tool executes. A write error is returned to the agent, which stops the run:
// a run never continues on a dead journal.
//
// Attach before the first Prompt or Continue. The listener relies on the
// agent delivering events one at a time.
func AttachJournal(agt *agent.Agent, store *journal.Store) (detach func()) {
	r := &recorder{agt: agt, store: store, toolStarts: map[string]time.Time{}}
	return agt.Subscribe(r.record)
}

type recorder struct {
	agt   *agent.Agent
	store *journal.Store
	runID string
	// toolStarts holds the start time of each in-flight tool call for its
	// duration.
	toolStarts map[string]time.Time
}

func (r *recorder) record(ctx context.Context, ev agent.AgentEvent) error {
	// Cancellation must not stop the journal: an aborted run still has to
	// record its last messages and its outcome. Writes are local and short.
	ctx = context.WithoutCancel(ctx)
	meta := journal.Meta{RunID: r.runID, EventSeq: ev.Seq}
	var err error
	switch ev.Type {
	case agent.EventAgentStart:
		r.runID = r.store.NewRunID()
		clear(r.toolStarts)
		meta.RunID = r.runID
		model := r.agt.State().Model
		err = r.store.Append(ctx, journal.RunStartedRecord{
			Meta:          meta,
			Model:         model.ID,
			Provider:      model.Provider,
			PromptSummary: promptSummary(ev.Messages),
		})
	case agent.EventMessageEnd:
		err = r.store.Append(ctx, journal.MessageEntry{Meta: meta, Message: ev.Message})
		if assistant, ok := ev.Message.(ptypes.AssistantMessage); ok && err == nil {
			err = r.store.Append(ctx, journal.UsageRecord{Meta: meta, Model: assistant.Model, Usage: assistant.Usage})
		}
	case agent.EventToolExecutionStart:
		r.toolStarts[ev.ToolCallID] = ev.At
		err = r.store.Append(ctx, journal.ToolStartedRecord{
			Meta:       meta,
			ToolCallID: ev.ToolCallID,
			Tool:       ev.ToolName,
			Args:       rawArgs(ev.Args),
		})
	case agent.EventToolExecutionEnd:
		finished := journal.ToolFinishedRecord{Meta: meta, ToolCallID: ev.ToolCallID, OK: !ev.IsError}
		if started, ok := r.toolStarts[ev.ToolCallID]; ok {
			finished.DurationMS = ev.At.Sub(started).Milliseconds()
			delete(r.toolStarts, ev.ToolCallID)
		}
		if ev.IsError {
			finished.Error = toolErrorText(ev.Result)
		}
		err = r.store.Append(ctx, finished)
	case agent.EventAgentEnd:
		finished := journal.RunFinishedRecord{
			Meta:       meta,
			StopReason: string(ev.StopReason),
			Turns:      ev.Turns,
			UsageTotal: ev.Usage,
		}
		if ev.Err != nil {
			finished.Error = ev.Err.Error()
		}
		err = r.store.Append(ctx, finished)
		r.runID = ""
	}
	if err != nil {
		return fmt.Errorf("journal %s: %w", ev.Type, err)
	}
	return nil
}

// promptSummary is the start of the prompt's text, enough to recognize a run
// in a listing.
func promptSummary(prompts []ptypes.Message) string {
	var parts []string
	for _, message := range prompts {
		user, ok := message.(ptypes.UserMessage)
		if !ok {
			continue
		}
		if !user.Content.IsBlocks() {
			parts = append(parts, user.Content.Text)
			continue
		}
		for _, block := range user.Content.Blocks {
			if block.Type == ptypes.BlockText {
				parts = append(parts, block.Text)
			}
		}
	}
	return truncateRunes(strings.Join(parts, "\n"), summaryRunes)
}

// rawArgs keeps the model's arguments as JSON. The start event fires before
// validation, so malformed arguments are kept as a JSON string rather than
// failing the write.
func rawArgs(args any) json.RawMessage {
	var raw []byte
	switch value := args.(type) {
	case nil:
		return nil
	case json.RawMessage:
		raw = value
	case []byte:
		raw = value
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			raw = []byte(fmt.Sprint(value))
		} else {
			raw = encoded
		}
	}
	if len(raw) == 0 {
		return nil
	}
	if json.Valid(raw) {
		return json.RawMessage(raw)
	}
	quoted, _ := json.Marshal(string(raw))
	return quoted
}

// toolErrorText is the start of a failed tool's text result.
func toolErrorText(result any) string {
	toolResult, ok := result.(agent.AgentToolResult)
	if !ok {
		return ""
	}
	var parts []string
	for _, block := range toolResult.Content {
		if block.Type == ptypes.BlockText {
			parts = append(parts, block.Text)
		}
	}
	return truncateRunes(strings.Join(parts, "\n"), summaryRunes)
}

func truncateRunes(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return string(runes[:limit])
}
