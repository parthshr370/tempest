package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"go.harness.dev/harness/internal/toolio"
	"go.harness.dev/harness/internal/work"
	"go.harness.dev/harness/internal/worktree"
)

const pipeBytes = 4096
const outputTailBytes = 64 << 10

// Logs also sync once a second; the byte trigger only bounds unsynced data for
// fast writers. Darwin fsync is a full flush, so 64 KiB made 16 MiB cost ~256 flushes.
const outputSyncBytes = 1 << 20
const pipeJoinDelay = 2 * time.Second

type processOutput struct {
	mu             sync.Mutex
	accumulator    *toolio.OutputAccumulator
	err            error
	bytesSinceSync int
	lastNotice     time.Time
	lastSync       time.Time
	// grew is closed and replaced on every append; write_stdin waits on it.
	grew chan struct{}
	// drained is closed once every output reader has finished.
	drained chan struct{}
}

func (manager *Manager) runBash(ctx context.Context, record *entry) {
	manager.mu.Lock()
	job := cloneJob(record.recovery.Job)
	spec := *record.recovery.Bash
	manager.mu.Unlock()
	input := record.input
	defer input.settleStart()
	job.State = "failed"
	outputPath := manager.jobPath(job.ID, "output.log")
	if err := worktree.PrivateDir(manager.jobPath(job.ID, ".")); err != nil {
		job.Reason = "output_io"
		job.ResultText = preview(err.Error(), maxPreviewBytes)
		manager.settle(record, job)
		return
	}
	output := &processOutput{accumulator: toolio.NewOutputAccumulator(toolio.OutputAccumulatorOptions{MaxLines: 1000000, MaxBytes: outputTailBytes, SpillPath: outputPath, MaxTotalBytes: maxLogBytes}), grew: make(chan struct{}), drained: make(chan struct{})}
	if err := output.accumulator.OpenSpill(); err != nil {
		job.Reason = "output_io"
		job.ResultText = preview(err.Error(), maxPreviewBytes)
		manager.settle(record, job)
		return
	}
	processCtx, cancel := context.WithTimeout(ctx, time.Duration(spec.TimeoutSeconds)*time.Second)
	defer cancel()
	command := spec.Command
	if manager.options.CommandPrefix != "" {
		command = manager.options.CommandPrefix + "\n" + command
	}
	cmd := exec.CommandContext(processCtx, manager.options.ShellPath, "-c", command)
	cmd.Dir = job.Cwd
	cmd.WaitDelay = pipeJoinDelay
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	streams, err := openStreams(cmd, spec.PTY, spec.Stdin)
	if err != nil {
		output.err = err
		manager.finishProcess(record, job, output, nil, processCtx)
		return
	}
	if err = cmd.Start(); err != nil {
		output.err = errors.Join(err, streams.closeAll())
		job.Reason = "process_start"
		manager.finishProcess(record, job, output, nil, processCtx)
		return
	}
	if closeErr := streams.closeChild(); closeErr != nil {
		output.err = closeErr
		cancel()
	}
	input.attach(streams.input, cmd.Process.Pid, output)
	var readers sync.WaitGroup
	readers.Add(len(streams.output))
	appendPipe := func(pipe *os.File, matcher *lineMatcher) {
		defer readers.Done()
		defer pipe.Close()
		buffer := make([]byte, pipeBytes)
		for {
			count, readErr := pipe.Read(buffer)
			if count > 0 {
				output.mu.Lock()
				if output.err == nil {
					output.err = output.accumulator.Append(buffer[:count])
					close(output.grew)
					output.grew = make(chan struct{})
					output.bytesSinceSync += count
					if output.err == nil && (output.lastSync.IsZero() || output.bytesSinceSync >= outputSyncBytes) {
						output.err = output.accumulator.Sync()
						if output.err == nil {
							manager.mu.Lock()
							record.outputBytes = output.accumulator.Bytes()
							manager.mu.Unlock()
						}
						output.bytesSinceSync = 0
						output.lastSync = time.Now()
					}
				}
				failed := output.err != nil
				notice := !failed && time.Since(output.lastNotice) >= 250*time.Millisecond
				if notice {
					output.lastNotice = time.Now()
				}
				output.mu.Unlock()
				if notice && manager.host.Publish != nil {
					manager.mu.Lock()
					runID := manager.run.RunID
					publishedJob := cloneJob(record.recovery.Job)
					available := record.outputBytes
					manager.mu.Unlock()
					if publishErr := manager.host.Publish(context.WithoutCancel(ctx), work.Event{Kind: "job_output", Job: &publishedJob, Bytes: available, RunID: runID}); publishErr != nil {
						manager.fail(publishErr)
						cancel()
					}
				}
				if failed {
					cancel()
				} else if matcher != nil {
					matcher.write(buffer[:count])
				}
			}
			if readErr != nil {
				// Linux reports a pty whose terminal side is closed by every
				// process as EIO: that is the end of its output.
				if readErr != io.EOF && !errors.Is(readErr, os.ErrClosed) && !(spec.PTY && errors.Is(readErr, syscall.EIO)) {
					output.mu.Lock()
					if output.err == nil {
						output.err = readErr
					}
					output.mu.Unlock()
					cancel()
				}
				return
			}
		}
	}
	for _, stream := range streams.output {
		var matcher *lineMatcher
		if record.service != nil && record.service.pattern != nil {
			matcher = &lineMatcher{manager: manager, record: record}
		}
		go appendPipe(stream, matcher)
	}
	if err = manager.running(context.WithoutCancel(ctx), record, cmd.Process.Pid, ""); err != nil {
		cancel()
	}
	pollCtx, stopPolling := context.WithCancel(processCtx)
	var pollers sync.WaitGroup
	if record.service != nil {
		readyDeadline := time.AfterFunc(record.service.timeout, func() {
			if manager.readinessFailed(record, true) {
				cancel()
			}
		})
		defer readyDeadline.Stop()
		if record.service.port != 0 {
			pollers.Go(func() { manager.watchPort(pollCtx, record) })
		}
	}
	// One Wait owner. The job worker remains available for bounded log syncing.
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var waitErr error
	waiting := true
	for waiting {
		select {
		case waitErr = <-waited:
			waiting = false
		case <-ticker.C:
			if !manager.syncOutput(record, output) {
				cancel()
			}
		}
	}
	if stdinErr := input.processExited(); stdinErr != nil {
		output.mu.Lock()
		if output.err == nil {
			output.err = stdinErr
		}
		output.mu.Unlock()
	}
	// A setsid descendant can inherit the pipes or terminal after the group
	// leader exits. It isn't ours to reattach, so close our read ends on the
	// same WaitDelay bound.
	closed := make(chan struct{})
	readerDeadline := time.AfterFunc(pipeJoinDelay, func() {
		for _, stream := range streams.output {
			stream.Close()
		}
		close(closed)
	})
	readers.Wait()
	if !readerDeadline.Stop() {
		<-closed
	}
	close(output.drained)
	stopPolling()
	pollers.Wait()
	code := cmd.ProcessState.ExitCode()
	job.ExitCode = &code
	if err != nil {
		output.mu.Lock()
		if output.err == nil {
			output.err = err
		}
		output.mu.Unlock()
	}
	manager.finishProcess(record, job, output, waitErr, processCtx)
}

// syncOutput commits the job's appended output so logs can page it, and
// reports false once the output has failed and the job must stop.
func (manager *Manager) syncOutput(record *entry, output *processOutput) bool {
	output.mu.Lock()
	defer output.mu.Unlock()
	if output.err == nil {
		output.err = output.accumulator.Sync()
		output.bytesSinceSync = 0
		if output.err == nil {
			manager.mu.Lock()
			record.outputBytes = output.accumulator.Bytes()
			manager.mu.Unlock()
		}
	}
	return output.err == nil
}

func (manager *Manager) finishProcess(record *entry, job work.Job, output *processOutput, waitErr error, ctx context.Context) {
	output.mu.Lock()
	finishErr := output.accumulator.Finish()
	syncErr := output.accumulator.Sync()
	snapshot, snapshotErr := output.accumulator.Snapshot(toolio.SnapshotOptions{})
	closeErr := output.accumulator.CloseTempFile()
	outputErr := errors.Join(output.err, finishErr, syncErr, snapshotErr, closeErr)
	output.mu.Unlock()
	job.ResultText = preview(snapshot.Content, maxPreviewBytes)
	if job.Reason == "" {
		job.State = "completed"
	}
	if waitErr != nil {
		job.State = "failed"
		job.Reason = "exit_nonzero"
	}
	if ctx.Err() != nil {
		job.State = "cancelled"
		job.Reason = "cancelled"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			job.State = "failed"
			job.Reason = "timeout"
		}
	}
	if outputErr != nil {
		job.State = "failed"
		job.Reason = "output_io"
		if errors.Is(outputErr, toolio.ErrOutputLimit) {
			job.Reason = "output_limit"
		}
		if job.ResultText == "" {
			job.ResultText = preview(outputErr.Error(), maxPreviewBytes)
		}
	}
	if artifact, err := hashLog(manager.jobPath(job.ID, "output.log")); err != nil {
		job.State = "failed"
		job.Reason = "output_io"
	} else {
		job.Output = &artifact
	}
	manager.readinessFailed(record, false)
	manager.mu.Lock()
	record.truncated = snapshot.Truncated
	// The worker's copy predates readiness; keep what observeReady recorded.
	job.Readiness = record.recovery.Job.Readiness
	job.ReadyMatch = record.recovery.Job.ReadyMatch
	if record.service != nil && record.service.timedOut {
		job.State = "failed"
		job.Reason = "ready_timeout"
	}
	manager.mu.Unlock()
	manager.settle(record, job)
}
func hashLog(path string) (work.Artifact, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return work.Artifact{}, err
	}
	defer file.Close()
	hash := sha256.New()
	count, err := io.Copy(hash, io.LimitReader(file, maxLogBytes+1))
	if err != nil {
		return work.Artifact{}, err
	}
	if count > maxLogBytes {
		return work.Artifact{}, toolio.ErrOutputLimit
	}
	return work.Artifact{Path: path, SHA256: hex.EncodeToString(hash.Sum(nil)), Bytes: count}, nil
}
