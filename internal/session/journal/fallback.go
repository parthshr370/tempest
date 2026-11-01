package journal

// KindModelFallback records a run's switch to a backup model.
const KindModelFallback EntryType = "model_fallback"

// ModelFallbackRecord is a run's switch from an unavailable model to the next
// model of its fallback chain. Models are "provider:model" specs. The record
// is synced before the loop handles any of the backup's events, so a resumed
// run keeps the backup instead of returning to the unavailable model.
type ModelFallbackRecord struct {
	Meta          `json:"-"`
	From          string `json:"from"`
	To            string `json:"to"`
	Reason        string `json:"reason"`
	Attempts      int    `json:"attempts"`
	FallbackIndex int    `json:"fallback_index"`
}

func (ModelFallbackRecord) kind() EntryType            { return KindModelFallback }
func (e ModelFallbackRecord) meta() Meta               { return e.Meta }
func (e ModelFallbackRecord) withMeta(meta Meta) Entry { e.Meta = meta; return e }

// LogicalFallback returns the last backup-model switch recorded by any run of
// one logical run, or nil when the logical run stayed on its primary model.
func (l *Loaded) LogicalFallback(logicalID string) *ModelFallbackRecord {
	var latest *ModelFallbackRecord
	for _, r := range l.Runs {
		if r.LogicalRunID == logicalID && r.Fallback != nil {
			latest = r.Fallback
		}
	}
	return latest
}
