package tempest

import (
	"context"

	"go.harness.dev/harness/internal/checkpoint"
)

// Checkpoint identifies a retained workspace and conversation snapshot.
type Checkpoint = checkpoint.Info

// RewindOptions selects a checkpoint; an empty ID selects the newest explicit
// checkpoint, or the newest automatic checkpoint when none are explicit.
// Force permits outside edits, but never bypasses capture gaps or path/job safety.
type RewindOptions struct {
	Checkpoint string `json:"checkpoint,omitempty"`
	Force      bool   `json:"force,omitempty"`
	Report     string `json:"report,omitempty"`
}

// Checkpoints lists retained snapshots, oldest first.
func (s *Session) Checkpoints() []Checkpoint { return s.core.checkpoints.List() }

// CreateCheckpoint saves files and the current idle conversation.
func (s *Session) CreateCheckpoint(ctx context.Context, goal string) (Checkpoint, error) {
	c := s.core
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return Checkpoint{}, ErrClosed
	}
	if c.active != nil {
		return Checkpoint{}, ErrRunActive
	}
	if err := c.checkpoints.CheckIdle(ctx); err != nil {
		return Checkpoint{}, err
	}
	return c.checkpoints.Create(ctx, goal, c.agent.State().Messages)
}

// Rewind restores workspace files and forks the conversation in the append-only journal.
// The session must be idle and its jobs and children must have settled.
func (s *Session) Rewind(ctx context.Context, o RewindOptions) error {
	c := s.core
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	if c.active != nil {
		return ErrRunActive
	}
	if err := c.checkpoints.CheckIdle(ctx); err != nil {
		return err
	}
	messages, err := c.checkpoints.Rewind(ctx, o.Checkpoint, o.Report, o.Force)
	if err != nil {
		return err
	}
	c.checkpoints.Install(messages)
	c.lastText = ""
	return nil
}
