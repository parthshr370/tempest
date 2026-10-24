package dap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.harness.dev/harness/internal/toolio"
)

const (
	outputDiskLimit    = 64 << 20
	outputPreviewBytes = 32 << 10
	outputPreviewLines = 500
)

// Manager owns one adapter and the programs it launches for a session.
type Manager struct {
	mu        sync.Mutex
	cwd       string
	outputDir string
	adapters  map[string]Adapter
	admit     func(context.Context) (func(), error)
	active    *adapterProcess
	closed    bool
}

type adapterProcess struct {
	client        *client
	cmd           *exec.Cmd
	transport     io.Closer
	exited        chan struct{}
	attached      bool
	release       func()
	cleanupOnce   sync.Once
	cleanupErr    error
	programGroups map[int]bool
	outputMu      sync.Mutex
	output        *toolio.OutputAccumulator
	outputErr     error
	stateMu       sync.Mutex
	thread        int
	stopped       bool
	terminated    bool
	stateChanged  chan struct{}
}

// New validates adapter settings but starts no processes.
func New(cwd, outputDir string, adapters map[string]Adapter, admit func(context.Context) (func(), error)) (*Manager, error) {
	resolved, err := Resolve(adapters)
	if err != nil {
		return nil, err
	}
	return &Manager{cwd: cwd, outputDir: outputDir, adapters: resolved, admit: admit}, nil
}

// Close disconnects the debuggee, then joins owned processes before releasing the permit.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return m.stop()
}
func (m *Manager) stop() error {
	if m.active == nil {
		return nil
	}
	process := m.active
	m.active = nil
	return process.cleanup()
}

func (process *adapterProcess) cleanup() error {
	process.cleanupOnce.Do(func() {
		// Cleanup must still work after the caller's request context expires.
		if process.client != nil {
			ctx, cancel := context.WithTimeout(context.Background(), disconnectTimeout)
			args := json.RawMessage(`{"terminateDebuggee":true}`)
			if process.attached {
				args = json.RawMessage(`{"terminateDebuggee":false}`)
			}
			// A failed disconnect isn't a cleanup failure if forced teardown works.
			_, _ = process.client.request(ctx, "disconnect", args)
			cancel()
		}
		// debugpy launches the target in a separate process group. Kill it while
		// the adapter can still reap it; attached programs are never ours to kill.
		programErr := process.killPrograms()
		select {
		case <-process.exited:
		case <-time.After(disconnectGrace):
		}
		killErr := process.kill()
		if process.transport != nil {
			_ = process.transport.Close()
		}
		<-process.exited
		if process.client != nil {
			<-process.client.done
		}
		// A late process event may have arrived during disconnect.
		programErr = errors.Join(programErr, process.killPrograms())
		process.outputMu.Lock()
		process.cleanupErr = errors.Join(programErr, killErr, process.outputErr, process.output.CloseTempFile())
		process.outputMu.Unlock()
		// SIGKILL is asynchronous; don't release the permit before target exit.
		if !process.releaseIfGone() {
			go process.releaseWhenGone()
		}
	})
	return process.cleanupErr
}

func (process *adapterProcess) killPrograms() error {
	if process.attached {
		return nil
	}
	process.stateMu.Lock()
	defer process.stateMu.Unlock()
	var result error
	for group := range process.programGroups {
		result = errors.Join(result, killGroup(group))
	}
	return result
}

func (process *adapterProcess) releaseWhenGone() {
	ticker := time.NewTicker(disconnectGrace)
	defer ticker.Stop()
	for {
		if process.releaseIfGone() {
			return
		}
		<-ticker.C
	}
}

func (process *adapterProcess) releaseIfGone() bool {
	process.stateMu.Lock()
	for group := range process.programGroups {
		if errors.Is(syscall.Kill(-group, 0), syscall.ESRCH) {
			delete(process.programGroups, group)
		}
	}
	gone := len(process.programGroups) == 0
	process.stateMu.Unlock()
	if !gone {
		return false
	}
	process.release()
	return true
}

func killGroup(group int) error {
	err := syscall.Kill(-group, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func (process *adapterProcess) kill() error {
	return killGroup(process.cmd.Process.Pid)
}

func (process *adapterProcess) Write(chunk []byte) (int, error) {
	process.outputMu.Lock()
	defer process.outputMu.Unlock()
	err := process.output.Append(chunk)
	if errors.Is(err, toolio.ErrOutputLimit) {
		return len(chunk), nil
	}
	if err != nil {
		process.outputErr = err
	}
	return len(chunk), err
}
func (process *adapterProcess) event(event message) {
	switch event.Event {
	case "process":
		var body struct {
			PID int `json:"systemProcessId"`
		}
		if !process.attached && json.Unmarshal(event.Body, &body) == nil && body.PID > 0 {
			group, err := syscall.Getpgid(body.PID)
			if err == nil && group > 0 && group != syscall.Getpgrp() {
				process.stateMu.Lock()
				process.programGroups[group] = true
				process.stateMu.Unlock()
			}
		}
	case "output":
		var body struct {
			Output string `json:"output"`
		}
		if err := json.Unmarshal(event.Body, &body); err == nil {
			if _, err := process.Write([]byte(body.Output)); err != nil {
				process.outputMu.Lock()
				process.outputErr = err
				process.outputMu.Unlock()
			}
		}
	case "stopped":
		var body struct {
			ThreadID int `json:"threadId"`
		}
		if err := json.Unmarshal(event.Body, &body); err == nil {
			process.stateMu.Lock()
			process.thread = body.ThreadID
			process.stopped = true
			process.stateMu.Unlock()
		}
	case "continued":
		process.stateMu.Lock()
		process.stopped = false
		process.stateMu.Unlock()
	case "terminated", "exited":
		process.stateMu.Lock()
		process.terminated = true
		process.stateMu.Unlock()
	}
	select {
	case process.stateChanged <- struct{}{}:
	default:
	}
}

// Start runs initialize, launch or attach, and configurationDone in DAP order.
// The launch response may arrive only after configurationDone, so it stays pending.
func (m *Manager) Start(ctx context.Context, action, name, program string, arguments map[string]json.RawMessage) (body json.RawMessage, startErr error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.closed {
		return nil, errors.New("debug manager is closed")
	}
	if m.active != nil {
		return nil, errors.New("a debug session already exists; terminate it before launching another")
	}
	if name == "" {
		if strings.HasSuffix(program, ".py") {
			name = "debugpy"
		} else {
			name = "dlv"
		}
	}
	adapter, ok := m.adapters[name]
	if !ok {
		return nil, fmt.Errorf("debug adapter %q is not configured", name)
	}
	process, err := m.spawn(ctx, adapter, action == "attach")
	if err != nil {
		return nil, err
	}
	m.active = process
	success := false
	defer func() {
		if !success {
			process.outputMu.Lock()
			snapshot, outputErr := process.output.Snapshot(toolio.SnapshotOptions{PersistIfTruncated: true})
			process.outputMu.Unlock()
			startErr = errors.Join(startErr, outputErr, m.stop())
			if snapshot.Content != "" {
				startErr = fmt.Errorf("%w\nAdapter output: %s", startErr, snapshot.Content)
			}
		}
	}()
	capabilities, err := process.client.request(ctx, "initialize", json.RawMessage(`{"clientID":"tempest","adapterID":"tempest","pathFormat":"path","linesStartAt1":true,"columnsStartAt1":true,"supportsRunInTerminalRequest":false,"supportsVariableType":true}`))
	if err != nil {
		return nil, err
	}
	defaults := adapter.Launch
	if action == "attach" {
		defaults = adapter.Attach
	}
	merged := make(map[string]json.RawMessage, len(defaults)+len(arguments))
	for key, value := range defaults {
		merged[key] = value
	}
	for key, value := range arguments {
		merged[key] = value
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	waitLaunch, err := process.client.start(ctx, action, encoded)
	if err != nil {
		return nil, err
	}
	type launchResult struct {
		body json.RawMessage
		err  error
	}
	launchDone := make(chan launchResult, 1)
	go func() {
		response, responseErr := waitLaunch()
		launchDone <- launchResult{response, responseErr}
	}()
	var earlyLaunch *launchResult
waitInitialized:
	for {
		select {
		case <-process.client.initialized:
			break waitInitialized
		case outcome := <-launchDone:
			if outcome.err != nil {
				return nil, outcome.err
			}
			earlyLaunch = &outcome
		case <-process.client.done:
			return nil, errors.New("debug adapter exited before initialized")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	var support struct {
		ConfigurationDone bool `json:"supportsConfigurationDoneRequest"`
	}
	if err = json.Unmarshal(capabilities, &support); err != nil {
		return nil, err
	}
	if support.ConfigurationDone {
		if _, err = process.client.request(ctx, "configurationDone", json.RawMessage(`{}`)); err != nil {
			return nil, err
		}
	}
	if earlyLaunch != nil {
		body, err = earlyLaunch.body, earlyLaunch.err
	} else {
		outcome := <-launchDone
		body, err = outcome.body, outcome.err
	}
	if err != nil {
		return nil, err
	}
	if string(merged["stopOnEntry"]) == "true" {
		body, err = process.waitStop(ctx)
		if err != nil {
			return nil, err
		}
	}
	success = true
	if len(body) == 0 {
		body = json.RawMessage(`{}`)
	}
	return body, nil
}

func (m *Manager) spawn(ctx context.Context, adapter Adapter, attached bool) (*adapterProcess, error) {
	release := func() {}
	if m.admit != nil {
		var err error
		release, err = m.admit(ctx)
		if err != nil {
			return nil, err
		}
	}
	process := &adapterProcess{attached: attached, release: release, programGroups: map[int]bool{}, exited: make(chan struct{}), stateChanged: make(chan struct{}, 1), output: toolio.NewOutputAccumulator(toolio.OutputAccumulatorOptions{MaxBytes: outputPreviewBytes, MaxLines: outputPreviewLines, MaxTotalBytes: outputDiskLimit, TempDir: m.outputDir, TempFilePrefix: "debug-"})}
	args := append([]string(nil), adapter.Args...)
	var listener net.Listener
	if adapter.Transport == "reverse_tcp" {
		var err error
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			release()
			return nil, err
		}
		defer listener.Close()
		for i, arg := range args {
			args[i] = strings.ReplaceAll(arg, "{address}", listener.Addr().String())
		}
	}
	command := exec.Command(adapter.Command, args...)
	command.Dir = m.cwd
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Stderr = process
	command.WaitDelay = disconnectGrace
	var reader io.Reader
	var writer io.Writer
	if listener == nil {
		stdout, err := command.StdoutPipe()
		if err != nil {
			release()
			return nil, err
		}
		stdin, err := command.StdinPipe()
		if err != nil {
			release()
			return nil, err
		}
		reader, writer, process.transport = stdout, stdin, stdin
	} else {
		command.Stdout = process
	}
	process.cmd = command
	if err := command.Start(); err != nil {
		release()
		return nil, fmt.Errorf("start debug adapter %s: %w", adapter.Command, err)
	}
	go func() { _ = command.Wait(); close(process.exited) }()
	if listener != nil {
		acceptDone := make(chan struct{})
		go func() {
			select {
			case <-process.exited:
				_ = listener.Close()
			case <-acceptDone:
			}
		}()
		stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
		connection, err := listener.Accept()
		close(acceptDone)
		stop()
		if err != nil {
			killErr := process.kill()
			<-process.exited
			release()
			process.outputMu.Lock()
			snapshot, outputErr := process.output.Snapshot(toolio.SnapshotOptions{PersistIfTruncated: true})
			closeErr := process.output.CloseTempFile()
			process.outputMu.Unlock()
			return nil, fmt.Errorf("debug adapter didn't connect: %w\nAdapter output: %s", errors.Join(err, ctx.Err(), killErr, outputErr, closeErr), snapshot.Content)
		}
		reader, writer, process.transport = connection, connection, connection
	}
	process.client = newClient(reader, writer, process.event)
	go func() {
		<-process.exited
		_ = process.cleanup()
	}()
	return process, nil
}

// Request executes a DAP operation. No old adapter survives session resume.
func (m *Manager) Request(ctx context.Context, command string, arguments json.RawMessage) (json.RawMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("debug manager is closed")
	}
	if m.active == nil {
		return nil, errors.New("no debug session exists; debug sessions are gone after resume; explicitly launch or attach a new session")
	}
	process := m.active
	isControl := command == "continue" || command == "next" || command == "stepIn" || command == "stepOut" || command == "pause"
	if isControl {
		process.stateMu.Lock()
		process.stopped = false
		process.stateMu.Unlock()
	}
	body, err := process.client.request(ctx, command, arguments)
	if err != nil && ctx.Err() != nil {
		return body, errors.Join(err, m.stop())
	}
	if err == nil && isControl {
		body, err = process.waitStop(ctx)
		if errors.Is(err, context.DeadlineExceeded) {
			return json.RawMessage(`{"state":"running"}`), nil
		}
	}
	return body, err
}

func (process *adapterProcess) waitStop(ctx context.Context) (json.RawMessage, error) {
	for {
		process.stateMu.Lock()
		stopped, terminated, thread := process.stopped, process.terminated, process.thread
		process.stateMu.Unlock()
		if stopped || terminated {
			state := "stopped"
			if terminated {
				state = "terminated"
			}
			return json.Marshal(struct {
				State    string `json:"state"`
				ThreadID int    `json:"thread_id,omitempty"`
			}{state, thread})
		}
		select {
		case <-process.stateChanged:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-process.client.done:
			return nil, errors.New("debug adapter transport closed before the program stopped")
		}
	}
}

// Thread returns the stopped thread when an action omits its id.
func (m *Manager) Thread() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil {
		return 0
	}
	m.active.stateMu.Lock()
	defer m.active.stateMu.Unlock()
	return m.active.thread
}

// Output returns a bounded snapshot with a private spill path when truncated.
func (m *Manager) Output() (toolio.OutputSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil {
		return toolio.OutputSnapshot{}, errors.New("no debug session exists; debug sessions are gone after resume")
	}
	m.active.outputMu.Lock()
	defer m.active.outputMu.Unlock()
	snapshot, err := m.active.output.Snapshot(toolio.SnapshotOptions{PersistIfTruncated: true})
	return snapshot, errors.Join(err, m.active.outputErr)
}

const disconnectGrace = 100 * time.Millisecond
const disconnectTimeout = 500 * time.Millisecond

// Terminate disconnects (detaching attached targets), then joins owned processes.
func (m *Manager) Terminate(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil {
		return errors.New("no debug session exists; debug sessions are gone after resume")
	}
	return m.stop()
}

// BoundResponse prevents large variable trees from flooding the model context.
func (m *Manager) BoundResponse(response json.RawMessage) (toolio.OutputSnapshot, error) {
	output := toolio.NewOutputAccumulator(toolio.OutputAccumulatorOptions{MaxBytes: outputPreviewBytes, MaxLines: outputPreviewLines, TempDir: m.outputDir, TempFilePrefix: "debug-response-", MaxTotalBytes: outputDiskLimit})
	defer output.CloseTempFile()
	if err := output.Append(response); err != nil {
		return toolio.OutputSnapshot{}, err
	}
	return output.Snapshot(toolio.SnapshotOptions{PersistIfTruncated: true})
}

// Timeout bounds debugger requests, including adapter startup.
const Timeout = 30 * time.Second
