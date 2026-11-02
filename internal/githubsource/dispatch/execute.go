package dispatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
	"go.harness.dev/harness/tempest"
	"go.harness.dev/harness/tempest/durable"
)

// failure is a pre-submission failure that settles the dispatch with
// Abandon: transient ones are retried by control with backoff, others hold
// the Ticket failed.
type failure struct {
	reason    string
	transient bool
	cause     error
}

func (f *failure) Error() string {
	if f.cause == nil {
		return f.reason
	}
	return f.reason + ": " + f.cause.Error()
}

func (f *failure) Unwrap() error { return f.cause }

func transientFailure(reason string, cause error) error {
	return &failure{reason: reason, transient: true, cause: cause}
}

func permanentFailure(reason string, cause error) error {
	return &failure{reason: reason, cause: cause}
}

// execute drives one live dispatch from its committed step to settlement.
// It returns an error, leaving the dispatch live, only when it could not
// prove an outcome; the next pass resumes it.
func (d *Dispatcher) execute(ctx context.Context, execution *execution, admitted model.AdmittedDispatch) error {
	if err := d.point("admitted"); err != nil {
		return err
	}
	var session *tempest.Session
	if admitted.Dispatch.State == model.DispatchSubmitted {
		opened, err := d.host.Open(ctx, admitted.Ticket.SessionID)
		if err != nil {
			return fmt.Errorf("open worker session %s: %w", admitted.Ticket.SessionID, err)
		}
		session = opened
	} else {
		submitted, err := d.launch(ctx, execution, admitted)
		if err != nil {
			return d.abandon(ctx, admitted, err)
		}
		session = submitted
	}
	return d.settle(ctx, execution, admitted, session)
}

// abandon settles a dispatch that failed before its submission was
// recorded. Shutdown and unexpected control errors leave it live.
func (d *Dispatcher) abandon(ctx context.Context, admitted model.AdmittedDispatch, err error) error {
	var settle *failure
	if ctx.Err() != nil || !errors.As(err, &settle) {
		return err
	}
	now := d.clock.Now()
	failure := model.DispatchFailure{Reason: settle.Error(), Transient: settle.transient, At: now}
	if settle.transient {
		failure.RetryAt = now.Add(transientBackoff << min(admitted.Dispatch.Attempts, maxBackoffDoublings))
	}
	if abandonErr := d.store.Abandon(ctx, admitted.Dispatch.ID, failure); abandonErr != nil {
		return errors.Join(err, abandonErr)
	}
	d.log.Info("github dispatch: abandoned before submission", "dispatch", admitted.Dispatch.ID, "transient", settle.transient, "reason", failure.Reason)
	return nil
}

// maxBackoffDoublings bounds the shift of transientBackoff; control holds
// a Ticket failed after three transient attempts anyway.
const maxBackoffDoublings = 4

// launch runs the steps before the worker's first provider request:
// worktree, prepare command, worker session with its identity document,
// binding, run budget, then the durable submission and its receipt in
// control. Each step adopts what an earlier attempt already did.
//
// Any *failure it returns abandons the dispatch and releases its
// reservation, so it may only be returned once no submission can exist. An
// admitted dispatch, and one whose Ticket has no bound session, cannot have
// submitted: control records preparing, and the binding, before the worker
// is given input. A preparing dispatch of a bound Ticket may have: a process
// can stop, or an acknowledgement be lost, between the worker accepting the
// submission and control recording it. Its receipt is looked up first, and
// only a proven absence lets launch go on.
func (d *Dispatcher) launch(ctx context.Context, execution *execution, admitted model.AdmittedDispatch) (*tempest.Session, error) {
	if admitted.Dispatch.State == model.DispatchAdmitted {
		if err := d.store.Preparing(ctx, admitted.Dispatch.ID, d.clock.Now()); err != nil {
			return nil, err
		}
	} else if admitted.Ticket.SessionID != "" {
		adopted, err := d.adoptSubmitted(ctx, admitted)
		if err != nil || adopted != nil {
			return adopted, err
		}
	}
	if execution.isStopped() {
		return nil, errStoppedBeforeStart
	}
	checkout, err := d.worktree(ctx, admitted)
	if err != nil {
		return nil, err
	}
	if err := d.point("worktree"); err != nil {
		return nil, err
	}
	if !admitted.Ticket.Prepared || checkout.Created || admitted.Ticket.Worktree != checkout.Path {
		if err := d.store.Prepared(ctx, admitted.Ticket.ID, checkout.Path, checkout.BaseSHA); err != nil {
			return nil, err
		}
	}
	session, err := d.workerSession(ctx, admitted, checkout)
	if err != nil {
		return nil, err
	}
	if execution.isStopped() {
		return nil, errStoppedBeforeStart
	}
	submission, err := d.submit(ctx, session, admitted)
	if err != nil {
		return nil, err
	}
	if err := d.point("submit"); err != nil {
		return nil, err
	}
	if err := d.recordSubmitted(ctx, admitted, submission); err != nil {
		return nil, err
	}
	return session, nil
}

// errStoppedBeforeStart abandons a stopped Ticket's dispatch once launch
// proved the dispatch holds no submission.
var errStoppedBeforeStart = permanentFailure("ticket stopped before its worker started", nil)

// adoptSubmitted looks up a preparing dispatch's launch receipt in its
// Ticket's bound worker session. An existing submission is recorded in
// control and its session returned, so settle runs, cancels or withdraws it
// and charges what it spent, unknown included. A proven absence returns no
// session. A session that cannot be opened or a receipt that cannot be read
// proves neither: the plain error leaves the dispatch live, with its
// reservation, for a later pass to look up again.
func (d *Dispatcher) adoptSubmitted(ctx context.Context, admitted model.AdmittedDispatch) (*tempest.Session, error) {
	session, err := d.host.Open(ctx, admitted.Ticket.SessionID)
	if err != nil {
		return nil, fmt.Errorf("open worker session %s: %w", admitted.Ticket.SessionID, err)
	}
	existing, err := session.Submission(ctx, admitted.Dispatch.RequestID)
	if errors.Is(err, tempest.ErrQueueNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read worker submission: %w", err)
	}
	if err := d.recordSubmitted(ctx, admitted, existing); err != nil {
		return nil, err
	}
	return session, nil
}

// recordSubmitted records the worker's receipt for the dispatch's launch.
func (d *Dispatcher) recordSubmitted(ctx context.Context, admitted model.AdmittedDispatch, submission tempest.Submission) error {
	receipt := model.SubmissionReceipt{RequestID: admitted.Dispatch.RequestID, SubmissionID: submission.ID, RunID: submission.RunID, ThroughInput: admitted.Dispatch.ThroughInput, At: d.clock.Now()}
	return d.store.Submitted(ctx, admitted.Dispatch.ID, receipt)
}

// submit returns the dispatch's existing receipt, or applies the run
// budget and submits the rendered prompt under the dispatch's request ID.
// A session that needs child recovery first takes the same prompt as a
// steer under the same request ID; settle continues it.
func (d *Dispatcher) submit(ctx context.Context, session *tempest.Session, admitted model.AdmittedDispatch) (tempest.Submission, error) {
	requestID := admitted.Dispatch.RequestID
	existing, err := session.Submission(ctx, requestID)
	if err == nil {
		return existing, nil
	}
	// Only a proven absence may lead to Submit, or to an abandon that
	// releases the reservation. An unreadable receipt leaves the dispatch
	// live for a later pass to look up again.
	if !errors.Is(err, tempest.ErrQueueNotFound) {
		return tempest.Submission{}, fmt.Errorf("read worker submission: %w", err)
	}
	prompt, err := d.render(ctx, admitted)
	if err != nil {
		return tempest.Submission{}, err
	}
	if err := session.SetBudget(runBudget(admitted.Budget)); err != nil {
		return tempest.Submission{}, permanentFailure("apply run budget", err)
	}
	input := tempest.Input{RequestID: requestID, Prompt: tempest.Prompt{Text: prompt}, Placement: tempest.PlacementInput}
	submission, err := session.Submit(ctx, input)
	if errors.Is(err, tempest.ErrRecoveryRequired) {
		input.Placement = tempest.PlacementSteer
		submission, err = session.Submit(ctx, input)
	}
	if err == nil {
		return submission, nil
	}
	if ctx.Err() != nil {
		return tempest.Submission{}, ctx.Err()
	}
	// The session commits acceptance before it starts the run and rereads
	// the receipt, so an error does not prove the input was refused: the
	// acceptance write may have landed with its acknowledgement lost. Only
	// the receipt decides. An unreadable one leaves the dispatch live, with
	// its reservation, for a later pass to look up again.
	existing, lookupErr := session.Submission(ctx, requestID)
	if lookupErr == nil {
		d.log.Warn("github dispatch: worker submission failed after it was accepted; settling the accepted submission", "dispatch", admitted.Dispatch.ID, "state", existing.State, "error", err)
		return existing, nil
	}
	if !errors.Is(lookupErr, tempest.ErrQueueNotFound) {
		return tempest.Submission{}, fmt.Errorf("worker submission: %w; receipt lookup: %w", err, lookupErr)
	}
	if errors.Is(err, tempest.ErrHostLimit) || errors.Is(err, tempest.ErrRunActive) || errors.Is(err, tempest.ErrQueueFull) {
		return tempest.Submission{}, transientFailure("worker submission", err)
	}
	return tempest.Submission{}, permanentFailure("worker submission", err)
}

// settle waits for the dispatch's runs to end and reports them. Inputs that
// raced the end of a run are continued under the same lease while the run
// completed, the Ticket is not stopped and budget is left; any still queued
// after that are withdrawn so the next dispatch renders them instead.
// Consumption and outcome come from the worker's receipts, so a restart
// re-derives exactly what an uninterrupted settle would have reported.
func (d *Dispatcher) settle(ctx context.Context, execution *execution, admitted model.AdmittedDispatch, session *tempest.Session) error {
	execution.running(session)
	requestID := admitted.Dispatch.RequestID
	launched, err := session.Submission(ctx, requestID)
	if errors.Is(err, tempest.ErrQueueNotFound) {
		return d.finish(ctx, execution, admitted, session, unknownOutcome("worker session has no receipt for "+requestID))
	}
	if err != nil {
		return err
	}
	launchedThrough, err := d.launchedThrough(ctx, session, admitted)
	if err != nil {
		return fmt.Errorf("read worker receipts: %w", err)
	}
	if launched.State == submissionAccepted {
		// Accepted but never dispatched: a restart before the run began,
		// or a recovery steer waiting for its continuation. A stopped
		// Ticket's input is withdrawn instead of started.
		if execution.isStopped() {
			if err := withdraw(ctx, session, []string{requestID}); err != nil {
				return err
			}
		} else {
			if err := session.SetBudget(runBudget(admitted.Budget)); err != nil {
				return err
			}
			if err := d.continueRun(ctx, execution, session); err != nil {
				return err
			}
		}
	}
	if err := d.point("running"); err != nil {
		return err
	}
	through := admitted.Dispatch.ThroughInput
	for {
		launched, err = session.WaitSubmission(ctx, requestID)
		if err != nil {
			return err
		}
		// A delivery may still land here, after the run's last checkpoint;
		// the receipts below find it pending and continue it.
		if err := d.point("run-ended"); err != nil {
			return err
		}
		execution.close()
		receipts, err := d.readReceipts(ctx, session, admitted, launched, launchedThrough)
		if err != nil {
			return err
		}
		if receipts.through > through {
			receipt := model.SubmissionReceipt{RequestID: requestID, SubmissionID: launched.ID, RunID: receipts.lastRunID(), ThroughInput: receipts.through, At: d.clock.Now()}
			if err := d.store.Submitted(ctx, admitted.Dispatch.ID, receipt); err != nil {
				return err
			}
			through = receipts.through
		}
		outcome := receipts.outcome()
		if len(receipts.pending) == 0 {
			return d.finish(ctx, execution, admitted, session, outcome)
		}
		budget, budgetLeft := remainingBudget(admitted.Budget, receipts)
		if outcome.StopReason != stopCompleted || execution.isStopped() || !budgetLeft {
			if err := withdraw(ctx, session, receipts.pending); err != nil {
				return err
			}
			return d.finish(ctx, execution, admitted, session, outcome)
		}
		if err := session.SetBudget(budget); err != nil {
			return err
		}
		execution.running(session)
		if err := d.continueRun(ctx, execution, session); err != nil {
			return err
		}
	}
}

// continueRun starts a continuation of the worker's accepted input and
// waits for it to end.
func (d *Dispatcher) continueRun(ctx context.Context, execution *execution, session *tempest.Session) error {
	run, err := session.BeginContinue(ctx)
	if err != nil {
		return fmt.Errorf("continue worker session: %w", err)
	}
	if execution.isStopped() {
		run.Cancel()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = run.Wait() // the run's receipts carry its result
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// finish cancels the background tasks and jobs the worker still owns, so
// nothing it started runs outside the lease, closes the idle session, then
// reports the outcome to control. The session is closed while the lease is
// still held, so no next dispatch of the Ticket can have opened it; that
// one reopens it by ID.
func (d *Dispatcher) finish(ctx context.Context, execution *execution, admitted model.AdmittedDispatch, session *tempest.Session, outcome model.RunOutcome) error {
	execution.close()
	// CancelAll also aborts session-background task roots, which Cancel
	// and run ends leave in place and which a task drain or an alarm could
	// otherwise run with no lease.
	if err := session.CancelAll(ctx); err != nil {
		return fmt.Errorf("cancel worker tasks: %w", err)
	}
	if err := cancelJobs(ctx, session); err != nil {
		return err
	}
	if err := d.host.CloseSession(ctx, session.ID()); err != nil {
		// The Host keeps closing it in the background; an open session
		// only costs memory, so settlement goes on.
		d.log.Warn("github dispatch: close finished worker session", "session", session.ID(), "error", err)
	}
	if err := d.store.Finished(ctx, admitted.Dispatch.ID, outcome, d.clock.Now()); err != nil {
		return err
	}
	d.log.Info("github dispatch: finished", "dispatch", admitted.Dispatch.ID, "stopReason", outcome.StopReason, "microUsd", outcome.Usage.MicroUSD, "usageKnown", outcome.UsageKnown)
	return nil
}

// jobPageLimit is the largest page Session.Jobs serves.
const jobPageLimit = 50

// cancelJobs cancels every job of the worker session that has not ended.
func cancelJobs(ctx context.Context, session *tempest.Session) error {
	cursor := ""
	for {
		page, err := session.Jobs(ctx, cursor, jobPageLimit)
		if err != nil {
			return fmt.Errorf("list worker jobs: %w", err)
		}
		for _, job := range page.Jobs {
			if !jobLive(job.State) {
				continue
			}
			if _, err := session.CancelJob(ctx, job.ID); err != nil && !errors.Is(err, tempest.ErrJobNotFound) {
				return fmt.Errorf("cancel worker job %s: %w", job.ID, err)
			}
		}
		if page.NextCursor == "" {
			return nil
		}
		cursor = page.NextCursor
	}
}

func jobLive(state string) bool {
	switch state {
	case "starting", "running", "recovering":
		return true
	}
	return false
}

// withdraw removes queued deliveries no run consumed. A delivery that a run
// claimed in the meantime is left to its receipt.
func withdraw(ctx context.Context, session *tempest.Session, keys []string) error {
	for _, key := range keys {
		if err := session.Withdraw(ctx, key); err != nil && !errors.Is(err, tempest.ErrQueueTooLate) {
			return fmt.Errorf("withdraw delivery %s: %w", key, err)
		}
	}
	return nil
}

func runBudget(budget model.RunBudget) tempest.Budget {
	return tempest.Budget{
		MaxTurns:       budget.MaxTurns,
		MaxCostUSD:     budget.MaxCostUSD,
		MaxWallTime:    time.Duration(budget.MaxWallTime),
		MaxTotalTokens: budget.MaxTotalTokens,
		MaxToolCalls:   budget.MaxToolCalls,
	}
}

// isConflict reports a durable revision or identity conflict.
func isConflict(err error) bool { return errors.Is(err, durable.ErrConflict) }
