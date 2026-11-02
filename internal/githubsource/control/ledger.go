package control

import (
	"fmt"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
)

// addSpend sums two amounts; unknown cost is sticky.
func addSpend(a, b model.Spend) model.Spend {
	return model.Spend{
		MicroUSD:     a.MicroUSD + b.MicroUSD,
		InputTokens:  a.InputTokens + b.InputTokens,
		OutputTokens: a.OutputTokens + b.OutputTokens,
		TotalTokens:  a.TotalTokens + b.TotalTokens,
		ToolCalls:    a.ToolCalls + b.ToolCalls,
		CostUnknown:  a.CostUnknown || b.CostUnknown,
	}
}

// reserved sums a ledger's open reservations.
func reserved(ledger model.Ledger) model.Spend {
	var total model.Spend
	for _, spend := range ledger.Reservations {
		total = addSpend(total, spend)
	}
	return total
}

// leftMicro is what a ledger can still reserve in micro-USD, never negative:
// a settled overrun can push Spent past the allowance.
func leftMicro(ledger model.Ledger) int64 {
	left := ledger.Allowance.MicroUSD + ledger.Granted.MicroUSD - ledger.Spent.MicroUSD - reserved(ledger).MicroUSD
	return max(left, 0)
}

// leftTokens is the token room of a ledger; ok is false when the ledger
// has no token allowance, which means tokens are not capped by it.
func leftTokens(ledger model.Ledger) (int64, bool) {
	allowance := ledger.Allowance.TotalTokens + ledger.Granted.TotalTokens
	if ledger.Allowance.TotalTokens == 0 {
		return 0, false
	}
	return max(allowance-ledger.Spent.TotalTokens-reserved(ledger).TotalTokens, 0), true
}

// reservation computes what one dispatch reserves: the run cap lowered to
// what both ledgers have left. ok is false with a reason when the result is
// below the minimum run or a ledger carries spend of unknown cost.
func reservation(runMicro, runTokens, minMicro int64, ticket, repo model.Ledger) (model.Spend, string, bool) {
	if ticket.Spent.CostUnknown {
		return model.Spend{}, "ticket ledger has spend of unknown cost", false
	}
	if repo.Spent.CostUnknown {
		return model.Spend{}, "repository ledger has spend of unknown cost", false
	}
	amount := min(runMicro, leftMicro(ticket), leftMicro(repo))
	if amount < minMicro {
		return model.Spend{}, fmt.Sprintf("can reserve %s, below the minimum run %s (ticket left %s, repository left %s)", usd(amount), usd(minMicro), usd(leftMicro(ticket)), usd(leftMicro(repo))), false
	}
	tokens, capped := runTokens, runTokens > 0
	for _, ledger := range []model.Ledger{ticket, repo} {
		if left, ok := leftTokens(ledger); ok {
			if !capped || left < tokens {
				tokens = left
			}
			capped = true
		}
	}
	if capped && tokens <= 0 {
		return model.Spend{}, "no token allowance left", false
	}
	return model.Spend{MicroUSD: amount, TotalTokens: tokens}, "", true
}

// reserve records a dispatch's reservation on a ledger.
func reserve(ledger *model.Ledger, id model.DispatchID, spend model.Spend) {
	if ledger.Reservations == nil {
		ledger.Reservations = make(map[model.DispatchID]model.Spend)
	}
	ledger.Reservations[id] = spend
}

// release drops a reservation without charging it.
func release(ledger *model.Ledger, id model.DispatchID) {
	delete(ledger.Reservations, id)
	if len(ledger.Reservations) == 0 {
		ledger.Reservations = nil
	}
}

// charge is what a finished dispatch costs: its reported usage, or the
// whole reservation when the usage is unknown.
func charge(outcome model.RunOutcome, reservation model.Spend) (model.Spend, bool) {
	if !outcome.UsageKnown {
		spend := reservation
		spend.CostUnknown = outcome.Usage.CostUnknown
		return spend, true
	}
	return outcome.Usage, false
}

// settle replaces a reservation by the charge.
func settle(ledger *model.Ledger, id model.DispatchID, spend model.Spend, estimated bool) {
	release(ledger, id)
	ledger.Spent = addSpend(ledger.Spent, spend)
	ledger.Estimated = ledger.Estimated || estimated
}

// periodStart floors now to the repository budget period, counted from the
// Unix epoch so every process derives the same key.
func periodStart(now time.Time, period time.Duration) time.Time {
	seconds := int64(period / time.Second)
	unix := now.Unix()
	start := unix - ((unix%seconds)+seconds)%seconds
	return time.Unix(start, 0).UTC()
}

func newTicketLedger(id model.TicketID, s settings) model.Ledger {
	return model.Ledger{Key: model.TicketLedger(id), Scope: model.LedgerTicket, Allowance: model.Spend{MicroUSD: s.ticketMicro, TotalTokens: s.ticketTokens}}
}

func newRepoLedger(start time.Time, s settings) model.Ledger {
	return model.Ledger{Key: model.RepoLedger(start), Scope: model.LedgerRepo, PeriodStart: start, PeriodEnd: start.Add(s.period), Allowance: model.Spend{MicroUSD: s.repoMicro, TotalTokens: s.repoTokens}}
}

func usd(microUSD int64) string {
	sign := ""
	if microUSD < 0 {
		sign, microUSD = "-", -microUSD
	}
	return fmt.Sprintf("%s$%d.%02d", sign, microUSD/1e6, (microUSD%1e6)/1e4)
}

// ticketLedger returns the Ticket's ledger, staging a new one when absent.
func (s *Store) ticketLedger(id model.TicketID) *model.Ledger {
	key := string(model.TicketLedger(id))
	if ledger := s.ledgers.edit(key); ledger != nil {
		return ledger
	}
	return s.ledgers.put(key, newTicketLedger(id, s.settings))
}

// repoLedger returns the repository ledger of the period containing now,
// staging a new one when absent.
func (s *Store) repoLedger(now time.Time) *model.Ledger {
	start := periodStart(now, s.settings.period)
	key := string(model.RepoLedger(start))
	if ledger := s.ledgers.edit(key); ledger != nil {
		return ledger
	}
	return s.ledgers.put(key, newRepoLedger(start, s.settings))
}

// peekLedgers returns both ledgers for a reservation check without staging
// anything.
func (s *Store) peekLedgers(id model.TicketID, now time.Time) (model.Ledger, model.Ledger) {
	ticket := newTicketLedger(id, s.settings)
	if ledger, ok := s.ledgers.get(string(model.TicketLedger(id))); ok {
		ticket = *ledger
	}
	start := periodStart(now, s.settings.period)
	repo := newRepoLedger(start, s.settings)
	if ledger, ok := s.ledgers.get(string(model.RepoLedger(start))); ok {
		repo = *ledger
	}
	return ticket, repo
}

// reservingLedger finds the repository ledger holding a dispatch's
// reservation; a dispatch admitted in one period settles in that period.
func (s *Store) reservingLedger(id model.DispatchID) *model.Ledger {
	for _, key := range s.ledgers.ids() {
		ledger, _ := s.ledgers.get(key)
		if ledger.Scope != model.LedgerRepo {
			continue
		}
		if _, ok := ledger.Reservations[id]; ok {
			return s.ledgers.edit(key)
		}
	}
	return nil
}

// settleDispatch settles or releases a dispatch's reservation on both
// ledgers. A nil outcome releases without charge (the dispatch never ran).
func (s *Store) settleDispatch(d *model.DispatchIntent, outcome *model.RunOutcome) {
	ticket := s.ticketLedger(d.Ticket)
	repo := s.reservingLedger(d.ID)
	if outcome == nil {
		release(ticket, d.ID)
		if repo != nil {
			release(repo, d.ID)
		}
		return
	}
	spend, estimated := charge(*outcome, d.Reservation)
	settle(ticket, d.ID, spend, estimated)
	if repo != nil {
		settle(repo, d.ID, spend, estimated)
	} else {
		// The reservation's period ledger is gone only if it was never
		// written; charge the current period so spend is never lost.
		settle(s.repoLedger(s.clock.Now()), d.ID, spend, estimated)
	}
	d.Settled = &spend
}
