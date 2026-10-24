package journal

import (
	"encoding/json"
	"time"

	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/work"
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
	Finished       bool
	Error          string
	FinishedAt     time.Time
	LogicalRunID   string
	ContinuationOf string
	StartedAt      time.Time
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
// never re-run. The lsp_* tools stay off: a query may start a language
// server, which is trusted executable code.
var safeToRerun = map[string]bool{"read": true, "grep": true, "find": true, "ls": true, "manifest_diff": true}

// toolKey identifies the queue of occurrences sharing an endpoint's call id.
type toolKey struct {
	runID      string
	toolCallID string
}

type toolState struct {
	started  *ToolStartedRecord
	finished *ToolFinishedRecord
	job      *work.Job
}

type replayTool struct {
	call     ptypes.ContentBlock
	state    *toolState
	answered bool
}

// reduce folds decoded lines, in file order, into loaded. It is a pure
// function of the lines: loading the same file twice gives the same result.
func reduce(loaded *Loaded, entries []Entry) {
	messages := make([]replayMessage, 0, len(entries))
	// Calls belong to their assistant turn, not just a provider-supplied id.
	// Finished records consume started occurrences in file order.
	calls := map[toolKey][]*toolState{}
	unfinished := map[toolKey][]*toolState{}
	var startOrder []*toolState
	var runs []RunSummary
	runIndex := map[string]int{}

	for _, entry := range entries {
		switch e := entry.(type) {
		case MessageEntry:
			replay := replayMessage{id: e.ID, runID: e.RunID, message: e.Message}
			if assistant, ok := e.Message.(ptypes.AssistantMessage); ok {
				clear(calls)
				if assistant.StopReason != ptypes.StopError && assistant.StopReason != ptypes.StopAborted {
					for _, block := range assistant.Content {
						if block.Type != ptypes.BlockToolCall {
							continue
						}
						state := &toolState{}
						replay.calls = append(replay.calls, replayTool{call: block, state: state})
						key := toolKey{runID: e.RunID, toolCallID: block.ID}
						calls[key] = append(calls[key], state)
					}
				}
			}
			messages = append(messages, replay)
		case ModelChangeEntry:
			loaded.Model = e.Model
			loaded.Role = e.Role
			if loaded.Role == "" {
				loaded.Role = "default"
			}
		case CompactionEntry:
			if e.Snapshot != nil {
				messages = nil
				for _, message := range loaded.snapshots[e.ID] {
					messages = append(messages, replayMessage{message: message})
				}
			} else {
				messages = applyCompaction(messages, e)
			}
		case RewindEntry:
			replacement, _ := e.messages()
			messages = nil
			clear(calls)
			clear(unfinished)
			startOrder = nil
			for _, message := range replacement {
				messages = append(messages, replayMessage{message: message})
			}
		case PermissionRecord:
			// Effective arguments refine an existing intent; a decision alone never creates one.
			key := toolKey{runID: e.RunID, toolCallID: e.ToolCallID}
			if e.CallID != "" {
				key.toolCallID = e.CallID
			}
			if queue := unfinished[key]; len(queue) > 0 && len(e.Args) > 0 {
				queue[len(queue)-1].started.Args = e.Args
			}
		case RunStartedRecord:
			clear(calls)
			if _, seen := runIndex[e.RunID]; seen {
				loaded.diagnose("seq %d: duplicate run_started for run %s ignored", e.Seq, e.RunID)
				continue
			}
			runIndex[e.RunID] = len(runs)
			logical := e.LogicalRunID
			if logical == "" {
				logical = e.RunID
			}
			runs = append(runs, RunSummary{RunID: e.RunID, LogicalRunID: logical, ContinuationOf: e.ContinuationOf, StartedAt: e.Timestamp})
		case RunFinishedRecord:
			index, ok := runIndex[e.RunID]
			if !ok {
				loaded.diagnose("seq %d: run_finished for run %s without run_started", e.Seq, e.RunID)
				continue
			}
			runs[index].Finished = true
			runs[index].StopReason = e.StopReason
			runs[index].Error = e.Error
			runs[index].FinishedAt = e.Timestamp
		case ToolStartedRecord:
			if e.Tool == "eval" {
				var request struct {
					Language string `json:"language"`
				}
				if json.Unmarshal(e.Args, &request) == nil && (request.Language == "py" || request.Language == "js") {
					if loaded.EvalLanguages == nil {
						loaded.EvalLanguages = make(map[string]bool)
					}
					loaded.EvalLanguages[request.Language] = true
				}
			}
			key := toolKey{runID: e.RunID, toolCallID: e.ToolCallID}
			var state *toolState
			if queue := calls[key]; len(queue) > 0 {
				state = queue[0]
				calls[key] = queue[1:]
			} else {
				state = &toolState{}
			}
			record := e
			state.started = &record
			for _, recovery := range loaded.Recoveries {
				if recovery.Invocation.RunID == e.RunID && recovery.Invocation.CallID == e.CallID && e.CallID != "" {
					job := recovery.Job
					state.job = &job
					break
				}
			}
			startOrder = append(startOrder, state)
			if e.CallID != "" {
				key.toolCallID = e.CallID
			}
			unfinished[key] = append(unfinished[key], state)
		case ToolFinishedRecord:
			key := toolKey{runID: e.RunID, toolCallID: e.ToolCallID}
			if e.CallID != "" {
				key.toolCallID = e.CallID
			}
			queue := unfinished[key]
			if len(queue) == 0 {
				loaded.diagnose("seq %d: tool_finished for %s without tool_started", e.Seq, e.ToolCallID)
				continue
			}
			record := e
			queue[0].finished = &record
			unfinished[key] = queue[1:]
		case UsageRecord:
			// reduceWork has already deduplicated receipts and metered fallback.
		}
	}

	if len(runs) > 0 {
		last := runs[len(runs)-1]
		loaded.LastRun = &last
	}
	for _, state := range startOrder {
		if state.finished != nil || loaded.LastRun == nil || state.started.RunID != loaded.LastRun.RunID {
			continue
		}
		loaded.Interrupted = append(loaded.Interrupted, InterruptedTool{
			RunID:      state.started.RunID,
			ToolCallID: state.started.ToolCallID,
			Tool:       state.started.Tool,
			Args:       state.started.Args,
			StartedAt:  state.started.Timestamp,
		})
	}
	// Like Interrupted, the warning is scoped to LastRun: a later run_started
	// means the session was already resumed past the interruption, so an older
	// unfinished run is history, not news. Its synthesized tool results stay.
	if loaded.LastRun != nil && !loaded.LastRun.Finished {
		loaded.diagnose("run %s was interrupted: no run_finished record", loaded.LastRun.RunID)
	}
	loaded.Messages = reconcile(messages)
}

// reconcile returns the transcript with one result for every tool call the
// provider will see. Assistant messages that stopped with error or aborted are
// skipped: the loop never runs their calls and the provider transform drops
// them. A missing result is synthesized after the turn's real results, worded
// from what the records prove.
func reconcile(messages []replayMessage) []ptypes.Message {
	out := make([]ptypes.Message, 0, len(messages))
	var pending []replayTool
	var pendingRun string
	var pendingTimestamp int64
	flush := func() {
		for _, tool := range pending {
			if tool.answered {
				continue
			}
			if result, ok := synthesizeResult(tool.call, pendingRun, tool.state, pendingTimestamp); ok {
				out = append(out, result)
			}
		}
		pending = nil
	}

	for _, replay := range messages {
		if result, ok := replay.message.(ptypes.ToolResultMessage); ok {
			for index := range pending {
				if pending[index].call.ID == result.ToolCallID && !pending[index].answered {
					pending[index].answered = true
					break
				}
			}
			out = append(out, replay.message)
			continue
		}
		flush()
		out = append(out, replay.message)
		assistant, ok := replay.message.(ptypes.AssistantMessage)
		if !ok || assistant.StopReason == ptypes.StopError || assistant.StopReason == ptypes.StopAborted {
			continue
		}
		pending = replay.calls
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
	if state != nil && state.job != nil {
		job := state.job
		return ptypes.ToolResultMessage{ToolCallID: call.ID, ToolName: call.Name, Content: []ptypes.ContentBlock{ptypes.NewText("Existing background job " + job.ID + " (" + job.Kind + ", " + job.State + "); this is its durable handle, not a new launch.")}, Timestamp: timestamp}, true
	}
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
		if call.Name == "eval" {
			text += " Eval kernel state was lost; use reset:true and re-run setup explicitly."
		}
	}
	return ptypes.ToolResultMessage{
		ToolCallID: call.ID,
		ToolName:   call.Name,
		Content:    []ptypes.ContentBlock{ptypes.NewText(text)},
		IsError:    true,
		Timestamp:  timestamp,
	}, true
}
