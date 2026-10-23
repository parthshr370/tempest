package tempest

import (
	"path/filepath"

	"go.harness.dev/harness/internal/toolio"
	"go.harness.dev/harness/internal/tools"
)

// journalDataDirGlob matches the session data directories (<journal>.d)
// next to the session journals in one journal directory.
const journalDataDirGlob = "*.jsonl.d"

// recoverEditBatches restores the edit batches dead processes left in any
// session data directory under journalDir, the per-workspace directory of
// session journals, and returns one diagnostic per interrupted batch. A
// batch still owned by a live session holds its record's flock and is
// skipped.
func recoverEditBatches(journalDir string, queue *toolio.FileMutationQueue) []string {
	dirs, err := filepath.Glob(filepath.Join(journalDir, journalDataDirGlob))
	if err != nil {
		return []string{"edit rollback recovery: " + err.Error()}
	}
	outcomes, err := tools.RecoverEditBatches(dirs, queue)
	messages := make([]string, 0, len(outcomes)+1)
	for _, outcome := range outcomes {
		messages = append(messages, outcome.String())
	}
	if err != nil {
		messages = append(messages, "edit rollback recovery: "+err.Error())
	}
	return messages
}
