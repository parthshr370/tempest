package jobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.harness.dev/harness/internal/work"
)

// ErrNameInUse reports a service name held by another running job.
var ErrNameInUse = errors.New("job_name_in_use")

const (
	maxNameBytes      = 64
	maxReadyLogBytes  = 1024
	maxReadyLineBytes = 4 << 10
	maxReadyMatch     = 256
	portPollInterval  = 100 * time.Millisecond
	portDialTimeout   = 250 * time.Millisecond
	readyTailBytes    = 2 << 10
)

var serviceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// service is a named job's readiness: every condition given must hold
// before StartBash returns. settled closes once the job is ready or can no
// longer become ready; the fields are guarded by Manager.mu.
type service struct {
	pattern  *regexp.Regexp
	port     int
	timeout  time.Duration
	settled  chan struct{}
	logMatch string
	portOpen bool
	timedOut bool
}

// validateService normalizes spec's service fields and compiles its readiness.
// A spec without a readiness condition has no service state.
func validateService(spec *work.BashSpec) (*service, error) {
	switch {
	case len(spec.Name) > maxNameBytes || spec.Name != "" && !serviceName.MatchString(spec.Name):
		return nil, fmt.Errorf("%w: name must be 1 to %d letters, digits, '.', '_' or '-', starting with a letter or digit", ErrInvalidInput, maxNameBytes)
	case len(spec.ReadyLog) > maxReadyLogBytes:
		return nil, fmt.Errorf("%w: ready_log is %d bytes; the limit is %d", ErrInvalidInput, len(spec.ReadyLog), maxReadyLogBytes)
	case spec.ReadyPort < 0 || spec.ReadyPort > 65535:
		return nil, fmt.Errorf("%w: ready_port must be between 1 and 65535", ErrInvalidInput)
	case spec.ReadyTimeoutMS < 0 || spec.ReadyTimeoutMS > work.MaxReadyTimeoutMS:
		return nil, fmt.Errorf("%w: ready_timeout_ms must be between 1 and %d", ErrInvalidInput, work.MaxReadyTimeoutMS)
	}
	if spec.ReadyLog == "" && spec.ReadyPort == 0 {
		// A deadline with nothing to wait for has no meaning. Ignoring it would
		// hide the caller's belief that the call blocks; inventing "wait for the
		// job to exit" would duplicate wait. Say which path fits instead.
		if spec.ReadyTimeoutMS != 0 {
			return nil, fmt.Errorf("%w: ready_timeout_ms is the deadline for ready_log (a regex an output line must match) or ready_port (a TCP port that must accept connections), and neither is set. To wait for the job to finish, drop ready_timeout_ms and call wait with the job's name or id once bash returns its handle", ErrInvalidInput)
		}
		return nil, nil
	}
	if spec.ReadyTimeoutMS == 0 {
		spec.ReadyTimeoutMS = work.DefaultReadyTimeoutMS
	}
	state := &service{port: spec.ReadyPort, timeout: time.Duration(spec.ReadyTimeoutMS) * time.Millisecond, settled: make(chan struct{})}
	if spec.ReadyLog != "" {
		pattern, err := regexp.Compile(spec.ReadyLog)
		if err != nil {
			return nil, fmt.Errorf("%w: ready_log is not a valid RE2 regular expression: %v", ErrInvalidInput, err)
		}
		state.pattern = pattern
	}
	return state, nil
}

// nameHolderLocked returns the running job holding name. A job whose
// cancellation was requested releases its name, so cancel then start
// restarts a service.
func (manager *Manager) nameHolderLocked(name string) string {
	for jobID, record := range manager.entries {
		if record.live && record.recovery.Job.Name == name && !record.recovery.CancelRequested && !terminal(record.recovery.Job.State) {
			return jobID
		}
	}
	return ""
}

// JobByName resolves a service name: its running job, else the one started last.
func (manager *Manager) JobByName(ctx context.Context, name string) (work.Job, error) {
	if err := ctx.Err(); err != nil {
		return work.Job{}, err
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if jobID := manager.nameHolderLocked(name); jobID != "" {
		return cloneJob(manager.entries[jobID].recovery.Job), nil
	}
	var found *work.Job
	for _, record := range manager.entries {
		job := record.recovery.Job
		if job.Name == name && (found == nil || job.StartedAt.After(found.StartedAt) || job.StartedAt.Equal(found.StartedAt) && job.ID > found.ID) {
			found = &job
		}
	}
	if found == nil {
		return work.Job{}, manager.notFoundLocked(fmt.Sprintf("no job named %q", name))
	}
	return cloneJob(*found), nil
}

// holdsRun reports whether a live job keeps the root run open. A ready
// service is infrastructure for the run, not work whose result it awaits.
func holdsRun(record *entry) bool {
	return record.live && !(record.service != nil && record.recovery.Job.Readiness == "ready")
}

// observeReady records one satisfied condition and settles readiness once
// every condition holds.
func (manager *Manager) observeReady(record *entry, line string, portOpen bool) {
	state := record.service
	manager.mu.Lock()
	select {
	case <-state.settled:
		manager.mu.Unlock()
		return
	default:
	}
	if line != "" && state.logMatch == "" {
		state.logMatch = preview(line, maxReadyMatch)
	}
	state.portOpen = state.portOpen || portOpen
	if state.pattern != nil && state.logMatch == "" || state.port != 0 && !state.portOpen {
		manager.mu.Unlock()
		return
	}
	var match []string
	if state.logMatch != "" {
		match = append(match, state.logMatch)
	}
	if state.port != 0 {
		match = append(match, "127.0.0.1:"+strconv.Itoa(state.port))
	}
	record.recovery.Job.Readiness = "ready"
	record.recovery.Job.ReadyMatch = strings.Join(match, "; ")
	close(state.settled)
	job := cloneJob(record.recovery.Job)
	manager.notifyLocked()
	manager.mu.Unlock()
	// publish records a failure as the manager's fault, which fails the run;
	// readiness itself is already settled.
	_ = manager.publish(context.Background(), job, "ready")
}

// readinessFailed settles readiness as not reached. It reports whether this
// call settled it, so only a real timeout kills the process.
func (manager *Manager) readinessFailed(record *entry, timedOut bool) bool {
	state := record.service
	if state == nil {
		return false
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	select {
	case <-state.settled:
		return false
	default:
	}
	state.timedOut = timedOut
	record.recovery.Job.Readiness = "not_ready"
	close(state.settled)
	manager.notifyLocked()
	return true
}

// watchPort polls 127.0.0.1:port until it accepts a connection.
func (manager *Manager) watchPort(ctx context.Context, record *entry) {
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(record.service.port))
	dialer := net.Dialer{Timeout: portDialTimeout}
	ticker := time.NewTicker(portPollInterval)
	defer ticker.Stop()
	for {
		if connection, err := dialer.DialContext(ctx, "tcp", address); err == nil {
			// A probe connection carries no data; only its acceptance matters.
			_ = connection.Close()
			manager.observeReady(record, "", true)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-record.service.settled:
			return
		case <-ticker.C:
		}
	}
}

// lineMatcher tests a service's ready_log against one pipe's output lines,
// including the unterminated tail, so a prompt without a newline matches.
type lineMatcher struct {
	manager *Manager
	record  *entry
	partial []byte
	done    bool
}

func (matcher *lineMatcher) write(chunk []byte) {
	if matcher.done {
		return
	}
	select {
	case <-matcher.record.service.settled:
		matcher.done = true
		return
	default:
	}
	matcher.partial = append(matcher.partial, chunk...)
	for {
		index := bytes.IndexByte(matcher.partial, '\n')
		if index < 0 {
			break
		}
		line := strings.TrimSuffix(string(matcher.partial[:index]), "\r")
		matcher.partial = matcher.partial[index+1:]
		if matcher.test(line) {
			return
		}
	}
	if len(matcher.partial) > maxReadyLineBytes {
		matcher.partial = append([]byte(nil), matcher.partial[len(matcher.partial)-maxReadyLineBytes:]...)
	}
	if len(matcher.partial) > 0 {
		matcher.test(string(matcher.partial))
	}
}

func (matcher *lineMatcher) test(line string) bool {
	if !matcher.record.service.pattern.MatchString(line) {
		return false
	}
	matcher.done = true
	matcher.manager.observeReady(matcher.record, strings.ToValidUTF8(line, "\ufffd"), false)
	return true
}

// awaitReady blocks StartBash until the service is ready, or returns the
// reason it never became ready together with its output tail.
func (manager *Manager) awaitReady(ctx context.Context, record *entry) (work.Job, error) {
	state := record.service
	select {
	case <-ctx.Done():
		return work.Job{}, ctx.Err()
	case <-state.settled:
	case <-record.done:
	}
	manager.mu.Lock()
	job := cloneJob(record.recovery.Job)
	timedOut := state.timedOut
	manager.mu.Unlock()
	if job.Readiness == "ready" {
		return job, nil
	}
	// Not ready: the worker is killing or has reaped the process; its
	// settled record carries the output tail.
	select {
	case <-ctx.Done():
		return work.Job{}, ctx.Err()
	case <-record.done:
	}
	manager.mu.Lock()
	job = cloneJob(record.recovery.Job)
	manager.mu.Unlock()
	label := job.ID
	if job.Name != "" {
		label = fmt.Sprintf("service %q (%s)", job.Name, job.ID)
	}
	var why string
	switch {
	case timedOut:
		why = fmt.Sprintf("%s was not ready within %d ms; the process was killed", label, int(state.timeout/time.Millisecond))
	case job.ExitCode != nil:
		why = fmt.Sprintf("%s exited with code %d before it was ready", label, *job.ExitCode)
	default:
		why = fmt.Sprintf("%s ended (%s) before it was ready", label, job.Reason)
	}
	tail := strings.TrimSpace(job.ResultText)
	if tail == "" {
		return job, errors.New(why + ". It printed no output.")
	}
	return job, fmt.Errorf("%s. Output tail:\n%s", why, tailPreview(tail, readyTailBytes))
}
