package session

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"go.harness.dev/harness/internal/agent"
	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/work"
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
	return AttachJournalWithRunID(agt, store, nil)
}

// AttachJournalWithRunID also reports the journal's run id before later listeners
// see agent_start, keeping public run identifiers equal to durable identifiers.
func AttachJournalWithRunID(agt *agent.Agent, store *journal.Store, started func(string)) func() {
	return AttachJournalWithOptions(agt, store, RecordOptions{Started: started})
}

// RecordOptions connects durable run identity and queue consumption to recording.
type RecordOptions struct {
	Started func(string)
	Queues  *DurableQueues
}

// AttachJournalWithOptions installs the durable recorder before public listeners.
func AttachJournalWithOptions(agt *agent.Agent, store *journal.Store, opts RecordOptions) func() {
	r := &recorder{agt: agt, store: store, toolStarts: map[string]time.Time{}, started: opts.Started, queues: opts.Queues, sources: map[string]ptypes.Usage{}}
	return agt.Subscribe(r.record)
}

type recorder struct {
	agt        *agent.Agent
	store      *journal.Store
	runID      string
	started    func(string)
	queues     *DurableQueues
	toolStarts map[string]time.Time
	sources    map[string]ptypes.Usage
}

func (r *recorder) record(ctx context.Context, ev agent.AgentEvent) error {
	ctx = context.WithoutCancel(ctx)
	meta := journal.Meta{RunID: r.runID, EventSeq: ev.Seq}
	invocation, _ := work.InvocationFromContext(ctx)
	callKey := invocation.CallID
	if callKey == "" {
		callKey = ev.ToolCallID
	}
	var err error
	switch ev.Type {
	case agent.EventHook:
		if ev.Hook != nil {
			err = r.store.Append(ctx, journal.HookRecord{Meta: meta, Event: ev.Hook.Event, Status: "finished", Report: ev.Hook, QueueID: ev.QueueID})
			if err == nil && ev.QueueID != "" && r.queues != nil {
				err = r.queues.Acknowledge(ev.QueueID)
			}
		}
	case agent.EventAgentStart:
		loaded, loadErr := journal.Load(ctx, r.store.Path())
		if loadErr != nil {
			return loadErr
		}
		r.sources = loaded.UsageSources
		state := agent.RunState{SessionID: r.store.ID(), RunID: r.store.NewRunID(), StartedAt: time.Now(), UsageSources: r.sources}
		state.LogicalRunID = state.RunID
		if ev.Resume && loaded.LastRun != nil && !loaded.LastRun.Finished {
			state.ContinuationOf = loaded.LastRun.RunID
			state.LogicalRunID = loaded.LastRun.LogicalRunID
			state.Usage, state.Turns, state.StartedAt = loaded.LogicalUsage(state.LogicalRunID)
		}
		r.runID = state.RunID
		clear(r.toolStarts)
		meta.RunID = r.runID
		model := r.agt.State().Model
		err = r.store.Append(ctx, journal.RunStartedRecord{Meta: meta, Model: model.ID, Provider: model.Provider, PromptSummary: promptSummary(ev.Messages), LogicalRunID: state.LogicalRunID, ContinuationOf: state.ContinuationOf})
		if err == nil {
			r.agt.SetRunState(state)
			if r.started != nil {
				r.started(r.runID)
			}
		}
	case agent.EventMessageEnd:
		assistant, isAssistant := recordAssistant(ev.Message)
		assigned, appendErr := r.store.AppendAssigned(ctx, journal.MessageEntry{Meta: meta, Message: ev.Message, QueueID: ev.QueueID, Metered: isAssistant})
		err = appendErr
		if err == nil && ev.QueueID != "" && r.queues != nil {
			err = r.queues.Acknowledge(ev.QueueID)
		}
		if err == nil && isAssistant {
			source := "message:" + r.store.ID() + ":" + assigned.ID
			err = r.store.Append(ctx, journal.UsageRecord{Meta: meta, Model: assistant.Model, Usage: assistant.Usage, SourceRef: source, MessageID: assigned.ID})
			if err == nil {
				r.sources[source] = assistant.Usage
				r.agt.BindMessage(assigned.ID, source, assistant)
			}
		}
	case agent.EventUsage:
		source := ev.UsageSource
		if ev.UsageRunID != "" {
			meta.RunID = ev.UsageRunID
		}
		if prior, ok := r.sources[source]; source != "" && ok {
			if !reflect.DeepEqual(prior, ev.Usage) {
				err = fmt.Errorf("conflicting usage source %q", source)
			}
			break
		}
		// Compaction's finished fact is already the local receipt.
		if source != "" {
			if prior, ok := r.store.UsageSource(source); ok {
				if !reflect.DeepEqual(prior, ev.Usage) {
					return fmt.Errorf("conflicting usage source %q", source)
				}
				r.sources[source] = prior
				break
			}
		}
		err = r.store.Append(ctx, journal.UsageRecord{Meta: meta, Model: ev.Model, Usage: ev.Usage, SourceRef: source, ChildSessionID: ev.ChildSessionID})
		if err == nil && source != "" {
			r.sources[source] = ev.Usage
		}
	case agent.EventToolPermission:
		if ev.Permission != nil {
			decision := "deny"
			if ev.Permission.Allow {
				decision = "allow"
			}
			err = r.store.Append(ctx, journal.PermissionRecord{Meta: meta, ToolCallID: ev.ToolCallID, CallID: invocation.CallID, Tool: ev.ToolName, Args: rawArgs(ev.Args), Decision: decision, Source: string(ev.Permission.Source), WaitMS: ev.Permission.Wait.Milliseconds(), AskID: ev.Permission.AskID})
		}
	case agent.EventToolExecutionStart:
		if _, already := r.toolStarts[callKey]; already {
			break
		}
		r.toolStarts[callKey] = ev.At
		err = r.store.Append(ctx, journal.ToolStartedRecord{Meta: meta, ToolCallID: ev.ToolCallID, CallID: invocation.CallID, Tool: ev.ToolName, Args: rawArgs(ev.Args)})
	case agent.EventToolExecutionEnd:
		finished := journal.ToolFinishedRecord{Meta: meta, ToolCallID: ev.ToolCallID, CallID: invocation.CallID, OK: !ev.IsError}
		if started, ok := r.toolStarts[callKey]; ok {
			finished.DurationMS = ev.At.Sub(started).Milliseconds()
			delete(r.toolStarts, callKey)
		}
		if ev.IsError {
			finished.Error = toolErrorText(ev.Result)
		}
		err = r.store.Append(ctx, finished)
	case agent.EventAgentEnd:
		if r.queues != nil {
			r.queues.ReleaseClaims()
		}
		state := agent.RunState{}
		if r.agt != nil {
			state = r.agt.RunState()
		}
		finished := journal.RunFinishedRecord{Meta: meta, StopReason: string(ev.StopReason), Turns: ev.Turns, UsageTotal: ev.Usage, ElapsedMS: ev.ElapsedMS, LogicalCalls: state.Turns + ev.Turns}
		if ev.Err != nil {
			finished.Error = ev.Err.Error()
		}
		err = r.store.Append(ctx, finished)
		r.runID = ""
	case agent.EventWork:
		// Publication follows an already committed fact.
	}
	if err != nil {
		return fmt.Errorf("journal %s: %w", ev.Type, err)
	}
	return nil
}

func recordAssistant(message ptypes.Message) (ptypes.AssistantMessage, bool) {
	switch m := message.(type) {
	case ptypes.AssistantMessage:
		return m, true
	case *ptypes.AssistantMessage:
		if m != nil {
			return *m, true
		}
	}
	return ptypes.AssistantMessage{}, false
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
