package control

import (
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
)

func TestAdmitRules(t *testing.T) {
	policy := model.Policy{Mode: model.ModeObserve, Actors: []model.ActorID{{ID: parth.ID}, {ID: alice.ID}}, Bots: []model.ActorID{{ID: rabbit.ID}}}
	open := []subscriber{{ID: "i1-a", Lifecycle: model.LifecycleOpen}}
	ended := []subscriber{{ID: "i1-a", Lifecycle: model.LifecycleEnded}}
	stoppedThenEnded := []subscriber{{ID: "i1-a", Lifecycle: model.LifecycleEnded}, {ID: "i1-b", Lifecycle: model.LifecycleEnded, Stopped: true}}
	onlyStopped := []subscriber{{ID: "i1-b", Lifecycle: model.LifecycleEnded, Stopped: true}}
	at := epoch
	plainBot := comment(1, false, 9, ci, "looks good", at)
	headEvent := model.Event{ID: "pr:1:head:abc", Kind: model.EventPRHead, Thread: model.ThreadRef{Number: 1, IsPR: true}, CommitSHA: "abc"}
	review := model.Event{ID: "review:7", Kind: model.EventReview, Thread: model.ThreadRef{Number: 1, IsPR: true}, Actor: parth, ReviewState: "APPROVED"}

	cases := []struct {
		name    string
		event   model.Event
		subs    []subscriber
		mode    model.Mode
		rule    int
		kind    model.DecisionKind
		action  action
		class   model.InputClass
		wakes   bool
		tickets []model.TicketID
	}{
		{name: "own account ignored", event: mention(1, false, 1, account, "hi", at), subs: open, rule: 1, kind: model.DecisionIgnored},
		{name: "listed stop", event: command(1, 2, parth, model.CommandStop, at), subs: open, rule: 2, kind: model.DecisionStop, action: actStop, tickets: []model.TicketID{"i1-a"}},
		{name: "stop without ticket ignored", event: command(1, 2, parth, model.CommandStop, at), rule: 2, kind: model.DecisionIgnored},
		{name: "stop does not stop ended", event: command(1, 2, parth, model.CommandStop, at), subs: ended, rule: 2, kind: model.DecisionIgnored},
		{name: "unlisted stop denied", event: command(1, 2, mallory, model.CommandStop, at), subs: open, rule: 7, kind: model.DecisionDenied},
		{name: "bot stop is machine input", event: func() model.Event { e := command(1, 2, rabbit, model.CommandStop, at); return e }(), subs: open, rule: 9, kind: model.DecisionInput, action: actInput, class: model.InputMachine, wakes: true, tickets: []model.TicketID{"i1-a"}},
		{name: "status for newest ticket", event: command(1, 3, alice, model.CommandStatus, at), subs: stoppedThenEnded[:1], rule: 3, kind: model.DecisionInput, action: actStatus, tickets: []model.TicketID{"i1-a"}},
		{name: "status for stopped ticket ignored", event: command(1, 3, alice, model.CommandStatus, at), subs: stoppedThenEnded, rule: 3, kind: model.DecisionIgnored},
		{name: "mention on open ticket wakes", event: mention(1, false, 4, parth, "also this", at), subs: open, rule: 4, kind: model.DecisionInput, action: actInput, class: model.InputAuthoritative, wakes: true, tickets: []model.TicketID{"i1-a"}},
		{name: "mention reopens ended", event: mention(1, false, 4, parth, "again", at), subs: ended, rule: 4, kind: model.DecisionReopen, action: actReopen, class: model.InputAuthoritative, wakes: true, tickets: []model.TicketID{"i1-a"}},
		{name: "mention skips stopped and reopens older", event: mention(1, false, 4, parth, "again", at), subs: stoppedThenEnded, rule: 4, kind: model.DecisionReopen, action: actReopen, class: model.InputAuthoritative, wakes: true, tickets: []model.TicketID{"i1-a"}},
		{name: "mention after stop requests new", event: mention(1, false, 4, parth, "again", at), subs: onlyStopped, rule: 4, kind: model.DecisionRequest, action: actRequest, class: model.InputAuthoritative, wakes: true},
		{name: "mention creates request", event: mention(1, false, 4, parth, "fix", at), rule: 4, kind: model.DecisionRequest, action: actRequest, class: model.InputAuthoritative, wakes: true},
		{name: "issue opened with mention requests", event: issueOpened(1, alice, "@tempest-bot fix", true, at), rule: 4, kind: model.DecisionRequest, action: actRequest, class: model.InputAuthoritative, wakes: true},
		{name: "listed follow-up without mention", event: comment(1, false, 5, alice, "more detail", at), subs: open, rule: 5, kind: model.DecisionInput, action: actInput, class: model.InputAuthoritative, wakes: true, tickets: []model.TicketID{"i1-a"}},
		{name: "listed review is authoritative", event: review, subs: open, rule: 5, kind: model.DecisionInput, action: actInput, class: model.InputAuthoritative, wakes: true, tickets: []model.TicketID{"i1-a"}},
		{name: "listed comment on ended thread only observed", event: comment(1, false, 5, alice, "thanks, merged", at), subs: ended, rule: 12, kind: model.DecisionObserved},
		{name: "edit is context even with mention", event: func() model.Event {
			e := edited(mention(1, false, 6, parth, "x", at), at.Add(time.Minute), "@tempest-bot do it")
			e.Mention = true
			return e
		}(), subs: open, rule: 6, kind: model.DecisionInput, action: actInput, class: model.InputContext, tickets: []model.TicketID{"i1-a"}},
		{name: "edit with mention never requests", event: func() model.Event {
			e := edited(comment(1, false, 6, parth, "x", at), at.Add(time.Minute), "@tempest-bot do it")
			e.Mention = true
			return e
		}(), rule: 12, kind: model.DecisionObserved},
		{name: "unlisted mention denied", event: mention(1, false, 7, mallory, "fix", at), subs: open, rule: 7, kind: model.DecisionDenied},
		{name: "unlisted comment is context", event: comment(1, false, 8, mallory, "me too", at), subs: open, rule: 8, kind: model.DecisionInput, action: actInput, class: model.InputContext, tickets: []model.TicketID{"i1-a"}},
		{name: "unlisted comment without ticket observed", event: comment(1, false, 8, mallory, "me too", at), rule: 12, kind: model.DecisionObserved},
		{name: "bot comment is machine", event: comment(1, false, 9, rabbit, "nit", at), subs: open, rule: 9, kind: model.DecisionInput, action: actInput, class: model.InputMachine, wakes: true, tickets: []model.TicketID{"i1-a"}},
		{name: "bot by login suffix", event: plainBot, subs: open, rule: 9, kind: model.DecisionInput, action: actInput, class: model.InputMachine, wakes: true, tickets: []model.TicketID{"i1-a"}},
		{name: "bot mention never requests", event: mention(1, false, 9, rabbit, "fix", at), rule: 12, kind: model.DecisionObserved},
		{name: "pr head wakes", event: headEvent, subs: open, rule: 10, kind: model.DecisionInput, action: actInput, class: model.InputMachine, wakes: true, tickets: []model.TicketID{"i1-a"}},
		{name: "pr closed is non-waking", event: prClosed(1, false, at), subs: open, rule: 11, kind: model.DecisionInput, action: actInput, class: model.InputMachine, tickets: []model.TicketID{"i1-a"}},
		{name: "observe off ignores", event: comment(1, false, 8, mallory, "hello", at), mode: model.ModeOff, rule: 12, kind: model.DecisionIgnored},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mode := tc.mode
			if mode == "" {
				mode = model.ModeObserve
			}
			got := admit(tc.event, admissionView{Policy: policy, Account: account.ID, Mode: mode, Subscribers: tc.subs})
			if got.Rule != tc.rule || got.Kind != tc.kind || got.Action != tc.action || got.Class != tc.class || got.Wakes != tc.wakes {
				t.Fatalf("got rule %d %s action %d class %q wakes %v, want rule %d %s action %d class %q wakes %v", got.Rule, got.Kind, got.Action, got.Class, got.Wakes, tc.rule, tc.kind, tc.action, tc.class, tc.wakes)
			}
			if strings.Join(ids(got.Tickets), ",") != strings.Join(ids(tc.tickets), ",") {
				t.Fatalf("tickets %v, want %v", got.Tickets, tc.tickets)
			}
		})
	}
}

func ids(tickets []model.TicketID) []string {
	out := make([]string, len(tickets))
	for i, id := range tickets {
		out[i] = string(id)
	}
	return out
}

func TestAdmitOwnPushHeadDoesNotWake(t *testing.T) {
	event := model.Event{ID: "pr:5:head:abc", Kind: model.EventPRHead, Thread: model.ThreadRef{Number: 5, IsPR: true}, CommitSHA: "abc"}
	got := admit(event, admissionView{Account: account.ID, Subscribers: []subscriber{{ID: "i1-a", Lifecycle: model.LifecycleOpen, OwnHead: true}}})
	if got.Rule != 10 || !got.NoWake["i1-a"] {
		t.Fatalf("own push head should not wake: %+v", got)
	}
}

func TestRequestCreatesTicketOnceAndDuplicatesCommitNothing(t *testing.T) {
	h := newHarness(t)
	at := h.at(0)
	request := mention(7, false, 501, parth, "please fix the parser", at)
	first := h.ingest("thread:7", request)
	if first.NewEvents != 1 || len(first.Decisions) != 1 || first.Decisions[0].Kind != model.DecisionRequest || !first.Dispatch {
		t.Fatalf("first ingest: %+v", first)
	}
	id := first.Decisions[0].Tickets[0]
	if id != model.NewTicketID(model.ThreadRef{Number: 7}, request.ID) {
		t.Fatalf("ticket id %s", id)
	}
	seq := first.Seq

	// The same page reread (same content) and a later reread with the same
	// event both commit nothing new.
	again := h.ingest("thread:7", request)
	h.at(time.Minute)
	later := h.ingest("thread:7", request)
	if again.NewEvents != 0 || later.NewEvents != 0 || later.Seq != seq {
		t.Fatalf("reread changed state: again %+v later %+v (seq %d)", again, later, seq)
	}
	h.reopen()
	afterRestart := h.ingest("thread:7", request)
	if afterRestart.NewEvents != 0 || afterRestart.Seq != seq {
		t.Fatalf("reread after restart changed state: %+v", afterRestart)
	}
	detail := h.detail(id)
	if detail.Ticket.Lifecycle != model.LifecycleOpen || detail.Ticket.InputSeq != 1 || len(detail.Dispatches) != 1 || detail.Mandate.Actor.ID != parth.ID {
		t.Fatalf("ticket after duplicates: %+v", detail.Ticket)
	}
	if detail.Dispatches[0].Kind != model.DispatchLaunch || detail.Dispatches[0].State != model.DispatchQueued {
		t.Fatalf("launch dispatch: %+v", detail.Dispatches[0])
	}
	page, err := h.store.Operator().Tickets(h.ctx, model.TicketQuery{})
	if err != nil || len(page.Tickets) != 1 {
		t.Fatalf("tickets: %+v %v", page, err)
	}
}

func TestDelayedCommentWithinLookbackIsAdmitted(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	request := mention(7, false, 501, parth, "fix", epoch)
	result := h.ingest("thread:7", request)
	id := result.Decisions[0].Tickets[0]
	// A comment written at +1m is only indexed at +10m; the next read (with
	// lookback) returns it along with the already-known request.
	h.at(10 * time.Minute)
	late := comment(7, false, 502, alice, "and the lexer too", epoch.Add(time.Minute))
	result = h.ingest("thread:7", request, late)
	if result.NewEvents != 1 || result.Decisions[0].Event != late.ID || result.Decisions[0].Kind != model.DecisionInput {
		t.Fatalf("late comment: %+v", result)
	}
	ticket := h.ticket(id)
	if ticket.InputSeq != 2 {
		t.Fatalf("inputs %d", ticket.InputSeq)
	}
	// The queued launch coalesces the new input: still one dispatch.
	if ticket.DispatchSeq != 1 {
		t.Fatalf("dispatches %d", ticket.DispatchSeq)
	}
}

func TestEditIsContextOnlyNeverRequest(t *testing.T) {
	h := newHarness(t)
	at := h.at(0)
	original := comment(9, false, 600, parth, "hmm", at)
	result := h.ingest("thread:9", original)
	if result.Decisions[0].Kind != model.DecisionObserved {
		t.Fatalf("plain comment: %+v", result.Decisions[0])
	}
	// Editing the old comment to add a mention never creates a request.
	edit := edited(original, at.Add(time.Minute), "@tempest-bot fix it")
	edit.Mention = true
	result = h.ingest("thread:9", edit)
	if result.Decisions[0].Kind == model.DecisionRequest || len(result.Decisions[0].Tickets) != 0 {
		t.Fatalf("edit requested work: %+v", result.Decisions[0])
	}
	// On an open Ticket an edit is a context Input that does not wake.
	request := mention(9, false, 601, parth, "fix", at.Add(2*time.Minute))
	id := h.ingest("thread:9", request).Decisions[0].Tickets[0]
	admitted := h.runAll()
	h.finish(admitted[0].Dispatch.ID, completed(100_000))
	second := edited(request, at.Add(3*time.Minute), "@tempest-bot fix, and docs")
	second.Mention = true
	result = h.ingest("thread:9", second)
	if result.Decisions[0].Kind != model.DecisionInput || result.Dispatch {
		t.Fatalf("edit on open ticket: %+v", result)
	}
	detail := h.detail(id)
	last := detail.Inputs[len(detail.Inputs)-1].Input
	if last.Class != model.InputContext || last.Wakes {
		t.Fatalf("edit input: %+v", last)
	}
	if detail.Ticket.DispatchSeq != 1 {
		t.Fatalf("edit queued a dispatch: %d", detail.Ticket.DispatchSeq)
	}
}

func TestUnlistedMentionDeniedAndBotLoopHitsWakeLimit(t *testing.T) {
	h := newHarness(t)
	at := h.at(0)
	denied := h.ingest("thread:3", mention(3, false, 700, mallory, "delete everything", at))
	if denied.Decisions[0].Kind != model.DecisionDenied || denied.Dispatch {
		t.Fatalf("unlisted mention: %+v", denied)
	}
	id := h.ingest("thread:3", mention(3, false, 701, parth, "fix", at)).Decisions[0].Tickets[0]
	admitted := h.runAll()
	h.finish(admitted[0].Dispatch.ID, completed(100_000))
	// Each bot comment is a machine Input that wakes the worker; the fourth
	// wake on the same UTC day trips maxAutoWakesPerDay (3).
	for i := range 4 {
		h.at(time.Duration(i+1) * time.Minute)
		result := h.ingest("thread:3", comment(3, false, int64(710+i), rabbit, "nit", h.clock.Now()))
		if result.Decisions[0].Kind != model.DecisionInput {
			t.Fatalf("bot comment %d: %+v", i, result.Decisions[0])
		}
		ticket := h.ticket(id)
		if i < 3 {
			if ticket.Hold != model.HoldNone {
				t.Fatalf("held early at %d: %s", i, ticket.Hold)
			}
			for _, a := range h.runAll() {
				h.finish(a.Dispatch.ID, completed(100_000))
			}
			continue
		}
		if ticket.Hold != model.HoldWakeLimit || ticket.AutoWakes != 3 {
			t.Fatalf("wake limit not applied: hold %q wakes %d", ticket.Hold, ticket.AutoWakes)
		}
	}
	if admitted := h.admit(); len(admitted) != 0 {
		t.Fatalf("held ticket admitted: %+v", admitted)
	}
	// An authoritative follow-up releases the hold and wakes it.
	h.at(10 * time.Minute)
	h.ingest("thread:3", comment(3, false, 720, alice, "ignore the bot", h.clock.Now()))
	ticket := h.ticket(id)
	if ticket.Hold != model.HoldNone {
		t.Fatalf("authoritative input did not release: %s", ticket.Hold)
	}
	admitted = h.admit()
	if len(admitted) != 1 || admitted[0].Dispatch.Class != model.SlotHuman {
		t.Fatalf("human wake: %+v", admitted)
	}
	// The counter resets the next UTC day.
	h.at(26 * time.Hour)
	h.finish(admitted[0].Dispatch.ID, completed(100_000))
	h.ingest("thread:3", comment(3, false, 730, rabbit, "nit", h.clock.Now()))
	if ticket := h.ticket(id); ticket.AutoWakes != 1 || ticket.Hold != model.HoldNone {
		t.Fatalf("next day: wakes %d hold %q", ticket.AutoWakes, ticket.Hold)
	}
}

func TestStatusCommandQueuesControllerReply(t *testing.T) {
	h := newHarness(t)
	at := h.at(0)
	id := h.ingest("thread:4", mention(4, false, 800, parth, "fix", at)).Decisions[0].Tickets[0]
	status := command(4, 801, alice, model.CommandStatus, at.Add(time.Minute))
	result := h.ingest("thread:4", status)
	if !result.Outbox || result.Decisions[0].Kind != model.DecisionInput {
		t.Fatalf("status: %+v", result)
	}
	due, err := h.store.Due(h.ctx, h.clock.Now())
	if err != nil || len(due) != 1 {
		t.Fatalf("due: %+v %v", due, err)
	}
	intent := due[0]
	if intent.ID != model.NewOutboxID(id, 0, string(status.ID)) || intent.Gen != 0 || intent.Kind != model.EffectComment || intent.Payload.Number != 4 || !strings.Contains(intent.Payload.Body, string(id)) {
		t.Fatalf("status intent: %+v", intent)
	}
	if ticket := h.ticket(id); ticket.InputSeq != 1 {
		t.Fatalf("status command became an input: %d", ticket.InputSeq)
	}
}

func TestStopCommandCancelsQueuedWorkAndIntents(t *testing.T) {
	h := newHarness(t)
	at := h.at(0)
	id := h.ingest("thread:5", mention(5, false, 900, parth, "fix", at)).Decisions[0].Tickets[0]
	admitted := h.runAll()
	if _, err := h.store.Enqueue(h.ctx, model.WorkerCall{SessionID: sessionOf(id), ToolCallID: "c1"}, model.EffectRequest{Kind: model.EffectComment, Payload: model.EffectPayload{Body: "working on it"}}); err != nil {
		t.Fatal(err)
	}
	result := h.ingest("thread:5", command(5, 901, alice, model.CommandStop, at.Add(time.Minute)))
	if result.Decisions[0].Kind != model.DecisionStop || !result.Dispatch {
		t.Fatalf("stop: %+v", result)
	}
	ticket := h.ticket(id)
	if !ticket.Stopped || ticket.Outcome != model.OutcomeStopped || ticket.EndedBy != "human:alice" {
		t.Fatalf("stopped ticket: %+v", ticket)
	}
	detail := h.detail(id)
	if detail.Outbox[0].State != model.EffectCancelled {
		t.Fatalf("pending intent not cancelled: %+v", detail.Outbox[0])
	}
	// The running dispatch keeps its slot until the dispatcher settles it.
	active, _ := h.store.Active(h.ctx)
	if len(active) != 1 || !active[0].Ticket.Stopped {
		t.Fatalf("active after stop: %+v", active)
	}
	h.finish(admitted[0].Dispatch.ID, model.RunOutcome{StopReason: "aborted", Usage: model.Spend{MicroUSD: 50_000}, UsageKnown: true})
	detail = h.detail(id)
	if len(detail.Dispatches) != 1 || detail.Ticket.Gen != 1 {
		t.Fatalf("stopped ticket got more work: %+v", detail.Dispatches)
	}
	// Stop is permanent: a later mention requests a new Ticket instead.
	result = h.ingest("thread:5", mention(5, false, 902, parth, "try again", at.Add(2*time.Minute)))
	if result.Decisions[0].Kind != model.DecisionRequest || result.Decisions[0].Tickets[0] == id {
		t.Fatalf("mention after stop: %+v", result.Decisions[0])
	}
	if _, err := h.store.Report(h.ctx, model.WorkerCall{SessionID: sessionOf(id), ToolCallID: "late"}, model.ReportRequest{Status: model.ReportReady}); err == nil {
		t.Fatal("report on stopped ticket accepted")
	}
}

func TestBatchAboveLimitsSplitsWithoutDropping(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	h.ingest("thread:11", mention(11, false, 1, parth, "fix", epoch))
	var events []model.Event
	for i := range 250 {
		events = append(events, comment(11, false, int64(1000+i), mallory, strings.Repeat("x", 4096), epoch.Add(time.Duration(i)*time.Second)))
	}
	position := model.Position{Since: epoch.Add(249 * time.Second), LastID: 1249}
	result, err := h.store.Ingest(h.ctx, model.IntakeBatch{Resource: "thread:11", Events: events, Threads: []model.ThreadUpdate{{Ref: model.ThreadRef{Number: 11}, Comments: &position}}, ReadAt: h.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if result.NewEvents != 250 {
		t.Fatalf("new events %d", result.NewEvents)
	}
	watched, err := h.store.Watched(h.ctx, h.clock.Now())
	if err != nil || len(watched) != 1 || !watched[0].Thread.Comments.Since.Equal(position.Since) {
		t.Fatalf("position: %+v %v", watched, err)
	}
	page, err := h.store.Operator().Events(h.ctx, model.EventQuery{Thread: 11, Limit: 100})
	if err != nil || len(page.Events) != 100 || page.NextCursor == "" {
		t.Fatalf("events page: %d %q %v", len(page.Events), page.NextCursor, err)
	}
}

func TestPositionsAndCursorsOnlyMoveForward(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	late := model.Position{Since: epoch.Add(time.Hour), LastID: 9}
	early := model.Position{Since: epoch, LastID: 1}
	reconcile, done := true, false
	ref := model.ThreadRef{Number: 12}
	batch := func(resource string, update model.ThreadUpdate, cursor *model.Cursor) {
		if _, err := h.store.Ingest(h.ctx, model.IntakeBatch{Resource: resource, Threads: []model.ThreadUpdate{update}, Cursor: cursor, ReadAt: h.clock.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	batch("notifications", model.ThreadUpdate{Ref: ref, Reconcile: &reconcile, NotifiedAt: epoch.Add(30 * time.Minute)}, &model.Cursor{Key: model.CursorNotifications, Since: epoch.Add(time.Hour)})
	batch("thread:12", model.ThreadUpdate{Ref: ref, Comments: &late}, nil)
	batch("thread:12", model.ThreadUpdate{Ref: ref, Comments: &early}, nil)
	batch("notifications", model.ThreadUpdate{Ref: ref}, &model.Cursor{Key: model.CursorNotifications, Since: epoch})
	watched, _ := h.store.Watched(h.ctx, h.clock.Now())
	if len(watched) != 1 || !watched[0].Thread.NeedsReconcile || watched[0].Thread.Comments != late {
		t.Fatalf("thread: %+v", watched)
	}
	cursor, ok, err := h.store.Cursor(h.ctx, model.CursorNotifications)
	if err != nil || !ok || !cursor.Since.Equal(epoch.Add(time.Hour)) {
		t.Fatalf("cursor moved back: %+v", cursor)
	}
	// A complete read that started before the notification keeps the flag;
	// one after it clears it, and the thread is no longer watched.
	h.at(10 * time.Minute)
	batch("thread:12", model.ThreadUpdate{Ref: ref, Reconcile: &done}, nil)
	if watched, _ := h.store.Watched(h.ctx, h.clock.Now()); len(watched) != 1 {
		t.Fatalf("flag cleared by an older read: %+v", watched)
	}
	h.at(time.Hour)
	batch("thread:12", model.ThreadUpdate{Ref: ref, Reconcile: &done}, nil)
	if watched, _ := h.store.Watched(h.ctx, h.clock.Now()); len(watched) != 0 {
		t.Fatalf("still watched: %+v", watched)
	}
	gone := model.ThreadUpdate{Ref: ref, State: "gone", Reconcile: &reconcile}
	batch("thread:12", gone, nil)
	if watched, _ := h.store.Watched(h.ctx, h.clock.Now()); len(watched) != 0 {
		t.Fatalf("gone thread watched: %+v", watched)
	}
}
