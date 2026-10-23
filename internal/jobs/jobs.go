// Package jobs supervises run-owned children and processes through durable host callbacks.
package jobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.harness.dev/harness/internal/work"
	"go.harness.dev/harness/internal/worktree"
)

var (
	ErrJobNotFound       = errors.New("job_not_found")
	ErrJobLimit          = errors.New("job_limit")
	ErrJobNotCleanupable = errors.New("job_not_cleanupable")
	ErrRecoveryRequired  = errors.New("recovery_required")
	ErrInvalidInput      = errors.New("invalid job input")
)

const (
	maxActiveKind      = 4
	maxCredits         = 8
	maxAccepted        = 64
	maxRetained        = 16
	maxResultBytes     = 1 << 20
	maxPreviewBytes    = 4 << 10
	maxCompletionBytes = 8 << 10
	maxLogBytes        = 16 << 20
	maxTimeoutSeconds  = 2147483
	maxPromptBytes     = 32 << 10
	maxDescription     = 512
	maxJobPage         = 50
	maxLogPage         = 64 << 10
	maxWait            = 300 * time.Second
)

// Options preserves the synchronous shell's configuration for async commands.
type Options struct {
	ShellPath     string
	CommandPrefix string
	BashTimeout   time.Duration
	OnError       func(error)
}

type entry struct {
	transition  sync.Mutex
	recovery    work.Recovery
	schema      *jsonschema.Schema
	cancel      context.CancelFunc
	done        chan struct{}
	live        bool
	credited    bool
	delivered   bool
	joined      bool
	truncated   bool
	outputBytes int64
}

// Manager owns every worker until its terminal fact and completion delivery settle.
type Manager struct {
	host      work.Host
	options   Options
	admission sync.Mutex
	mu        sync.Mutex
	entries   map[string]*entry
	run       work.RunInfo
	scope     context.Context
	cancel    context.CancelFunc
	sealed    bool
	fault     error
	changed   chan struct{}
	finish    sync.Mutex
	finished  bool
	accepted  map[string]int
	workers   sync.WaitGroup
}

var _ work.Controller = (*Manager)(nil)
var _ work.RunHooks = (*Manager)(nil)

// Identity uses the canonical occurrence id, never the provider's reusable call id.
func Identity(sessionID, callID string) (jobID, childID, digest string) {
	job := sha256.Sum256([]byte("tempest-job-v1\x00" + sessionID + "\x00" + callID))
	child := sha256.Sum256([]byte("tempest-child-v1\x00" + sessionID + "\x00" + callID))
	digest = hex.EncodeToString(job[:])
	return "job_" + digest[:32], hex.EncodeToString(child[:])[:16], digest
}

// New only restores metadata. Continue or Cancel explicitly activates recovery.
func New(host work.Host, options Options, recoveries []work.Recovery) (*Manager, error) {
	if host.SessionID == "" || host.JournalPath == "" || host.Append == nil || host.Enqueue == nil {
		return nil, errors.New("jobs require a durable session host")
	}
	if options.ShellPath == "" {
		options.ShellPath = "/bin/bash"
	}
	if options.BashTimeout <= 0 {
		options.BashTimeout = 10 * time.Minute
	}
	manager := &Manager{host: host, options: options, entries: make(map[string]*entry), accepted: make(map[string]int), changed: make(chan struct{}), sealed: true}
	for _, recovery := range recoveries {
		if recovery.Job.Kind == "task" && (recovery.Task == nil || recovery.Environment == nil || recovery.ChildJournal == "") {
			return nil, errors.New("incomplete task recovery intent")
		}
		if recovery.Job.Kind == "bash" && recovery.Bash == nil {
			return nil, errors.New("incomplete bash recovery intent")
		}
		if recovery.Job.Kind != "task" && recovery.Job.Kind != "bash" {
			return nil, errors.New("invalid recovery job kind")
		}
		jobID, childID, digest := Identity(host.SessionID, recovery.Invocation.CallID)
		if jobID != recovery.Job.ID || recovery.IdentityDigest != digest || recovery.Job.Kind == "task" && childID != recovery.Job.ChildSessionID {
			return nil, errors.New("job recovery identity mismatch")
		}
		if _, exists := manager.entries[jobID]; exists {
			return nil, errors.New("duplicate job recovery")
		}
		recovery = cloneRecovery(recovery)
		record := &entry{recovery: recovery, done: make(chan struct{}), credited: recovery.DeliveryPending}
		if recovery.Job.Kind == "task" && !terminal(recovery.Job.State) {
			record.recovery.Job.State = "recovering"
			record.credited = true
		} else if recovery.Job.Kind == "bash" && !terminal(recovery.Job.State) {
			record.recovery.Job.State = "unknown"
			record.recovery.Job.Reason = "outcome_unknown"
			record.recovery.DeliveryPending = true
			record.credited = true
		}
		record.joined = true
		if !record.recovery.DeliveryPending && terminal(record.recovery.Job.State) {
			close(record.done)
		}
		if recovery.Task != nil {
			var err error
			record.schema, err = compileSchema(recovery.Task.OutputSchema)
			if err != nil {
				return nil, err
			}
		}
		manager.entries[jobID] = record
		manager.accepted[recovery.Job.LogicalRunID]++
	}
	return manager, nil
}

func cloneRecovery(recovery work.Recovery) work.Recovery {
	recovery.Job = cloneJob(recovery.Job)
	if recovery.Task != nil {
		value := *recovery.Task
		value.OutputSchema = bytes.Clone(value.OutputSchema)
		recovery.Task = &value
	}
	if recovery.Bash != nil {
		value := *recovery.Bash
		recovery.Bash = &value
	}
	if recovery.Environment != nil {
		value := *recovery.Environment
		value.Ceiling.Tools = append([]string(nil), value.Ceiling.Tools...)
		value.Ceiling.Overrides = cloneStrings(value.Ceiling.Overrides)
		value.Ceiling.Interactive = cloneStrings(value.Ceiling.Interactive)
		recovery.Environment = &value
	}
	if recovery.Workspace != nil {
		value := *recovery.Workspace
		recovery.Workspace = &value
	}
	return recovery
}
func cloneStrings(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
func cloneJob(job work.Job) work.Job {
	job.Value = bytes.Clone(job.Value)
	if job.Usage.CacheWrite1h != nil {
		value := *job.Usage.CacheWrite1h
		job.Usage.CacheWrite1h = &value
	}
	if job.Usage.Reasoning != nil {
		value := *job.Usage.Reasoning
		job.Usage.Reasoning = &value
	}
	if job.Result != nil {
		value := *job.Result
		job.Result = &value
	}
	if job.Patch != nil {
		value := *job.Patch
		job.Patch = &value
	}
	if job.Output != nil {
		value := *job.Output
		job.Output = &value
	}
	if job.ExitCode != nil {
		value := *job.ExitCode
		job.ExitCode = &value
	}
	return job
}
func terminal(state string) bool {
	return state == "completed" || state == "failed" || state == "cancelled" || state == "unknown"
}
func (manager *Manager) notifyLocked() { close(manager.changed); manager.changed = make(chan struct{}) }
func (manager *Manager) fail(err error) {
	if err == nil {
		return
	}
	manager.mu.Lock()
	first := manager.fault == nil
	if first {
		manager.fault = err
		manager.sealed = true
		manager.notifyLocked()
	}
	cancel := manager.cancel
	manager.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if first && manager.options.OnError != nil {
		manager.options.OnError(err)
	}
}
func (manager *Manager) append(ctx context.Context, kind, runID string, payload json.RawMessage) error {
	err := manager.host.Append(ctx, work.Fact{Kind: kind, RunID: runID, Payload: payload})
	if err != nil && !(ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))) {
		manager.fail(err)
	}
	return err
}
func (manager *Manager) publish(ctx context.Context, job work.Job, action string) error {
	if manager.host.Publish == nil {
		return nil
	}
	manager.mu.Lock()
	runID := manager.run.RunID
	manager.mu.Unlock()
	err := manager.host.Publish(ctx, work.Event{Kind: "job", Job: &job, Action: action, RunID: runID})
	if err != nil {
		manager.fail(err)
	}
	return err
}

// RecoveryRequired reports whether a new prompt would strand an interrupted task.
func (manager *Manager) RecoveryRequired() bool {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	for _, record := range manager.entries {
		if record.recovery.Job.State == "recovering" {
			return true
		}
	}
	return false
}

// Pending reports whether Continue has jobs to recover or completions to enqueue.
func (manager *Manager) Pending() bool {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	for _, record := range manager.entries {
		if record.recovery.Job.State == "recovering" || record.recovery.DeliveryPending {
			return true
		}
	}
	return false
}

// Snapshot returns owned copies; callers choose their own bounded projection.
func (manager *Manager) Snapshot() []work.Job {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	result := make([]work.Job, 0, len(manager.entries))
	for _, record := range manager.entries {
		result = append(result, cloneJob(record.recovery.Job))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

// Start binds children to the accounting-bearing root run context.
func (manager *Manager) Start(ctx context.Context, run work.RunInfo) error {
	if run.RunID == "" || run.SessionID != "" && run.SessionID != manager.host.SessionID {
		return errors.New("invalid durable run identity")
	}
	manager.finish.Lock()
	defer manager.finish.Unlock()
	manager.admission.Lock()
	defer manager.admission.Unlock()
	manager.mu.Lock()
	if manager.fault != nil {
		err := manager.fault
		manager.mu.Unlock()
		return err
	}
	for _, record := range manager.entries {
		if record.live {
			manager.mu.Unlock()
			return errors.New("previous run still owns jobs")
		}
		if record.recovery.Job.State == "recovering" && !run.Resume {
			manager.mu.Unlock()
			return ErrRecoveryRequired
		}
	}
	if manager.cancel != nil {
		manager.cancel()
	}
	manager.scope, manager.cancel = context.WithCancel(ctx)
	manager.run = run
	if manager.run.LogicalRunID == "" {
		manager.run.LogicalRunID = run.RunID
	}
	manager.sealed = false
	manager.finished = false
	records := make([]*entry, 0, len(manager.entries))
	for _, record := range manager.entries {
		if record.recovery.Job.State == "recovering" || record.recovery.DeliveryPending {
			records = append(records, record)
		}
	}
	manager.mu.Unlock()
	sort.Slice(records, func(i, j int) bool { return records[i].recovery.Job.ID < records[j].recovery.Job.ID })
	for _, record := range records {
		if err := manager.adopt(ctx, run, record); err != nil {
			return err
		}
	}
	return manager.error()
}

func (manager *Manager) adopt(ctx context.Context, run work.RunInfo, record *entry) error {
	record.transition.Lock()
	manager.mu.Lock()
	recovery := cloneRecovery(record.recovery)
	logicalRunID := manager.run.LogicalRunID
	manager.mu.Unlock()
	if recovery.Job.State == "recovering" && !recovery.CancelRequested {
		payload, err := json.Marshal(attachedPayload{recovery.Job.ID, run.RunID, logicalRunID})
		if err == nil {
			err = manager.append(ctx, "job_attached", run.RunID, payload)
		}
		if err == nil {
			manager.launch(record, true)
		}
		record.transition.Unlock()
		return err
	}
	if recovery.Job.State == "recovering" || recovery.Job.Kind == "bash" && recovery.Job.State == "unknown" && recovery.Job.FinishedAt.IsZero() {
		record.transition.Unlock()
		job := recovery.Job
		if job.Kind == "task" {
			job.State = "cancelled"
			job.Reason = "cancelled"
			if recovery.Workspace != nil {
				job.Cleanup = "blocked"
			}
		} else {
			job.Reason = "outcome_unknown"
			job.ResultText = "External process may still exist; command was not restarted."
		}
		manager.settle(record, job)
		return manager.error()
	}
	record.transition.Unlock()
	if recovery.DeliveryPending {
		if err := manager.deliver(context.WithoutCancel(ctx), record); err != nil {
			return err
		}
		manager.mu.Lock()
		closeDone(record)
		manager.notifyLocked()
		manager.mu.Unlock()
	}
	return nil
}
func (manager *Manager) error() error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.fault
}

func (manager *Manager) refreshCredits() {
	source, ok := manager.host.Queues.(interface{ PendingJobIDs() []string })
	if !ok {
		return
	}
	pending := make(map[string]bool)
	for _, jobID := range source.PendingJobIDs() {
		pending[jobID] = true
	}
	manager.mu.Lock()
	for jobID, record := range manager.entries {
		if record.credited && !record.live && record.delivered && !pending[jobID] {
			record.credited = false
		}
	}
	manager.mu.Unlock()
}

// capacity admits one more job of kind, or says which limit is full, which
// jobs hold it and what frees it, so the model can act on the refusal.
func (manager *Manager) capacity(kind string, shared, writer bool) error {
	manager.refreshCredits()
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.fault != nil {
		return manager.fault
	}
	if manager.sealed || manager.scope == nil {
		return errors.New("background jobs require an active durable root run")
	}
	if err := manager.scope.Err(); err != nil {
		return err
	}
	var running, sameKind, unread, retained []string
	sharedJob := ""
	for jobID, record := range manager.entries {
		job := record.recovery.Job
		if record.live {
			running = append(running, jobID)
			if job.Kind == kind {
				sameKind = append(sameKind, jobID)
			}
			if job.Isolation == "shared" {
				sharedJob = jobID
			}
		} else if record.credited {
			unread = append(unread, jobID)
		}
		if job.Isolation == "worktree" && (job.Cleanup == "retained" || job.Cleanup == "blocked") {
			retained = append(retained, jobID)
		}
	}
	for _, ids := range [][]string{running, sameKind, unread, retained} {
		sort.Strings(ids)
	}
	switch {
	case shared && sharedJob != "":
		return fmt.Errorf("%w: general task %s is already running directly in the working directory (no isolated git worktree is available here), and only one such task can run at a time. %s, or start an explore task instead", ErrJobLimit, sharedJob, freeRunning([]string{sharedJob}))
	case len(sameKind) >= maxActiveKind:
		return fmt.Errorf("%w: %d background %s jobs are already running, the most allowed at once: %s. %s, then retry", ErrJobLimit, len(sameKind), kind, strings.Join(sameKind, ", "), freeRunning(sameKind))
	case len(running)+len(unread) >= maxCredits:
		message := fmt.Sprintf("%d background jobs are running or have completion notices you have not received yet (limit %d).", len(running)+len(unread), maxCredits)
		if len(running) > 0 {
			message += fmt.Sprintf(" Running: %s.", strings.Join(running, ", "))
		}
		if len(unread) > 0 {
			message += fmt.Sprintf(" Finished, notice pending: %s; their notices arrive after you end this turn (reply without tool calls), which frees their slots.", strings.Join(unread, ", "))
		}
		if len(running) > 0 {
			message += " " + freeRunning(running) + "."
		}
		return fmt.Errorf("%w: %s", ErrJobLimit, message)
	case manager.accepted[manager.run.LogicalRunID] >= maxAccepted:
		return fmt.Errorf("%w: this run has already started %d background jobs, the most one run may start; continue with the jobs you have", ErrJobLimit, maxAccepted)
	case writer && len(retained) >= maxRetained:
		return fmt.Errorf("%w: %d finished general tasks still keep their git worktrees (limit %d): %s. Take what you need from a task's patch, then free its worktree with jobs {\"action\":\"cleanup\",\"job_id\":%q} and retry", ErrJobLimit, len(retained), maxRetained, strings.Join(retained, ", "), retained[0])
	}
	return nil
}

// freeRunning tells the model how to free a slot held by running jobs.
func freeRunning(ids []string) string {
	quoted := make([]string, len(ids))
	for i, jobID := range ids {
		quoted[i] = fmt.Sprintf("%q", jobID)
	}
	return fmt.Sprintf("Wait for one to finish with wait {\"job_ids\":[%s]} or stop one with cancel {\"job_id\":%s}", strings.Join(quoted, ","), quoted[0])
}

func (manager *Manager) invocation(ctx context.Context) (work.Invocation, string, string, string, error) {
	invocation, ok := work.InvocationFromContext(ctx)
	if !ok || invocation.CallID == "" {
		return invocation, "", "", "", errors.New("background work requires a journaled tool occurrence")
	}
	manager.mu.Lock()
	runID := manager.run.RunID
	manager.mu.Unlock()
	if invocation.RunID != runID {
		return invocation, "", "", "", errors.New("tool occurrence belongs to another run")
	}
	jobID, childID, digest := Identity(manager.host.SessionID, invocation.CallID)
	return invocation, jobID, childID, digest, nil
}

// validateTask rejects a task spec with the reason the model needs to fix it.
func validateTask(spec work.TaskSpec) error {
	switch {
	case strings.TrimSpace(spec.Prompt) == "":
		return fmt.Errorf("%w: prompt is required", ErrInvalidInput)
	case len(spec.Prompt) > maxPromptBytes:
		return fmt.Errorf("%w: prompt is %d bytes; the limit is %d", ErrInvalidInput, len(spec.Prompt), maxPromptBytes)
	case len(spec.Description) > maxDescription:
		return fmt.Errorf("%w: description is %d bytes; the limit is %d", ErrInvalidInput, len(spec.Description), maxDescription)
	case spec.SubagentType != "explore" && spec.SubagentType != "general":
		return fmt.Errorf("%w: subagent_type must be \"explore\" or \"general\"", ErrInvalidInput)
	}
	return nil
}

// StartTask returns after durable intent; workspace setup and provider work stay in its worker.
func (manager *Manager) StartTask(ctx context.Context, spec work.TaskSpec) (work.Job, error) {
	if err := validateTask(spec); err != nil {
		return work.Job{}, err
	}
	compiled, err := compileSchema(spec.OutputSchema)
	if err != nil {
		return work.Job{}, err
	}
	if manager.host.PrepareTask == nil || manager.host.RunTask == nil {
		return work.Job{}, errors.New("task runner is unavailable")
	}
	manager.admission.Lock()
	defer manager.admission.Unlock()
	invocation, jobID, childID, digest, err := manager.invocation(ctx)
	if err != nil {
		return work.Job{}, err
	}
	if prior, exists, err := manager.existing(jobID, digest); exists || err != nil {
		return prior, err
	}
	if err = manager.capacity("task", false, spec.SubagentType == "general"); err != nil {
		return work.Job{}, err
	}
	environment, err := manager.host.PrepareTask(ctx, spec)
	if err != nil {
		return work.Job{}, err
	}
	var workspace *work.Workspace
	isolation := ""
	cwd := manager.host.Cwd
	if spec.SubagentType == "general" {
		workspace, err = worktree.Plan(ctx, cwd, manager.host.JournalPath, jobID, digest)
		if err != nil {
			return work.Job{}, err
		}
		isolation = "shared"
		if workspace != nil {
			isolation = "worktree"
			cwd = filepath.Join(workspace.Path, workspace.RelativeCwd)
		}
		if err = manager.capacity("task", workspace == nil, true); err != nil {
			if workspace != nil {
				err = errors.Join(err, worktree.DropPin(context.WithoutCancel(ctx), *workspace, jobID))
			}
			return work.Job{}, err
		}
	}
	manager.mu.Lock()
	run := manager.run
	manager.mu.Unlock()
	job := work.Job{ID: jobID, Kind: "task", State: "starting", RunID: invocation.RunID, LogicalRunID: run.LogicalRunID, ToolCallID: invocation.ToolCallID, ChildSessionID: childID, Description: spec.Description, StartedAt: time.Now().UTC(), Cwd: cwd, Isolation: isolation, Cleanup: "none"}
	if workspace != nil {
		job.BaseCommit = workspace.BaseCommit
		job.Cleanup = "retained"
	}
	childJournal := filepath.Join(manager.host.JournalPath+".d", "children", childID, "session.jsonl")
	recovery := work.Recovery{Job: job, Invocation: invocation, IdentityDigest: digest, Task: &spec, Environment: &environment, Workspace: workspace, ChildJournal: childJournal}
	record := &entry{recovery: cloneRecovery(recovery), schema: compiled, done: make(chan struct{}), credited: true}
	if err = manager.commitStart(ctx, record); err != nil {
		if workspace != nil {
			err = errors.Join(err, worktree.DropPin(context.WithoutCancel(ctx), *workspace, jobID))
		}
		return work.Job{}, err
	}
	manager.launch(record, false)
	return cloneJob(job), nil
}
func (manager *Manager) existing(jobID, digest string) (work.Job, bool, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if record, ok := manager.entries[jobID]; ok {
		if record.recovery.IdentityDigest != digest {
			return work.Job{}, false, errors.New("job identity collision")
		}
		return cloneJob(record.recovery.Job), true, nil
	}
	return work.Job{}, false, nil
}

// StartBash never starts a process before its job intent is synced.
func (manager *Manager) StartBash(ctx context.Context, spec work.BashSpec) (work.Job, error) {
	if strings.TrimSpace(spec.Command) == "" {
		return work.Job{}, fmt.Errorf("%w: command is required", ErrInvalidInput)
	}
	if spec.TimeoutSeconds < 0 || spec.TimeoutSeconds > maxTimeoutSeconds {
		return work.Job{}, fmt.Errorf("%w: timeout must be between 1 and %d seconds", ErrInvalidInput, maxTimeoutSeconds)
	}
	if spec.TimeoutSeconds == 0 {
		spec.TimeoutSeconds = int(manager.options.BashTimeout / time.Second)
		if spec.TimeoutSeconds < 1 {
			spec.TimeoutSeconds = 1
		}
	}
	manager.admission.Lock()
	defer manager.admission.Unlock()
	invocation, jobID, _, digest, err := manager.invocation(ctx)
	if err != nil {
		return work.Job{}, err
	}
	if prior, exists, err := manager.existing(jobID, digest); exists || err != nil {
		return prior, err
	}
	if err = manager.capacity("bash", false, false); err != nil {
		return work.Job{}, err
	}
	manager.mu.Lock()
	run := manager.run
	manager.mu.Unlock()
	job := work.Job{ID: jobID, Kind: "bash", State: "starting", RunID: invocation.RunID, LogicalRunID: run.LogicalRunID, ToolCallID: invocation.ToolCallID, StartedAt: time.Now().UTC(), Cwd: manager.host.Cwd, Cleanup: "none"}
	record := &entry{recovery: work.Recovery{Job: job, Invocation: invocation, IdentityDigest: digest, Bash: &spec}, done: make(chan struct{}), credited: true}
	if err = manager.commitStart(ctx, record); err != nil {
		return work.Job{}, err
	}
	manager.launch(record, false)
	return cloneJob(job), nil
}

func (manager *Manager) launch(record *entry, resume bool) {
	manager.mu.Lock()
	scope := manager.scope
	jobCtx, cancel := context.WithCancel(scope)
	if record.recovery.CancelRequested {
		cancel()
	}
	record.cancel = cancel
	record.live = true
	record.joined = false
	manager.workers.Add(1)
	job := cloneJob(record.recovery.Job)
	manager.notifyLocked()
	manager.mu.Unlock()
	go func() {
		defer manager.workers.Done()
		defer cancel()
		defer func() {
			if failure := recover(); failure != nil {
				job.State = "failed"
				job.Reason = "worker_panic"
				job.ResultText = fmt.Sprintf("Background worker panic: %.512v", failure)
				manager.settle(record, job)
			}
		}()
		if job.Kind == "bash" {
			manager.runBash(jobCtx, record)
			return
		}
		manager.runTask(jobCtx, record, resume)
	}()
}
func (manager *Manager) runTask(ctx context.Context, record *entry, resume bool) {
	manager.mu.Lock()
	recovery := cloneRecovery(record.recovery)
	manager.mu.Unlock()
	job := recovery.Job
	if recovery.Workspace != nil {
		if err := worktree.Ensure(ctx, *recovery.Workspace, job.ID, recovery.IdentityDigest); err != nil {
			job.State = "failed"
			job.Reason = "workspace_error"
			job.ResultText = preview(err.Error(), maxPreviewBytes)
			manager.settle(record, job)
			return
		}
	}
	if err := worktree.PrivateDir(filepath.Dir(recovery.ChildJournal)); err != nil {
		job.State = "failed"
		job.Reason = "artifact_error"
		job.ResultText = preview(err.Error(), maxPreviewBytes)
		manager.settle(record, job)
		return
	}
	taskCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	launchSpec := *recovery.Task
	if len(launchSpec.OutputSchema) > 0 {
		var compact bytes.Buffer
		if compactErr := json.Compact(&compact, launchSpec.OutputSchema); compactErr != nil {
			job.State = "failed"
			job.Reason = "yield_invalid"
			manager.settle(record, job)
			return
		}
		launchSpec.Prompt += "\n\nYour final answer must be exactly one JSON value, at most 64 KiB, without Markdown fences or trailing prose, satisfying this JSON Schema:\n" + compact.String()
	}
	result, err := manager.host.RunTask(taskCtx, work.TaskLaunch{Job: cloneJob(job), Spec: launchSpec, Environment: *recovery.Environment, JournalPath: recovery.ChildJournal, Resume: resume})
	job.Usage = result.Usage
	job.State = "completed"
	if err != nil || result.Error != "" || result.StopReason != "" && result.StopReason != "completed" {
		job.State = "failed"
		job.Reason = "child_failed"
	}
	if taskCtx.Err() != nil {
		job.State = "cancelled"
		job.Reason = "cancelled"
	}
	if result.StopReason == "unknown" {
		job.State = "unknown"
		job.Reason = "outcome_unknown"
		if recovery.Workspace != nil {
			job.Cleanup = "blocked"
		}
	}
	text := result.Text
	if text == "" && err != nil {
		text = err.Error()
	}
	if text == "" && result.Error != "" {
		text = result.Error
	}
	job.ResultText = preview(text, maxPreviewBytes)
	artifact, artifactErr := worktree.WriteArtifact(manager.jobPath(job.ID, "result.txt"), strings.NewReader(text), maxResultBytes)
	if artifactErr != nil {
		job.State = "failed"
		job.Reason = "artifact_error"
		if errors.Is(artifactErr, worktree.ErrLimit) {
			job.Reason = "output_limit"
		}
	} else {
		job.Result = &artifact
	}
	if record.schema != nil && job.State == "completed" {
		value, validationErr := validateYield(record.schema, text)
		if validationErr != nil {
			job.State = "failed"
			job.Reason = "yield_invalid"
			job.ResultText = preview("Invalid typed yield: "+validationErr.Error(), 1024)
		} else {
			job.Value = value
		}
	}
	if recovery.Workspace != nil && job.Cleanup != "blocked" {
		patch, exportErr := worktree.Export(context.WithoutCancel(ctx), *recovery.Workspace, manager.jobPath(job.ID, "changes.patch"))
		if exportErr != nil {
			job.State = "failed"
			job.Reason = "artifact_error"
			if errors.Is(exportErr, worktree.ErrLimit) {
				job.Reason = "output_limit"
			}
		} else {
			job.Patch = &patch
		}
	}
	manager.settle(record, job)
}
func (manager *Manager) jobPath(jobID, name string) string {
	return filepath.Join(manager.host.JournalPath+".d", work.JobArtifactsDir, jobID, name)
}
func preview(text string, limit int) string {
	text = strings.ToValidUTF8(text, "\ufffd")
	if len(text) <= limit {
		return text
	}
	text = text[:limit]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text
}

// TaskRunning records the child's durable start after its own recorder commits it.
func (manager *Manager) TaskRunning(ctx context.Context, jobID, childRunID string) error {
	if childRunID == "" {
		return ErrInvalidInput
	}
	record, err := manager.find(jobID)
	if err != nil {
		return err
	}
	manager.mu.Lock()
	valid := record.live && record.recovery.Job.Kind == "task" && !terminal(record.recovery.Job.State)
	manager.mu.Unlock()
	if !valid {
		return ErrInvalidInput
	}
	return manager.running(context.WithoutCancel(ctx), record, 0, childRunID)
}

func (manager *Manager) running(ctx context.Context, record *entry, pid int, childRunID string) error {
	payload, err := json.Marshal(runningPayload{JobID: record.recovery.Job.ID, PID: pid, PGID: pid, ChildRunID: childRunID})
	if err != nil {
		return err
	}
	manager.mu.Lock()
	runID := manager.run.RunID
	manager.mu.Unlock()
	if err = manager.append(ctx, "job_running", runID, payload); err != nil {
		return err
	}
	manager.mu.Lock()
	record.recovery.Job.State = "running"
	job := cloneJob(record.recovery.Job)
	manager.notifyLocked()
	manager.mu.Unlock()
	return manager.publish(ctx, job, "running")
}

func closeDone(record *entry) {
	select {
	case <-record.done:
	default:
		close(record.done)
	}
}
func (manager *Manager) settle(record *entry, job work.Job) {
	record.transition.Lock()
	manager.mu.Lock()
	if !record.recovery.Job.FinishedAt.IsZero() {
		manager.mu.Unlock()
		record.transition.Unlock()
		return
	}
	cancelled := record.recovery.CancelRequested
	runID := manager.run.RunID
	scope := manager.scope
	manager.mu.Unlock()
	if scope == nil {
		scope = context.Background()
	}
	ctx := context.WithoutCancel(scope)
	if cancelled && job.State != "unknown" {
		job.State = "cancelled"
		job.Reason = "cancelled"
	}
	job.FinishedAt = time.Now().UTC()
	payload, err := json.Marshal(finishedPayload{JobID: job.ID, State: job.State, FinishedAt: job.FinishedAt, Reason: job.Reason, ExitCode: job.ExitCode, Usage: job.Usage, Result: job.Result, Patch: job.Patch, Output: job.Output, Value: job.Value, ResultPreview: job.ResultText, Cleanup: job.Cleanup})
	if err == nil {
		err = manager.append(ctx, "job_finished", runID, payload)
	}
	if err != nil {
		record.transition.Unlock()
	}
	if err == nil {
		manager.mu.Lock()
		record.recovery.Job = cloneJob(job)
		record.recovery.DeliveryPending = true
		manager.mu.Unlock()
		record.transition.Unlock()
		err = manager.publish(ctx, cloneJob(job), "finished")
		if err == nil {
			err = manager.deliver(ctx, record)
		}
		if err == nil && record.recovery.Workspace != nil && job.Cleanup != "blocked" {
			workspace := *record.recovery.Workspace
			clean, cleanErr := worktree.Pristine(ctx, workspace)
			if cleanErr == nil && clean {
				if cleanupErr := worktree.Remove(ctx, workspace, job.ID, record.recovery.IdentityDigest, job.Patch, true); cleanupErr == nil {
					err = manager.cleaned(ctx, record, runID)
					if err == nil {
						manager.mu.Lock()
						cleanedJob := cloneJob(record.recovery.Job)
						manager.mu.Unlock()
						err = manager.publish(ctx, cleanedJob, "cleaned")
					}
				}
			}
		}
	}
	if err != nil {
		manager.fail(err)
	}
	manager.mu.Lock()
	record.live = false
	record.joined = true
	record.cancel = nil
	closeDone(record)
	manager.notifyLocked()
	manager.mu.Unlock()
}

func (manager *Manager) deliver(ctx context.Context, record *entry) error {
	manager.mu.Lock()
	job := cloneJob(record.recovery.Job)
	manager.mu.Unlock()
	text, err := completionNotice(job)
	if err != nil {
		return err
	}
	if _, err = manager.host.Enqueue(ctx, work.QueueInput{Kind: work.FollowUp, Text: text, Key: "job:" + job.ID + ":terminal", SourceJobID: job.ID}); err != nil {
		manager.fail(err)
		return err
	}
	manager.mu.Lock()
	record.delivered = true
	record.recovery.DeliveryPending = false
	manager.mu.Unlock()
	return nil
}

// Idle parks without spending provider calls while owned work or queue delivery is pending.
func (manager *Manager) Idle(ctx context.Context) (bool, error) {
	for {
		manager.mu.Lock()
		if manager.fault != nil {
			err := manager.fault
			manager.mu.Unlock()
			return false, err
		}
		live := false
		for _, record := range manager.entries {
			live = live || record.live
		}
		changed := manager.changed
		manager.mu.Unlock()
		var queueChanged <-chan struct{}
		if manager.host.Queues != nil {
			pending, channel := manager.host.Queues.Pending()
			if pending {
				return true, nil
			}
			queueChanged = channel
		}
		if !live {
			manager.admission.Lock()
			manager.mu.Lock()
			for _, record := range manager.entries {
				live = live || record.live
			}
			manager.mu.Unlock()
			pending := false
			if manager.host.Queues != nil {
				pending, _ = manager.host.Queues.Pending()
			}
			if live || pending {
				manager.admission.Unlock()
				continue
			}
			manager.mu.Lock()
			manager.sealed = true
			manager.mu.Unlock()
			manager.admission.Unlock()
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-changed:
		case <-queueChanged:
		}
	}
}

// Finish seals admission before cancellation and doesn't return until workers join.
func (manager *Manager) Finish(ctx context.Context, reason string) error {
	manager.finish.Lock()
	defer manager.finish.Unlock()
	manager.admission.Lock()
	manager.mu.Lock()
	if manager.finished {
		err := manager.fault
		manager.mu.Unlock()
		manager.admission.Unlock()
		return err
	}
	manager.finished = true
	manager.sealed = true
	runID := manager.run.RunID
	records := make([]*entry, 0)
	for _, record := range manager.entries {
		if record.live {
			records = append(records, record)
		}
	}
	manager.mu.Unlock()
	manager.admission.Unlock()
	persistCtx := context.WithoutCancel(ctx)
	if reason != "completed" && runID != "" {
		payload, err := json.Marshal(stopPayload{Reason: reason})
		if err != nil {
			return err
		}
		if err = manager.append(persistCtx, "run_stop_requested", runID, payload); err != nil {
			manager.fail(err)
		}
	}
	for _, record := range records {
		if _, err := manager.cancelRecord(persistCtx, record, "parent_stopped"); err != nil {
			manager.fail(err)
		}
	}
	manager.mu.Lock()
	cancel := manager.cancel
	manager.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	manager.workers.Wait()
	manager.mu.Lock()
	manager.run.RunID = ""
	manager.scope = nil
	manager.cancel = nil
	manager.mu.Unlock()
	return manager.error()
}

// Close also discards recoverable children while idle, without opening a child journal.
func (manager *Manager) Close(ctx context.Context) error {
	if err := manager.Finish(ctx, "aborted"); err != nil {
		return err
	}
	manager.mu.Lock()
	records := make([]*entry, 0)
	for _, record := range manager.entries {
		if record.recovery.Job.State == "recovering" {
			records = append(records, record)
		}
	}
	manager.mu.Unlock()
	for _, record := range records {
		if _, err := manager.cancelRecord(context.WithoutCancel(ctx), record, "session_closed"); err != nil {
			return err
		}
	}
	return manager.error()
}

func (manager *Manager) find(jobID string) (*entry, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	record := manager.entries[jobID]
	if record == nil {
		return nil, ErrJobNotFound
	}
	return record, nil
}

// Cancel acknowledges its durable request, not the worker's final join.
func (manager *Manager) Cancel(ctx context.Context, jobID string) (work.Job, error) {
	record, err := manager.find(jobID)
	if err != nil {
		return work.Job{}, err
	}
	return manager.cancelRecord(ctx, record, "cancelled")
}
func (manager *Manager) cancelRecord(ctx context.Context, record *entry, reason string) (work.Job, error) {
	record.transition.Lock()
	manager.mu.Lock()
	job := cloneJob(record.recovery.Job)
	if terminal(job.State) {
		manager.mu.Unlock()
		record.transition.Unlock()
		return job, nil
	}
	already := record.recovery.CancelRequested
	runID := manager.run.RunID
	manager.mu.Unlock()
	if !already {
		payload, err := json.Marshal(cancelPayload{JobID: job.ID, Reason: reason})
		if err == nil {
			err = manager.append(ctx, "job_cancel_requested", runID, payload)
		}
		if err != nil {
			if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
				record.transition.Unlock()
				return work.Job{}, err
			}
			manager.mu.Lock()
			cancel := record.cancel
			manager.mu.Unlock()
			if cancel != nil {
				cancel()
			}
			record.transition.Unlock()
			return work.Job{}, err
		}
	}
	manager.mu.Lock()
	record.recovery.CancelRequested = true
	cancel := record.cancel
	live := record.live
	manager.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	record.transition.Unlock()
	if !live {
		if job.State == "recovering" && job.Isolation == "worktree" {
			job.Cleanup = "blocked"
		}
		job.State = "cancelled"
		job.Reason = "cancelled"
		manager.settle(record, job)
		manager.mu.Lock()
		job = cloneJob(record.recovery.Job)
		manager.mu.Unlock()
	}
	return job, manager.error()
}

// Jobs pages stable ids and omits large values and private artifact paths.
func (manager *Manager) Jobs(ctx context.Context, cursor string, limit int) (work.JobPage, error) {
	if err := ctx.Err(); err != nil {
		return work.JobPage{}, err
	}
	if limit == 0 {
		limit = 20
	}
	if limit < 1 || limit > maxJobPage {
		return work.JobPage{}, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidInput, maxJobPage)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if cursor != "" {
		if _, ok := manager.entries[cursor]; !ok {
			return work.JobPage{}, fmt.Errorf("%w: cursor %q is not a job id from this session", ErrInvalidInput, cursor)
		}
	}
	ids := make([]string, 0, len(manager.entries))
	for jobID := range manager.entries {
		if jobID > cursor {
			ids = append(ids, jobID)
		}
	}
	sort.Strings(ids)
	page := work.JobPage{Jobs: make([]work.Job, 0, limit)}
	for index, jobID := range ids {
		if index == limit {
			page.NextCursor = ids[index-1]
			break
		}
		job := cloneJob(manager.entries[jobID].recovery.Job)
		job.Value = nil
		job.Cwd = ""
		job.Result = nil
		job.Patch = nil
		job.Output = nil
		job.ResultText = preview(job.ResultText, 512)
		page.Jobs = append(page.Jobs, job)
	}
	return page, nil
}

// Wait observes any terminal job; cancelling a waiter never cancels its jobs.
func (manager *Manager) Wait(ctx context.Context, jobIDs []string, timeout time.Duration) ([]work.Job, error) {
	if len(jobIDs) < 1 || len(jobIDs) > maxCredits {
		return nil, fmt.Errorf("%w: wait takes 1 to %d job ids", ErrInvalidInput, maxCredits)
	}
	if timeout < 0 || timeout > maxWait {
		return nil, fmt.Errorf("%w: timeout must be between 0 and %d seconds", ErrInvalidInput, int(maxWait/time.Second))
	}
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	seen := make(map[string]bool, len(jobIDs))
	for _, jobID := range jobIDs {
		if seen[jobID] {
			return nil, fmt.Errorf("%w: job id %s is listed twice", ErrInvalidInput, jobID)
		}
		seen[jobID] = true
		if _, err := manager.find(jobID); err != nil {
			return nil, err
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		manager.mu.Lock()
		jobs := make([]work.Job, 0, len(jobIDs))
		settled := false
		for _, jobID := range jobIDs {
			job := cloneJob(manager.entries[jobID].recovery.Job)
			settled = settled || terminal(job.State) && (!manager.entries[jobID].live || manager.entries[jobID].joined)
			jobs = append(jobs, job)
		}
		changed := manager.changed
		manager.mu.Unlock()
		// Eight escaped 64 KiB JSON values can exceed a transport frame; refs preserve them.
		encoded, err := json.Marshal(jobs)
		if err != nil {
			return nil, err
		}
		if len(encoded) > 900<<10 {
			for index := range jobs {
				if len(jobs[index].Value) > 0 {
					jobs[index].Value = nil
					jobs[index].ValueInResult = true
				}
			}
		}
		if settled {
			return jobs, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return jobs, nil
		case <-changed:
		}
	}
}

// Logs uses raw byte offsets even when replacement runes change rendered length.
func (manager *Manager) Logs(ctx context.Context, jobID string, offset int64, limit int) (work.LogPage, error) {
	if err := ctx.Err(); err != nil {
		return work.LogPage{}, err
	}
	if offset < 0 || limit < 0 || limit > maxLogPage {
		return work.LogPage{}, fmt.Errorf("%w: offset must be at least 0 and limit between 0 and %d bytes", ErrInvalidInput, maxLogPage)
	}
	if limit == 0 {
		limit = 16 << 10
	}
	record, err := manager.find(jobID)
	if err != nil {
		return work.LogPage{}, err
	}
	manager.mu.Lock()
	job := cloneJob(record.recovery.Job)
	truncated := record.truncated || job.Reason == "output_limit"
	committedBytes := record.outputBytes
	live := record.live
	manager.mu.Unlock()
	if job.Kind != "bash" {
		return work.LogPage{}, fmt.Errorf("%w: logs reads background bash output, and %s is a %s job; its result is in the artifacts wait returns", ErrInvalidInput, jobID, job.Kind)
	}
	file, err := os.OpenFile(manager.jobPath(jobID, "output.log"), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) && offset == 0 {
		return work.LogPage{Offset: offset, NextOffset: offset, EOF: terminal(job.State)}, nil
	}
	if err != nil {
		return work.LogPage{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return work.LogPage{}, err
	}
	available := info.Size()
	if live && !terminal(job.State) && committedBytes < available {
		available = committedBytes
	}
	if offset > available {
		return work.LogPage{}, fmt.Errorf("%w: offset %d is past the %d bytes of output so far", ErrInvalidInput, offset, available)
	}
	if int64(limit) > available-offset {
		limit = int(available - offset)
	}
	buffer := make([]byte, limit)
	count, err := file.ReadAt(buffer, offset)
	if err != nil && err != io.EOF {
		return work.LogPage{}, err
	}
	next := offset + int64(count)
	return work.LogPage{Text: strings.ToValidUTF8(string(buffer[:count]), "\ufffd"), Offset: offset, NextOffset: next, EOF: next >= available && terminal(job.State), Truncated: truncated}, nil
}

// Cleanup only removes an exact owned workspace with a still-valid complete patch.
func (manager *Manager) Cleanup(ctx context.Context, jobID string) (work.Job, error) {
	record, err := manager.find(jobID)
	if err != nil {
		return work.Job{}, err
	}
	record.transition.Lock()
	manager.mu.Lock()
	alreadyRemoved := record.recovery.Job.Cleanup == "removed"
	manager.mu.Unlock()
	job, err := manager.cleanupOwned(ctx, record)
	record.transition.Unlock()
	if err != nil {
		return work.Job{}, err
	}
	if !alreadyRemoved {
		if err = manager.publish(ctx, job, "cleaned"); err != nil {
			return work.Job{}, err
		}
	}
	return job, nil
}

func (manager *Manager) cleanupOwned(ctx context.Context, record *entry) (work.Job, error) {
	manager.mu.Lock()
	job := cloneJob(record.recovery.Job)
	joined := record.joined
	workspace := record.recovery.Workspace
	runID := manager.run.RunID
	manager.mu.Unlock()
	if !terminal(job.State) || !joined || workspace == nil || job.Cleanup == "blocked" || job.State == "unknown" {
		return work.Job{}, ErrJobNotCleanupable
	}
	if job.Cleanup == "removed" {
		return job, nil
	}
	if err := worktree.Remove(ctx, *workspace, job.ID, record.recovery.IdentityDigest, job.Patch, false); err != nil {
		return work.Job{}, fmt.Errorf("%w: %v", ErrJobNotCleanupable, err)
	}
	if err := manager.cleaned(ctx, record, runID); err != nil {
		return work.Job{}, err
	}
	manager.mu.Lock()
	job = cloneJob(record.recovery.Job)
	manager.mu.Unlock()
	return job, nil
}
func (manager *Manager) cleaned(ctx context.Context, record *entry, runID string) error {
	payload, err := json.Marshal(cleanedPayload{record.recovery.Job.ID, record.recovery.Workspace.Path})
	if err != nil {
		return err
	}
	if err = manager.append(ctx, "job_cleaned", runID, payload); err != nil {
		return err
	}
	manager.mu.Lock()
	record.recovery.Job.Cleanup = "removed"
	manager.notifyLocked()
	manager.mu.Unlock()
	return nil
}
