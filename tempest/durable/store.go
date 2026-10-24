package durable

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// MaxValueBytes bounds document, memo and task values. Larger transcript and
// submission values use immutable blobs in the file adapters.
const MaxValueBytes = 512 << 10

// MaxBatchBytes bounds an on-disk atomic envelope, not its referenced blob.
const MaxBatchBytes = 1 << 20

// Envelope is the v4 atomic storage unit. Checksum covers the canonical commit.
type Envelope struct {
	Type    string  `json:"type"`
	Version uint32  `json:"version"`
	Prior   uint64  `json:"prior"`
	Receipt Receipt `json:"receipt"`
	Commit  Commit  `json:"commit"`
	Blob    string  `json:"blob,omitempty"`
}

// Log serializes one transaction domain. Its writer must append and sync the
// entire envelope before returning nil. A write error fences future commits.
type Log struct {
	mu        sync.Mutex
	seq       uint64
	records   []Record
	latest    map[string]Record
	histories map[string][]Record
	kinds     map[Kind][]Record
	active    map[string]Record
	owners    map[string]bool
	receipts  map[string]Receipt
	write     func(Envelope) error
	close     func() error
	failed    error
	closed    bool
}

// NewLog binds an atomic persistence callback. Recovery calls Restore before
// publishing the log. A nil writer explicitly selects nonpersistent memory.
func NewLog(write func(Envelope) error, close func() error) *Log {
	return &Log{latest: make(map[string]Record), histories: make(map[string][]Record), kinds: make(map[Kind][]Record), active: make(map[string]Record), owners: make(map[string]bool), receipts: make(map[string]Receipt), write: write, close: close}
}

// NewMemory creates an isolated store with no crash-durability promise.
func NewMemory() *Log { return NewLog(nil, nil) }

func recordKey(s string, k Kind, n, id string) string {
	return s + "\x00" + string(k) + "\x00" + n + "\x00" + id
}
func validPart(s string) bool { return len(s) <= 1024 && !strings.ContainsRune(s, 0) }

// CanonicalJSON normalizes object order and decimal numbers without float loss.
func CanonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value any
	if err := d.Decode(&value); err != nil {
		return nil, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("expected one JSON value")
	}
	var b bytes.Buffer
	var emit func(any) error
	emit = func(v any) error {
		switch x := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			b.WriteByte('{')
			for i, k := range keys {
				if i > 0 {
					b.WriteByte(',')
				}
				key, _ := json.Marshal(k)
				b.Write(key)
				b.WriteByte(':')
				if err := emit(x[k]); err != nil {
					return err
				}
			}
			b.WriteByte('}')
		case []any:
			b.WriteByte('[')
			for i, e := range x {
				if i > 0 {
					b.WriteByte(',')
				}
				if err := emit(e); err != nil {
					return err
				}
			}
			b.WriteByte(']')
		case json.Number:
			number, err := canonicalNumber(string(x))
			if err != nil {
				return err
			}
			b.WriteString(number)
		default:
			data, err := json.Marshal(v)
			if err != nil {
				return err
			}
			b.Write(data)
		}
		return nil
	}
	if err := emit(value); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Digest hashes canonical JSON. Its normalization bytes aren't a wire encoding.
func Digest(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	raw, err = CanonicalJSON(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func validate(c Commit) error {
	if c.ID == "" || !validPart(c.ID) || len(c.Changes) == 0 || len(c.Changes) > 1024 {
		return fmt.Errorf("invalid commit identity or batch size")
	}
	// Entry and submission payloads may contain legacy inline media. File
	// adapters spill large batches to immutable blobs; documents stay bounded.
	seen := make(map[string]bool)
	for _, ch := range c.Changes {
		switch ch.Kind {
		case EntryKind, DocumentKind, SubmissionKind, TaskKind, ChildKind, MemoKind:
		default:
			return fmt.Errorf("%w: kind %q", ErrUnsupported, ch.Kind)
		}
		if ch.Version != 1 {
			return fmt.Errorf("%w: record version %d", ErrUnsupported, ch.Version)
		}
		if ch.SessionID == "" || ch.ID == "" || !validPart(ch.SessionID) || !validPart(ch.Namespace) || !validPart(ch.ID) {
			return fmt.Errorf("invalid record identity")
		}
		if ch.Delete && ch.Kind != DocumentKind {
			return fmt.Errorf("only documents can be deleted")
		}
		if !ch.Delete && ((ch.Kind != EntryKind && ch.Kind != SubmissionKind && len(ch.Value) > MaxValueBytes) || !json.Valid(ch.Value)) {
			return fmt.Errorf("invalid or oversized record value")
		}
		key := recordKey(ch.SessionID, ch.Kind, ch.Namespace, ch.ID)
		if seen[key] {
			return fmt.Errorf("duplicate record in batch")
		}
		seen[key] = true
	}
	for _, c := range c.Expected {
		if !validPart(c.SessionID) || !validPart(c.Namespace) || !validPart(c.ID) {
			return fmt.Errorf("invalid revision identity")
		}
	}
	return nil
}

// Commit validates every comparison before invoking the durable writer.
func (l *Log) Commit(ctx context.Context, c Commit) (Receipt, error) {
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}
	// Detach caller-owned slices and raw values before retaining them.
	raw, err := json.Marshal(c)
	if err != nil {
		return Receipt{}, err
	}
	var copy Commit
	if err = json.Unmarshal(raw, &copy); err != nil {
		return Receipt{}, err
	}
	c = copy
	if err = validate(c); err != nil {
		return Receipt{}, err
	}
	digest, err := Digest(c)
	if err != nil {
		return Receipt{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return Receipt{}, ErrClosed
	}
	if l.failed != nil {
		return Receipt{}, fmt.Errorf("store fenced after write failure: %w", l.failed)
	}
	if r, ok := l.receipts[c.ID]; ok {
		if r.Digest != digest {
			return Receipt{}, ErrConflict
		}
		return r, nil
	}
	if err = ctx.Err(); err != nil {
		return Receipt{}, err
	}
	var conflicts []RevisionCheck
	for _, check := range c.Expected {
		r := l.latest[recordKey(check.SessionID, check.Kind, check.Namespace, check.ID)]
		if r.Revision != check.Revision {
			check.Revision = r.Revision
			conflicts = append(conflicts, check)
		}
	}
	for _, ch := range c.Changes {
		if old, ok := l.latest[recordKey(ch.SessionID, ch.Kind, ch.Namespace, ch.ID)]; ok && (ch.Kind == MemoKind || ch.Kind == EntryKind || ch.Kind == ChildKind) {
			a, _ := CanonicalJSON(old.Value)
			b, _ := CanonicalJSON(ch.Value)
			if !bytes.Equal(a, b) {
				return Receipt{}, ErrConflict
			}
		}
	}
	if len(conflicts) > 0 {
		return Receipt{}, &ConflictError{Current: conflicts}
	}
	receipt := Receipt{ID: c.ID, Seq: l.seq + 1, Digest: digest}
	envelope := Envelope{Type: "commit", Version: 4, Prior: l.seq, Receipt: receipt, Commit: c}
	if l.write != nil {
		if err = l.write(envelope); err != nil {
			l.failed = err
			return Receipt{}, err
		}
	}
	l.adopt(envelope)
	return receipt, nil
}

func (l *Log) adopt(e Envelope) {
	for _, ch := range e.Commit.Changes {
		key := recordKey(ch.SessionID, ch.Kind, ch.Namespace, ch.ID)
		old := l.latest[key]
		r := Record{Change: ch, Revision: old.Revision + 1, Seq: e.Receipt.Seq, CommitID: e.Commit.ID}
		l.records = append(l.records, r)
		l.latest[key] = r
		l.histories[key] = append(l.histories[key], r)
		l.kinds[ch.Kind] = append(l.kinds[ch.Kind], r)
		if ch.Kind == SubmissionKind {
			var submission Submission
			if json.Unmarshal(ch.Value, &submission) == nil && (submission.State == "accepted" || submission.State == "running" || submission.State == "consumed") {
				l.active[key] = r
			} else {
				delete(l.active, key)
			}
		}
	}
	l.seq = e.Receipt.Seq
	l.receipts[e.Commit.ID] = e.Receipt
}

// Restore validates one complete envelope without calling the persistence hook.
// It never invokes application code or schedules effects.
func (l *Log) Restore(e Envelope) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e.Type != "commit" || e.Version != 4 {
		return fmt.Errorf("%w: envelope version", ErrUnsupported)
	}
	if err := validate(e.Commit); err != nil {
		return err
	}
	digest, err := Digest(e.Commit)
	if err != nil {
		return err
	}
	if e.Prior != l.seq || e.Receipt.Seq != l.seq+1 || e.Receipt.ID != e.Commit.ID || e.Receipt.Digest != digest {
		return fmt.Errorf("%w: commit sequence or checksum", ErrCorrupt)
	}
	if _, ok := l.receipts[e.Commit.ID]; ok {
		return fmt.Errorf("%w: duplicate commit ID", ErrCorrupt)
	}
	for _, c := range e.Commit.Expected {
		if l.latest[recordKey(c.SessionID, c.Kind, c.Namespace, c.ID)].Revision != c.Revision {
			return fmt.Errorf("%w: invalid committed revision", ErrCorrupt)
		}
	}
	l.adopt(e)
	return nil
}

type cursor struct {
	Cut    uint64 `json:"cut"`
	Offset int    `json:"offset"`
	Query  string `json:"query"`
}

// Read returns a query-bound snapshot page. Later commits cannot move its cut.
func (l *Log) Read(ctx context.Context, q Query) (Page, error) {
	if err := ctx.Err(); err != nil {
		return Page{}, err
	}
	if q.Limit < 0 || q.Limit > 1000 {
		return Page{}, fmt.Errorf("limit must be between 1 and 1000")
	}
	if q.Limit == 0 {
		q.Limit = 100
	}
	token := q.Cursor
	q.Cursor = ""
	fingerprint, err := Digest(q)
	if err != nil {
		return Page{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return Page{}, ErrClosed
	}
	cut := q.AsOf
	if cut == 0 {
		cut = l.seq
	}
	offset := 0
	if token != "" {
		raw, e := base64.RawURLEncoding.DecodeString(token)
		var c cursor
		if e != nil || json.Unmarshal(raw, &c) != nil || c.Query != fingerprint || c.Offset < 0 {
			return Page{}, fmt.Errorf("invalid query cursor")
		}
		cut = c.Cut
		offset = c.Offset
	}
	if cut > l.seq {
		return Page{}, fmt.Errorf("cut exceeds committed sequence")
	}
	p := Page{Seq: cut, Records: []Record{}}
	if q.CommitID != "" {
		if r, ok := l.receipts[q.CommitID]; ok && r.Seq <= cut {
			p.Receipts = []Receipt{r}
		}
		return p, nil
	}
	candidates := l.records
	if q.Kind != "" {
		candidates = l.kinds[q.Kind]
	}
	if q.SessionID != "" && q.Kind != "" && q.Namespace != "" && q.ID != "" {
		candidates = l.histories[recordKey(q.SessionID, q.Kind, q.Namespace, q.ID)]
	}
	if q.Active && cut == l.seq {
		candidates = make([]Record, 0, len(l.active))
		for _, record := range l.active {
			candidates = append(candidates, record)
		}
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].Seq < candidates[j].Seq })
	}
	latest := make(map[string]uint64)
	if !q.History && cut != l.seq {
		for _, record := range candidates {
			if record.Seq <= cut {
				latest[recordKey(record.SessionID, record.Kind, record.Namespace, record.ID)] = record.Revision
			}
		}
	}
	matched := 0
	more := false
	for index := range candidates {
		if q.Reverse {
			index = len(candidates) - 1 - index
		}
		r := candidates[index]
		if err = ctx.Err(); err != nil {
			return Page{}, err
		}
		if r.Seq > cut || q.SessionID != "" && q.SessionID != r.SessionID || q.Kind != "" && q.Kind != r.Kind || q.Namespace != "" && q.Namespace != r.Namespace || q.ID != "" && q.ID != r.ID {
			continue
		}
		key := recordKey(r.SessionID, r.Kind, r.Namespace, r.ID)
		revision := latest[key]
		if cut == l.seq {
			revision = l.latest[key].Revision
		}
		if !q.History && (r.Delete || revision != r.Revision) {
			continue
		}
		if q.Application && strings.HasPrefix(r.Namespace, "_") {
			continue
		}
		if q.Active {
			var submission Submission
			if r.Kind != SubmissionKind || json.Unmarshal(r.Value, &submission) != nil || submission.State != "accepted" && submission.State != "running" && submission.State != "consumed" {
				continue
			}
		}
		if matched < offset {
			matched++
			continue
		}
		if len(p.Records) == q.Limit {
			more = true
			break
		}
		r.Value = append(json.RawMessage(nil), r.Value...)
		p.Records = append(p.Records, r)
		matched++
	}
	if more {
		raw, _ := json.Marshal(cursor{Cut: cut, Offset: matched, Query: fingerprint})
		p.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return p, nil
}

// AcquireSession rejects a second writer in the same transaction domain.
func (l *Log) AcquireSession(ctx context.Context, id string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, ErrClosed
	}
	if l.owners[id] {
		return nil, ErrLocked
	}
	l.owners[id] = true
	var once sync.Once
	return func() { once.Do(func() { l.mu.Lock(); delete(l.owners, id); l.mu.Unlock() }) }, nil
}

// Close fences the log and releases its writer once.
func (l *Log) Close(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	if l.close != nil {
		return l.close()
	}
	return nil
}

// MemoID encodes scope without ambiguous concatenation.
func MemoID(scope MemoScope, key string) string {
	b, _ := json.Marshal([]string{scope.InvocationID, scope.ExtensionID, scope.HookID, key})
	sum := sha256.Sum256(b)
	return strconv.QuoteToASCII(hex.EncodeToString(sum[:]))
}

func canonicalNumber(value string) (string, error) {
	negative := strings.HasPrefix(value, "-")
	value = strings.TrimPrefix(value, "-")
	exponent := int64(0)
	if index := strings.IndexAny(value, "eE"); index >= 0 {
		var err error
		exponent, err = strconv.ParseInt(value[index+1:], 10, 64)
		if err != nil || exponent > 1<<60 || exponent < -(1<<60) {
			return "", fmt.Errorf("numeric exponent exceeds bound")
		}
		value = value[:index]
	}
	if index := strings.IndexByte(value, '.'); index >= 0 {
		exponent -= int64(len(value) - index - 1)
		value = value[:index] + value[index+1:]
	}
	value = strings.TrimLeft(value, "0")
	if value == "" {
		return "0", nil
	}
	trimmed := strings.TrimRight(value, "0")
	exponent += int64(len(value) - len(trimmed))
	value = trimmed
	if negative {
		value = "-" + value
	}
	if exponent != 0 {
		value += "e" + strconv.FormatInt(exponent, 10)
	}
	return value, nil
}
