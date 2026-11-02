package dispatch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
	"go.harness.dev/harness/internal/procgroup"
	"go.harness.dev/harness/internal/worktree"
)

// prepareMarker is written in the worktree's private git directory before
// the prepare command runs. It records the generation and whether the
// command failed; a "started" marker of the current generation proves an
// interrupted run, which is never assumed safe to repeat.
const prepareMarker = "tempest-prepare-started"

const (
	prepareStarted = "started"
	prepareFailed  = "failed"
)

// prepareOutputBytes bounds the prepare output kept for a failure reason.
const prepareOutputBytes = 4 << 10

// prepareWaitDelay bounds how long a killed prepare's output pipes may stay
// open, held by descendants that escaped the process group.
const prepareWaitDelay = 2 * time.Second

func (d *Dispatcher) branchSpec(ticket model.TicketID) worktree.BranchSpec {
	return worktree.BranchSpec{
		Clone:  d.config.Clone,
		Path:   filepath.Join(d.worktrees, string(ticket)),
		Branch: model.TicketBranch(ticket),
		Remote: d.remote,
		Base:   d.base,
		Env:    d.env,
	}
}

// worktree adopts or creates the Ticket's branch checkout and runs the
// operator's prepare command in it once. An interrupted prepare of this
// generation fails the dispatch permanently; a later generation (operator
// retry) or a failed prepare recreates the checkout and runs it again.
func (d *Dispatcher) worktree(ctx context.Context, admitted model.AdmittedDispatch) (worktree.BranchCheckout, error) {
	spec := d.branchSpec(admitted.Ticket.ID)
	checkout, err := ensureCheckout(ctx, spec)
	if err != nil || len(d.config.Workers.Prepare) == 0 {
		return checkout, err
	}
	if admitted.Ticket.Prepared && !checkout.Created {
		return checkout, nil
	}
	gen, state, found, err := readMarker(checkout.GitDir)
	if err != nil {
		return checkout, transientFailure("read prepare marker", err)
	}
	if found && !checkout.Created {
		if state == prepareStarted && gen == admitted.Dispatch.Gen {
			return checkout, permanentFailure("prepare command was interrupted; an operator retry recreates the worktree", nil)
		}
		if err := worktree.RemoveBranch(ctx, spec, true); err != nil {
			return checkout, transientFailure("remove the worktree of an earlier prepare", err)
		}
		if checkout, err = ensureCheckout(ctx, spec); err != nil {
			return checkout, err
		}
	}
	if err := writeMarker(checkout.GitDir, admitted.Dispatch.Gen, prepareStarted); err != nil {
		return checkout, transientFailure("write prepare marker", err)
	}
	if err := d.runPrepare(ctx, checkout.Path); err != nil {
		if ctx.Err() != nil {
			return checkout, ctx.Err()
		}
		if markErr := writeMarker(checkout.GitDir, admitted.Dispatch.Gen, prepareFailed); markErr != nil {
			return checkout, errors.Join(err, markErr)
		}
		return checkout, permanentFailure("prepare command failed", err)
	}
	return checkout, nil
}

func ensureCheckout(ctx context.Context, spec worktree.BranchSpec) (worktree.BranchCheckout, error) {
	checkout, err := worktree.EnsureBranch(ctx, spec)
	switch {
	case err == nil:
		return checkout, nil
	case ctx.Err() != nil:
		return checkout, ctx.Err()
	case errors.Is(err, worktree.ErrBranchMismatch):
		return checkout, permanentFailure("ticket worktree", err)
	default:
		return checkout, transientFailure("ticket worktree", err)
	}
}

func readMarker(gitDir string) (uint32, string, bool, error) {
	raw, err := os.ReadFile(filepath.Join(gitDir, prepareMarker))
	if errors.Is(err, os.ErrNotExist) {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, err
	}
	fields := strings.Fields(string(raw))
	if len(fields) != 2 {
		return 0, "", false, fmt.Errorf("malformed prepare marker %q", raw)
	}
	gen, err := strconv.ParseUint(fields[0], 10, 32)
	if err != nil {
		return 0, "", false, fmt.Errorf("malformed prepare marker %q: %w", raw, err)
	}
	return uint32(gen), fields[1], true, nil
}

// writeMarker replaces the marker durably: a crash leaves the old or the
// new content, never a torn file.
func writeMarker(gitDir string, gen uint32, state string) error {
	path := filepath.Join(gitDir, prepareMarker)
	temp := path + ".tmp"
	file, err := os.OpenFile(temp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(file, "%d %s\n", gen, state)
	err = errors.Join(err, file.Sync(), file.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		return err
	}
	dir, err := os.Open(gitDir)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

// runPrepare runs the operator's argv in the worktree without a shell, in
// its own process group, killed with the group at the prepare timeout. The
// controller's git environment (credentials) is not passed to it.
func (d *Dispatcher) runPrepare(ctx context.Context, dir string) error {
	timeout := time.Duration(d.config.Workers.PrepareTimeout)
	if timeout <= 0 {
		timeout = defaultPrepareLimit
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	argv := d.config.Workers.Prepare
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	var output bytes.Buffer
	tail := &tailWriter{buffer: &output, limit: prepareOutputBytes}
	cmd.Stdout, cmd.Stderr = tail, tail
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return procgroup.Signal(cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = prepareWaitDelay
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(output.String()))
	}
	return nil
}

// tailWriter keeps the last limit bytes written.
type tailWriter struct {
	buffer *bytes.Buffer
	limit  int
}

func (w *tailWriter) Write(chunk []byte) (int, error) {
	w.buffer.Write(chunk)
	if excess := w.buffer.Len() - w.limit; excess > 0 {
		w.buffer.Next(excess)
	}
	return len(chunk), nil
}
