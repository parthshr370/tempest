// Package envrpc carries workspace operations over bounded JSON-RPC streams.
package envrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.harness.dev/harness/internal/rpc"
	"go.harness.dev/harness/tempest/environment"
)

// ProtocolVersion pins the environment wire contract independently of ACP.
const ProtocolVersion = "tempest-environment/1"
const prefix = "_tempest/extension/env/"
const maxFrameSize = 8 << 20

// ProcessOptions contains operator authority, never model-provided launch text.
type ProcessOptions struct {
	Argv         []string
	Env          []string
	Cwd          string
	Stderr       io.Writer
	Timeout      time.Duration
	AdmitProcess func(context.Context) (func(), error)
}

type initialize struct {
	ProtocolVersion string                   `json:"protocolVersion"`
	MaxFrameSize    int                      `json:"maxFrameSize"`
	Environment     environment.Reference    `json:"environment"`
	Capabilities    environment.Capabilities `json:"capabilities"`
}

type pipe struct {
	io.Reader
	io.Writer
	in, out io.Closer
}

func (p *pipe) Close() error { return errors.Join(p.in.Close(), p.out.Close()) }

type logWriter struct {
	mu sync.Mutex
	n  int
	w  io.Writer
}

func (l *logWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(p)
	if len(p) > l.n {
		p = p[:l.n]
	}
	l.n -= len(p)
	if l.w != nil && len(p) > 0 {
		_, _ = l.w.Write(p)
	}
	return n, nil
}

// Remote owns one negotiated daemon connection. It never retries a call.
type Remote struct {
	conn  *rpc.Conn
	ref   environment.Reference
	caps  environment.Capabilities
	close func() error
}

// Connect negotiates a caller-owned stream and closes it on handshake failure.
func Connect(ctx context.Context, stream io.ReadWriteCloser, want environment.Reference) (*Remote, error) {
	r := &Remote{conn: rpc.NewStreamWithLimit(stream, maxFrameSize)}
	r.close = r.conn.Close
	r.conn.SetProtocolErrorHandler(func(error) { go r.conn.Close() })
	go func() {
		_ = r.conn.Serve(func(context.Context, string, json.RawMessage) (any, *rpc.Error) {
			return nil, rpc.NewError(-32601, "unsupported", "environment daemon callbacks are not supported")
		})
	}()
	offered := environment.Capabilities{environment.Files, environment.Search, environment.Exec, environment.Cancel, environment.Lookup, environment.Permissions}
	var response initialize
	if err := r.conn.Call(ctx, "initialize", initialize{ProtocolVersion: ProtocolVersion, MaxFrameSize: maxFrameSize, Environment: want, Capabilities: offered}, &response); err != nil {
		_ = r.Close()
		return nil, classifyError(err)
	}
	if response.ProtocolVersion != ProtocolVersion || response.MaxFrameSize != maxFrameSize {
		_ = r.Close()
		return nil, errors.New("environment protocol version or frame limit mismatch")
	}
	if err := environment.Match(want, response.Environment); err != nil {
		_ = r.Close()
		return nil, err
	}
	for _, c := range response.Capabilities {
		if !offered.Has(c) {
			_ = r.Close()
			return nil, fmt.Errorf("unoffered environment capability %q", c)
		}
		if !r.caps.Has(c) {
			r.caps = append(r.caps, c)
		}
	}
	r.ref = response.Environment
	if err := r.conn.Notify(ctx, "notifications/initialized", struct{}{}); err != nil {
		_ = r.Close()
		return nil, err
	}
	return r, nil
}

func signalProcessGroupUnlessDone(done <-chan struct{}, pid int, signal syscall.Signal, kill func(int, syscall.Signal) error) error {
	select {
	case <-done:
		return nil
	default:
		return kill(-pid, signal)
	}
}

// Launch starts exactly the configured argv, without a shell or inherited secrets.
func Launch(ctx context.Context, o ProcessOptions, want environment.Reference) (*Remote, error) {
	if len(o.Argv) == 0 || o.Argv[0] == "" {
		return nil, errors.New("environment daemon argv required")
	}
	if o.Timeout <= 0 {
		o.Timeout = 30 * time.Second
	}
	cmd := exec.Command(o.Argv[0], o.Argv[1:]...)
	cmd.Dir = o.Cwd
	if o.Env != nil {
		cmd.Env = append([]string{}, o.Env...)
	} else {
		for _, entry := range os.Environ() {
			name, _, _ := strings.Cut(entry, "=")
			switch name {
			case "PATH", "HOME", "TMPDIR", "TMP", "TEMP", "LANG", "LC_ALL", "LC_CTYPE", "SSH_AUTH_SOCK", "USER", "LOGNAME":
				cmd.Env = append(cmd.Env, entry)
			}
		}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stderr = &logWriter{n: 64 << 10, w: o.Stderr}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		_ = in.Close()
		return nil, err
	}
	release := func() {}
	if o.AdmitProcess != nil {
		release, err = o.AdmitProcess(ctx)
		if err != nil {
			_ = in.Close()
			_ = out.Close()
			return nil, err
		}
	}
	if err = cmd.Start(); err != nil {
		release()
		_ = in.Close()
		_ = out.Close()
		return nil, err
	}
	exited, done := make(chan struct{}), make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited); release(); close(done) }()
	var once sync.Once
	stop := func() error {
		once.Do(func() {
			select {
			case <-exited:
				_ = in.Close()
				_ = out.Close()
				<-done
				return
			default:
			}
			_ = signalProcessGroupUnlessDone(exited, cmd.Process.Pid, syscall.SIGTERM, syscall.Kill)
			_ = in.Close()
			_ = out.Close()
			select {
			case <-done:
			case <-time.After(time.Second):
				_ = signalProcessGroupUnlessDone(exited, cmd.Process.Pid, syscall.SIGKILL, syscall.Kill)
				<-done
			}
		})
		return nil
	}
	handshake, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	r, err := Connect(handshake, &pipe{Reader: out, Writer: in, in: in, out: out}, want)
	if err != nil {
		_ = stop()
		return nil, err
	}
	r.close = func() error { err := r.conn.Close(); return errors.Join(err, stop()) }
	return r, nil
}

// Reference returns the negotiated workspace generation.
func (r *Remote) Reference() environment.Reference { return r.ref }

// Capabilities returns a detached negotiated service set.
func (r *Remote) Capabilities() environment.Capabilities {
	return append(environment.Capabilities(nil), r.caps...)
}

// Close retires the connection and its operator-launched process lease.
func (r *Remote) Close() error { return r.close() }

// Call returns unknown after a dispatched call loses its response. Lookup is explicit.
func (r *Remote) Call(ctx context.Context, c environment.Call) (json.RawMessage, error) {
	if err := environment.Match(r.ref, c.Environment); err != nil {
		return nil, err
	}
	if c.Key == "" {
		return nil, errors.New("environment invocation key required")
	}
	cap := environment.ToolCapability(c.Operation)
	if cap == "" || !r.caps.Has(cap) {
		return nil, fmt.Errorf("%w: %s", environment.ErrUnsupported, c.Operation)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var result json.RawMessage
	err := r.conn.Call(ctx, prefix+"call", c, &result)
	if err == nil {
		return result, nil
	}
	if errors.Is(err, rpc.ErrNotSent) {
		return nil, err
	}
	var remoteError *rpc.Error
	if errors.As(err, &remoteError) {
		if errorName(remoteError) == "frame_too_large" {
			return nil, fmt.Errorf("%w: %v", environment.ErrUnknown, err)
		}
		return nil, classifyError(err)
	}
	if ctx.Err() != nil && r.caps.Has(environment.Cancel) {
		cancelCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, _ = r.Cancel(cancelCtx, c.Key)
		cancel()
	}
	return nil, fmt.Errorf("%w: %v", environment.ErrUnknown, err)
}

type keyRequest struct {
	Environment environment.Reference `json:"environment"`
	Key         string                `json:"key"`
}

// Cancel confirms only an explicit daemon acknowledgment, never a pipe close.
func (r *Remote) Cancel(ctx context.Context, key string) (environment.Cancellation, error) {
	if !r.caps.Has(environment.Cancel) {
		return environment.Cancellation{}, environment.ErrUnsupported
	}
	var out environment.Cancellation
	err := r.conn.Call(ctx, prefix+"cancel", keyRequest{r.ref, key}, &out)
	if err != nil {
		return environment.Cancellation{Requested: true}, classifyError(err)
	}
	if !out.Requested && out.Confirmed {
		return environment.Cancellation{}, errors.New("invalid environment cancellation acknowledgment")
	}
	return out, nil
}

// Lookup asks only a daemon that explicitly negotiated durable result lookup.
func (r *Remote) Lookup(ctx context.Context, key string) (environment.Outcome, error) {
	if !r.caps.Has(environment.Lookup) {
		return environment.Outcome{}, environment.ErrUnsupported
	}
	var out environment.Outcome
	err := r.conn.Call(ctx, prefix+"lookup", keyRequest{r.ref, key}, &out)
	if err != nil {
		return environment.Outcome{}, classifyError(err)
	}
	if out.Complete && !out.Found {
		return environment.Outcome{}, errors.New("invalid environment lookup receipt")
	}
	return out, nil
}
