package journal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	types "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/tempest/durable"
)

func stateDefinition() durable.DocumentDefinition {
	return durable.DocumentDefinition{Namespace: "app", Version: 1, Schema: json.RawMessage(`{"type":"integer"}`), Initial: json.RawMessage(`0`), History: durable.HistoryRewindable, Fork: durable.ForkAsOf, Reset: durable.ResetPreserve}
}

func TestV4AtomicStateAndTranscript(t *testing.T) {
	for _, boundary := range []string{"before_write", "torn_batch", "after_fsync_before_ack"} {
		t.Run(boundary, func(t *testing.T) {
			s, err := Create(t.Context(), t.TempDir(), t.TempDir(), Options{})
			if err != nil {
				t.Fatal(err)
			}
			if err = s.RegisterDocuments(t.Context(), []durable.DocumentDefinition{stateDefinition()}); err != nil {
				t.Fatal(err)
			}
			raw, err := MarshalEntry(MessageEntry{Meta: Meta{ID: "entry-one", Seq: 2, Lane: LaneMain, Timestamp: time.Unix(1, 0)}, Message: types.UserMessage{Content: types.StringContent("atomic input")}})
			if err != nil {
				t.Fatal(err)
			}
			changes := []durable.Change{{SessionID: s.ID(), Kind: durable.EntryKind, Namespace: "_tempest/journal", ID: "entry-one", Version: 1, Value: raw}, {SessionID: s.ID(), Kind: durable.DocumentKind, Namespace: "app", ID: "default", Version: 1, Value: json.RawMessage(`1`)}}
			for _, kind := range []durable.Kind{durable.MemoKind, durable.TaskKind, durable.SubmissionKind} {
				changes = append(changes, durable.Change{SessionID: s.ID(), Kind: kind, Namespace: "_tempest/control", ID: "one", Version: 1, Value: json.RawMessage(`{"state":"accepted"}`)})
			}
			commit := durable.Commit{ID: "atomic", Expected: []durable.RevisionCheck{{SessionID: s.ID(), Kind: durable.DocumentKind, Namespace: "app", ID: "default", Revision: 1}}, Changes: changes}
			if boundary == "before_write" {
				if err = s.file.Close(); err != nil {
					t.Fatal(err)
				}
				if _, err = s.Commit(t.Context(), commit); !errors.Is(err, os.ErrClosed) {
					t.Fatalf("write boundary was not exercised: %v", err)
				}
			}
			if boundary == "after_fsync_before_ack" {
				if _, err = s.Commit(t.Context(), commit); err != nil {
					t.Fatal(err)
				}
			}
			path := s.Path()
			if err = s.Close(); err != nil && boundary != "before_write" {
				t.Fatal(err)
			}
			if boundary == "torn_batch" {
				f, e := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
				if e != nil {
					t.Fatal(e)
				}
				f.WriteString(`{"type":"commit","version":4,"commit":{"changes":[`)
				f.Close()
			}
			for range 2 {
				s, loaded, err := OpenForResume(t.Context(), path, Options{})
				if err != nil {
					t.Fatal(err)
				}
				page, e := s.Read(t.Context(), durable.Query{Namespace: "_tempest/control"})
				if e != nil {
					t.Fatal(e)
				}
				want := 0
				messages := 0
				value := "0"
				if boundary == "after_fsync_before_ack" {
					want = 3
					messages = 1
					value = "1"
				}
				if len(page.Records) != want || len(loaded.Messages) != messages {
					t.Fatalf("partial adoption: control=%d messages=%d", len(page.Records), len(loaded.Messages))
				}
				doc, e := s.Read(t.Context(), durable.Query{Kind: durable.DocumentKind, Namespace: "app", ID: "default"})
				if e != nil || string(doc.Records[0].Value) != value {
					t.Fatalf("document adoption: %+v %v", doc, e)
				}
				if e = s.Close(); e != nil {
					t.Fatal(e)
				}
			}
		})
	}
}

func TestV4CASAndSchemaWriteNothing(t *testing.T) {
	s, err := Create(t.Context(), t.TempDir(), t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.RegisterDocuments(t.Context(), []durable.DocumentDefinition{stateDefinition()}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		revision uint64
		value    string
	}{{"CAS", 9, `1`}, {"schema", 1, `"not an integer"`}} {
		t.Run(test.name, func(t *testing.T) {
			_, err := s.Commit(t.Context(), durable.Commit{ID: test.name, Expected: []durable.RevisionCheck{{SessionID: s.ID(), Kind: durable.DocumentKind, Namespace: "app", ID: "default", Revision: test.revision}}, Changes: []durable.Change{{SessionID: s.ID(), Kind: durable.DocumentKind, Namespace: "app", ID: "default", Version: 1, Value: json.RawMessage(test.value)}, {SessionID: s.ID(), Kind: durable.EntryKind, Namespace: "app", ID: "must-not-append", Version: 1, Value: json.RawMessage(`true`)}}})
			if err == nil {
				t.Fatal("invalid batch accepted")
			}
			after, e := os.ReadFile(s.Path())
			if e != nil {
				t.Fatal(e)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("invalid batch wrote bytes")
			}
		})
	}
}

func TestV4MigrationCrashRecoveryAndStableLock(t *testing.T) {
	for _, version := range []int{1, 2, 3} {
		for _, phase := range []string{"marker", "prepared", "renamed"} {
			t.Run(fmt.Sprintf("v%d/%s", version, phase), func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "session.jsonl")
				h := Header{Type: "session", Version: version, ID: "0011223344556677", Timestamp: time.Unix(1, 0), Cwd: dir}
				header, _ := json.Marshal(h)
				raw, err := MarshalEntry(MessageEntry{Meta: Meta{ID: "old-entry", Seq: 1, Lane: LaneMain, Timestamp: time.Unix(1, 0)}, Message: types.UserMessage{Content: types.StringContent("old input")}})
				if err != nil {
					t.Fatal(err)
				}
				original := append(append(append([]byte(nil), header...), '\n'), append(raw, '\n')...)
				if err = os.WriteFile(path, original, 0600); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path+".migrate", []byte("v4\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if phase == "prepared" {
					if err = os.WriteFile(path+".v4", []byte(`{"type":"session","version":4`), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if phase == "renamed" {
					s, _, e := OpenForResume(t.Context(), path, Options{})
					if e != nil {
						t.Fatal(e)
					}
					s.Close()
					if e = os.WriteFile(path+".migrate", []byte("v4\n"), 0600); e != nil {
						t.Fatal(e)
					}
				}
				for range 2 {
					s, loaded, e := OpenForResume(t.Context(), path, Options{})
					if e != nil {
						t.Fatal(e)
					}
					if loaded.Header.Version != 4 || len(loaded.Messages) != 1 {
						t.Fatalf("bad migration: %+v", loaded)
					}
					if other, _, e := OpenForResume(t.Context(), path, Options{}); !errors.Is(e, ErrSessionLocked) {
						if other != nil {
							other.Close()
						}
						t.Fatalf("replacement lost stable writer lock: %v", e)
					}
					legacy, legacyErr := os.OpenFile(path, os.O_RDWR, 0600)
					if legacyErr != nil {
						t.Fatal(legacyErr)
					}
					legacyErr = lockJournal(legacy)
					if closeErr := legacy.Close(); closeErr != nil {
						t.Fatal(closeErr)
					}
					if !errors.Is(legacyErr, ErrSessionLocked) {
						t.Fatalf("old inode-lock-only writer admitted during migration: %v", legacyErr)
					}
					if e = s.Close(); e != nil {
						t.Fatal(e)
					}
				}
				if _, e := os.Stat(path + ".migrate"); !os.IsNotExist(e) {
					t.Fatalf("migration marker not recovered: %v", e)
				}
				after, e := os.ReadFile(path)
				if e != nil {
					t.Fatal(e)
				}
				lines := bytes.Split(after, []byte{'\n'})
				var migrated Header
				if e = json.Unmarshal(lines[0], &migrated); e != nil || migrated.Version != 4 {
					t.Fatalf("not v4: %s %v", lines[0], e)
				}
				var envelope durable.Envelope
				if e = json.Unmarshal(lines[1], &envelope); e != nil || envelope.Type != "commit" {
					t.Fatalf("unwrapped old entry: %s %v", lines[1], e)
				}
			})
		}
	}
}

func TestMemoPresenceAndFirstWriteWins(t *testing.T) {
	s, err := Create(t.Context(), t.TempDir(), t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	scope := durable.MemoScope{SessionID: s.ID(), InvocationID: "call", ExtensionID: "approval", HookID: "before-tool"}
	for _, value := range []string{`false`, `null`} {
		scope.InvocationID = value
		if _, ok, e := s.ReadMemo(t.Context(), scope, "decision"); e != nil || ok {
			t.Fatalf("missing: %v %v", ok, e)
		}
		winner, e := s.Memo(t.Context(), scope, "decision", json.RawMessage(value))
		if e != nil || string(winner) != value {
			t.Fatalf("insert: %s %v", winner, e)
		}
		winner, e = s.Memo(t.Context(), scope, "decision", json.RawMessage(`true`))
		if e != nil || string(winner) != value {
			t.Fatalf("winner: %s %v", winner, e)
		}
	}
	path := s.Path()
	s.Close()
	for range 2 {
		s, _, err = OpenForResume(context.Background(), path, Options{})
		if err != nil {
			t.Fatal(err)
		}
		scope.InvocationID = "false"
		v, ok, e := s.ReadMemo(t.Context(), scope, "decision")
		if e != nil || !ok || string(v) != "false" {
			t.Fatalf("reopened false: %s %v %v", v, ok, e)
		}
		s.Close()
	}
}

func TestPureRecoveryReusesExtensionResult(t *testing.T) {
	s, err := Create(t.Context(), t.TempDir(), t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	appendAll := func(entry Entry) {
		t.Helper()
		if err := s.Append(t.Context(), entry); err != nil {
			t.Fatal(err)
		}
	}
	appendAll(RunStartedRecord{Meta: Meta{RunID: "run"}, Model: "test"})
	appendAll(MessageEntry{Meta: Meta{RunID: "run"}, Message: types.AssistantMessage{Content: []types.ContentBlock{types.NewToolCall("provider-id", "ops__read", json.RawMessage(`{}`))}}})
	appendAll(ToolStartedRecord{Meta: Meta{RunID: "run"}, ToolCallID: "provider-id", CallID: "occurrence", Tool: "ops__read", Args: json.RawMessage(`{}`), Replay: durable.ReplaySafe})
	_, err = s.Commit(t.Context(), durable.Commit{ID: "result", Changes: []durable.Change{{SessionID: s.ID(), Kind: durable.EntryKind, Namespace: "_host.extensions", ID: "occurrence:result", Version: 1, Value: json.RawMessage(`{"kind":"tool_result","callId":"occurrence","result":{"content":[{"type":"text","text":"recorded result"}]}}`)}}})
	if err != nil {
		t.Fatal(err)
	}
	path := s.Path()
	s.Close()
	for range 2 {
		loaded, e := Load(t.Context(), path)
		if e != nil {
			t.Fatal(e)
		}
		if len(loaded.Interrupted) != 0 || len(loaded.Messages) != 2 {
			t.Fatalf("result not adopted in original pair: %+v", loaded)
		}
		result, ok := loaded.Messages[1].(types.ToolResultMessage)
		if !ok || len(result.Content) != 1 || result.Content[0].Text != "recorded result" || result.IsError {
			t.Fatalf("wrong recovered result: %+v", loaded.Messages[1])
		}
	}
}

func TestLegacyQueuesMigrateIntoUnifiedReceipts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.jsonl")
	header := Header{Type: "session", Version: 3, ID: "0011223344556677", Timestamp: time.Unix(1, 0), Cwd: dir}
	raw, _ := json.Marshal(header)
	var file bytes.Buffer
	file.Write(raw)
	file.WriteByte('\n')
	entries := []Entry{
		WorkRecord{Type: KindQueueEnqueued, Payload: json.RawMessage(`{"queue_id":"queued","kind":"steer","text":"legacy input","key":"old-key"}`)},
		RunStartedRecord{Meta: Meta{RunID: "run"}, Model: "faux"},
		MessageEntry{Meta: Meta{ID: "message", RunID: "run"}, Message: types.UserMessage{Content: types.StringContent("legacy input")}, QueueID: "queued"},
		RunFinishedRecord{Meta: Meta{RunID: "run"}, StopReason: "aborted"},
	}
	for i, entry := range entries {
		meta := entry.meta()
		meta.Timestamp = header.Timestamp
		meta.Seq = uint64(i + 1)
		meta.Lane = LaneMain
		raw, err := MarshalEntry(entry.withMeta(meta))
		if err != nil {
			t.Fatal(err)
		}
		file.Write(raw)
		file.WriteByte('\n')
	}
	if err := os.WriteFile(path, file.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		store, loaded, err := OpenForResume(t.Context(), path, Options{})
		if err != nil {
			t.Fatal(err)
		}
		page, err := store.Read(t.Context(), durable.Query{Kind: durable.SubmissionKind, Namespace: "_tempest/submissions", ID: "old-key", Limit: 1})
		if err != nil || len(page.Records) != 1 {
			t.Fatalf("missing migrated receipt: %+v %v", page, err)
		}
		var receipt durable.Submission
		if err = json.Unmarshal(page.Records[0].Value, &receipt); err != nil {
			t.Fatal(err)
		}
		var result struct{ StopReason string }
		if err = json.Unmarshal(receipt.Result, &result); err != nil {
			t.Fatal(err)
		}
		if receipt.ID != "queued" || receipt.State != "completed" || receipt.RunID != "run" || result.StopReason != "aborted" || len(loaded.Queued) != 0 {
			t.Fatalf("migrated receipt changed: %+v", receipt)
		}
		store.Close()
	}
}
