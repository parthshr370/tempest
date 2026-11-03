package control

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
)

func TestScheduleTable(t *testing.T) {
	human := func(id string, session bool, queued int) candidate {
		return candidate{Dispatch: model.DispatchID(id), Class: model.SlotHuman, HasSession: session, QueuedAt: epoch.Add(time.Duration(queued) * time.Minute)}
	}
	normal := func(id string, session bool, queued int) candidate {
		return candidate{Dispatch: model.DispatchID(id), Class: model.SlotNormal, HasSession: session, QueuedAt: epoch.Add(time.Duration(queued) * time.Minute)}
	}
	cases := []struct {
		name       string
		candidates []candidate
		normalUsed int
		humanUsed  int
		refuse     string
		want       string
	}{
		{name: "fifo within normal", candidates: []candidate{normal("b", false, 2), normal("a", false, 1), normal("c", false, 3)}, want: "a:normal,b:normal"},
		{name: "human first", candidates: []candidate{normal("a", true, 1), human("b", true, 5)}, want: "b:normal,a:normal"},
		{name: "id breaks ties", candidates: []candidate{normal("b", false, 1), normal("a", false, 1), normal("c", false, 1)}, want: "a:normal,b:normal"},
		{name: "human continuation takes the extra slot", candidates: []candidate{normal("a", true, 1), human("h", true, 2)}, normalUsed: 2, want: "h:human"},
		{name: "human launch never takes the extra slot", candidates: []candidate{human("h", false, 1)}, normalUsed: 2, want: ""},
		{name: "normal never takes the extra slot", candidates: []candidate{normal("a", true, 1)}, normalUsed: 2, want: ""},
		{name: "one extra slot only", candidates: []candidate{human("h1", true, 1), human("h2", true, 2)}, normalUsed: 2, want: "h1:human"},
		{name: "extra slot occupied", candidates: []candidate{human("h1", true, 1)}, normalUsed: 2, humanUsed: 1, want: ""},
		{name: "human uses normal slot first", candidates: []candidate{human("h1", true, 1), human("h2", true, 2)}, normalUsed: 1, want: "h1:normal,h2:human"},
		{name: "budget refusal frees the slot", candidates: []candidate{normal("a", false, 1), normal("b", false, 2), normal("c", false, 3)}, refuse: "a", want: "b:normal,c:normal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			occupancy := model.Occupancy{Normal: tc.normalUsed, Human: tc.humanUsed, MaxNormal: 2, MaxHuman: humanSlots}
			placements := schedule(tc.candidates, occupancy, func(c candidate) bool { return string(c.Dispatch) != tc.refuse })
			var got []string
			for _, p := range placements {
				got = append(got, fmt.Sprintf("%s:%s", p.Dispatch, p.Slot))
			}
			if strings.Join(got, ",") != tc.want {
				t.Fatalf("got %q, want %q", strings.Join(got, ","), tc.want)
			}
		})
	}
}

func TestNPlusOneHumanPriority(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	// Two normal slots fill with two launches.
	first, _ := h.launched(200, 1)
	second, _ := h.launched(201, 2)
	if occupancy := h.store.occupancy(); occupancy.Normal != 2 || occupancy.Human != 0 {
		t.Fatalf("occupancy %+v", occupancy)
	}
	// A third requested Ticket waits: a launch never uses the human slot.
	third := h.ingest("thread", mention(202, false, 3, parth, "fix", h.clock.Now())).Decisions[0].Tickets[0]
	if admitted := h.admit(); len(admitted) != 0 {
		t.Fatalf("launch took the extra slot: %+v", admitted)
	}
	// A human follow-up on a quiescent Ticket with a session is a
	// continuation and gets the extra slot ahead of the queued launch.
	h.finish(model.NewDispatchID(first, 1), completed(1))
	h.ingest("thread", comment(201, false, 4, alice, "while you are at it", h.clock.Now()))
	h.at(time.Minute)
	h.ingest("thread", comment(200, false, 5, alice, "status?", h.clock.Now()))
	admitted := h.admit()
	if len(admitted) != 1 || admitted[0].Dispatch.Ticket != first || admitted[0].Dispatch.Class != model.SlotHuman || admitted[0].Dispatch.Slot != model.SlotNormal {
		t.Fatalf("human continuation should take the freed normal slot first: %+v", admitted)
	}
	// Second's follow-up arrived while it ran. When it finishes, the
	// undelivered Input queues a human wake that takes the free normal slot
	// ahead of the older queued launch.
	h.finish(model.NewDispatchID(second, 1), completed(1))
	h.start(admitted[0])
	admitted = h.admit()
	if len(admitted) != 1 || admitted[0].Dispatch.Ticket != second || admitted[0].Dispatch.Slot != model.SlotNormal {
		t.Fatalf("second wake: %+v", admitted)
	}
	h.start(admitted[0])
	// Both normal slots are busy; finishing first frees one.
	occupancy := h.store.occupancy()
	if occupancy.Normal != 2 || occupancy.Human != 0 {
		t.Fatalf("occupancy %+v", occupancy)
	}
	h.finish(model.NewDispatchID(first, 2), completed(1))
	// The freed normal slot goes to the oldest queued normal: third's launch.
	admitted = h.admit()
	if len(admitted) != 1 || admitted[0].Dispatch.Ticket != third {
		t.Fatalf("third launch: %+v", admitted)
	}
	h.start(admitted[0])
	// Now N normal busy; a human follow-up on first uses the +1 slot.
	h.ingest("thread", comment(200, false, 6, parth, "one more", h.clock.Now()))
	admitted = h.admit()
	if len(admitted) != 1 || admitted[0].Dispatch.Ticket != first || admitted[0].Dispatch.Slot != model.SlotHuman {
		t.Fatalf("extra slot: %+v", admitted)
	}
	// Restart: occupancy is rebuilt from the records.
	h.reopen()
	if occupancy := h.store.occupancy(); occupancy.Normal != 2 || occupancy.Human != 1 {
		t.Fatalf("occupancy after restart %+v", occupancy)
	}
	active, err := h.store.Active(h.ctx)
	if err != nil || len(active) != 3 {
		t.Fatalf("active after restart: %d %v", len(active), err)
	}
	// A bot comment on the running Ticket is delivered into its run, not
	// dispatched.
	h.start(admitted[0])
	h.ingest("thread", comment(200, false, 7, rabbit, "nit", h.clock.Now()))
	if admitted := h.admit(); len(admitted) != 0 {
		t.Fatalf("over capacity: %+v", admitted)
	}
	deliveries, err := h.store.Deliveries(h.ctx)
	if err != nil || len(deliveries) != 1 || deliveries[0].Ticket != first || len(deliveries[0].Inputs) != 1 {
		t.Fatalf("deliveries: %+v %v", deliveries, err)
	}
	through := deliveries[0].Inputs[0].Input.Seq
	if err := h.store.Delivered(h.ctx, first, deliveries[0].Gen, through); err != nil {
		t.Fatal(err)
	}
	if deliveries, _ := h.store.Deliveries(h.ctx); len(deliveries) != 0 {
		t.Fatalf("delivered input listed again: %+v", deliveries)
	}
}
