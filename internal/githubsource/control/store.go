// Package control is the GitHub ticket runner's control state: one Store over
// the repository's control journal that admits events, owns the Ticket,
// Assignment, dispatch and outbox state machines, schedules worker slots and
// keeps the budget ledgers.
//
// Every state change is one durable commit. The Store keeps a projection of
// the journal in memory, changes it only from committed bytes and rebuilds it
// from the journal after any commit error whose durability is unknown, so a
// restart at any commit boundary resumes from exactly what was committed.
// Methods are safe for concurrent use; one mutex serializes them, matching the
// journal's single writer.
package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
	"go.harness.dev/harness/tempest/durable"
)

// Options configures Open.
type Options struct {
	// Store is the repository's control journal, normally from
	// durable.OpenFile at <stateDir>/control.jsonl. A second process on the
	// same file fails there with durable.ErrLocked.
	Store  durable.Store
	Config model.Config
	// Policy is the admission policy in force. An empty Digest is computed.
	Policy model.Policy
	// Repo is the validated repository identity; it is recorded on first
	// open and must match on every later open.
	Repo  model.RepoMeta
	Clock model.Clock
}

// Store implements model.Intake, model.DispatchStore, model.EffectStore,
// model.WorkerGateway and model.Control over one control journal.
type Store struct {
	mu       sync.Mutex
	store    durable.Store
	clock    model.Clock
	policy   model.Policy
	repo     model.RepoMeta
	settings settings
	// broken is set when a reload after a failed commit also failed; the
	// projection can no longer be trusted and every call fails.
	broken error
	seq    uint64

	meta        *docTable[json.RawMessage]
	cursors     *docTable[model.Cursor]
	threads     *docTable[model.Thread]
	tickets     *docTable[model.Ticket]
	assignments *docTable[model.Assignment]
	dispatches  *docTable[model.DispatchIntent]
	sessions    *docTable[model.SessionBinding]
	prs         *docTable[model.PullRequest]
	outbox      *docTable[model.OutboxIntent]
	ledgers     *docTable[model.Ledger]

	policies  *entryTable[model.Policy]
	events    *entryTable[model.Event]
	decisions *entryTable[model.Decision]
	mandates  *entryTable[model.Mandate]
	inputs    *entryTable[model.Input]
	ops       *entryTable[model.OperatorCommand]

	// Indexes over committed records, rebuilt on load and extended on
	// apply. Staged records are scanned separately where a transaction
	// needs them.
	threadEvents   map[int][]model.EventID
	reviewComments map[int64]int
	ticketOutbox   map[model.TicketID][]model.OutboxID
	retried        map[model.TicketID]bool

	// work records what the current transaction created for the dispatch
	// and outbox loops; Ingest reports it.
	work workFlags
}

// Store implements every slice interface except model.Control, whose
// Status and Ticket clash with WorkerGateway.Status and EffectStore.Ticket;
// Operator implements it.
var (
	_ model.Intake        = (*Store)(nil)
	_ model.DispatchStore = (*Store)(nil)
	_ model.EffectStore   = (*Store)(nil)
	_ model.WorkerGateway = (*Store)(nil)
)

// settings is the resolved configuration control applies.
type settings struct {
	repo          string
	account       int64
	mode          model.Mode
	maxNormal     int
	run           model.RunBudget
	runMicro      int64
	runTokens     int64
	ticketMicro   int64
	ticketTokens  int64
	repoMicro     int64
	repoTokens    int64
	period        time.Duration
	minRunMicro   int64
	maxAutoWakes  int
	retention     time.Duration
	base          string
	maxBodyBytes  int
	maxEventBytes int
}

// Defaults documented on model.Config.
const (
	defaultWorkers      = 3
	defaultAutoWakes    = 12
	defaultRetention    = 336 * time.Hour
	defaultEventBytes   = 64 << 10
	maxEffectBodyBytes  = 65536
	maxReportTextBytes  = 16 << 10
	maxDispatchAttempts = 3
	maxEffectAttempts   = 5
	transientBackoff    = 30 * time.Second
)

func resolve(config model.Config, policy model.Policy, repo model.RepoMeta) (settings, error) {
	budgets := config.Budgets
	s := settings{
		repo:          config.Repository,
		account:       policy.Account.ID,
		mode:          policy.Mode,
		maxNormal:     config.Workers.Max,
		run:           budgets.Run,
		runMicro:      micro(budgets.Run.MaxCostUSD),
		runTokens:     int64(budgets.Run.MaxTotalTokens),
		ticketMicro:   micro(budgets.TicketUSD),
		ticketTokens:  budgets.TicketTokens,
		repoMicro:     micro(budgets.RepoUSD),
		repoTokens:    budgets.RepoTokens,
		period:        time.Duration(budgets.RepoPeriod),
		minRunMicro:   micro(budgets.MinRunUSD),
		maxAutoWakes:  budgets.MaxAutoWakesPerDay,
		retention:     time.Duration(config.Poll.Retention),
		base:          config.Workers.BaseBranch,
		maxEventBytes: config.Limits.MaxEventBodyBytes,
	}
	if s.repo == "" {
		s.repo = repo.Repo
	}
	if s.account == 0 {
		s.account = repo.Account.ID
	}
	if s.mode == "" {
		s.mode = config.Admission.Mode
	}
	if s.maxNormal == 0 {
		s.maxNormal = defaultWorkers
	}
	if s.minRunMicro == 0 {
		s.minRunMicro = s.runMicro / 10
	}
	if s.maxAutoWakes == 0 {
		s.maxAutoWakes = defaultAutoWakes
	}
	if s.retention == 0 {
		s.retention = defaultRetention
	}
	if s.base == "" {
		s.base = repo.DefaultBranch
	}
	if s.maxEventBytes == 0 {
		s.maxEventBytes = defaultEventBytes
	}
	switch {
	case s.repo == "":
		return settings{}, errors.New("control: repository is required")
	case s.account <= 0:
		return settings{}, errors.New("control: account id is required")
	case s.maxNormal < 1:
		return settings{}, errors.New("control: workers.max must be positive")
	case s.runMicro <= 0:
		return settings{}, errors.New("control: budgets.run.maxCostUsd must be positive")
	case s.ticketMicro <= 0:
		return settings{}, errors.New("control: budgets.ticketUsd must be positive")
	case s.repoMicro <= 0:
		return settings{}, errors.New("control: budgets.repoUsd must be positive")
	case s.period <= 0:
		return settings{}, errors.New("control: budgets.repoPeriod must be positive")
	case s.minRunMicro <= 0:
		return settings{}, errors.New("control: budgets.minRunUsd must be positive")
	}
	return s, nil
}

// micro converts dollars to integer micro-USD.
func micro(usd float64) int64 {
	if math.IsNaN(usd) || math.IsInf(usd, 0) {
		return 0
	}
	return int64(math.Round(usd * 1e6))
}

// PolicyDigest is the digest stored as a Policy's identity: the admission
// rules without their load time.
func PolicyDigest(policy model.Policy) (string, error) {
	policy.Digest = ""
	policy.LoadedAt = time.Time{}
	return durable.Digest(policy)
}

// Open loads the projection from the journal, records the repository
// identity on first open (and checks it on later ones), and records the
// policy under its digest.
func Open(ctx context.Context, options Options) (*Store, error) {
	if options.Store == nil {
		return nil, errors.New("control: store is required")
	}
	if options.Clock == nil {
		return nil, errors.New("control: clock is required")
	}
	policy := options.Policy
	if policy.Mode == "" {
		policy.Mode = options.Config.Admission.Mode
	}
	if policy.Actors == nil {
		policy.Actors = options.Config.Admission.Actors
	}
	if policy.Bots == nil {
		policy.Bots = options.Config.Admission.Bots
	}
	if policy.Account.ID == 0 {
		policy.Account = model.ActorID{Login: options.Config.Account.Login, ID: options.Config.Account.ID}
	}
	if policy.Digest == "" {
		digest, err := PolicyDigest(policy)
		if err != nil {
			return nil, fmt.Errorf("control: policy digest: %w", err)
		}
		policy.Digest = digest
	}
	resolved, err := resolve(options.Config, policy, options.Repo)
	if err != nil {
		return nil, err
	}
	s := &Store{store: options.Store, clock: options.Clock, policy: policy, repo: options.Repo, settings: resolved}
	if err := s.load(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if raw, ok := s.meta.get(metaRepo); ok {
		var stored model.RepoMeta
		if err := json.Unmarshal(*raw, &stored); err != nil {
			return nil, fmt.Errorf("control: decode repository meta: %w", err)
		}
		if !strings.EqualFold(stored.Repo, options.Repo.Repo) || stored.RepoID != options.Repo.RepoID || stored.Account.ID != options.Repo.Account.ID {
			return nil, fmt.Errorf("control: journal belongs to %s (id %d, account %d), not %s (id %d, account %d)", stored.Repo, stored.RepoID, stored.Account.ID, options.Repo.Repo, options.Repo.RepoID, options.Repo.Account.ID)
		}
		s.repo = stored
	} else {
		raw, err := json.Marshal(options.Repo)
		if err != nil {
			return nil, err
		}
		s.meta.put(metaRepo, raw)
	}
	if !s.policies.has(policy.Digest) {
		s.policies.add(policy.Digest, policy)
	}
	if _, err := s.commit(ctx, "open:"+policy.Digest+fmt.Sprintf("@%d", s.seq)); err != nil {
		return nil, err
	}
	return s, nil
}

// Close closes the control journal.
func (s *Store) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store.Close(ctx)
}

const (
	metaRepo  = "repo"
	metaPause = "pause"
)

func (s *Store) reset() {
	s.meta = newDocTable[json.RawMessage](model.NamespaceMeta)
	s.cursors = newDocTable[model.Cursor](model.NamespaceCursor)
	s.threads = newDocTable[model.Thread](model.NamespaceThread)
	s.tickets = newDocTable[model.Ticket](model.NamespaceTicket)
	s.assignments = newDocTable[model.Assignment](model.NamespaceAssignment)
	s.dispatches = newDocTable[model.DispatchIntent](model.NamespaceDispatch)
	s.sessions = newDocTable[model.SessionBinding](model.NamespaceSession)
	s.prs = newDocTable[model.PullRequest](model.NamespacePR)
	s.outbox = newDocTable[model.OutboxIntent](model.NamespaceOutbox)
	s.ledgers = newDocTable[model.Ledger](model.NamespaceLedger)
	s.policies = newEntryTable[model.Policy](model.NamespacePolicy, true)
	s.events = newEntryTable[model.Event](model.NamespaceEvent, false)
	s.decisions = newEntryTable[model.Decision](model.NamespaceDecision, true)
	s.mandates = newEntryTable[model.Mandate](model.NamespaceMandate, true)
	s.inputs = newEntryTable[model.Input](model.NamespaceInput, true)
	s.ops = newEntryTable[model.OperatorCommand](model.NamespaceOperator, true)
	s.threadEvents = make(map[int][]model.EventID)
	s.reviewComments = make(map[int64]int)
	s.ticketOutbox = make(map[model.TicketID][]model.OutboxID)
	s.retried = make(map[model.TicketID]bool)
}

// tables lists every table in the fixed order changes are written.
func (s *Store) tables() []table {
	return []table{s.meta, s.policies, s.cursors, s.threads, s.events, s.decisions, s.tickets, s.mandates, s.inputs, s.assignments, s.dispatches, s.sessions, s.prs, s.outbox, s.ledgers, s.ops}
}

// load rebuilds the projection from the journal's current records.
func (s *Store) load(ctx context.Context) error {
	s.reset()
	loaders := map[string]func(durable.Record) error{
		model.NamespaceMeta:       s.meta.load,
		model.NamespaceCursor:     s.cursors.load,
		model.NamespaceThread:     s.threads.load,
		model.NamespaceTicket:     s.tickets.load,
		model.NamespaceAssignment: s.assignments.load,
		model.NamespaceDispatch:   s.dispatches.load,
		model.NamespaceSession:    s.sessions.load,
		model.NamespacePR:         s.prs.load,
		model.NamespaceOutbox:     s.outbox.load,
		model.NamespaceLedger:     s.ledgers.load,
		model.NamespacePolicy:     s.policies.load,
		model.NamespaceEvent: func(record durable.Record) error {
			var event model.Event
			if err := json.Unmarshal(record.Value, &event); err != nil {
				return fmt.Errorf("decode event %s: %w", record.ID, err)
			}
			s.indexEvent(event)
			return s.events.load(record)
		},
		model.NamespaceDecision: s.decisions.load,
		model.NamespaceMandate:  s.mandates.load,
		model.NamespaceInput:    s.inputs.load,
		model.NamespaceOperator: func(record durable.Record) error {
			var op model.OperatorCommand
			if err := json.Unmarshal(record.Value, &op); err != nil {
				return fmt.Errorf("decode operator command %s: %w", record.ID, err)
			}
			s.indexOp(op)
			return s.ops.load(record)
		},
	}
	var seq uint64
	for _, kind := range []durable.Kind{durable.DocumentKind, durable.EntryKind} {
		cursor := ""
		for {
			page, err := s.store.Read(ctx, durable.Query{SessionID: model.ControlSession, Kind: kind, Cursor: cursor, Limit: 1000})
			if err != nil {
				return fmt.Errorf("control: read journal: %w", err)
			}
			if page.Seq > seq {
				seq = page.Seq
			}
			for _, record := range page.Records {
				loader, ok := loaders[record.Namespace]
				if !ok {
					continue
				}
				if err := loader(record); err != nil {
					return fmt.Errorf("control: %w", err)
				}
			}
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
	}
	for _, id := range s.outbox.ids() {
		intent, _ := s.outbox.get(id)
		s.ticketOutbox[intent.Ticket] = append(s.ticketOutbox[intent.Ticket], intent.ID)
	}
	for ticket := range s.ticketOutbox {
		s.sortOutbox(ticket)
	}
	s.seq = seq
	return nil
}

func (s *Store) indexEvent(event model.Event) {
	s.threadEvents[event.Thread.Number] = append(s.threadEvents[event.Thread.Number], event.ID)
	if event.CommentID != 0 && (event.Kind == model.EventReviewComment || event.Kind == model.EventReviewCommentEdited) {
		s.reviewComments[event.CommentID] = event.Thread.Number
	}
}

func (s *Store) indexOp(op model.OperatorCommand) {
	if op.Method == methodRetry {
		s.retried[model.TicketID(op.Target)] = true
	}
}

func (s *Store) sortOutbox(ticket model.TicketID) {
	ids := s.ticketOutbox[ticket]
	seqOf := func(id model.OutboxID) uint64 {
		intent, _ := s.outbox.get(string(id))
		return intent.Seq
	}
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && seqOf(ids[j]) < seqOf(ids[j-1]); j-- {
			ids[j], ids[j-1] = ids[j-1], ids[j]
		}
	}
}

// check reports a projection that can no longer be trusted.
func (s *Store) check() error {
	if s.broken != nil {
		return fmt.Errorf("control: projection unavailable after failed reload: %w", s.broken)
	}
	return nil
}

func (s *Store) rollback() {
	for _, t := range s.tables() {
		t.rollback()
	}
}

// commit writes everything staged as one durable commit. With nothing
// staged it commits nothing. On success the projection adopts the committed
// bytes; on failure staged changes are dropped and, unless the store
// rejected the batch before writing, the projection is rebuilt from the
// journal because the commit may have become durable.
func (s *Store) commit(ctx context.Context, id string) (durable.Receipt, error) {
	tables := s.tables()
	perTable := make([][]staged, len(tables))
	var commit durable.Commit
	commit.ID = id
	for i, t := range tables {
		changes, err := t.stage()
		if err != nil {
			s.rollback()
			return durable.Receipt{}, fmt.Errorf("control: %w", err)
		}
		perTable[i] = changes
		for _, c := range changes {
			commit.Changes = append(commit.Changes, change(c))
			if c.kind == durable.DocumentKind {
				commit.Expected = append(commit.Expected, durable.RevisionCheck{SessionID: model.ControlSession, Kind: c.kind, Namespace: c.namespace, ID: c.id, Revision: c.revision})
			}
		}
	}
	if len(commit.Changes) == 0 {
		s.rollback()
		return durable.Receipt{Seq: s.seq}, nil
	}
	receipt, err := s.store.Commit(ctx, commit)
	if err != nil {
		s.rollback()
		if !errors.Is(err, durable.ErrInvalidCommit) {
			if reloadErr := s.load(context.WithoutCancel(ctx)); reloadErr != nil {
				s.broken = reloadErr
			}
		}
		return durable.Receipt{}, fmt.Errorf("control: commit %s: %w", id, err)
	}
	for i, t := range tables {
		if err := t.apply(perTable[i]); err != nil {
			s.broken = err
			return durable.Receipt{}, fmt.Errorf("control: %w", err)
		}
		for _, c := range perTable[i] {
			switch c.namespace {
			case model.NamespaceEvent:
				var event model.Event
				if err := json.Unmarshal(c.value, &event); err == nil {
					s.indexEvent(event)
				}
			case model.NamespaceOperator:
				var op model.OperatorCommand
				if err := json.Unmarshal(c.value, &op); err == nil {
					s.indexOp(op)
				}
			case model.NamespaceOutbox:
				if c.revision == 0 {
					var intent model.OutboxIntent
					if err := json.Unmarshal(c.value, &intent); err == nil {
						s.ticketOutbox[intent.Ticket] = append(s.ticketOutbox[intent.Ticket], intent.ID)
						s.sortOutbox(intent.Ticket)
					}
				}
			}
		}
	}
	s.seq = receipt.Seq
	return receipt, nil
}

// applied reports whether a commit ID already has a receipt, which makes a
// repeated call with the same idempotency key return without deciding again.
func (s *Store) applied(ctx context.Context, id string) (bool, error) {
	page, err := s.store.Read(ctx, durable.Query{CommitID: id})
	if err != nil {
		return false, fmt.Errorf("control: read receipt %s: %w", id, err)
	}
	return len(page.Receipts) > 0, nil
}

// readEvent reads one event body from the journal.
func (s *Store) readEvent(ctx context.Context, id model.EventID) (model.Event, error) {
	if event, ok := s.events.dirty[string(id)]; ok {
		return event, nil
	}
	page, err := s.store.Read(ctx, durable.Query{SessionID: model.ControlSession, Kind: durable.EntryKind, Namespace: model.NamespaceEvent, ID: string(id), Limit: 1})
	if err != nil {
		return model.Event{}, fmt.Errorf("control: read event %s: %w", id, err)
	}
	if len(page.Records) == 0 {
		return model.Event{}, fmt.Errorf("control: event %s: %w", id, model.ErrNotFound)
	}
	var event model.Event
	if err := json.Unmarshal(page.Records[0].Value, &event); err != nil {
		return model.Event{}, fmt.Errorf("control: decode event %s: %w", id, err)
	}
	return event, nil
}

// suffix makes a commit ID unique for transitions that may legitimately
// repeat on the same subject (a requeued dispatch admitted again).
func (s *Store) suffix() string { return fmt.Sprintf("@%d", s.seq) }

func (s *Store) pause() model.Pause {
	raw, ok := s.meta.get(metaPause)
	if !ok {
		return model.Pause{}
	}
	var pause model.Pause
	if json.Unmarshal(*raw, &pause) != nil {
		return model.Pause{}
	}
	return pause
}
