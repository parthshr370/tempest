package journal

import "go.harness.dev/harness/internal/hooks"

// KindHook records a lifecycle command receipt or keyed execution intent.
const KindHook EntryType = "hook"

// HookRecord keeps command output out of the conversation's parent chain.
// Keyed records protect compaction and queued prompts from repeating shell effects.
type HookRecord struct {
	Meta    `json:"-"`
	Event   string        `json:"event"`
	Key     string        `json:"key,omitempty"`
	Status  string        `json:"status"`
	Result  *hooks.Result `json:"result,omitempty"`
	Report  *hooks.Report `json:"report,omitempty"`
	QueueID string        `json:"queue_id,omitempty"`
}

func (HookRecord) kind() EntryType            { return KindHook }
func (e HookRecord) meta() Meta               { return e.Meta }
func (e HookRecord) withMeta(meta Meta) Entry { e.Meta = meta; return e }

// HookState distinguishes a reusable result from an interrupted side effect.
type HookState struct {
	Finished bool
	Result   hooks.Result
}
