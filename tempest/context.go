package tempest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.harness.dev/harness/internal/compaction"
	"go.harness.dev/harness/internal/conversation"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/tempest/durable"
)

// CompactionSettings sets per-model reserve, recent tail and speculative budgets.
type CompactionSettings = compaction.CompactionSettings

// HistoryOptions selects a literal, paged archive search.
type HistoryOptions = journal.HistoryOptions

// HistoryPage includes entries hidden from the active context.
type HistoryPage = journal.HistoryPage

// HistoryMatch identifies an immutable audit entry.
type HistoryMatch = journal.HistoryMatch

// SearchHistory reads audit text under the host's ordinary read authority.
func (s *Session) SearchHistory(ctx context.Context, options HistoryOptions) (HistoryPage, error) {
	if err := s.authorize(ctx, "search_history", "", ""); err != nil {
		return HistoryPage{}, err
	}
	return s.core.store.SearchHistory(ctx, options)
}

// Reset admits an ordered context change. Busy runs place it after their tool batch.
// It doesn't create a user message, withdraw submissions or erase lifetime spend.
func (s *Session) Reset(ctx context.Context, note, requestID string) (Submission, error) {
	if err := s.authorize(ctx, "reset", "", requestID); err != nil {
		return Submission{}, err
	}
	if requestID == "" || len(requestID) > 128 || strings.ContainsRune(requestID, 0) {
		return Submission{}, ErrInvalidQueueInput
	}
	ctx, err := s.submissionContext(ctx)
	if err != nil {
		return Submission{}, err
	}
	key := durable.SubmissionKey(durable.ClientNamespace(ctx), requestID)
	raw, err := json.Marshal(note)
	if err != nil {
		return Submission{}, err
	}
	hash, err := durable.SubmissionHash("reset", raw)
	if err != nil {
		return Submission{}, err
	}
	c := s.core
	c.admission.Lock()
	defer c.admission.Unlock()
	prior, err := s.submissionRecord(ctx, key)
	if err == nil {
		if prior.RequestHash != hash {
			return Submission{}, durable.ErrConflict
		}
		if err := s.applyIdleContext(context.WithoutCancel(ctx)); err != nil {
			return Submission{}, err
		}
		stored, err := s.submissionRecord(ctx, key)
		return publicSubmission(stored), err
	}
	if !errors.Is(err, ErrQueueNotFound) {
		return Submission{}, err
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return Submission{}, ErrClosed
	}
	receipt := durable.Submission{ID: c.store.NewRunID(), RequestID: requestID, ClientNamespace: durable.ClientNamespace(ctx), State: "accepted", Placement: "reset", Payload: raw, RequestHash: hash}
	if err := s.saveSubmission(ctx, receipt, "admit:"+key); err != nil {
		return Submission{}, err
	}
	if err := s.applyIdleContext(context.WithoutCancel(ctx)); err != nil {
		return Submission{}, err
	}
	stored, err := s.submissionRecord(ctx, key)
	return publicSubmission(stored), err
}

func (s *Session) applyIdleContext(ctx context.Context) error {
	c := s.core
	c.mu.Lock()
	busy := c.active != nil
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if busy {
		return nil
	}
	changed, err := c.store.ApplyPendingContext(ctx)
	if err != nil {
		return err
	}
	if changed {
		c.agent.SetMessages(nil)
		if c.contextChanged != nil {
			c.contextChanged(nil)
		}
		s.publishDurable(ctx)
	}
	return nil
}

// CompactOptions controls whether the caller waits for a native summary phase.
type CompactOptions struct {
	Wait bool `json:"wait,omitempty"`
}

// Compact coalesces with existing work. Cancelling the wait doesn't cancel work.
func (s *Session) Compact(ctx context.Context, options CompactOptions) (Task, error) {
	if err := s.authorize(ctx, "compact", "", ""); err != nil {
		return Task{}, err
	}
	c := s.core
	c.admission.Lock()
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		c.admission.Unlock()
		return Task{}, ErrClosed
	}
	if c.compaction == nil {
		c.admission.Unlock()
		return Task{}, fmt.Errorf("compaction unavailable for this model")
	}
	messages := c.agent.State().Messages
	var boundary conversation.Boundary
	safe := 0
	for index, message := range messages {
		if boundary.Add(message) {
			safe = index + 1
		}
	}
	if safe == 0 {
		c.admission.Unlock()
		return Task{}, fmt.Errorf("no complete transcript prefix to compact")
	}
	task, err := c.compaction.Request(ctx, messages[:safe])
	c.admission.Unlock()
	if err != nil {
		return Task{}, err
	}
	c.compaction.Start(task.ID)
	if !options.Wait {
		return task, nil
	}
	task, err = c.compaction.Wait(ctx, task.ID)
	if err != nil {
		return Task{}, err
	}
	c.admission.Lock()
	defer c.admission.Unlock()
	c.mu.Lock()
	busy := c.active != nil
	closed = c.closed
	c.mu.Unlock()
	if closed {
		return task, ErrClosed
	}
	if !busy {
		replacement, changed, err := c.compaction.Place(ctx, c.agent.State().Messages, task)
		if err != nil {
			return task, err
		}
		if changed {
			c.agent.SetMessages(replacement)
			if c.contextChanged != nil {
				c.contextChanged(replacement)
			}
		}
	}
	return task, nil
}
