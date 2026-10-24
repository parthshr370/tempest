package durable

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func batch(id string) Commit {
	var changes []Change
	for _, kind := range []Kind{EntryKind, DocumentKind, MemoKind, TaskKind, SubmissionKind, ChildKind} {
		changes = append(changes, Change{SessionID: "s", Kind: kind, Namespace: "app", ID: "one", Version: 1, Value: json.RawMessage(`{"value":1}`)})
	}
	return Commit{ID: id, Changes: changes}
}

func TestStoreConformance(t *testing.T) {
	for _, name := range []string{"memory", "file"} {
		t.Run(name, func(t *testing.T) {
			var store *Log
			var err error
			path := filepath.Join(t.TempDir(), "store.jsonl")
			if name == "memory" {
				store = NewMemory()
			} else {
				store, err = OpenFile(t.Context(), path)
				if err != nil {
					t.Fatal(err)
				}
			}
			defer func() { _ = store.Close(context.Background()) }()
			release, err := store.AcquireSession(t.Context(), "session-owner")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.AcquireSession(t.Context(), "session-owner"); !errors.Is(err, ErrLocked) {
				t.Fatalf("duplicate session writer: %v", err)
			}
			other, err := store.AcquireSession(t.Context(), "unrelated-session")
			if err != nil {
				t.Fatal(err)
			}
			other()
			release()
			reopened, err := store.AcquireSession(t.Context(), "session-owner")
			if err != nil {
				t.Fatal(err)
			}
			reopened()
			first := batch("decision")
			receipt, err := store.Commit(t.Context(), first)
			if err != nil {
				t.Fatal(err)
			}
			duplicate := batch("decision")
			duplicate.Changes[0].Value = json.RawMessage(`{"value":1.00e0}`)
			same, err := store.Commit(t.Context(), duplicate)
			if err != nil || same != receipt {
				t.Fatalf("canonical duplicate: %+v %v", same, err)
			}
			duplicate.Changes[0].Value = json.RawMessage(`{"value":2}`)
			if _, err = store.Commit(t.Context(), duplicate); !errors.Is(err, ErrConflict) {
				t.Fatalf("changed decision: %v", err)
			}
			page, err := store.Read(t.Context(), Query{SessionID: "s", Limit: 2})
			if err != nil || len(page.Records) != 2 || page.NextCursor == "" {
				t.Fatalf("first page: %+v %v", page, err)
			}
			conflict := Commit{ID: "conflict", Expected: []RevisionCheck{{SessionID: "s", Kind: DocumentKind, Namespace: "app", ID: "one", Revision: 9}}, Changes: []Change{{SessionID: "s", Kind: EntryKind, Namespace: "app", ID: "not-adopted", Version: 1, Value: json.RawMessage(`true`)}}}
			if _, err = store.Commit(t.Context(), conflict); !errors.Is(err, ErrConflict) {
				t.Fatalf("CAS: %v", err)
			}
			absent, _ := store.Read(t.Context(), Query{ID: "not-adopted"})
			if len(absent.Records) != 0 {
				t.Fatal("CAS partially appended entry")
			}
			update := Commit{ID: "update", Expected: []RevisionCheck{{SessionID: "s", Kind: DocumentKind, Namespace: "app", ID: "one", Revision: 1}}, Changes: []Change{{SessionID: "s", Kind: DocumentKind, Namespace: "app", ID: "one", Version: 1, Value: json.RawMessage(`{"value":2}`)}}}
			if _, err = store.Commit(t.Context(), update); err != nil {
				t.Fatal(err)
			}
			second, err := store.Read(t.Context(), Query{SessionID: "s", Limit: 2, Cursor: page.NextCursor})
			if err != nil || second.Seq != receipt.Seq {
				t.Fatalf("cursor cut moved: %+v %v", second, err)
			}
			if _, err = store.Read(t.Context(), Query{SessionID: "foreign", Limit: 2, Cursor: page.NextCursor}); err == nil {
				t.Fatal("cursor escaped query")
			}
			atCut, err := store.Read(t.Context(), Query{Kind: DocumentKind, AsOf: receipt.Seq})
			if err != nil || string(atCut.Records[0].Value) != `{"value":1}` {
				t.Fatalf("snapshot read: %+v %v", atCut, err)
			}
			lookup, err := store.Read(t.Context(), Query{CommitID: "decision"})
			if err != nil || len(lookup.Receipts) != 1 || lookup.Receipts[0] != receipt {
				t.Fatalf("lookup: %+v %v", lookup, err)
			}
			if name == "file" {
				if contender, e := OpenFile(t.Context(), path); !errors.Is(e, ErrLocked) {
					if contender != nil {
						contender.Close(t.Context())
					}
					t.Fatalf("concurrent owner: %v", e)
				}
				for range 2 {
					if err = store.Close(t.Context()); err != nil {
						t.Fatal(err)
					}
					store, err = OpenFile(t.Context(), path)
					if err != nil {
						t.Fatal(err)
					}
					recovered, e := store.Read(t.Context(), Query{Kind: DocumentKind})
					if e != nil || len(recovered.Records) != 1 || string(recovered.Records[0].Value) != `{"value":2}` {
						t.Fatalf("reopen: %+v %v", recovered, e)
					}
				}
			}
			if err = store.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err = store.Commit(t.Context(), batch("stale")); !errors.Is(err, ErrClosed) {
				t.Fatalf("stale owner: %v", err)
			}
		})
	}
}

func TestAtomicCrashBoundaries(t *testing.T) {
	for _, boundary := range []string{"before_write", "torn_batch", "after_fsync_before_ack"} {
		t.Run(boundary, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.jsonl")
			store, err := OpenFile(t.Context(), path)
			if err != nil {
				t.Fatal(err)
			}
			if boundary == "after_fsync_before_ack" {
				if _, err = store.Commit(t.Context(), batch("atomic")); err != nil {
					t.Fatal(err)
				}
			}
			if err = store.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			if boundary == "torn_batch" {
				f, e := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
				if e != nil {
					t.Fatal(e)
				}
				if _, e = f.WriteString(`{"type":"commit","version":4,"commit":{"changes":[`); e != nil {
					t.Fatal(e)
				}
				if e = f.Close(); e != nil {
					t.Fatal(e)
				}
			}
			for range 2 {
				store, err = OpenFile(t.Context(), path)
				if err != nil {
					t.Fatal(err)
				}
				page, e := store.Read(t.Context(), Query{SessionID: "s"})
				if e != nil {
					t.Fatal(e)
				}
				want := 0
				if boundary == "after_fsync_before_ack" {
					want = 6
				}
				if len(page.Records) != want {
					t.Fatalf("partial atomic adoption: %d records, want %d", len(page.Records), want)
				}
				if err = store.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestMiddleCorruptionFailsLoudly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.jsonl")
	s, err := OpenFile(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Commit(t.Context(), batch("one")); err != nil {
		t.Fatal(err)
	}
	s.Close(t.Context())
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	file.WriteString("not-json\n")
	file.WriteString("{}\n")
	file.Close()
	if s, err = OpenFile(t.Context(), path); !errors.Is(err, ErrCorrupt) {
		if s != nil {
			s.Close(t.Context())
		}
		t.Fatalf("middle corruption: %v", err)
	}
}

func TestNoAcknowledgmentBeforeDurability(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	s := NewLog(func(Envelope) error { close(entered); <-release; return nil }, nil)
	go func() { _, err := s.Commit(context.Background(), batch("wait")); done <- err }()
	<-entered
	select {
	case err := <-done:
		t.Fatalf("acknowledged before durability: %v", err)
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestFailureFencesWriter(t *testing.T) {
	failure := errors.New("sync failed")
	writes := 0
	s := NewLog(func(Envelope) error { writes++; return failure }, nil)
	if _, err := s.Commit(t.Context(), batch("one")); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if _, err := s.Commit(t.Context(), batch("two")); !errors.Is(err, failure) || writes != 1 {
		t.Fatalf("failed writer reused: writes=%d err=%v", writes, err)
	}
	p, _ := s.Read(t.Context(), Query{})
	if len(p.Records) != 0 {
		t.Fatal("failed commit adopted")
	}
}

func TestConcurrentDecisionDeduplication(t *testing.T) {
	s := NewMemory()
	var wg sync.WaitGroup
	receipts := make(chan Receipt, 32)
	failures := make(chan error, 32)
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.Commit(context.Background(), batch("one"))
			receipts <- r
			failures <- err
		}()
	}
	wg.Wait()
	close(receipts)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first Receipt
	for r := range receipts {
		if first.ID == "" {
			first = r
		}
		if first != r {
			t.Fatalf("different receipts: %+v %+v", first, r)
		}
	}
	p, _ := s.Read(t.Context(), Query{})
	if p.Seq != 1 || len(p.Records) != 6 {
		t.Fatalf("duplicate admission: %+v", p)
	}
}

func TestCanonicalNumbers(t *testing.T) {
	for _, pair := range [][2]string{{`1`, `1.000`}, {`0`, `-0e999999999`}, {`0.01`, `1e-2`}, {`100000000000000000001`, `100000000000000000001.0`}, {`1e999999999`, `10e999999998`}} {
		a, err := CanonicalJSON([]byte(pair[0]))
		if err != nil {
			t.Fatal(err)
		}
		b, err := CanonicalJSON([]byte(pair[1]))
		if err != nil || string(a) != string(b) {
			t.Fatalf("%s != %s: %s %s %v", pair[0], pair[1], a, b, err)
		}
	}
}
