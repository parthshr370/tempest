package control

import (
	"testing"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
)

func TestReservationTable(t *testing.T) {
	ledger := func(allowance, spent int64, reservations ...int64) model.Ledger {
		l := model.Ledger{Allowance: model.Spend{MicroUSD: allowance}, Spent: model.Spend{MicroUSD: spent}}
		for i, r := range reservations {
			reserve(&l, model.DispatchID(rune('a'+i)), model.Spend{MicroUSD: r})
		}
		return l
	}
	unknown := ledger(10_000_000, 1_000_000)
	unknown.Spent.CostUnknown = true
	granted := ledger(10_000_000, 10_000_000)
	granted.Granted = model.Spend{MicroUSD: 1_000_000}
	tokenLedger := ledger(10_000_000, 0)
	tokenLedger.Allowance.TotalTokens = 50_000
	tokenLedger.Spent.TotalTokens = 20_000
	cases := []struct {
		name    string
		ticket  model.Ledger
		repo    model.Ledger
		tokens  int64
		want    model.Spend
		reserve bool
	}{
		{name: "run cap", ticket: ledger(10_000_000, 0), repo: ledger(100_000_000, 0), want: model.Spend{MicroUSD: 2_000_000}, reserve: true},
		{name: "ticket left", ticket: ledger(10_000_000, 9_500_000), repo: ledger(100_000_000, 0), want: model.Spend{MicroUSD: 500_000}, reserve: true},
		{name: "repo left with reservations", ticket: ledger(10_000_000, 0), repo: ledger(100_000_000, 98_000_000, 1_200_000), want: model.Spend{MicroUSD: 800_000}, reserve: true},
		{name: "below minimum run", ticket: ledger(10_000_000, 9_900_000), repo: ledger(100_000_000, 0)},
		{name: "overrun clamps to zero", ticket: ledger(10_000_000, 12_000_000), repo: ledger(100_000_000, 0)},
		{name: "unknown cost blocks", ticket: unknown, repo: ledger(100_000_000, 0)},
		{name: "grant extends", ticket: granted, repo: ledger(100_000_000, 0), want: model.Spend{MicroUSD: 1_000_000}, reserve: true},
		{name: "token ledger caps tokens", ticket: tokenLedger, repo: ledger(100_000_000, 0), tokens: 40_000, want: model.Spend{MicroUSD: 2_000_000, TotalTokens: 30_000}, reserve: true},
		{name: "run tokens when ledgers uncapped", ticket: ledger(10_000_000, 0), repo: ledger(100_000_000, 0), tokens: 40_000, want: model.Spend{MicroUSD: 2_000_000, TotalTokens: 40_000}, reserve: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason, ok := reservation(2_000_000, tc.tokens, 200_000, tc.ticket, tc.repo)
			if ok != tc.reserve || got != tc.want {
				t.Fatalf("got %+v %v (%s), want %+v %v", got, ok, reason, tc.want, tc.reserve)
			}
			if !ok && reason == "" {
				t.Fatal("refusal without a reason")
			}
		})
	}
}

func TestPeriodStartIsEpochAligned(t *testing.T) {
	period := 720 * time.Hour
	start := periodStart(epoch, period)
	if start.Unix()%int64(period/time.Second) != 0 || start.After(epoch) || !start.Add(period).After(epoch) {
		t.Fatalf("period start %v for %v", start, epoch)
	}
	if periodStart(start.Add(period-time.Second), period) != start || periodStart(start.Add(period), period) != start.Add(period) {
		t.Fatal("period boundaries")
	}
}

func TestLedgersPersistAndNeverReset(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	id, dispatch := h.launched(300, 1)
	repoKey := model.RepoLedger(periodStart(epoch, 720*time.Hour))
	// Admission reserved the run cap in both ledgers; the admitted budget
	// equals the reservation.
	ticketLedger := h.ledger(model.TicketLedger(id))
	if ticketLedger.Reservations[dispatch].MicroUSD != 2_000_000 || h.ledger(repoKey).Reservations[dispatch].MicroUSD != 2_000_000 {
		t.Fatalf("reservations: %+v", ticketLedger)
	}
	active, _ := h.store.Active(h.ctx)
	if active[0].Budget.MaxCostUSD != 2 || active[0].Budget.MaxTurns != 60 {
		t.Fatalf("budget: %+v", active[0].Budget)
	}
	// Restart mid-run: the reservation survives.
	h.reopen()
	if ledger := h.ledger(model.TicketLedger(id)); ledger.Reservations[dispatch].MicroUSD != 2_000_000 {
		t.Fatalf("reservation lost on restart: %+v", ledger)
	}
	// Unknown usage (the run was interrupted) charges the whole reservation.
	h.finish(dispatch, model.RunOutcome{StopReason: "unknown"})
	ticketLedger = h.ledger(model.TicketLedger(id))
	if ticketLedger.Spent.MicroUSD != 2_000_000 || !ticketLedger.Estimated || len(ticketLedger.Reservations) != 0 {
		t.Fatalf("unknown usage: %+v", ticketLedger)
	}
	if h.dispatch(dispatch).Settled.MicroUSD != 2_000_000 {
		t.Fatalf("settled: %+v", h.dispatch(dispatch).Settled)
	}
	// Recovery generation: spend carries over and the next reservation is
	// min(run cap, ticket left).
	admitted := h.runAll()
	h.finish(admitted[0].Dispatch.ID, model.RunOutcome{StopReason: "completed", Usage: model.Spend{MicroUSD: 2_300_000, TotalTokens: 1000}, UsageKnown: true})
	h.reopen()
	ticketLedger = h.ledger(model.TicketLedger(id))
	if ticketLedger.Spent.MicroUSD != 4_300_000 || ticketLedger.Spent.TotalTokens != 1000 {
		t.Fatalf("overrun not recorded: %+v", ticketLedger)
	}
	if repo := h.ledger(repoKey); repo.Spent.MicroUSD != 4_300_000 {
		t.Fatalf("repo ledger: %+v", repo)
	}
	// Retry does not reset either.
	if err := h.store.Operator().Retry(h.ctx, model.OperatorRequest{ID: "retry", Target: string(id)}); err != nil {
		t.Fatal(err)
	}
	admitted = h.runAll()
	h.finish(admitted[0].Dispatch.ID, completed(5_600_000))
	// 9.9 of 10 spent: the next wake cannot reserve the 0.2 minimum.
	h.ingest("thread", comment(300, false, 2, parth, "more", h.clock.Now()))
	if admitted := h.admit(); len(admitted) != 0 {
		t.Fatalf("admitted beyond budget: %+v", admitted)
	}
	ticket := h.ticket(id)
	if ticket.Hold != model.HoldBudget {
		t.Fatalf("hold %q", ticket.Hold)
	}
	if stage := h.detail(id).Stage; stage != model.StageHeld {
		t.Fatalf("stage %s", stage)
	}
	// A grant re-enables admission at the next AdmitNext.
	if err := h.store.Operator().Grant(h.ctx, model.OperatorRequest{ID: "grant", Target: string(id)}, model.Spend{MicroUSD: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	admitted = h.admit()
	if len(admitted) != 1 || admitted[0].Dispatch.Reservation.MicroUSD != 1_100_000 || admitted[0].Budget.MaxCostUSD != 1.1 {
		t.Fatalf("after grant: %+v", admitted)
	}
	if ticket := h.ticket(id); ticket.Hold != model.HoldNone {
		t.Fatalf("hold after grant %q", ticket.Hold)
	}
}

func TestRepoLedgerNewPeriodReleasesBudgetHold(t *testing.T) {
	config := testConfig()
	config.Budgets.RepoUSD = 3
	config.Budgets.RepoPeriod = model.Duration(24 * time.Hour)
	h := newHarnessWith(t, config)
	h.at(0)
	first, dispatch := h.launched(310, 1)
	h.finish(dispatch, completed(2_900_000))
	second := h.ingest("thread", mention(311, false, 2, parth, "fix", h.clock.Now())).Decisions[0].Tickets[0]
	if admitted := h.admit(); len(admitted) != 0 {
		t.Fatalf("admitted beyond repo budget: %+v", admitted)
	}
	if ticket := h.ticket(second); ticket.Hold != model.HoldBudget {
		t.Fatalf("hold %q", ticket.Hold)
	}
	// The next period has a fresh repository ledger.
	h.at(24 * time.Hour)
	admitted := h.admit()
	if len(admitted) != 1 || admitted[0].Dispatch.Ticket != second || admitted[0].Dispatch.Reservation.MicroUSD != 2_000_000 {
		t.Fatalf("new period: %+v", admitted)
	}
	// The first Ticket's spend stays in its lifetime ledger.
	if ledger := h.ledger(model.TicketLedger(first)); ledger.Spent.MicroUSD != 2_900_000 {
		t.Fatalf("ticket ledger %+v", ledger)
	}
	// A dispatch admitted in one period settles in that period's ledger.
	h.start(admitted[0])
	h.at(48 * time.Hour)
	h.finish(admitted[0].Dispatch.ID, completed(700_000))
	if ledger := h.ledger(model.RepoLedger(periodStart(epoch.Add(24*time.Hour), 24*time.Hour))); ledger.Spent.MicroUSD != 700_000 || len(ledger.Reservations) != 0 {
		t.Fatalf("period ledger %+v", ledger)
	}
}

func TestUnknownPriceBlocksFurtherReservations(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	id, dispatch := h.launched(320, 1)
	h.finish(dispatch, model.RunOutcome{StopReason: "completed", Usage: model.Spend{MicroUSD: 0, CostUnknown: true}, UsageKnown: true})
	h.ingest("thread", comment(320, false, 2, parth, "next", h.clock.Now()))
	if admitted := h.admit(); len(admitted) != 0 {
		t.Fatalf("admitted with unknown cost: %+v", admitted)
	}
	if ticket := h.ticket(id); ticket.Hold != model.HoldBudget {
		t.Fatalf("hold %q", ticket.Hold)
	}
}
