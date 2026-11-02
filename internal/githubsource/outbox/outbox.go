// Package outbox publishes the control journal's GitHub write intents:
// comments, review replies, pushes, pull request creation and edits. Workers
// hold no GitHub write credential; every write runs here, as the dedicated
// account, under the intent-before-effect rule:
//
//  1. the intent is committed (a worker tool's Enqueue);
//  2. Attempt commits it attempting before anything is sent;
//  3. the request is sent once;
//  4. Settle records what GitHub returned.
//
// A write whose reply is lost (gh printed no status, a 5xx, a timeout, a
// crash after step 2) may have reached GitHub. It is never sent again on
// that suspicion: reconciliation looks for it first. Comments and pull
// requests carry the marker <!-- tempest:<OutboxID> --> and are searched
// for by marker, author and thread; a push is checked against the branch's
// SHA on GitHub. Found settles confirmed, proven absent goes back to
// pending for a later attempt, and a search that cannot finish or a branch
// moved by someone else settles unknown, which only an operator resolves.
package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
)

// Options configures New.
type Options struct {
	// Client is the account's REST client.
	Client model.GHClient
	// Store is the control journal's effect side.
	Store model.EffectStore
	// Config supplies the repository, the account (its ID scopes
	// reconciliation; GHPath and GHConfigDir are git's credential) and the
	// push remote.
	Config model.Config
	Clock  model.Clock
	// GitEnv is added to git push's environment after the inherited one,
	// from which GH_*, GitHub token and git routing variables are dropped;
	// the account's credential selection is added last and always wins.
	GitEnv []string
	Logger *slog.Logger
	// Interval bounds how long a due retry waits without a Wake. Default
	// 15 seconds.
	Interval time.Duration
}

const (
	defaultInterval = 15 * time.Second
	defaultRemote   = "origin"
	// retryBackoff is the delay after the first failed attempt; each later
	// attempt doubles it up to maxRetryBackoff.
	retryBackoff    = 30 * time.Second
	maxRetryBackoff = 15 * time.Minute
	// maxReconcileReads bounds how often one process tries to read an
	// attempting write's outcome before settling it unknown.
	maxReconcileReads = 3
	reconcileBackoff  = 30 * time.Second
	// reconcileSkew widens a marker search below the intent's creation
	// time: the intent carries the controller clock, comments GitHub's.
	reconcileSkew = time.Hour
	// maxScanPages bounds one marker search; past it the search cannot
	// prove absence.
	maxScanPages = 100
	scanPageSize = 100
)

// Outbox implements model.Outbox.
type Outbox struct {
	client   model.GHClient
	store    model.EffectStore
	clock    model.Clock
	log      *slog.Logger
	interval time.Duration

	repo    string
	owner   string
	account int64
	git     gitRunner

	wake    chan struct{}
	running atomic.Bool

	mu sync.Mutex
	// reads counts failed outcome reads of attempting intents in this
	// process; next delays the following read.
	reads map[model.OutboxID]readState

	// hook, when set by tests, runs at named crash points; an error stops
	// processing there as a process death would.
	hook func(point string, intent model.OutboxIntent) error
}

type readState struct {
	failures int
	next     time.Time
}

var _ model.Outbox = (*Outbox)(nil)

// New validates options.
func New(options Options) (*Outbox, error) {
	if options.Client == nil || options.Store == nil || options.Clock == nil {
		return nil, errors.New("outbox: client, store and clock are required")
	}
	config := options.Config
	owner, name, found := strings.Cut(config.Repository, "/")
	if !found || owner == "" || name == "" || strings.Contains(name, "/") {
		return nil, fmt.Errorf("outbox: repository %q is not owner/repo", config.Repository)
	}
	if config.Account.ID <= 0 {
		return nil, errors.New("outbox: account id is required: it scopes reconciliation to the account's own writes")
	}
	if config.Account.GHPath == "" || config.Account.GHConfigDir == "" {
		return nil, errors.New("outbox: account ghPath and ghConfigDir are required for pushes")
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	interval := options.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	remote := config.Workers.Remote
	if remote == "" {
		remote = defaultRemote
	}
	return &Outbox{
		client: options.Client, store: options.Store, clock: options.Clock, log: logger, interval: interval,
		repo: config.Repository, owner: owner, account: config.Account.ID,
		git:   newGitRunner(config.Account, remote, options.GitEnv),
		wake:  make(chan struct{}, 1),
		reads: make(map[model.OutboxID]readState),
	}, nil
}

// Wake nudges Run after a commit that may have created or released work.
func (o *Outbox) Wake() {
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

// Run publishes due intents until ctx ends. Intents left attempting by an
// earlier process are due first and are reconciled, never resent.
func (o *Outbox) Run(ctx context.Context) error {
	if !o.running.CompareAndSwap(false, true) {
		return errors.New("outbox: Run is already running")
	}
	defer o.running.Store(false)
	timer := time.NewTimer(o.interval)
	defer timer.Stop()
	for {
		if err := o.publish(ctx); err != nil && ctx.Err() == nil {
			o.log.Warn("github outbox: publish", "error", err)
		}
		timer.Reset(o.interval)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-o.wake:
		case <-timer.C:
		}
	}
}

// publish works once on every due intent: at most one per Ticket, in the
// Ticket's order. A settled intent may make the Ticket's next one due, so
// it wakes Run for another pass.
func (o *Outbox) publish(ctx context.Context) error {
	due, err := o.store.Due(ctx, o.clock.Now())
	if err != nil {
		return fmt.Errorf("outbox: list due intents: %w", err)
	}
	var errs []error
	for _, intent := range due {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		settled, err := o.process(ctx, intent)
		if err != nil {
			errs = append(errs, fmt.Errorf("outbox: intent %s: %w", intent.ID, err))
		}
		if settled {
			o.Wake()
		}
	}
	return errors.Join(errs...)
}

// process attempts a pending intent or reconciles an attempting one, and
// reports whether it settled.
func (o *Outbox) process(ctx context.Context, due model.OutboxIntent) (bool, error) {
	if due.State == model.EffectAttempting {
		if !o.readDue(due.ID) {
			return false, nil
		}
		return o.settle(ctx, due, o.reconcile(ctx, due))
	}
	intent, err := o.store.Attempt(ctx, due.ID, o.clock.Now())
	if err != nil {
		if intent.State == model.EffectFailed || intent.State == model.EffectCancelled {
			o.log.Info("github outbox: intent settled at attempt", "intent", due.ID, "state", intent.State, "reason", intent.LastError)
			return true, nil
		}
		return false, err
	}
	if intent.State != model.EffectAttempting {
		return false, fmt.Errorf("attempt left state %s", intent.State)
	}
	if err := o.point("attempted", intent); err != nil {
		return false, err
	}
	return o.settle(ctx, intent, o.attempt(ctx, intent))
}

// step is what one attempt or reconciliation decided.
type step struct {
	state   model.EffectState
	result  *model.EffectReply
	err     error
	retryAt time.Time
	// keep leaves the intent attempting: its outcome could not be read
	// yet. Nothing is sent again until a later read decides it.
	keep bool
}

func confirmed(result *model.EffectReply) step {
	return step{state: model.EffectConfirmed, result: result}
}

func failed(err error) step { return step{state: model.EffectFailed, err: err} }

func unknown(err error) step { return step{state: model.EffectUnknown, err: err} }

// absent is a proven non-write: the intent goes back to pending.
func (o *Outbox) absent(intent model.OutboxIntent, reason error, retryAfter time.Duration) step {
	delay := retryBackoff
	for range max(intent.Attempts-1, 0) {
		delay = min(delay*2, maxRetryBackoff)
	}
	return step{state: model.EffectPending, err: reason, retryAt: o.clock.Now().Add(max(delay, retryAfter))}
}

// unread keeps the intent attempting after a failed outcome read.
func unread(err error) step { return step{keep: true, err: err} }

// settle commits a step. A kept intent stays attempting: after
// maxReconcileReads failed reads in this process it settles unknown.
func (o *Outbox) settle(ctx context.Context, intent model.OutboxIntent, decided step) (bool, error) {
	if ctx.Err() != nil {
		// Shutdown: whatever was sent is reconciled by the next process.
		return false, ctx.Err()
	}
	if decided.keep {
		if !o.readFailed(intent.ID) {
			o.log.Warn("github outbox: outcome unread; reading again later", "intent", intent.ID, "error", decided.err)
			return false, nil
		}
		decided = unknown(fmt.Errorf("outcome could not be read after %d tries: %w", maxReconcileReads, decided.err))
	}
	if err := o.point("decided", intent); err != nil {
		return false, err
	}
	settlement := model.EffectSettlement{State: decided.state, Result: decided.result, RetryAt: decided.retryAt, SettledAt: o.clock.Now()}
	if decided.err != nil {
		settlement.Error = bound(decided.err.Error())
	}
	settledIntent, err := o.store.Settle(ctx, intent.ID, settlement)
	if err != nil {
		return false, fmt.Errorf("settle %s: %w", decided.state, err)
	}
	o.forget(intent.ID)
	if settledIntent.State != model.EffectConfirmed {
		o.log.Info("github outbox: intent settled", "intent", intent.ID, "kind", intent.Kind, "state", settledIntent.State, "reason", settlement.Error)
	}
	return true, nil
}

func (o *Outbox) point(name string, intent model.OutboxIntent) error {
	if o.hook == nil {
		return nil
	}
	return o.hook(name, intent)
}

func (o *Outbox) readDue(id model.OutboxID) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return !o.clock.Now().Before(o.reads[id].next)
}

// readFailed records a failed outcome read and reports whether the limit
// is reached.
func (o *Outbox) readFailed(id model.OutboxID) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	state := o.reads[id]
	state.failures++
	delay := reconcileBackoff << (state.failures - 1)
	state.next = o.clock.Now().Add(delay)
	o.reads[id] = state
	return state.failures >= maxReconcileReads
}

func (o *Outbox) forget(id model.OutboxID) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.reads, id)
}

// attempt sends a freshly attempting intent once and decides its outcome.
func (o *Outbox) attempt(ctx context.Context, intent model.OutboxIntent) step {
	switch intent.Kind {
	case model.EffectComment, model.EffectReviewReply:
		return o.attemptComment(ctx, intent)
	case model.EffectPush:
		return o.attemptPush(ctx, intent)
	case model.EffectOpenPR:
		return o.attemptOpenPR(ctx, intent)
	case model.EffectUpdatePR:
		return o.attemptUpdatePR(ctx, intent)
	}
	return failed(fmt.Errorf("unknown effect kind %q", intent.Kind))
}

// reconcile decides an intent found attempting: its request may or may not
// have reached GitHub.
func (o *Outbox) reconcile(ctx context.Context, intent model.OutboxIntent) step {
	switch intent.Kind {
	case model.EffectComment, model.EffectReviewReply:
		return o.reconcileComment(ctx, intent)
	case model.EffectPush:
		return o.reconcilePush(ctx, intent)
	case model.EffectOpenPR:
		return o.reconcileOpenPR(ctx, intent)
	case model.EffectUpdatePR:
		return o.reconcileUpdatePR(ctx, intent)
	}
	return unknown(fmt.Errorf("unknown effect kind %q", intent.Kind))
}

// verdict classifies a write request's error.
type verdict int

const (
	// sent: GitHub accepted the write.
	sent verdict = iota
	// maybeSent: the write may have reached GitHub (no status, 5xx,
	// timeout); its outcome must be read, never assumed.
	maybeSent
	// refusedRetry: GitHub refused it for now (rate limit, 404 that may
	// be lost access); nothing was written.
	refusedRetry
	// refused: GitHub refused it for good (validation, permission).
	refused
)

func classify(err error) (verdict, time.Duration) {
	if err == nil {
		return sent, 0
	}
	var ghErr *model.GHError
	if !errors.As(err, &ghErr) {
		return maybeSent, 0
	}
	switch {
	case ghErr.Status == 0, ghErr.Status >= http.StatusInternalServerError, ghErr.Status == http.StatusRequestTimeout:
		return maybeSent, 0
	case ghErr.Transient():
		return refusedRetry, ghErr.RetryAfter
	}
	return refused, 0
}

// status returns the HTTP status of a GHError, zero otherwise.
func status(err error) int {
	var ghErr *model.GHError
	if errors.As(err, &ghErr) {
		return ghErr.Status
	}
	return 0
}

// maxErrorBytes bounds error text stored in an intent.
const maxErrorBytes = 1024

func bound(text string) string {
	if len(text) <= maxErrorBytes {
		return text
	}
	cut := maxErrorBytes
	for cut > 0 && text[cut]&0xC0 == 0x80 {
		cut--
	}
	return text[:cut] + "…"
}

// marked appends the intent's marker to a body it posts.
func marked(body string, id model.OutboxID) string {
	return strings.TrimRight(body, "\n") + "\n\n" + model.OutboxMarker(id)
}
