package irc_test

import (
	"testing"

	"go.harness.dev/harness/internal/irc"
	"go.harness.dev/harness/internal/session"
)

func TestMailboxReopensWhenStopHookContinues(t *testing.T) {
	bus := irc.New(nil, nil)
	parent := bus.Register(irc.Parent, session.NewDurableQueues(store(t), nil, nil))
	queue := session.NewDurableQueues(store(t), nil, nil)
	child := bus.Register("job_child", queue)
	if child.Idle() {
		t.Fatal("empty mailbox wasn't sealed")
	}
	child.Reopen()
	receipt, err := parent.Send(call("after-stop"), "job_child", "finish the remaining work", 0)
	if err != nil || len(receipt.DeliveredTo) != 1 || len(queue.Snapshot()) != 1 {
		t.Fatalf("continued child mailbox remained closed: receipt=%+v err=%v", receipt, err)
	}
}
