package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"go.harness.dev/harness/internal/obs"
	"go.harness.dev/harness/internal/rpc"
)

const (
	// maxOutbound is the escaped wire size cap for any frame Tempest sends.
	maxOutbound = 2 << 20
	// envelopeOverhead bounds the JSON-RPC envelope around params, method and ID.
	envelopeOverhead = 64
	stderrTailBytes  = 64 << 10
	stderrReadBuffer = 32 << 10
	stderrDetail     = 2048
	serverMessageMax = 512
	maxConfigItems   = 256
	// stderrJoinGrace is how long teardown lets stderr drain before closing
	// the read end that a descendant may still hold open.
	stderrJoinGrace = 200 * time.Millisecond
	exitGrace       = time.Second
)

// Standard JSON-RPC and LSP error codes the client distinguishes.
const (
	rpcInvalidParams    = -32602
	rpcMethodNotFound   = -32601
	lspRequestCancelled = -32800
	lspContentModified  = -32801
)

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
		return count, nil
	}
	if excess := len(b.bytes) + len(p) - stderrTailBytes; excess > 0 {
		b.bytes = append(b.bytes[:0], b.bytes[excess:]...)
	}
	b.bytes = append(b.bytes, p...)
	return count, nil
}

// detail is the redacted, bounded end of the retained stderr tail.
func (b *tailBuffer) detail() string {
	b.mu.Lock()
	tail := strings.ToValidUTF8(string(b.bytes), "\uFFFD")
	b.mu.Unlock()
	tail = obs.Redact(strings.Join(strings.Fields(tail), " "))
	if len(tail) > stderrDetail {
		cut := len(tail) - stderrDetail
		for cut < len(tail) && !utf8.RuneStart(tail[cut]) {
			cut++
		}
		tail = "…" + tail[cut:]
	}
	return tail
}

// pipeStream joins the child's stdout read end and stdin write end.
type pipeStream struct {
	stdout *os.File
	stdin  *os.File
	once   sync.Once
}

func (s *pipeStream) Read(p []byte) (int, error)  { return s.stdout.Read(p) }
func (s *pipeStream) Write(p []byte) (int, error) { return s.stdin.Write(p) }
func (s *pipeStream) Close() error {
	var err error
	s.once.Do(func() { err = errors.Join(s.stdin.Close(), s.stdout.Close()) })
	return err
}

// serverCaps is what the client uses from the initialize result.
type serverCaps struct {
	syncKind        int // 1 full, 2 incremental
	save            bool
	saveIncludeText bool
	definition      bool
	references      bool
	rename          bool
	diagnostic      bool
	diagIdentifier  string
}

// server is one language server process generation.
type server struct {
	m          *Manager
	route      route
	cfg        ServerConfig
	generation uint64

	cmd    *exec.Cmd
	stream *pipeStream
	stderr *os.File
	conn   *rpc.Conn
	caps   serverCaps

	exited     chan struct{}
	served     chan struct{}
	stderrDone chan struct{}
	gone       chan struct{} // closed once the server is retiring or lost
	stopped    chan struct{}
	tail       tailBuffer

	nextID   atomic.Uint64
	goneOnce sync.Once
	stopOnce sync.Once

	// mu guards everything below. It is held only for short in-memory work,
	// never across I/O; Manager.mu may be held while taking it, never the reverse.
	mu        sync.Mutex
	fault     *Error
	docs      map[string]*openDoc
	pinned    string
	openBytes int
	useSeq    uint64
	diags     diagnosticsState
}

// launch starts and initializes one server generation. On failure the
// process, if any, is fully torn down before returning.
func (m *Manager) launch(ctx context.Context, r route, cfg ServerConfig, generation uint64) (*server, error) {
	executable, err := exec.LookPath(cfg.Command)
	if err != nil {
		return nil, newError(CodeServerMissing, fmt.Sprintf("language server %q for the %s route is not installed or not executable; install it, point the LSP config's %s command at it, or disable the %s server", filepath.Base(cfg.Command), r.name, r.name, r.name))
	}
	stdinRead, stdinWrite, err := os.Pipe()
	if err != nil {
		return nil, wrapError(CodeServerUnavailable, "create pipes", err)
	}
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		closeFiles(stdinRead, stdinWrite)
		return nil, wrapError(CodeServerUnavailable, "create pipes", err)
	}
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		closeFiles(stdinRead, stdinWrite, stdoutRead, stdoutWrite)
		return nil, wrapError(CodeServerUnavailable, "create pipes", err)
	}
	cmd := &exec.Cmd{
		Path:        executable,
		Args:        append([]string{cfg.Command}, cfg.Args...),
		Dir:         m.root,
		Env:         serverEnv(cfg.Env),
		Stdin:       stdinRead,
		Stdout:      stdoutWrite,
		Stderr:      stderrWrite,
		SysProcAttr: &syscall.SysProcAttr{Setpgid: true},
	}
	if err = ctx.Err(); err == nil {
		err = cmd.Start()
	}
	// The child owns its ends now; keeping ours would hide EOF.
	closeFiles(stdinRead, stdoutWrite, stderrWrite)
	if err != nil {
		closeFiles(stdinWrite, stdoutRead, stderrRead)
		if ctx.Err() != nil {
			return nil, asError(ctx.Err())
		}
		return nil, wrapError(CodeServerUnavailable, fmt.Sprintf("start %s language server failed", r.name), err)
	}
	s := &server{
		m: m, route: r, cfg: cfg, generation: generation,
		cmd: cmd, stream: &pipeStream{stdout: stdoutRead, stdin: stdinWrite}, stderr: stderrRead,
		exited: make(chan struct{}), served: make(chan struct{}), stderrDone: make(chan struct{}),
		gone: make(chan struct{}), stopped: make(chan struct{}),
		docs: make(map[string]*openDoc),
	}
	s.diags.init()
	s.conn = rpc.NewContentLengthStream(s.stream, rpc.MaxContentLength)
	s.conn.SetProtocolErrorHandler(func(err error) { s.setFault(protocolFault(err)) })
	go func() {
		defer close(s.stderrDone)
		buffer := make([]byte, stderrReadBuffer)
		for {
			n, readErr := s.stderr.Read(buffer)
			if n > 0 {
				_, _ = s.tail.Write(buffer[:n])
			}
			if readErr != nil {
				return
			}
		}
	}()
	go func() {
		defer close(s.served)
		_ = s.conn.ServeOrdered(s.handle)
	}()
	// The only Wait owner. Pipes are *os.File, so Wait doesn't join copiers.
	go func() {
		_ = cmd.Wait()
		close(s.exited)
	}()
	if err = s.initialize(ctx); err != nil {
		s.markGone()
		s.stop(false)
		if ctx.Err() != nil {
			return nil, asError(ctx.Err())
		}
		return nil, err
	}
	return s, nil
}

func closeFiles(files ...*os.File) {
	for _, file := range files {
		_ = file.Close()
	}
}

// protocolFault turns an rpc strict-mode fault ("lsp_protocol: ...") into an Error.
func protocolFault(err error) *Error {
	code, detail, ok := strings.Cut(err.Error(), ": ")
	if !ok || (code != CodeProtocol && code != CodeOutputLimit) {
		return newError(CodeProtocol, boundText(err.Error(), serverMessageMax))
	}
	return newError(code, detail)
}

// setFault records the first protocol failure; the connection is closing.
func (s *server) setFault(err *Error) {
	s.mu.Lock()
	if s.fault == nil {
		s.fault = err
	}
	s.mu.Unlock()
}

// fail retires the server after a protocol or synchronization failure. The
// lifecycle watcher observes the closed connection and tears it down.
func (s *server) fail(err *Error) {
	s.setFault(err)
	_ = s.conn.Close()
}

func (s *server) markGone() {
	s.goneOnce.Do(func() { close(s.gone) })
}

func (s *server) alive() bool {
	select {
	case <-s.gone:
		return false
	default:
		return s.connected()
	}
}

// connected reports whether the process and connection are both still up.
func (s *server) connected() bool {
	select {
	case <-s.conn.Done():
		return false
	case <-s.exited:
		return false
	default:
		return true
	}
}

// lostError explains why a request couldn't complete on this server.
func (s *server) lostError() *Error {
	s.mu.Lock()
	fault := s.fault
	s.mu.Unlock()
	if fault != nil {
		return fault
	}
	detail := fmt.Sprintf("%s language server exited or closed its connection", s.route.name)
	if tail := s.tail.detail(); tail != "" {
		detail += "; stderr tail: " + tail
	}
	return &Error{Code: CodeServerUnavailable, Detail: detail, RetryAfter: s.m.timing.cooldown}
}

// stop tears the process down and joins its reader, Wait owner and stderr
// pipe. graceful sends shutdown, then exit only after shutdown succeeded.
func (s *server) stop(graceful bool) {
	s.stopOnce.Do(func() {
		defer close(s.stopped)
		s.markGone()
		waitExit := func(limit time.Duration) bool {
			timer := time.NewTimer(limit)
			defer timer.Stop()
			select {
			case <-s.exited:
				return true
			case <-timer.C:
				return false
			}
		}
		if graceful && s.connected() {
			ctx, cancel := context.WithTimeout(context.Background(), s.m.timing.shutdown)
			_, err := s.conn.CallWithID(ctx, s.newID(), "shutdown", nil)
			if err == nil {
				_ = s.conn.Notify(ctx, "exit", nil)
			}
			cancel()
			if err == nil {
				waitExit(exitGrace)
			}
		}
		// EOF on stdin, then TERM and KILL with one-second grace each.
		_ = s.stream.stdin.Close()
		if !waitExit(exitGrace) {
			_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGTERM)
			if !waitExit(exitGrace) {
				_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
			}
		}
		// Group members may outlive the leader and keep inherited pipes open.
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		_ = s.conn.Close()
		<-s.exited
		<-s.served
		timer := time.NewTimer(stderrJoinGrace)
		select {
		case <-s.stderrDone:
		case <-timer.C:
		}
		timer.Stop()
		// A setsid descendant can hold stderr open indefinitely; closing our
		// read end unblocks the reader.
		_ = s.stderr.Close()
		<-s.stderrDone
		s.mu.Lock()
		s.docs = map[string]*openDoc{}
		s.openBytes = 0
		s.diags.init()
		s.mu.Unlock()
	})
	<-s.stopped
}

func (s *server) newID() string {
	return fmt.Sprintf("tempest-%d", s.nextID.Add(1))
}

// encodeParams preflights an outbound frame against the 2 MiB escaped cap.
func encodeParams(method string, params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	body, err := json.Marshal(params)
	if err != nil {
		return nil, wrapError(CodeProtocol, "encode "+method, err)
	}
	if len(body)+len(method)+envelopeOverhead > maxOutbound {
		return nil, newError(CodeOutputLimit, fmt.Sprintf("%s message is %d bytes after JSON escaping; the outbound limit is 2 MiB", method, len(body)))
	}
	return body, nil
}

// call sends one request. Cancellation sends $/cancelRequest with the exact
// outbound ID; a late response is discarded by RPC correlation. Requests are
// never resent.
func (s *server) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	body, err := encodeParams(method, params)
	if err != nil {
		return nil, err
	}
	var payload any
	if body != nil {
		payload = body
	}
	id := s.newID()
	raw, err := s.conn.CallWithID(ctx, id, method, payload)
	if err == nil {
		return raw, nil
	}
	if ctx.Err() != nil {
		s.cancelRequest(id)
		return nil, asError(ctx.Err())
	}
	var remote *rpc.Error
	if errors.As(err, &remote) {
		return nil, serverError(method, remote)
	}
	return nil, s.lostError()
}

func (s *server) cancelRequest(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), s.m.timing.cancelBudget)
	defer cancel()
	_ = s.conn.Notify(ctx, "$/cancelRequest", map[string]string{"id": id})
}

func serverError(method string, remote *rpc.Error) *Error {
	switch remote.Code {
	case lspContentModified:
		return newError(CodeContentModified, "the language server reported the content changed during "+method)
	case lspRequestCancelled:
		return newError(CodeCancelled, "the language server cancelled "+method)
	case rpcMethodNotFound:
		return newError(CodeUnsupported, "the language server doesn't implement "+method)
	}
	return newError(CodeServerError, fmt.Sprintf("%s failed with server error %d: %s", method, remote.Code, obs.RedactTruncate(remote.Message, serverMessageMax)))
}

// notify sends a notification. A send that can't be queued within the send
// budget means the writer is blocked: the server is retired.
func (s *server) notify(ctx context.Context, method string, params any) error {
	body, err := encodeParams(method, params)
	if err != nil {
		return err
	}
	var payload any
	if body != nil {
		payload = body
	}
	sendCtx, cancel := context.WithTimeout(ctx, s.m.timing.sendBudget)
	defer cancel()
	if err = s.conn.Notify(sendCtx, method, payload); err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return asError(ctx.Err())
	}
	if sendCtx.Err() != nil {
		s.fail(newError(CodeServerUnavailable, fmt.Sprintf("%s language server stopped reading its input", s.route.name)))
	}
	return s.lostError()
}

type workspaceFolder struct {
	URI  string `json:"uri"`
	Name string `json:"name"`
}

// clientCapabilities advertises exactly what this client implements.
func clientCapabilities() map[string]any {
	return map[string]any{
		"general": map[string]any{"positionEncodings": []string{"utf-16"}},
		"workspace": map[string]any{
			"configuration":    true,
			"workspaceFolders": true,
			"applyEdit":        false,
			"workspaceEdit":    map[string]any{"documentChanges": true},
		},
		"textDocument": map[string]any{
			"synchronization":    map[string]any{"dynamicRegistration": false, "didSave": true},
			"definition":         map[string]any{"dynamicRegistration": false, "linkSupport": true},
			"references":         map[string]any{"dynamicRegistration": false},
			"rename":             map[string]any{"dynamicRegistration": false, "prepareSupport": false},
			"publishDiagnostics": map[string]any{"versionSupport": true},
			"diagnostic":         map[string]any{"dynamicRegistration": false, "relatedDocumentSupport": false},
		},
	}
}

func (s *server) initialize(ctx context.Context) error {
	params := map[string]any{
		"processId":        os.Getpid(),
		"clientInfo":       map[string]string{"name": "tempest"},
		"rootUri":          s.m.rootURI,
		"workspaceFolders": []workspaceFolder{s.m.folder()},
		"capabilities":     clientCapabilities(),
	}
	if len(s.cfg.InitializationOptions) > 0 {
		params["initializationOptions"] = s.cfg.InitializationOptions
	}
	raw, err := s.call(ctx, "initialize", params)
	if err != nil {
		return err
	}
	if s.caps, err = parseCapabilities(raw); err != nil {
		return err
	}
	if err = s.notify(ctx, "initialized", struct{}{}); err != nil {
		return err
	}
	settings := s.cfg.Settings
	if len(settings) == 0 {
		settings = json.RawMessage("null")
	}
	return s.notify(ctx, "workspace/didChangeConfiguration", map[string]json.RawMessage{"settings": settings})
}

// parseCapabilities reads the server capabilities the client relies on.
func parseCapabilities(raw json.RawMessage) (serverCaps, error) {
	var result struct {
		Capabilities *struct {
			PositionEncoding   *string         `json:"positionEncoding"`
			TextDocumentSync   json.RawMessage `json:"textDocumentSync"`
			DefinitionProvider json.RawMessage `json:"definitionProvider"`
			ReferencesProvider json.RawMessage `json:"referencesProvider"`
			RenameProvider     json.RawMessage `json:"renameProvider"`
			DiagnosticProvider json.RawMessage `json:"diagnosticProvider"`
		} `json:"capabilities"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Capabilities == nil {
		return serverCaps{}, newError(CodeProtocol, "initialize result has no capabilities object")
	}
	c := result.Capabilities
	if c.PositionEncoding != nil && *c.PositionEncoding != "utf-16" {
		return serverCaps{}, newError(CodeUnsupported, fmt.Sprintf("server selected position encoding %q; Tempest supports only utf-16", boundText(*c.PositionEncoding, maxEchoedName)))
	}
	caps := serverCaps{
		definition: providerEnabled(c.DefinitionProvider),
		references: providerEnabled(c.ReferencesProvider),
		rename:     providerEnabled(c.RenameProvider),
	}
	if providerEnabled(c.DiagnosticProvider) {
		var options struct {
			Identifier string `json:"identifier"`
		}
		if json.Unmarshal(c.DiagnosticProvider, &options) != nil {
			return serverCaps{}, newError(CodeProtocol, "invalid diagnosticProvider")
		}
		caps.diagnostic = true
		caps.diagIdentifier = options.Identifier
	}
	openClose := false
	var kind int
	if json.Unmarshal(c.TextDocumentSync, &kind) == nil {
		openClose = true
		caps.syncKind = kind
	} else if len(c.TextDocumentSync) > 0 && c.TextDocumentSync[0] == '{' {
		var options struct {
			OpenClose bool            `json:"openClose"`
			Change    int             `json:"change"`
			Save      json.RawMessage `json:"save"`
		}
		if json.Unmarshal(c.TextDocumentSync, &options) != nil {
			return serverCaps{}, newError(CodeProtocol, "invalid textDocumentSync")
		}
		openClose = options.OpenClose
		caps.syncKind = options.Change
		var save bool
		var saveOptions struct {
			IncludeText bool `json:"includeText"`
		}
		switch {
		case json.Unmarshal(options.Save, &save) == nil:
			caps.save = save
		case len(options.Save) > 0 && options.Save[0] == '{' && json.Unmarshal(options.Save, &saveOptions) == nil:
			caps.save = true
			caps.saveIncludeText = saveOptions.IncludeText
		}
	}
	if !openClose || (caps.syncKind != 1 && caps.syncKind != 2) {
		return serverCaps{}, newError(CodeUnsupported, "server doesn't support open/close with full or incremental document sync, so query freshness can't be guaranteed")
	}
	return caps, nil
}

// providerEnabled accepts true or an options object.
func providerEnabled(raw json.RawMessage) bool {
	var enabled bool
	if json.Unmarshal(raw, &enabled) == nil {
		return enabled
	}
	return len(raw) > 0 && raw[0] == '{'
}

// handle runs on the RPC reader. It returns promptly and never performs I/O
// besides queueing replies, so responses keep flowing while queries wait.
func (s *server) handle(ctx context.Context, method string, params json.RawMessage) (any, *rpc.Error) {
	if len(rpc.RequestID(ctx)) == 0 {
		switch method {
		case "textDocument/publishDiagnostics":
			if err := s.onPublish(params); err != nil {
				s.fail(err)
			}
		case "window/logMessage", "window/showMessage":
			var message struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(params, &message) == nil {
				s.m.logger.Debug("language server message", slog.String("server", s.route.name), slog.String("message", obs.RedactTruncate(message.Message, serverMessageMax)))
			}
		}
		return nil, nil
	}
	switch method {
	case "workspace/configuration":
		return s.configuration(params)
	case "workspace/workspaceFolders":
		return []workspaceFolder{s.m.folder()}, nil
	case "workspace/applyEdit":
		return map[string]any{"applied": false, "failureReason": "Tempest requires explicit edit tools"}, nil
	case "workspace/diagnostic/refresh":
		s.onRefresh()
		return nil, nil
	}
	return nil, rpc.NewError(rpcMethodNotFound, "method_not_found", "Tempest doesn't support "+boundText(method, maxEchoedName))
}

func (s *server) configuration(params json.RawMessage) (any, *rpc.Error) {
	var request struct {
		Items []struct {
			Section string `json:"section"`
		} `json:"items"`
	}
	if json.Unmarshal(params, &request) != nil || len(request.Items) > maxConfigItems {
		return nil, rpc.NewError(rpcInvalidParams, "invalid_params", "invalid configuration request")
	}
	results := make([]json.RawMessage, len(request.Items))
	for i, item := range request.Items {
		results[i] = settingsSection(s.cfg.Settings, item.Section)
	}
	return results, nil
}
