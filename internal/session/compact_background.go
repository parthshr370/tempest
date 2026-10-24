package session

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/compaction"
	"go.harness.dev/harness/internal/conversation"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/taskstate"
	"go.harness.dev/harness/internal/work"
	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/extension"
)

// CompactionTaskName identifies the host's native summary phase.
const CompactionTaskName = "_tempest.compaction"

type compactionTaskKey struct{}

// CompactionController uses the ordinary durable phase scheduler for summaries.
type CompactionController = compactionRunner

type compactionState struct {
	mu          sync.Mutex
	tasks       *taskstate.Runtime
	active      string
	ctx         context.Context
	cancel      context.CancelFunc
	workers     sync.WaitGroup
	closed      bool
	notifyUsage func(string, string, types.Usage)
	running     map[string]bool
	err         error
}
type compactionCapture struct {
	Epoch    string                        `json:"epoch"`
	Source   string                        `json:"source"`
	Snapshot *work.Artifact                `json:"snapshot"`
	Count    int                           `json:"count"`
	Settings compaction.CompactionSettings `json:"settings"`
	Model    string                        `json:"model"`
}
type compactionResult struct {
	Capture  compactionCapture `json:"capture"`
	Snapshot *work.Artifact    `json:"snapshot,omitempty"`
	Changed  bool              `json:"changed"`
}

// NativeCompactionDefinition pins the built-in phase independently of extensions.
func NativeCompactionDefinition() taskstate.Definition {
	return taskstate.Definition{Revision: "1", Release: func() {}, Task: extension.TaskDefinition{Name: CompactionTaskName, Version: 1, InputSchema: json.RawMessage(`{"type":"object","required":["epoch","source","snapshot","count"]}`), StateSchema: json.RawMessage(`{"type":"object"}`), Initial: json.RawMessage(`{}`)}}
}

// BindTasks connects the existing session scheduler after stack construction.
func (r *compactionRunner) BindTasks(tasks *taskstate.Runtime, usage func(string, string, types.Usage)) {
	r.background.mu.Lock()
	defer r.background.mu.Unlock()
	r.background.tasks = tasks
	r.background.notifyUsage = usage
}

// InvokeTask runs only summary work. Context placement stays at a tool boundary.
func (r *compactionRunner) InvokeTask(ctx context.Context, task extension.Task, abort bool) (extension.TaskDecision, error) {
	if abort {
		return extension.TaskDecision{Kind: "complete"}, nil
	}
	retired, err := r.disposed(ctx, task.ID)
	if err != nil {
		return extension.TaskDecision{}, err
	}
	if retired {
		return extension.TaskDecision{Kind: "complete"}, nil
	}
	ctx = context.WithValue(ctx, compactionTaskKey{}, task.ID)
	var capture compactionCapture
	if err := json.Unmarshal(task.Input, &capture); err != nil {
		return extension.TaskDecision{}, err
	}
	epoch, err := r.store.ContextEpoch(ctx)
	if err != nil {
		return extension.TaskDecision{}, err
	}
	if epoch != capture.Epoch {
		return extension.TaskDecision{Kind: "complete", Result: json.RawMessage(`{"stale":true}`)}, nil
	}
	if capture.Model != r.model.Provider+"/"+r.model.ID {
		return extension.TaskDecision{}, fmt.Errorf("captured compaction model unavailable")
	}
	messages, err := r.store.ReadSnapshot("capture_"+capture.Source, capture.Source, capture.Snapshot)
	if err != nil {
		return extension.TaskDecision{}, err
	}
	replacement, changed, err := r.summarizeUsing(ctx, messages, true, false, capture.Settings)
	if err != nil {
		return extension.TaskDecision{}, err
	}
	result := compactionResult{Capture: capture, Changed: changed}
	if changed {
		result.Snapshot, err = r.store.WriteSnapshot("result_"+task.ID, replacement)
		if err != nil {
			return extension.TaskDecision{}, err
		}
	}
	raw, err := json.Marshal(result)
	return extension.TaskDecision{Kind: "complete", Result: raw}, err
}

// Start drives one admitted task using the controller's session lifetime.
func (r *compactionRunner) Start(id string) {
	b := r.background
	b.mu.Lock()
	if b.closed || b.running[id] {
		b.mu.Unlock()
		return
	}
	if b.running == nil {
		b.running = make(map[string]bool)
	}
	b.running[id] = true
	b.err = nil
	b.workers.Add(1)
	ctx, tasks := b.ctx, b.tasks
	b.mu.Unlock()
	go func() {
		defer b.workers.Done()
		err := tasks.ResumeTask(ctx, id)
		if task, getErr := tasks.Get(id); getErr == nil && (task.Status == "parked" || taskstate.Terminal(task) && task.Status != "completed") {
			if retireErr := r.retire(context.WithoutCancel(ctx), task.ID, task.Status+": "+task.Reason); retireErr != nil {
				err = retireErr
			}
		}
		b.mu.Lock()
		delete(b.running, id)
		if err != nil && ctx.Err() == nil {
			b.err = err
		}
		b.mu.Unlock()
	}()
}

// Request coalesces manual and automatic requests into one durable summary task.
func (r *compactionRunner) Request(ctx context.Context, messages []types.Message) (extension.Task, error) {
	b := r.background
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return extension.Task{}, durable.ErrClosed
	}
	if b.tasks == nil {
		return extension.Task{}, fmt.Errorf("compaction scheduler unavailable")
	}
	// Completed but unplaced work also owns the cut. Don't pay for a weaker cut.
	if b.active != "" {
		task, err := b.tasks.Get(b.active)
		if err != nil {
			return extension.Task{}, err
		}
		disposed, err := r.disposed(ctx, task.ID)
		if err != nil {
			return extension.Task{}, err
		}
		if !disposed && task.Status != "parked" && (!taskstate.Terminal(task) || task.Status == "completed") {
			return task, nil
		}
		if !disposed {
			if err := r.retireLocked(ctx, task.ID, task.Status+": "+task.Reason); err != nil {
				return extension.Task{}, err
			}
		}
		b.active = ""
	}
	for _, task := range b.tasks.Snapshot() {
		disposed, err := r.disposed(ctx, task.ID)
		if err != nil {
			return extension.Task{}, err
		}
		if task.Definition == CompactionTaskName && !disposed && task.Status != "parked" && (!taskstate.Terminal(task) || task.Status == "completed") {
			b.active = task.ID
			return task, nil
		}
	}
	detached, err := conversation.Clone(messages)
	if err != nil {
		return extension.Task{}, err
	}
	if err := conversation.Safe(detached); err != nil {
		return extension.Task{}, err
	}
	source, err := stableHash(detached)
	if err != nil {
		return extension.Task{}, err
	}
	epoch, err := r.store.ContextEpoch(ctx)
	if err != nil {
		return extension.Task{}, err
	}
	identity, err := stableHash(struct {
		Epoch, Source, Model string
		Settings             compaction.CompactionSettings
	}{epoch, source, r.model.Provider + "/" + r.model.ID, r.settings})
	if err != nil {
		return extension.Task{}, err
	}
	snapshot, err := r.store.WriteSnapshot("capture_"+source, detached)
	if err != nil {
		return extension.Task{}, err
	}
	capture := compactionCapture{Epoch: epoch, Source: source, Snapshot: snapshot, Count: len(detached), Settings: r.settings, Model: r.model.Provider + "/" + r.model.ID}
	raw, err := json.Marshal(capture)
	if err != nil {
		return extension.Task{}, err
	}
	task, err := b.tasks.Create(ctx, extension.TaskRequest{ID: "compact_" + identity[:32], Definition: CompactionTaskName, Input: raw, Background: true})
	if err == nil {
		disposed, readErr := r.disposed(ctx, task.ID)
		if readErr != nil {
			return extension.Task{}, readErr
		}
		if !disposed {
			b.active = task.ID
		}
	}
	return task, err
}

// Wait observes task completion. Cancelling this wait doesn't suspend its phase.
func (r *compactionRunner) Wait(ctx context.Context, id string) (extension.Task, error) {
	r.Start(id)
	for {
		changed := r.background.tasks.Changes()
		task, err := r.background.tasks.Get(id)
		if err != nil {
			return extension.Task{}, err
		}
		if taskstate.Terminal(task) || task.Status == "parked" {
			if task.Status != "completed" {
				if err := r.retire(context.WithoutCancel(ctx), task.ID, task.Status+": "+task.Reason); err != nil {
					return task, err
				}
				return task, fmt.Errorf("compaction %s: %s", task.Status, task.Reason)
			}
			return task, nil
		}
		r.background.mu.Lock()
		driveErr := r.background.err
		r.background.mu.Unlock()
		if driveErr != nil {
			return task, driveErr
		}
		select {
		case <-ctx.Done():
			return task, ctx.Err()
		case <-r.background.ctx.Done():
			return task, durable.ErrClosed
		case <-changed:
		}
	}
}

func (r *compactionRunner) place(ctx context.Context, messages []types.Message, task extension.Task) ([]types.Message, bool, error) {
	if !taskstate.Terminal(task) && task.Status != "parked" {
		return messages, false, nil
	}
	if task.Status != "completed" {
		return messages, false, r.retire(ctx, task.ID, task.Status+": "+task.Reason)
	}
	disposed, err := r.disposed(ctx, task.ID)
	if err != nil || disposed {
		return messages, false, err
	}
	var capture compactionCapture
	if err := json.Unmarshal(task.Input, &capture); err != nil {
		return nil, false, err
	}
	epoch, err := r.store.ContextEpoch(ctx)
	if err != nil {
		return nil, false, err
	}
	b := r.background
	retire := func(reason string) ([]types.Message, bool, error) {
		return messages, false, r.retire(ctx, task.ID, reason)
	}
	if epoch != capture.Epoch {
		return retire("stale epoch")
	}
	var result compactionResult
	if err := json.Unmarshal(task.Result, &result); err != nil {
		return nil, false, err
	}
	if !result.Changed {
		return retire("no cut")
	}
	if len(messages) < capture.Count {
		return retire("superseded prefix")
	}
	source, err := stableHash(messages[:capture.Count])
	if err != nil {
		return nil, false, err
	}
	if source != capture.Source {
		return retire("superseded prefix")
	}
	prior, err := r.store.Read(ctx, durable.Query{Kind: durable.DocumentKind, Namespace: "_tempest/compaction", ID: task.ID, Limit: 1})
	if err != nil {
		return nil, false, err
	}
	if len(prior.Records) != 0 {
		b.mu.Lock()
		if b.active == task.ID {
			b.active = ""
		}
		b.mu.Unlock()
		return messages, false, nil
	}
	replacement, err := r.store.ReadSnapshot("result_"+task.ID, source, result.Snapshot)
	if err != nil {
		return nil, false, err
	}
	replacement = append(replacement, messages[capture.Count:]...)
	if reflect.DeepEqual(replacement, messages) {
		return retire("no change")
	}
	digest, err := stableHash(replacement)
	if err != nil {
		return nil, false, err
	}
	id := "place_" + digest[:32]
	snapshot, err := r.store.WriteSnapshot(id, replacement)
	if err != nil {
		return nil, false, err
	}
	err = r.store.AppendCompactionPlacement(ctx, journal.CompactionEntry{SourceHash: source, CompactionID: id, Snapshot: snapshot, TokensBefore: compaction.EstimateContextTokens(messages).Tokens}, task.ID)
	if err != nil {
		return nil, false, err
	}
	b.mu.Lock()
	if b.active == task.ID {
		b.active = ""
	}
	b.mu.Unlock()
	return replacement, true, nil
}

// Place adopts completed work only when its epoch and captured prefix still match.
func (r *compactionRunner) Place(ctx context.Context, messages []types.Message, task extension.Task) ([]types.Message, bool, error) {
	return r.place(ctx, messages, task)
}

func (r *compactionRunner) backgroundTurn(ctx context.Context, c agent.ShouldStopAfterTurnContext) ([]types.Message, bool, error) {
	b := r.background
	b.mu.Lock()
	active := b.active
	b.mu.Unlock()
	messages := c.Context.Messages
	if active != "" {
		task, err := b.tasks.Get(active)
		if err != nil {
			return nil, false, err
		}
		r.Start(active)
		hard := compaction.ShouldCompact(compaction.EstimateContextTokens(messages).Tokens, r.contextWindow, r.settings)
		if hard && !taskstate.Terminal(task) {
			task, err = r.Wait(ctx, active)
			if err != nil {
				return nil, false, err
			}
		}
		if taskstate.Terminal(task) {
			return r.place(ctx, messages, task)
		}
		return messages, false, nil
	}
	tokens := compaction.EstimateContextTokens(messages).Tokens
	soft, hard := compaction.Thresholds(r.contextWindow, r.settings)
	if !r.settings.Enabled || tokens < soft {
		return messages, false, nil
	}
	task, err := r.Request(ctx, messages)
	if err != nil {
		return nil, false, err
	}
	if tokens >= hard {
		task, err = r.Wait(ctx, task.ID)
		if err != nil {
			return nil, false, err
		}
		return r.place(ctx, messages, task)
	}
	r.Start(task.ID)
	return messages, false, nil
}

// Close suspends and joins this controller's local scheduler waiters.
func (r *compactionRunner) Close() error {
	if r == nil || r.background == nil {
		return nil
	}
	b := r.background
	b.mu.Lock()
	b.closed = true
	b.cancel()
	b.mu.Unlock()
	b.workers.Wait()
	return nil
}

// Recover selects committed, unplaced summary work without dispatching it.
func (r *compactionRunner) Recover(ctx context.Context) error {
	query := durable.Query{Kind: durable.TaskKind, Namespace: taskstate.Namespace, Reverse: true, Limit: 100}
	for {
		page, err := r.store.Read(ctx, query)
		if err != nil {
			return err
		}
		for _, record := range page.Records {
			var task extension.Task
			if err := json.Unmarshal(record.Value, &task); err != nil {
				return err
			}
			if task.Definition != CompactionTaskName {
				continue
			}
			if task.Status == "parked" || taskstate.Terminal(task) && task.Status != "completed" {
				if err := r.retire(ctx, task.ID, task.Status+": "+task.Reason); err != nil {
					return err
				}
				continue
			}
			disposition, err := r.store.Read(ctx, durable.Query{Kind: durable.DocumentKind, Namespace: "_tempest/compaction", ID: task.ID, Limit: 1})
			if err != nil {
				return err
			}
			if len(disposition.Records) == 0 {
				r.background.mu.Lock()
				r.background.active = task.ID
				r.background.mu.Unlock()
				return nil
			}
		}
		if page.NextCursor == "" {
			return nil
		}
		query.Cursor = page.NextCursor
	}
}

func (r *compactionRunner) recoverOverflow(ctx context.Context, messages []types.Message) ([]types.Message, bool, error) {
	if r.store == nil || !r.settings.Enabled {
		return messages, false, nil
	}
	claimed, err := r.store.ClaimOverflow(ctx)
	if err != nil || !claimed {
		return messages, false, err
	}
	if r.background.tasks == nil {
		return r.summarize(ctx, messages, true, true)
	}
	task, err := r.Request(ctx, messages)
	if err != nil {
		return nil, false, err
	}
	task, err = r.Wait(ctx, task.ID)
	if err != nil {
		return nil, false, err
	}
	return r.place(ctx, messages, task)
}

func (r *compactionRunner) disposed(ctx context.Context, id string) (bool, error) {
	page, err := r.store.Read(ctx, durable.Query{Kind: durable.DocumentKind, Namespace: "_tempest/compaction", ID: id, Limit: 1})
	return len(page.Records) != 0, err
}

func (r *compactionRunner) retireLocked(ctx context.Context, id, reason string) error {
	value, err := json.Marshal(reason)
	if err != nil {
		return err
	}
	_, err = r.store.Commit(ctx, durable.Commit{ID: "compaction:retire:" + id, Changes: []durable.Change{{SessionID: r.store.ID(), Kind: durable.DocumentKind, Namespace: "_tempest/compaction", ID: id, Version: 1, Value: value}}})
	if err == nil && r.background.active == id {
		r.background.active = ""
	}
	return err
}

func (r *compactionRunner) retire(ctx context.Context, id, reason string) error {
	r.background.mu.Lock()
	defer r.background.mu.Unlock()
	already, err := r.disposed(ctx, id)
	if err != nil {
		return err
	}
	if already {
		if r.background.active == id {
			r.background.active = ""
		}
		return nil
	}
	return r.retireLocked(ctx, id, reason)
}
