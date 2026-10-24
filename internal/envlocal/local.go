// Package envlocal runs the existing workspace operations on their owning host.
package envlocal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"go.harness.dev/harness/tempest/environment"
)

// Handler is one existing bounded built-in implementation.
type Handler func(context.Context, environment.Call) (json.RawMessage, error)

type invocation struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// Local retains the host's os.Root, anchors and mutation queues in its handlers.
type Local struct {
	root        string
	rootInfo    os.FileInfo
	ref         environment.Reference
	mu          sync.Mutex
	handlers    map[string]Handler
	active      map[string]*invocation
	closed      bool
	artifactDir string
}

// New identifies the actual root directory, including its filesystem generation.
func New(cwd string) (*Local, error) { return NewNamed(cwd, "") }

// NewNamed binds an operator name to the actual directory generation.
func NewNamed(cwd, id string) (*Local, error) {
	root, err := filepath.Abs(cwd)
	if err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("environment root is not a directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, errors.New("environment root has no filesystem identity")
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)))
	if id == "" {
		id = "local:" + root
		sum = sha256.Sum256([]byte(id))
	}
	local := &Local{root: root, rootInfo: info, ref: environment.Reference{ID: id, Generation: hex.EncodeToString(sum[:16]), Cwd: "."}, handlers: make(map[string]Handler), active: make(map[string]*invocation)}
	local.bindPrimitives()
	return local, nil
}

// Artifacts owns a private output directory for standalone and daemon leases.
func (l *Local) Artifacts() (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return "", errors.New("environment closed")
	}
	if l.artifactDir == "" {
		var err error
		l.artifactDir, err = os.MkdirTemp("", "tempest-env-")
		if err != nil {
			return "", err
		}
	}
	return l.artifactDir, nil
}

// Reference returns the root's stable workspace identity.
func (l *Local) Reference() environment.Reference { return l.ref }

// LocalRoot explicitly identifies a host-local lease.
func (l *Local) LocalRoot() string { return l.root }

// Capabilities lists services implemented on this workspace host.
func (l *Local) Capabilities() environment.Capabilities {
	capabilities := environment.Capabilities{environment.Files, environment.Search, environment.Exec, environment.Checkpoint, environment.Eval, environment.LSP, environment.Git, environment.AST, environment.Cancel}
	l.mu.Lock()
	if l.handlers["inspect"] != nil {
		capabilities = append(capabilities, environment.Permissions)
	}
	l.mu.Unlock()
	return capabilities
}

// Bind installs an existing implementation without a second filesystem convention.
func (l *Local) Bind(name string, h Handler) { l.mu.Lock(); defer l.mu.Unlock(); l.handlers[name] = h }

// Call fences identity before dispatch and tracks cancellation until the handler joins.
func (l *Local) Call(ctx context.Context, c environment.Call) (out json.RawMessage, err error) {
	l.mu.Lock()
	h := l.handlers[c.Operation]
	l.mu.Unlock()
	if h == nil {
		return nil, fmt.Errorf("%w: %s", environment.ErrUnsupported, c.Operation)
	}
	err = l.Run(ctx, c, func(callCtx context.Context) error { out, err = h(callCtx, c); return err })
	return out, err
}

// Run keeps in-process results native instead of serializing them through JSON.
func (l *Local) Run(ctx context.Context, c environment.Call, fn func(context.Context) error) error {
	if err := environment.Match(l.ref, c.Environment); err != nil {
		return err
	}
	if c.Key == "" {
		return errors.New("environment invocation key required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	currentRoot, err := os.Stat(l.root)
	if err != nil {
		return fmt.Errorf("%w: workspace root unavailable: %v", environment.ErrIdentity, err)
	}
	if !os.SameFile(l.rootInfo, currentRoot) {
		return fmt.Errorf("%w: workspace root was replaced", environment.ErrIdentity)
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return errors.New("environment closed")
	}
	if _, exists := l.active[c.Key]; exists {
		l.mu.Unlock()
		return errors.New("environment invocation already running")
	}
	callCtx, cancel := context.WithCancel(ctx)
	inv := &invocation{cancel: cancel, done: make(chan struct{})}
	l.active[c.Key] = inv
	l.mu.Unlock()
	defer func() { cancel(); l.mu.Lock(); delete(l.active, c.Key); close(inv.done); l.mu.Unlock() }()
	return fn(callCtx)
}

// Cancel confirms only after the selected live handler has stopped.
func (l *Local) Cancel(ctx context.Context, key string) (environment.Cancellation, error) {
	l.mu.Lock()
	inv := l.active[key]
	l.mu.Unlock()
	if inv == nil {
		return environment.Cancellation{}, errors.New("environment invocation is not live")
	}
	inv.cancel()
	select {
	case <-inv.done:
		return environment.Cancellation{Requested: true, Confirmed: true}, nil
	case <-ctx.Done():
		return environment.Cancellation{Requested: true}, ctx.Err()
	}
}

// Lookup is unavailable because local process memory isn't a durable receipt store.
func (l *Local) Lookup(context.Context, string) (environment.Outcome, error) {
	return environment.Outcome{}, environment.ErrUnsupported
}

// Close stops and joins all workspace calls before releasing the lease.
func (l *Local) Close() error {
	l.mu.Lock()
	l.closed = true
	active := make([]*invocation, 0, len(l.active))
	for _, inv := range l.active {
		inv.cancel()
		active = append(active, inv)
	}
	artifacts := l.artifactDir
	l.mu.Unlock()
	for _, inv := range active {
		<-inv.done
	}
	if artifacts != "" {
		return os.RemoveAll(artifacts)
	}
	return nil
}
