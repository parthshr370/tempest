// Package taskstate owns the persisted phase graph used by the jobs supervisor.
package taskstate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"go.harness.dev/harness/internal/outputschema"
	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/extension"
)

// Namespace holds host-owned workflow records, separate from native job receipts.
const Namespace = "_tempest/tasks"

const maxPhaseWorkers = 4

// Store is borrowed from the owning session. We never open another writer.
type Store interface {
	Read(context.Context, durable.Query) (durable.Page, error)
	Commit(context.Context, durable.Commit) (durable.Receipt, error)
}

// Definition pins executable code through a phase or migration.
type Definition struct {
	Task       extension.TaskDefinition
	Revision   string
	Namespaces []string
	Bundles    []extension.Definition
	Release    func()
}

// Options supplies host authority and a clock. Resolve must pin its generation.
type Options struct {
	SessionID    string
	Store        Store
	Now          func() time.Time
	Resolve      func(string) (Definition, error)
	Invoke       func(context.Context, Definition, extension.Task, bool) (extension.TaskDecision, error)
	MaxCrashes   uint32
	OwnedSettled func(string) bool
	AbortOwned   func(context.Context, string) error
}

type live struct {
	cancel context.CancelFunc
	ctx    context.Context
	done   chan struct{}
}

type phaseResult struct {
	task       extension.Task
	definition Definition
	decision   extension.TaskDecision
	err        error
	suspended  bool
}

type pendingCommit struct {
	batch   durable.Commit
	updates []extension.Task
}

type rejectedDecision struct{ reason string }

func (e *rejectedDecision) Error() string { return e.reason }

// Runtime serializes graph transitions, not extension code or external APIs.
type Runtime struct {
	mu            sync.Mutex
	options       Options
	tasks         map[string]extension.Task
	live          map[string]*live
	results       chan phaseResult
	changed       chan struct{}
	closed        bool
	paused        bool
	pending       *pendingCommit
	cleanupFaults map[string]string
}

// New reduces committed records without resolving or invoking task definitions.
func New(ctx context.Context, o Options) (*Runtime, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.MaxCrashes == 0 {
		o.MaxCrashes = 3
	}
	if o.SessionID == "" || o.Store == nil {
		return nil, errors.New("task session and store required")
	}
	r := &Runtime{options: o, tasks: map[string]extension.Task{}, live: map[string]*live{}, results: make(chan phaseResult, maxPhaseWorkers), changed: make(chan struct{}), cleanupFaults: map[string]string{}}
	q := durable.Query{SessionID: o.SessionID, Kind: durable.TaskKind, Namespace: Namespace, Limit: 100}
	for {
		page, err := o.Store.Read(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, record := range page.Records {
			var t extension.Task
			if record.Version != 1 || json.Unmarshal(record.Value, &t) != nil || t.ID != record.ID || t.SessionID != o.SessionID || t.Revision != record.Revision {
				return nil, fmt.Errorf("%w: invalid task record", durable.ErrCorrupt)
			}
			if !Terminal(t) {
				r.tasks[t.ID] = t
			} else if t.Status == "cleanup_faulted" && t.Owner != "" {
				r.cleanupFaults[t.Owner] = t.ID
			}
		}
		if page.NextCursor == "" {
			break
		}
		q.Cursor = page.NextCursor
	}
	for owner := range r.cleanupFaults {
		if _, active := r.tasks[owner]; !active {
			delete(r.cleanupFaults, owner)
		}
	}
	graph, err := r.graph(ctx, nil)
	if err != nil {
		return nil, err
	}
	if err := validateGraph(graph); err != nil {
		return nil, fmt.Errorf("%w: %v", durable.ErrCorrupt, err)
	}
	return r, nil
}

// Terminal reports whether cleanup has settled, including unresolved cleanup.
func Terminal(t extension.Task) bool {
	return t.Status == "completed" || t.Status == "failed" || t.Status == "aborted" || t.Status == "cleanup_faulted"
}
func failed(t extension.Task) bool {
	return t.Status == "failed" || t.Status == "aborted" || t.Status == "cleanup_faulted"
}
func copyTask(t extension.Task) extension.Task {
	t.Input = bytes.Clone(t.Input)
	t.Checkpoint = bytes.Clone(t.Checkpoint)
	t.Result = bytes.Clone(t.Result)
	t.Wait = slices.Clone(t.Wait)
	if t.WakeAt != nil {
		wake := *t.WakeAt
		t.WakeAt = &wake
	}
	return t
}

// Snapshot returns detached active tasks in task identity order.
func (r *Runtime) Snapshot() []extension.Task {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]extension.Task, 0, len(r.tasks))
	for _, t := range r.tasks {
		out = append(out, copyTask(t))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Get returns one detached task without executing its phase.
func (r *Runtime) Get(id string) (extension.Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.get(context.Background(), id)
}

func decodeTask(record durable.Record, session string) (extension.Task, error) {
	var task extension.Task
	if record.Version != 1 || json.Unmarshal(record.Value, &task) != nil || task.ID != record.ID || task.SessionID != session || task.Revision != record.Revision {
		return task, fmt.Errorf("%w: invalid task record", durable.ErrCorrupt)
	}
	return task, nil
}

func (r *Runtime) get(ctx context.Context, id string) (extension.Task, error) {
	if id == "" {
		return extension.Task{}, errTaskNotFound
	}
	if task, ok := r.tasks[id]; ok {
		return copyTask(task), nil
	}
	page, err := r.options.Store.Read(ctx, durable.Query{SessionID: r.options.SessionID, Kind: durable.TaskKind, Namespace: Namespace, ID: id, Limit: 1})
	if err != nil {
		return extension.Task{}, err
	}
	if len(page.Records) == 0 {
		return extension.Task{}, errTaskNotFound
	}
	return decodeTask(page.Records[0], r.options.SessionID)
}

var errTaskNotFound = errors.New("task not found")

// graph borrows terminal references for validation, never for the working set.
func (r *Runtime) graph(ctx context.Context, updates []extension.Task) (map[string]extension.Task, error) {
	graph := make(map[string]extension.Task, len(r.tasks)+len(updates))
	for id, task := range r.tasks {
		graph[id] = task
	}
	for _, task := range updates {
		graph[task.ID] = task
	}
	var load func(string) error
	load = func(id string) error {
		if _, exists := graph[id]; exists {
			return nil
		}
		task, err := r.get(ctx, id)
		if err != nil {
			if errors.Is(err, errTaskNotFound) {
				return &rejectedDecision{reason: "foreign or missing wait reference or owner: " + id}
			}
			return err
		}
		graph[id] = task
		if task.Owner != "" {
			if err := load(task.Owner); err != nil {
				return err
			}
		}
		for _, dep := range task.Wait {
			if err := load(dep); err != nil {
				return err
			}
		}
		return nil
	}
	for _, task := range graph {
		if task.Owner != "" {
			if err := load(task.Owner); err != nil {
				return nil, err
			}
		}
		for _, dep := range task.Wait {
			if err := load(dep); err != nil {
				return nil, err
			}
		}
	}
	return graph, nil
}

func validateGraph(tasks map[string]extension.Task) error {
	return validateChanges(tasks, nil)
}

func validateChanges(tasks map[string]extension.Task, roots []string) error {
	children := make(map[string][]string)
	for id, task := range tasks {
		if task.Owner != "" && !Terminal(task) {
			children[task.Owner] = append(children[task.Owner], id)
		}
	}
	if roots == nil {
		for id := range tasks {
			roots = append(roots, id)
		}
	}
	for _, id := range roots {
		t := tasks[id]
		if t.ID != id {
			return errors.New("task identity mismatch")
		}
		switch t.Status {
		case "runnable", "suspended", "running", "aborting", "sleeping", "waiting", "completing", "abort_requested", "parked", "completed", "failed", "aborted", "cleanup_faulted":
		default:
			return errors.New("invalid task status")
		}
		if t.Phase == "" || t.Version == 0 || t.CodeRevision == "" || !json.Valid(t.Input) || !json.Valid(t.Checkpoint) {
			return errors.New("invalid task descriptor or checkpoint")
		}
		if t.Status == "sleeping" && t.WakeAt == nil {
			return errors.New("sleeping task has no deadline")
		}
		if t.Owner != "" {
			p, ok := tasks[t.Owner]
			if !ok || p.SessionID != t.SessionID || p.Background != t.Background {
				return errors.New("invalid task owner")
			}
		}
		if len(t.Wait) > 64 {
			return errors.New("wait limit exceeded")
		}
		for _, dep := range t.Wait {
			child, ok := tasks[dep]
			if !ok || child.SessionID != t.SessionID {
				return errors.New("foreign or missing wait reference")
			}
			if t.FailFast && child.Owner != id {
				return errors.New("fail-fast can only wait on owned children")
			}
		}
	}
	// A completing owner waits implicitly for its children. Include those edges
	// when rejecting deadlocks, even if neither task has an explicit wait yet.
	for _, ownershipOnly := range []bool{true, false} {
		marks := map[string]uint8{}
		var visit func(string, int) error
		visit = func(id string, depth int) error {
			if depth > 64 || marks[id] == 1 {
				return errors.New("task owner/wait cycle")
			}
			if marks[id] == 2 {
				return nil
			}
			marks[id] = 1
			t := tasks[id]
			if ownershipOnly {
				if t.Owner != "" {
					if err := visit(t.Owner, depth+1); err != nil {
						return err
					}
				}
			} else {
				for _, dep := range t.Wait {
					if err := visit(dep, depth+1); err != nil {
						return err
					}
				}
				for _, child := range children[id] {
					if err := visit(child, depth+1); err != nil {
						return err
					}
				}
			}
			marks[id] = 2
			return nil
		}
		for _, id := range roots {
			if err := visit(id, 0); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Runtime) makeTask(req extension.TaskRequest, owner *extension.Task, pinned *Definition) (extension.Task, error) {
	if req.ID == "" || len(req.ID) > 256 || len(req.Definition) > 256 || !json.Valid(req.Input) {
		return extension.Task{}, errors.New("invalid task request")
	}
	if r.options.Resolve == nil {
		return extension.Task{}, errors.New("task definitions unavailable")
	}
	var d Definition
	var err error
	if pinned != nil && len(pinned.Bundles) > 0 {
		for _, bundle := range pinned.Bundles {
			for _, task := range bundle.Tasks {
				if bundle.ID+"/"+task.Name == req.Definition {
					d = Definition{Task: task, Revision: bundle.Revision, Release: func() {}}
				}
			}
		}
		if d.Task.Run == nil {
			return extension.Task{}, errors.New("child definition absent from pinned phase generation")
		}
	} else {
		d, err = r.options.Resolve(req.Definition)
		if err != nil {
			return extension.Task{}, err
		}
	}
	defer d.Release()
	schema, err := outputschema.Compile(d.Task.InputSchema)
	if err != nil {
		return extension.Task{}, err
	}
	if _, err = schema.Validate(string(req.Input)); err != nil {
		return extension.Task{}, err
	}
	t := extension.Task{ID: req.ID, SessionID: r.options.SessionID, Definition: req.Definition, Version: d.Task.Version, CodeRevision: d.Revision, Input: append(json.RawMessage(nil), req.Input...), Checkpoint: append(json.RawMessage(nil), d.Task.Initial...), Phase: "run", Status: "runnable", Owner: req.Owner, RunID: req.RunID, Background: req.Background}
	if owner != nil {
		if Terminal(*owner) || owner.AbortReason != "" || owner.Status == "completing" {
			return t, errors.New("owner no longer admits children")
		}
		t.Owner = owner.ID
		t.Background = owner.Background
		t.RunID = owner.RunID
	} else if !t.Background && t.RunID == "" {
		return t, errors.New("foreground task requires a run")
	}
	return t, nil
}

func (r *Runtime) commit(ctx context.Context, id string, updates []extension.Task, extra durable.Commit) error {
	if err := r.reconcile(ctx); err != nil {
		return err
	}
	c := extra
	c.ID = id
	roots := make([]string, 0, len(updates))
	for i, t := range updates {
		prior := t.Revision
		t.Now = time.Time{}
		t.Revision++
		updates[i] = t
		roots = append(roots, t.ID)
		raw, err := json.Marshal(t)
		if err != nil {
			return err
		}
		c.Expected = append(c.Expected, durable.RevisionCheck{SessionID: r.options.SessionID, Kind: durable.TaskKind, Namespace: Namespace, ID: t.ID, Revision: prior})
		c.Changes = append(c.Changes, durable.Change{SessionID: r.options.SessionID, Kind: durable.TaskKind, Namespace: Namespace, ID: t.ID, Version: 1, Value: raw})
	}
	graph, err := r.graph(ctx, updates)
	if err != nil {
		return err
	}
	if err := validateChanges(graph, roots); err != nil {
		return &rejectedDecision{reason: err.Error()}
	}
	r.pending = &pendingCommit{batch: c, updates: updates}
	return r.reconcile(ctx)
}

func (r *Runtime) reconcile(ctx context.Context) error {
	if r.pending == nil {
		return nil
	}
	pending := r.pending
	if _, err := r.options.Store.Commit(ctx, pending.batch); err != nil {
		if errors.Is(err, durable.ErrConflict) || errors.Is(err, durable.ErrInvalidCommit) {
			r.pending = nil
			return err
		}
		page, lookupErr := r.options.Store.Read(context.WithoutCancel(ctx), durable.Query{SessionID: r.options.SessionID, CommitID: pending.batch.ID, Limit: 1})
		if lookupErr != nil {
			return errors.Join(err, lookupErr)
		}
		if len(page.Receipts) != 1 {
			return err
		}
		if _, verifyErr := r.options.Store.Commit(context.WithoutCancel(ctx), pending.batch); verifyErr != nil {
			return errors.Join(err, verifyErr)
		}
	}
	for _, task := range pending.updates {
		if Terminal(task) {
			delete(r.tasks, task.ID)
			delete(r.cleanupFaults, task.ID)
			if task.Status == "cleanup_faulted" && task.Owner != "" {
				if _, active := r.tasks[task.Owner]; active {
					r.cleanupFaults[task.Owner] = task.ID
				}
			}
		} else {
			r.tasks[task.ID] = task
		}
	}
	r.pending = nil
	r.notify()
	return nil
}

// Create durably admits work without executing it. Duplicate identical roots reuse it.
func (r *Runtime) Create(ctx context.Context, req extension.TaskRequest) (extension.Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return extension.Task{}, durable.ErrClosed
	}
	if err := r.reconcile(ctx); err != nil {
		return extension.Task{}, err
	}
	if t, err := r.get(ctx, req.ID); err == nil {
		a, _ := durable.Digest(t.Input)
		b, _ := durable.Digest(req.Input)
		if t.Definition != req.Definition || a != b || t.Owner != req.Owner || (req.Owner == "" && (t.Background != req.Background || t.RunID != req.RunID)) {
			return t, durable.ErrConflict
		}
		return copyTask(t), nil
	} else if !errors.Is(err, errTaskNotFound) {
		return extension.Task{}, err
	}
	if len(r.tasks) >= 64 {
		return extension.Task{}, errors.New("active task limit exceeded")
	}
	var owner *extension.Task
	if req.Owner != "" {
		t, ok := r.tasks[req.Owner]
		if !ok {
			return extension.Task{}, errors.New("owner not found")
		}
		owner = &t
	}
	t, err := r.makeTask(req, owner, nil)
	if err != nil {
		return t, err
	}
	if err = r.commit(ctx, "task:create:"+t.ID, []extension.Task{t}, durable.Commit{}); err != nil {
		return t, err
	}
	return copyTask(r.tasks[t.ID]), nil
}

// NextWake is a pure alarm query. Opening and observing a task never runs it.
func (r *Runtime) NextWake() (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	graph, err := r.graph(context.Background(), nil)
	if err != nil {
		return r.options.Now(), true
	}
	tasks := make([]extension.Task, 0, len(graph))
	for _, task := range graph {
		if r.live[task.ID] != nil || (task.Status == "completing" && r.options.OwnedSettled != nil && !r.options.OwnedSettled(task.ID)) {
			task.Status = "active"
		}
		tasks = append(tasks, task)
	}
	return Alarm(tasks, r.options.Now())
}

// Changes wakes idle owners after a committed transition or joined invocation.
func (r *Runtime) Changes() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.changed
}

// NextWakeRun excludes background timers from a foreground run's idle alarm.
func (r *Runtime) NextWakeRun(runID string) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	graph, err := r.graph(context.Background(), nil)
	if err != nil {
		return r.options.Now(), true
	}
	tasks := make([]extension.Task, 0, len(graph))
	for _, task := range graph {
		if !Terminal(task) && (task.Background || task.RunID != runID || r.live[task.ID] != nil || (task.Status == "completing" && r.options.OwnedSettled != nil && !r.options.OwnedSettled(task.ID))) {
			task.Status = "active"
		}
		tasks = append(tasks, task)
	}
	return Alarm(tasks, r.options.Now())
}

func (r *Runtime) settle(ctx context.Context) (bool, error) {
	ids := make([]string, 0, len(r.tasks))
	for id := range r.tasks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		t := r.tasks[id]
		if Terminal(t) || t.Status == "running" || t.Status == "aborting" || t.Status == "parked" {
			continue
		}
		changed := false
		if t.Status == "sleeping" && t.WakeAt != nil && !r.options.Now().Before(*t.WakeAt) {
			t.Status = "runnable"
			t.WakeAt = nil
			changed = true
		}
		if t.Status == "waiting" {
			done := true
			bad := false
			for _, dep := range t.Wait {
				c, err := r.get(ctx, dep)
				if err != nil {
					return false, err
				}
				done = done && Terminal(c)
				bad = bad || failed(c)
			}
			if t.FailFast && bad {
				return true, r.abortLocked(ctx, id, "fail_fast")
			}
			if done {
				t.Status = "runnable"
				t.Wait = nil
				t.FailFast = false
				changed = true
			}
		}
		if t.Status == "abort_requested" {
			// A crash may split the owner's abort fence from descendant fences.
			// Finish propagating intent before selecting any runnable child.
			for childID, child := range r.tasks {
				if child.Owner == id && !Terminal(child) && child.AbortReason == "" {
					return true, r.abortLocked(ctx, childID, t.AbortReason)
				}
			}
			if r.options.AbortOwned != nil {
				revision := t.Revision
				r.mu.Unlock()
				err := r.options.AbortOwned(ctx, id)
				r.mu.Lock()
				if err != nil {
					return false, err
				}
				if current, ok := r.tasks[id]; !ok || current.Revision != revision {
					return true, nil
				}
			}
		}
		if t.Status == "completing" || t.Status == "abort_requested" {
			done := true
			if r.options.OwnedSettled != nil {
				done = r.options.OwnedSettled(id)
			}
			if child, faulted := r.cleanupFaults[id]; faulted {
				t.Outcome = "cleanup_faulted"
				t.Reason = "owned cleanup unresolved: " + child
			}
			for _, c := range r.tasks {
				if c.Owner == id && !Terminal(c) {
					done = false
				}
			}
			if done {
				if t.Status == "abort_requested" {
					t.Status = "runnable"
					t.Phase = "abort"
				} else {
					t.Status = t.Outcome
					if t.Status == "" {
						t.Status = "completed"
					}
				}
				changed = true
			}
		}
		if changed {
			return true, r.commit(ctx, fmt.Sprintf("task:settle:%s:%d", id, t.Revision), []extension.Task{t}, durable.Commit{})
		}
	}
	return false, nil
}

func (r *Runtime) park(ctx context.Context, t extension.Task, reason string) error {
	t.Status = "parked"
	t.Reason = reason
	return r.commit(ctx, fmt.Sprintf("task:park:%s:%d", t.ID, t.Revision), []extension.Task{t}, durable.Commit{})
}

// Resume drains ready phases. Sleeping tasks hold no invocation or worker.
// A host calls it again at NextWake; cancellation of this wait only suspends work.
func (r *Runtime) Resume(ctx context.Context) error { return r.resume(ctx, "", "", false) }

// ResumeRun drains only the current run's foreground work.
func (r *Runtime) ResumeRun(ctx context.Context, runID string) error {
	return r.resume(ctx, runID, "", false)
}

// ResumeBackground doesn't borrow a conversational run's cancellation scope.
func (r *Runtime) ResumeBackground(ctx context.Context) error {
	return r.resume(ctx, "", "", true)
}

// ResumeTask drains one owned subtree without waiting for unrelated work.
func (r *Runtime) ResumeTask(ctx context.Context, id string) error {
	return r.resume(ctx, "", id, false)
}

func (r *Runtime) inScope(task extension.Task, runID, root string, backgroundOnly bool) bool {
	if runID != "" && (task.Background || task.RunID != runID) {
		return false
	}
	if backgroundOnly && !task.Background {
		return false
	}
	if root != "" {
		for owner := task.ID; owner != ""; owner = r.tasks[owner].Owner {
			if owner == root {
				return true
			}
		}
		return false
	}
	return true
}

func (r *Runtime) resume(ctx context.Context, foregroundRun, root string, backgroundOnly bool) (resumeErr error) {
	owned := make(map[string]uint64)
	defer func() {
		if resumeErr != nil {
			resumeErr = errors.Join(resumeErr, r.suspendAttempts(context.WithoutCancel(ctx), owned))
			for {
				select {
				case result := <-r.results:
					if err := r.adoptPhase(context.WithoutCancel(ctx), result); err != nil {
						resumeErr = errors.Join(resumeErr, err)
					}
				default:
					return
				}
			}
		}
	}()
	for range 1024 {
		select {
		case result := <-r.results:
			if err := r.adoptPhase(context.WithoutCancel(ctx), result); err != nil {
				return err
			}
			continue
		default:
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		r.mu.Lock()
		if err := r.reconcile(ctx); err != nil {
			r.mu.Unlock()
			return err
		}
		if r.closed {
			r.mu.Unlock()
			return durable.ErrClosed
		}
		if r.paused {
			r.mu.Unlock()
			return nil
		}
		changed, err := r.settle(ctx)
		if err != nil {
			r.mu.Unlock()
			return err
		}
		if changed {
			r.mu.Unlock()
			continue
		}
		ids := make([]string, 0, len(r.tasks))
		for id := range r.tasks {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		var t extension.Task
		found := false
		for _, id := range ids {
			v := r.tasks[id]
			if r.paused || r.live[id] != nil || len(r.live) >= maxPhaseWorkers {
				continue
			}
			if owned[id] == v.Attempt && v.Reason == "decision conflict; fresh attempt required" {
				continue
			}
			if !r.inScope(v, foregroundRun, root, backgroundOnly) {
				continue
			}
			if v.Status == "runnable" || v.Status == "suspended" || v.Status == "running" || v.Status == "aborting" {
				t = v
				found = true
				break
			}
		}
		if !found {
			pending := false
			for _, task := range r.tasks {
				ready := task.Status == "runnable" || task.Status == "suspended" || task.Status == "running" || task.Status == "aborting"
				released := owned[task.ID] == task.Attempt && task.Reason == "decision conflict; fresh attempt required"
				pending = pending || r.inScope(task, foregroundRun, root, backgroundOnly) && ready && !r.paused && !released
			}
			change := r.changed
			r.mu.Unlock()
			if !pending {
				return nil
			}
			select {
			case result := <-r.results:
				if err := r.adoptPhase(context.WithoutCancel(ctx), result); err != nil {
					return err
				}
			case <-change:
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}
		if t.Status == "running" || t.Status == "aborting" {
			t.Crashes++
			if t.Crashes >= r.options.MaxCrashes {
				err = r.park(ctx, t, "phase crash limit reached; explicit retry required")
				r.mu.Unlock()
				if err != nil {
					return err
				}
				continue
			}
		}
		var d Definition
		if r.options.Resolve == nil {
			err = errors.New("definition unavailable")
		} else {
			d, err = r.options.Resolve(t.Definition)
		}
		if err != nil {
			err = r.park(ctx, t, "definition unavailable: "+err.Error())
			r.mu.Unlock()
			if err != nil {
				return err
			}
			continue
		}
		inputSchema, schemaErr := outputschema.Compile(d.Task.InputSchema)
		if schemaErr == nil {
			_, schemaErr = inputSchema.Validate(string(t.Input))
		}
		if schemaErr != nil {
			d.Release()
			err = r.park(ctx, t, "incompatible task input: "+schemaErr.Error())
			r.mu.Unlock()
			if err != nil {
				return err
			}
			continue
		}
		if d.Task.Version != t.Version || d.Revision != t.CodeRevision {
			if d.Task.Version <= t.Version || d.Task.Migrate == nil {
				d.Release()
				err = r.park(ctx, t, "incompatible definition version or code revision")
				r.mu.Unlock()
				if err != nil {
					return err
				}
				continue
			}
			// Reserve the worker, but don't hold the graph lock across extension code.
			callCtx, cancel := context.WithCancel(ctx)
			migration := &live{ctx: callCtx, cancel: cancel, done: make(chan struct{})}
			r.live[t.ID] = migration
			r.mu.Unlock()
			state, migrationErr := d.Task.Migrate(t.Version, append(json.RawMessage(nil), t.Checkpoint...))
			close(migration.done)
			r.mu.Lock()
			delete(r.live, t.ID)
			cancelled := callCtx.Err() != nil
			cancel()
			r.notify()
			if current, ok := r.tasks[t.ID]; !ok || current.Revision != t.Revision || cancelled || r.paused || r.closed {
				d.Release()
				r.mu.Unlock()
				continue
			}
			schema, compileErr := outputschema.Compile(d.Task.StateSchema)
			if migrationErr == nil {
				migrationErr = compileErr
			}
			if migrationErr == nil {
				_, migrationErr = schema.Validate(string(state))
			}
			if migrationErr != nil {
				d.Release()
				err = r.park(ctx, t, "migration failed: "+migrationErr.Error())
				r.mu.Unlock()
				if err != nil {
					return err
				}
				continue
			}
			t.Checkpoint = state
			t.Version = d.Task.Version
			t.CodeRevision = d.Revision
			t.Status = "runnable"
			err = r.commit(ctx, fmt.Sprintf("task:migrate:%s:%d", t.ID, t.Revision), []extension.Task{t}, durable.Commit{})
			d.Release()
			r.mu.Unlock()
			if err != nil {
				return err
			}
			continue
		}
		stateSchema, stateErr := outputschema.Compile(d.Task.StateSchema)
		if stateErr == nil {
			_, stateErr = stateSchema.Validate(string(t.Checkpoint))
		}
		if stateErr != nil {
			d.Release()
			err = r.park(ctx, t, "invalid task checkpoint: "+stateErr.Error())
			r.mu.Unlock()
			if err != nil {
				return err
			}
			continue
		}
		t.Attempt++
		if t.AbortReason == "" {
			t.Reason = ""
		}
		if t.AbortReason != "" {
			t.Status = "aborting"
		} else {
			t.Status = "running"
		}
		err = r.commit(ctx, fmt.Sprintf("task:intent:%s:%d", t.ID, t.Attempt), []extension.Task{t}, durable.Commit{})
		if err != nil {
			d.Release()
			r.mu.Unlock()
			return err
		}
		t = copyTask(r.tasks[t.ID])
		callCtx, cancel := context.WithCancel(ctx)
		invocation := &live{ctx: callCtx, cancel: cancel, done: make(chan struct{})}
		r.live[t.ID] = invocation
		owned[t.ID] = t.Attempt
		t.Now = r.options.Now()
		r.mu.Unlock()
		go func(task extension.Task, definition Definition, invocation *live) {
			decision, err := r.options.Invoke(invocation.ctx, definition, task, task.AbortReason != "")
			suspended := invocation.ctx.Err() != nil
			invocation.cancel()
			r.results <- phaseResult{task: task, definition: definition, decision: decision, err: err, suspended: suspended}
			close(invocation.done)
		}(t, d, invocation)
	}
	return r.suspendAttempts(context.WithoutCancel(ctx), owned)
}

func (r *Runtime) adoptPhase(ctx context.Context, result phaseResult) error {
	defer result.definition.Release()
	r.mu.Lock()
	defer r.mu.Unlock()
	if invocation := r.live[result.task.ID]; invocation != nil {
		<-invocation.done
	}
	delete(r.live, result.task.ID)
	r.notify()
	if err := r.reconcile(ctx); err != nil {
		return err
	}
	task := r.tasks[result.task.ID]
	if r.closed || task.Revision != result.task.Revision || task.Attempt != result.task.Attempt {
		return nil
	}
	if result.suspended {
		task.Status = "suspended"
		return r.commit(ctx, fmt.Sprintf("task:suspended:%s:%d", task.ID, task.Revision), []extension.Task{task}, durable.Commit{})
	}
	if result.err == nil {
		err := r.decide(ctx, task, result.definition, result.decision)
		var rejected *rejectedDecision
		if errors.As(err, &rejected) {
			return r.park(ctx, task, "decision rejected: "+rejected.reason)
		}
		if errors.Is(err, durable.ErrInvalidCommit) {
			return r.park(ctx, task, "decision rejected: "+err.Error())
		}
		if errors.Is(err, durable.ErrConflict) {
			task.Status = "suspended"
			task.Reason = "decision conflict; fresh attempt required"
			task.Crashes = 0
			return r.commit(ctx, fmt.Sprintf("task:conflict:%s:%d", task.ID, task.Revision), []extension.Task{task}, durable.Commit{})
		}
		return err
	}
	if errors.Is(result.err, durable.ErrConflict) {
		task.Status = "suspended"
		task.Reason = "decision conflict; fresh attempt required"
		task.Crashes = 0
		return r.commit(ctx, fmt.Sprintf("task:conflict:%s:%d", task.ID, task.Revision), []extension.Task{task}, durable.Commit{})
	}
	if task.AbortReason == "" {
		return r.park(ctx, task, "phase failed: "+result.err.Error())
	}
	task.Status = "cleanup_faulted"
	task.Reason = result.err.Error()
	task.CleanupFailed = true
	return r.commit(ctx, fmt.Sprintf("task:cleanup-fault:%s:%d", task.ID, task.Revision), []extension.Task{task}, durable.Commit{})
}

func (r *Runtime) decide(ctx context.Context, t extension.Task, d Definition, v extension.TaskDecision) error {
	invokedPhase := t.Phase
	original := t
	if v.Kind != "advance" && v.Kind != "sleep" && v.Kind != "wait" && v.Kind != "complete" {
		return r.park(ctx, original, "invalid phase decision")
	}
	if len(v.Children) > 64 || len(v.Submissions) > 64 || len(v.Memos) > 64 || len(v.Commit.Changes) > 128 {
		return r.park(ctx, original, "decision limit exceeded")
	}
	allowed := func(ns string) bool {
		for _, n := range d.Namespaces {
			if n == ns {
				return true
			}
		}
		return false
	}
	for _, ch := range v.Commit.Changes {
		if ch.SessionID != t.SessionID || ch.Kind != durable.DocumentKind || !allowed(ch.Namespace) {
			return r.park(ctx, original, "decision outside granted documents")
		}
	}
	for _, c := range v.Commit.Expected {
		if c.SessionID != t.SessionID || c.Kind != durable.DocumentKind || !allowed(c.Namespace) {
			return r.park(ctx, original, "comparison outside granted documents")
		}
	}
	if len(v.Checkpoint) > 0 {
		schema, err := outputschema.Compile(d.Task.StateSchema)
		if err != nil {
			return r.park(ctx, original, "invalid checkpoint schema: "+err.Error())
		}
		if _, err = schema.Validate(string(v.Checkpoint)); err != nil {
			return r.park(ctx, original, "invalid checkpoint: "+err.Error())
		}
		t.Checkpoint = append(json.RawMessage(nil), v.Checkpoint...)
	}
	if v.Phase != "" {
		t.Phase = v.Phase
	}
	t.Crashes = 0
	t.Status = "runnable"
	t.Wait = nil
	t.WakeAt = nil
	updates := []extension.Task{}
	children := make(map[string]bool, len(v.Children))
	for _, req := range v.Children {
		if req.Owner != "" && req.Owner != t.ID {
			return r.park(ctx, original, "child owner outside invocation")
		}
		if children[req.ID] {
			return r.park(ctx, original, "child ID reused in decision: "+req.ID)
		}
		children[req.ID] = true
		if _, err := r.get(ctx, req.ID); err == nil {
			return r.park(ctx, original, "child ID already exists: "+req.ID)
		} else if !errors.Is(err, errTaskNotFound) {
			return err
		}
		child, err := r.makeTask(req, &t, &d)
		if err != nil {
			return r.park(ctx, original, "invalid child: "+err.Error())
		}
		updates = append(updates, child)
	}
	active := len(updates) + len(r.tasks)
	if active > 64 {
		return r.park(ctx, original, "active task limit exceeded")
	}
	switch v.Kind {
	case "sleep":
		if v.WakeAt == nil {
			return r.park(ctx, original, "sleep deadline required")
		}
		at := v.WakeAt.UTC()
		t.WakeAt = &at
		t.Status = "sleeping"
	case "wait":
		t.Wait = append([]string(nil), v.Wait...)
		t.FailFast = v.FailFast
		t.Status = "waiting"
	case "complete":
		if len(v.Result) > 0 || t.AbortReason == "" {
			t.Result = append(json.RawMessage(nil), v.Result...)
		}
		t.Status = "completing"
		if t.AbortReason != "" {
			if t.Outcome != "failed" && t.Outcome != "cleanup_faulted" {
				t.Outcome = "aborted"
			}
			if v.Error != "" {
				t.CleanupFailed = true
				t.Status = "cleanup_faulted"
				t.Reason = v.Error
			}
		} else if v.Error != "" {
			t.Outcome = "failed"
			t.AbortReason = v.Error
			t.Reason = v.Error
			t.Status = "abort_requested"
		} else {
			t.Outcome = "completed"
		}
	}
	updates = append(updates, t)
	bundle, _, _ := strings.Cut(t.Definition, "/")
	scope := durable.MemoScope{SessionID: t.SessionID, InvocationID: t.ID, ExtensionID: bundle, HookID: "task:" + invokedPhase}
	for _, memo := range v.Memos {
		if memo.Key == "" || len(memo.Key) > 256 || len(memo.Value) > 64<<10 || !json.Valid(memo.Value) {
			return r.park(ctx, original, "invalid decision memo")
		}
		id := durable.MemoID(scope, memo.Key)
		page, err := r.options.Store.Read(ctx, durable.Query{SessionID: t.SessionID, Kind: durable.MemoKind, Namespace: bundle, ID: id, Limit: 1})
		if err != nil {
			return err
		}
		if len(page.Records) != 0 {
			continue
		}
		v.Commit.Expected = append(v.Commit.Expected, durable.RevisionCheck{SessionID: t.SessionID, Kind: durable.MemoKind, Namespace: bundle, ID: id})
		v.Commit.Changes = append(v.Commit.Changes, durable.Change{SessionID: t.SessionID, Kind: durable.MemoKind, Namespace: bundle, ID: id, Version: 1, Value: memo.Value})
	}
	for _, submission := range v.Submissions {
		if submission.Key == "" || len(submission.Key) > 256 || len(submission.Text) > 32<<10 {
			return r.park(ctx, original, "invalid task submission")
		}
		key := "task:" + t.ID + ":" + submission.Key
		payload, _ := json.Marshal(struct {
			Text string `json:"text"`
		}{submission.Text})
		hash, _ := durable.SubmissionHash("input", payload)
		runDigest, err := durable.Digest(key)
		if err != nil {
			return err
		}
		value := durable.Submission{ID: key, RequestID: key, RunID: runDigest[:16], State: "accepted", Revision: 1, RequestHash: hash, Placement: "input", Payload: payload}
		raw, _ := json.Marshal(value)
		prior, err := r.options.Store.Read(ctx, durable.Query{SessionID: t.SessionID, Kind: durable.SubmissionKind, Namespace: "_tempest/submissions", ID: key, Limit: 1})
		if err != nil {
			return err
		}
		if len(prior.Records) > 0 {
			var old durable.Submission
			if json.Unmarshal(prior.Records[0].Value, &old) != nil || old.RequestHash != hash {
				return r.park(ctx, original, "submission key reused with different content: "+submission.Key)
			}
			continue
		}
		v.Commit.Expected = append(v.Commit.Expected, durable.RevisionCheck{SessionID: t.SessionID, Kind: durable.SubmissionKind, Namespace: "_tempest/submissions", ID: key})
		v.Commit.Changes = append(v.Commit.Changes, durable.Change{SessionID: t.SessionID, Kind: durable.SubmissionKind, Namespace: "_tempest/submissions", ID: key, Version: 1, Value: raw})
	}
	id := fmt.Sprintf("task:decision:%s:%d:%d", t.ID, t.Revision, t.Attempt)
	if err := r.commit(ctx, id, updates, v.Commit); err != nil {
		return err
	}
	if t.Outcome == "failed" {
		return r.abortChildrenLocked(ctx, t.ID, "owner_failed")
	}
	return nil
}

func (r *Runtime) abortChildrenLocked(ctx context.Context, id, reason string) error {
	ids := []string{}
	for child, t := range r.tasks {
		if t.Owner == id && !Terminal(t) {
			ids = append(ids, child)
		}
	}
	sort.Strings(ids)
	for _, child := range ids {
		if err := r.abortLocked(ctx, child, reason); err != nil {
			return err
		}
	}
	return nil
}
func (r *Runtime) abortLocked(ctx context.Context, id, reason string) error {
	t, err := r.get(ctx, id)
	if err != nil {
		return err
	}
	if Terminal(t) {
		return nil
	}
	if t.AbortReason != "" {
		return r.abortChildrenLocked(ctx, id, t.AbortReason)
	}
	// Record the parent's fence before descending so admission stops immediately.
	t.AbortReason = reason
	if reason == "fail_fast" {
		t.Outcome = "failed"
	}
	t.Status = "abort_requested"
	t.Wait = nil
	t.WakeAt = nil
	t.FailFast = false
	if err := r.commit(ctx, fmt.Sprintf("task:abort:%s:%d", id, t.Revision), []extension.Task{t}, durable.Commit{}); err != nil {
		return err
	}
	if live := r.live[id]; live != nil {
		live.cancel()
	}
	return r.abortChildrenLocked(ctx, id, reason)
}

// Abort records cancellation before revoking live invocations. Resume runs cleanup.
func (r *Runtime) Abort(ctx context.Context, id, reason string) error {
	if strings.TrimSpace(reason) == "" {
		reason = "cancelled"
	}
	r.mu.Lock()
	err := r.abortLocked(ctx, id, reason)
	var joins []chan struct{}
	for taskID, invocation := range r.live {
		if invocation.ctx.Err() == nil {
			continue
		}
		for owner := taskID; owner != ""; owner = r.tasks[owner].Owner {
			if owner == id {
				joins = append(joins, invocation.done)
				break
			}
		}
	}
	r.mu.Unlock()
	if err != nil {
		return err
	}
	for _, done := range joins {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// Retry explicitly unblocks parked work after compatible code is installed.
func (r *Runtime) Retry(ctx context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.reconcile(ctx); err != nil {
		return err
	}
	t, err := r.get(ctx, id)
	if err != nil {
		return err
	}
	if t.Status != "parked" && t.Status != "cleanup_faulted" {
		return errors.New("task is not parked")
	}
	if t.Status == "cleanup_faulted" {
		if !t.CleanupFailed {
			return errors.New("retry the faulted child cleanup; owner's work has already completed")
		}
		t.CleanupFailed = false
	}
	t.Status = "runnable"
	t.Reason = ""
	t.Crashes = 0
	return r.commit(ctx, fmt.Sprintf("task:retry:%s:%d", id, t.Revision), []extension.Task{t}, durable.Commit{})
}

// StopRun records foreground abort intent without waiting for compensation.
func (r *Runtime) StopRun(ctx context.Context, runID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, task := range r.tasks {
		if task.Owner == "" && !task.Background && task.RunID == runID {
			if err := r.abortLocked(ctx, task.ID, "run_stopped"); err != nil {
				return err
			}
		}
	}
	return nil
}

// Close revokes and joins local invocations without recording user cancellation.
func (r *Runtime) Close(ctx context.Context) error {
	r.mu.Lock()
	r.closed = true
	var joins []chan struct{}
	for _, v := range r.live {
		v.cancel()
		joins = append(joins, v.done)
	}
	r.mu.Unlock()
	for _, done := range joins {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for {
		select {
		case result := <-r.results:
			result.definition.Release()
		default:
			return nil
		}
	}
}

// FencedCommit compares the phase's task revision without advancing its state.
func (r *Runtime) FencedCommit(ctx context.Context, task extension.Task, batch durable.Commit) (durable.Receipt, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current, exists := r.tasks[task.ID]
	invocation := r.live[task.ID]
	if invocation == nil || invocation.ctx.Err() != nil {
		return durable.Receipt{}, extension.ErrExpired
	}
	if r.closed || !exists || current.Revision != task.Revision || current.Attempt != task.Attempt || (current.Status != "running" && current.Status != "aborting") {
		return durable.Receipt{}, extension.ErrExpired
	}
	revision := task.Revision
	page, err := r.options.Store.Read(ctx, durable.Query{SessionID: task.SessionID, CommitID: batch.ID, Limit: 1})
	if err != nil {
		return durable.Receipt{}, err
	}
	if len(page.Receipts) == 1 {
		// Reconstruct the original fence so a later attempt can verify a replay.
		prior, err := r.options.Store.Read(ctx, durable.Query{SessionID: task.SessionID, Kind: durable.TaskKind, Namespace: Namespace, ID: task.ID, AsOf: page.Receipts[0].Seq, Limit: 1})
		if err != nil {
			return durable.Receipt{}, err
		}
		if len(prior.Records) != 1 {
			return durable.Receipt{}, errors.New("missing callback task fence")
		}
		revision = prior.Records[0].Revision
	}
	batch.Expected = append(batch.Expected, durable.RevisionCheck{SessionID: task.SessionID, Kind: durable.TaskKind, Namespace: Namespace, ID: task.ID, Revision: revision})
	return r.options.Store.Commit(ctx, batch)
}

// Suspend joins local code without changing task outcomes. Background anchors
// keep their durable phase and deadline for the next explicit Resume.
func (r *Runtime) Suspend(ctx context.Context) error {
	r.mu.Lock()
	r.paused = true
	var joins []chan struct{}
	for id, invocation := range r.live {
		task := r.tasks[id]
		if task.Status == "running" || task.Status == "aborting" || task.Status == "runnable" {
			task.Status = "suspended"
			if err := r.commit(ctx, fmt.Sprintf("task:suspend:%s:%d", id, task.Revision), []extension.Task{task}, durable.Commit{}); err != nil {
				r.mu.Unlock()
				return err
			}
		}
		invocation.cancel()
		joins = append(joins, invocation.done)
	}
	r.mu.Unlock()
	for _, done := range joins {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (r *Runtime) notify() {
	close(r.changed)
	r.changed = make(chan struct{})
}

func (r *Runtime) suspendAttempts(ctx context.Context, attempts map[string]uint64) error {
	r.mu.Lock()
	var joins []chan struct{}
	for id, attempt := range attempts {
		invocation := r.live[id]
		task := r.tasks[id]
		if invocation == nil || task.Attempt != attempt {
			continue
		}
		if !r.closed && (task.Status == "running" || task.Status == "aborting") {
			task.Status = "suspended"
			if err := r.commit(ctx, fmt.Sprintf("task:suspend:%s:%d", id, task.Revision), []extension.Task{task}, durable.Commit{}); err != nil {
				r.mu.Unlock()
				return err
			}
		}
		invocation.cancel()
		joins = append(joins, invocation.done)
	}
	r.mu.Unlock()
	for _, done := range joins {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
