package journal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	types "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/tempest/durable"
)

func TestLargeLegacyValuesMigrateAndAppend(t *testing.T) {
	for _, version := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "session.jsonl")
			header, _ := json.Marshal(Header{Type: "session", Version: version, ID: "0011223344556677", Timestamp: time.Unix(1, 0), Cwd: dir})
			text := strings.Repeat("image-data-", 150000)
			entry := MessageEntry{Meta: Meta{ID: "legacy", Seq: 1, Lane: LaneMain, Timestamp: time.Unix(1, 0)}, Message: types.UserMessage{Content: types.StringContent(text)}}
			raw, err := MarshalEntry(entry)
			if err != nil {
				t.Fatal(err)
			}
			original := append(append(header, '\n'), append(raw, '\n')...)
			if err = os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			for attempt := range 2 {
				store, loaded, err := OpenForResume(t.Context(), path, Options{})
				if err != nil {
					t.Fatal(err)
				}
				if len(loaded.Messages) != attempt+1 {
					t.Fatalf("lost large transcript: %d", len(loaded.Messages))
				}
				if attempt == 0 {
					found, positionErr := store.ContainsAfter(t.Context(), Position{LegacyOffset: int64(len(original))}, entry)
					if positionErr != nil || found {
						t.Fatalf("old EOF treated as new offset: %v %v", found, positionErr)
					}
					found, positionErr = store.ContainsAfter(t.Context(), Position{LegacyOffset: int64(len(header) + 1)}, entry)
					if positionErr != nil || !found {
						t.Fatalf("committed old offset not mapped: %v %v", found, positionErr)
					}
					if err = store.AppendMessage(t.Context(), types.UserMessage{Content: types.StringContent(text)}); err != nil {
						t.Fatal(err)
					}
				}
				if err = store.Close(); err != nil {
					t.Fatal(err)
				}
			}
			contents, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(contents) > durable.MaxBatchBytes || bytes.Contains(contents, []byte(text)) {
				t.Fatal("large value retained inline")
			}
			matches, err := filepath.Glob(path + ".blobs/*")
			if err != nil || len(matches) == 0 {
				t.Fatalf("missing immutable blobs: %v", err)
			}
			if err = os.WriteFile(matches[0], []byte("corrupt"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err = Load(t.Context(), path); !errors.Is(err, durable.ErrCorrupt) {
				t.Fatalf("corrupt blob accepted: %v", err)
			}
		})
	}
}

func TestLegacyCheckpointOffsetWithoutMappingIsUncommitted(t *testing.T) {
	store, err := Create(t.Context(), t.TempDir(), t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	expected := MessageEntry{Message: types.UserMessage{Content: types.StringContent("not appended")}}
	for _, offset := range []int64{1, 47, 100000} {
		found, err := store.ContainsAfter(t.Context(), Position{LegacyOffset: offset}, expected)
		if err != nil || found {
			t.Fatalf("old offset interpreted in v4: %d %v %v", offset, found, err)
		}
	}
}

func TestCatalogSkipsInvalidAndBoundsLegacyInspection(t *testing.T) {
	dir, cwd := t.TempDir(), t.TempDir()
	store, err := Create(t.Context(), dir, cwd, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.MarkInitialized(true); err != nil {
		t.Fatal(err)
	}
	run := Meta{RunID: "run"}
	if err = store.Append(t.Context(), RunStartedRecord{Meta: run, Model: "faux"}); err != nil {
		t.Fatal(err)
	}
	if err = store.Append(t.Context(), RunFinishedRecord{Meta: run, StopReason: "completed"}); err != nil {
		t.Fatal(err)
	}
	path := store.Path()
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{"empty.jsonl": "", "bad.jsonl": "not-json\n"} {
		if err = os.WriteFile(filepath.Join(filepath.Dir(path), name), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := Catalog(t.Context(), dir, cwd)
	if err != nil || len(entries) != 1 || !entries[0].Initialized || !entries[0].HostManaged || entries[0].LastRun == nil || !entries[0].LastRun.Finished {
		t.Fatalf("catalog: %+v %v", entries, err)
	}
}

func TestDocumentDefinitionRejectsChangeWithoutChangingDefaults(t *testing.T) {
	store, err := Create(t.Context(), t.TempDir(), t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	definition := stateDefinition()
	if err = store.RegisterDocuments(t.Context(), []durable.DocumentDefinition{definition}); err != nil {
		t.Fatal(err)
	}
	changed := definition
	changed.Initial = json.RawMessage(`1`)
	if err = store.RegisterDocuments(t.Context(), []durable.DocumentDefinition{changed}); !errors.Is(err, durable.ErrUnsupported) {
		t.Fatalf("incompatible definition: %v", err)
	}
	if err = store.RegisterDocuments(t.Context(), []durable.DocumentDefinition{definition}); err != nil {
		t.Fatalf("old definition no longer usable: %v", err)
	}
	page, err := store.Read(t.Context(), durable.Query{Kind: durable.DocumentKind, Namespace: "app", ID: "default"})
	if err != nil || len(page.Records) != 1 || string(page.Records[0].Value) != "0" {
		t.Fatalf("default changed: %+v %v", page, err)
	}
}

func TestMemoInsertedHasOneWinner(t *testing.T) {
	store, err := Create(t.Context(), t.TempDir(), t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := durable.MemoScope{InvocationID: "call", ExtensionID: "bundle", HookID: "approval"}
	var winners atomic.Int32
	var group sync.WaitGroup
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			value, inserted, err := store.MemoInserted(t.Context(), scope, "key", json.RawMessage(`false`))
			if err != nil || string(value) != "false" {
				t.Errorf("memo: %s %v", value, err)
			}
			if inserted {
				winners.Add(1)
			}
		}()
	}
	group.Wait()
	if winners.Load() != 1 {
		t.Fatalf("insert winners: %d", winners.Load())
	}
}

func TestSharedBackendSessionWriterFence(t *testing.T) {
	backend := durable.NewMemory()
	defer backend.Close(t.Context())
	store, err := CreateBackend(t.Context(), backend, t.TempDir(), t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	id := store.ID()
	if contender, _, err := OpenBackend(t.Context(), backend, t.TempDir(), id, Options{}); !errors.Is(err, ErrSessionLocked) {
		if contender != nil {
			contender.Close()
		}
		t.Fatalf("concurrent writer accepted: %v", err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, _, err := OpenBackend(t.Context(), backend, t.TempDir(), id, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err = store.AppendModelChange(t.Context(), "stale", "default"); !errors.Is(err, durable.ErrClosed) {
		t.Fatalf("stale owner append: %v", err)
	}
}

type backendWithoutSessionLease struct{ durable.Store }

func TestSharedBackendWithoutSessionLeaseFailsVisibly(t *testing.T) {
	backend := durable.NewMemory()
	defer backend.Close(t.Context())
	if _, err := CreateBackend(t.Context(), backendWithoutSessionLease{backend}, t.TempDir(), t.TempDir(), Options{}); !errors.Is(err, durable.ErrUnsupported) {
		t.Fatalf("unfenced shared store accepted: %v", err)
	}
	if _, err := backend.Read(t.Context(), durable.Query{}); err != nil {
		t.Fatalf("failed open closed caller backend: %v", err)
	}
}

func TestMigrationRetainsAcceptedOptionalLegacyRecords(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "optional.jsonl")
	header, _ := json.Marshal(Header{Type: "session", Version: 3, ID: "0011223344556677", Timestamp: time.Unix(1, 0), Cwd: directory})
	optional := []byte(`{"type":"future_optional_record","seq":1,"id":"optional","timestamp":"2024-01-02T03:04:06Z","payload":{"note":"preserve"}}`)
	if err := os.WriteFile(path, append(append(header, '\n'), append(optional, '\n')...), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		store, loaded, err := OpenForResume(t.Context(), path, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if len(loaded.Diagnostics) == 0 {
			t.Fatal("optional legacy record lost its diagnostic")
		}
		page, err := store.Read(t.Context(), durable.Query{Kind: durable.EntryKind, Namespace: "_tempest/legacy_unknown"})
		if err != nil || len(page.Records) != 1 || !bytes.Equal(page.Records[0].Value, optional) {
			t.Fatalf("optional legacy bytes lost: %+v %v", page, err)
		}
		if err = store.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCatalogAcceptsOversizedLegacyEntry(t *testing.T) {
	directory, cwd := t.TempDir(), t.TempDir()
	bucket := filepath.Join(directory, "workspace")
	if err := os.MkdirAll(bucket, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(bucket, "legacy.jsonl")
	header, err := json.Marshal(Header{Type: "session", Version: 3, ID: "0011223344556677", Timestamp: time.Unix(1, 0), Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	record, err := MarshalEntry(MessageEntry{Meta: Meta{ID: "large", Timestamp: time.Unix(1, 0)}, Message: types.UserMessage{Content: types.StringContent(strings.Repeat("x", 2<<20))}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, append(append(header, '\n'), append(record, '\n')...), 0600); err != nil {
		t.Fatal(err)
	}
	entries, err := Catalog(t.Context(), directory, cwd)
	if err != nil || len(entries) != 1 || entries[0].Path != path || !entries[0].Initialized {
		t.Fatalf("oversized legacy entry poisoned catalog: %+v %v", entries, err)
	}
}
