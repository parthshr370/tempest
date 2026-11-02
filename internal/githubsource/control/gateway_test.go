package control

import (
	"errors"
	"testing"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
)

func TestGatewayTargetValidation(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	id, _ := h.launched(400, 1)
	h.openPR(id, "pr", 401)
	review := model.Event{ID: model.ReviewCommentID(77, h.clock.Now()), Kind: model.EventReviewComment, Thread: model.ThreadRef{Number: 401, IsPR: true}, Actor: alice, At: h.clock.Now(), Body: "rename this", CommentID: 77}
	foreign := model.Event{ID: model.ReviewCommentID(88, h.clock.Now()), Kind: model.EventReviewComment, Thread: model.ThreadRef{Number: 999, IsPR: true}, Actor: alice, At: h.clock.Now(), Body: "elsewhere", CommentID: 88}
	h.ingest("pr:401", review)
	h.ingest("pr:999", foreign)
	call := func(name string) model.WorkerCall {
		return model.WorkerCall{SessionID: sessionOf(id), ToolCallID: name, At: h.clock.Now()}
	}
	cases := []struct {
		name    string
		request model.EffectRequest
		invalid bool
		check   func(t *testing.T, intent model.OutboxIntent)
	}{
		{name: "comment defaults to the ticket thread", request: model.EffectRequest{Kind: model.EffectComment, Payload: model.EffectPayload{Body: "hi"}}, check: func(t *testing.T, intent model.OutboxIntent) {
			if intent.Payload.Number != 400 {
				t.Fatalf("number %d", intent.Payload.Number)
			}
		}},
		{name: "comment on own PR", request: model.EffectRequest{Kind: model.EffectComment, Payload: model.EffectPayload{Number: 401, Body: "hi"}}},
		{name: "comment on another issue", request: model.EffectRequest{Kind: model.EffectComment, Payload: model.EffectPayload{Number: 12, Body: "hi"}}, invalid: true},
		{name: "review reply on own PR", request: model.EffectRequest{Kind: model.EffectReviewReply, Payload: model.EffectPayload{ReplyTo: 77, Body: "done"}}, check: func(t *testing.T, intent model.OutboxIntent) {
			if intent.Payload.Number != 401 {
				t.Fatalf("number %d", intent.Payload.Number)
			}
		}},
		{name: "review reply on a foreign PR", request: model.EffectRequest{Kind: model.EffectReviewReply, Payload: model.EffectPayload{ReplyTo: 88, Body: "done"}}, invalid: true},
		{name: "review reply to unknown comment", request: model.EffectRequest{Kind: model.EffectReviewReply, Payload: model.EffectPayload{ReplyTo: 5, Body: "done"}}, invalid: true},
		{name: "push to another branch", request: model.EffectRequest{Kind: model.EffectPush, Payload: model.EffectPayload{Branch: "main", SHA: "abc"}}, invalid: true},
		{name: "push fills branch and first lease", request: model.EffectRequest{Kind: model.EffectPush, Payload: model.EffectPayload{SHA: "sha1", LeaseSHA: "forged"}}, check: func(t *testing.T, intent model.OutboxIntent) {
			if intent.Payload.Branch != model.TicketBranch(id) || intent.Payload.LeaseSHA != "" {
				t.Fatalf("push payload %+v", intent.Payload)
			}
		}},
		{name: "second push leases the first", request: model.EffectRequest{Kind: model.EffectPush, Payload: model.EffectPayload{SHA: "sha2"}}, check: func(t *testing.T, intent model.OutboxIntent) {
			if intent.Payload.LeaseSHA != "sha1" {
				t.Fatalf("lease %q", intent.Payload.LeaseSHA)
			}
		}},
		{name: "open_pr against another base", request: model.EffectRequest{Kind: model.EffectOpenPR, Payload: model.EffectPayload{Title: "x", Base: "release"}}, invalid: true},
		{name: "update a PR the ticket did not open", request: model.EffectRequest{Kind: model.EffectUpdatePR, Payload: model.EffectPayload{Number: 999, Title: "x"}}, invalid: true},
		{name: "update own PR", request: model.EffectRequest{Kind: model.EffectUpdatePR, Payload: model.EffectPayload{Number: 401, Title: "x"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			intent, err := h.store.Enqueue(h.ctx, call(tc.name), tc.request)
			if tc.invalid {
				if !errors.Is(err, model.ErrInvalidTarget) {
					t.Fatalf("want ErrInvalidTarget, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.check != nil {
				tc.check(t, intent)
			}
		})
	}
	if _, err := h.store.Enqueue(h.ctx, model.WorkerCall{SessionID: "stranger", ToolCallID: "x"}, model.EffectRequest{Kind: model.EffectComment, Payload: model.EffectPayload{Body: "hi"}}); !errors.Is(err, model.ErrNotBound) {
		t.Fatalf("unbound session: %v", err)
	}
	if _, err := h.store.Thread(h.ctx, sessionOf(id), model.ThreadQuery{Number: 999}); !errors.Is(err, model.ErrInvalidTarget) {
		t.Fatalf("thread outside ticket: %v", err)
	}
	page, err := h.store.Thread(h.ctx, sessionOf(id), model.ThreadQuery{Number: 401})
	if err != nil || len(page.Events) != 1 || page.Events[0].Event.ID != review.ID || page.Events[0].Decision == nil {
		t.Fatalf("thread page: %+v %v", page, err)
	}
}

func TestEnqueueIsIdempotentPerToolCall(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	id, _ := h.launched(410, 1)
	call := model.WorkerCall{SessionID: sessionOf(id), ToolCallID: "call-1", At: h.clock.Now()}
	request := model.EffectRequest{Kind: model.EffectComment, Payload: model.EffectPayload{Body: "first"}}
	first, err := h.store.Enqueue(h.ctx, call, request)
	if err != nil {
		t.Fatal(err)
	}
	h.reopen()
	request.Payload.Body = "replayed with different text"
	again, err := h.store.Enqueue(h.ctx, call, request)
	if err != nil || again.ID != first.ID || again.Payload.Body != "first" || again.Seq != 1 {
		t.Fatalf("replay: %+v %v", again, err)
	}
	effect, err := h.store.Effect(h.ctx, sessionOf(id), first.ID)
	if err != nil || effect.ID != first.ID {
		t.Fatalf("effect: %+v %v", effect, err)
	}
	other, _ := h.launched(411, 2)
	if _, err := h.store.Effect(h.ctx, sessionOf(other), first.ID); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("other ticket read the intent: %v", err)
	}
}

func TestOutboxDueAttemptSettle(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	id, _ := h.launched(420, 1)
	enqueue := func(call string, body string) model.OutboxIntent {
		intent, err := h.store.Enqueue(h.ctx, model.WorkerCall{SessionID: sessionOf(id), ToolCallID: call, At: h.clock.Now()}, model.EffectRequest{Kind: model.EffectComment, Payload: model.EffectPayload{Body: body}})
		if err != nil {
			t.Fatal(err)
		}
		return intent
	}
	first := enqueue("c1", "one")
	second := enqueue("c2", "two")
	due, _ := h.store.Due(h.ctx, h.clock.Now())
	if len(due) != 1 || due[0].ID != first.ID {
		t.Fatalf("due should be the oldest only: %+v", due)
	}
	attempting, err := h.store.Attempt(h.ctx, first.ID, h.clock.Now())
	if err != nil || attempting.State != model.EffectAttempting || attempting.Attempts != 1 {
		t.Fatalf("attempt: %+v %v", attempting, err)
	}
	// Restart with the attempt in flight: it is due for reconciliation and a
	// repeated Attempt does not count a second send.
	h.reopen()
	due, _ = h.store.Due(h.ctx, h.clock.Now())
	if len(due) != 1 || due[0].State != model.EffectAttempting {
		t.Fatalf("attempting after restart: %+v", due)
	}
	if again, _ := h.store.Attempt(h.ctx, first.ID, h.clock.Now()); again.Attempts != 1 {
		t.Fatalf("repeated attempt counted: %+v", again)
	}
	// Reconciliation proves absence: back to pending with a retry time.
	retryAt := h.clock.Now().Add(time.Minute)
	if _, err := h.store.Settle(h.ctx, first.ID, model.EffectSettlement{State: model.EffectPending, Error: "not found", RetryAt: retryAt}); err != nil {
		t.Fatal(err)
	}
	if due, _ := h.store.Due(h.ctx, h.clock.Now()); len(due) != 0 {
		t.Fatalf("due before retry time: %+v", due)
	}
	h.clock.Set(retryAt)
	h.store.Attempt(h.ctx, first.ID, h.clock.Now())
	confirmed, err := h.store.Settle(h.ctx, first.ID, model.EffectSettlement{State: model.EffectConfirmed, Result: &model.EffectReply{RemoteID: 5, Reconciled: true}})
	if err != nil || confirmed.State != model.EffectConfirmed || confirmed.Attempts != 2 {
		t.Fatalf("confirm: %+v %v", confirmed, err)
	}
	if repeat, err := h.store.Settle(h.ctx, first.ID, model.EffectSettlement{State: model.EffectConfirmed}); err != nil || repeat.State != model.EffectConfirmed {
		t.Fatalf("repeated settle: %+v %v", repeat, err)
	}
	due, _ = h.store.Due(h.ctx, h.clock.Now())
	if len(due) != 1 || due[0].ID != second.ID {
		t.Fatalf("next intent: %+v", due)
	}
	// An unknown outcome blocks nothing but needs the operator; only
	// confirmed, failed or pending (resend) resolve it.
	h.store.Attempt(h.ctx, second.ID, h.clock.Now())
	h.store.Settle(h.ctx, second.ID, model.EffectSettlement{State: model.EffectUnknown, Error: "listing failed"})
	if stage := h.detail(id).Stage; stage != model.StageDelivery {
		t.Fatalf("stage %s", stage)
	}
	operator := h.store.Operator()
	for _, state := range []model.EffectState{model.EffectAttempting, model.EffectCancelled, model.EffectUnknown} {
		if err := operator.ResolveEffect(h.ctx, model.OperatorRequest{ID: "res-0", Target: string(second.ID)}, state); err == nil {
			t.Fatalf("unknown resolved to %s", state)
		}
	}
	if err := operator.ResolveEffect(h.ctx, model.OperatorRequest{ID: "res-1", Target: string(second.ID), Reason: "checked"}, model.EffectConfirmed); err != nil {
		t.Fatal(err)
	}
	status, _ := operator.Status(h.ctx)
	if status.UnknownEffects != 0 {
		t.Fatalf("unknown effects %d", status.UnknownEffects)
	}
}

func TestOutboxAttemptLimitAndStop(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	id, _ := h.launched(430, 1)
	intent, _ := h.store.Enqueue(h.ctx, model.WorkerCall{SessionID: sessionOf(id), ToolCallID: "c1"}, model.EffectRequest{Kind: model.EffectComment, Payload: model.EffectPayload{Body: "x"}})
	for range maxEffectAttempts {
		if _, err := h.store.Attempt(h.ctx, intent.ID, h.clock.Now()); err != nil {
			t.Fatal(err)
		}
		h.store.Settle(h.ctx, intent.ID, model.EffectSettlement{State: model.EffectPending, Error: "502"})
	}
	failed, err := h.store.Attempt(h.ctx, intent.ID, h.clock.Now())
	if err == nil || failed.State != model.EffectFailed {
		t.Fatalf("sixth attempt: %+v %v", failed, err)
	}
	// Stop: pending intents are cancelled; an attempting one still
	// reconciles, and proven absence cancels it instead of retrying.
	pending, _ := h.store.Enqueue(h.ctx, model.WorkerCall{SessionID: sessionOf(id), ToolCallID: "c2"}, model.EffectRequest{Kind: model.EffectComment, Payload: model.EffectPayload{Body: "y"}})
	h.store.Attempt(h.ctx, pending.ID, h.clock.Now())
	queued, _ := h.store.Enqueue(h.ctx, model.WorkerCall{SessionID: sessionOf(id), ToolCallID: "c3"}, model.EffectRequest{Kind: model.EffectComment, Payload: model.EffectPayload{Body: "z"}})
	if err := h.store.Operator().Stop(h.ctx, model.OperatorRequest{ID: "s", Target: string(id)}); err != nil {
		t.Fatal(err)
	}
	due, _ := h.store.Due(h.ctx, h.clock.Now())
	if len(due) != 1 || due[0].ID != pending.ID {
		t.Fatalf("due after stop: %+v", due)
	}
	settled, err := h.store.Settle(h.ctx, pending.ID, model.EffectSettlement{State: model.EffectPending, Error: "absent"})
	if err != nil || settled.State != model.EffectCancelled {
		t.Fatalf("absent after stop: %+v %v", settled, err)
	}
	if effect, _ := h.store.Effect(h.ctx, sessionOf(id), queued.ID); effect.State != model.EffectCancelled {
		t.Fatalf("queued after stop: %+v", effect)
	}
	if _, err := h.store.Enqueue(h.ctx, model.WorkerCall{SessionID: sessionOf(id), ToolCallID: "c4"}, model.EffectRequest{Kind: model.EffectComment, Payload: model.EffectPayload{Body: "w"}}); !errors.Is(err, model.ErrStopped) {
		t.Fatalf("enqueue after stop: %v", err)
	}
}

func TestStageNeverShowsOldHeadFactsAsCurrent(t *testing.T) {
	ready := &model.Report{Status: model.ReportReady, Gen: 1}
	ticket := model.Ticket{Lifecycle: model.LifecycleOpen, Gen: 1, LastReport: ready, PRs: []model.PRLink{{Number: 5, Own: true}}}
	pr := model.PullRequest{Number: 5, State: "open", HeadSHA: "new",
		CI:      &model.CISnapshot{SHA: "old", Failing: []string{"test"}},
		Reviews: []model.ReviewFact{{ID: 1, State: "APPROVED", CommitSHA: "old"}, {ID: 2, State: "CHANGES_REQUESTED", CommitSHA: "old"}}}
	if CurrentCI(pr) != nil || len(CurrentReviews(pr)) != 0 {
		t.Fatal("old-head CI or review presented as current")
	}
	if stage := Stage(ticket, nil, []model.PullRequest{pr}, nil); stage != model.StageReview {
		t.Fatalf("old-head failure blocked review: %s", stage)
	}
	pr.Reviews = append(pr.Reviews, model.ReviewFact{ID: 3, State: "CHANGES_REQUESTED", CommitSHA: "new"})
	if stage := Stage(ticket, nil, []model.PullRequest{pr}, nil); stage == model.StageReview {
		t.Fatal("current-head changes requested still in review")
	}
	pr.Reviews = pr.Reviews[:2]
	pr.CI = &model.CISnapshot{SHA: "new", Failing: []string{"lint"}}
	if stage := Stage(ticket, nil, []model.PullRequest{pr}, nil); stage == model.StageReview {
		t.Fatal("current-head failing CI still in review")
	}
	// The store drops a snapshot whose SHA is not the new head.
	h := newHarness(t)
	h.at(0)
	id, dispatch := h.launched(440, 1)
	h.openPR(id, "pr", 441)
	h.ingest("pr:441", model.Event{ID: model.PRHeadID(441, "h1"), Kind: model.EventPRHead, Thread: model.ThreadRef{Number: 441, IsPR: true}, CommitSHA: "h1"})
	h.ingest("pr:441", prCI(441, "h1", nil, h.clock.Now()))
	h.ingest("pr:441", model.Event{ID: model.ReviewID(9), Kind: model.EventReview, Thread: model.ThreadRef{Number: 441, IsPR: true}, Actor: parth, ReviewState: "APPROVED", CommitSHA: "h1", CommentID: 9})
	h.ingest("pr:441", model.Event{ID: model.PRHeadID(441, "h2"), Kind: model.EventPRHead, Thread: model.ThreadRef{Number: 441, IsPR: true}, CommitSHA: "h2"})
	h.ingest("pr:441", prCI(441, "h1", []string{"late"}, h.clock.Now()))
	detail := h.detail(id)
	stored := detail.PullRequests[0]
	if stored.HeadSHA != "h2" || stored.CI != nil || len(CurrentReviews(stored)) != 0 || len(stored.Reviews) != 1 {
		t.Fatalf("stored PR: %+v", stored)
	}
	h.finish(dispatch, completed(1))
}
