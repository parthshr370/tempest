package work

import (
	"encoding/json"
	"time"

	types "go.harness.dev/harness/internal/engine/types"
)

// The model reads job results through tools whose arguments are snake_case
// (job_id, job_ids), so every job value a tool returns uses the same keys.
// Job, JobPage and LogPage keep the camelCase shape of the wire protocols.

// ToolHandle is what task and async bash return when a job is admitted.
type ToolHandle struct {
	JobID          string `json:"job_id"`
	Kind           string `json:"kind"`
	State          string `json:"state"`
	ChildSessionID string `json:"child_session_id,omitempty"`
}

// ToolJob is a Job as model tools render it. Run and tool-call correlation
// ids are host bookkeeping no model tool accepts, so they are left out.
type ToolJob struct {
	JobID          string          `json:"job_id"`
	Kind           string          `json:"kind"`
	State          string          `json:"state"`
	ChildSessionID string          `json:"child_session_id,omitempty"`
	Description    string          `json:"description,omitempty"`
	StartedAt      time.Time       `json:"started_at"`
	FinishedAt     time.Time       `json:"finished_at,omitzero"`
	Cwd            string          `json:"cwd,omitempty"`
	Isolation      string          `json:"isolation,omitempty"`
	BaseCommit     string          `json:"base_commit,omitempty"`
	ResultText     string          `json:"result_text,omitempty"`
	Result         *Artifact       `json:"result,omitempty"`
	Patch          *Artifact       `json:"patch,omitempty"`
	Output         *Artifact       `json:"output,omitempty"`
	Value          json.RawMessage `json:"value,omitempty"`
	ValueInResult  bool            `json:"value_in_result,omitempty"`
	Usage          ToolUsage       `json:"usage"`
	ExitCode       *int            `json:"exit_code,omitempty"`
	Reason         string          `json:"reason,omitempty"`
	Cleanup        string          `json:"cleanup,omitempty"`
}

// ToolUsage is a job's token and cost usage as model tools render it.
type ToolUsage struct {
	Input       int     `json:"input"`
	Output      int     `json:"output"`
	CacheRead   int     `json:"cache_read"`
	CacheWrite  int     `json:"cache_write"`
	Reasoning   *int    `json:"reasoning,omitempty"`
	TotalTokens int     `json:"total_tokens"`
	CostUSD     float64 `json:"cost_usd"`
	CostUnknown bool    `json:"cost_unknown,omitempty"`
}

// ToolJobPage is a JobPage as model tools render it.
type ToolJobPage struct {
	Jobs       []ToolJob `json:"jobs"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

// ToolLogPage is a LogPage as model tools render it.
type ToolLogPage struct {
	Text       string `json:"text"`
	Offset     int64  `json:"offset"`
	NextOffset int64  `json:"next_offset"`
	EOF        bool   `json:"eof"`
	Truncated  bool   `json:"truncated"`
}

// Handle returns the admission handle for job.
func (job Job) Handle() ToolHandle {
	return ToolHandle{JobID: job.ID, Kind: job.Kind, State: job.State, ChildSessionID: job.ChildSessionID}
}

// ToolView returns job as model tools render it.
func (job Job) ToolView() ToolJob {
	return ToolJob{
		JobID: job.ID, Kind: job.Kind, State: job.State, ChildSessionID: job.ChildSessionID,
		Description: job.Description, StartedAt: job.StartedAt, FinishedAt: job.FinishedAt,
		Cwd: job.Cwd, Isolation: job.Isolation, BaseCommit: job.BaseCommit, ResultText: job.ResultText,
		Result: job.Result, Patch: job.Patch, Output: job.Output, Value: job.Value, ValueInResult: job.ValueInResult,
		Usage: toolUsage(job.Usage), ExitCode: job.ExitCode, Reason: job.Reason, Cleanup: job.Cleanup,
	}
}

// ToolViews renders jobs as model tools render them.
func ToolViews(jobs []Job) []ToolJob {
	views := make([]ToolJob, len(jobs))
	for i, job := range jobs {
		views[i] = job.ToolView()
	}
	return views
}

// ToolView returns page as model tools render it.
func (page JobPage) ToolView() ToolJobPage {
	return ToolJobPage{Jobs: ToolViews(page.Jobs), NextCursor: page.NextCursor}
}

// ToolView returns page as model tools render it.
func (page LogPage) ToolView() ToolLogPage {
	return ToolLogPage(page)
}

func toolUsage(usage types.Usage) ToolUsage {
	return ToolUsage{
		Input: usage.Input, Output: usage.Output, CacheRead: usage.CacheRead, CacheWrite: usage.CacheWrite,
		Reasoning: usage.Reasoning, TotalTokens: usage.TotalTokens, CostUSD: usage.Cost.Total, CostUnknown: usage.Cost.Unknown,
	}
}
