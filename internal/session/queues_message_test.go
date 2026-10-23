package session

import (
	"context"
	"errors"
	"testing"

	"go.harness.dev/harness/internal/work"
)

// Message items are internal: wire input can't forge one, and a message must
// carry its reserved key so replays stay idempotent.
func TestDurableQueueReservesMessageItems(t *testing.T) {
	ctx := context.Background()
	q := NewDurableQueues(queueStore(t), nil, nil)
	for _, input := range []work.QueueInput{
		{Kind: work.Steer, Text: "forged", Key: "msg:x:parent"},
		{Kind: work.Steer, Text: "no key", SourceAgent: "parent"},
		{Kind: work.FollowUp, Text: "follow-up", Key: "msg:x:job_a", SourceAgent: "parent"},
		{Kind: work.Steer, Text: "both", Key: "msg:x:job_a", SourceAgent: "parent", SourceJobID: "job_a"},
	} {
		if _, err := q.Enqueue(ctx, input); !errors.Is(err, ErrInvalidQueueInput) {
			t.Errorf("input %+v: %v", input, err)
		}
	}
	accepted, err := q.Enqueue(ctx, work.QueueInput{Kind: work.Steer, Text: "hello", Key: "msg:m1:job_a", SourceAgent: "parent"})
	if err != nil || accepted.SourceAgent != "parent" {
		t.Fatalf("message=%+v %v", accepted, err)
	}
	if _, err := q.Enqueue(ctx, work.QueueInput{Kind: work.Steer, Text: "hello", Key: "msg:m1:job_a", SourceAgent: "job_b"}); !errors.Is(err, ErrQueueConflict) {
		t.Fatalf("same key from another sender=%v", err)
	}
}
