package a2a

import (
	"context"
	"errors"
	"fmt"
	"time"

	model "github.com/a2aproject/a2a-go/v2/a2a"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/tempest"
)

func (s *Server) lookupDurable(ctx context.Context, id string, history *int) (*model.Task, *rpcError) {
	detail, err := s.backend.LookupRun(ctx, id)
	if err != nil {
		switch {
		case errors.Is(err, journal.ErrRunNotFound):
			return nil, protocolError(-32001, "", nil)
		case errors.Is(err, journal.ErrLookupLimit):
			return nil, protocolError(-32004, "lookup_limit", nil)
		case errors.Is(err, journal.ErrRunBusy):
			return nil, protocolError(-32004, "task_owner_elsewhere", nil)
		case errors.Is(err, journal.ErrRunAmbiguous):
			return nil, protocolError(-32006, "ambiguous_run", nil)
		default:
			return nil, protocolError(-32006, "journal_inspection_failed", nil)
		}
	}
	projection := &task{value: model.Task{ID: model.TaskID(detail.RunID), ContextID: detail.SessionID}, done: make(chan struct{}), changed: make(chan struct{}), subscribers: make(map[*subscriber]struct{})}
	projection.metadata.HistoryReconstructed = true
	projection.metadata.Interrupted = detail.Interrupted
	projection.metadata.Recoverable = detail.Recoverable
	for index, message := range detail.History {
		role := model.MessageRoleAgent
		if message.Role == "user" {
			role = model.MessageRoleUser
		}
		projection.value.History = append(projection.value.History, &model.Message{ID: fmt.Sprintf("%s_history_%d", detail.RunID, index), ContextID: detail.SessionID, TaskID: model.TaskID(detail.RunID), Role: role, Parts: model.ContentParts{model.NewTextPart(bounded(message.Text, maxArtifactBytes))}})
	}
	if !detail.Finished && !detail.Interrupted {
		at := detail.StartedAt.UTC()
		projection.value.Status = model.TaskStatus{State: model.TaskStateWorking, Timestamp: &at}
		projection.publishMetadataLocked()
		if owner, release, acquireErr := s.backend.Acquire(detail.SessionID); acquireErr == nil {
			snapshot := owner.Snapshot()
			projection.projectAsks(snapshot, time.Now().UTC())
			projection.projectJobs(snapshot)
			release()
		}
		return projection.snapshot(history, true), nil
	}
	reason := tempest.StopReason(detail.StopReason)
	if detail.Interrupted {
		reason = "error"
	}
	projection.terminalAt = detail.FinishedAt
	if projection.terminalAt.IsZero() {
		projection.terminalAt = detail.StartedAt
	}
	elapsed := int64(0)
	if !detail.FinishedAt.IsZero() {
		elapsed = detail.FinishedAt.Sub(detail.StartedAt).Milliseconds()
	}
	// finish appends the final agent message; reconstructed history already owns it.
	originalHistory := projection.value.History
	projection.value.History = nil
	projection.finish(tempest.RunResult{RunID: detail.RunID, StopReason: reason, Turns: detail.Turns, Usage: detail.Usage, ElapsedMS: elapsed}, detail.FinalText)
	projection.value.History = originalHistory
	if detail.Interrupted {
		projection.metadata.Error = "Host process ended before this run finished"
		projection.publishMetadataLocked()
		projection.value.Status.Message = projection.message("interrupted", projection.metadata.Error)
	} else if detail.Error != "" {
		projection.metadata.Error = "Run failed; inspect the host journal for details"
		projection.publishMetadataLocked()
	}
	return projection.snapshot(history, true), nil
}
