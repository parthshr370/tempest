package extension

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"go.harness.dev/harness/tempest/durable"
)

// GrantOptions binds host operations to one invocation. Callbacks retain host gates.
type GrantOptions struct {
	Identity     InvocationIdentity
	Read         func(context.Context, durable.Query) (durable.Page, error)
	Commit       func(context.Context, durable.Commit) (durable.Receipt, error)
	Memo         func(context.Context, string, json.RawMessage) (json.RawMessage, error)
	MemoInserted func(context.Context, string, json.RawMessage) (json.RawMessage, bool, error)
	ReadMemo     func(context.Context, string) (json.RawMessage, bool, error)
	Output       func(context.Context, OutputChunk) error
	CallTool     func(context.Context, string, string, json.RawMessage) (Result, error)
	Environment  func(context.Context) (EnvironmentLease, error)
	Namespaces   []string
}

// Grant revokes and joins callback operations before returning from Revoke.
type Grant struct {
	mu        sync.Mutex
	options   GrantOptions
	ctx       context.Context
	cancel    context.CancelFunc
	revoked   bool
	active    sync.WaitGroup
	callbacks int
	output    uint64
	bytes     int
}

func NewGrant(ctx context.Context, o GrantOptions) *Grant {
	ctx, cancel := context.WithCancel(ctx)
	o.Namespaces = append([]string(nil), o.Namespaces...)
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	o.Identity.Token = hex.EncodeToString(b)
	return &Grant{options: o, ctx: ctx, cancel: cancel}
}
func (g *Grant) Identity() InvocationIdentity { return g.options.Identity }
func (g *Grant) enter(ctx context.Context) (context.Context, func(), error) {
	g.mu.Lock()
	if g.revoked || g.ctx.Err() != nil || ctx.Err() != nil {
		g.mu.Unlock()
		return nil, nil, ErrExpired
	}
	if g.callbacks >= 1024 {
		g.mu.Unlock()
		return nil, nil, errors.New("extension callback limit exceeded")
	}
	g.callbacks++
	g.active.Add(1)
	g.mu.Unlock()
	combined, cancel := context.WithCancel(g.ctx)
	stop := context.AfterFunc(ctx, cancel)
	return combined, func() { stop(); cancel(); g.active.Done() }, nil
}
func (g *Grant) Revoke() { g.mu.Lock(); g.revoked = true; g.cancel(); g.mu.Unlock(); g.active.Wait() }
func (g *Grant) Read(ctx context.Context, q durable.Query) (durable.Page, error) {
	ctx, done, err := g.enter(ctx)
	if err != nil {
		return durable.Page{}, err
	}
	defer done()
	if q.SessionID != "" && q.SessionID != g.options.Identity.SessionID {
		return durable.Page{}, errors.New("foreign session read forbidden")
	}
	q.SessionID = g.options.Identity.SessionID
	allowed := false
	for _, namespace := range g.options.Namespaces {
		if q.Namespace == namespace {
			allowed = true
			break
		}
	}
	if !allowed || q.Kind != "" && q.Kind != durable.DocumentKind {
		return durable.Page{}, errors.New("read outside granted documents")
	}
	q.Kind = durable.DocumentKind
	if q.Limit == 0 {
		q.Limit = 100
	}
	if q.Limit > 256 || q.Limit < 1 {
		return durable.Page{}, errors.New("read limit invalid")
	}
	if g.options.Read == nil {
		return durable.Page{}, durable.ErrUnsupported
	}
	return g.options.Read(ctx, q)
}
func (g *Grant) Commit(ctx context.Context, c durable.Commit) (durable.Receipt, error) {
	ctx, done, err := g.enter(ctx)
	if err != nil {
		return durable.Receipt{}, err
	}
	defer done()
	if len(c.Changes) > 128 {
		return durable.Receipt{}, errors.New("commit change limit exceeded")
	}
	allowed := func(ns string) bool {
		for _, n := range g.options.Namespaces {
			if n == ns {
				return true
			}
		}
		return false
	}
	for _, ch := range c.Changes {
		if ch.SessionID != g.options.Identity.SessionID || ch.Kind != durable.DocumentKind || !allowed(ch.Namespace) {
			return durable.Receipt{}, errors.New("callback commit outside granted documents")
		}
	}
	for _, check := range c.Expected {
		if check.SessionID != g.options.Identity.SessionID || check.Kind != durable.DocumentKind || !allowed(check.Namespace) {
			return durable.Receipt{}, errors.New("callback comparison outside granted documents")
		}
	}
	if g.options.Commit == nil {
		return durable.Receipt{}, durable.ErrUnsupported
	}
	if c.ID == "" {
		return durable.Receipt{}, errors.New("callback commit ID required")
	}
	scope, _ := json.Marshal([]string{g.options.Identity.SessionID, g.options.Identity.InvocationID, g.options.Identity.ExtensionID, g.options.Identity.Handler, c.ID})
	sum := sha256.Sum256(scope)
	c.ID = fmt.Sprintf("callback:%x", sum[:])
	return g.options.Commit(ctx, c)
}
func (g *Grant) Memo(ctx context.Context, key string, value json.RawMessage) (json.RawMessage, error) {
	ctx, done, err := g.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	if key == "" || len(key) > 256 || len(value) > 64<<10 || !json.Valid(value) {
		return nil, errors.New("invalid memo key or value")
	}
	if g.options.Memo == nil {
		return nil, durable.ErrUnsupported
	}
	return g.options.Memo(ctx, key, value)
}

// MemoInserted reports whether this candidate won the durable insert.
func (g *Grant) MemoInserted(ctx context.Context, key string, value json.RawMessage) (json.RawMessage, bool, error) {
	ctx, done, err := g.enter(ctx)
	if err != nil {
		return nil, false, err
	}
	defer done()
	if key == "" || len(key) > 256 || len(value) > 64<<10 || !json.Valid(value) {
		return nil, false, errors.New("invalid memo key or value")
	}
	if g.options.MemoInserted == nil {
		return nil, false, durable.ErrUnsupported
	}
	return g.options.MemoInserted(ctx, key, value)
}
func (g *Grant) ReadMemo(ctx context.Context, key string) (json.RawMessage, bool, error) {
	ctx, done, err := g.enter(ctx)
	if err != nil {
		return nil, false, err
	}
	defer done()
	if key == "" || len(key) > 256 {
		return nil, false, errors.New("invalid memo key")
	}
	if g.options.ReadMemo == nil {
		return nil, false, durable.ErrUnsupported
	}
	return g.options.ReadMemo(ctx, key)
}
func (g *Grant) Output(ctx context.Context, ch OutputChunk) error {
	ctx, done, err := g.enter(ctx)
	if err != nil {
		return err
	}
	defer done()
	raw, err := json.Marshal(ch)
	if err != nil {
		return err
	}
	g.mu.Lock()
	if ch.Number != g.output+1 || g.bytes+len(raw) > 1<<20 {
		g.mu.Unlock()
		return errors.New("output sequence or byte limit exceeded")
	}
	g.output = ch.Number
	g.bytes += len(raw)
	g.mu.Unlock()
	if g.options.Output == nil {
		return durable.ErrUnsupported
	}
	return g.options.Output(ctx, ch)
}
func (g *Grant) CallTool(ctx context.Context, key, name string, args json.RawMessage) (Result, error) {
	ctx, done, err := g.enter(ctx)
	if err != nil {
		return Result{}, err
	}
	defer done()
	if key == "" || len(key) > 256 || !json.Valid(args) {
		return Result{}, errors.New("invalid nested call")
	}
	if g.options.CallTool == nil {
		return Result{}, durable.ErrUnsupported
	}
	return g.options.CallTool(ctx, key, name, args)
}
func (g *Grant) Environment(ctx context.Context) (EnvironmentLease, error) {
	ctx, done, err := g.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	if g.options.Environment == nil {
		return nil, durable.ErrUnsupported
	}
	environment, err := g.options.Environment(ctx)
	if err != nil {
		return nil, err
	}
	return scopedEnvironment{grant: g, environment: environment}, nil
}

type scopedEnvironment struct {
	grant       *Grant
	environment EnvironmentLease
}

func (environment scopedEnvironment) Identity() string { return environment.environment.Identity() }
func (environment scopedEnvironment) ReadFile(ctx context.Context, path string, offset int64, limit int) (string, error) {
	ctx, done, err := environment.grant.enter(ctx)
	if err != nil {
		return "", err
	}
	defer done()
	return environment.environment.ReadFile(ctx, path, offset, limit)
}

type continuation struct {
	mu      sync.Mutex
	used    bool
	expired bool
	active  sync.WaitGroup
	call    func(context.Context) (Result, error)
}

func (n *continuation) Call(ctx context.Context) (Result, error) {
	n.mu.Lock()
	if n.used || n.expired {
		n.mu.Unlock()
		return Result{}, ErrNextUsed
	}
	n.used = true
	n.active.Add(1)
	n.mu.Unlock()
	defer n.active.Done()
	return n.call(ctx)
}

// Execute runs ordered decorators around a pinned tool. Validation and intent belong outside this stack.
func Execute(ctx context.Context, tool Tool, call Call, inv Invocation, wraps []Wrap) (Result, error) {
	if len(wraps) > 32 {
		return Result{}, errors.New("wrapper stack limit exceeded")
	}
	var run func(context.Context, int) (Result, error)
	run = func(runCtx context.Context, i int) (Result, error) {
		pinned := call
		pinned.Arguments = append(json.RawMessage(nil), call.Arguments...)
		if i == len(wraps) {
			return tool.Execute(runCtx, pinned, inv)
		}
		n := &continuation{call: func(nextCtx context.Context) (Result, error) {
			if err := runCtx.Err(); err != nil {
				return Result{}, err
			}
			if nextCtx.Err() != nil {
				return Result{}, nextCtx.Err()
			}
			pinnedContext, cancel := context.WithCancel(runCtx)
			stop := context.AfterFunc(nextCtx, cancel)
			defer func() { stop(); cancel() }()
			return run(pinnedContext, i+1)
		}}
		defer func() {
			n.mu.Lock()
			n.expired = true
			n.mu.Unlock()
			n.active.Wait()
		}()
		return wraps[i].Handle(runCtx, pinned, inv, n)
	}
	result, err := run(ctx, 0)
	if err != nil {
		return result, err
	}
	if result.Control != "" && result.Control != "terminate" && result.Control != "handoff" {
		return Result{}, fmt.Errorf("invalid result control %q", result.Control)
	}
	return result, nil
}

// ApplyDecision validates the event-specific union and feeds rewrites to the next handler.
func ApplyDecision(input HookInput, d HookDecision) (HookInput, error) {
	switch d.Kind {
	case "", "continue":
		if len(d.Arguments) > 0 || d.Result != nil || d.System != nil || d.Continuation != "" {
			return input, errors.New("continue decision has payload")
		}
	case "deny":
		if input.Event != BeforeTool && input.Event != BeforeRequest && input.Event != BeforeCompaction {
			return input, errors.New("deny not allowed for observer")
		}
		return input, fmt.Errorf("%w: %s", ErrDenied, d.Reason)
	case "rewrite":
		switch input.Event {
		case BeforeTool:
			if input.Call == nil || len(d.Arguments) == 0 || !json.Valid(d.Arguments) || d.System != nil {
				return input, errors.New("invalid tool rewrite")
			}
			call := *input.Call
			call.Arguments = append(json.RawMessage(nil), d.Arguments...)
			input.Call = &call
		case BeforeRequest:
			if d.System == nil || len(d.Arguments) > 0 {
				return input, errors.New("invalid request rewrite")
			}
			input.System = *d.System
		default:
			return input, errors.New("rewrite not allowed for event")
		}
	case "redact":
		if input.Event != AfterTool || input.Result == nil || d.Result == nil {
			return input, errors.New("redaction requires a complete tool result")
		}
		redacted := *input.Result
		redacted.Content = d.Result.Content
		redacted.StructuredContent = d.Result.StructuredContent
		redacted.Details = d.Result.Details
		input.Result = &redacted
	case "continue_with":
		if input.Event != OnYield || d.Continuation == "" {
			return input, errors.New("invalid continuation")
		}
	default:
		return input, errors.New("invalid hook decision kind")
	}
	return input, nil
}
