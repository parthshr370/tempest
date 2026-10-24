package extension

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
	"sync/atomic"
	"syscall"
	"time"

	"go.harness.dev/harness/internal/rpc"
	"go.harness.dev/harness/tempest/durable"
)

const ProtocolVersion = "tempest-extension/1"
const methodPrefix = "_tempest/extension/"

// ProcessOptions is executable operator configuration, never session state.
type ProcessOptions struct {
	ID           string
	Argv         []string
	Env          []string
	Cwd          string
	Capabilities []string
	Timeout      time.Duration
	ReadTools    []string
	SafeHandlers []string
	Stderr       io.Writer                             `json:"-"`
	AdmitProcess func(context.Context) (func(), error) `json:"-"`
}
type process struct {
	cmd          *exec.Cmd
	conn         *rpc.Conn
	done         chan struct{}
	mu           sync.Mutex
	grants       map[string]processGrant
	capabilities map[string]bool
	workers      chan struct{}
	timeout      time.Duration
	once         sync.Once
	nextID       atomic.Uint64
	closed       atomic.Bool
}
type processGrant struct {
	invocation Invocation
	view       ReadView
	next       Next
	cancel     context.CancelFunc
	budget     *callBudget
	ctx        context.Context
}
type processPipe struct {
	io.Reader
	io.Writer
	stdin  io.Closer
	stdout io.Closer
}

func (p *processPipe) Close() error { return errors.Join(p.stdin.Close(), p.stdout.Close()) }

type boundedLog struct {
	mu        sync.Mutex
	remaining int
	writer    io.Writer
}

func (b *boundedLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if len(p) > b.remaining {
		p = p[:b.remaining]
	}
	b.remaining -= len(p)
	if b.writer != nil && len(p) > 0 {
		_, _ = b.writer.Write(p)
	}
	return n, nil
}

type callBudget struct {
	mu       sync.Mutex
	timer    *time.Timer
	deadline time.Time
	finished bool
}

func (b *callBudget) pause() func() {
	b.mu.Lock()
	stopped := b.timer.Stop()
	remaining := time.Until(b.deadline)
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if stopped && !b.finished {
			b.deadline = time.Now().Add(remaining)
			b.timer.Reset(remaining)
		}
	}
}

func (b *callBudget) finish() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.finished = true
	b.timer.Stop()
}

// InstallProcess launches only an explicitly operator-selected argv. No project discovery occurs.
func (r *Registry) InstallProcess(ctx context.Context, o ProcessOptions) error {
	if !identifier.MatchString(o.ID) || len(o.Argv) == 0 || o.Argv[0] == "" {
		return errors.New("operator extension ID and argv required")
	}
	if o.Timeout <= 0 {
		o.Timeout = 30 * time.Second
	}
	cmd := exec.Command(o.Argv[0], o.Argv[1:]...)
	cmd.Dir = o.Cwd
	cmd.Env = processEnvironment()
	if o.Env != nil {
		cmd.Env = append([]string(nil), o.Env...)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stderr = &boundedLog{remaining: 64 << 10, writer: o.Stderr}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return err
	}
	p := &process{cmd: cmd, done: make(chan struct{}), grants: map[string]processGrant{}, capabilities: map[string]bool{}, workers: make(chan struct{}, 32), timeout: o.Timeout}
	p.conn = rpc.NewStream(&processPipe{Reader: stdout, Writer: stdin, stdin: stdin, stdout: stdout})
	p.conn.SetProtocolErrorHandler(func(error) { go p.close(context.Background()) })
	release := func() {}
	if o.AdmitProcess != nil {
		release, err = o.AdmitProcess(ctx)
		if err != nil {
			_ = p.conn.Close()
			return err
		}
	}
	if err = cmd.Start(); err != nil {
		release()
		_ = p.conn.Close()
		return err
	}
	go func() { _ = cmd.Wait(); p.closed.Store(true); release(); close(p.done); _ = p.conn.Close() }()
	go func() { _ = p.conn.ServeOrdered(p.admit) }()
	ok := false
	defer func() {
		if !ok {
			_ = p.close(context.Background())
		}
	}()
	offered := map[string]bool{}
	for _, c := range o.Capabilities {
		switch c {
		case "read", "commit", "memo", "output", "call_tool", "next", "environment_read", "tasks":
			offered[c] = true
		default:
			return fmt.Errorf("unsupported host capability %q", c)
		}
	}
	handshake, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	var response struct {
		ProtocolVersion string     `json:"protocolVersion"`
		ID              string     `json:"id"`
		Revision        string     `json:"revision"`
		Capabilities    []string   `json:"capabilities"`
		Definition      Definition `json:"definition"`
	}
	request := initializeRequest{ProtocolVersion: ProtocolVersion, MaxFrameSize: rpc.MaxFrame, Capabilities: o.Capabilities}
	request.Host.Name = "tempest"
	if err = p.conn.Call(handshake, "initialize", request, &response); err != nil {
		return err
	}
	if response.ProtocolVersion != ProtocolVersion || response.ID != o.ID || response.Revision == "" {
		return errors.New("extension handshake identity, revision or version mismatch")
	}
	for _, c := range response.Capabilities {
		if offered[c] {
			p.capabilities[c] = true
		}
	}
	d := response.Definition
	if d.ID != "" && d.ID != o.ID {
		return errors.New("manifest identity mismatch")
	}
	if d.Revision != "" && d.Revision != response.Revision {
		return errors.New("manifest revision mismatch")
	}
	d.ID = o.ID
	d.Revision = response.Revision
	for i := range d.Sections {
		name := d.Sections[i].Name
		d.Sections[i].Render = func(ctx context.Context, v ReadView) (string, error) {
			var out struct {
				Text *string `json:"text"`
			}
			raw, err := p.call(ctx, "sections/render", handlerRequest{Name: name, Context: v.Identity()}, processGrant{view: v})
			if err != nil {
				return "", err
			}
			if err = json.Unmarshal(raw, &out); err != nil {
				return "", p.rejectResult(err)
			}
			if out.Text == nil {
				return "", p.rejectResult(errors.New("section response missing text"))
			}
			return *out.Text, nil
		}
	}
	for i := range d.Tools {
		name := d.Tools[i].Name
		d.Tools[i].Execute = func(ctx context.Context, c Call, inv Invocation) (Result, error) {
			var out Result
			raw, err := p.call(ctx, "tools/call", handlerRequest{Name: name, Arguments: c.Arguments, Context: inv.Identity()}, processGrant{invocation: inv, view: inv})
			if err != nil {
				return out, err
			}
			if err = json.Unmarshal(raw, &out); err != nil {
				return out, p.rejectResult(err)
			}
			if out.Content == nil {
				return out, p.rejectResult(errors.New("tool response missing content"))
			}
			return out, err
		}
	}
	for i := range d.Hooks {
		name := d.Hooks[i].Name
		d.Hooks[i].Handle = func(ctx context.Context, in HookInput, inv Invocation) (HookDecision, error) {
			var out HookDecision
			raw, err := p.call(ctx, "hooks/call", handlerRequest{Name: name, Input: &in, Context: inv.Identity()}, processGrant{invocation: inv, view: inv})
			if err != nil {
				return out, err
			}
			if err = json.Unmarshal(raw, &out); err != nil {
				return out, p.rejectResult(err)
			}
			if out.Kind == "" {
				return out, p.rejectResult(errors.New("hook response missing decision"))
			}
			return out, err
		}
	}
	for i := range d.Wraps {
		name := d.Wraps[i].Name
		d.Wraps[i].Handle = func(ctx context.Context, c Call, inv Invocation, n Next) (Result, error) {
			var out Result
			raw, err := p.call(ctx, "wraps/call", handlerRequest{Name: name, Call: &c, Context: inv.Identity(), Continuation: inv.Identity().Token}, processGrant{invocation: inv, view: inv, next: n})
			if err != nil {
				return out, err
			}
			if err = json.Unmarshal(raw, &out); err != nil {
				return out, p.rejectResult(err)
			}
			if out.Content == nil {
				return out, p.rejectResult(errors.New("wrapper response missing content"))
			}
			return out, err
		}
	}
	for i := range d.Tasks {
		name := d.Tasks[i].Name
		if len(d.Tasks[i].Migrations) > 0 {
			versions := append([]uint32(nil), d.Tasks[i].Migrations...)
			d.Tasks[i].Migrate = func(from uint32, checkpoint json.RawMessage) (json.RawMessage, error) {
				supported := false
				for _, version := range versions {
					supported = supported || from == version
				}
				if !supported {
					return nil, errors.New("task migration source version unsupported")
				}
				ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
				defer cancel()
				var result struct {
					Checkpoint json.RawMessage `json:"checkpoint"`
				}
				// Pure transforms get no invocation token or callback grant.
				err := p.conn.Call(ctx, methodPrefix+"tasks/step", struct {
					Name    string `json:"name"`
					Migrate struct {
						FromVersion uint32          `json:"fromVersion"`
						Checkpoint  json.RawMessage `json:"checkpoint"`
					} `json:"migrate"`
				}{Name: name, Migrate: struct {
					FromVersion uint32          `json:"fromVersion"`
					Checkpoint  json.RawMessage `json:"checkpoint"`
				}{from, checkpoint}}, &result)
				if err != nil {
					return nil, err
				}
				if !json.Valid(result.Checkpoint) {
					return nil, p.rejectResult(errors.New("migration checkpoint missing"))
				}
				return result.Checkpoint, nil
			}
		}
		step := func(ctx context.Context, task Task, inv Invocation, abort bool) (TaskDecision, error) {
			var decision TaskDecision
			raw, err := p.call(ctx, "tasks/step", handlerRequest{Name: name, Task: &task, Abort: abort, Context: inv.Identity()}, processGrant{invocation: inv, view: inv})
			if err != nil {
				return decision, err
			}
			if err = json.Unmarshal(raw, &decision); err != nil {
				return decision, p.rejectResult(err)
			}
			if decision.Kind == "" {
				return decision, p.rejectResult(errors.New("task response missing decision"))
			}
			return decision, nil
		}
		d.Tasks[i].Run = func(ctx context.Context, task Task, inv Invocation) (TaskDecision, error) {
			return step(ctx, task, inv, false)
		}
		d.Tasks[i].Abort = func(ctx context.Context, task Task, inv Invocation) (TaskDecision, error) {
			return step(ctx, task, inv, true)
		}
	}
	d.Close = p.close
	d.available = func() bool { return !p.closed.Load() }
	readTools := map[string]bool{}
	for _, name := range o.ReadTools {
		readTools[name] = true
	}
	safeHandlers := map[string]bool{}
	for _, name := range o.SafeHandlers {
		safeHandlers[name] = true
	}
	for i := range d.Tools {
		if !readTools[d.Tools[i].Name] && d.Tools[i].Effect == EffectRead {
			d.Tools[i].Effect = EffectExecute
		}
		if !safeHandlers["tool:"+d.Tools[i].Name] {
			d.Tools[i].Replay = ReplayNever
		}
	}
	for i := range d.Hooks {
		if !safeHandlers["hook:"+d.Hooks[i].Name] {
			d.Hooks[i].Replay = ReplayNever
		}
	}
	for i := range d.Wraps {
		if !safeHandlers["wrap:"+d.Wraps[i].Name] {
			d.Wraps[i].Replay = ReplayNever
		}
	}
	if err = validateDefinition(d); err != nil {
		return err
	}
	if err = p.conn.Notify(handshake, "notifications/initialized", initializedNotification{Capabilities: p.capabilities}); err != nil {
		return err
	}
	if err = r.Install(ctx, d); err != nil {
		return err
	}
	ok = true
	return nil
}
func (p *process) rejectResult(err error) error {
	closeErr := p.close(context.Background())
	return errors.Join(fmt.Errorf("extension protocol result: %w", err), closeErr)
}
func (p *process) call(ctx context.Context, method string, params handlerRequest, g processGrant) (json.RawMessage, error) {
	token := g.view.Identity().Token
	if token == "" {
		return nil, errors.New("invocation token absent")
	}
	callCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(context.Canceled)
	budget := &callBudget{deadline: time.Now().Add(p.timeout)}
	budget.timer = time.AfterFunc(p.timeout, func() { cancel(context.DeadlineExceeded) })
	defer budget.finish()
	g.cancel = func() { cancel(context.Canceled) }
	g.budget = budget
	g.ctx = callCtx
	p.mu.Lock()
	if _, exists := p.grants[token]; exists {
		p.mu.Unlock()
		return nil, errors.New("invocation token already dispatched")
	}
	p.grants[token] = g
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.grants, token); p.mu.Unlock() }()
	rpcID := fmt.Sprintf("extension-call-%d", p.nextID.Add(1))
	result, err := p.conn.CallWithID(callCtx, rpcID, methodPrefix+method, params)
	if err != nil {
		if callCtx.Err() != nil {
			notifyCtx, stop := context.WithTimeout(context.Background(), time.Second)
			_ = p.conn.Notify(notifyCtx, methodPrefix+"cancel", map[string]string{"token": token, "rpcId": rpcID, "invocationId": g.view.Identity().InvocationID})
			stop()
			err = context.Cause(callCtx)
		}
		if ctx.Err() == nil && errors.Is(context.Cause(callCtx), context.DeadlineExceeded) || errors.Is(err, rpc.ErrClosed) {
			_ = p.close(context.Background())
		}
	}
	return result, err
}
func (p *process) admit(ctx context.Context, method string, raw json.RawMessage) (any, *rpc.Error) {
	return rpc.Deferred(func() (any, *rpc.Error) {
		if method == methodPrefix+"context/next" {
			value, err := p.callback(ctx, method, raw)
			if err != nil {
				return nil, rpc.NewError(-32000, "capability_denied", err.Error())
			}
			return value, nil
		}
		select {
		case p.workers <- struct{}{}:
		case <-ctx.Done():
			return nil, rpc.NewError(-32000, "cancelled", ctx.Err().Error())
		case <-p.done:
			return nil, rpc.NewError(-32000, "unavailable", "extension process exited")
		}
		defer func() { <-p.workers }()
		value, err := p.callback(ctx, method, raw)
		if err != nil {
			return nil, rpc.NewError(-32000, "capability_denied", err.Error())
		}
		return value, nil
	}), nil
}
func (p *process) callback(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	var in struct {
		Token        string          `json:"token"`
		InvocationID string          `json:"invocationId"`
		Query        durable.Query   `json:"query"`
		Commit       durable.Commit  `json:"commit"`
		Key          string          `json:"key"`
		Candidate    json.RawMessage `json:"candidate"`
		Name         string          `json:"name"`
		Arguments    json.RawMessage `json:"arguments"`
		Chunk        OutputChunk     `json:"chunk"`
		Path         string          `json:"path"`
		Offset       int64           `json:"offset"`
		Limit        int             `json:"limit"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	p.mu.Lock()
	g, ok := p.grants[in.Token]
	p.mu.Unlock()
	if !ok {
		return nil, ErrExpired
	}
	allowed := func(cap string) bool { return p.capabilities[cap] }
	switch method {
	case methodPrefix + "cancel":
		if in.InvocationID != "" && in.InvocationID != g.view.Identity().InvocationID {
			return nil, errors.New("cancellation invocation mismatch")
		}
		if g.cancel != nil {
			g.cancel()
		}
		return nil, nil
	case methodPrefix + "context/read":
		if !allowed("read") || g.view == nil {
			break
		}
		return g.view.Read(ctx, in.Query)
	case methodPrefix + "context/commit":
		if !allowed("commit") || g.invocation == nil {
			break
		}
		return g.invocation.Commit(ctx, in.Commit)
	case methodPrefix + "context/memo":
		if !allowed("memo") || g.invocation == nil {
			break
		}
		if len(in.Candidate) == 0 {
			v, present, err := g.invocation.ReadMemo(ctx, in.Key)
			return memoResponse{Value: v, Present: present}, err
		}
		memo, ok := g.invocation.(interface {
			MemoInserted(context.Context, string, json.RawMessage) (json.RawMessage, bool, error)
		})
		if !ok {
			return nil, durable.ErrUnsupported
		}
		v, inserted, err := memo.MemoInserted(ctx, in.Key, in.Candidate)
		return memoResponse{Value: v, Present: err == nil, Inserted: inserted}, err
	case methodPrefix + "context/call_tool":
		if !allowed("call_tool") || g.invocation == nil {
			break
		}
		return g.invocation.CallTool(ctx, in.Key, in.Name, in.Arguments)
	case methodPrefix + "context/next":
		if !allowed("next") || g.next == nil {
			break
		}
		if len(in.Arguments) > 0 || in.Name != "" {
			return nil, errors.New("Next cannot change gated arguments")
		}
		if g.budget != nil {
			resume := g.budget.pause()
			defer resume()
		}
		nextCtx, cancel := context.WithCancel(g.ctx)
		stop := context.AfterFunc(ctx, cancel)
		defer func() { stop(); cancel() }()
		return g.next.Call(nextCtx)
	case methodPrefix + "env/read":
		if !allowed("environment_read") || g.view == nil {
			break
		}
		environment, err := g.view.Environment(ctx)
		if err != nil {
			return nil, err
		}
		text, err := environment.ReadFile(ctx, in.Path, in.Offset, in.Limit)
		return struct {
			Text string `json:"text"`
		}{Text: text}, err
	case methodPrefix + "output":
		if !allowed("output") || g.invocation == nil {
			break
		}
		return nil, g.invocation.Output(ctx, in.Chunk)
	}
	return nil, errors.New("callback capability unavailable")
}
func (p *process) close(ctx context.Context) error {
	p.closed.Store(true)
	p.once.Do(func() {
		closeCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		var out json.RawMessage
		_ = p.conn.Call(closeCtx, methodPrefix+"shutdown", nil, &out)
		_ = p.conn.Close()
		select {
		case <-p.done:
			return
		default:
		}
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-p.done:
		case <-closeCtx.Done():
			_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
			<-p.done
		}
	})
	return nil
}

func processEnvironment() []string {
	allowed := map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true, "TMP": true, "TEMP": true, "LANG": true, "LC_ALL": true, "LC_CTYPE": true, "SYSTEMROOT": true, "WINDIR": true, "USERPROFILE": true}
	var environment []string
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if ok && (allowed[key] || strings.HasPrefix(key, "LC_")) {
			environment = append(environment, entry)
		}
	}
	return environment
}
