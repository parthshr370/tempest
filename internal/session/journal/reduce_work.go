package journal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	types "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/work"
)

// RunMessage is the bounded user/assistant history reconstructed for read-only lookup.
type RunMessage struct {
	Role string
	Text string
}

// RunDetail describes one original run, independently of later context compaction.
type RunDetail struct {
	SessionID      string
	RunID          string
	LogicalRunID   string
	ContinuationOf string
	Model          string
	Provider       string
	StopReason     string
	Error          string
	FinalText      string
	StartedAt      time.Time
	FinishedAt     time.Time
	Finished       bool
	Interrupted    bool
	Recoverable    bool
	Turns          int
	LogicalCalls   int
	ElapsedMS      int64
	Usage          types.Usage
	History        []RunMessage
}

func assistantBlocks(m types.Message) []types.ContentBlock {
	switch v := m.(type) {
	case types.AssistantMessage:
		return v.Content
	case *types.AssistantMessage:
		if v != nil {
			return v.Content
		}
	}
	return nil
}
func asAssistantMessage(m types.Message) (types.AssistantMessage, bool) {
	switch v := m.(type) {
	case types.AssistantMessage:
		return v, true
	case *types.AssistantMessage:
		if v != nil {
			return *v, true
		}
	}
	return types.AssistantMessage{}, false
}
func messageText(m types.Message) string {
	var blocks []types.ContentBlock
	switch v := m.(type) {
	case types.UserMessage:
		if !v.Content.IsBlocks() {
			return v.Content.Text
		}
		blocks = v.Content.Blocks
	case *types.UserMessage:
		if v != nil {
			return messageText(*v)
		}
	default:
		blocks = assistantBlocks(m)
	}
	var text []string
	for _, b := range blocks {
		if b.Type == types.BlockText {
			text = append(text, b.Text)
		}
	}
	return strings.Join(text, "\n")
}
func boundedText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func (l *Loaded) reduceWork(ctx context.Context, entries []Entry, path string, withSnapshots bool) error {
	l.QueueCancelled = map[string]bool{}
	l.QueueConsumed = map[string]bool{}
	l.UsageSources = map[string]types.Usage{}
	l.CompactionParts = map[string]CompactionPart{}
	l.StopRequests = map[string]string{}
	l.snapshots = map[string][]types.Message{}
	queue := map[string]work.QueueItem{}
	keys := map[string]string{}
	jobs := map[string]*work.Recovery{}
	observedRuns := map[string]string{}
	runs := map[string]int{}
	historyBytes := map[string]int{}
	var jobOrder []string
	addUsage := func(source, runID string, usage types.Usage) error {
		if prior, ok := l.UsageSources[source]; ok {
			if !reflect.DeepEqual(prior, usage) {
				return fmt.Errorf("conflicting usage source %q", source)
			}
			return nil
		}
		l.UsageSources[source] = usage
		if i, ok := runs[runID]; ok {
			l.Runs[i].Usage.Add(usage)
		}
		l.UsageTotal.Add(usage)
		return nil
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch e := entry.(type) {
		case RunStartedRecord:
			if _, ok := runs[e.RunID]; ok {
				continue
			}
			logical := e.LogicalRunID
			if logical == "" {
				logical = e.RunID
			}
			runs[e.RunID] = len(l.Runs)
			l.Runs = append(l.Runs, RunDetail{SessionID: l.Header.ID, RunID: e.RunID, LogicalRunID: logical, ContinuationOf: e.ContinuationOf, StartedAt: e.Timestamp, Model: e.Model, Provider: e.Provider, Interrupted: true, Recoverable: true})
		case RunFinishedRecord:
			if i, ok := runs[e.RunID]; ok {
				r := &l.Runs[i]
				r.Finished = true
				r.Interrupted = false
				r.Recoverable = false
				r.FinishedAt = e.Timestamp
				r.StopReason = e.StopReason
				r.Error = e.Error
				r.Turns = e.Turns
				r.LogicalCalls = e.LogicalCalls
				r.ElapsedMS = e.ElapsedMS
			}
		case MessageEntry:
			if e.QueueID != "" {
				if _, ok := queue[e.QueueID]; !ok || l.QueueCancelled[e.QueueID] || l.QueueConsumed[e.QueueID] {
					return fmt.Errorf("invalid or repeated queue consumption %q", e.QueueID)
				}
				l.QueueConsumed[e.QueueID] = true
			}
			if i, ok := runs[e.RunID]; ok {
				r := &l.Runs[i]
				text := boundedText(messageText(e.Message), 1<<20)
				if a, ok := asAssistantMessage(e.Message); ok {
					r.Turns++
					if strings.TrimSpace(text) != "" {
						r.FinalText = text
					}
					_ = a
				}
				if e.Message != nil && e.Message.Role() == "user" && text != "" {
					text = boundedText(text, (1<<20)-historyBytes[e.RunID])
					if text != "" {
						r.History = append(r.History, RunMessage{Role: "user", Text: text})
						historyBytes[e.RunID] += len(text)
					}
				}
			}
			if e.Metered {
				if a, ok := asAssistantMessage(e.Message); ok {
					if err := addUsage("message:"+l.Header.ID+":"+e.ID, e.RunID, a.Usage); err != nil {
						return err
					}
				}
			}
		case UsageRecord:
			source := e.SourceRef
			if e.MessageID != "" {
				expected := "message:" + l.Header.ID + ":" + e.MessageID
				if source != "" && source != expected {
					return fmt.Errorf("invalid local message source_ref")
				}
				source = expected
			}
			if source == "" {
				source = fmt.Sprintf("legacy:%s:%d", l.Header.ID, e.Seq)
				if e.Seq == 0 {
					source = fmt.Sprintf("legacy:%s:%d", l.Header.ID, len(l.UsageSources))
				}
			}
			if err := addUsage(source, e.RunID, e.Usage); err != nil {
				return err
			}
		case CompactionEntry:
			if e.Snapshot != nil && withSnapshots {
				messages, err := readSnapshot(path, e)
				if err != nil {
					return err
				}
				l.snapshots[e.ID] = messages
			}
		case WorkRecord:
			switch e.Type {
			case KindQueueEnqueued:
				var v QueueEnqueued
				_ = json.Unmarshal(e.Payload, &v)
				item := work.QueueItem{ID: v.QueueID, Kind: v.Kind, Text: v.Text, Key: v.Key, SourceJobID: v.SourceJobID, EnqueuedAt: e.Timestamp.UTC()}
				if prior, ok := queue[v.QueueID]; ok {
					prior.EnqueuedAt = item.EnqueuedAt
					if !equalJSON(prior, item) {
						return fmt.Errorf("conflicting queue receipt %q", v.QueueID)
					}
					continue
				}
				if v.Key != "" {
					if _, ok := keys[v.Key]; ok {
						return fmt.Errorf("conflicting queue key %q", v.Key)
					}
					keys[v.Key] = v.QueueID
				}
				queue[v.QueueID] = item
				l.QueueReceipts = append(l.QueueReceipts, item)
			case KindQueueCancelled:
				var v QueueCancelled
				_ = json.Unmarshal(e.Payload, &v)
				if _, ok := queue[v.QueueID]; !ok || l.QueueConsumed[v.QueueID] {
					return fmt.Errorf("invalid queue cancellation %q", v.QueueID)
				}
				l.QueueCancelled[v.QueueID] = true
			case KindJobStarted:
				var v JobStarted
				_ = json.Unmarshal(e.Payload, &v)
				if prior, ok := jobs[v.JobID]; ok {
					if prior.IdentityDigest != v.IdentityDigest {
						return fmt.Errorf("job identity collision %q", v.JobID)
					}
					return fmt.Errorf("duplicate job intent %q", v.JobID)
				}
				startedAt := v.StartedAt
				if startedAt.IsZero() {
					startedAt = e.Timestamp
				}
				r := &work.Recovery{Job: work.Job{ID: v.JobID, Kind: v.Kind, State: "starting", RunID: e.RunID, LogicalRunID: v.LogicalRunID, ToolCallID: v.ToolCallID, ChildSessionID: v.ChildSessionID, StartedAt: startedAt.UTC(), Cwd: v.Cwd, Isolation: v.Isolation, Cleanup: "none"}, Invocation: work.Invocation{RunID: e.RunID, CallID: v.CallID, ToolCallID: v.ToolCallID}, IdentityDigest: v.IdentityDigest, ChildJournal: v.ChildJournal}
				if v.Environment != nil {
					r.Environment = &work.TaskEnvironment{Model: v.Environment.Model, Provider: v.Environment.Provider, Ceiling: work.Ceiling{Mode: v.Environment.Ceiling.Mode, Tools: v.Environment.Ceiling.Tools, Overrides: v.Environment.Ceiling.Overrides, Interactive: v.Environment.Ceiling.Interactive, CloudRules: v.Environment.Ceiling.CloudRules}}
				}
				if v.Worktree != nil {
					w := v.Worktree
					r.Workspace = &work.Workspace{Repo: w.Repo, CommonDir: w.CommonDir, BaseCommit: w.BaseCommit, RelativeCwd: w.RelativeCwd, Path: w.Path, Marker: w.Marker}
					r.Job.BaseCommit = w.BaseCommit
					r.Job.Cleanup = "retained"
					if r.Job.Isolation == "" {
						r.Job.Isolation = "worktree"
					}
				}
				if v.Kind == "task" {
					var spec work.TaskSpec
					_ = json.Unmarshal(v.Spec, &spec)
					r.Task = &spec
					r.Job.Description = spec.Description
					if spec.SubagentType == "general" && r.Job.Isolation == "" {
						r.Job.Isolation = "shared"
					}
				} else {
					var spec work.BashSpec
					_ = json.Unmarshal(v.Spec, &spec)
					r.Bash = &spec
					if v.OutputPath != "" {
						r.Job.Output = &work.Artifact{Path: v.OutputPath}
					}
				}
				jobs[v.JobID] = r
				observedRuns[v.JobID] = e.RunID
				jobOrder = append(jobOrder, v.JobID)
			case KindJobRunning:
				var v JobRunning
				_ = json.Unmarshal(e.Payload, &v)
				r := jobs[v.JobID]
				if r == nil || terminal(r.Job.State) || r.Job.State != "starting" {
					return fmt.Errorf("invalid job_running %q", v.JobID)
				}
				r.Job.State = "running"
			case KindJobAttached:
				var v JobAttached
				_ = json.Unmarshal(e.Payload, &v)
				r := jobs[v.JobID]
				if r == nil || r.Job.Kind != "task" || terminal(r.Job.State) || r.Job.LogicalRunID != v.LogicalRunID || r.CancelRequested {
					return fmt.Errorf("invalid job_attached %q", v.JobID)
				}
				r.Job.State = "starting"
				observedRuns[v.JobID] = v.RunID
			case KindJobCancelRequested:
				var v JobCancelRequested
				_ = json.Unmarshal(e.Payload, &v)
				r := jobs[v.JobID]
				if r == nil || terminal(r.Job.State) {
					return fmt.Errorf("invalid job_cancel_requested %q", v.JobID)
				}
				r.CancelRequested = true
				r.Job.Reason = v.Reason
			case KindJobFinished:
				var v JobFinished
				_ = json.Unmarshal(e.Payload, &v)
				r := jobs[v.JobID]
				if r == nil || terminal(r.Job.State) {
					return fmt.Errorf("invalid job_finished %q", v.JobID)
				}
				r.Job.State = v.State
				r.Job.FinishedAt = v.FinishedAt.UTC()
				r.Job.Reason = v.Reason
				r.Job.ExitCode = v.ExitCode
				r.Job.Usage = v.Usage
				r.Job.Result = v.Result
				r.Job.Patch = v.Patch
				r.Job.Output = v.Output
				r.Job.Value = v.Value
				r.Job.ResultText = v.ResultPreview
				r.Job.Cleanup = v.Cleanup
			case KindJobCleaned:
				var v JobCleaned
				_ = json.Unmarshal(e.Payload, &v)
				r := jobs[v.JobID]
				if r == nil || !terminal(r.Job.State) || r.Workspace == nil || v.Path != r.Workspace.Path {
					return fmt.Errorf("invalid job_cleaned %q", v.JobID)
				}
				r.Job.Cleanup = "removed"
			case KindRunStopRequested:
				var v RunStopRequested
				_ = json.Unmarshal(e.Payload, &v)
				if prior := l.StopRequests[e.RunID]; prior != "" && prior != v.Reason {
					return fmt.Errorf("conflicting run stop request")
				}
				l.StopRequests[e.RunID] = v.Reason
			case KindCompactionCallStarted, KindCompactionCallFinished:
				var v CompactionCall
				_ = json.Unmarshal(e.Payload, &v)
				key := v.SourceHash + ":" + v.Phase
				p := l.CompactionParts[key]
				if e.Type == KindCompactionCallStarted {
					if p.Started != nil {
						return fmt.Errorf("duplicate compaction intent")
					}
					p.Started = &v
				} else {
					if p.Started == nil || p.Finished != nil || p.Started.CompactionID != v.CompactionID || p.Started.Model != v.Model {
						return fmt.Errorf("invalid compaction result")
					}
					p.Finished = &v
					if err := addUsage(v.SourceRef, e.RunID, v.Usage); err != nil {
						return err
					}
				}
				l.CompactionParts[key] = p
			}
		}
	}
	for i := range l.Runs {
		r := &l.Runs[i]
		remaining := (1 << 20) - len(r.FinalText)
		kept := r.History[:0]
		for _, message := range r.History {
			if remaining <= 0 {
				break
			}
			message.Text = boundedText(message.Text, remaining)
			remaining -= len(message.Text)
			kept = append(kept, message)
		}
		r.History = kept
		if r.FinalText != "" {
			r.History = append(r.History, RunMessage{Role: "assistant", Text: r.FinalText})
		}
		if !r.Finished {
			r.Error = "Host process ended before this run finished"
		}
	}
	for _, item := range l.QueueReceipts {
		if !l.QueueCancelled[item.ID] && !l.QueueConsumed[item.ID] {
			l.Queued = append(l.Queued, item)
		}
	}
	for _, id := range jobOrder {
		r := jobs[id]
		if !terminal(r.Job.State) {
			if r.Job.Kind == "task" {
				r.Job.State = "recovering"
			} else {
				r.Job.State = "unknown"
				r.Job.Reason = "outcome_unknown"
				r.Job.Cleanup = "blocked"
			}
		}
		if i, ok := runs[r.Job.RunID]; ok && l.Runs[i].Finished || l.StopRequests[r.Job.RunID] != "" {
			r.CancelRequested = true
		}
		if i, ok := runs[observedRuns[id]]; ok && l.Runs[i].Finished || l.StopRequests[observedRuns[id]] != "" {
			r.CancelRequested = true
		}
		key := "job:" + id + ":terminal"
		queueID, hasReceipt := keys[key]
		r.DeliveryPending = !hasReceipt || (!l.QueueConsumed[queueID] && !l.QueueCancelled[queueID])
		l.Jobs = append(l.Jobs, r.Job)
		l.Recoveries = append(l.Recoveries, *r)
	}
	return nil
}

// LogicalUsage returns the unique receipts and ordinary turns belonging to one logical run.
func (l *Loaded) LogicalUsage(logicalID string) (types.Usage, int, time.Time) {
	var usage types.Usage
	turns := 0
	var started time.Time
	for _, r := range l.Runs {
		if r.LogicalRunID == logicalID {
			usage.Add(r.Usage)
			turns += r.Turns
			if started.IsZero() || r.StartedAt.Before(started) {
				started = r.StartedAt
			}
		}
	}
	return usage, turns, started
}

// SortedUsageSources returns stable source ordering for recovery imports.
func (l *Loaded) SortedUsageSources() []string {
	keys := make([]string, 0, len(l.UsageSources))
	for key := range l.UsageSources {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
