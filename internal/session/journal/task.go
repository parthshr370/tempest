package journal

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"go.harness.dev/harness/internal/taskstate"
	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/extension"
)

// nativeTaskChanges commits the native adapter projection with its existing job
// receipt. The jobs supervisor still owns execution, limits and no-replay policy.
func (s *Store) nativeTaskChanges(ctx context.Context, raw []byte) ([]durable.Change, error) {
	entry, err := decodeEntry(raw)
	if err != nil {
		return nil, err
	}
	fact, ok := entry.(WorkRecord)
	if !ok {
		return nil, nil
	}
	if fact.Type != KindJobStarted && fact.Type != KindJobRunning && fact.Type != KindJobCancelRequested && fact.Type != KindJobFinished {
		return nil, nil
	}
	var identity struct {
		JobID string `json:"job_id"`
	}
	if err = json.Unmarshal(fact.Payload, &identity); err != nil {
		return nil, err
	}
	page, err := s.durable.Read(ctx, durable.Query{SessionID: s.id, Kind: durable.TaskKind, Namespace: "_tempest/native", ID: identity.JobID, Limit: 1})
	if err != nil {
		return nil, err
	}
	task := extension.Task{ID: identity.JobID, SessionID: s.id, Version: 1, CodeRevision: "native-v1", Phase: "execute", RunID: fact.RunID, Status: "runnable", Revision: 1}
	if len(page.Records) > 0 {
		if err = json.Unmarshal(page.Records[0].Value, &task); err != nil {
			return nil, err
		}
		task.Revision = page.Records[0].Revision + 1
	}
	switch fact.Type {
	case KindJobStarted:
		var started JobStarted
		if err = json.Unmarshal(fact.Payload, &started); err != nil {
			return nil, err
		}
		task.Definition = "builtin/" + started.Kind
		task.Owner = started.OwnerTask
		task.Input = started.Spec
		task.Checkpoint = json.RawMessage(`null`)
	case KindJobRunning:
		task.Status = "running"
		task.Attempt++
	case KindJobCancelRequested:
		task.Status = "abort_requested"
	case KindJobFinished:
		var finished JobFinished
		if err = json.Unmarshal(fact.Payload, &finished); err != nil {
			return nil, err
		}
		task.Status = finished.State
		task.Reason = finished.Reason
		task.Result = finished.Value
	}
	value, err := json.Marshal(task)
	if err != nil {
		return nil, err
	}
	return []durable.Change{{SessionID: s.id, Kind: durable.TaskKind, Namespace: "_tempest/native", ID: task.ID, Version: 1, Value: value}}, nil
}

// TaskWake reads task control facts without acquiring a writer or executing code.
// This is an archive scan; live sessions use the supervisor's reduced graph.
func TaskWake(ctx context.Context, path string, now time.Time) (wake time.Time, present bool, err error) {
	file, err := os.Open(path)
	if err != nil {
		return wake, false, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	reader := bufio.NewReader(file)
	headerBytes, err := reader.ReadBytes('\n')
	if err != nil {
		return wake, false, err
	}
	var header Header
	if err = json.Unmarshal(headerBytes, &header); err != nil {
		return wake, false, err
	}
	if header.Version != 4 {
		return wake, false, nil
	}
	tasks := make(map[string]extension.Task)
	for {
		if err = ctx.Err(); err != nil {
			return wake, false, err
		}
		line, readErr := reader.ReadBytes('\n')
		if readErr == io.EOF {
			break // A torn final commit was never acknowledged.
		}
		if readErr != nil {
			return wake, false, readErr
		}
		envelope, decodeErr := durable.DecodeEnvelope(path, line)
		if decodeErr != nil {
			return wake, false, decodeErr
		}
		for _, change := range envelope.Commit.Changes {
			if change.Kind != durable.TaskKind || change.Namespace != "_tempest/tasks" {
				continue
			}
			var task extension.Task
			if change.Version != 1 || json.Unmarshal(change.Value, &task) != nil || task.ID != change.ID || task.SessionID != header.ID {
				return wake, false, fmt.Errorf("%w: invalid task alarm", durable.ErrCorrupt)
			}
			tasks[task.ID] = task
		}
	}
	graph := make([]extension.Task, 0, len(tasks))
	for _, task := range tasks {
		graph = append(graph, task)
	}
	wake, present = taskstate.Alarm(graph, now)
	return wake, present, nil
}
