package dispatch

import (
	"context"
	"errors"
	"math"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
	"go.harness.dev/harness/tempest"
	"go.harness.dev/harness/tempest/durable"
)

// Worker submission states (tempest.Submission.State).
const (
	submissionAccepted = "accepted"
	submissionRunning  = "running"
	submissionConsumed = "consumed"
	submissionDone     = "completed"
	submissionFailed   = "failed"
	submissionUnknown  = "unknown"
)

// Run stop reasons the dispatcher decides on. stopUnknown is the RunOutcome
// stop reason control settles as an interrupted run: whole reservation
// charged, recovery generation queued.
const (
	stopCompleted = "completed"
	stopUnknown   = "unknown"
)

// runReceipt is one worker run as its submission receipts record it.
type runReceipt struct {
	id         string
	stopReason string
	err        string
	turns      int
	elapsed    time.Duration
	usage      tempest.Usage
}

// dispatchReceipts is what the worker session proves about one dispatch:
// the runs it made and the delivered Inputs they consumed.
type dispatchReceipts struct {
	// through is the highest Input seq consumed without a gap.
	through uint64
	// pending are delivery keys still queued, not consumed by any run.
	pending []string
	runs    []runReceipt
	// unknown names an interrupted run, whose result cannot be proven.
	unknown string
}

func (r *dispatchReceipts) lastRunID() string {
	if len(r.runs) == 0 {
		return ""
	}
	return r.runs[len(r.runs)-1].id
}

// addRun records the run a terminal receipt belongs to, once per run.
func (r *dispatchReceipts) addRun(submission tempest.Submission) {
	if submission.State == submissionUnknown {
		r.unknown = submission.Reason
	}
	id := submission.RunID
	if id == "" {
		id = submission.Result.RunID
	}
	for _, run := range r.runs {
		if run.id == id {
			return
		}
	}
	run := runReceipt{id: id, stopReason: string(submission.Result.StopReason), err: submission.Error, turns: submission.Result.Turns, elapsed: time.Duration(submission.Result.ElapsedMS) * time.Millisecond, usage: submission.Result.Usage}
	r.runs = append(r.runs, run)
}

// submissionNamespace is where a tempest session journals its submission
// receipts, one record per request ID, revised as the submission advances.
const submissionNamespace = "_tempest/submissions"

// acceptedAt is the journal position where the session first accepted
// requestID: the seq of the receipt's first revision.
func acceptedAt(ctx context.Context, session *tempest.Session, requestID string) (uint64, error) {
	page, err := session.Read(ctx, durable.Query{Kind: durable.SubmissionKind, Namespace: submissionNamespace, ID: requestID, History: true, Limit: 1})
	if err != nil {
		return 0, err
	}
	if len(page.Records) == 0 {
		return 0, tempest.ErrQueueNotFound
	}
	return page.Records[0].Seq, nil
}

// launchedThrough is the last Input the dispatch's launch prompt carried;
// the dispatch's deliveries are the Inputs after it. Control's ThroughInput
// is a consumption cursor that continuation receipts advance, so it cannot
// say where the dispatch's runs began. Every delivery at or below the
// cursor that the session accepted after the launch belongs to this
// dispatch, and they are contiguous: walking down from the cursor, the
// first key the session never saw, or saw before the launch (a delivery an
// earlier dispatch of the generation withdrew), ends them.
func (d *Dispatcher) launchedThrough(ctx context.Context, session *tempest.Session, admitted model.AdmittedDispatch) (uint64, error) {
	launchAt, err := acceptedAt(ctx, session, admitted.Dispatch.RequestID)
	if err != nil {
		return 0, err
	}
	through := admitted.Dispatch.ThroughInput
	for through > 0 {
		at, err := acceptedAt(ctx, session, model.DeliveryRequestID(admitted.Ticket.ID, through, admitted.Dispatch.Gen))
		if errors.Is(err, tempest.ErrQueueNotFound) {
			break
		}
		if err != nil {
			return 0, err
		}
		if at < launchAt {
			break
		}
		through--
	}
	return through, nil
}

// readReceipts reads every run of the dispatch: the launch receipt and the
// receipts of all its deliveries, the Inputs after launchedThrough. Each
// call rereads the whole set, so the runs, their usage and the consumption
// cursor are derived the same way on every pass and after a restart.
// Delivery keys are written in seq order, so the scan stops at the first key
// the session never saw. A key a vanished run had claimed is settled as
// unknown here, before any later run could adopt it.
func (d *Dispatcher) readReceipts(ctx context.Context, session *tempest.Session, admitted model.AdmittedDispatch, launched tempest.Submission, launchedThrough uint64) (dispatchReceipts, error) {
	receipts := dispatchReceipts{through: launchedThrough}
	receipts.addRun(launched)
	contiguous := true
	for seq := launchedThrough + 1; ; seq++ {
		key := model.DeliveryRequestID(admitted.Ticket.ID, seq, admitted.Dispatch.Gen)
		delivery, err := session.Submission(ctx, key)
		if errors.Is(err, tempest.ErrQueueNotFound) {
			return receipts, nil
		}
		if err != nil {
			return receipts, err
		}
		if delivery.State == submissionRunning || delivery.State == submissionConsumed {
			if delivery, err = session.WaitSubmission(ctx, key); err != nil {
				return receipts, err
			}
		}
		switch delivery.State {
		case submissionAccepted:
			receipts.pending = append(receipts.pending, key)
			contiguous = false
		case submissionDone, submissionFailed, submissionUnknown:
			if contiguous {
				receipts.through = seq
			}
			receipts.addRun(delivery)
		default:
			// Withdrawn or refused by a prompt hook: never shown to the
			// model, so it stays unhandled for the next dispatch.
			contiguous = false
		}
	}
}

// outcome sums every run of the dispatch and reports the last run's stop.
// Any interrupted run makes the whole outcome unknown.
func (r *dispatchReceipts) outcome() model.RunOutcome {
	outcome := model.RunOutcome{UsageKnown: r.unknown == ""}
	for _, run := range r.runs {
		outcome.Usage = addSpend(outcome.Usage, spendOf(run.usage))
	}
	if len(r.runs) > 0 {
		last := r.runs[len(r.runs)-1]
		outcome.StopReason, outcome.Error = last.stopReason, last.err
	}
	if r.unknown != "" {
		outcome.StopReason, outcome.Error = stopUnknown, r.unknown
	}
	return outcome
}

func unknownOutcome(reason string) model.RunOutcome {
	return model.RunOutcome{StopReason: stopUnknown, Error: reason}
}

// remainingBudget lowers the dispatch's run budget by what its runs used,
// for a continuation under the same reservation. It reports false when a
// capped dimension has nothing left.
func remainingBudget(reserved model.RunBudget, receipts dispatchReceipts) (tempest.Budget, bool) {
	var used tempest.Usage
	var turns int
	var elapsed time.Duration
	for _, run := range receipts.runs {
		used.Add(run.usage)
		turns += run.turns
		elapsed += run.elapsed
	}
	budget := runBudget(reserved)
	hasRoom := true
	if budget.MaxCostUSD > 0 {
		budget.MaxCostUSD -= used.Cost.Total
		hasRoom = hasRoom && budget.MaxCostUSD > 0
	}
	if budget.MaxWallTime > 0 {
		budget.MaxWallTime -= elapsed
		hasRoom = hasRoom && budget.MaxWallTime > 0
	}
	hasRoom = lowerCap(&budget.MaxTurns, turns) && hasRoom
	hasRoom = lowerCap(&budget.MaxTotalTokens, used.TotalTokens) && hasRoom
	hasRoom = lowerCap(&budget.MaxToolCalls, used.ToolCalls) && hasRoom
	return budget, hasRoom
}

// lowerCap subtracts used from a set cap (zero means uncapped) and reports
// whether any of it is left.
func lowerCap(limit *int, used int) bool {
	if *limit == 0 {
		return true
	}
	*limit -= used
	return *limit > 0
}

// microPerUSD converts provider dollars to ledger micro-USD.
const microPerUSD = 1e6

// spendOf converts a run's usage to ledger units. Cost rounds up so a run
// never looks cheaper than it was.
func spendOf(usage tempest.Usage) model.Spend {
	return model.Spend{
		MicroUSD:     microUSD(usage.Cost.Total),
		InputTokens:  int64(usage.Input) + int64(usage.CacheRead) + int64(usage.CacheWrite),
		OutputTokens: int64(usage.Output),
		TotalTokens:  int64(usage.TotalTokens),
		ToolCalls:    int64(usage.ToolCalls),
		CostUnknown:  usage.Cost.Unknown,
	}
}

func microUSD(usd float64) int64 {
	micro := math.Ceil(usd * microPerUSD)
	switch {
	case math.IsNaN(micro) || micro <= 0:
		return 0
	case micro >= math.MaxInt64:
		return math.MaxInt64
	}
	return int64(micro)
}

func addSpend(a, b model.Spend) model.Spend {
	return model.Spend{
		MicroUSD:     saturatingAdd(a.MicroUSD, b.MicroUSD),
		InputTokens:  saturatingAdd(a.InputTokens, b.InputTokens),
		OutputTokens: saturatingAdd(a.OutputTokens, b.OutputTokens),
		TotalTokens:  saturatingAdd(a.TotalTokens, b.TotalTokens),
		ToolCalls:    saturatingAdd(a.ToolCalls, b.ToolCalls),
		CostUnknown:  a.CostUnknown || b.CostUnknown,
	}
}

func saturatingAdd(a, b int64) int64 {
	a, b = max(a, 0), max(b, 0)
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}
