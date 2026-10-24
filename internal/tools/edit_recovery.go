package tools

import (
	"errors"

	"go.harness.dev/harness/internal/editbatch"
	"go.harness.dev/harness/internal/toolio"
)

// RecoverEditBatches restores the multi-file edit batches a dead process
// left in each session data directory (see editbatch.Recover), holding the
// file tools' mutation locks: queue is the one the session's tools use, nil
// meaning the tools' process-wide default. Batches whose owner is alive are
// skipped, so callers may pass other sessions' directories.
func RecoverEditBatches(sessionDataDirs []string, queue *toolio.FileMutationQueue) ([]editbatch.Outcome, error) {
	queue = mutationQueue(ToolsOptions{MutationQueue: queue})
	var outcomes []editbatch.Outcome
	var errs []error
	for _, dir := range sessionDataDirs {
		recovered, err := editbatch.Recover(dir, queue)
		outcomes = append(outcomes, recovered...)
		errs = append(errs, err)
	}
	return outcomes, errors.Join(errs...)
}

func editStateDir(options ToolsOptions) string {
	if options.EditStateDir != "" {
		return options.EditStateDir
	}
	return options.SessionDataDir
}
