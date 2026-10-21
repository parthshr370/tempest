package journal

import (
	"encoding/json"
	"time"

	ptypes "go.harness.dev/harness/internal/engine/types"
)

// InterruptedTool is a tool call whose intent was recorded but whose result
// was not. It may or may not have run; nothing re-runs it automatically.
type InterruptedTool struct {
	RunID      string
	ToolCallID string
	Tool       string
	Args       json.RawMessage
	StartedAt  time.Time
}

// RunSummary says how the most recent run in a journal ended.
type RunSummary struct {
	RunID string
	// StopReason is empty when Finished is false.
	StopReason string
	// Finished is false when the run has no run_finished record: the process
	// ended mid-run.
	Finished bool
}

// Synthesized tool-result wording. The model reads these, so they state what
// the journal knows and nothing more.
const (
	textOutcomeUnknown = "outcome unknown: the harness process ended before this tool's result was recorded; do not assume it ran or did not run."
	textNotExecuted    = "not executed: the run stopped before this tool call started."
	textSafeToRerun    = " It is read-only and safe to run again if you still need its output."
	textNoRerun        = " It will not be re-run automatically."
)

// safeToRerun names the shipped tools whose re-execution has no side effects
// (docs/design/03-durable-core.md section 6). Everything else defaults to
// never re-run.
var safeToRerun = map[string]bool{"read": true, "grep": true, "find": true, "ls": true}

// toolKey scopes a tool call id to its run: providers and adapters may reuse
// ids across processes, but never within one assistant turn.
type toolKey struct {
	runID      string
	toolCallID string
}

type toolState struct {
	started  *ToolStartedRecord
	finished *ToolFinishedRecord
}

// reduce folds decoded lines, in file order, into loaded. It is a pure
// function of the lines: loading the same file twice gives the same result.
func reduce(loaded *Loaded, entries []Entry) {
	messages := make([]replayMessage, 0, len(entries))
	tools := map[toolKey]*toolState{}
	var startOrder []toolKey
	var runs []RunSummary
	runIndex := map[string]int{}

	for _, entry := range entries {
		switch e := entry.(type) {
		case MessageEntry:
			messages = append(messages, replayMessage{id: e.ID, runID: e.RunID, message: e.Message})
		case ModelChangeEntry:
			loaded.Model = e.Model
			loaded.Role = e.Role
			if loaded.Role == "" {
				loaded.Role = "default"
			}
		case CompactionEntry:
			messages = applyCompaction(messages, e)
		case RunStartedRecord:
			if _, seen := runIndex[e.RunID]; seen {
				loaded.diagnose("seq %d: duplicate run_started for run %s ignored", e.Seq, e.RunID)
				continue
			}
			runIndex[e.RunID] = len(runs)
			runs = append(runs, RunSummary{RunID: e.RunID})
		case RunFinishedRecord:
			index, ok := runIndex[e.RunID]
			if !ok {
				loaded.diagnose("seq %d: run_finished for run %s without run_started", e.Seq, e.RunID)
				continue
			}
			runs[index].Finished = true
			runs[index].StopReason = e.StopReason
		case ToolStartedRecord:
			key := toolKey{runID: e.RunID, toolCallID: e.ToolCallID}
			state := tools[key]
			if state == nil {
				state = &toolState{}
				tools[key] = state
			}
			if state.started != nil {
				loaded.diagnose("seq %d: duplicate tool_started for %s ignored", e.Seq, e.ToolCallID)
				continue
			}
			record := e
			state.started = &record
			startOrder = append(startOrder, key)
		case ToolFinishedRecord:
			key := toolKey{runID: e.RunID, toolCallID: e.ToolCallID}
			state := tools[key]
			if state == nil || state.started == nil {
				// Intent precedes effect, so this means a damaged or foreign
				// file. Keep the fact; the call is not reported interrupted.
				loaded.diagnose("seq %d: tool_finished for %s without tool_started", e.Seq, e.ToolCallID)
				if state == nil {
					state = &toolState{}
					tools[key] = state
				}
			}
			record := e
			state.finished = &record
		case UsageRecord:
			loaded.UsageTotal.Add(e.Usage)
		}
	}

	for _, key := range startOrder {
		state := tools[key]
		if state.finished != nil {
			continue
		}
		loaded.Interrupted = append(loaded.Interrupted, InterruptedTool{
			RunID:      key.runID,
			ToolCallID: key.toolCallID,
			Tool:       state.started.Tool,
			Args:       state.started.Args,
			StartedAt:  state.started.Timestamp,
		})
	}
	for _, run := range runs {
		if !run.Finished {
			loaded.diagnose("run %s was interrupted: no run_finished record", run.RunID)
		}
	}
	if len(runs) > 0 {
		last := runs[len(runs)-1]
		loaded.LastRun = &last
	}
	loaded.Messages = reconcile(messages, tools)
}

// reconcile returns the transcript with one result for every tool call the
// provider will see. Assistant messages that stopped with error or aborted are
// skipped: the loop never runs their calls and the provider transform drops
// them. A missing result is synthesized after the turn's real results, worded
// from what the records prove.
func reconcile(messages []replayMessage, tools map[toolKey]*toolState) []ptypes.Message {
	out := make([]ptypes.Message, 0, len(messages))
	var pending []ptypes.ContentBlock
	var pendingRun string
	var pendingTimestamp int64
	answered := map[string]bool{}
	flush := func() {
		for _, call := range pending {
			if answered[call.ID] {
				continue
			}
			if result, ok := synthesizeResult(call, pendingRun, tools[toolKey{runID: pendingRun, toolCallID: call.ID}], pendingTimestamp); ok {
				out = append(out, result)
			}
		}
		pending = nil
		clear(answered)
	}

	for _, replay := range messages {
		if result, ok := replay.message.(ptypes.ToolResultMessage); ok {
			answered[result.ToolCallID] = true
			out = append(out, replay.message)
			continue
		}
		flush()
		out = append(out, replay.message)
		assistant, ok := replay.message.(ptypes.AssistantMessage)
		if !ok || assistant.StopReason == ptypes.StopError || assistant.StopReason == ptypes.StopAborted {
			continue
		}
		for _, block := range assistant.Content {
			if block.Type == ptypes.BlockToolCall {
				pending = append(pending, block)
			}
		}
		pendingRun = replay.runID
		pendingTimestamp = assistant.Timestamp
	}
	flush()
	return out
}

// synthesizeResult words a missing tool result from the records. A call with
// no records is only claimed "not executed" when its assistant message was
// written inside a run, where tool_started always precedes execution; calls
// from version 1 or external writers are left untouched.
func synthesizeResult(call ptypes.ContentBlock, runID string, state *toolState, timestamp int64) (ptypes.ToolResultMessage, bool) {
	var text string
	switch {
	case state != nil && state.started != nil && state.finished == nil:
		text = textOutcomeUnknown
		timestamp = state.started.Timestamp.UnixMilli()
	case state != nil && state.finished != nil:
		outcome := "succeeded"
		if !state.finished.OK {
			outcome = "failed"
			if state.finished.Error != "" {
				outcome += " (" + state.finished.Error + ")"
			}
		}
		text = "result lost: the tool " + outcome + ", but the harness process ended before its output was recorded."
		timestamp = state.finished.Timestamp.UnixMilli()
	case runID != "":
		text = textNotExecuted
	default:
		return ptypes.ToolResultMessage{}, false
	}
	if safeToRerun[call.Name] {
		text += textSafeToRerun
	} else {
		text += textNoRerun
	}
	return ptypes.ToolResultMessage{
		ToolCallID: call.ID,
		ToolName:   call.Name,
		Content:    []ptypes.ContentBlock{ptypes.NewText(text)},
		IsError:    true,
		Timestamp:  timestamp,
	}, true
}
