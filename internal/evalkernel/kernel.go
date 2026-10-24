// Package evalkernel owns a session's disposable Python and JavaScript processes.
package evalkernel

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"go.harness.dev/harness/internal/toolio"
)

//go:embed python.py
var pythonDriver string

//go:embed javascript.cjs
var javascriptDriver string

const (
	maxFrameBytes  = 1 << 20
	maxOutputBytes = 16 << 20
	interruptGrace = 2 * time.Second
	startupTimeout = 10 * time.Second
	transportGrace = 500 * time.Millisecond
)

// Request executes one cell. Timeout zero leaves it unbounded until cancellation.
type Request struct {
	Language string
	Code     string
	Timeout  time.Duration
	Reset    bool
}

type cellRequest struct {
	Code      string `json:"code"`
	Boundary  string `json:"boundary"`
	TimeoutMS int64  `json:"timeout_ms"`
}

type event struct {
	Event    string `json:"event"`
	Error    string `json:"error"`
	TimedOut bool   `json:"timed_out"`
}

// Result keeps output even when execution or transport fails.
type Result struct {
	Output   toolio.OutputSnapshot
	Failed   bool
	TimedOut bool
}

type kernel struct {
	cmd      *exec.Cmd
	request  *os.File
	response *os.File
	output   *os.File
	lifetime *os.File
	reader   *bufio.Reader
	waited   chan struct{}
	stopOnce sync.Once
	dead     bool // protected by the manager's language gate
}

type startup struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// Manager gives each language its own namespace. Close also kills descendants.
type Manager struct {
	mu        sync.Mutex
	cwd       string
	outputDir string
	kernels   map[string]*kernel
	lost      map[string]bool
	gates     map[string]chan struct{}
	starting  map[string]startup
	closed    bool
}

// New doesn't start a runtime. Lost languages require an explicit reset after resume.
func New(cwd, outputDir string, lost map[string]bool) *Manager {
	copied := make(map[string]bool, len(lost))
	for language, stateLost := range lost {
		copied[language] = stateLost
	}
	return &Manager{
		cwd: cwd, outputDir: outputDir, kernels: make(map[string]*kernel), lost: copied,
		gates:    map[string]chan struct{}{"py": make(chan struct{}, 1), "js": make(chan struct{}, 1)},
		starting: make(map[string]startup),
	}
}

// Close is safe during a cell and joins every process it owned.
func (m *Manager) Close() error {
	m.mu.Lock()
	m.closed = true
	starting := make([]<-chan struct{}, 0, len(m.starting))
	for _, pending := range m.starting {
		pending.cancel()
		starting = append(starting, pending.done)
	}
	kernels := make([]*kernel, 0, len(m.kernels))
	for _, process := range m.kernels {
		kernels = append(kernels, process)
	}
	m.mu.Unlock()
	for _, process := range kernels {
		process.stop()
	}
	for _, done := range starting {
		<-done
	}
	return nil
}

// MarkStateLost stops abandoned eval activity before a workspace rewind.
func (m *Manager) MarkStateLost() {
	m.mu.Lock()
	starting := make([]<-chan struct{}, 0, len(m.starting))
	for language, pending := range m.starting {
		pending.cancel()
		starting = append(starting, pending.done)
		m.lost[language] = true
	}
	kernels := m.kernels
	m.kernels = make(map[string]*kernel)
	for language := range kernels {
		m.lost[language] = true
	}
	m.mu.Unlock()
	for _, process := range kernels {
		process.stop()
	}
	for _, done := range starting {
		<-done
	}
}

// Eval serializes cells within a language, but Python and JS may run independently.
func (m *Manager) Eval(ctx context.Context, request Request) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if request.Language != "py" && request.Language != "js" {
		return Result{}, errors.New("language must be py or js")
	}
	if len(request.Code) > maxFrameBytes/2 {
		return Result{}, errors.New("eval code exceeds 512 KiB")
	}
	gate := m.gates[request.Language]
	select {
	case gate <- struct{}{}:
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	defer func() { <-gate }()
	process, err := m.acquire(ctx, request.Language, request.Reset)
	if err != nil {
		return Result{}, err
	}

	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	result, err := m.run(ctx, process, request)
	if process.dead {
		m.mu.Lock()
		if m.kernels[request.Language] == process {
			delete(m.kernels, request.Language)
			m.lost[request.Language] = true
		}
		m.mu.Unlock()
	}
	return result, err
}

func (m *Manager) acquire(ctx context.Context, language string, reset bool) (*kernel, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("eval session is closed")
	}
	if m.lost[language] && !reset {
		m.mu.Unlock()
		return nil, errors.New("eval kernel state was lost; use reset:true and re-run setup; previous cells will not be re-run automatically")
	}
	process := m.kernels[language]
	if process != nil && !reset {
		select {
		case <-process.waited:
			delete(m.kernels, language)
			m.lost[language] = true
			m.mu.Unlock()
			process.stop()
			return nil, errors.New("eval kernel exited; state was lost; use reset:true and re-run setup")
		default:
			m.mu.Unlock()
			return process, nil
		}
	}
	delete(m.kernels, language)
	startCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	m.starting[language] = startup{cancel: cancel, done: done}
	m.mu.Unlock()
	defer cancel()
	defer close(done)
	if process != nil {
		process.stop()
	}
	replacement, err := startKernel(startCtx, m.cwd, language)
	m.mu.Lock()
	delete(m.starting, language)
	if err == nil {
		err = startCtx.Err()
	}
	if err == nil && m.closed {
		err = errors.New("eval session is closed")
	}
	if err != nil {
		if reset {
			m.lost[language] = true
		}
		m.mu.Unlock()
		if replacement != nil {
			replacement.stop()
		}
		return nil, err
	}
	m.kernels[language] = replacement
	m.lost[language] = false
	m.mu.Unlock()
	return replacement, nil
}

func startKernel(ctx context.Context, cwd, language string) (*kernel, error) {
	runtime, driver, args := "python3", pythonDriver, []string{"-I", "-u", "-c"}
	if language == "js" {
		runtime, driver, args = "node", javascriptDriver, []string{"-e"}
		if _, err := exec.LookPath(runtime); err != nil {
			runtime = "bun"
		}
	}
	executable, err := exec.LookPath(runtime)
	if err != nil {
		return nil, fmt.Errorf("eval %s runtime missing: install %s (JS accepts node or bun)", language, runtime)
	}
	requestRead, requestWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	responseRead, responseWrite, err := os.Pipe()
	if err != nil {
		requestRead.Close()
		requestWrite.Close()
		return nil, err
	}
	outputRead, outputWrite, err := os.Pipe()
	if err != nil {
		requestRead.Close()
		requestWrite.Close()
		responseRead.Close()
		responseWrite.Close()
		return nil, err
	}
	lifeRead, lifeWrite, err := os.Pipe()
	if err != nil {
		requestRead.Close()
		requestWrite.Close()
		responseRead.Close()
		responseWrite.Close()
		outputRead.Close()
		outputWrite.Close()
		return nil, err
	}
	cmd := exec.Command(executable, append(args, driver)...)
	cmd.Dir = cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.ExtraFiles = []*os.File{requestRead, responseWrite, lifeRead}
	cmd.Stdout, cmd.Stderr = outputWrite, outputWrite
	process := &kernel{cmd: cmd, request: requestWrite, response: responseRead, output: outputRead, lifetime: lifeWrite, reader: bufio.NewReader(outputRead), waited: make(chan struct{})}
	err = cmd.Start()
	requestRead.Close()
	responseWrite.Close()
	outputWrite.Close()
	lifeRead.Close()
	if err != nil {
		requestWrite.Close()
		responseRead.Close()
		outputRead.Close()
		lifeWrite.Close()
		return nil, err
	}
	go func() { _ = cmd.Wait(); close(process.waited) }()
	ready := make(chan error, 1)
	go func() {
		response, err := readEvent(responseRead)
		if err == nil && response.Event != "ready" {
			err = fmt.Errorf("unexpected startup event %q", response.Event)
		}
		ready <- err
	}()
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case <-process.waited:
		select {
		case err = <-ready:
		default:
			err = errors.New("kernel exited before ready")
		}
	case err = <-ready:
	case <-time.After(startupTimeout):
		err = errors.New("runtime startup timed out")
	}
	if err != nil {
		_ = process.output.SetReadDeadline(time.Now().Add(transportGrace))
		startupOutput, _ := io.ReadAll(io.LimitReader(process.reader, maxFrameBytes))
		process.stop()
		return nil, fmt.Errorf("eval %s startup: %w; output: %s", language, err, startupOutput)
	}
	return process, nil
}

func (process *kernel) stop() {
	process.stopOnce.Do(func() {
		// The watchdog owns group termination while it still holds that group
		// alive. We never signal a possibly reaped/reused process-group ID.
		process.lifetime.Close()
		process.request.Close()
		process.response.Close()
		process.output.Close()
		select {
		case <-process.waited:
		case <-time.After(interruptGrace):
			_ = process.cmd.Process.Kill()
			<-process.waited
		}
	})
}

func (m *Manager) run(ctx context.Context, process *kernel, request Request) (Result, error) {
	nonce := make([]byte, 24)
	if _, err := rand.Read(nonce); err != nil {
		return Result{}, err
	}
	boundary := "\x00tempest-eval-" + hex.EncodeToString(nonce) + "\x00"
	payload, err := json.Marshal(cellRequest{Code: request.Code, Boundary: boundary, TimeoutMS: request.Timeout.Milliseconds()})
	if err != nil {
		return Result{}, err
	}
	accumulator := toolio.NewOutputAccumulator(toolio.OutputAccumulatorOptions{TempFilePrefix: "harness-eval", TempDir: m.outputDir, MaxTotalBytes: maxOutputBytes})
	defer accumulator.CloseTempFile()
	completed := make(chan error, 1)
	outputDone := make(chan error, 1)
	go func() { outputDone <- readCellOutput(process.reader, []byte(boundary), accumulator) }()
	go func() { completed <- writeFrame(process.request, payload) }()
	select {
	case err = <-completed:
	case <-ctx.Done():
		err = ctx.Err()
	case <-process.waited:
		err = errors.New("kernel exited while accepting request")
	case <-time.After(transportGrace):
		err = errors.New("kernel did not accept request")
	}
	if err != nil {
		process.dead = true
		process.stop()
		<-outputDone
		return Result{}, fmt.Errorf("eval outcome unknown; kernel state was lost: %w", err)
	}
	events := make(chan event, 2)
	protocolDone := make(chan error, 1)
	protocolStop := make(chan struct{})
	defer close(protocolStop)
	driverFinished := make(chan struct{})
	go func() {
		for {
			response, readErr := readEvent(process.response)
			if readErr != nil {
				protocolDone <- readErr
				return
			}
			if response.Event == "done" {
				close(driverFinished)
			}
			select {
			case events <- response:
			case <-protocolStop:
				return
			}
			if response.Event == "done" {
				protocolDone <- nil
				return
			}
		}
	}()
	var timeout <-chan time.Time
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	var grace <-chan time.Time
	var graceTimer *time.Timer
	defer func() {
		if graceTimer != nil {
			graceTimer.Stop()
		}
	}()
	settleTimer := time.NewTimer(transportGrace)
	defer settleTimer.Stop()
	settle := settleTimer.C
	settleReason := "kernel did not acknowledge cell"
	waited := process.waited
	startSettlement := func(reason string) {
		if settle == nil {
			settleTimer.Reset(transportGrace)
			settle = settleTimer.C
			settleReason = reason
		}
	}
	interrupted, timedOut := false, false
	contextDone := ctx.Done()
	var outputErr, transportErr error
	outputFinished, cellFinished := false, false
	cellError := ""
	interrupt := func() {
		if interrupted {
			return
		}
		interrupted = true
		_ = process.cmd.Process.Signal(syscall.SIGINT)
		graceTimer = time.NewTimer(interruptGrace)
		grace = graceTimer.C
	}
	for !outputFinished || !cellFinished {
		select {
		case response := <-events:
			switch response.Event {
			case "started":
				if !cellFinished && !outputFinished {
					settleTimer.Stop()
					settle = nil
				}
				if request.Timeout > 0 && timer == nil {
					// The driver's own deadline settles normal timeouts. This
					// fallback covers broken/uncooperative runtimes without
					// racing a completed cell's done frame.
					timer = time.NewTimer(request.Timeout + transportGrace)
					timeout = timer.C
				}
			case "done":
				cellFinished = true
				cellError = response.Error
				timedOut = timedOut || response.TimedOut
				timeout = nil
				startSettlement("kernel completed without an output boundary")
			default:
				transportErr = fmt.Errorf("unexpected eval event %q", response.Event)
			}
		case outputErr = <-outputDone:
			outputFinished = true
			outputDone = nil
			if !cellFinished {
				startSettlement("output ended without a done response")
			}
			if outputErr != nil {
				transportErr = outputErr
			}
		case protocolErr := <-protocolDone:
			protocolDone = nil
			if protocolErr != nil {
				transportErr = protocolErr
			}
		case <-timeout:
			timeout = nil
			select {
			case <-driverFinished:
			default:
				timedOut = true
				interrupt()
			}
		case <-contextDone:
			contextDone = nil
			interrupt()
		case <-grace:
			transportErr = errors.New("kernel did not settle after SIGINT")
		case <-waited:
			waited = nil
			startSettlement("kernel exited before cell completion")
		case <-settle:
			transportErr = errors.New(settleReason)
		}
		if transportErr != nil {
			process.dead = true
			process.stop()
			if !outputFinished {
				outputErr = <-outputDone
				outputFinished = true
			}
			break
		}
	}
	finishErr := accumulator.Finish()
	snapshot, snapshotErr := accumulator.Snapshot(toolio.SnapshotOptions{PersistIfTruncated: true})
	closeErr := accumulator.CloseTempFile()
	result := Result{Output: snapshot, Failed: cellError != "" || timedOut || interrupted || transportErr != nil, TimedOut: timedOut || cellError == "ERR_SCRIPT_EXECUTION_TIMEOUT"}
	if transportErr != nil {
		return result, fmt.Errorf("eval outcome unknown; kernel state was lost; use reset:true and re-run setup: %w", errors.Join(transportErr, finishErr, snapshotErr, closeErr))
	}
	if interrupted && ctx.Err() != nil {
		return result, errors.Join(errors.New("eval interrupted: outcome unknown; effects before interruption may have happened; the cell will not be re-run automatically"), finishErr, snapshotErr, closeErr)
	}
	return result, errors.Join(outputErr, finishErr, snapshotErr, closeErr)
}

func readEvent(reader io.Reader) (event, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return event{}, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size > maxFrameBytes {
		return event{}, errors.New("eval response frame exceeds 1 MiB")
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return event{}, err
	}
	var response event
	err := json.Unmarshal(payload, &response)
	return response, err
}

func writeFrame(writer io.Writer, payload []byte) error {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if _, err := io.Copy(writer, bytes.NewReader(header[:])); err != nil {
		return err
	}
	_, err := io.Copy(writer, bytes.NewReader(payload))
	return err
}

// readCellOutput keeps only the possible delimiter suffix between reads. The
// random delimiter bounds output, not the JSON protocol on its dedicated fd.
func readCellOutput(reader *bufio.Reader, boundary []byte, accumulator *toolio.OutputAccumulator) error {
	pending := make([]byte, 0, 4096+len(boundary))
	for {
		chunk, err := reader.ReadSlice(0)
		if errors.Is(err, bufio.ErrBufferFull) {
			err = nil
		}
		pending = append(pending, chunk...)
		if index := bytes.Index(pending, boundary); index >= 0 {
			return accumulator.Append(pending[:index])
		}
		keep := len(boundary) - 1
		if len(pending) > keep {
			flush := len(pending) - keep
			if appendErr := accumulator.Append(pending[:flush]); appendErr != nil {
				return appendErr
			}
			pending = append(pending[:0], pending[flush:]...)
		}
		if err != nil {
			return errors.Join(accumulator.Append(pending), err)
		}
	}
}
