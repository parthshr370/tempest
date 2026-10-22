package mcpwire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.harness.dev/harness/internal/mcpconfig"
	"go.harness.dev/harness/internal/obs"
	"go.harness.dev/harness/internal/rpc"
)

const stderrTailBytes = 64 << 10
const childExitGrace = time.Second
const diagnosticTailRunes = 2048

type tailBuffer struct {
	mu    sync.Mutex
	bytes []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	count := len(p)
	if len(p) >= stderrTailBytes {
		b.bytes = append(b.bytes[:0], p[len(p)-stderrTailBytes:]...)
	} else {
		excess := len(b.bytes) + len(p) - stderrTailBytes
		if excess > 0 {
			copy(b.bytes, b.bytes[excess:])
			b.bytes = b.bytes[:len(b.bytes)-excess]
		}
		b.bytes = append(b.bytes, p...)
	}
	return count, nil
}

type childStream struct {
	stdout io.ReadCloser
	stdin  io.WriteCloser
	once   sync.Once
}

func (s *childStream) Read(p []byte) (int, error)  { return s.stdout.Read(p) }
func (s *childStream) Write(p []byte) (int, error) { return s.stdin.Write(p) }
func (s *childStream) Close() error {
	var err error
	s.once.Do(func() { err = errors.Join(s.stdin.Close(), s.stdout.Close()) })
	return err
}

type stdioTransport struct {
	conn       *rpc.Conn
	cmd        *exec.Cmd
	stream     *childStream
	exited     chan struct{}
	served     chan struct{}
	stderr     tailBuffer
	closeOnce  sync.Once
	closeErr   error
	faultMu    sync.Mutex
	fault      error
	redactions []string
}

func dialStdio(ctx context.Context, cfg mcpconfig.Server, cwd string, handlers Handlers) (*stdioTransport, error) {
	command := exec.Command(cfg.Command, cfg.Args...)
	command.Dir = cwd
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	allowed := map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true, "TMP": true, "TEMP": true, "LANG": true, "LC_ALL": true, "LC_CTYPE": true, "SYSTEMROOT": true, "WINDIR": true, "USERPROFILE": true}
	environment := map[string]string{}
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok && (allowed[key] || strings.HasPrefix(key, "LC_")) {
			environment[key] = value
		}
	}
	for key, value := range cfg.Env {
		environment[key] = value
	}
	for key, value := range environment {
		command.Env = append(command.Env, key+"="+value)
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	command.Stdout = stdoutWriter
	wire := &stdioTransport{cmd: command, stream: &childStream{stdout: stdout, stdin: stdin}, exited: make(chan struct{}), served: make(chan struct{})}
	for _, value := range cfg.Env {
		if value != "" {
			wire.redactions = append(wire.redactions, value)
		}
	}
	for _, argument := range cfg.Args {
		if argument != "" {
			wire.redactions = append(wire.redactions, argument)
		}
		if _, value, ok := strings.Cut(argument, "="); ok && value != "" {
			wire.redactions = append(wire.redactions, value)
		}
	}
	sort.Slice(wire.redactions, func(i, j int) bool { return len(wire.redactions[i]) > len(wire.redactions[j]) })
	// os/exec drains this writer and joins its pipe goroutine in Wait.
	command.Stderr = &wire.stderr
	if err = ctx.Err(); err != nil {
		_ = stdoutWriter.Close()
		_ = wire.stream.Close()
		return nil, err
	}
	if err = command.Start(); err != nil {
		_ = stdoutWriter.Close()
		_ = wire.stream.Close()
		return nil, err
	}
	_ = stdoutWriter.Close()
	wire.conn = rpc.NewStreamWithLimit(wire.stream, MaxResponse)
	wire.conn.SetProtocolErrorHandler(func(err error) { wire.faultMu.Lock(); wire.fault = err; wire.faultMu.Unlock() })
	go func() {
		defer close(wire.served)
		_ = wire.conn.ServeOrdered(func(ctx context.Context, method string, params json.RawMessage) (any, *rpc.Error) {
			if len(rpc.RequestID(ctx)) == 0 {
				return rpc.Deferred(func() (any, *rpc.Error) {
					if handlers.Notify != nil {
						handlers.Notify(method, params)
					}
					return nil, nil
				}), nil
			}
			return rpc.Deferred(func() (any, *rpc.Error) { return handlers.Request(ctx, method, params) }), nil
		})
	}()
	go func() { _ = command.Wait(); close(wire.exited) }()
	return wire, nil
}
func (s *stdioTransport) alive() bool {
	select {
	case <-s.conn.Done():
		return false
	case <-s.exited:
		return false
	default:
		return true
	}
}
func (s *stdioTransport) initialized() {}
func (s *stdioTransport) call(ctx context.Context, id, method string, params any) (json.RawMessage, error) {
	frame, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      string `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{"2.0", id, method, params})
	if err != nil {
		return nil, err
	}
	if len(frame) > MaxRequest {
		return nil, errors.New("mcp_output_limit: outbound request exceeds 1 MiB")
	}
	result, err := s.conn.CallWithID(ctx, id, method, params)
	if err != nil {
		s.faultMu.Lock()
		fault := s.fault
		s.faultMu.Unlock()
		if fault != nil {
			return nil, fault
		}
		var remoteError *rpc.Error
		if !errors.As(err, &remoteError) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			s.stderr.mu.Lock()
			tail := strings.ToValidUTF8(string(s.stderr.bytes), "\uFFFD")
			s.stderr.mu.Unlock()
			for _, secret := range s.redactions {
				tail = strings.ReplaceAll(tail, secret, "[REDACTED]")
			}
			tail = obs.RedactTruncate(tail, diagnosticTailRunes)
			if tail != "" {
				return nil, fmt.Errorf("%w; server stderr tail: %s", err, tail)
			}
		}
	}
	return result, err
}
func (s *stdioTransport) notify(ctx context.Context, method string, params any) error {
	encoded, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{"2.0", method, params})
	if err != nil {
		return err
	}
	if len(encoded) > MaxRequest {
		return errors.New("mcp_output_limit: outbound notification exceeds 1 MiB")
	}
	return s.conn.Notify(ctx, method, params)
}
func (s *stdioTransport) close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		_ = s.stream.stdin.Close()
		wait := func() bool {
			timer := time.NewTimer(childExitGrace)
			defer timer.Stop()
			select {
			case <-s.exited:
				return true
			case <-timer.C:
				return false
			case <-ctx.Done():
				return false
			}
		}
		if !wait() {
			_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGTERM)
			if !wait() {
				_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
			}
		}
		// Descendants may still own inherited pipes even after the group leader exited.
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		_ = s.conn.Close()
		s.closeErr = errors.Join(s.closeErr, s.conn.Drain(ctx))
		select {
		case <-s.exited:
		case <-ctx.Done():
			s.closeErr = ctx.Err()
		}
		select {
		case <-s.served:
		case <-ctx.Done():
			s.closeErr = ctx.Err()
		}
	})
	return s.closeErr
}
