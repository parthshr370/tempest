package journal

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"go.harness.dev/harness/internal/cloudcmd"
	types "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/work"
)

const (
	KindQueueEnqueued          EntryType = "queue_enqueued"
	KindQueueCancelled         EntryType = "queue_cancelled"
	KindJobStarted             EntryType = "job_started"
	KindJobRunning             EntryType = "job_running"
	KindJobAttached            EntryType = "job_attached"
	KindJobCancelRequested     EntryType = "job_cancel_requested"
	KindJobFinished            EntryType = "job_finished"
	KindJobCleaned             EntryType = "job_cleaned"
	KindRunStopRequested       EntryType = "run_stop_requested"
	KindCompactionCallStarted  EntryType = "compaction_call_started"
	KindCompactionCallFinished EntryType = "compaction_call_finished"
)

// WorkRecord stores one validated background-work fact in the normal envelope.
type WorkRecord struct {
	Meta
	Type    EntryType
	Payload json.RawMessage
}

func (r WorkRecord) kind() EntryType       { return r.Type }
func (r WorkRecord) meta() Meta            { return r.Meta }
func (r WorkRecord) withMeta(m Meta) Entry { r.Meta = m; return r }

// QueueEnqueued is a durable acceptance receipt. Timestamp comes from its envelope.
type QueueEnqueued struct {
	QueueID     string         `json:"queue_id"`
	Kind        work.QueueKind `json:"kind"`
	Text        string         `json:"text"`
	Key         string         `json:"key,omitempty"`
	SourceJobID string         `json:"source_job_id,omitempty"`
}

// QueueCancelled withdraws an accepted, unconsumed receipt.
type QueueCancelled struct {
	QueueID string `json:"queue_id"`
}

// JobEnvironment keeps launch-time policy private to recovery.
type JobEnvironment struct {
	Ceiling struct {
		Mode        string            `json:"mode"`
		Tools       []string          `json:"tools"`
		Overrides   map[string]string `json:"overrides"`
		Interactive map[string]string `json:"interactive"`
		// CloudRules is additive to v3: absent in older records (no grants).
		CloudRules []cloudcmd.Rule `json:"cloud_rules,omitempty"`
	} `json:"ceiling"`
	Model    string `json:"model"`
	Provider string `json:"provider"`
}

// JobWorkspace pins the exact owned worktree intent.
type JobWorkspace struct {
	Repo        string `json:"repo"`
	CommonDir   string `json:"common_dir"`
	BaseCommit  string `json:"base_commit"`
	RelativeCwd string `json:"relative_cwd"`
	Path        string `json:"path"`
	Marker      string `json:"marker"`
}

// JobStarted precedes every child, process and workspace effect.
type JobStarted struct {
	JobID          string `json:"job_id"`
	IdentityDigest string `json:"identity_digest"`
	CallID         string `json:"call_id"`
	ToolCallID     string `json:"tool_call_id"`
	LogicalRunID   string `json:"logical_run_id"`
	// StartedAt is the job's own admission time; older v3 lines omit it and
	// reduce to the record timestamp.
	StartedAt      time.Time       `json:"started_at,omitzero"`
	Kind           string          `json:"kind"`
	Spec           json.RawMessage `json:"spec"`
	Environment    *JobEnvironment `json:"environment,omitempty"`
	ChildSessionID string          `json:"child_session_id,omitempty"`
	ChildJournal   string          `json:"child_journal,omitempty"`
	Cwd            string          `json:"cwd"`
	Isolation      string          `json:"isolation,omitempty"`
	OutputPath     string          `json:"output_path,omitempty"`
	Worktree       *JobWorkspace   `json:"worktree,omitempty"`
}

// JobRunning records diagnostic process or child-run identity, never restart authority.
type JobRunning struct {
	JobID      string `json:"job_id"`
	ChildRunID string `json:"child_run_id,omitempty"`
	PID        int    `json:"pid,omitempty"`
	PGID       int    `json:"pgid,omitempty"`
}

// JobAttached assigns an existing task to a continuation's observing run.
type JobAttached struct {
	JobID        string `json:"job_id"`
	RunID        string `json:"run_id"`
	LogicalRunID string `json:"logical_run_id"`
}

// JobCancelRequested prevents an interrupted child from restarting.
type JobCancelRequested struct {
	JobID  string `json:"job_id"`
	Reason string `json:"reason"`
}

// JobFinished is a joined, immutable terminal outcome.
type JobFinished struct {
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

// JobCleaned acknowledges removal of one exact owned worktree.
type JobCleaned struct {
	JobID string `json:"job_id"`
	Path  string `json:"path"`
}

// RunStopRequested seals recovery before cancellation or budget teardown.
type RunStopRequested struct {
	Reason string `json:"reason"`
}

// CompactionCall records one paid summary phase and its reusable result.
type CompactionCall struct {
	CompactionID    string          `json:"compaction_id"`
	SourceHash      string          `json:"source_hash"`
	Phase           string          `json:"phase"`
	Model           string          `json:"model"`
	Settings        json.RawMessage `json:"settings,omitempty"`
	PreparationHash string          `json:"preparation_hash,omitempty"`
	Status          string          `json:"status,omitempty"`
	Summary         string          `json:"summary,omitempty"`
	Usage           types.Usage     `json:"usage"`
	SourceRef       string          `json:"source_ref,omitempty"`
	Error           string          `json:"error,omitempty"`
}

// CompactionPart tracks intent and result independently across crash boundaries.
type CompactionPart struct {
	Started  *CompactionCall
	Finished *CompactionCall
}

func isWorkKind(k EntryType) bool {
	switch k {
	case KindQueueEnqueued, KindQueueCancelled, KindJobStarted, KindJobRunning, KindJobAttached, KindJobCancelRequested, KindJobFinished, KindJobCleaned, KindRunStopRequested, KindCompactionCallStarted, KindCompactionCallFinished:
		return true
	}
	return false
}
func strictPayload(raw json.RawMessage, value any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("trailing payload data")
	}
	return nil
}
func validDigest(s string) bool { b, e := hex.DecodeString(s); return e == nil && len(b) == 32 }

// validCloudRules accepts only rules NormalizeRules accepts; a missing list
// (records before M5) is valid and restores no grants.
func validCloudRules(rules []cloudcmd.Rule) bool {
	_, err := cloudcmd.NormalizeRules(rules)
	return err == nil
}
func terminal(s string) bool {
	return s == "completed" || s == "failed" || s == "cancelled" || s == "unknown"
}
func validateWorkRecord(r WorkRecord) error {
	var value any
	switch r.Type {
	case KindQueueEnqueued:
		value = &QueueEnqueued{}
	case KindQueueCancelled:
		value = &QueueCancelled{}
	case KindJobStarted:
		value = &JobStarted{}
	case KindJobRunning:
		value = &JobRunning{}
	case KindJobAttached:
		value = &JobAttached{}
	case KindJobCancelRequested:
		value = &JobCancelRequested{}
	case KindJobFinished:
		value = &JobFinished{}
	case KindJobCleaned:
		value = &JobCleaned{}
	case KindRunStopRequested:
		value = &RunStopRequested{}
	case KindCompactionCallStarted, KindCompactionCallFinished:
		value = &CompactionCall{}
	default:
		return fmt.Errorf("unsupported work fact %q", r.Type)
	}
	if err := strictPayload(r.Payload, value); err != nil {
		return fmt.Errorf("%s payload: %w", r.Type, err)
	}
	valid := true
	switch v := value.(type) {
	case *QueueEnqueued:
		valid = v.QueueID != "" && (v.Kind == work.Steer || v.Kind == work.FollowUp) && utf8.ValidString(v.Text) && strings.TrimSpace(v.Text) != "" && utf8.ValidString(v.Key) && len(v.Key) <= 128
		if v.SourceJobID == "" {
			valid = valid && len(v.Text) <= 16<<10 && !strings.HasPrefix(v.Key, "job:")
		} else {
			valid = valid && v.Kind == work.FollowUp && v.Key == "job:"+v.SourceJobID+":terminal"
		}
	case *QueueCancelled:
		valid = v.QueueID != ""
	case *JobStarted:
		valid = r.RunID != "" && v.JobID != "" && validDigest(v.IdentityDigest) && v.CallID != "" && v.ToolCallID != "" && v.LogicalRunID != "" && v.Cwd != ""
		if v.Kind == "task" {
			var s work.TaskSpec
			err := strictPayload(v.Spec, &s)
			valid = valid && err == nil && strings.TrimSpace(s.Prompt) != "" && len(s.Prompt) <= 32<<10 && len(s.Description) <= 512 && len(s.OutputSchema) <= 16<<10 && (s.SubagentType == "general" || s.SubagentType == "explore") && v.ChildSessionID != "" && v.ChildJournal != "" && v.Environment != nil && validCloudRules(v.Environment.Ceiling.CloudRules)
		} else if v.Kind == "bash" {
			var s work.BashSpec
			err := strictPayload(v.Spec, &s)
			valid = valid && err == nil && strings.TrimSpace(s.Command) != "" && s.TimeoutSeconds > 0 && v.OutputPath != ""
		} else {
			valid = false
		}
	case *JobRunning:
		valid = v.JobID != "" && r.RunID != "" && v.PID >= 0 && v.PGID >= 0
	case *JobAttached:
		valid = v.JobID != "" && v.RunID != "" && v.LogicalRunID != ""
	case *JobCancelRequested:
		valid = v.JobID != "" && v.Reason != ""
	case *JobFinished:
		valid = v.JobID != "" && terminal(v.State) && !v.FinishedAt.IsZero() && len(v.ResultPreview) <= 4096 && len(v.Value) <= 64<<10 && (v.Cleanup == "none" || v.Cleanup == "retained" || v.Cleanup == "removed" || v.Cleanup == "blocked")
	case *JobCleaned:
		valid = v.JobID != "" && v.Path != ""
	case *RunStopRequested:
		valid = r.RunID != "" && (v.Reason == "aborted" || v.Reason == "max_turns" || v.Reason == "max_cost" || v.Reason == "max_wall_time" || v.Reason == "error")
	case *CompactionCall:
		valid = r.RunID != "" && v.CompactionID != "" && validDigest(v.SourceHash) && (v.Phase == "history" || v.Phase == "prefix") && v.Model != ""
		if r.Type == KindCompactionCallStarted {
			valid = valid && validDigest(v.PreparationHash) && json.Valid(v.Settings) && v.Status == "" && v.SourceRef == ""
		} else {
			valid = valid && (v.Status == "ok" || v.Status == "error") && v.SourceRef != "" && len(v.Summary) <= 512<<10 && (v.Status != "ok" || strings.TrimSpace(v.Summary) != "")
		}
	}
	if !valid {
		return fmt.Errorf("invalid %s payload", r.Type)
	}
	return nil
}

// AppendWorkFact validates a known schema and commits it through the journal writer.
func (s *Store) AppendWorkFact(ctx context.Context, fact work.Fact) error {
	return s.Append(ctx, WorkRecord{Meta: Meta{RunID: fact.RunID}, Type: EntryType(fact.Kind), Payload: fact.Payload})
}
