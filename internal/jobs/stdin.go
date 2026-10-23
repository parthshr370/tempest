package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"go.harness.dev/harness/internal/work"
)

const (
	// maxStdinBytes bounds one write_stdin text.
	maxStdinBytes = 64 << 10
	// maxStdinOutputBytes bounds the output one write_stdin returns, like a
	// default logs page; the newest bytes win and logs pages the rest.
	maxStdinOutputBytes = 16 << 10
	// stdinWriteTimeout bounds a write to a process that has stopped reading
	// its input, so a full pipe or terminal queue fails the call instead of
	// hanging it.
	stdinWriteTimeout = 5 * time.Second
	// stdinQuietPeriod is how long output must pause after it starts before
	// write_stdin treats the program as waiting for input.
	stdinQuietPeriod = 300 * time.Millisecond
	// interruptText is Ctrl-C. A terminal's line discipline turns it into
	// SIGINT; for a pipe job the manager sends the signal itself.
	interruptText = "\x03"
	// endOfFileByte is Ctrl-D, the terminal's default VEOF character: read
	// returns end of file when it starts a line.
	endOfFileByte = 0x04
)

// ptyEnvironment stops programs that see a terminal from paging output or
// painting it with colour and cursor codes; the job's log is read as text.
var ptyEnvironment = []string{"TERM=dumb", "NO_COLOR=1", "PAGER=cat", "GIT_PAGER=cat"}

// jobInput is a bash job's input side and write_stdin's read position in its
// output. The worker attaches the live process once it starts; a manager
// restored from a journal has none, since a process is never reattached.
type jobInput struct {
	pty bool
	// stdin reports a pipe or terminal write_stdin can write to. A job
	// started with neither stdin nor pty reads /dev/null.
	stdin   bool
	started chan struct{} // closed once the process started or failed to
	start   sync.Once
	exited  chan struct{} // closed once the process was waited for
	exit    sync.Once

	// Set before started closes, read-only after.
	file   *os.File // stdin pipe write end, the pty master the reader shares, or nil
	pid    int      // process group id: the job's own group or session
	output *processOutput

	stdinClosed atomic.Bool

	call   sync.Mutex // one write_stdin at a time per job; guards cursor
	cursor int64      // raw log offset write_stdin has returned output up to
}

func newJobInput(pty, stdin bool) *jobInput {
	input := &jobInput{pty: pty, stdin: pty || stdin, started: make(chan struct{}), exited: make(chan struct{})}
	// /dev/null is input already at its end; there is nothing to close.
	input.stdinClosed.Store(!input.stdin)
	return input
}

// attach publishes the started process; settleStart covers every path where
// none started.
func (input *jobInput) attach(file *os.File, pid int, output *processOutput) {
	input.file, input.pid, input.output = file, pid, output
	input.settleStart()
}

func (input *jobInput) settleStart() { input.start.Do(func() { close(input.started) }) }

// processExited releases the stdin pipe once nothing can read it. A pty
// master stays with the output reader, which closes it after the last byte.
func (input *jobInput) processExited() error {
	input.exit.Do(func() { close(input.exited) })
	if input.pty {
		return nil
	}
	return input.closeStdin()
}

func (input *jobInput) closeStdin() error {
	if !input.stdinClosed.CompareAndSwap(false, true) {
		return nil
	}
	return input.file.Close()
}

func (input *jobInput) hasExited() bool {
	select {
	case <-input.exited:
		return true
	default:
		return false
	}
}

// WriteStdin writes text to a running bash job, optionally ends its input,
// and returns the output produced since the previous WriteStdin on the job.
// It returns once output has been quiet for stdinQuietPeriod, request.Wait
// passes, or the job's output ends, whichever comes first.
func (manager *Manager) WriteStdin(ctx context.Context, jobID string, request work.StdinRequest) (work.StdinResult, error) {
	if len(request.Text) > maxStdinBytes {
		return work.StdinResult{}, fmt.Errorf("%w: text is %d bytes; write at most %d per call", ErrInvalidInput, len(request.Text), maxStdinBytes)
	}
	if request.Wait < 0 || request.Wait > work.MaxStdinWaitMS*time.Millisecond {
		return work.StdinResult{}, fmt.Errorf("%w: wait must be between 0 and %d ms", ErrInvalidInput, work.MaxStdinWaitMS)
	}
	record, err := manager.find(jobID)
	if err != nil {
		return work.StdinResult{}, err
	}
	manager.mu.Lock()
	job := cloneJob(record.recovery.Job)
	input := record.input
	manager.mu.Unlock()
	if job.Kind != "bash" {
		return work.StdinResult{}, fmt.Errorf("%w: write_stdin drives background bash jobs, and %s is a %s job; use message to reach a child", ErrInvalidInput, jobID, job.Kind)
	}
	if input == nil {
		return work.StdinResult{}, fmt.Errorf("%w: job %s is %s and its process is not attached to this session (a restart never reattaches one); read its output with logs", ErrInvalidInput, jobID, job.State)
	}
	select {
	case <-input.started:
	case <-ctx.Done():
		return work.StdinResult{}, ctx.Err()
	}
	if input.output == nil {
		return work.StdinResult{}, fmt.Errorf("%w: job %s never started a process; wait reports why", ErrInvalidInput, jobID)
	}
	input.call.Lock()
	defer input.call.Unlock()
	if err = input.send(jobID, request); err != nil {
		return work.StdinResult{}, err
	}
	if err = input.awaitQuiet(ctx, request.Wait); err != nil {
		return work.StdinResult{}, err
	}
	result, err := input.readNew(manager.jobPath(jobID, "output.log"))
	if err != nil {
		return work.StdinResult{}, err
	}
	// Commit what this call returned so logs offsets cover it; a sync
	// failure is the worker's to act on at its next tick.
	manager.syncOutput(record, input.output)
	manager.mu.Lock()
	result.Job = cloneJob(record.recovery.Job)
	manager.mu.Unlock()
	return result, nil
}

// send delivers the request's text and end of input.
func (input *jobInput) send(jobID string, request work.StdinRequest) error {
	if request.Text == "" && !request.CloseStdin {
		return nil
	}
	exited := fmt.Errorf("%w: job %s's process has exited and reads no more input; read its output with logs or wait", ErrInvalidInput, jobID)
	if input.hasExited() {
		return exited
	}
	if request.Text != "" {
		var err error
		switch {
		case !input.pty && request.Text == interruptText:
			err = syscall.Kill(-input.pid, syscall.SIGINT)
		case !input.stdin:
			return fmt.Errorf("%w: job %s reads its stdin from /dev/null, so it takes no input; start the command with bash stdin: true (or pty: true for a terminal) to write to it (text exactly \\u0003 still sends Ctrl-C)", ErrInvalidInput, jobID)
		default:
			err = input.write(jobID, []byte(request.Text))
		}
		if isProcessGone(err) {
			return exited
		}
		if err != nil {
			return err
		}
	}
	if !request.CloseStdin {
		return nil
	}
	if input.pty {
		// A terminal has no separate input to close; Ctrl-D at the start
		// of a line is how its reader sees end of file.
		err := input.write(jobID, []byte{endOfFileByte})
		if isProcessGone(err) {
			return exited
		}
		return err
	}
	return input.closeStdin()
}

func (input *jobInput) write(jobID string, text []byte) error {
	if input.stdinClosed.Load() {
		if input.hasExited() {
			return os.ErrClosed
		}
		return fmt.Errorf("%w: job %s's stdin is closed (close_stdin ended it); only a pty job reads input after close_stdin", ErrInvalidInput, jobID)
	}
	// Each write sets its own deadline, so an expired one never outlives it.
	if err := input.file.SetWriteDeadline(time.Now().Add(stdinWriteTimeout)); err != nil {
		return err
	}
	written, err := input.file.Write(text)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return fmt.Errorf("job %s did not read its input within %s; %d of %d bytes were written", jobID, stdinWriteTimeout, written, len(text))
	}
	return err
}

// isProcessGone reports a write or signal that failed because the process,
// or the descriptor the worker closed after it exited, is gone.
func isProcessGone(err error) bool {
	return errors.Is(err, os.ErrClosed) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.EIO) || errors.Is(err, syscall.ESRCH)
}

// awaitQuiet waits until output newer than the cursor has paused for
// stdinQuietPeriod, wait passes or the output ends. With no new output it
// waits the whole bound, since the program may still be working.
func (input *jobInput) awaitQuiet(ctx context.Context, wait time.Duration) error {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	quiet := time.NewTimer(stdinQuietPeriod)
	quiet.Stop()
	defer quiet.Stop()
	output := input.output
	counting := false
	for {
		output.mu.Lock()
		grew := output.grew
		pending := output.accumulator.Bytes() > input.cursor
		output.mu.Unlock()
		if pending && !counting {
			counting = true
			quiet.Reset(stdinQuietPeriod)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return nil
		case <-output.drained:
			return nil
		case <-quiet.C:
			return nil
		case <-grew:
			if counting {
				quiet.Reset(stdinQuietPeriod)
			}
		}
	}
}

// readNew returns the output after the cursor, at most maxStdinOutputBytes
// of the newest, and advances the cursor past it. A rune split by a live
// stream stays for the next read.
func (input *jobInput) readNew(path string) (work.StdinResult, error) {
	output := input.output
	output.mu.Lock()
	end := output.accumulator.Bytes()
	output.mu.Unlock()
	drained := false
	select {
	case <-output.drained:
		drained = true
	default:
	}
	start := max(input.cursor, end-maxStdinOutputBytes)
	result := work.StdinResult{Offset: start, NextOffset: start, OmittedBytes: start - input.cursor, StdinClosed: input.stdinClosed.Load()}
	if start == end {
		input.cursor = end
		return result, nil
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return work.StdinResult{}, err
	}
	defer file.Close()
	buffer := make([]byte, end-start)
	count, err := file.ReadAt(buffer, start)
	if err != nil && err != io.EOF {
		return work.StdinResult{}, err
	}
	buffer = buffer[:count]
	if result.OmittedBytes > 0 {
		skip := 0
		for skip < len(buffer) && skip < utf8.UTFMax && !utf8.RuneStart(buffer[skip]) {
			skip++
		}
		buffer = buffer[skip:]
		result.Offset += int64(skip)
		result.OmittedBytes += int64(skip)
	}
	if !drained {
		buffer = buffer[:completeRunes(buffer)]
	}
	result.Output = strings.ToValidUTF8(string(buffer), "\ufffd")
	result.NextOffset = result.Offset + int64(len(buffer))
	input.cursor = result.NextOffset
	return result, nil
}

// completeRunes is the length of buffer without a trailing rune whose
// remaining bytes have not arrived yet.
func completeRunes(buffer []byte) int {
	for index := len(buffer) - 1; index >= 0 && index >= len(buffer)-utf8.UTFMax; index-- {
		if utf8.RuneStart(buffer[index]) {
			if utf8.FullRune(buffer[index:]) {
				return len(buffer)
			}
			return index
		}
	}
	return len(buffer)
}
