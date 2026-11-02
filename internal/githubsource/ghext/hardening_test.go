package ghext

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
)

// The controller resolves the worktree's HEAD outside the worker's sandbox,
// so it must not run the worker's fsmonitor or content filters, which
// `git status` does for a file whose stat data is stale.
func TestHeadReadRunsNothingFromTheWorker(t *testing.T) {
	w := newWorld(t, Options{})
	ticket := w.launch(1)
	worktree := w.worktree(ticket)
	w.commit(worktree, ".gitattributes", "a.txt filter=worker\n")
	w.commit(worktree, "a.txt", "a\n")

	// Staged changes are uncommitted too.
	if err := os.WriteFile(filepath.Join(worktree, "b.txt"), []byte("b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.git(worktree, "add", "b.txt")
	if result := w.call(session(ticket), model.ToolPush, "staged", struct{}{}); !result.IsError || !strings.Contains(text(result), "b.txt") {
		t.Fatalf("push with a staged file = %q", text(result))
	}
	w.git(worktree, "rm", "--quiet", "--cached", "b.txt")

	capture := filepath.Join(w.dir, "executed")
	program := filepath.Join(w.dir, "worker-program")
	if err := os.WriteFile(program, []byte("#!/bin/sh\necho \"$0 $*\" >> '"+capture+"'\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"core.fsmonitor", "filter.worker.process", "filter.worker.clean"} {
		w.git(worktree, "config", key, program)
	}
	w.git(worktree, "config", "filter.worker.required", "true")
	// Stale stat data, same content.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(worktree, "a.txt"), old, old); err != nil {
		t.Fatal(err)
	}
	result := w.call(session(ticket), model.ToolPush, "touched", struct{}{})
	if ran, err := os.ReadFile(capture); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worker programs ran: %s %v", ran, err)
	}
	if result.IsError {
		t.Fatalf("push of a touched but unchanged file = %q", text(result))
	}

	if err := os.WriteFile(filepath.Join(worktree, "a.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result = w.call(session(ticket), model.ToolPush, "changed", struct{}{})
	if !result.IsError || !strings.Contains(text(result), "a.txt") {
		t.Fatalf("push of a changed file = %q", text(result))
	}
	if ran, err := os.ReadFile(capture); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worker programs ran: %s %v", ran, err)
	}
}

// reply is ReplaySafe: the same call replayed after recovery moved the
// Ticket to a new generation returns its original intent.
func TestReplyReplayAfterRecoveryGeneration(t *testing.T) {
	w := newWorld(t, Options{})
	ticket := w.launch(1)
	arguments := map[string]string{"body": "same interrupted reply"}
	first := structured[effectView](t, w.call(session(ticket), model.ToolReply, "interrupted-call", arguments))
	if err := w.store.Finished(w.ctx, model.NewDispatchID(ticket, 1), model.RunOutcome{StopReason: "unknown"}, start); err != nil {
		t.Fatal(err)
	}
	if gen := w.detail(ticket).Ticket.Gen; gen < 2 {
		t.Fatalf("generation = %d, want a recovery generation", gen)
	}
	second := structured[effectView](t, w.call(session(ticket), model.ToolReply, "interrupted-call", arguments))
	if first.ID != second.ID || len(w.detail(ticket).Outbox) != 1 {
		t.Fatalf("replayed reply = %s then %s, outbox %+v", first.ID, second.ID, w.detail(ticket).Outbox)
	}
}
