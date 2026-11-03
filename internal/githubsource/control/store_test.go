package control

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
	"go.harness.dev/harness/tempest/durable"
)

func TestSecondWriterIsLocked(t *testing.T) {
	h := newHarness(t)
	if _, err := durable.OpenFile(h.ctx, h.path); !errors.Is(err, durable.ErrLocked) {
		t.Fatalf("second open: %v", err)
	}
}

func TestJournalOfAnotherRepositoryIsRefused(t *testing.T) {
	h := newHarness(t)
	h.close()
	log, err := durable.OpenFile(h.ctx, h.path)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close(h.ctx)
	other := testRepo()
	other.RepoID = 43
	if _, err := Open(h.ctx, Options{Store: log, Config: h.config, Repo: other, Clock: h.clock}); err == nil {
		t.Fatal("opened a journal recorded for another repository")
	}
}

func TestPolicyRecordedOncePerDigest(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	result := h.ingest("thread", mention(600, false, 1, parth, "fix", h.clock.Now()))
	digest := result.Decisions[0].PolicyDigest
	h.clock.Advance(time.Hour)
	h.reopen()
	h.config.Admission.Actors = append(h.config.Admission.Actors, model.ActorID{Login: mallory.Login, ID: mallory.ID})
	h.reopen()
	second := h.ingest("thread", mention(601, false, 2, mallory, "fix", h.clock.Now()))
	if second.Decisions[0].Kind != model.DecisionRequest || second.Decisions[0].PolicyDigest == digest {
		t.Fatalf("new policy: %+v", second.Decisions[0])
	}
	page, err := h.store.store.Read(h.ctx, durable.Query{SessionID: model.ControlSession, Kind: durable.EntryKind, Namespace: model.NamespacePolicy})
	if err != nil || len(page.Records) != 2 {
		t.Fatalf("policies: %d %v", len(page.Records), err)
	}
	if mandate := h.detail(second.Decisions[0].Tickets[0]).Mandate; mandate.PolicyDigest != second.Decisions[0].PolicyDigest {
		t.Fatalf("mandate policy %s", mandate.PolicyDigest)
	}
}

func TestTicketTitleFromLaterThreadRead(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	id := h.ingest("thread:610", mention(610, false, 1, parth, "fix", h.clock.Now())).Decisions[0].Tickets[0]
	if _, err := h.store.Ingest(h.ctx, model.IntakeBatch{Resource: "thread:610", Threads: []model.ThreadUpdate{{Ref: model.ThreadRef{Number: 610}, Title: "Parser crashes on empty input"}}, ReadAt: h.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	if ticket := h.ticket(id); ticket.Title != "Parser crashes on empty input" {
		t.Fatalf("title %q", ticket.Title)
	}
}

func TestConcurrentCallers(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	var wg sync.WaitGroup
	for worker := range 4 {
		wg.Go(func() {
			for i := range 10 {
				number := 700 + worker*10 + i
				if _, err := h.store.Ingest(h.ctx, model.IntakeBatch{Resource: fmt.Sprintf("thread:%d", number), Events: []model.Event{mention(number, false, int64(number), parth, "fix", epoch)}, ReadAt: epoch}); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Go(func() {
		for range 20 {
			if _, err := h.store.AdmitNext(h.ctx, epoch); err != nil {
				t.Error(err)
			}
			if _, err := h.store.Operator().Status(h.ctx); err != nil {
				t.Error(err)
			}
			if _, err := h.store.Due(h.ctx, epoch); err != nil {
				t.Error(err)
			}
		}
	})
	wg.Wait()
	status, err := h.store.Operator().Status(h.ctx)
	if err != nil || status.OpenTickets != 40 {
		t.Fatalf("status %+v %v", status, err)
	}
	if occupancy := status.Occupancy; occupancy.Normal > occupancy.MaxNormal || occupancy.Human > occupancy.MaxHuman {
		t.Fatalf("over-admitted: %+v", occupancy)
	}
}

func TestOperatorListings(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	blocked, dispatch := h.launched(800, 1)
	if _, err := h.store.Report(h.ctx, model.WorkerCall{SessionID: sessionOf(blocked), ToolCallID: "q"}, model.ReportRequest{Status: model.ReportBlocked, Question: &model.Question{Text: "Which?"}}); err != nil {
		t.Fatal(err)
	}
	h.finish(dispatch, completed(1))
	h.at(time.Minute)
	working, _ := h.launched(801, 2)
	h.at(2 * time.Minute)
	done, doneDispatch := h.launched(802, 3)
	h.report(done, "r", model.ReportReady)
	h.finish(doneDispatch, completed(1))
	operator := h.store.Operator()
	page, err := operator.Tickets(h.ctx, model.TicketQuery{Filter: model.FilterAttention})
	if err != nil || len(page.Tickets) != 2 || page.Tickets[0].ID != blocked || page.Tickets[0].Stage != model.StageAnswer || page.Tickets[0].Question == nil || page.Tickets[1].ID != working {
		t.Fatalf("attention: %+v %v", page, err)
	}
	page, _ = operator.Tickets(h.ctx, model.TicketQuery{Filter: model.FilterEnded})
	if len(page.Tickets) != 1 || page.Tickets[0].ID != done {
		t.Fatalf("ended: %+v", page)
	}
	page, _ = operator.Tickets(h.ctx, model.TicketQuery{Limit: 2})
	if len(page.Tickets) != 2 || page.Tickets[0].ID != done || page.NextCursor != "2" {
		t.Fatalf("all page 1: %+v", page)
	}
	page, _ = operator.Tickets(h.ctx, model.TicketQuery{Limit: 2, Cursor: page.NextCursor})
	if len(page.Tickets) != 1 || page.Tickets[0].ID != blocked || page.NextCursor != "" {
		t.Fatalf("all page 2: %+v", page)
	}
	events, err := operator.Events(h.ctx, model.EventQuery{Ticket: blocked})
	if err != nil || len(events.Events) != 1 || events.Events[0].Decision == nil || events.Events[0].Decision.Kind != model.DecisionRequest {
		t.Fatalf("ticket events: %+v %v", events, err)
	}
	status, _ := operator.Status(h.ctx)
	if status.OpenTickets != 2 || status.Attention != 1 || status.Occupancy.Normal != 1 || status.RepoLedger.Spent.MicroUSD != 2 {
		t.Fatalf("status: %+v", status)
	}
	if detail := h.detail(working); detail.Ticket.SessionID != sessionOf(working) || detail.Stage != model.StageWorking {
		t.Fatalf("working detail: %+v", detail)
	}
}
