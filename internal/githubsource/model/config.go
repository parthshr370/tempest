package model

import (
	"encoding/json"
	"fmt"
	"time"
)

// ConfigVersion is the only operator file version this build reads.
const ConfigVersion = 1

// Config is the operator file selected by `serve -github-config`. It is
// executable policy: it names the gh binary, credentials directory, clone,
// prepare command and authorized actors, so it comes only from the operator
// and is never read from a repository. Unknown fields are load errors.
type Config struct {
	Version int `json:"version"`
	// Repository is "owner/repo" and must equal the `-github` flag.
	Repository string        `json:"repository"`
	Account    AccountConfig `json:"account"`
	// Clone is an absolute path to a local clone whose fetch remote is
	// Repository. Ticket worktrees branch from it.
	Clone string `json:"clone"`
	// StateDir holds control.jsonl and Ticket worktrees. Absolute; defaults
	// to <session-dir>/github/<owner>/<repo>.
	StateDir  string          `json:"stateDir,omitempty"`
	Poll      PollConfig      `json:"poll"`
	Observe   ObserveConfig   `json:"observe"`
	Admission AdmissionConfig `json:"admission"`
	Workers   WorkersConfig   `json:"workers"`
	Budgets   BudgetsConfig   `json:"budgets"`
	Limits    LimitsConfig    `json:"limits"`
	Prompts   TeamPrompts     `json:"prompts"`
	// Merge must be "manual". Workers hold no merge authority.
	Merge string `json:"merge"`
}

// AccountConfig selects the dedicated GitHub user. Its credentials live in
// GHConfigDir and are passed only to the controller's gh and git
// subprocesses, never to the serve process or worker shells.
type AccountConfig struct {
	Login       string `json:"login"`
	ID          int64  `json:"id"`
	GHPath      string `json:"ghPath"`
	GHConfigDir string `json:"ghConfigDir"`
}

// ActorID is an account named by numeric ID; Login is checked as a guard
// against a misconfigured ID, not used for authorization.
type ActorID struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
}

// PollConfig bounds the polling loop.
type PollConfig struct {
	// Interval between rounds: 60s to 1h, default 60s. A larger GitHub
	// X-Poll-Interval wins.
	Interval Duration `json:"interval"`
	// Lookback is subtracted from a thread position on every reread so a
	// late-indexed comment is still read; EventIDs deduplicate. Default 1h.
	Lookback Duration `json:"lookback"`
	// Retention keeps a thread polled after its last Ticket ended. Default
	// 336h (two weeks).
	Retention Duration `json:"retention"`
	// CallTimeout bounds one gh subprocess. Default 30s.
	CallTimeout Duration `json:"callTimeout"`
	// MaxPages bounds pages read per resource per round; the rest is read
	// next round from the committed position. Default 10.
	MaxPages int `json:"maxPages"`
}

// ObserveConfig enables receive-all streams. Receiving is not permission:
// observed events without an authorized mention are recorded, not worked.
type ObserveConfig struct {
	Issues   bool `json:"issues"`
	Pulls    bool `json:"pulls"`
	Comments bool `json:"comments"`
	CI       bool `json:"ci"`
}

// Mode is the admission mode for events without an explicit request.
type Mode string

// Admission modes. "auto" is rejected at load: this build never authorizes
// work that nobody explicitly requested.
const (
	ModeOff     Mode = "off"
	ModeObserve Mode = "observe"
)

// AdmissionConfig lists who may authorize work. Bots never authorize.
type AdmissionConfig struct {
	Mode   Mode      `json:"mode"`
	Actors []ActorID `json:"actors"`
	Bots   []ActorID `json:"bots"`
}

// WorkersConfig shapes Ticket worker sessions.
type WorkersConfig struct {
	// Max normal slots, 1 to 16, default 3. One more slot serves only
	// authoritative human continuations of existing workers.
	Max int `json:"max"`
	// Model overrides the serve default model for new worker sessions.
	Model string `json:"model,omitempty"`
	// PermissionMode for worker sessions; "bypass" is rejected. Default
	// "default": guarded commands wait for an ACP approver, then deny.
	PermissionMode string `json:"permissionMode,omitempty"`
	// Prepare is an argv run once in a new worktree; no shell is implied.
	Prepare        []string `json:"prepare,omitempty"`
	PrepareTimeout Duration `json:"prepareTimeout"`
	// BaseBranch defaults to the repository's default branch.
	BaseBranch string `json:"baseBranch,omitempty"`
	// Remote is the clone's remote for fetch and push. Default "origin".
	Remote string `json:"remote,omitempty"`
}

// BudgetsConfig sets the finite ledgers. Every amount must be positive:
// there is no unlimited setting.
type BudgetsConfig struct {
	// Run caps each worker run and is the most one dispatch reserves.
	Run RunBudget `json:"run"`
	// TicketUSD is each Ticket's lifetime allowance.
	TicketUSD float64 `json:"ticketUsd"`
	// RepoUSD is the allowance of each RepoPeriod.
	RepoUSD    float64  `json:"repoUsd"`
	RepoPeriod Duration `json:"repoPeriod"`
	// TicketTokens and RepoTokens optionally cap total tokens as well.
	TicketTokens int64 `json:"ticketTokens,omitempty"`
	RepoTokens   int64 `json:"repoTokens,omitempty"`
	// MinRunUSD is the smallest reservation worth dispatching; below it the
	// Ticket is held for budget. Default a tenth of Run.MaxCostUSD.
	MinRunUSD float64 `json:"minRunUsd,omitempty"`
	// MaxAutoWakesPerDay bounds machine-input wakes per Ticket per UTC day,
	// which ends bot and CI loops. Default 12.
	MaxAutoWakesPerDay int `json:"maxAutoWakesPerDay"`
}

// RunBudget is the per-run cap applied with Session.SetBudget before each
// dispatch, lowered to what the ledgers reserved.
type RunBudget struct {
	MaxTurns       int      `json:"maxTurns"`
	MaxCostUSD     float64  `json:"maxCostUsd"`
	MaxWallTime    Duration `json:"maxWallTime"`
	MaxTotalTokens int      `json:"maxTotalTokens,omitempty"`
	MaxToolCalls   int      `json:"maxToolCalls,omitempty"`
}

// LimitsConfig bounds stored and rendered text.
type LimitsConfig struct {
	// MaxEventBodyBytes truncates stored event bodies. Default 65536.
	MaxEventBodyBytes int `json:"maxEventBodyBytes"`
	// MaxWakeBytes bounds one rendered wake prompt; older Inputs are
	// summarized by count and read through the thread tool. Default 65536.
	MaxWakeBytes int `json:"maxWakeBytes"`
}

// TeamPrompts are operator-written prompt sections. Each Ticket captures
// them at creation so its worker's system prompt never changes mid-Ticket.
type TeamPrompts struct {
	Communication string `json:"communication,omitempty"`
	Scope         string `json:"scope,omitempty"`
	Delivery      string `json:"delivery,omitempty"`
}

// Duration is a time.Duration written in Go syntax ("90s", "10m").
type Duration time.Duration

// MarshalJSON writes Go duration syntax.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON reads Go duration syntax and rejects bare numbers, which
// would otherwise be ambiguous between seconds and nanoseconds.
func (d *Duration) UnmarshalJSON(raw []byte) error {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return fmt.Errorf("duration must be a string such as \"90s\": %w", err)
	}
	parsed, err := time.ParseDuration(text)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}
