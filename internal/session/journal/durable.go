package journal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go.harness.dev/harness/internal/outputschema"
	"io"
	"os"
	"path/filepath"
	"strings"

	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/tempest/durable"
)

func (s *Store) initDurable() {
	s.durable = durable.NewLog(func(e durable.Envelope) error {
		raw, err := durable.EncodeEnvelope(s.path, e)
		if err != nil {
			return err
		}
		if err = writeLine(s.file, raw); err != nil {
			s.failed = err
			return err
		}
		if err = s.file.Sync(); err != nil {
			s.failed = err
			return err
		}
		return nil
	}, nil)
}
func (s *Store) appendEnvelope(ctx context.Context, raw []byte, meta Meta, kind EntryType) error {
	if s.durable == nil {
		s.initDurable()
	}
	id := meta.ID
	if id == "" {
		id = fmt.Sprintf("record:%d", meta.Seq)
	}
	changes := []durable.Change{{SessionID: s.id, Kind: durable.EntryKind, ID: id, Namespace: "_tempest/journal", Version: 1, Value: raw}}
	extra, err := s.submissionChanges(ctx, raw, meta)
	if err != nil {
		return err
	}
	changes = append(changes, extra...)
	decision := fmt.Sprintf("journal:%d", meta.Seq)
	if s.external {
		decision = s.id + ":" + decision
	}
	expected := make([]durable.RevisionCheck, 0, len(extra))
	if kind == KindQueueEnqueued {
		for _, ch := range extra {
			expected = append(expected, durable.RevisionCheck{SessionID: ch.SessionID, Kind: ch.Kind, Namespace: ch.Namespace, ID: ch.ID})
		}
	}
	receipt, err := s.durable.Commit(ctx, durable.Commit{ID: decision, Expected: expected, Changes: changes})
	if err == nil {
		s.seq = receipt.Seq
		if kind == KindRunStarted || kind == KindRunFinished {
			s.updateCatalogRun(raw)
		}
	}
	return err
}

// Read returns a bounded committed page without running hooks or jobs.
func (s *Store) Read(ctx context.Context, q durable.Query) (durable.Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.file == nil && !s.external {
		return durable.Page{}, durable.ErrClosed
	}
	if s.durable == nil {
		s.initDurable()
	}
	if q.SessionID == "" {
		q.SessionID = s.id
	}
	if q.SessionID != s.id {
		return durable.Page{}, fmt.Errorf("foreign session read")
	}
	if s.external && q.CommitID != "" {
		q.CommitID = s.id + ":" + q.CommitID
	}
	page, err := s.durable.Read(ctx, q)
	if err != nil {
		return durable.Page{}, err
	}
	if q.CommitID == "" && (q.Kind == "" || q.Kind == durable.DocumentKind) && (q.AsOf != 0 || q.History || q.Cursor != "") {
		definitions := durable.Query{SessionID: s.id, Kind: durable.DocumentKind, Namespace: "_tempest/definitions", Limit: 100}
		for {
			policies, err := s.durable.Read(ctx, definitions)
			if err != nil {
				return durable.Page{}, err
			}
			if q.AsOf == 0 && !q.History && page.Seq == policies.Seq {
				break
			}
			for _, record := range policies.Records {
				var definition durable.DocumentDefinition
				if err := json.Unmarshal(record.Value, &definition); err != nil {
					return durable.Page{}, err
				}
				if definition.History == durable.HistoryLatest && (q.Namespace == "" || q.Namespace == definition.Namespace) {
					return durable.Page{}, fmt.Errorf("namespace %q rejects historical reads", definition.Namespace)
				}
			}
			if policies.NextCursor == "" {
				break
			}
			definitions.Cursor = policies.NextCursor
		}
	}
	return page, nil
}

// Commit atomically writes application state and host control records.
// Public adapters must validate their namespace grants before calling it.
func (s *Store) Commit(ctx context.Context, c durable.Commit) (durable.Receipt, error) {
	s.mu.Lock()
	committed := false
	defer func() {
		observer := s.onCommit
		s.mu.Unlock()
		if committed && observer != nil {
			observer()
		}
	}()
	if s.closed || s.file == nil && !s.external {
		return durable.Receipt{}, durable.ErrClosed
	}
	if s.failed != nil {
		return durable.Receipt{}, s.failed
	}
	if s.durable == nil {
		s.initDurable()
	}
	for _, ch := range c.Changes {
		if ch.SessionID != s.id {
			return durable.Receipt{}, fmt.Errorf("foreign session commit")
		}
	}
	for _, check := range c.Expected {
		if check.SessionID != s.id {
			return durable.Receipt{}, fmt.Errorf("foreign session comparison")
		}
	}
	lastID := ""
	for _, change := range c.Changes {
		if change.Kind == durable.EntryKind && change.Namespace == "_tempest/journal" {
			entry, err := decodeEntry(change.Value)
			if err != nil {
				return durable.Receipt{}, err
			}
			if id := entry.meta().ID; id != "" {
				lastID = id
			}
		}
	}
	for _, ch := range c.Changes {
		if ch.Kind == durable.DocumentKind && !strings.HasPrefix(ch.Namespace, "_") {
			schema := s.schemas[ch.Namespace]
			if schema == nil {
				return durable.Receipt{}, fmt.Errorf("unregistered document namespace %q", ch.Namespace)
			}
			if !ch.Delete {
				if _, err := schema.Validate(string(ch.Value)); err != nil {
					return durable.Receipt{}, err
				}
			}
		}
	}
	originalID := c.ID
	if s.external {
		c.ID = s.id + ":" + c.ID
	}
	r, err := s.durable.Commit(ctx, c)
	if err == nil {
		committed = r.Seq > s.seq
		if r.Seq > s.seq && lastID != "" {
			s.lastID = lastID
		}
		s.seq = max(s.seq, r.Seq)
		r.ID = originalID
		if committed {
			s.updateCatalogSubmissions(c)
		}
	}
	return r, err
}

func expandV4(ctx context.Context, path string, contents []byte) ([]byte, *durable.Log, error) {
	lines := bytes.Split(contents, []byte{'\n'})
	var h Header
	if err := json.Unmarshal(lines[0], &h); err != nil {
		return nil, nil, err
	}
	h.Version = 3
	header, _ := json.Marshal(h)
	var out bytes.Buffer
	out.Write(header)
	out.WriteByte('\n')
	log := durable.NewMemory()
	for i := 1; i < len(lines); i++ {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if len(lines[i]) == 0 && i == len(lines)-1 {
			continue
		}
		// An unterminated final envelope is never an acknowledged commit.
		if i == len(lines)-1 {
			break
		}
		e, decodeErr := durable.DecodeEnvelope(path, lines[i])
		if err := decodeErr; err != nil {
			return nil, nil, fmt.Errorf("%w: line %d: %v", durable.ErrCorrupt, i+1, err)
		}
		if err := log.Restore(e); err != nil {
			return nil, nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		for _, ch := range e.Commit.Changes {
			if ch.Kind == durable.EntryKind && ch.Namespace == "_tempest/legacy_unknown" {
				var unknown unknownEntryError
				if _, err := decodeEntry(ch.Value); !errors.As(err, &unknown) {
					return nil, nil, fmt.Errorf("%w: invalid legacy optional record", durable.ErrCorrupt)
				}
				out.Write(ch.Value)
				out.WriteByte('\n')
			}
			if ch.Kind == durable.EntryKind && ch.Namespace == "_tempest/journal" {
				if _, err := decodeEntry(ch.Value); err != nil {
					return nil, nil, err
				}
				out.Write(ch.Value)
				out.WriteByte('\n')
			}
		}
	}
	return out.Bytes(), log, nil
}

func acquireStableLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = lockJournal(f); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// migrateV4 keeps the stable sidecar and old inode locked across replacement.
// The synced marker identifies the prepared replacement. Recovery can restart
// preparation from the unchanged old file, or adopt the already-renamed v4 file.
func migrateV4(ctx context.Context, file *os.File, h Header) (*os.File, error) {
	if h.Version == 4 {
		marker := file.Name() + ".migrate"
		if _, err := os.Stat(marker); err == nil {
			if err = os.Remove(file.Name() + ".v4"); err != nil && !os.IsNotExist(err) {
				return nil, err
			}
			if err = os.Remove(marker); err != nil {
				return nil, err
			}
			if err = syncDirectory(filepath.Dir(file.Name())); err != nil {
				return nil, err
			}
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		return file, nil
	}
	contents, err := os.ReadFile(file.Name())
	if err != nil {
		return nil, err
	}
	lines := bytes.Split(contents, []byte{'\n'})
	h.Version = 4
	header, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.Write(header)
	out.WriteByte('\n')
	log := durable.NewLog(func(e durable.Envelope) error {
		raw, err := durable.EncodeEnvelope(file.Name(), e)
		if err != nil {
			return err
		}
		out.Write(raw)
		out.WriteByte('\n')
		return nil
	}, nil)
	offset := int64(len(lines[0]) + 1)
	for i := 1; i < len(lines); i++ {
		legacyOffset := offset
		offset += int64(len(lines[i]) + 1)
		if len(lines[i]) == 0 {
			continue
		}
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		entry, e := decodeEntry(lines[i])
		if e != nil {
			var unknown unknownEntryError
			if errors.As(e, &unknown) {
				_, commitErr := log.Commit(ctx, durable.Commit{ID: fmt.Sprintf("journal:%d", i), Changes: []durable.Change{{SessionID: h.ID, Kind: durable.EntryKind, Namespace: "_tempest/legacy_unknown", ID: fmt.Sprintf("record:%d", i), Version: 1, Value: lines[i]}}})
				if commitErr != nil {
					return nil, commitErr
				}
				continue
			}
			return nil, e
		}
		meta := entry.meta()
		meta.Seq = uint64(i)
		raw, e := MarshalEntry(entry.withMeta(meta))
		if e != nil {
			return nil, e
		}
		// Legacy transcript IDs weren't required to be unique or bounded.
		// Keep them in the value and give each storage record its own key.
		id := fmt.Sprintf("legacy-entry:%d", i)
		mapping, _ := json.Marshal(struct {
			Seq uint64 `json:"seq"`
		}{uint64(i)})
		changes := []durable.Change{{SessionID: h.ID, Kind: durable.EntryKind, Namespace: "_tempest/journal", ID: id, Version: 1, Value: raw}, {SessionID: h.ID, Kind: durable.EntryKind, Namespace: "_tempest/legacy_offsets", ID: fmt.Sprint(legacyOffset), Version: 1, Value: mapping}}
		receipts, receiptErr := (&Store{id: h.ID, durable: log}).submissionChanges(ctx, raw, meta)
		if receiptErr != nil {
			return nil, receiptErr
		}
		changes = append(changes, receipts...)
		_, e = log.Commit(ctx, durable.Commit{ID: fmt.Sprintf("journal:%d", i), Changes: changes})
		if e != nil {
			return nil, e
		}
	}
	marker := file.Name() + ".migrate"
	prepared := file.Name() + ".v4"
	m, err := os.OpenFile(marker, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	_, err = m.WriteString("v4\n")
	err = errors.Join(err, m.Sync(), m.Close())
	if err != nil {
		return nil, err
	}
	if err = syncDirectory(filepath.Dir(file.Name())); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(prepared, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	cleanup := func(e error) (*os.File, error) { f.Close(); return nil, e }
	if err = lockJournal(f); err != nil {
		return cleanup(err)
	}
	if _, err = io.Copy(f, bytes.NewReader(out.Bytes())); err != nil {
		return cleanup(err)
	}
	if err = f.Sync(); err != nil {
		return cleanup(err)
	}
	if err = os.Rename(prepared, file.Name()); err != nil {
		return cleanup(err)
	}
	if err = syncDirectory(filepath.Dir(file.Name())); err != nil {
		return cleanup(err)
	}
	if err = os.Remove(marker); err != nil {
		return cleanup(err)
	}
	if err = syncDirectory(filepath.Dir(file.Name())); err != nil {
		return cleanup(err)
	}
	file.Close()
	return f, nil
}

func (s *Store) restoreDurable(ctx context.Context) error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	lines := bytes.Split(raw, []byte{'\n'})
	log := s.durable.(*durable.Log)
	for i := 1; i < len(lines); i++ {
		if len(lines[i]) == 0 {
			continue
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		e, decodeErr := durable.DecodeEnvelope(s.path, lines[i])
		if err = decodeErr; err != nil {
			return err
		}
		if err = log.Restore(e); err != nil {
			return err
		}
	}
	return nil
}

// ReadMemo distinguishes a missing decision from explicit false or null.
func (s *Store) ReadMemo(ctx context.Context, scope durable.MemoScope, key string) (json.RawMessage, bool, error) {
	if scope.SessionID == "" {
		scope.SessionID = s.id
	}
	p, err := s.Read(ctx, durable.Query{SessionID: scope.SessionID, Kind: durable.MemoKind, Namespace: scope.ExtensionID, ID: durable.MemoID(scope, key), Limit: 1})
	if err != nil {
		return nil, false, err
	}
	if len(p.Records) == 0 {
		return nil, false, nil
	}
	return p.Records[0].Value, true, nil
}

// Memo inserts a first-write-wins value under a host-established scope.
func (s *Store) Memo(ctx context.Context, scope durable.MemoScope, key string, candidate json.RawMessage) (json.RawMessage, error) {
	value, _, err := s.MemoInserted(ctx, scope, key, candidate)
	return value, err
}

// MemoInserted reports whether this call won the first-write comparison.
func (s *Store) MemoInserted(ctx context.Context, scope durable.MemoScope, key string, candidate json.RawMessage) (json.RawMessage, bool, error) {
	if scope.SessionID == "" {
		scope.SessionID = s.id
	}
	if scope.SessionID != s.id || scope.InvocationID == "" || scope.ExtensionID == "" || key == "" {
		return nil, false, fmt.Errorf("invalid memo scope")
	}
	if v, ok, err := s.ReadMemo(ctx, scope, key); err != nil || ok {
		return v, false, err
	}
	id := durable.MemoID(scope, key)
	_, err := s.Commit(ctx, durable.Commit{ID: "memo:" + id + ":" + s.NewRunID(), Expected: []durable.RevisionCheck{{SessionID: s.id, Kind: durable.MemoKind, Namespace: scope.ExtensionID, ID: id}}, Changes: []durable.Change{{SessionID: s.id, Kind: durable.MemoKind, Namespace: scope.ExtensionID, ID: id, Version: 1, Value: candidate}}})
	inserted := err == nil
	if err != nil && !errors.Is(err, durable.ErrConflict) {
		return nil, false, err
	}
	v, present, err := s.ReadMemo(ctx, scope, key)
	if err == nil && !present {
		err = fmt.Errorf("%w: memo winner missing", durable.ErrCorrupt)
	}
	return v, inserted, err
}

func (s *Store) submissionChanges(ctx context.Context, raw []byte, meta Meta) ([]durable.Change, error) {
	entry, err := decodeEntry(raw)
	if err != nil {
		return nil, err
	}
	var queued *QueueEnqueued
	var cancelID, consumeID, refuseID string
	var finished *RunFinishedRecord
	var started *RunStartedRecord
	var answer string
	isAnswer := false
	switch e := entry.(type) {
	case WorkRecord:
		switch e.Type {
		case KindQueueEnqueued:
			var q QueueEnqueued
			if err = json.Unmarshal(e.Payload, &q); err != nil {
				return nil, err
			}
			queued = &q
		case KindQueueCancelled:
			var q QueueCancelled
			if err = json.Unmarshal(e.Payload, &q); err != nil {
				return nil, err
			}
			cancelID = q.QueueID
		}
	case MessageEntry:
		consumeID = e.QueueID
		for _, b := range assistantBlocks(e.Message) {
			if b.Type == "text" {
				answer += b.Text
				isAnswer = true
			}
		}
	case HookRecord:
		refuseID = e.QueueID
	case RunStartedRecord:
		started = &e
	case RunFinishedRecord:
		finished = &e
	}
	makeChange := func(v durable.Submission) durable.Change {
		value, _ := json.Marshal(v)
		return durable.Change{SessionID: s.id, Kind: durable.SubmissionKind, Namespace: "_tempest/submissions", ID: durable.SubmissionKey(v.ClientNamespace, v.RequestID), Version: 1, Value: value}
	}
	if queued != nil {
		id := queued.Key
		if id == "" {
			id = queued.QueueID
		}
		if queued.RequestID != "" {
			id = queued.RequestID
		}
		payload, _ := json.Marshal(struct {
			Text            string
			Media           json.RawMessage
			OutputSchema    json.RawMessage `json:",omitempty"`
			ContentIdentity json.RawMessage
		}{Text: queued.Text})
		if len(queued.Prompt) > 0 {
			payload = queued.Prompt
		}
		hash, err := durable.SubmissionHash(string(queued.Kind), payload)
		if err != nil {
			return nil, err
		}
		return []durable.Change{makeChange(durable.Submission{ID: queued.QueueID, RequestID: id, ClientNamespace: queued.ClientNamespace, State: "accepted", RequestHash: hash, Placement: string(queued.Kind), Payload: payload, QueueID: queued.QueueID})}, nil
	}
	if cancelID == "" && consumeID == "" && refuseID == "" && started == nil && finished == nil && !isAnswer {
		return nil, nil
	}
	var changes []durable.Change
	q := durable.Query{SessionID: s.id, Kind: durable.SubmissionKind, Namespace: "_tempest/submissions", Limit: 100, Active: true}
	for {
		p, err := s.durable.Read(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, r := range p.Records {
			var v durable.Submission
			if err = json.Unmarshal(r.Value, &v); err != nil {
				return nil, err
			}
			changed := false
			switch {
			case started != nil && (v.State == "running" || v.State == "consumed") && (v.LogicalRunID == "" || v.LogicalRunID == started.LogicalRunID || v.RunID == started.ContinuationOf):
				v.RunID, v.LogicalRunID = started.RunID, started.LogicalRunID
				changed = true
			case cancelID != "" && v.QueueID == cancelID:
				v.State = "withdrawn"
				changed = true
			case refuseID != "" && v.QueueID == refuseID:
				v.State = "blocked"
				v.Reason = "prompt hook refused queued input"
				changed = true
			case consumeID != "" && v.QueueID == consumeID:
				v.State = "consumed"
				v.RunID = meta.RunID
				changed = true
			case isAnswer && v.RunID == meta.RunID && (v.State == "running" || v.State == "consumed"):
				v.Answer = answer
				v.AnswerEntryID = meta.ID
				changed = true
			case finished != nil && v.RunID == meta.RunID && (v.State == "running" || v.State == "consumed"):
				v.State = "completed"
				v.Reason = finished.StopReason
				v.Error = finished.Error
				v.Result, _ = json.Marshal(struct {
					RunID      string
					StopReason string
					Turns      int
					Usage      ptypes.Usage
					ElapsedMS  int64
				}{meta.RunID, finished.StopReason, finished.Turns, finished.UsageTotal, finished.ElapsedMS})
				if v.Error != "" {
					v.State = "failed"
				}
				changed = true
			}
			if changed {
				v.Revision = r.Revision + 1
				changes = append(changes, makeChange(v))
			}
		}
		if p.NextCursor == "" {
			break
		}
		q.Cursor = p.NextCursor
	}
	return changes, nil
}

// RegisterDocuments installs schemas before callbacks receive state. Definitions
// and their initial values commit together; reopening never reapplies defaults.
func (s *Store) RegisterDocuments(ctx context.Context, defs []durable.DocumentDefinition) error {
	for _, d := range defs {
		if d.Namespace == "" || strings.HasPrefix(d.Namespace, "_") || d.Version != 1 {
			return fmt.Errorf("invalid document definition")
		}
		if d.History == "" {
			d.History = durable.HistoryRewindable
		}
		if d.Fork == "" {
			d.Fork = durable.ForkAsOf
		}
		if d.Reset == "" {
			d.Reset = durable.ResetPreserve
		}
		if d.History != durable.HistoryRewindable && d.History != durable.HistoryLatest || d.Fork != durable.ForkAsOf && d.Fork != durable.ForkCurrent && d.Fork != durable.ForkInitial || d.Reset != durable.ResetPreserve && d.Reset != durable.ResetInitial || d.Fork == durable.ForkAsOf && d.History != durable.HistoryRewindable {
			return fmt.Errorf("invalid document policy")
		}
		schema, err := outputschema.Compile(d.Schema)
		if err != nil {
			return err
		}
		if schema == nil {
			return fmt.Errorf("document schema required")
		}
		if _, err = schema.Validate(string(d.Initial)); err != nil {
			return err
		}
		raw, _ := json.Marshal(d)
		s.mu.Lock()
		if s.closed || s.file == nil && !s.external {
			s.mu.Unlock()
			return durable.ErrClosed
		}
		if s.durable == nil {
			s.initDurable()
		}
		prior, readErr := s.durable.Read(ctx, durable.Query{SessionID: s.id, Kind: durable.DocumentKind, Namespace: "_tempest/definitions", ID: d.Namespace, Limit: 1})
		if readErr != nil {
			s.mu.Unlock()
			return readErr
		}
		if len(prior.Records) > 0 {
			old, _ := durable.CanonicalJSON(prior.Records[0].Value)
			next, _ := durable.CanonicalJSON(raw)
			if !bytes.Equal(old, next) {
				s.mu.Unlock()
				return fmt.Errorf("%w: document definition %q requires an explicit migration", durable.ErrUnsupported, d.Namespace)
			}
			if s.schemas == nil {
				s.schemas = make(map[string]*outputschema.Schema)
			}
			s.schemas[d.Namespace] = schema
			s.mu.Unlock()
			continue
		}
		decision := "define:" + d.Namespace
		if s.external {
			decision = s.id + ":" + decision
		}
		receipt, commitErr := s.durable.Commit(ctx, durable.Commit{ID: decision, Changes: []durable.Change{{SessionID: s.id, Kind: durable.DocumentKind, Namespace: "_tempest/definitions", ID: d.Namespace, Version: 1, Value: raw}, {SessionID: s.id, Kind: durable.DocumentKind, Namespace: d.Namespace, ID: "default", Version: 1, Value: d.Initial}}})
		if commitErr == nil {
			if s.schemas == nil {
				s.schemas = make(map[string]*outputschema.Schema)
			}
			s.schemas[d.Namespace] = schema
			s.seq = max(s.seq, receipt.Seq)
		}
		observer := s.onCommit
		s.mu.Unlock()
		if commitErr == nil && observer != nil {
			observer()
		}
		if commitErr != nil {
			return commitErr
		}
	}
	return nil
}

type recordedToolResult struct {
	Content []ptypes.ContentBlock `json:"content"`
	IsError bool                  `json:"isError"`
}

func readExtensionResults(ctx context.Context, store durable.Store, sessionID string, cut uint64) (map[string]recordedToolResult, error) {
	results := make(map[string]recordedToolResult)
	q := durable.Query{SessionID: sessionID, Kind: durable.EntryKind, Namespace: "_host.extensions", Limit: 100, AsOf: cut}
	for {
		page, err := store.Read(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, record := range page.Records {
			var fact struct {
				Kind   string              `json:"kind"`
				CallID string              `json:"callId"`
				Result *recordedToolResult `json:"result"`
			}
			if err = json.Unmarshal(record.Value, &fact); err != nil {
				return nil, err
			}
			if fact.Kind == "tool_result" && fact.CallID != "" && fact.Result != nil {
				results[fact.CallID] = *fact.Result
			}
		}
		if page.NextCursor == "" {
			break
		}
		q.Cursor = page.NextCursor
	}
	return results, nil
}

// ObserveCommit installs a post-adoption observer. We release the writer lock
// before calling it, so a snapshot read can't deadlock a callback commit.
func (s *Store) ObserveCommit(observer func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onCommit = observer
}
