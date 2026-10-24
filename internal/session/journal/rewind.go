package journal

import (
	"encoding/json"
	"fmt"

	"go.harness.dev/harness/internal/engine/types"
)

// KindRewind forks the visible transcript without removing journal history.
const KindRewind EntryType = "rewind"

// RewindEntry records the exact transcript at a workspace checkpoint.
// Usage and job records remain lifetime facts, even across a rewind.
type RewindEntry struct {
	Meta
	Checkpoint string            `json:"checkpoint"`
	Report     string            `json:"report,omitempty"`
	Messages   []json.RawMessage `json:"messages"`
}

func (RewindEntry) kind() EntryType         { return KindRewind }
func (e RewindEntry) meta() Meta            { return e.Meta }
func (e RewindEntry) withMeta(m Meta) Entry { e.Meta = m; return e }

// NewRewindEntry serializes a detached transcript for an append-only fork.
func NewRewindEntry(id, report string, messages []types.Message) (RewindEntry, error) {
	e := RewindEntry{Checkpoint: id, Report: report}
	size := 0
	for _, m := range messages {
		b, err := json.Marshal(m)
		if err != nil {
			return e, err
		}
		size += len(b)
		if size > 16<<20 {
			return e, fmt.Errorf("rewind transcript exceeds 16 MiB")
		}
		e.Messages = append(e.Messages, b)
	}
	return e, nil
}

func (e RewindEntry) messages() ([]types.Message, error) {
	var out []types.Message
	for _, b := range e.Messages {
		m, err := types.UnmarshalMessage(b)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}
