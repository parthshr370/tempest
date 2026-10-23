package lsp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	maxDirtyPaths            = 256
	maxQueryPath             = 4096
	maxNewNameRunes          = 256
	maxDiagnosticsWait       = 10 * time.Second
	maxQueryResults          = 200
	defaultLocationResults   = 50
	defaultDiagnosticResults = 100
)

// timing holds the lifecycle policy. Tests shorten it; production uses defaultTiming.
type timing struct {
	now             func() time.Time
	idle            time.Duration
	cooldown        time.Duration
	circuitWindow   time.Duration
	circuitOpen     time.Duration
	circuitFailures int
	startup         time.Duration
	request         time.Duration
	shutdown        time.Duration
	cancelBudget    time.Duration
	sendBudget      time.Duration
}

func defaultTiming() timing {
	return timing{
		now:             time.Now,
		idle:            5 * time.Minute,
		cooldown:        2 * time.Second,
		circuitWindow:   time.Minute,
		circuitOpen:     time.Minute,
		circuitFailures: 3,
		startup:         30 * time.Second,
		request:         15 * time.Second,
		shutdown:        time.Second,
		cancelBudget:    100 * time.Millisecond,
		sendBudget:      5 * time.Second,
	}
}

type slotState int

const (
	stateStopped slotState = iota
	stateStarting
	stateReady
	stateRetiring
)

// startAttempt is the one startup promise per route; joiners wait on done.
type startAttempt struct {
	done chan struct{}
	err  *Error
}

// slot is one route's lifecycle. All mutable fields are guarded by Manager.mu.
type slot struct {
	route route
	cfg   ServerConfig
	// lane serializes document reconciliation and the query on one server.
	lane chan struct{}

	state        slotState
	srv          *server
	attempt      *startAttempt
	retired      chan struct{}
	generation   uint64
	active       int // queries, startups and sync tasks using the slot
	lastActive   time.Time
	failures     []time.Time
	nextStart    time.Time
	circuitUntil time.Time
	dirty        map[string]uint64
	dirtyAll     bool
	dirtyAllSeq  uint64
	syncing      bool
}

func (sl *slot) resetDirty() {
	sl.dirty = map[string]uint64{}
	sl.dirtyAll = false
}

// watch records paths changed while one query runs.
type watch struct {
	all   bool
	paths map[string]bool
}

func (w *watch) record(p string) {
	if w.all {
		return
	}
	if len(w.paths) >= maxDirtyPaths {
		w.all, w.paths = true, nil
		return
	}
	w.paths[p] = true
}

// Manager owns the language servers for one session workspace.
type Manager struct {
	ws      Workspace
	cfg     Config
	root    string
	rootURI string
	logger  *slog.Logger
	timing  timing
	slots   map[string]*slot // fixed after New

	ctx        context.Context
	cancel     context.CancelFunc
	signal     chan struct{}
	workerDone chan struct{}
	closeOnce  sync.Once
	closeDone  chan struct{}
	closeErr   error
	queries    sync.WaitGroup
	tasks      sync.WaitGroup

	mu        sync.Mutex
	closed    bool
	changeSeq uint64
	watches   map[*watch]struct{}
}

// New validates cfg and returns an inert manager. It starts no process and
// reads no files; servers start lazily on their first query.
func New(workspace Workspace, cfg Config, logger *slog.Logger) (*Manager, error) {
	return newManager(workspace, cfg, logger, defaultTiming())
}

func newManager(workspace Workspace, cfg Config, logger *slog.Logger, t timing) (*Manager, error) {
	if workspace == nil {
		return nil, newError(CodeConfig, "a workspace is required")
	}
	normalized, err := Normalize(cfg)
	if err != nil {
		return nil, err
	}
	root := workspace.Root()
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, newError(CodeConfig, "workspace root must be a canonical absolute path")
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		ws: workspace, cfg: normalized, root: filepath.ToSlash(root), rootURI: fileURI(filepath.ToSlash(root)),
		logger: logger, timing: t, slots: map[string]*slot{},
		ctx: ctx, cancel: cancel, signal: make(chan struct{}, 1), workerDone: make(chan struct{}), closeDone: make(chan struct{}),
		watches: map[*watch]struct{}{},
	}
	for _, server := range normalized.Servers {
		r, _ := findRoute(server.Name)
		m.slots[server.Name] = &slot{route: r, cfg: server, lane: make(chan struct{}, 1), dirty: map[string]uint64{}}
	}
	go m.worker()
	return m, nil
}

func (m *Manager) folder() workspaceFolder {
	return workspaceFolder{URI: m.rootURI, Name: path.Base(m.root)}
}

func closedError() *Error { return newError(CodeClosed, "the language server manager is closed") }

// Query runs one definition, references, rename or diagnostics request. It
// never writes workspace files and never resends a request.
func (m *Manager) Query(ctx context.Context, q Query) (Result, error) {
	if err := normalizeQuery(&q); err != nil {
		return Result{}, err
	}
	if !m.cfg.Enabled {
		return Result{}, newError(CodeDisabled, "language servers are disabled")
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Result{}, closedError()
	}
	m.queries.Add(1)
	m.mu.Unlock()
	defer m.queries.Done()
	ctx, cancel := context.WithTimeout(ctx, m.timing.request)
	defer cancel()
	stop := context.AfterFunc(m.ctx, cancel)
	defer stop()
	value, err := m.query(ctx, q)
	if err != nil {
		if m.ctx.Err() != nil {
			return Result{}, closedError()
		}
		return Result{}, asError(err)
	}
	return Result{Value: value}, nil
}

func normalizeQuery(q *Query) error {
	invalid := func(detail string) error { return newError(CodeInvalidArgument, detail) }
	switch q.Method {
	case MethodDefinition, MethodReferences, MethodRename, MethodDiagnostics:
	default:
		return invalid(fmt.Sprintf("unknown method %q", boundText(q.Method, maxEchoedName)))
	}
	if q.Path == "" || len(q.Path) > maxQueryPath || strings.ContainsRune(q.Path, 0) {
		return invalid("path must be a nonempty workspace filename without NUL")
	}
	if q.Server != "" {
		if _, ok := findRoute(q.Server); !ok {
			return invalid("server must be one of go, yaml, terraform, helm")
		}
	}
	switch {
	case q.MaxResults == 0 && q.Method == MethodDiagnostics:
		q.MaxResults = defaultDiagnosticResults
	case q.MaxResults == 0:
		q.MaxResults = defaultLocationResults
	case q.MaxResults < 1 || q.MaxResults > maxQueryResults:
		return invalid("max_results must be between 1 and 200")
	}
	if q.Method != MethodDiagnostics && (q.Position.Line < 1 || q.Position.Column < 1) {
		return newError(CodeInvalidPosition, "line and column are one-based")
	}
	if q.Method == MethodRename {
		if q.NewName == "" || !utf8.ValidString(q.NewName) || utf8.RuneCountInString(q.NewName) > maxNewNameRunes || strings.ContainsRune(q.NewName, 0) {
			return invalid("new_name must be 1 to 256 characters of valid text")
		}
	}
	if q.Method == MethodDiagnostics {
		if q.Severity == "" {
			q.Severity = "all"
		}
		switch q.Severity {
		case "all", "error", "warning", "information", "hint":
		default:
			return invalid("severity must be all, error, warning, information or hint")
		}
		if q.Wait < 0 || q.Wait > maxDiagnosticsWait {
			return invalid("wait_ms must be between 0 and 10000")
		}
	}
	return nil
}

func (m *Manager) query(ctx context.Context, q Query) ([]byte, error) {
	w := m.watchChanges()
	defer m.unwatch(w)
	// This first read canonicalizes the path, routes it and rejects invalid
	// input before any server starts. run reads again inside the lane, so a
	// change made while queued can't push stale content to the server.
	doc, err := m.readDocument(ctx, q.Path)
	if err != nil {
		return nil, err
	}
	sl, languageID, err := m.routeFor(ctx, doc.Path, q.Server)
	if err != nil {
		return nil, err
	}
	if q.Method != MethodDiagnostics {
		if _, err = newLineIndex(doc.Text).modelOffset(q.Position); err != nil {
			return nil, err
		}
	}
	for attempt := 0; ; attempt++ {
		srv, err := m.acquire(ctx, sl)
		if err != nil {
			return nil, err
		}
		if err = m.enterLane(ctx, sl); err != nil {
			m.release(sl)
			return nil, err
		}
		value, err := m.run(ctx, srv, q, doc.Path, languageID, w)
		m.leaveLane(sl)
		m.release(sl)
		if !errors.Is(err, errReopen) {
			return value, err
		}
		if attempt > 0 {
			return nil, newError(CodeServerUnavailable, "document versions were exhausted twice")
		}
		// Retire and reopen in a fresh generation rather than wrap int32 versions.
		m.retireNow(sl, srv)
	}
}

// readDocument reads and validates one contained document.
func (m *Manager) readDocument(ctx context.Context, p string) (Document, error) {
	doc, err := m.ws.ReadFile(ctx, p)
	if err != nil {
		if ctx.Err() != nil {
			return Document{}, asError(ctx.Err())
		}
		return Document{}, asError(err)
	}
	if err = validateDocument(doc); err != nil {
		return Document{}, err
	}
	return doc, nil
}

// run synchronizes the requested document and every open document, then
// performs the method in the same operation lane.
func (m *Manager) run(ctx context.Context, srv *server, q Query, rel, languageID string, w *watch) ([]byte, error) {
	doc, err := m.readDocument(ctx, rel)
	if err != nil {
		return nil, err
	}
	if doc.Path != rel {
		return nil, newError(CodeContentModified, rel+" was replaced while the query was queued; retry if still useful")
	}
	srv.pin(rel)
	defer srv.pin("")
	record, err := srv.syncDocument(ctx, doc, languageID)
	if err != nil {
		return nil, err
	}
	srv.touch(record)
	if err = srv.reconcileOpen(ctx, rel); err != nil {
		return nil, err
	}
	var position lspPosition
	if q.Method != MethodDiagnostics {
		offset, err := record.index.modelOffset(q.Position)
		if err != nil {
			return nil, err
		}
		position = record.index.utf16(offset)
	}
	target := map[string]any{"textDocument": textDocumentID{URI: record.uri}, "position": position}
	switch q.Method {
	case MethodDefinition, MethodReferences:
		method := "textDocument/definition"
		supported := srv.caps.definition
		if q.Method == MethodReferences {
			method, supported = "textDocument/references", srv.caps.references
			target["context"] = map[string]bool{"includeDeclaration": q.IncludeDeclaration}
		}
		if !supported {
			return nil, newError(CodeUnsupported, fmt.Sprintf("the %s language server doesn't provide %s", srv.route.name, q.Method))
		}
		raw, err := srv.call(ctx, method, target)
		if err != nil {
			return nil, err
		}
		if err = m.verifyCurrent(ctx, w, record); err != nil {
			return nil, err
		}
		return srv.locationsValue(ctx, record, raw, q.MaxResults)
	case MethodRename:
		if !srv.caps.rename {
			return nil, newError(CodeUnsupported, fmt.Sprintf("the %s language server doesn't provide rename", srv.route.name))
		}
		snapshot, err := m.snapshotHashes(ctx)
		if err != nil {
			return nil, err
		}
		target["newName"] = q.NewName
		raw, err := srv.call(ctx, "textDocument/rename", target)
		if err != nil {
			return nil, err
		}
		value, err := srv.renameValue(ctx, raw, snapshot, w)
		if err != nil {
			return nil, err
		}
		if err = m.verifyCurrent(ctx, w, record); err != nil {
			return nil, err
		}
		return value, nil
	}
	entry, freshness, err := srv.diagnostics(ctx, q, record)
	if err != nil {
		return nil, err
	}
	if (freshness == freshCurrent || freshness == freshUnversioned) && m.verifyCurrent(ctx, w, record) != nil {
		if ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, asError(ctx.Err())
		}
		freshness = freshOutdated
	}
	return diagnosticsValue(srv.route.name, record, entry, freshness, q.Severity, q.MaxResults)
}

// verifyCurrent fails when the captured document changed while the request
// ran, whether through Changed or an unannounced disk write.
func (m *Manager) verifyCurrent(ctx context.Context, w *watch, record *openDoc) error {
	modified := newError(CodeContentModified, record.path+" changed while the language server was answering; retry if still useful")
	if m.changedSince(w, record.path) {
		return modified
	}
	doc, err := m.ws.ReadFile(ctx, record.path)
	if ctx.Err() != nil {
		return asError(ctx.Err())
	}
	if err != nil || doc.Anchor != record.anchor {
		return modified
	}
	return nil
}

// routeFor selects the server route for a canonical path.
func (m *Manager) routeFor(ctx context.Context, p, explicit string) (*slot, string, error) {
	name := explicit
	ext := strings.ToLower(path.Ext(p))
	if name != "" {
		if r, _ := findRoute(name); !r.handles(p) {
			return nil, "", newError(CodeNoServer, fmt.Sprintf("the %s server doesn't handle %s files", name, ext))
		}
	} else {
		switch ext {
		case ".go":
			name = "go"
		case ".tf", ".tfvars":
			name = "terraform"
		case ".yaml", ".yml", ".tpl":
			template, err := m.chartTemplate(ctx, p)
			if err != nil {
				return nil, "", err
			}
			switch {
			case template:
				name = "helm"
			case ext == ".tpl":
				return nil, "", newError(CodeNoServer, p+" is not beneath a Helm chart's templates directory")
			default:
				name = "yaml"
			}
		default:
			return nil, "", newError(CodeNoServer, fmt.Sprintf("no language server route handles %q files", boundText(ext, maxEchoedName)))
		}
	}
	sl := m.slots[name]
	if sl == nil || sl.cfg.Disabled {
		detail := fmt.Sprintf("the %s language server is not configured or is disabled", name)
		if name == "helm" && explicit == "" {
			detail += "; chart templates aren't interpreted as plain YAML (pass server \"yaml\" to choose the YAML server explicitly)"
		}
		return nil, "", newError(CodeNoServer, detail)
	}
	return sl, sl.route.languageID(p), nil
}

// chartTemplate reports whether p is beneath templates/ of its nearest
// Chart.yaml ancestor. The search never leaves the workspace root.
func (m *Manager) chartTemplate(ctx context.Context, p string) (bool, error) {
	dir := path.Dir(p)
	if !strings.Contains("/"+dir+"/", "/templates/") {
		return false, nil
	}
	for {
		candidate := "Chart.yaml"
		if dir != "." {
			candidate = dir + "/Chart.yaml"
		}
		_, err := m.ws.ReadFile(ctx, candidate)
		if ctx.Err() != nil {
			return false, asError(ctx.Err())
		}
		if err == nil {
			rel := p
			if dir != "." {
				rel = strings.TrimPrefix(p, dir+"/")
			}
			return strings.HasPrefix(rel, "templates/"), nil
		}
		if dir == "." {
			return false, nil
		}
		dir = path.Dir(dir)
	}
}

// acquire marks the slot active and returns its ready server, starting it
// once if needed. Joiners wait with their own cancellation.
func (m *Manager) acquire(ctx context.Context, sl *slot) (*server, error) {
	m.mu.Lock()
	sl.active++
	m.mu.Unlock()
	srv, err := m.awaitServer(ctx, sl)
	if err != nil {
		m.release(sl)
	}
	return srv, err
}

func (m *Manager) awaitServer(ctx context.Context, sl *slot) (*server, error) {
	for {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return nil, closedError()
		}
		var wait <-chan struct{}
		var attempt *startAttempt
		switch sl.state {
		case stateReady:
			srv := sl.srv
			m.mu.Unlock()
			return srv, nil
		case stateRetiring:
			wait = sl.retired
		case stateStarting:
			attempt = sl.attempt
		default:
			now := m.timing.now()
			if now.Before(sl.circuitUntil) {
				m.mu.Unlock()
				return nil, &Error{Code: CodeServerUnavailable, Detail: fmt.Sprintf("the %s language server failed repeatedly; restarts are paused", sl.route.name), RetryAfter: sl.circuitUntil.Sub(now)}
			}
			if now.Before(sl.nextStart) {
				m.mu.Unlock()
				return nil, &Error{Code: CodeServerUnavailable, Detail: fmt.Sprintf("the %s language server stopped recently; retry after the cooldown", sl.route.name), RetryAfter: sl.nextStart.Sub(now)}
			}
			attempt = &startAttempt{done: make(chan struct{})}
			sl.state, sl.attempt = stateStarting, attempt
			sl.generation++
			m.tasks.Add(1)
			go m.start(sl, attempt, sl.generation)
		}
		m.mu.Unlock()
		if attempt != nil {
			wait = attempt.done
		}
		select {
		case <-wait:
			if attempt != nil && attempt.err != nil {
				return nil, attempt.err
			}
		case <-ctx.Done():
			return nil, asError(ctx.Err())
		}
	}
}

// start launches one generation on the manager's lifetime, not the query's.
func (m *Manager) start(sl *slot, attempt *startAttempt, generation uint64) {
	defer m.tasks.Done()
	ctx, cancel := context.WithTimeout(m.ctx, m.timing.startup)
	srv, err := m.launch(ctx, sl.route, sl.cfg, generation)
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	cancel()
	var discard *server
	m.mu.Lock()
	switch {
	case err == nil && m.closed:
		discard, attempt.err = srv, closedError()
	case err != nil && m.closed:
		attempt.err = closedError()
	case err != nil:
		attempt.err = asError(err)
		if timedOut {
			attempt.err = newError(CodeServerUnavailable, fmt.Sprintf("the %s language server didn't initialize within %s", sl.route.name, m.timing.startup))
		}
		if attempt.err.Code != CodeServerMissing {
			m.recordFailure(sl)
			attempt.err.RetryAfter = sl.nextStart.Sub(m.timing.now())
		}
	}
	if attempt.err != nil {
		sl.state = stateStopped
	} else {
		sl.state, sl.srv = stateReady, srv
		sl.lastActive = m.timing.now()
		sl.resetDirty()
		m.tasks.Add(1)
		go m.watchServer(sl, srv)
	}
	close(attempt.done)
	m.mu.Unlock()
	if discard != nil {
		discard.stop(false)
	}
}

// recordFailure applies the cooldown and opens the circuit after repeated
// failures within the window. Caller holds m.mu.
func (m *Manager) recordFailure(sl *slot) {
	now := m.timing.now()
	kept := sl.failures[:0]
	for _, failure := range sl.failures {
		if now.Sub(failure) < m.timing.circuitWindow {
			kept = append(kept, failure)
		}
	}
	sl.failures = append(kept, now)
	sl.nextStart = now.Add(m.timing.cooldown)
	if len(sl.failures) >= m.timing.circuitFailures {
		sl.circuitUntil = now.Add(m.timing.circuitOpen)
		sl.nextStart = sl.circuitUntil
		sl.failures = nil
	}
}

// watchServer retires a generation that exits or loses its connection.
func (m *Manager) watchServer(sl *slot, srv *server) {
	defer m.tasks.Done()
	select {
	case <-srv.exited:
	case <-srv.conn.Done():
	case <-srv.gone:
		return
	}
	m.mu.Lock()
	if sl.srv == srv && sl.state == stateReady {
		sl.state, sl.srv = stateStopped, nil
		sl.resetDirty()
		if !m.closed {
			m.recordFailure(sl)
		}
	}
	m.mu.Unlock()
	srv.markGone()
	srv.stop(false)
}

// retireLocked moves a ready slot to retiring. Caller holds m.mu and must
// call finishRetire.
func (m *Manager) retireLocked(sl *slot) *server {
	srv := sl.srv
	sl.state, sl.srv = stateRetiring, nil
	sl.retired = make(chan struct{})
	sl.resetDirty()
	srv.markGone()
	return srv
}

func (m *Manager) finishRetire(sl *slot, srv *server) {
	srv.stop(true)
	m.mu.Lock()
	sl.state = stateStopped
	close(sl.retired)
	m.mu.Unlock()
}

// retireNow gracefully retires srv if it is still the slot's server.
func (m *Manager) retireNow(sl *slot, srv *server) {
	m.mu.Lock()
	if sl.srv != srv || sl.state != stateReady {
		m.mu.Unlock()
		return
	}
	m.retireLocked(sl)
	m.mu.Unlock()
	m.finishRetire(sl, srv)
}

func (m *Manager) release(sl *slot) {
	m.mu.Lock()
	sl.active--
	sl.lastActive = m.timing.now()
	m.mu.Unlock()
}

func (m *Manager) enterLane(ctx context.Context, sl *slot) error {
	select {
	case sl.lane <- struct{}{}:
		return nil
	case <-ctx.Done():
		return asError(ctx.Err())
	}
}

func (m *Manager) leaveLane(sl *slot) { <-sl.lane }

func (m *Manager) watchChanges() *watch {
	w := &watch{paths: map[string]bool{}}
	m.mu.Lock()
	m.watches[w] = struct{}{}
	m.mu.Unlock()
	return w
}

func (m *Manager) unwatch(w *watch) {
	m.mu.Lock()
	delete(m.watches, w)
	m.mu.Unlock()
}

func (m *Manager) changedSince(w *watch, p string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return w.all || w.paths[p]
}

// Changed marks a canonical root-relative path dirty after a successful disk
// write. It never starts a server, performs I/O, waits or fails.
func (m *Manager) Changed(p string) {
	if p == "" || len(p) > maxQueryPath || strings.ContainsRune(p, 0) {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.changeSeq++
	for w := range m.watches {
		w.record(p)
	}
	for _, sl := range m.slots {
		if sl.state != stateReady {
			continue
		}
		if _, known := sl.dirty[p]; known || len(sl.dirty) < maxDirtyPaths {
			sl.dirty[p] = m.changeSeq
		} else {
			sl.dirtyAll, sl.dirtyAllSeq = true, m.changeSeq
		}
		if !sl.route.handles(p) {
			// The server isn't told about this file, so earlier diagnostics are uncertain.
			sl.srv.markUncertain()
		}
	}
	m.mu.Unlock()
	m.kick()
}

func (m *Manager) kick() {
	select {
	case m.signal <- struct{}{}:
	default:
	}
}

// worker is the manager's single timer and change-coalescing loop.
func (m *Manager) worker() {
	defer close(m.workerDone)
	timer := time.NewTimer(m.timing.idle)
	defer timer.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.signal:
			m.scheduleSyncs()
		case <-timer.C:
		}
		timer.Reset(m.retireIdle())
	}
}

// retireIdle retires servers with no query, sync or startup for the idle
// period and returns the delay until the next check.
func (m *Manager) retireIdle() time.Duration {
	type retirement struct {
		sl  *slot
		srv *server
	}
	var retiring []retirement
	next := m.timing.idle
	m.mu.Lock()
	if !m.closed {
		now := m.timing.now()
		for _, sl := range m.slots {
			if sl.state != stateReady || sl.active > 0 {
				continue
			}
			if idle := now.Sub(sl.lastActive); idle >= m.timing.idle {
				retiring = append(retiring, retirement{sl, m.retireLocked(sl)})
				m.tasks.Add(1)
			} else if remaining := m.timing.idle - idle; remaining < next {
				next = remaining
			}
		}
	}
	m.mu.Unlock()
	for _, r := range retiring {
		go func() {
			defer m.tasks.Done()
			m.finishRetire(r.sl, r.srv)
		}()
	}
	return next
}

func (m *Manager) scheduleSyncs() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	for _, sl := range m.slots {
		if sl.state == stateReady && !sl.syncing && (len(sl.dirty) > 0 || sl.dirtyAll) {
			sl.syncing = true
			sl.active++
			m.tasks.Add(1)
			go m.syncSlot(sl, sl.srv)
		}
	}
}

// syncSlot pushes coalesced changes to a ready server: changed open files get
// didChange, newly edited routed files are opened. A path whose epoch moved
// during its read stays dirty.
func (m *Manager) syncSlot(sl *slot, srv *server) {
	defer m.tasks.Done()
	defer func() {
		m.mu.Lock()
		sl.syncing = false
		sl.active--
		sl.lastActive = m.timing.now()
		again := sl.state == stateReady && (len(sl.dirty) > 0 || sl.dirtyAll)
		m.mu.Unlock()
		if again {
			m.kick()
		}
	}()
	ctx, cancel := context.WithTimeout(m.ctx, m.timing.request)
	defer cancel()
	if m.enterLane(ctx, sl) != nil {
		return
	}
	defer m.leaveLane(sl)
	m.mu.Lock()
	if sl.srv != srv {
		m.mu.Unlock()
		return
	}
	paths := maps.Clone(sl.dirty)
	all, allSeq := sl.dirtyAll, sl.dirtyAllSeq
	m.mu.Unlock()
	if all {
		if err := srv.reconcileOpen(ctx, ""); err != nil {
			m.logger.Debug("language server sync failed", slog.String("server", sl.route.name), slog.String("error", err.Error()))
		}
	}
	for p, seq := range paths {
		if err := m.syncChanged(ctx, sl, srv, p); err != nil {
			m.logger.Debug("language server sync failed", slog.String("server", sl.route.name), slog.String("error", err.Error()))
			if ctx.Err() != nil || !srv.alive() {
				return
			}
		}
		m.mu.Lock()
		if sl.dirty[p] == seq {
			delete(sl.dirty, p)
		}
		m.mu.Unlock()
	}
	m.mu.Lock()
	if all && sl.dirtyAllSeq == allSeq {
		sl.dirtyAll = false
	}
	m.mu.Unlock()
}

func (m *Manager) syncChanged(ctx context.Context, sl *slot, srv *server, p string) error {
	if srv.openDocument(p) != nil {
		return srv.reconcilePath(ctx, p)
	}
	if !sl.route.handles(p) {
		return nil
	}
	routed, languageID, err := m.routeFor(ctx, p, "")
	if err != nil || routed != sl {
		return nil
	}
	doc, err := m.ws.ReadFile(ctx, p)
	if err != nil || doc.Path != p {
		return nil
	}
	_, err = srv.syncDocument(ctx, doc, languageID)
	return err
}

// Close cancels admission, startups, queries and the worker, retires every
// server and joins its process, readers and timers, then closes the
// Workspace. It is idempotent; an expired ctx returns early while the
// teardown finishes in the background.
func (m *Manager) Close(ctx context.Context) error {
	m.closeOnce.Do(func() { go m.shutdown() })
	select {
	case <-m.closeDone:
		return m.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) shutdown() {
	defer close(m.closeDone)
	type retirement struct {
		sl  *slot
		srv *server
	}
	var retiring []retirement
	m.mu.Lock()
	m.closed = true
	for _, sl := range m.slots {
		if sl.state == stateReady {
			retiring = append(retiring, retirement{sl, m.retireLocked(sl)})
		}
	}
	m.mu.Unlock()
	m.cancel()
	<-m.workerDone
	m.queries.Wait()
	var group sync.WaitGroup
	for _, r := range retiring {
		group.Go(func() { m.finishRetire(r.sl, r.srv) })
	}
	group.Wait()
	m.tasks.Wait()
	m.closeErr = m.ws.Close()
}
