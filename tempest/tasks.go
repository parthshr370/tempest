package tempest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/taskstate"
	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/extension"
)

// Task is the shared Go and subprocess workflow snapshot.
type Task = extension.Task

// TaskRequest is a durable root or owned-child admission.
type TaskRequest = extension.TaskRequest

// TaskOptions controls phase recovery and the host clock. Nil Now uses time.Now.
type TaskOptions struct {
	Now              func() time.Time
	MaxCrashAttempts uint32
}

func (s *Session) configureTasks(ctx context.Context, cfg StackConfig, settings TaskOptions) error {
	options := taskstate.Options{SessionID: s.ID(), Store: s.core.store, Now: settings.Now, MaxCrashes: settings.MaxCrashAttempts}
	options.OwnedSettled = s.core.jobs.OwnedTasksSettled
	options.AbortOwned = s.core.jobs.AbortOwnedTasks
	if cfg.Extensions != nil {
		cfg.Extensions.TaskOutput = func(_ context.Context, task extension.Task, chunk extension.OutputChunk) error {
			s.core.hub.Emit(update("tempest/task_output", map[string]any{"taskId": task.ID, "phase": task.Phase, "attempt": task.Attempt, "chunk": chunk}, time.Now()))
			return nil
		}
		options.Resolve = cfg.Extensions.ResolveTask
		options.Invoke = func(ctx context.Context, definition taskstate.Definition, task extension.Task, abort bool) (extension.TaskDecision, error) {
			run, _ := s.core.jobs.PhaseRun()
			runID := task.RunID
			if !task.Background && run.LogicalRunID == task.RunID {
				runID = run.RunID
			}
			return cfg.Extensions.InvokeTask(ctx, s.core.jobs.Tasks, definition, task, runID, abort)
		}
	}
	runtime, err := taskstate.New(ctx, options)
	if err != nil {
		return err
	}
	s.core.jobs.Tasks = runtime
	s.core.taskContext, s.core.cancelTasks = context.WithCancel(context.Background())
	return nil
}

// CreateTask admits a phase workflow. Background roots belong to the session;
// ordinary roots require the current conversational run and its permission tier.
func (s *Session) CreateTask(ctx context.Context, request TaskRequest) (Task, error) {
	if err := s.authorize(ctx, "create_task", "", request.ID); err != nil {
		return Task{}, err
	}
	s.core.mu.Lock()
	closed := s.core.closed
	active := s.core.active
	plan := s.core.plan || s.core.mode == permission.ModePlan
	s.core.mu.Unlock()
	if closed {
		return Task{}, ErrClosed
	}
	if plan {
		return Task{}, errors.New("plan mode cannot create tasks")
	}
	if request.Owner == "" && !request.Background {
		if active == nil {
			return Task{}, errors.New("foreground task requires an active run")
		}
		run, admits := s.core.jobs.PhaseRun()
		if !admits || run.LogicalRunID == "" {
			return Task{}, errors.New("foreground task admission is sealed")
		}
		if request.RunID != "" && request.RunID != run.LogicalRunID {
			return Task{}, errors.New("foreign task run")
		}
		request.RunID = run.LogicalRunID
	}
	return s.core.jobs.Tasks.Create(ctx, request)
}

// Task reads a committed workflow without running it.
func (s *Session) Task(ctx context.Context, id string) (Task, error) {
	if err := s.authorize(ctx, "get_task", "", id); err != nil {
		return Task{}, err
	}
	return s.core.jobs.Tasks.Get(id)
}

// TaskQuery selects one immutable page of task summaries.
type TaskQuery struct {
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// TaskPage includes terminal history without retaining it in the scheduler.
type TaskPage struct {
	Tasks      []extension.TaskSummary `json:"tasks"`
	NextCursor string                  `json:"nextCursor,omitempty"`
	Seq        uint64                  `json:"seq"`
}

// Tasks pages summaries. Task reads a complete individual record.
func (s *Session) Tasks(ctx context.Context, query TaskQuery) (TaskPage, error) {
	if err := s.authorize(ctx, "list_tasks", "", ""); err != nil {
		return TaskPage{}, err
	}
	const maxTaskPage = 100
	if query.Limit < 0 || query.Limit > maxTaskPage {
		return TaskPage{}, fmt.Errorf("task limit must be between 1 and %d", maxTaskPage)
	}
	page, err := s.core.store.Read(ctx, durable.Query{Kind: durable.TaskKind, Namespace: taskstate.Namespace, Limit: query.Limit, Cursor: query.Cursor, Reverse: true})
	if err != nil {
		return TaskPage{}, err
	}
	result := TaskPage{Tasks: make([]extension.TaskSummary, 0, len(page.Records)), NextCursor: page.NextCursor, Seq: page.Seq}
	for _, record := range page.Records {
		var task Task
		if err := json.Unmarshal(record.Value, &task); err != nil {
			return TaskPage{}, err
		}
		result.Tasks = append(result.Tasks, task.Summary())
	}
	return result, nil
}

// CancelTask records abort for the owned subtree, joins live phases, then runs cleanup.
func (s *Session) CancelTask(ctx context.Context, id string) error {
	if err := s.authorize(ctx, "cancel_task", "", id); err != nil {
		return err
	}
	if err := s.core.jobs.Tasks.Abort(ctx, id, "cancelled"); err != nil {
		return err
	}
	return s.core.jobs.Tasks.ResumeTask(ctx, id)
}

// RetryTask explicitly unblocks a parked phase after correcting its definition.
func (s *Session) RetryTask(ctx context.Context, id string) error {
	if err := s.authorize(ctx, "retry_task", "", id); err != nil {
		return err
	}
	return s.core.jobs.Tasks.Retry(ctx, id)
}

// CancelAll includes session-background roots. Cancel alone retains them.
func (s *Session) CancelAll(ctx context.Context) error {
	if err := s.authorize(ctx, "cancel_all", "", ""); err != nil {
		return err
	}
	s.Cancel()
	for _, task := range s.core.jobs.Tasks.Snapshot() {
		if task.Owner == "" && !taskstate.Terminal(task) {
			if err := s.core.jobs.Tasks.Abort(ctx, task.ID, "cancel_all"); err != nil {
				return err
			}
		}
	}
	return s.core.jobs.Tasks.Resume(ctx)
}

// NextWake returns the earliest persisted deadline without invoking code.
func (s *Session) NextWake() (time.Time, bool) { return s.core.jobs.Tasks.NextWake() }

// awaitForeground keeps the existing run idle while phase work owns it. There
// is at most one alarm here, not one sleeping goroutine per reminder.
func (c *sessionCore) awaitForeground(ctx context.Context) (bool, error) {
	c.mu.Lock()
	active := c.active
	c.mu.Unlock()
	if active == nil {
		return false, nil
	}
	run, _ := c.jobs.PhaseRun()
	for {
		taskChanges, nativeChanges := c.jobs.Tasks.Changes(), c.jobs.Changes()
		// A cancelled idle context probes quiescence without starting more work.
		if ctx.Err() == nil {
			if err := c.jobs.Tasks.ResumeRun(ctx, run.LogicalRunID); err != nil {
				return false, err
			}
		}
		pending := false
		nativePending := false
		for _, task := range c.jobs.Tasks.Snapshot() {
			if !task.Background && task.RunID == run.LogicalRunID && !taskstate.Terminal(task) && task.Status != "parked" {
				pending = true
				nativePending = nativePending || !c.jobs.OwnedTasksSettled(task.ID)
			}
		}
		if !pending {
			return false, nil
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		queuePending, queueChanges := c.queues.Pending()
		if queuePending {
			return true, nil
		}
		deadline, timed := c.jobs.Tasks.NextWakeRun(run.LogicalRunID)
		if !timed && !nativePending {
			// Parked dependencies retain their graph, not a conversational run.
			return false, nil
		}
		var timer *time.Timer
		var alarm <-chan time.Time
		if timed {
			timer = time.NewTimer(time.Until(deadline))
			alarm = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return false, ctx.Err()
		case <-taskChanges:
		case <-nativeChanges:
		case <-alarm:
		case <-queueChanges:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func (host *Host) archivedTaskAlarms(ctx context.Context) (map[string]time.Time, error) {
	alarms := make(map[string]time.Time)
	var scanErr error
	now := time.Now()
	if host.options.Options.Tasks.Now != nil {
		now = host.options.Options.Tasks.Now()
	}
	if host.options.Store != nil {
		graphs := make(map[string][]extension.Task)
		query := durable.Query{Kind: durable.TaskKind, Namespace: "_tempest/tasks", Limit: 100}
		for {
			page, err := host.options.Store.Read(ctx, query)
			if err != nil {
				return alarms, errors.Join(scanErr, err)
			}
			for _, record := range page.Records {
				var task Task
				if record.Version != 1 || json.Unmarshal(record.Value, &task) != nil {
					scanErr = errors.Join(scanErr, fmt.Errorf("session %s: %w: invalid archived task", record.SessionID, durable.ErrCorrupt))
					continue
				}
				graphs[record.SessionID] = append(graphs[record.SessionID], task)
			}
			if page.NextCursor == "" {
				for id, graph := range graphs {
					if wake, present := taskstate.Alarm(graph, now); present {
						alarms[id] = wake
					}
				}
				return alarms, scanErr
			}
			query.Cursor = page.NextCursor
		}
	}
	entries, err := journal.Catalog(ctx, host.options.SessionDir, "")
	if err != nil {
		return alarms, err
	}
	for _, entry := range entries {
		if !entry.HostManaged || !entry.Initialized {
			continue
		}
		wake, present, err := journal.TaskWake(ctx, entry.Path, now)
		if err != nil {
			scanErr = errors.Join(scanErr, fmt.Errorf("session %s: %w", entry.Header.ID, err))
			continue
		}
		if present {
			alarms[entry.Header.ID] = wake
		}
	}
	return alarms, scanErr
}
