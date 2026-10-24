package envlocal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"go.harness.dev/harness/tempest/environment"
)

const maxPrimitiveBytes = 64 << 10
const defaultExecTimeout = 10 * time.Minute

func (l *Local) bindPrimitives() {
	l.Bind("read_bytes", l.readBytes)
	l.Bind("stat", l.stat)
	l.Bind("exec_argv", l.execArgv)
}

func (l *Local) readBytes(ctx context.Context, call environment.Call) (json.RawMessage, error) {
	var request environment.ReadRequest
	if err := json.Unmarshal(call.Arguments, &request); err != nil {
		return nil, err
	}
	if request.Offset < 0 || request.Limit < 1 || request.Limit > maxPrimitiveBytes {
		return nil, errors.New("environment read range exceeds bounds")
	}
	root, err := os.OpenRoot(l.root)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	file, err := root.Open(request.Path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	contents := make([]byte, request.Limit)
	size, err := file.ReadAt(contents, request.Offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if !utf8.Valid(contents[:size]) {
		return nil, errors.New("environment text read requires UTF-8 boundaries")
	}
	return json.Marshal(environment.ReadResult{Text: string(contents[:size])})
}

func (l *Local) stat(ctx context.Context, call environment.Call) (json.RawMessage, error) {
	var request environment.StatRequest
	if err := json.Unmarshal(call.Arguments, &request); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(l.root)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	info, err := root.Stat(request.Path)
	if err != nil {
		return nil, err
	}
	return json.Marshal(environment.FileInfo{Size: info.Size(), Mode: uint32(info.Mode()), Directory: info.IsDir()})
}

type execOutput struct {
	mu        sync.Mutex
	bytes     []byte
	remaining int
	truncated bool
}

func (output *execOutput) Write(chunk []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	size := len(chunk)
	retained := min(size, output.remaining)
	output.bytes = append(output.bytes, chunk[:retained]...)
	output.remaining -= retained
	output.truncated = output.truncated || retained < size
	return size, nil
}

func (l *Local) execArgv(ctx context.Context, call environment.Call) (json.RawMessage, error) {
	var request environment.ExecRequest
	if err := json.Unmarshal(call.Arguments, &request); err != nil {
		return nil, err
	}
	if len(request.Argv) == 0 || request.Argv[0] == "" {
		return nil, errors.New("environment exec requires argv")
	}
	if request.MaxBytes == 0 {
		request.MaxBytes = maxPrimitiveBytes
	}
	if request.MaxBytes < 1 || request.MaxBytes > maxPrimitiveBytes || request.TimeoutSeconds < 0 || request.TimeoutSeconds > int(defaultExecTimeout/time.Second) {
		return nil, errors.New("environment exec limits exceed bounds")
	}
	timeout := defaultExecTimeout
	if request.TimeoutSeconds > 0 {
		timeout = time.Duration(request.TimeoutSeconds) * time.Second
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(commandCtx, request.Argv[0], request.Argv[1:]...)
	command.Dir = l.root
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	command.WaitDelay = time.Second
	output := &execOutput{remaining: request.MaxBytes}
	command.Stdout = output
	command.Stderr = output
	err := command.Run()
	if commandCtx.Err() != nil {
		return nil, commandCtx.Err()
	}
	result := environment.ExecResult{Output: string(output.bytes), Truncated: output.truncated}
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return nil, err
		}
		result.ExitCode = exit.ExitCode()
	}
	return json.Marshal(result)
}
