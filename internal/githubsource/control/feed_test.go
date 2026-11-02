package control

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"go.harness.dev/harness/internal/githubsource/model"
	"go.harness.dev/harness/tempest/durable"
)

type notice struct {
	seq     uint64
	tickets []model.TicketID
}

type recorder struct {
	mu      sync.Mutex
	notices []notice
}

func (r *recorder) notify(seq uint64, tickets []model.TicketID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notices = append(r.notices, notice{seq, tickets})
}

func (r *recorder) take() []notice {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.notices
	r.notices = nil
	return out
}

func TestSubscribeNotifiesEveryCommit(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	var r recorder
	cancel := h.store.Subscribe(r.notify)
	id := h.ingest("thread:450", mention(450, false, 1, parth, "fix", h.clock.Now())).Decisions[0].Tickets[0]
	got := r.take()
	if len(got) != 1 || got[0].seq != h.status().Seq || !slices.Equal(got[0].tickets, []model.TicketID{id}) {
		t.Fatalf("ingest notices: %+v", got)
	}
	// A reread commits nothing and notifies nothing.
	h.ingest("thread:450", mention(450, false, 1, parth, "fix", h.clock.Now()))
	if got := r.take(); len(got) != 0 {
		t.Fatalf("no-op notices: %+v", got)
	}
	// A dispatch transition names its Ticket though the Ticket document
	// may not change.
	admitted := h.admit()
	if got := r.take(); len(got) != 1 || !slices.Equal(got[0].tickets, []model.TicketID{id}) {
		t.Fatalf("admit notices: %+v", got)
	}
	if err := h.store.Preparing(h.ctx, admitted[0].Dispatch.ID, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if got := r.take(); len(got) != 1 || !slices.Equal(got[0].tickets, []model.TicketID{id}) {
		t.Fatalf("preparing notices: %+v", got)
	}
	// An operator pause names no Ticket but is still a commit.
	if err := h.store.Operator().SetPaused(h.ctx, model.OperatorRequest{ID: "p"}, true); err != nil {
		t.Fatal(err)
	}
	if got := r.take(); len(got) != 1 || len(got[0].tickets) != 0 || got[0].seq != h.status().Seq {
		t.Fatalf("pause notices: %+v", got)
	}
	cancel()
	if err := h.store.Operator().SetPaused(h.ctx, model.OperatorRequest{ID: "u"}, false); err != nil {
		t.Fatal(err)
	}
	if got := r.take(); len(got) != 0 {
		t.Fatalf("notified after cancel: %+v", got)
	}
}

// failAfterDurable makes the next commit durable and then fails it, as a
// lost acknowledgment.
type failAfterDurable struct {
	durable.Store
	fail bool
}

func (f *failAfterDurable) Commit(ctx context.Context, commit durable.Commit) (durable.Receipt, error) {
	receipt, err := f.Store.Commit(ctx, commit)
	if err == nil && f.fail {
		f.fail = false
		return durable.Receipt{}, errors.New("acknowledgment lost")
	}
	return receipt, err
}

// A commit whose acknowledgment was lost but which the reload shows durable
// is still announced: subscribers must not miss a state change.
func TestSubscribeAnnouncesDurableCommitWithLostAck(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	wrapper := &failAfterDurable{Store: h.store.store}
	h.store.store = wrapper
	var r recorder
	h.store.Subscribe(r.notify)
	wrapper.fail = true
	_, err := h.store.Ingest(h.ctx, model.IntakeBatch{Resource: "thread:451", Events: []model.Event{mention(451, false, 1, parth, "fix", h.clock.Now())}, ReadAt: h.clock.Now()})
	if err == nil {
		t.Fatal("ingest succeeded through a lost acknowledgment")
	}
	got := r.take()
	if len(got) != 1 || len(got[0].tickets) != 1 || got[0].seq != h.status().Seq {
		t.Fatalf("notices: %+v", got)
	}
}
