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
// A write whose reply is lost (gh printed no status, a 5xx, a 408, a
// timeout, a crash after step 2) may have reached GitHub. Reconciliation
// looks for it: comments and pull requests end with the marker
// <!-- tempest:<OutboxID> --> and are searched for by marker, author and
// thread; a push is checked against the branch's SHA on GitHub.
//
// Found settles confirmed. Not finding a comment or pull request proves
// nothing: GitHub's listings are mutable and eventually consistent, and a
// human may have edited the marker away. Such a write is never sent again
// automatically. Because a listing can lag the write, it is read again on
// later passes (absentReads reads, the last at least absentWindow after
// the attempt) and settles unknown only if it is still not found; then
// only an operator resolves it. Only a write proven not sent (GitHub
// refused it before writing, or nothing was sent yet) goes back to
// pending. A push is the exception: it carries an exact lease and is
// repeatable, so a branch still at the lease retries.
package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
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
	// Config supplies the repository (the push destination), the account
	// (its ID scopes reconciliation; GHPath and GHConfigDir are git's
	// credential) and StateDir, which holds the outbox's push repository.
	Config model.Config
	Clock  model.Clock
	Logger *slog.Logger
	// Interval bounds how long a due retry waits without a Wake. Default
	// 15 seconds.
	Interval time.Duration
	// PushRepository replaces the push destination
	// https://github.com/<repository>.git with a local bare repository path,
	// pushed over git's file protocol. Only hermetic end-to-end tests set it;
	// the operator file has no such field.
	PushRepository string
}

const (
	defaultInterval = 15 * time.Second
	// maxInFlight bounds how many Tickets publish at once. Each Ticket has
	// at most one operation in flight, so its writes keep their order; a
	// slow push or scan holds only its own Ticket.
	maxInFlight = 4
	// retryBackoff is the delay after the first refused attempt; each
	// later attempt doubles it up to maxRetryBackoff.
	retryBackoff    = 30 * time.Second
	maxRetryBackoff = 15 * time.Minute
	// maxReconcileReads bounds how often one process tries to read an
	// attempting write's outcome before settling it unknown.
	maxReconcileReads = 3
	reconcileBackoff  = 30 * time.Second
	// A comment or pull request that may have been written and is not
	// found is read again absentBackoff later, since GitHub's listings
	// can lag a write. It settles unknown after absentReads such reads in
	// one process, the last at least absentWindow after the attempt.
	absentReads   = 3
	absentWindow  = 2 * time.Minute
	absentBackoff = time.Minute
	// maxScanPages bounds one listing search; past it the search fails.
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
	// flights counts operations still running, so Run returns after them.
	flights sync.WaitGroup

	mu sync.Mutex
	// reads counts outcome reads of attempting intents in this process
	// that failed or did not find the write; next delays the following
	// read.
	reads map[model.OutboxID]readState
	// busy holds the Tickets with an operation in flight; released holds
	// the ones whose operation ended since the current pass listed due
	// intents, whose listed intent may already be settled.
	busy, released map[model.TicketID]bool
	// deferred is set when a due intent waited for a free slot.
	deferred bool
	// coolUntil is the account's rate-limit wait: no request is sent before
	// it, by any Ticket.
	coolUntil time.Time

	// hook, when set by tests, runs at named crash points; an error stops
	// processing there as a process death would.
	hook func(point string, intent model.OutboxIntent) error
}

type readState struct {
	failures int
	absent   int
	next     time.Time
	// cause is the ambiguous failure of this process's own attempt, kept
	// for the error of a later read that still does not find the write.
	cause error
}

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
	git, err := newGitRunner(config, owner, name)
	if err != nil {
		return nil, err
	}
	if options.PushRepository != "" {
		if !filepath.IsAbs(options.PushRepository) {
			return nil, fmt.Errorf("outbox: push repository %q is not absolute", options.PushRepository)
		}
		git.url, git.protocol = options.PushRepository, "file"
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	interval := options.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	return &Outbox{
		client: options.Client, store: options.Store, clock: options.Clock, log: logger, interval: interval,
		repo: config.Repository, owner: owner, account: config.Account.ID, git: git,
		wake:  make(chan struct{}, 1),
		reads: make(map[model.OutboxID]readState),
		busy:  make(map[model.TicketID]bool), released: make(map[model.TicketID]bool),
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
	defer o.flights.Wait()
	timer := time.NewTimer(o.interval)
	defer timer.Stop()
	for {
		if err := o.pass(ctx, nil); err != nil && ctx.Err() == nil {
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

// publish runs one pass and waits for the operations it started.
func (o *Outbox) publish(ctx context.Context) error {
	var collected errorList
	err := o.pass(ctx, &collected)
	return errors.Join(err, collected.join())
}

// errorList collects the errors of concurrent operations.
type errorList struct {
	mu   sync.Mutex
	errs []error
	done sync.WaitGroup
}

func (l *errorList) add(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errs = append(l.errs, err)
}

func (l *errorList) join() error {
	l.done.Wait()
	l.mu.Lock()
	defer l.mu.Unlock()
	return errors.Join(l.errs...)
}

// pass starts one operation on each due intent whose Ticket is idle, up to
// maxInFlight. A finished operation wakes Run, which starts the Ticket's
// next intent and any skipped here. With collect set, operation errors are
// gathered for the caller instead of logged.
func (o *Outbox) pass(ctx context.Context, collect *errorList) error {
	if o.cooling() {
		return nil
	}
	o.mu.Lock()
	clear(o.released)
	o.mu.Unlock()
	due, err := o.store.Due(ctx, o.clock.Now())
	if err != nil {
		return fmt.Errorf("outbox: list due intents: %w", err)
	}
	for _, intent := range due {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !o.claim(intent.Ticket) {
			continue
		}
		o.flights.Add(1)
		if collect != nil {
			collect.done.Add(1)
		}
		go func() {
			defer o.flights.Done()
			if collect != nil {
				defer collect.done.Done()
			}
			settled, err := o.process(ctx, intent)
			deferred := o.release(intent.Ticket)
			switch {
			case err != nil && collect != nil:
				collect.add(fmt.Errorf("outbox: intent %s: %w", intent.ID, err))
			case err != nil && ctx.Err() == nil:
				o.log.Warn("github outbox: publish", "intent", intent.ID, "error", err)
			}
			if settled || deferred {
				o.Wake()
			}
		}()
	}
	return nil
}

// claim marks a Ticket busy unless it already is, its operation ended since
// the listing (the listed intent is stale), or maxInFlight is reached.
func (o *Outbox) claim(ticket model.TicketID) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch {
	case o.busy[ticket] || o.released[ticket]:
		return false
	case len(o.busy) >= maxInFlight:
		o.deferred = true
		return false
	}
	o.busy[ticket] = true
	return true
}

// release frees a Ticket and reports whether a pass skipped a due intent
// for want of a free slot, which needs another pass now.
func (o *Outbox) release(ticket model.TicketID) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.busy, ticket)
	o.released[ticket] = true
	deferred := o.deferred
	o.deferred = false
	return deferred
}

// cooling reports whether the account is inside a rate-limit wait.
func (o *Outbox) cooling() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.clock.Now().Before(o.coolUntil)
}

// do sends one REST request as the account. Inside a rate-limit wait it
// sends nothing and returns the refusal GitHub would; a reply carrying
// Retry-After starts or extends the wait for every Ticket.
func (o *Outbox) do(ctx context.Context, request model.GHRequest) (model.GHResponse, error) {
	o.mu.Lock()
	wait := o.coolUntil.Sub(o.clock.Now())
	o.mu.Unlock()
	if wait > 0 {
		return model.GHResponse{}, &model.GHError{Status: http.StatusTooManyRequests, Message: fmt.Sprintf("%s %s: not sent: the account is rate limited", request.Method, request.Path), RetryAfter: wait}
	}
	response, err := o.client.Do(ctx, request)
	if after := retryAfter(err); after > 0 {
		o.mu.Lock()
		if until := o.clock.Now().Add(after); until.After(o.coolUntil) {
			o.coolUntil = until
		}
		o.mu.Unlock()
		o.log.Warn("github outbox: account rate limited", "retryAfter", after)
	}
	return response, err
}

// retryAfter is GitHub's requested wait carried by err, zero without one.
func retryAfter(err error) time.Duration {
	var ghErr *model.GHError
	if errors.As(err, &ghErr) {
		return ghErr.RetryAfter
	}
	return 0
}

// process attempts a pending intent or reconciles an attempting one, and
// reports whether it settled.
func (o *Outbox) process(ctx context.Context, due model.OutboxIntent) (bool, error) {
	if due.State == model.EffectAttempting {
		if !o.readDue(due.ID) {
			return false, nil
		}
		return o.settle(ctx, due, o.reconcile(ctx, due, o.cause(due.ID)))
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
	// absent is a write that may have been sent and was not found. It
	// settles as state (unknown) only after the listing had time to show
	// it (absentRead); until then it stays attempting.
	absent bool
	// cause is an absent write's ambiguous failure.
	cause error
}

func confirmed(result *model.EffectReply) step {
	return step{state: model.EffectConfirmed, result: result}
}

func failed(err error) step { return step{state: model.EffectFailed, err: err} }

func unknown(err error) step { return step{state: model.EffectUnknown, err: err} }

// unsure decides a comment or pull request whose request may have reached
// GitHub but which no listing shows: read again later, then unknown, for an
// operator.
func unsure(what string, cause error) step {
	return step{state: model.EffectUnknown, absent: true, cause: cause, err: fmt.Errorf("%s may have been sent and is not found; it is not sent again until an operator resolves it: %w", what, cause)}
}

// retry sends a write proven not sent back to pending.
func (o *Outbox) retry(intent model.OutboxIntent, reason error, retryAfter time.Duration) step {
	delay := retryBackoff
	for range max(intent.Attempts-1, 0) {
		delay = min(delay*2, maxRetryBackoff)
	}
	return step{state: model.EffectPending, err: reason, retryAt: o.clock.Now().Add(max(delay, retryAfter))}
}

// unread keeps the intent attempting after a failed outcome read.
func unread(err error) step { return step{keep: true, err: err} }

// settle commits a step. A kept intent stays attempting: after
// maxReconcileReads failed reads in this process it settles unknown. An
// absent one stays attempting until absentRead says the listing had time
// to show it.
func (o *Outbox) settle(ctx context.Context, intent model.OutboxIntent, decided step) (bool, error) {
	if ctx.Err() != nil {
		// Shutdown: whatever was sent is reconciled by the next process.
		return false, ctx.Err()
	}
	if decided.keep {
		if !o.readFailed(intent.ID, retryAfter(decided.err)) {
			o.log.Warn("github outbox: outcome unread; reading again later", "intent", intent.ID, "error", decided.err)
			return false, nil
		}
		decided = unknown(fmt.Errorf("outcome could not be read after %d tries: %w", maxReconcileReads, decided.err))
	}
	if decided.absent {
		reads, done := o.absentRead(intent, decided.cause)
		if !done {
			o.log.Info("github outbox: write not found yet; reading again later", "intent", intent.ID, "kind", intent.Kind, "reads", reads)
			return false, nil
		}
		decided.err = fmt.Errorf("%w (not found in %d reads over %s)", decided.err, reads, o.clock.Now().Sub(intent.AttemptedAt).Round(time.Second))
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

// readFailed records a failed outcome read, delayed at least by GitHub's
// requested wait, and reports whether the limit is reached.
func (o *Outbox) readFailed(id model.OutboxID, wait time.Duration) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	state := o.reads[id]
	state.failures++
	delay := max(reconcileBackoff<<(state.failures-1), wait)
	state.next = o.clock.Now().Add(delay)
	o.reads[id] = state
	return state.failures >= maxReconcileReads
}

// absentRead records a read that did not find a write that may have been
// sent, and reports the count and whether to settle it unknown now:
// after absentReads reads, the last at least absentWindow after the
// attempt. Otherwise the next read waits absentBackoff, and the last one
// until the window has passed. The first cause seen is kept for the reads
// of reconciliation, which no longer know it.
func (o *Outbox) absentRead(intent model.OutboxIntent, cause error) (int, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := o.clock.Now()
	state := o.reads[intent.ID]
	state.absent++
	if state.cause == nil {
		state.cause = cause
	}
	windowEnd := intent.AttemptedAt.Add(absentWindow)
	if state.absent >= absentReads && !now.Before(windowEnd) {
		return state.absent, true
	}
	state.next = now.Add(absentBackoff)
	if state.absent+1 >= absentReads && state.next.Before(windowEnd) {
		state.next = windowEnd
	}
	o.reads[intent.ID] = state
	return state.absent, false
}

// cause is the ambiguous failure this process saw when it sent the
// intent, or nil after a restart.
func (o *Outbox) cause(id model.OutboxID) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.reads[id].cause
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
// have reached GitHub. cause is the ambiguous failure this process saw
// when it sent the request, nil when an earlier process sent it.
func (o *Outbox) reconcile(ctx context.Context, intent model.OutboxIntent, cause error) step {
	if cause == nil {
		cause = errors.New("the process stopped after the attempt was recorded")
	}
	switch intent.Kind {
	case model.EffectComment, model.EffectReviewReply:
		return o.reconcileComment(ctx, intent, cause)
	case model.EffectPush:
		return o.reconcilePush(ctx, intent)
	case model.EffectOpenPR:
		return o.searchPR(ctx, intent, cause)
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
	// 408, timeout); its outcome must be read, never assumed.
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

// marked appends the intent's marker line to a body it posts.
func marked(body string, id model.OutboxID) string {
	return strings.TrimRight(body, "\n") + "\n\n" + model.OutboxMarker(id)
}

// carries reports whether body ends with the intent's marker line, as
// marked leaves it. A marker quoted anywhere else in a body does not count.
func carries(body string, id model.OutboxID) bool {
	rest, found := strings.CutSuffix(strings.TrimRight(body, " \t\r\n"), model.OutboxMarker(id))
	return found && strings.HasSuffix(rest, "\n")
}
