package jobs

import (
	"context"
	"go.harness.dev/harness/internal/work"
)

// OwnedTasksSettled includes joined native children in phase-owner completion.
func (manager *Manager) OwnedTasksSettled(owner string) bool {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	for _, record := range manager.entries {
		if record.recovery.Job.OwnerTask == owner && (!terminal(record.recovery.Job.State) || !record.joined) {
			return false
		}
	}
	return true
}

// AbortOwnedTasks uses live supervisor handles, never persisted process IDs.
func (manager *Manager) AbortOwnedTasks(ctx context.Context, owner string) error {
	manager.mu.Lock()
	var records []*entry
	for _, record := range manager.entries {
		if record.recovery.Job.OwnerTask == owner && (!terminal(record.recovery.Job.State) || !record.joined) {
			records = append(records, record)
		}
	}
	manager.mu.Unlock()
	for _, record := range records {
		if _, err := manager.cancelRecord(ctx, record, "owner_aborted"); err != nil {
			return err
		}
	}
	for _, record := range records {
		select {
		case <-record.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// PhaseRun returns the current logical owner and whether it still admits work.
func (manager *Manager) PhaseRun() (work.RunInfo, bool) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.run, !manager.sealed && !manager.finished && manager.scope != nil && manager.scope.Err() == nil
}

// Changes wakes phase owners after native admission, completion or join.
func (manager *Manager) Changes() <-chan struct{} {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.changed
}
