package serve

import (
	"context"
	"os"
	"time"

	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/tempest"
)

// LookupRun reads exact durable runs, checking locally owned live work first.
func (s *Server) LookupRun(ctx context.Context, id string) (tempest.RunDetail, error) {
	lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	s.mu.Lock()
	sessions := make([]*tempest.Session, 0, len(s.sessions))
	for _, entry := range s.sessions {
		if !entry.closing {
			sessions = append(sessions, entry.session)
		}
	}
	s.mu.Unlock()
	for _, owner := range sessions {
		snapshot := owner.Snapshot()
		if snapshot.Cwd == s.opts.Cwd && snapshot.Run.Status == "running" && snapshot.Run.RunID == id {
			return s.lookupLocalRun(lookupCtx, snapshot, id)
		}
	}
	return journal.LookupRun(lookupCtx, s.opts.SessionDir, s.opts.Cwd, id)
}
func (s *Server) lookupLocalRun(ctx context.Context, snapshot tempest.Snapshot, id string) (tempest.RunDetail, error) {
	path, err := journal.Resolve(ctx, s.opts.SessionDir, s.opts.Cwd, snapshot.SessionID)
	if err != nil {
		return tempest.RunDetail{}, err
	}
	stat, err := os.Stat(path)
	if err != nil {
		return tempest.RunDetail{}, err
	}
	if stat.Size() > 256<<20 {
		return tempest.RunDetail{}, journal.ErrLookupLimit
	}
	loaded, err := journal.Load(ctx, path)
	if err != nil {
		return tempest.RunDetail{}, err
	}
	for _, detail := range loaded.Runs {
		if detail.RunID != id {
			continue
		}
		if !detail.Finished {
			detail.Interrupted = false
			detail.Recoverable = false
		}
		return detail, nil
	}
	return tempest.RunDetail{}, journal.ErrRunNotFound
}
