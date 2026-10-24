package tempest

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"go.harness.dev/harness/internal/config"
	"go.harness.dev/harness/internal/mcpconfig"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/extension"
)

// HostLimits bounds work shared by independently running conversations.
// Zero leaves a resource unlimited. Full limits reject new work rather than
// making a parent wait for a slot held by its own child.
type HostLimits struct {
	MaxConcurrentRuns      int
	MaxConcurrentProcesses int
}

// ErrHostLimit means the host has no capacity for another run or process.
var ErrHostLimit = errors.New("host resource limit reached")

// HostOptions supplies runtime configuration shared by hosted conversations.
// Store ownership transfers only after OpenHost succeeds.
type HostOptions struct {
	SessionDir      string
	Provider        ProviderConfig
	Stack           *StackConfig
	Prompter        Prompter
	Logger          *slog.Logger
	FauxScript      string
	MCPServers      []MCPServerConfig
	Store           durable.Store
	Registry        *extension.Registry
	Extensions      []extension.Selection
	Options         Options
	Authorize       Authorizer
	ClientNamespace func(context.Context) string
	Limits          HostLimits
}

// AgentOptions selects a new session and its workspace.
type AgentOptions struct {
	Cwd string
	New *NewSession
}

// ListOptions selects a bounded page of published sessions.
type ListOptions struct {
	Cwd    string
	Cursor string
	Limit  int
}

// SessionInfo describes an archived or live conversation.
type SessionInfo struct {
	SessionID string
	Cwd       string
	Title     string
	UpdatedAt time.Time
	LastRun   *RunSummary
}

// SessionPage holds a bounded catalog page.
type SessionPage struct {
	Sessions   []SessionInfo
	NextCursor string
}

const (
	hostCatalogSession   = "_tempest/host"
	hostCatalogNamespace = "_tempest/catalog"
	hostCatalogPageSize  = 100
	hostCatalogLockRetry = 10 * time.Millisecond
)

type catalogSession struct {
	SessionID, Cwd, Title, Path string
	CreatedAt                   time.Time
	UpdatedAt                   time.Time
	LastRun                     *RunSummary
}
type hostCursor struct {
	Cwd       string
	Cut       uint64
	AfterTime time.Time
	AfterID   string
}

// Host owns a registry of independent, single-run sessions.
type Host struct {
	mu              sync.Mutex
	options         HostOptions
	sessions        map[string]*Session
	opening         map[string]chan struct{}
	alarmRetry      map[string]time.Time
	openers         sync.WaitGroup
	ctx             context.Context
	cancel          context.CancelFunc
	runs, processes chan struct{}
	closed          bool
	closeOnce       sync.Once
	closeDone       chan struct{}
	closeErr        error
}

// OpenHost creates an idle host and recovers publication of initialized sessions.
// It doesn't run providers, hooks or accepted input.
func OpenHost(ctx context.Context, options HostOptions) (*Host, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.Limits.MaxConcurrentRuns < 0 || options.Limits.MaxConcurrentProcesses < 0 {
		return nil, fmt.Errorf("host limits must not be negative")
	}
	if options.SessionDir == "" {
		cwd := options.Options.Cwd
		if cwd == "" {
			var err error
			cwd, err = os.Getwd()
			if err != nil {
				return nil, err
			}
		}
		resolved, err := (config.Config{WorkingDir: cwd}).Resolve()
		if err != nil {
			return nil, err
		}
		options.SessionDir = resolved.SessionDir()
	}
	lifetime, cancel := context.WithCancel(context.Background())
	host := &Host{options: options, sessions: make(map[string]*Session), opening: make(map[string]chan struct{}), alarmRetry: make(map[string]time.Time), ctx: lifetime, cancel: cancel, closeDone: make(chan struct{})}
	if options.Limits.MaxConcurrentRuns > 0 {
		host.runs = make(chan struct{}, options.Limits.MaxConcurrentRuns)
	}
	if options.Limits.MaxConcurrentProcesses > 0 {
		host.processes = make(chan struct{}, options.Limits.MaxConcurrentProcesses)
	}
	catalog, owned, err := host.openCatalog(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	err = host.syncCatalog(ctx, catalog)
	if owned {
		err = errors.Join(err, catalog.Close(context.Background()))
	}
	if err != nil {
		cancel()
		return nil, err
	}
	return host, nil
}

func (host *Host) operationContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(host.ctx, cancel)
	return ctx, func() { stop(); cancel() }
}
func releaseUnlimitedHostPermit() {}
func (host *Host) admit(ctx context.Context, slots chan struct{}, resource string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := host.ctx.Err(); err != nil {
		return nil, ErrClosed
	}
	if slots == nil {
		return releaseUnlimitedHostPermit, nil
	}
	select {
	case slots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-slots }) }, nil
	default:
		return nil, fmt.Errorf("%w: concurrent %s", ErrHostLimit, resource)
	}
}
func (host *Host) admitRun(ctx context.Context) (func(), error) {
	return host.admit(ctx, host.runs, "runs")
}
func (host *Host) admitProcess(ctx context.Context) (func(), error) {
	return host.admit(ctx, host.processes, "processes")
}
func (host *Host) sessionOptions() Options {
	options := host.options.Options
	options.SessionDir = host.options.SessionDir
	options.Store = host.options.Store
	options.admitRun = host.admitRun
	options.admitProcess = host.admitProcess
	if host.options.Authorize != nil {
		options.Authorize = host.options.Authorize
	}
	if host.options.ClientNamespace != nil {
		options.ClientNamespace = host.options.ClientNamespace
	}
	if host.options.Registry != nil {
		options.Registry = host.options.Registry
	}
	if host.options.Extensions != nil {
		options.Extensions = host.options.Extensions
	}
	if host.options.Stack != nil {
		options.Stack = host.options.Stack
	}
	if host.options.Prompter != nil {
		options.Prompter = host.options.Prompter
	}
	if host.options.Logger != nil {
		options.Logger = host.options.Logger
	}
	if host.options.FauxScript != "" {
		options.FauxScript = host.options.FauxScript
	}
	if host.options.Provider.ModelID != "" || host.options.Provider.Providers != nil || host.options.Provider.OpenAIBaseURL != "" || host.options.Provider.AnthropicBaseURL != "" || host.options.Provider.ChatGPTCredentials != nil {
		options.Provider = host.options.Provider
	}
	return options
}

// openCatalog holds the stable backend lease through initialization/publication.
// A caller-owned backend already provides its transaction-domain writer lease.
func (host *Host) openCatalog(ctx context.Context) (durable.Store, bool, error) {
	if host.options.Store != nil {
		return host.options.Store, false, nil
	}
	for {
		catalog, err := durable.OpenFile(ctx, filepath.Join(host.options.SessionDir, ".tempest-host"))
		if !errors.Is(err, durable.ErrLocked) {
			return catalog, true, err
		}
		timer := time.NewTimer(hostCatalogLockRetry)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, false, ctx.Err()
		case <-timer.C:
		}
	}
}
func publishSession(ctx context.Context, catalog durable.Store, entry catalogSession) error {
	existing, err := catalog.Read(ctx, durable.Query{SessionID: hostCatalogSession, Kind: durable.DocumentKind, Namespace: hostCatalogNamespace, ID: entry.SessionID, Limit: 1})
	if err != nil {
		return err
	}
	var revision uint64
	if len(existing.Records) > 0 {
		revision = existing.Records[0].Revision
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if revision > 0 && string(existing.Records[0].Value) == string(raw) {
		return nil
	}
	digest := sha256.Sum256(raw)
	_, err = catalog.Commit(ctx, durable.Commit{ID: fmt.Sprintf("publish:%s:%d:%x", entry.SessionID, revision, digest), Expected: []durable.RevisionCheck{{SessionID: hostCatalogSession, Kind: durable.DocumentKind, Namespace: hostCatalogNamespace, ID: entry.SessionID, Revision: revision}}, Changes: []durable.Change{{SessionID: hostCatalogSession, Kind: durable.DocumentKind, Namespace: hostCatalogNamespace, ID: entry.SessionID, Version: 1, Value: raw}}})
	return err
}
func (host *Host) syncCatalog(ctx context.Context, catalog durable.Store) error {
	if host.options.Store == nil {
		entries, err := journal.Catalog(ctx, host.options.SessionDir, "")
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.Initialized {
				continue
			}
			var last *RunSummary
			if entry.LastRun != nil {
				r := entry.LastRun
				last = &RunSummary{RunID: r.RunID, StopReason: r.StopReason, Error: r.Error, Finished: r.Finished, FinishedAt: r.FinishedAt}
			}
			if err := publishSession(ctx, catalog, catalogSession{SessionID: entry.Header.ID, Cwd: entry.Header.Cwd, Title: entry.Header.Title, Path: entry.Path, CreatedAt: entry.Header.Timestamp, UpdatedAt: entry.UpdatedAt, LastRun: last}); err != nil {
				return err
			}
		}
		return nil
	}
	query := durable.Query{Kind: durable.DocumentKind, Namespace: "_tempest/header", ID: "header", Limit: hostCatalogPageSize}
	for {
		page, err := catalog.Read(ctx, query)
		if err != nil {
			return err
		}
		for _, record := range page.Records {
			ready, err := catalog.Read(ctx, durable.Query{SessionID: record.SessionID, Kind: durable.DocumentKind, Namespace: "_tempest/agent", ID: "selection", AsOf: page.Seq, Limit: 1})
			if err != nil {
				return err
			}
			if len(ready.Records) == 0 {
				continue
			}
			var header journal.Header
			if err := json.Unmarshal(record.Value, &header); err != nil {
				return fmt.Errorf("catalog header: %w", err)
			}
			if err := publishSession(ctx, catalog, catalogSession{SessionID: header.ID, Cwd: header.Cwd, Title: header.Title, CreatedAt: header.Timestamp}); err != nil {
				return err
			}
		}
		if page.NextCursor == "" {
			return nil
		}
		query.Cursor = page.NextCursor
	}
}

// Create publishes a conversation only after all initialization has committed.
func (host *Host) Create(ctx context.Context, agent AgentOptions) (created *Session, err error) {
	host.mu.Lock()
	if host.closed {
		host.mu.Unlock()
		return nil, ErrClosed
	}
	host.openers.Add(1)
	host.mu.Unlock()
	defer host.openers.Done()
	ctx, cancel := host.operationContext(ctx)
	defer cancel()
	options := host.sessionOptions()
	options.Cwd = agent.Cwd
	options.New = agent.New
	options.Resume = nil
	options.hostCreate = true
	if options.New == nil {
		options.New = &NewSession{}
	} else {
		copy := *options.New
		options.New = &copy
	}
	if options.New.MCPServers == nil {
		options.New.MCPServers = host.options.MCPServers
	}
	created, err = Open(ctx, options)
	if err != nil {
		return nil, err
	}
	catalog, owned, err := host.openCatalog(ctx)
	if err == nil {
		snap := created.Snapshot()
		path := ""
		if host.options.Store == nil {
			path = created.core.store.Path()
		}
		now := time.Now()
		err = publishSession(ctx, catalog, catalogSession{SessionID: created.ID(), Cwd: snap.Cwd, Path: path, CreatedAt: now, UpdatedAt: now})
		if owned {
			err = errors.Join(err, catalog.Close(context.Background()))
		}
	}
	host.mu.Lock()
	closed := host.closed || host.ctx.Err() != nil
	if err == nil && !closed {
		host.sessions[created.ID()] = created
	}
	host.mu.Unlock()
	if err != nil || closed {
		if closed {
			err = errors.Join(err, ErrClosed)
		}
		return nil, errors.Join(err, created.Close(context.Background()))
	}
	return created, nil
}

// Open reuses a live writer or opens a published session without executing it.
func (host *Host) Open(ctx context.Context, id string) (*Session, error) {
	return host.OpenWithOptions(ctx, id, ResumeSession{})
}

// OpenWithOptions applies explicit runtime overrides to a published session.
func (host *Host) OpenWithOptions(ctx context.Context, id string, resume ResumeSession) (opened *Session, err error) {
	host.mu.Lock()
	if host.closed {
		host.mu.Unlock()
		return nil, ErrClosed
	}
	host.openers.Add(1)
	host.mu.Unlock()
	defer host.openers.Done()
	ctx, cancel := host.operationContext(ctx)
	defer cancel()
	catalog, owned, err := host.openCatalog(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if owned {
			err = errors.Join(err, catalog.Close(context.Background()))
		}
	}()
	query := durable.Query{SessionID: hostCatalogSession, Kind: durable.DocumentKind, Namespace: hostCatalogNamespace, Limit: hostCatalogPageSize}
	var selected catalogSession
	for {
		page, readErr := catalog.Read(ctx, query)
		if readErr != nil {
			return nil, readErr
		}
		for _, record := range page.Records {
			var entry catalogSession
			if err = json.Unmarshal(record.Value, &entry); err != nil {
				return nil, err
			}
			if entry.SessionID == id || entry.Path == id || entry.Path != "" && filepath.Base(entry.Path) == id {
				selected = entry
				break
			}
		}
		if selected.SessionID != "" || page.NextCursor == "" {
			break
		}
		query.Cursor = page.NextCursor
	}
	if selected.SessionID == "" {
		return nil, journal.ErrSessionNotFound
	}
	if owned {
		if err := catalog.Close(context.Background()); err != nil {
			return nil, err
		}
		owned = false
	}
	for {
		host.mu.Lock()
		if host.closed || host.ctx.Err() != nil {
			host.mu.Unlock()
			return nil, ErrClosed
		}
		if session := host.sessions[selected.SessionID]; session != nil {
			session.core.mu.Lock()
			closed := session.core.closed
			session.core.mu.Unlock()
			if !closed {
				host.mu.Unlock()
				if resume.MCPServers != nil && !session.MCPServersEqual(resume.MCPServers) {
					return nil, ErrMCPConfigConflict
				}
				return session, nil
			}
			host.mu.Unlock()
			if err := session.Close(ctx); err != nil {
				return nil, err
			}
			host.mu.Lock()
			if host.sessions[selected.SessionID] == session {
				delete(host.sessions, selected.SessionID)
			}
			host.mu.Unlock()
			continue
		}
		if pending := host.opening[selected.SessionID]; pending != nil {
			host.mu.Unlock()
			select {
			case <-pending:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		host.opening[selected.SessionID] = make(chan struct{})
		host.mu.Unlock()
		break
	}
	defer func() {
		host.mu.Lock()
		close(host.opening[selected.SessionID])
		delete(host.opening, selected.SessionID)
		host.mu.Unlock()
	}()
	options := host.sessionOptions()
	options.Cwd = selected.Cwd
	options.New = nil
	resume.SessionID = selected.SessionID
	resume.Path = selected.Path
	if resume.MCPServers == nil {
		resume.MCPServers = host.options.MCPServers
	}
	options.Resume = &resume
	opened, err = Open(ctx, options)
	if err != nil {
		return nil, err
	}
	host.mu.Lock()
	closed := host.closed || host.ctx.Err() != nil
	if !closed {
		host.sessions[opened.ID()] = opened
	}
	host.mu.Unlock()
	if closed {
		return nil, errors.Join(ErrClosed, opened.Close(context.Background()))
	}
	return opened, nil
}

// List pages the published catalog newest first at one immutable cut.
func (host *Host) List(ctx context.Context, options ListOptions) (result SessionPage, err error) {
	host.mu.Lock()
	if host.closed {
		host.mu.Unlock()
		return result, ErrClosed
	}
	host.openers.Add(1)
	host.mu.Unlock()
	defer host.openers.Done()
	if options.Limit == 0 {
		options.Limit = hostCatalogPageSize
	}
	if options.Limit < 1 || options.Limit > 1000 {
		return result, fmt.Errorf("%w: invalid catalog limit", ErrInvalidQueueInput)
	}
	cursor := hostCursor{Cwd: options.Cwd}
	if options.Cursor != "" {
		raw, decodeErr := base64.RawURLEncoding.DecodeString(options.Cursor)
		if decodeErr != nil || json.Unmarshal(raw, &cursor) != nil || cursor.Cwd != options.Cwd || cursor.AfterID == "" || cursor.Cut == 0 {
			return result, fmt.Errorf("%w: invalid catalog cursor", ErrInvalidQueueInput)
		}
	}
	ctx, cancel := host.operationContext(ctx)
	defer cancel()
	catalog, owned, err := host.openCatalog(ctx)
	if err != nil {
		return result, err
	}
	defer func() {
		if owned {
			err = errors.Join(err, catalog.Close(context.Background()))
		}
	}()
	if options.Cursor == "" {
		host.mu.Lock()
		live := make([]*Session, 0, len(host.sessions))
		for _, session := range host.sessions {
			live = append(live, session)
		}
		host.mu.Unlock()
		for _, session := range live {
			if err := host.refreshSession(ctx, catalog, session); err != nil {
				return result, err
			}
		}
	}
	if options.Cursor == "" {
		cut, readErr := catalog.Read(ctx, durable.Query{Limit: 1})
		if readErr != nil {
			return result, readErr
		}
		cursor.Cut = cut.Seq
	}
	result.Sessions = []SessionInfo{}
	newer := func(a, b SessionInfo) bool {
		if !a.UpdatedAt.Equal(b.UpdatedAt) {
			return a.UpdatedAt.After(b.UpdatedAt)
		}
		return a.SessionID > b.SessionID
	}
	query := durable.Query{SessionID: hostCatalogSession, Kind: durable.DocumentKind, Namespace: hostCatalogNamespace, AsOf: cursor.Cut, Limit: hostCatalogPageSize}
	for {
		page, readErr := catalog.Read(ctx, query)
		if readErr != nil {
			return result, readErr
		}
		for _, record := range page.Records {
			var entry catalogSession
			if err = json.Unmarshal(record.Value, &entry); err != nil {
				return result, err
			}
			if options.Cwd != "" && entry.Cwd != options.Cwd {
				continue
			}
			if entry.Path != "" {
				if _, statErr := os.Stat(entry.Path); statErr != nil {
					continue
				}
			}
			info := SessionInfo{SessionID: entry.SessionID, Cwd: entry.Cwd, Title: entry.Title, UpdatedAt: entry.UpdatedAt, LastRun: entry.LastRun}
			if info.UpdatedAt.IsZero() {
				info.UpdatedAt = entry.CreatedAt
			}
			if cursor.AfterID != "" && !newer(SessionInfo{SessionID: cursor.AfterID, UpdatedAt: cursor.AfterTime}, info) {
				continue
			}
			at := sort.Search(len(result.Sessions), func(i int) bool { return newer(info, result.Sessions[i]) })
			if at <= options.Limit {
				result.Sessions = append(result.Sessions, SessionInfo{})
				copy(result.Sessions[at+1:], result.Sessions[at:])
				result.Sessions[at] = info
				result.Sessions = result.Sessions[:min(len(result.Sessions), options.Limit+1)]
			}
		}
		if page.NextCursor == "" {
			break
		}
		query.Cursor = page.NextCursor
	}
	if len(result.Sessions) > options.Limit {
		result.Sessions = result.Sessions[:options.Limit]
		last := result.Sessions[len(result.Sessions)-1]
		cursor.AfterTime, cursor.AfterID = last.UpdatedAt, last.SessionID
		raw, marshalErr := json.Marshal(cursor)
		if marshalErr != nil {
			return result, marshalErr
		}
		result.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return result, nil
}

func (host *Host) refreshSession(ctx context.Context, catalog durable.Store, session *Session) error {
	page, err := catalog.Read(ctx, durable.Query{SessionID: hostCatalogSession, Kind: durable.DocumentKind, Namespace: hostCatalogNamespace, ID: session.ID(), Limit: 1})
	if err != nil || len(page.Records) == 0 {
		return err
	}
	var entry catalogSession
	if err := json.Unmarshal(page.Records[0].Value, &entry); err != nil {
		return err
	}
	entry.LastRun = session.Snapshot().LastRun
	if stat, err := os.Stat(entry.Path); err == nil {
		entry.UpdatedAt = stat.ModTime()
	}
	return publishSession(ctx, catalog, entry)
}

func (host *Host) archiveSession(ctx context.Context, session *Session) (err error) {
	catalog, owned, err := host.openCatalog(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if owned {
			err = errors.Join(err, catalog.Close(context.Background()))
		}
	}()
	return host.refreshSession(ctx, catalog, session)
}

// CloseSession joins local work before releasing its writer.
func (host *Host) CloseSession(ctx context.Context, id string) error {
	host.mu.Lock()
	session := host.sessions[id]
	host.mu.Unlock()
	if session == nil {
		return nil
	}
	err := session.Close(ctx)
	if err != nil {
		go func() {
			if closeErr := session.Close(context.Background()); closeErr != nil {
				logger := host.options.Logger
				if logger == nil {
					logger = slog.Default()
				}
				logger.Warn("host session close failed", "sessionId", id, "error", closeErr)
			}
			if err := host.archiveSession(context.Background(), session); err != nil {
				slog.Warn("host session catalog update failed", "sessionId", id, "error", err)
			}
			host.mu.Lock()
			if host.sessions[id] == session {
				delete(host.sessions, id)
			}
			host.mu.Unlock()
		}()
		return err
	}
	err = errors.Join(err, host.archiveSession(ctx, session))
	host.mu.Lock()
	if host.sessions[id] == session {
		delete(host.sessions, id)
	}
	host.mu.Unlock()
	return err
}

// ResumePending schedules accepted input and reopens archived tasks whose alarm is due.
func (host *Host) ResumePending(ctx context.Context) error {
	host.mu.Lock()
	if host.closed {
		host.mu.Unlock()
		return ErrClosed
	}
	sessions := make([]*Session, 0, len(host.sessions))
	for _, session := range host.sessions {
		sessions = append(sessions, session)
	}
	host.mu.Unlock()
	alarms, scanErr := host.archivedTaskAlarms(ctx)
	joined := scanErr
	for _, session := range sessions {
		if err := session.ResumePending(ctx); err != nil {
			joined = errors.Join(joined, fmt.Errorf("session %s: %w", session.ID(), err))
		}
	}
	now := time.Now()
	if host.options.Options.Tasks.Now != nil {
		now = host.options.Options.Tasks.Now()
	}
	for id, wake := range alarms {
		host.mu.Lock()
		open := host.sessions[id] != nil
		retryAt := host.alarmRetry[id]
		host.mu.Unlock()
		if open || now.Before(wake) || now.Before(retryAt) {
			continue
		}
		err := host.resumeAlarmSession(ctx, id)
		host.mu.Lock()
		if err != nil {
			host.alarmRetry[id] = now.Add(taskAlarmRetryDelay)
		} else {
			delete(host.alarmRetry, id)
		}
		host.mu.Unlock()
		if err != nil {
			joined = errors.Join(joined, fmt.Errorf("session %s: %w", id, err))
		}
	}
	return joined
}

const taskAlarmRetryDelay = time.Minute

func (host *Host) resumeAlarmSession(ctx context.Context, id string) (drainErr error) {
	session, err := host.Open(ctx, id)
	if err != nil {
		return err
	}
	defer func() {
		drainErr = errors.Join(drainErr, host.CloseSession(ctx, id))
	}()
	run, err := session.resumePending(ctx)
	if err != nil {
		return err
	}
	if run != nil {
		select {
		case <-run.done:
			_, err = run.Wait()
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	return err
}

// NextWake reports the earliest persisted alarm, including archived sessions.
func (host *Host) NextWake() (time.Time, bool) {
	host.mu.Lock()
	sessions := make([]*Session, 0, len(host.sessions))
	for _, session := range host.sessions {
		sessions = append(sessions, session)
	}
	host.mu.Unlock()
	var earliest time.Time
	for _, session := range sessions {
		if deadline, ok := session.NextWake(); ok && (earliest.IsZero() || deadline.Before(earliest)) {
			earliest = deadline
		}
	}
	alarms, err := host.archivedTaskAlarms(context.Background())
	if err != nil {
		slog.Warn("host task alarm scan failed", "error", err)
	}
	for id, deadline := range alarms {
		host.mu.Lock()
		if retryAt := host.alarmRetry[id]; deadline.Before(retryAt) {
			deadline = retryAt
		}
		host.mu.Unlock()
		open := false
		for _, session := range sessions {
			open = open || session.ID() == id
		}
		if !open && (earliest.IsZero() || deadline.Before(earliest)) {
			earliest = deadline
		}
	}
	return earliest, !earliest.IsZero()
}

// Close joins sessions before closing the host-owned backend. A cancelled wait
// doesn't release a backend that still has live users.
func (host *Host) Close(ctx context.Context) error {
	host.closeOnce.Do(func() {
		host.cancel()
		go func() {
			host.mu.Lock()
			host.closed = true
			sessions := host.sessions
			host.sessions = make(map[string]*Session)
			host.mu.Unlock()
			host.openers.Wait()
			var closeErr error
			for _, session := range sessions {
				closeErr = errors.Join(closeErr, session.Close(context.Background()))
			}
			if host.options.Store != nil {
				closeErr = errors.Join(closeErr, host.options.Store.Close(context.Background()))
			}
			host.closeErr = closeErr
			close(host.closeDone)
		}()
	})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-host.closeDone:
		return host.closeErr
	}
}

// MCPServersEqual reports whether explicit endpoints match the live selection.
func (session *Session) MCPServersEqual(servers []MCPServerConfig) bool {
	normalized, err := mcpconfig.Normalize(servers)
	if err != nil {
		return false
	}
	return reflect.DeepEqual(normalized, session.core.mcpServers)
}

// Access names a host operation before it reads or mutates durable state.
type Access struct {
	SessionID string
	Action    string
	Namespace string
	ID        string
}

// Authorizer uses the host's authenticated context. Nil trusts the operator.
type Authorizer func(context.Context, Access) error

func (session *Session) authorize(ctx context.Context, action, namespace, id string) error {
	if session.core.authorize != nil {
		return session.core.authorize(ctx, Access{SessionID: session.ID(), Action: action, Namespace: namespace, ID: id})
	}
	return ctx.Err()
}
func (session *Session) submissionContext(ctx context.Context) (context.Context, error) {
	namespace := durable.ClientNamespace(ctx)
	if namespace == "" && session.core.clientNamespace != nil {
		namespace = session.core.clientNamespace(ctx)
	}
	if len(namespace) > 128 || strings.ContainsRune(namespace, 0) {
		return nil, ErrInvalidQueueInput
	}
	if namespace != "" {
		ctx = durable.WithClientNamespace(ctx, namespace)
	}
	return ctx, nil
}
