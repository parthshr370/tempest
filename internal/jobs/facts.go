package jobs

import (
	"context"
	"encoding/json"
	"time"

	"go.harness.dev/harness/internal/cloudcmd"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/work"
)

type ceilingPayload struct {
	Mode        string            `json:"mode"`
	Tools       []string          `json:"tools"`
	Overrides   map[string]string `json:"overrides,omitempty"`
	Interactive map[string]string `json:"interactive,omitempty"`
	// CloudRules is additive: older records omit it and restore no grants.
	CloudRules []cloudcmd.Rule `json:"cloud_rules,omitempty"`
}
type environmentPayload struct {
	Ceiling  ceilingPayload `json:"ceiling"`
	Model    string         `json:"model"`
	Provider string         `json:"provider"`
}
type workspacePayload struct {
	Repo        string `json:"repo"`
	CommonDir   string `json:"common_dir"`
	BaseCommit  string `json:"base_commit"`
	RelativeCwd string `json:"relative_cwd"`
	Path        string `json:"path"`
	Marker      string `json:"marker"`
}
type startedPayload struct {
	JobID          string              `json:"job_id"`
	OwnerTask      string              `json:"owner_task,omitempty"`
	OwnerRevision  uint64              `json:"owner_revision,omitempty"`
	IdentityDigest string              `json:"identity_digest"`
	CallID         string              `json:"call_id"`
	ToolCallID     string              `json:"tool_call_id"`
	LogicalRunID   string              `json:"logical_run_id"`
	StartedAt      time.Time           `json:"started_at,omitzero"`
	Kind           string              `json:"kind"`
	Spec           json.RawMessage     `json:"spec"`
	Environment    *environmentPayload `json:"environment,omitempty"`
	ChildSessionID string              `json:"child_session_id,omitempty"`
	ChildJournal   string              `json:"child_journal,omitempty"`
	Cwd            string              `json:"cwd"`
	OutputPath     string              `json:"output_path,omitempty"`
	Worktree       *workspacePayload   `json:"worktree,omitempty"`
}
type runningPayload struct {
	JobID      string `json:"job_id"`
	PID        int    `json:"pid,omitempty"`
	PGID       int    `json:"pgid,omitempty"`
	ChildRunID string `json:"child_run_id,omitempty"`
}
type attachedPayload struct {
	JobID        string `json:"job_id"`
	RunID        string `json:"run_id"`
	LogicalRunID string `json:"logical_run_id"`
}
type cancelPayload struct {
	JobID  string `json:"job_id"`
	Reason string `json:"reason"`
}
type finishedPayload struct {
	JobID         string          `json:"job_id"`
	State         string          `json:"state"`
	FinishedAt    time.Time       `json:"finished_at"`
	Reason        string          `json:"reason,omitempty"`
	ExitCode      *int            `json:"exit_code,omitempty"`
	Usage         types.Usage     `json:"usage"`
	Result        *work.Artifact  `json:"result,omitempty"`
	Patch         *work.Artifact  `json:"patch,omitempty"`
	Output        *work.Artifact  `json:"output,omitempty"`
	Value         json.RawMessage `json:"value,omitempty"`
	ResultPreview string          `json:"result_preview,omitempty"`
	Cleanup       string          `json:"cleanup"`
}
type cleanedPayload struct {
	JobID string `json:"job_id"`
	Path  string `json:"path"`
}
type stopPayload struct {
	Reason string `json:"reason"`
}

func (manager *Manager) commitStart(ctx context.Context, record *entry) error {
	recovery := record.recovery
	payload := startedPayload{JobID: recovery.Job.ID, OwnerTask: recovery.Job.OwnerTask, IdentityDigest: recovery.IdentityDigest, CallID: recovery.Invocation.CallID, ToolCallID: recovery.Invocation.ToolCallID, LogicalRunID: recovery.Job.LogicalRunID, StartedAt: recovery.Job.StartedAt, Kind: recovery.Job.Kind, ChildSessionID: recovery.Job.ChildSessionID, ChildJournal: recovery.ChildJournal, Cwd: recovery.Job.Cwd}
	payload.OwnerRevision = recovery.Invocation.OwnerRevision
	var err error
	if recovery.Task != nil {
		payload.Spec, err = json.Marshal(recovery.Task)
		environment := recovery.Environment
		ceiling := environment.Ceiling
		payload.Environment = &environmentPayload{Ceiling: ceilingPayload{ceiling.Mode, ceiling.Tools, ceiling.Overrides, ceiling.Interactive, ceiling.CloudRules}, Model: environment.Model, Provider: environment.Provider}
	} else {
		payload.Spec, err = json.Marshal(recovery.Bash)
		payload.OutputPath = manager.jobPath(recovery.Job.ID, "output.log")
	}
	if err != nil {
		return err
	}
	if workspace := recovery.Workspace; workspace != nil {
		payload.Worktree = &workspacePayload{workspace.Repo, workspace.CommonDir, workspace.BaseCommit, workspace.RelativeCwd, workspace.Path, workspace.Marker}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err = manager.append(ctx, "job_started", recovery.Invocation.RunID, encoded); err != nil {
		return err
	}
	manager.mu.Lock()
	manager.entries[recovery.Job.ID] = record
	// Reserve liveness before publishing: Cancel can arrive before launch returns.
	record.live = true
	record.joined = false
	manager.accepted[recovery.Job.LogicalRunID]++
	manager.notifyLocked()
	manager.mu.Unlock()
	if err := manager.publish(context.WithoutCancel(ctx), cloneJob(recovery.Job), "started"); err != nil {
		manager.mu.Lock()
		record.live = false
		record.joined = true
		closeDone(record)
		manager.notifyLocked()
		manager.mu.Unlock()
		return err
	}
	return nil
}
