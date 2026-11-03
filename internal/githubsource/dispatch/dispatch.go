// Package dispatch runs admitted GitHub Tickets in worker sessions: one Host
// session and one author-branch worktree per Ticket, a durable submission per
// dispatch, steering of new Inputs into running workers, and settlement of
// every dispatch back into control.
//
// The dispatcher keeps no durable state of its own. Every step resumes from
// what control and the worker session committed: control's dispatch record
// says which step was reached, the worker session's submission receipts say
// whether a run started, what it consumed and how it ended, and the clone's
// worktree registration says whether the checkout exists. A restart at any
// point therefore adopts what exists and never starts a second worker
// session, a second run for one dispatch, or a second delivery of one Input.
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
	"go.harness.dev/harness/tempest"
)

type authorityKey struct{}

// Issued reports whether ctx comes from a Dispatcher's Run. A Host whose
// Authorizer lets only such contexts run or change worker sessions keeps
// every worker run inside a dispatch: its lease, its budget and Ticket stop.
func Issued(ctx context.Context) bool {
	issued, _ := ctx.Value(authorityKey{}).(bool)
	return issued
}

// Options configures New.
type Options struct {
	// Host owns worker sessions. Its registry must hold the github extension
	// (model.ExtensionID); every worker session selects it as required and
	// no other session gets it from the dispatcher.
	Host  *tempest.Host
	Store model.DispatchStore
	// Config supplies the clone, state directory, worker settings, limits
	// and team prompts.
	Config model.Config
	// Repo supplies the base branch when Config.Workers.BaseBranch is empty.
	Repo   model.RepoMeta
	Clock  model.Clock
	GitEnv []string
	Logger *slog.Logger
	// Interval bounds how long due work (a passed RetryAt) waits without a
	// Wake. Default 15 seconds.
	Interval time.Duration
}

const (
	defaultInterval     = 15 * time.Second
	defaultWakeBytes    = 64 << 10
	defaultPrepareLimit = 10 * time.Minute
	// transientBackoff is the first RetryAt delay of a transient abandon;
	// each further attempt doubles it.
	transientBackoff = 30 * time.Second
	// executorRetry is the first delay before an executor that stopped on
	// an unexpected error resumes its still-live dispatch.
	executorRetry    = time.Second
	maxExecutorRetry = time.Minute
)

// Dispatcher implements model.Dispatcher.
type Dispatcher struct {
	host      *tempest.Host
	store     model.DispatchStore
	config    model.Config
	clock     model.Clock
	env       []string
	log       *slog.Logger
	interval  time.Duration
	worktrees string
	base      string
	remote    string
	wakeBytes int

	wake    chan struct{}
	running atomic.Bool

	mu         sync.Mutex
	executions map[model.DispatchID]*execution
	// backoff delays resuming a dispatch whose executor failed without
	// settling it.
	backoff map[model.DispatchID]retryState

	// hook, when set by tests, runs at named crash points; an error stops
	// the executor there as a process death would.
	hook func(point string) error
}

type retryState struct {
	at    time.Time
	delay time.Duration
}

var _ model.Dispatcher = (*Dispatcher)(nil)

// New validates options and prepares <stateDir>/worktrees.
func New(options Options) (*Dispatcher, error) {
	if options.Host == nil || options.Store == nil || options.Clock == nil {
		return nil, errors.New("dispatch: host, store and clock are required")
	}
	config := options.Config
	if !filepath.IsAbs(config.Clone) || !filepath.IsAbs(config.StateDir) {
		return nil, errors.New("dispatch: clone and stateDir must be absolute")
	}
	base := config.Workers.BaseBranch
	if base == "" {
		base = options.Repo.DefaultBranch
	}
	if base == "" {
		return nil, errors.New("dispatch: no base branch: set workers.baseBranch or the repository default branch")
	}
	worktrees := filepath.Join(config.StateDir, "worktrees")
	if err := os.MkdirAll(worktrees, 0o700); err != nil {
		return nil, err
	}
	// git registers worktrees by resolved path; resolve once so session
	// cwd, worktree registration and control records all name one path.
	worktrees, err := filepath.EvalSymlinks(worktrees)
	if err != nil {
		return nil, err
	}
	remote := config.Workers.Remote
	if remote == "" {
		remote = "origin"
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	interval := options.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	wakeBytes := config.Limits.MaxWakeBytes
	if wakeBytes <= 0 {
		wakeBytes = defaultWakeBytes
	}
	return &Dispatcher{
		host: options.Host, store: options.Store, config: config, clock: options.Clock,
		env: append([]string(nil), options.GitEnv...), log: logger, interval: interval,
		worktrees: worktrees, base: base, remote: remote, wakeBytes: wakeBytes,
		wake:       make(chan struct{}, 1),
		executions: make(map[model.DispatchID]*execution),
		backoff:    make(map[model.DispatchID]retryState),
	}, nil
}

// Wake nudges Run after a commit that may have created, stopped or fed work.
func (d *Dispatcher) Wake() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Run starts resuming every live dispatch from its committed step before
// its first admission, then admits, executes, steers and stops work until
// ctx ends; admission does not wait for resumed dispatches to settle, whose
// slots stay held meanwhile. A dispatch is settled (Finished or Abandon)
// only from a proven result; on shutdown its slot stays held for the next
// Run to reconcile. Its context carries the authority Issued reports.
func (d *Dispatcher) Run(ctx context.Context) error {
	if !d.running.CompareAndSwap(false, true) {
		return errors.New("dispatch: Run is already running")
	}
	defer d.running.Store(false)
	var executors sync.WaitGroup
	defer executors.Wait()
	ctx, cancel := context.WithCancel(context.WithValue(ctx, authorityKey{}, true))
	defer cancel()
	active, err := d.store.Active(ctx)
	if err != nil {
		return fmt.Errorf("dispatch: reconcile active dispatches: %w", err)
	}
	for _, admitted := range active {
		d.start(ctx, &executors, admitted)
	}
	timer := time.NewTimer(d.interval)
	defer timer.Stop()
	for {
		d.step(ctx, &executors)
		timer.Reset(d.interval)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-d.wake:
		case <-timer.C:
		}
	}
}

// step is one scheduling pass: resume or stop live dispatches, admit queued
// ones into free slots, then steer undelivered Inputs into running workers.
func (d *Dispatcher) step(ctx context.Context, executors *sync.WaitGroup) {
	active, err := d.store.Active(ctx)
	if err != nil {
		d.log.Warn("github dispatch: list active dispatches", "error", err)
		return
	}
	for _, admitted := range active {
		if execution := d.execution(admitted.Dispatch.ID); execution != nil {
			if admitted.Ticket.Stopped {
				execution.stop()
			}
			continue
		}
		d.start(ctx, executors, admitted)
	}
	admitted, err := d.store.AdmitNext(ctx, d.clock.Now())
	if err != nil {
		d.log.Warn("github dispatch: admit", "error", err)
	}
	for _, next := range admitted {
		d.start(ctx, executors, next)
	}
	d.deliver(ctx)
}

func (d *Dispatcher) execution(id model.DispatchID) *execution {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.executions[id]
}

// start runs one executor per live dispatch, unless one runs already or a
// failed executor is still backing off.
func (d *Dispatcher) start(ctx context.Context, executors *sync.WaitGroup, admitted model.AdmittedDispatch) {
	id := admitted.Dispatch.ID
	d.mu.Lock()
	if d.executions[id] != nil || time.Now().Before(d.backoff[id].at) {
		d.mu.Unlock()
		return
	}
	execution := &execution{dispatch: id, ticket: admitted.Ticket.ID, gen: admitted.Dispatch.Gen, stopped: admitted.Ticket.Stopped}
	d.executions[id] = execution
	d.mu.Unlock()
	executors.Go(func() {
		err := d.execute(ctx, execution, admitted)
		execution.close()
		d.mu.Lock()
		delete(d.executions, id)
		if err != nil && ctx.Err() == nil {
			delay := min(max(d.backoff[id].delay*2, executorRetry), maxExecutorRetry)
			d.backoff[id] = retryState{at: time.Now().Add(delay), delay: delay}
			time.AfterFunc(delay, d.Wake)
		} else {
			delete(d.backoff, id)
		}
		d.mu.Unlock()
		if err != nil && ctx.Err() == nil {
			d.log.Warn("github dispatch: executor stopped before settling; it resumes from the committed step", "dispatch", id, "error", err)
		}
		d.Wake()
	})
}

// point runs the test crash hook.
func (d *Dispatcher) point(name string) error {
	if d.hook == nil {
		return nil
	}
	return d.hook(name)
}

// execution is the in-process state of one live dispatch: the worker
// session once its run is submitted, whether Inputs may be steered into it,
// and whether its Ticket was stopped.
type execution struct {
	dispatch model.DispatchID
	ticket   model.TicketID
	gen      uint32

	mu        sync.Mutex
	session   *tempest.Session
	accepting bool
	stopped   bool
}

// stop marks the Ticket stopped and cancels the worker's current run. The
// executor settles the dispatch once the run has ended.
func (e *execution) stop() {
	e.mu.Lock()
	if e.stopped {
		e.mu.Unlock()
		return
	}
	e.stopped = true
	session := e.session
	e.mu.Unlock()
	if session != nil {
		session.Cancel()
	}
}

func (e *execution) isStopped() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stopped
}

// running publishes the worker session of a submitted run and opens it to
// deliveries. A stop that arrived earlier cancels the run now.
func (e *execution) running(session *tempest.Session) {
	e.mu.Lock()
	e.session, e.accepting = session, !e.stopped
	stopped := e.stopped
	e.mu.Unlock()
	if stopped {
		session.Cancel()
	}
}

// close ends deliveries. It waits for a delivery in progress, so after it
// returns no Input reaches the session's queue until running reopens it.
func (e *execution) close() {
	e.mu.Lock()
	e.accepting = false
	e.mu.Unlock()
}
