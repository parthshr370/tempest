package githubsource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.harness.dev/harness/internal/githubsource/control"
	"go.harness.dev/harness/internal/githubsource/dispatch"
	"go.harness.dev/harness/internal/githubsource/gh"
	"go.harness.dev/harness/internal/githubsource/ghext"
	"go.harness.dev/harness/internal/githubsource/model"
	"go.harness.dev/harness/internal/githubsource/outbox"
	"go.harness.dev/harness/internal/githubsource/source"
	"go.harness.dev/harness/tempest"
	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/extension"
)

// tokenVariables select GitHub credentials ahead of GH_CONFIG_DIR. Worker
// sessions drop them (ghext.WorkerEnviron), but every other session of the
// serve process would inherit them, so startup refuses them.
var tokenVariables = []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}

const (
	// minPollWait keeps the poll loop from spinning when a round reports a
	// next poll time that already passed.
	minPollWait = time.Second
	// loopRetry is the wait before a dispatcher or outbox loop that stopped
	// on an error starts again.
	loopRetry = 5 * time.Second
)

// Options configures Open. Config is a file validated by LoadConfig; the
// other fields default to the production values.
type Options struct {
	Config model.Config
	Logger *slog.Logger
	// Clock is the controller's time source. Nil is the wall clock.
	Clock model.Clock
	// Environ is the serve process environment, checked for token
	// variables. Nil reads os.Environ.
	Environ []string
	// GHEnv is added to the environment of the account's gh subprocesses.
	GHEnv []string
	// GitEnv is added to the environment of the dispatcher's worktree git
	// commands and of the clone check.
	GitEnv []string
	// PushRepository makes the outbox push to a local bare repository
	// instead of https://github.com/<repository>.git; tests only.
	PushRepository string
	// DispatchInterval and OutboxInterval bound how long due work waits
	// without a wake; zero keeps the packages' defaults.
	DispatchInterval time.Duration
	OutboxInterval   time.Duration
}

// Service is one repository's ticket runner: the control journal, the poll
// loop, the dispatcher and the outbox. Open checks the account, repository
// and clone and takes the journal's lock; Start runs the loops over a Host
// whose registry holds Extension; Stop ends them and Close releases the
// journal.
type Service struct {
	config model.Config
	repo   model.RepoMeta
	log    *slog.Logger
	clock  model.Clock
	gitEnv []string

	store  *control.Store
	source *source.Source
	outbox *outbox.Outbox

	dispatchInterval time.Duration

	mu          sync.Mutex
	dispatcher  *dispatch.Dispatcher
	cancel      context.CancelFunc
	loops       sync.WaitGroup
	unsubscribe func()
	closed      bool
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

// Open runs the startup checks in order (token variables, the account,
// the repository and its push permission, the clone's remote), then opens
// the control journal under <stateDir>/control.jsonl. Nothing is polled or
// dispatched before Start.
func Open(ctx context.Context, options Options) (*Service, error) {
	config := options.Config
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	clock := options.Clock
	if clock == nil {
		clock = wallClock{}
	}
	environ := options.Environ
	if environ == nil {
		environ = os.Environ()
	}
	if err := checkEnvironment(environ); err != nil {
		return nil, err
	}
	client := gh.New(gh.Options{Path: config.Account.GHPath, ConfigDir: config.Account.GHConfigDir, Env: options.GHEnv, CallTimeout: time.Duration(config.Poll.CallTimeout), Clock: clock})
	account, err := checkAccount(ctx, client, config.Account)
	if err != nil {
		return nil, err
	}
	repo, err := checkRepository(ctx, client, config.Repository)
	if err != nil {
		return nil, err
	}
	repo.Account, repo.CreatedAt = account, clock.Now().UTC()
	remote := config.Workers.Remote
	if remote == "" {
		remote = defaultRemote
	}
	if err := checkClone(ctx, config.Clone, remote, config.Repository, options.GitEnv); err != nil {
		return nil, err
	}
	if err := privateStateDir(config.StateDir); err != nil {
		return nil, err
	}
	path := filepath.Join(config.StateDir, "control.jsonl")
	journal, err := durable.OpenFile(ctx, path)
	if err != nil {
		if errors.Is(err, durable.ErrLocked) {
			return nil, fmt.Errorf("github: control journal %s is locked by another serve for %s: %w", path, config.Repository, err)
		}
		return nil, fmt.Errorf("github: open control journal %s: %w", path, err)
	}
	store, err := control.Open(ctx, control.Options{Store: journal, Config: config, Policy: model.Policy{LoadedAt: clock.Now().UTC()}, Repo: repo, Clock: clock})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("github: open control state: %w", err), journal.Close(ctx))
	}
	service := &Service{config: config, repo: repo, log: logger.With("github", config.Repository), clock: clock, gitEnv: append([]string(nil), options.GitEnv...), store: store, dispatchInterval: options.DispatchInterval}
	service.source, err = source.New(source.Options{Client: client, Intake: store, Clock: clock, Config: config, Repo: repo, Account: account})
	if err == nil {
		service.outbox, err = outbox.New(outbox.Options{Client: client, Store: store, Config: config, Clock: clock, Logger: service.log, Interval: options.OutboxInterval, PushRepository: options.PushRepository})
	}
	if err != nil {
		return nil, errors.Join(err, store.Close(ctx))
	}
	return service, nil
}

// checkEnvironment refuses a serve process that carries a GitHub token: it
// would outrank the account's GH_CONFIG_DIR wherever it is inherited.
func checkEnvironment(environ []string) error {
	for _, entry := range environ {
		key, _, _ := strings.Cut(entry, "=")
		for _, name := range tokenVariables {
			if key == name {
				return fmt.Errorf("github: %s is set in the serve environment; unset it: the serve process's sessions would inherit it, and the account's credentials come only from account.ghConfigDir", name)
			}
		}
	}
	return nil
}

// privateStateDir creates the state directory 0700, or accepts an existing
// one only if it is a directory this user owns that no other user can
// write: it holds the control journal and the outbox's push repository.
func privateStateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("github: state directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("github: state directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("github: state directory %s is not a directory (a symlink is refused)", path)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("github: state directory %s has mode %o, writable by other users; chmod go-w it", path, info.Mode().Perm())
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Getuid() {
		return fmt.Errorf("github: state directory %s is owned by uid %d, not this user (%d)", path, stat.Uid, os.Getuid())
	}
	return nil
}

func get(ctx context.Context, client model.GHClient, path string, value any) error {
	response, err := client.Do(ctx, model.GHRequest{Method: "GET", Path: path})
	if err != nil {
		return err
	}
	if err := json.Unmarshal(response.Body, value); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

// checkAccount asks GitHub who GH_CONFIG_DIR authenticates as.
func checkAccount(ctx context.Context, client model.GHClient, want model.AccountConfig) (model.Actor, error) {
	var user struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
		Type  string `json:"type"`
	}
	if err := get(ctx, client, "user", &user); err != nil {
		return model.Actor{}, fmt.Errorf("github: read the account from account.ghConfigDir: %w", err)
	}
	if user.ID != want.ID || !strings.EqualFold(user.Login, want.Login) {
		return model.Actor{}, fmt.Errorf("github: account.ghConfigDir authenticates as %s (id %d), not account %s (id %d)", user.Login, user.ID, want.Login, want.ID)
	}
	return model.Actor{Login: user.Login, ID: user.ID, Type: user.Type}, nil
}

// checkRepository reads the repository's identity and requires push
// permission, which the outbox needs for branches, comments and PRs.
func checkRepository(ctx context.Context, client model.GHClient, repository string) (model.RepoMeta, error) {
	var repo struct {
		ID            int64           `json:"id"`
		FullName      string          `json:"full_name"`
		DefaultBranch string          `json:"default_branch"`
		Permissions   map[string]bool `json:"permissions"`
	}
	if err := get(ctx, client, "repos/"+repository, &repo); err != nil {
		return model.RepoMeta{}, fmt.Errorf("github: read repository %s as the account: %w", repository, err)
	}
	if !strings.EqualFold(repo.FullName, repository) {
		return model.RepoMeta{}, fmt.Errorf("github: repository %s answers as %q (renamed or transferred?)", repository, repo.FullName)
	}
	if !repo.Permissions["push"] {
		return model.RepoMeta{}, fmt.Errorf("github: the account has no push permission on %s", repository)
	}
	if repo.ID <= 0 || repo.DefaultBranch == "" {
		return model.RepoMeta{}, fmt.Errorf("github: repository %s reply has no id or default branch", repository)
	}
	return model.RepoMeta{Repo: repo.FullName, RepoID: repo.ID, DefaultBranch: repo.DefaultBranch}, nil
}

// checkClone requires the clone's configured remote URL, as written in its
// configuration (url.insteadOf is not applied), to name the repository.
func checkClone(ctx context.Context, clone, remote, repository string, gitEnv []string) error {
	command := exec.CommandContext(ctx, "git", "-C", clone, "config", "--get", "remote."+remote+".url")
	command.Env = append(os.Environ(), gitEnv...)
	out, err := command.Output()
	if err != nil {
		return fmt.Errorf("github: clone %s has no remote %q: %w", clone, remote, err)
	}
	raw := strings.TrimSpace(string(out))
	named, ok := remoteRepository(raw)
	if !ok || !strings.EqualFold(named, repository) {
		return fmt.Errorf("github: clone %s remote %q is %q, which is not github.com/%s", clone, remote, raw, repository)
	}
	return nil
}

// remoteRepository extracts owner/repo from a github.com remote URL in
// https, ssh:// or scp-like form.
func remoteRepository(raw string) (string, bool) {
	var path string
	if rest, found := strings.CutPrefix(raw, "git@github.com:"); found {
		path = rest
	} else {
		parsed, err := url.Parse(raw)
		if err != nil || !strings.EqualFold(parsed.Hostname(), "github.com") {
			return "", false
		}
		switch parsed.Scheme {
		case "https", "ssh":
		default:
			return "", false
		}
		path = strings.TrimPrefix(parsed.Path, "/")
	}
	path = strings.TrimSuffix(strings.TrimSuffix(path, "/"), ".git")
	if _, _, err := splitRepository(path); err != nil {
		return "", false
	}
	return path, true
}

// Extension is the github worker bundle bound to this repository's
// control journal. Install it in the Host's registry before Start.
func (s *Service) Extension() extension.Definition {
	return ghext.Definition(s.store, ghext.Options{Wake: s.outbox.Wake, GitEnv: s.gitEnv})
}

// ErrWorkerSession refuses an operation on a worker session that did not
// come from the dispatcher.
var ErrWorkerSession = errors.New("github: worker sessions run and change only through the ticket runner")

// readActions are the Authorizer actions that only read a session.
var readActions = map[string]bool{"read": true, "get_submission": true, "get_task": true, "list_tasks": true, "search_history": true}

// Authorize is the Host's Authorizer. A session bound to a Ticket runs and
// changes only from the dispatcher's context, so every worker run is a
// dispatch with its slot, budget and stop; other clients may read it and
// watch it. Pause, stop, retry and grant go through Control.
func (s *Service) Authorize(ctx context.Context, access tempest.Access) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if readActions[access.Action] || dispatch.Issued(ctx) {
		return nil
	}
	if ticket, ok := s.store.BoundTicket(access.SessionID); ok {
		return fmt.Errorf("%w: session %s is the worker of Ticket %s; %s is refused", ErrWorkerSession, access.SessionID, ticket, access.Action)
	}
	return nil
}

// Worker reports whether sessionID is bound to a Ticket as its worker.
func (s *Service) Worker(sessionID string) bool {
	_, ok := s.store.BoundTicket(sessionID)
	return ok
}

// WorkerResume is how anyone reopens a worker session: with the model and
// permission mode it journaled and no MCP servers, as the dispatcher
// created it.
func WorkerResume() tempest.ResumeSession { return dispatch.WorkerResume() }

// Start runs the dispatcher, the outbox and the poll loop concurrently
// until Stop; a Service starts once, and a restart opens a new one. The
// dispatcher resumes its live dispatches from their committed step before
// its first admission, but does not wait for them to settle; the outbox
// reconciles writes left attempting in its first pass. What restart keeps
// is per Ticket: a held slot is released only from a proven receipt, and
// one Ticket's writes run in order. host's registry must hold Extension
// and its Authorizer must be Authorize.
func (s *Service) Start(host *tempest.Host) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.cancel != nil {
		return errors.New("github: service already started or closed")
	}
	dispatcher, err := dispatch.New(dispatch.Options{Host: host, Store: s.store, Config: s.config, Repo: s.repo, Clock: s.clock, GitEnv: s.gitEnv, Logger: s.log, Interval: s.dispatchInterval})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.dispatcher, s.cancel = dispatcher, cancel
	// Every control commit may have created, stopped or released work.
	s.unsubscribe = s.store.Subscribe(func(uint64, []model.TicketID) {
		dispatcher.Wake()
		s.outbox.Wake()
	})
	s.loops.Add(3)
	go s.keepRunning(ctx, "dispatcher", dispatcher.Run)
	go s.keepRunning(ctx, "outbox", s.outbox.Run)
	go s.pollLoop(ctx)
	return nil
}

// keepRunning restarts a loop that stopped on an error until ctx ends.
func (s *Service) keepRunning(ctx context.Context, name string, run func(context.Context) error) {
	defer s.loops.Done()
	for {
		err := run(ctx)
		if ctx.Err() != nil {
			return
		}
		s.log.Error("github "+name+" stopped; restarting", "error", err, "after", loopRetry)
		timer := time.NewTimer(loopRetry)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// pollLoop runs a round, then waits until the round's NextPollAt, which
// carries the configured interval, GitHub's X-Poll-Interval and any
// rate-limit wait.
func (s *Service) pollLoop(ctx context.Context) {
	defer s.loops.Done()
	interval := time.Duration(s.config.Poll.Interval)
	for {
		report, err := s.source.Poll(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.log.Warn("github poll failed", "error", err)
		}
		for _, failure := range report.Errors {
			s.log.Warn("github poll resource failed", "resource", failure.Resource, "status", failure.Status, "error", failure.Message)
		}
		wait := interval
		if !report.NextPollAt.IsZero() {
			wait = report.NextPollAt.Sub(s.clock.Now())
		}
		timer := time.NewTimer(max(wait, minPollWait))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// Stop ends the loops and waits for them, or for ctx. Live dispatches keep
// their slots in the journal for the next process's Service to reconcile;
// this Service does not start again.
func (s *Service) Stop(ctx context.Context) error {
	s.mu.Lock()
	cancel, unsubscribe := s.cancel, s.unsubscribe
	s.unsubscribe = nil
	s.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	if unsubscribe != nil {
		unsubscribe()
	}
	done := make(chan struct{})
	go func() { s.loops.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("github: stop loops: %w", ctx.Err())
	}
}

// Close stops the loops and releases the control journal. Call it after
// the Host closed, so no worker tool call meets a closed journal. If the
// Host's close timed out, a worker still finishing then gets an error from
// the github tools; it cannot commit a write.
func (s *Service) Close(ctx context.Context) error {
	err := s.Stop(ctx)
	s.mu.Lock()
	closed := s.closed
	s.closed = true
	s.mu.Unlock()
	if closed {
		return err
	}
	return errors.Join(err, s.store.Close(ctx))
}

// Control is the operator surface of the control journal.
func (s *Service) Control() model.Control { return s.store.Operator() }

// Repo is the repository, owner/repo.
func (s *Service) Repo() string { return s.config.Repository }

// Health is the poll loop's in-memory health.
func (s *Service) Health() model.SourceHealth { return s.source.Health() }

// PollNow runs one round now. The source serializes it with the loop's.
func (s *Service) PollNow(ctx context.Context) (model.PollReport, error) {
	return s.source.Poll(ctx)
}

// Subscribe calls notify after every durable control commit with the
// Tickets it changed. notify must not block or call the Service.
func (s *Service) Subscribe(notify func(seq uint64, tickets []model.TicketID)) (cancel func()) {
	return s.store.Subscribe(notify)
}
