package jobs

import (
	"context"
	"errors"
	"fmt"

	"go.harness.dev/harness/internal/work"
	"go.harness.dev/harness/internal/worktree"
)

// ErrNoPatch reports a job with no exported change to integrate.
var ErrNoPatch = errors.New("job has no patch to integrate")

// ChildPatch returns a settled isolated task's verified patch and base.
func (manager *Manager) ChildPatch(ctx context.Context, jobID string) (work.ChildPatch, error) {
	if err := ctx.Err(); err != nil {
		return work.ChildPatch{}, err
	}
	record, err := manager.find(jobID)
	if err != nil {
		return work.ChildPatch{}, err
	}
	manager.mu.Lock()
	job := cloneJob(record.recovery.Job)
	settled := terminal(job.State) && (!record.live || record.joined)
	workspace := record.recovery.Workspace
	manager.mu.Unlock()
	switch {
	case job.Kind != "task":
		return work.ChildPatch{}, fmt.Errorf("%w: %s is a %s job; only general tasks in a worktree leave a patch", ErrNoPatch, jobID, job.Kind)
	case !settled:
		return work.ChildPatch{}, fmt.Errorf("%w: %s is still %s; wait for it, then integrate", ErrNoPatch, jobID, job.State)
	case workspace == nil:
		return work.ChildPatch{}, fmt.Errorf("%w: %s ran in the shared workspace, so its edits are already in place", ErrNoPatch, jobID)
	case job.Patch == nil:
		return work.ChildPatch{}, fmt.Errorf("%w: %s ended %s (%s) without exporting one", ErrNoPatch, jobID, job.State, job.Reason)
	}
	if err := worktree.Verify(*job.Patch); err != nil {
		return work.ChildPatch{}, fmt.Errorf("%s's patch artifact failed verification: %w", jobID, err)
	}
	return work.ChildPatch{Job: job, Repo: workspace.Repo, BaseCommit: workspace.BaseCommit, Patch: *job.Patch}, nil
}
