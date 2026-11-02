package outbox

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
)

// An ordinary 502 whose comment GitHub lists a minute late is confirmed by
// a later read, not stranded as unknown.
func TestAmbiguousWriteFoundOnLaterRead(t *testing.T) {
	w := newWorld(t)
	thread := issueThread(w)
	ticket := w.launch(thread)
	w.writes.LoseAck(http.MethodPost, "/repos/"+repoName+"/issues/1/comments", http.StatusBadGateway, 1)
	w.repo.SetIndexDelay(time.Minute)
	intent := w.enqueue(ticket, "call-1", model.EffectComment, model.EffectPayload{Body: "Done."})
	if err := w.publish(); err != nil {
		t.Fatal(err)
	}
	if got := w.intent(ticket, intent.ID); got.State != model.EffectAttempting {
		t.Fatalf("after the first read: %+v, want attempting", got)
	}
	w.clock.Advance(absentBackoff)
	w.repo.Advance(absentBackoff)
	if err := w.publish(); err != nil {
		t.Fatal(err)
	}
	got := w.intent(ticket, intent.ID)
	if got.State != model.EffectConfirmed || got.Result == nil || !got.Result.Reconciled {
		t.Fatalf("intent = %+v, want confirmed by the later read", got)
	}
	if posted := w.marked(thread.Number, intent.ID); len(posted) != 1 || posted[0].ID != got.Result.RemoteID {
		t.Fatalf("comments = %+v, want exactly one", posted)
	}
	if n := len(w.requests(http.MethodPost, "/comments")); n != 1 {
		t.Fatalf("POSTs = %d, want 1", n)
	}
}

// A write still not found settles unknown only after absentReads reads, the
// last at least absentWindow after the attempt; no read is sent before it
// is due, the original failure is kept in the error, and nothing is resent.
func TestAmbiguousWriteNotFoundSettlesAfterWindow(t *testing.T) {
	w := newWorld(t)
	thread := issueThread(w)
	ticket := w.launch(thread)
	w.writes.LoseAck(http.MethodPost, "/repos/"+repoName+"/issues/1/comments", http.StatusBadGateway, 1)
	w.repo.SetIndexDelay(time.Hour)
	intent := w.enqueue(ticket, "call-1", model.EffectComment, model.EffectPayload{Body: "Done."})
	reads := func() int { return len(w.requests(http.MethodGet, "/issues/1/comments")) }
	if err := w.publish(); err != nil {
		t.Fatal(err)
	}
	attempted := w.intent(ticket, intent.ID).AttemptedAt
	for read := 2; read <= absentReads; read++ {
		before := reads()
		// Not due yet: a pass reads nothing.
		w.clock.Advance(absentBackoff - time.Second)
		if err := w.publish(); err != nil {
			t.Fatal(err)
		}
		if reads() != before {
			t.Fatalf("read %d sent before it was due", read)
		}
		if got := w.intent(ticket, intent.ID); got.State != model.EffectAttempting {
			t.Fatalf("before read %d: %+v, want attempting", read, got)
		}
		w.clock.Advance(time.Second)
		if err := w.publish(); err != nil {
			t.Fatal(err)
		}
		if reads() == before {
			t.Fatalf("read %d not sent when due", read)
		}
	}
	got := w.intent(ticket, intent.ID)
	if got.State != model.EffectUnknown || !strings.Contains(got.LastError, "502") || !strings.Contains(got.LastError, fmt.Sprintf("not found in %d reads", absentReads)) {
		t.Fatalf("intent = %+v, want unknown naming the 502", got)
	}
	if elapsed := got.SettledAt.Sub(attempted); elapsed < absentWindow {
		t.Fatalf("settled %s after the attempt, want at least %s", elapsed, absentWindow)
	}
	w.clock.Advance(24 * time.Hour)
	w.repo.Advance(24 * time.Hour)
	w.drain()
	if n := len(w.requests(http.MethodPost, "/comments")); n != 1 {
		t.Fatalf("POSTs = %d, want the one write", n)
	}
}

// A restart between rereads starts the count again in the new process; the
// window still runs from the committed attempt time.
func TestAbsentReadsAcrossRestart(t *testing.T) {
	w := newWorld(t)
	thread := issueThread(w)
	ticket := w.launch(thread)
	w.writes.LoseAck(http.MethodPost, "/repos/"+repoName+"/issues/1/comments", http.StatusBadGateway, 1)
	w.repo.SetIndexDelay(time.Hour)
	intent := w.enqueue(ticket, "call-1", model.EffectComment, model.EffectPayload{Body: "Done."})
	if err := w.publish(); err != nil {
		t.Fatal(err)
	}
	w.clock.Advance(time.Hour)
	w.restart()
	for read := 1; read < absentReads; read++ {
		if err := w.publish(); err != nil {
			t.Fatal(err)
		}
		if got := w.intent(ticket, intent.ID); got.State != model.EffectAttempting {
			t.Fatalf("read %d after restart: %+v, want attempting", read, got)
		}
		w.clock.Advance(absentBackoff)
	}
	if err := w.publish(); err != nil {
		t.Fatal(err)
	}
	if got := w.intent(ticket, intent.ID); got.State != model.EffectUnknown || !strings.Contains(got.LastError, "process stopped") {
		t.Fatalf("intent = %+v, want unknown", got)
	}
}

// An operator resend publishes an unknown write again, once.
func TestResendPublishesUnknownWrite(t *testing.T) {
	w := newWorld(t)
	thread := issueThread(w)
	ticket := w.launch(thread)
	intent := w.enqueue(ticket, "call-1", model.EffectComment, model.EffectPayload{Body: "hello"})
	crashAt(w, "attempted")
	w.restart()
	w.reread()
	if got := w.intent(ticket, intent.ID); got.State != model.EffectUnknown {
		t.Fatalf("intent = %+v, want unknown", got)
	}
	if err := w.store.Operator().ResolveEffect(w.ctx, model.OperatorRequest{ID: "resend-1", Target: string(intent.ID), Reason: "not on GitHub"}, model.EffectPending); err != nil {
		t.Fatal(err)
	}
	w.drain()
	got := w.intent(ticket, intent.ID)
	if got.State != model.EffectConfirmed || got.Result == nil || got.Result.Reconciled {
		t.Fatalf("intent = %+v, want confirmed by the resend", got)
	}
	if posted := w.marked(thread.Number, intent.ID); len(posted) != 1 {
		t.Fatalf("comments = %+v, want one", posted)
	}
	w.clock.Advance(24 * time.Hour)
	w.drain()
	if n := len(w.requests(http.MethodPost, "/comments")); n != 1 {
		t.Fatalf("POSTs = %d, want 1", n)
	}
}
