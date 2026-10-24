package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	types "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/work"
	"go.harness.dev/harness/tempest/durable"
)

var (
	// ErrQueueFull reports either pending-user-item or encoded-byte capacity.
	ErrQueueFull = errors.New("queue full")
	// ErrQueueConflict reports an idempotency key reused for different content.
	ErrQueueConflict = errors.New("queue key conflict")
	// ErrQueueNotFound reports an unknown queue receipt.
	ErrQueueNotFound = errors.New("queue item not found")
	// ErrQueueTooLate reports an already claimed or committed queue message.
	ErrQueueTooLate = errors.New("queue item already claimed or consumed")
	// ErrInvalidQueueInput reports malformed text, kind or idempotency key.
	ErrInvalidQueueInput = errors.New("invalid queue input")
)

// DurableQueues owns durable acceptance receipts and claims one FIFO head per checkpoint.
type DurableQueues struct {
	mutation  sync.Mutex
	mu        sync.Mutex
	store     *journal.Store
	publish   func(context.Context, work.Event) error
	items     map[string]work.QueueItem
	keys      map[string]string
	pending   []string
	cancelled map[string]bool
	consumed  map[string]bool
	claimed   map[string]bool
	changed   chan struct{}
}

var _ work.QueueSource = (*DurableQueues)(nil)

// NewDurableQueues restores pending items and lifetime idempotency receipts.
func NewDurableQueues(store *journal.Store, loaded *journal.Loaded, publish func(context.Context, work.Event) error) *DurableQueues {
	q := &DurableQueues{store: store, publish: publish, items: map[string]work.QueueItem{}, keys: map[string]string{}, cancelled: map[string]bool{}, consumed: map[string]bool{}, claimed: map[string]bool{}, changed: make(chan struct{})}
	if loaded != nil {
		for _, item := range loaded.QueueReceipts {
			q.items[item.ID] = item
			if item.Key != "" {
				q.keys[item.Key] = item.ID
			}
		}
		for _, item := range loaded.Queued {
			q.pending = append(q.pending, item.ID)
		}
		for id, v := range loaded.QueueCancelled {
			q.cancelled[id] = v
		}
		for id, v := range loaded.QueueConsumed {
			q.consumed[id] = v
		}
	}
	return q
}
func (q *DurableQueues) notifyLocked() { close(q.changed); q.changed = make(chan struct{}) }

// Enqueue fsyncs acceptance before making the item visible or returning its receipt.
func (q *DurableQueues) Enqueue(ctx context.Context, input work.QueueInput) (work.QueueItem, error) {
	return q.EnqueuePrompt(ctx, input, nil)
}

// EnqueuePrompt shares admission with Enqueue while preserving source blocks.
func (q *DurableQueues) EnqueuePrompt(ctx context.Context, input work.QueueInput, original json.RawMessage) (work.QueueItem, error) {
	if (input.Kind != work.Steer && input.Kind != work.FollowUp) || !utf8.ValidString(input.Text) || strings.TrimSpace(input.Text) == "" || !utf8.ValidString(input.Key) || len(input.Key) > 128 {
		return work.QueueItem{}, ErrInvalidQueueInput
	}
	switch {
	case input.SourceAgent != "":
		if input.SourceJobID != "" || input.Kind != work.Steer || len(input.Text) > 16<<10 || !strings.HasPrefix(input.Key, work.MessageKeyPrefix) {
			return work.QueueItem{}, ErrInvalidQueueInput
		}
	case input.SourceJobID == "":
		if len(input.Text) > 16<<10 || strings.HasPrefix(input.Key, "job:") || strings.HasPrefix(input.Key, work.MessageKeyPrefix) {
			return work.QueueItem{}, ErrInvalidQueueInput
		}
	case input.Kind != work.FollowUp || input.Key != "job:"+input.SourceJobID+":terminal":
		return work.QueueItem{}, ErrInvalidQueueInput
	}
	requestID, namespace := input.Key, durable.ClientNamespace(ctx)
	if input.SourceAgent != "" || input.SourceJobID != "" {
		namespace = ""
	}
	if len(namespace) > 128 {
		return work.QueueItem{}, ErrInvalidQueueInput
	}
	if input.Key != "" && input.SourceAgent == "" && input.SourceJobID == "" {
		input.Key = durable.SubmissionKey(namespace, input.Key)
	}
	q.mutation.Lock()
	defer q.mutation.Unlock()
	if err := ctx.Err(); err != nil {
		return work.QueueItem{}, err
	}
	if len(original) == 0 {
		original, _ = json.Marshal(struct {
			Text            string
			Media           json.RawMessage
			OutputSchema    json.RawMessage `json:",omitempty"`
			ContentIdentity json.RawMessage
		}{Text: input.Text})
	}
	if input.Key != "" {
		page, err := q.store.Read(ctx, durable.Query{Kind: durable.SubmissionKind, Namespace: "_tempest/submissions", ID: input.Key, Limit: 1})
		if err != nil {
			return work.QueueItem{}, err
		}
		if len(page.Records) > 0 {
			var prior durable.Submission
			if err = json.Unmarshal(page.Records[0].Value, &prior); err != nil {
				return work.QueueItem{}, err
			}
			hash, err := durable.SubmissionHash(string(input.Kind), original)
			if err != nil {
				return work.QueueItem{}, err
			}
			if prior.RequestHash != hash {
				return work.QueueItem{}, ErrQueueConflict
			}
		}
	}
	q.mu.Lock()
	if id, ok := q.keys[input.Key]; input.Key != "" && ok {
		prior := q.items[id]
		q.mu.Unlock()
		if prior.Kind != input.Kind || prior.Text != input.Text || prior.SourceJobID != input.SourceJobID || prior.SourceAgent != input.SourceAgent {
			return work.QueueItem{}, ErrQueueConflict
		}
		return prior, nil
	}
	item := work.QueueItem{ID: q.store.NewRunID(), Kind: input.Kind, Text: input.Text, Key: input.Key, SourceJobID: input.SourceJobID, SourceAgent: input.SourceAgent, EnqueuedAt: time.Now()}
	encoded, _ := json.Marshal(item)
	count, total, messages := 0, len(encoded), 0
	for _, id := range q.pending {
		v := q.items[id]
		switch {
		case v.SourceAgent != "":
			messages++
		case v.SourceJobID == "":
			count++
			b, _ := json.Marshal(v)
			total += len(b)
		}
	}
	if input.SourceAgent != "" && messages >= work.MailboxCap {
		q.mu.Unlock()
		return work.QueueItem{}, fmt.Errorf("%w: %d unread messages, the mailbox limit", ErrQueueFull, messages)
	}
	if input.SourceAgent == "" && input.SourceJobID == "" && (count >= 32 || total > 256<<10) {
		q.mu.Unlock()
		return work.QueueItem{}, ErrQueueFull
	}
	if input.SourceJobID != "" && len(encoded) > 8<<10 {
		q.mu.Unlock()
		return work.QueueItem{}, fmt.Errorf("%w: completion exceeds encoded 8 KiB", ErrInvalidQueueInput)
	}
	q.mu.Unlock()
	raw, _ := json.Marshal(journal.QueueEnqueued{QueueID: item.ID, Kind: item.Kind, Text: item.Text, Key: item.Key, SourceJobID: item.SourceJobID, SourceAgent: item.SourceAgent, Prompt: original, RequestID: requestID, ClientNamespace: namespace})
	meta, err := q.store.AppendAssigned(ctx, journal.WorkRecord{Type: journal.KindQueueEnqueued, Payload: raw})
	if err != nil {
		return work.QueueItem{}, err
	}
	item.EnqueuedAt = meta.Timestamp.UTC()
	q.mu.Lock()
	q.items[item.ID] = item
	if item.Key != "" {
		q.keys[item.Key] = item.ID
	}
	q.pending = append(q.pending, item.ID)
	q.notifyLocked()
	q.mu.Unlock()
	if q.publish != nil {
		if err := q.publish(context.WithoutCancel(ctx), work.Event{Kind: "queue", Queue: &item, Action: "enqueued"}); err != nil {
			return item, err
		}
	}
	return item, nil
}

// Cancel durably withdraws an unclaimed item; known cancellations are idempotent.
func (q *DurableQueues) Cancel(ctx context.Context, id string) error {
	q.mutation.Lock()
	defer q.mutation.Unlock()
	q.mu.Lock()
	item, exists := q.items[id]
	if !exists {
		q.mu.Unlock()
		return ErrQueueNotFound
	}
	if q.cancelled[id] {
		q.mu.Unlock()
		return nil
	}
	if q.consumed[id] || q.claimed[id] {
		q.mu.Unlock()
		return ErrQueueTooLate
	}
	q.mu.Unlock()
	raw, _ := json.Marshal(journal.QueueCancelled{QueueID: id})
	if err := q.store.AppendWorkFact(ctx, work.Fact{Kind: string(journal.KindQueueCancelled), Payload: raw}); err != nil {
		return err
	}
	q.mu.Lock()
	q.cancelled[id] = true
	q.removePendingLocked(id)
	q.notifyLocked()
	q.mu.Unlock()
	if q.publish != nil {
		return q.publish(context.WithoutCancel(ctx), work.Event{Kind: "queue", Queue: &item, Action: "cancelled"})
	}
	return nil
}
func (q *DurableQueues) removePendingLocked(id string) {
	for i, v := range q.pending {
		if v == id {
			q.pending = append(q.pending[:i], q.pending[i+1:]...)
			return
		}
	}
}

// Take claims one head. Only the recorder's committed message removes it.
func (q *DurableQueues) Take(ctx context.Context, kind work.QueueKind) ([]work.QueuedMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	q.mutation.Lock()
	defer q.mutation.Unlock()
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, id := range q.pending {
		item := q.items[id]
		if item.Kind != kind {
			continue
		}
		if q.claimed[id] {
			return nil, nil
		}
		q.claimed[id] = true
		return []work.QueuedMessage{{ID: id, Message: types.UserMessage{Content: types.StringContent(item.Text), Timestamp: item.EnqueuedAt.UnixMilli()}}}, nil
	}
	return nil, nil
}

// ReleaseClaims restores uncommitted claims synchronously at a run boundary.
func (q *DurableQueues) ReleaseClaims() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.claimed) == 0 {
		return
	}
	clear(q.claimed)
	q.notifyLocked()
}

// Acknowledge removes an item only after its consuming message has committed.
func (q *DurableQueues) Acknowledge(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.items[id]; !ok {
		return ErrQueueNotFound
	}
	if q.cancelled[id] || q.consumed[id] {
		return ErrQueueTooLate
	}
	q.consumed[id] = true
	delete(q.claimed, id)
	q.removePendingLocked(id)
	q.notifyLocked()
	return nil
}

// Snapshot returns accepted, unconsumed items in acceptance order.
func (q *DurableQueues) Snapshot() []work.QueueItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]work.QueueItem, 0, len(q.pending))
	for _, id := range q.pending {
		out = append(out, q.items[id])
	}
	return out
}

// Pending captures its predicate and notification channel under the same lock.
func (q *DurableQueues) Pending() (bool, <-chan struct{}) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending) > 0, q.changed
}

// PendingJobIDs includes claimed completions until their consuming message commits.
func (q *DurableQueues) PendingJobIDs() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	var ids []string
	for _, id := range q.pending {
		if jobID := q.items[id].SourceJobID; jobID != "" {
			ids = append(ids, jobID)
		}
	}
	return ids
}
