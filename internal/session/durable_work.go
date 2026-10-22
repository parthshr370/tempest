package session

import (
	"context"

	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/work"
)

// AppendWorkFact commits only the journal's known background-work fact schemas.
func AppendWorkFact(ctx context.Context, store *journal.Store, fact work.Fact) error {
	return store.AppendWorkFact(ctx, fact)
}
