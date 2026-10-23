package irc_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/irc"
	"go.harness.dev/harness/internal/session"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/work"
)

func store(t *testing.T) *journal.Store {
	t.Helper()
	s, err := journal.Create(context.Background(), t.TempDir(), "/tmp/irc", journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func call(id string) context.Context {
	return work.WithInvocation(context.Background(), work.Invocation{RunID: "run", CallID: id, ToolCallID: "raw"})
}

type recorder struct {
	mu     sync.Mutex
	events []work.AgentMessage
}

func (r *recorder) publish(_ context.Context, event work.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if event.Kind == "message" && event.Message != nil {
		r.events = append(r.events, *event.Message)
	}
	return nil
}

func (r *recorder) snapshot() []work.AgentMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]work.AgentMessage(nil), r.events...)
}

// consume takes and acknowledges every pending steering item, as the agent
// loop and journal recorder do at a step boundary.
func consume(t *testing.T, q *session.DurableQueues) []work.QueuedMessage {
	t.Helper()
	var all []work.QueuedMessage
	for {
		taken, err := q.Take(context.Background(), work.Steer)
		if err != nil {
			t.Fatal(err)
		}
		if len(taken) == 0 {
			return all
		}
		for _, message := range taken {
			if err := q.Acknowledge(message.ID); err != nil {
				t.Fatal(err)
			}
		}
		all = append(all, taken...)
	}
}

func TestParentMessageIsOneDurableAsideAcrossReplayAndRecovery(t *testing.T) {
	events := &recorder{}
	bus := irc.New(events.publish, nil)
	parentStore, childStore := store(t), store(t)
	parent := bus.Register(irc.Parent, session.NewDurableQueues(parentStore, nil, nil))
	childQueue := session.NewDurableQueues(childStore, nil, nil)
	bus.Register("job_a", childQueue)

	receipt, err := parent.Send(call("call-1"), "job_a", "stop editing main.go", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt.DeliveredTo) != 1 || receipt.DeliveredTo[0] != "job_a" || receipt.Reply != nil {
		t.Fatalf("receipt=%+v", receipt)
	}
	pending := childQueue.Snapshot()
	if len(pending) != 1 || pending[0].Kind != work.Steer || pending[0].SourceAgent != irc.Parent {
		t.Fatalf("child mailbox=%+v", pending)
	}
	if text := pending[0].Text; !strings.HasPrefix(text, "[Message from parent]") || !strings.Contains(text, "stop editing main.go") || !strings.Contains(text, `message {"to":"parent"`) {
		t.Fatalf("rendered message=%q", text)
	}
	// A replayed tool occurrence reuses its idempotency key.
	if again, err := parent.Send(call("call-1"), "job_a", "stop editing main.go", 0); err != nil || again.MessageID != receipt.MessageID {
		t.Fatalf("replay=%+v %v", again, err)
	}
	if got := len(childQueue.Snapshot()); got != 1 {
		t.Fatalf("replay duplicated the message: %d pending", got)
	}

	// Recovery rebuilds the child's mailbox from its journal; the pending
	// message comes back once and a replay still does not duplicate it.
	loaded, err := journal.Load(context.Background(), childStore.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Queued) != 1 || loaded.Queued[0].SourceAgent != irc.Parent {
		t.Fatalf("journaled mailbox=%+v", loaded.Queued)
	}
	recovered := session.NewDurableQueues(childStore, loaded, nil)
	bus.Register("job_a", recovered)
	if _, err := parent.Send(call("call-1"), "job_a", "stop editing main.go", 0); err != nil {
		t.Fatal(err)
	}
	if read := consume(t, recovered); len(read) != 1 {
		t.Fatalf("recovered mailbox delivered %d messages", len(read))
	}
	// Once read, a replay is still the same receipt and nothing new arrives.
	if _, err := parent.Send(call("call-1"), "job_a", "stop editing main.go", 0); err != nil {
		t.Fatal(err)
	}
	if read := consume(t, recovered); len(read) != 0 {
		t.Fatalf("replay after consumption redelivered %d messages", len(read))
	}
	// A different occurrence is a new message.
	if _, err := parent.Send(call("call-2"), "job_a", "stop editing main.go", 0); err != nil {
		t.Fatal(err)
	}
	if read := consume(t, recovered); len(read) != 1 {
		t.Fatalf("new occurrence delivered %d messages", len(read))
	}
	if got := events.snapshot(); len(got) == 0 || got[0].From != irc.Parent || got[0].To != "job_a" || got[0].Delivery != "mailbox" || got[0].Body != "stop editing main.go" {
		t.Fatalf("events=%+v", got)
	}
}

func TestAwaitReplyReturnsThePeersMessageInsteadOfAnAside(t *testing.T) {
	events := &recorder{}
	bus := irc.New(events.publish, nil)
	parentQueue := session.NewDurableQueues(store(t), nil, nil)
	childQueue := session.NewDurableQueues(store(t), nil, nil)
	parent := bus.Register(irc.Parent, parentQueue)
	child := bus.Register("job_a", childQueue)

	// The parent replies once the child's question is in its mailbox.
	replied := make(chan error, 1)
	go func() {
		for {
			if len(parentQueue.Snapshot()) == 1 {
				_, err := parent.Send(call("answer"), "job_a", "use port 8080", 0)
				replied <- err
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	receipt, err := child.Send(call("question"), irc.Parent, "which port?", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-replied; err != nil {
		t.Fatal(err)
	}
	if receipt.Reply == nil || receipt.Reply.From != irc.Parent || receipt.Reply.Body != "use port 8080" || receipt.ReplyTimedOut {
		t.Fatalf("receipt=%+v", receipt)
	}
	if pending := childQueue.Snapshot(); len(pending) != 0 {
		t.Fatalf("awaited reply also queued as an aside: %+v", pending)
	}
	if read := consume(t, parentQueue); len(read) != 1 {
		t.Fatalf("parent mailbox=%d", len(read))
	}
	last := events.snapshot()
	if len(last) != 2 || last[1].Delivery != "reply" {
		t.Fatalf("events=%+v", last)
	}

	// A peer that stays silent times the await out; the message itself is delivered.
	started := time.Now()
	receipt, err = child.Send(call("question-2"), irc.Parent, "still there?", 150*time.Millisecond)
	if err != nil || !receipt.ReplyTimedOut || receipt.Reply != nil || len(receipt.DeliveredTo) != 1 {
		t.Fatalf("timeout receipt=%+v %v", receipt, err)
	}
	if elapsed := time.Since(started); elapsed < 150*time.Millisecond {
		t.Fatalf("await returned after %v", elapsed)
	}
	// A reply after the timeout is an ordinary aside, not lost.
	if _, err := parent.Send(call("late"), "job_a", "yes", 0); err != nil {
		t.Fatal(err)
	}
	if pending := childQueue.Snapshot(); len(pending) != 1 || !strings.Contains(pending[0].Text, "yes") {
		t.Fatalf("late reply=%+v", pending)
	}
	if _, err := parent.Send(call("bad"), irc.All, "x", time.Second); !errors.Is(err, irc.ErrInvalidMessage) {
		t.Fatalf("broadcast await=%v", err)
	}
}

func TestMailboxCapRejectsWithoutTouchingUserQueue(t *testing.T) {
	bus := irc.New(nil, nil)
	childQueue := session.NewDurableQueues(store(t), nil, nil)
	parent := bus.Register(irc.Parent, session.NewDurableQueues(store(t), nil, nil))
	bus.Register("job_a", childQueue)
	for i := range work.MailboxCap {
		if _, err := parent.Send(call(fmt.Sprint(i)), "job_a", "note", 0); err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
	}
	_, err := parent.Send(call("over"), "job_a", "note", 0)
	if !errors.Is(err, irc.ErrUnavailable) || !strings.Contains(err.Error(), "mailbox limit") {
		t.Fatalf("over cap=%v", err)
	}
	// Messages have their own cap; user input still fits.
	if _, err := childQueue.Enqueue(context.Background(), work.QueueInput{Kind: work.FollowUp, Text: "user follow-up"}); err != nil {
		t.Fatal(err)
	}
	// Reading frees room.
	taken := consume(t, childQueue)
	if len(taken) != work.MailboxCap {
		t.Fatalf("consumed %d", len(taken))
	}
	if _, err := parent.Send(call("after"), "job_a", "note", 0); err != nil {
		t.Fatal(err)
	}
}

func TestChildIdleSealsMailboxOnlyWhenNothingIsPending(t *testing.T) {
	bus := irc.New(nil, func(id string) string {
		if id == "job_done" {
			return "job_done is completed"
		}
		return ""
	})
	parent := bus.Register(irc.Parent, session.NewDurableQueues(store(t), nil, nil))
	childQueue := session.NewDurableQueues(store(t), nil, nil)
	child := bus.Register("job_a", childQueue)
	if _, err := parent.Send(call("1"), "job_a", "one more thing", 0); err != nil {
		t.Fatal(err)
	}
	if !child.Idle() {
		t.Fatal("a pending message must give the child another turn")
	}
	consume(t, childQueue)
	if child.Idle() {
		t.Fatal("an empty mailbox must let the child finish")
	}
	_, err := parent.Send(call("2"), "job_a", "too late", 0)
	if !errors.Is(err, irc.ErrUnavailable) {
		t.Fatalf("sealed send=%v", err)
	}
	if len(childQueue.Snapshot()) != 0 {
		t.Fatal("a sealed mailbox accepted a message")
	}
	if _, err := parent.Send(call("3"), "job_done", "x", 0); err == nil || !strings.Contains(err.Error(), "job_done is completed") {
		t.Fatalf("finished recipient=%v", err)
	}
	// A resumed child reopens its mailbox.
	bus.Register("job_a", childQueue)
	if _, err := parent.Send(call("4"), "job_a", "welcome back", 0); err != nil {
		t.Fatal(err)
	}
}

func TestBroadcastAndAddressing(t *testing.T) {
	bus := irc.New(nil, nil)
	parentQueue := session.NewDurableQueues(store(t), nil, nil)
	parent := bus.Register(irc.Parent, parentQueue)
	queues := map[string]*session.DurableQueues{}
	endpoints := map[string]*irc.Endpoint{}
	for _, id := range []string{"job_a", "job_b", "job_c"} {
		queues[id] = session.NewDurableQueues(store(t), nil, nil)
		endpoints[id] = bus.Register(id, queues[id])
	}
	endpoints["job_c"].Close()

	receipt, err := parent.Send(call("p"), irc.All, "status?", 0)
	if err != nil || strings.Join(receipt.DeliveredTo, ",") != "job_a,job_b" {
		t.Fatalf("parent broadcast=%+v %v", receipt, err)
	}
	if len(parentQueue.Snapshot()) != 0 {
		t.Fatal("broadcast reached its sender")
	}
	// From a child, all is the parent plus running siblings.
	receipt, err = endpoints["job_a"].Send(call("a"), irc.All, "taking src/", 0)
	if err != nil || strings.Join(receipt.DeliveredTo, ",") != "parent,job_b" {
		t.Fatalf("child broadcast=%+v %v", receipt, err)
	}
	sibling := queues["job_b"].Snapshot()
	if len(sibling) != 2 || !strings.HasPrefix(sibling[1].Text, "[Message from job_a (sibling)]") {
		t.Fatalf("sibling mailbox=%+v", sibling)
	}
	if fromChild := parentQueue.Snapshot(); len(fromChild) != 1 || !strings.HasPrefix(fromChild[0].Text, "[Message from job_a (child)]") || !strings.Contains(fromChild[0].Text, `message {"to":"job_a"`) {
		t.Fatalf("parent mailbox=%+v", fromChild)
	}
	for _, test := range []struct{ to, body, want string }{
		{"job_a", "", "body is required"},
		{"", "x", "to is required"},
		{"parent", strings.Repeat("x", irc.MaxBodyBytes+1), "limit"},
		{"job_c", "x", "Running peers: job_a, job_b"},
		{"job_zz", "x", `no agent "job_zz"`},
	} {
		if _, err := parent.Send(call("bad"), test.to, test.body, 0); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("send to %q: %v, want %q", test.to, err, test.want)
		}
	}
	if _, err := endpoints["job_a"].Send(call("self"), "job_a", "x", 0); err == nil {
		t.Error("self message accepted")
	}
}
