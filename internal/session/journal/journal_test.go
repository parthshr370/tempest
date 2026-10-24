package journal

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	ptypes "go.harness.dev/harness/internal/engine/types"
)

var testContext = context.Background()

func TestHeaderRoundTrip(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	defer closeStore(t, store)

	contents, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	var header Header
	if err := json.Unmarshal([]byte(strings.SplitN(string(contents), "\n", 2)[0]), &header); err != nil {
		t.Fatalf("decode header: %v", err)
	}
	if header.Type != "session" || header.Version != JournalVersion || header.ID != store.ID() {
		t.Fatalf("header = %#v", header)
	}
	loaded, err := Load(testContext, store.Path())
	if err != nil {
		t.Fatalf("load journal: %v", err)
	}
	if loaded.Header != header {
		t.Fatalf("loaded header = %#v, want %#v", loaded.Header, header)
	}
}

func TestAppendAndReplayLinearChain(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	defer closeStore(t, store)
	first := userMessage("first")
	second := assistantMessage("second")
	if err := store.AppendMessage(testContext, first); err != nil {
		t.Fatalf("append first message: %v", err)
	}
	if err := store.AppendModelChange(testContext, "example/model", "plan"); err != nil {
		t.Fatalf("append model change: %v", err)
	}
	if err := store.AppendMessage(testContext, second); err != nil {
		t.Fatalf("append second message: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}

	loaded, err := Load(testContext, store.Path())
	if err != nil {
		t.Fatalf("load journal: %v", err)
	}
	if loaded.Model != "example/model" || loaded.Role != "plan" {
		t.Fatalf("model state = %q/%q", loaded.Model, loaded.Role)
	}
	if !reflect.DeepEqual(loaded.Messages, []ptypes.Message{first, second}) {
		t.Fatalf("messages = %#v", loaded.Messages)
	}

	lines := journalEntryLines(t, store.Path())
	var previous string
	for index, line := range lines[1:] {
		var record struct {
			ID       string `json:"id"`
			ParentID string `json:"parent_id"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode entry %d: %v", index, err)
		}
		if record.ParentID != previous {
			t.Fatalf("entry %d parent = %q, want %q", index, record.ParentID, previous)
		}
		previous = record.ID
	}
}

func TestCompactionReconstruction(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	defer closeStore(t, store)
	if err := store.AppendMessage(testContext, userMessage("old request")); err != nil {
		t.Fatalf("append first message: %v", err)
	}
	if err := store.AppendMessage(testContext, assistantMessage("old response")); err != nil {
		t.Fatalf("append second message: %v", err)
	}
	kept := ptypes.UserMessage{Content: ptypes.StringContent("current request")}
	if err := store.AppendMessage(testContext, kept); err != nil {
		t.Fatalf("append kept message: %v", err)
	}
	if err := store.AppendCompaction(testContext, "recap", "0000000000000004", 123); err != nil {
		t.Fatalf("append compaction: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}

	loaded, err := Load(testContext, store.Path())
	if err != nil {
		t.Fatalf("load journal: %v", err)
	}
	compactionTime := time.Date(2024, time.January, 2, 3, 4, 9, 0, time.UTC).UnixMilli()
	want := []ptypes.Message{ptypes.UserMessage{
		Content: ptypes.BlockContent(
			ptypes.NewText(compactionPrefix+"recap"+compactionSuffix),
			ptypes.NewText("current request"),
		),
		Timestamp: compactionTime,
	}}
	if !reflect.DeepEqual(loaded.Messages, want) {
		t.Fatalf("messages = %#v, want %#v", loaded.Messages, want)
	}
}

func TestTruncatedFinalLineTolerated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "truncated.jsonl")
	header := testHeader()
	entry, err := MarshalEntry(MessageEntry{Meta: Meta{ID: "entry-1", Timestamp: header.Timestamp}, Message: userMessage("kept")})
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	contents, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	if err := os.WriteFile(path, append(append(append(contents, '\n'), entry...), []byte("\n{\"type\":\"message\"")...), 0o600); err != nil {
		t.Fatalf("write journal: %v", err)
	}
	loaded, err := Load(testContext, path)
	if err != nil {
		t.Fatalf("load truncated journal: %v", err)
	}
	if len(loaded.Messages) != 1 || loaded.Messages[0].Role() != "user" {
		t.Fatalf("messages = %#v", loaded.Messages)
	}
	resumed, _, err := OpenForResume(testContext, path, Options{
		Clock: func() time.Time { return header.Timestamp.Add(time.Second) },
		NewID: func() string { return "entry-2" },
	})
	if err != nil {
		t.Fatalf("open truncated journal for resume: %v", err)
	}
	if err := resumed.AppendMessage(testContext, userMessage("after")); err != nil {
		t.Fatalf("append resumed message: %v", err)
	}
	closeStore(t, resumed)
	loaded, err = Load(testContext, path)
	if err != nil {
		t.Fatalf("reload resumed journal: %v", err)
	}
	if len(loaded.Messages) != 2 {
		t.Fatalf("resumed message count = %d, want 2", len(loaded.Messages))
	}
}

func TestMalformedInteriorLineIsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.jsonl")
	header := testHeader()
	entry, err := MarshalEntry(MessageEntry{Meta: Meta{ID: "entry-1", Timestamp: header.Timestamp}, Message: userMessage("after")})
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	contents := string(headerJSON) + "\nnot-json\n" + string(entry) + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write journal: %v", err)
	}
	if _, err := Load(testContext, path); err == nil {
		t.Fatal("Load succeeded for malformed interior line")
	}
}

func TestResolveAndRecent(t *testing.T) {
	dir := t.TempDir()
	first := newTestStore(t, dir)
	firstPath, firstID := first.Path(), first.ID()
	closeStore(t, first)
	second := newTestStoreAfter(t, dir, 1)
	secondPath := second.Path()
	closeStore(t, second)
	newer := time.Date(2030, time.January, 1, 1, 0, 0, 0, time.UTC)
	if err := os.Chtimes(secondPath, newer, newer); err != nil {
		t.Fatalf("set newest mtime: %v", err)
	}
	resolved, err := Resolve(testContext, dir, "/workspace/project", firstID)
	if err != nil {
		t.Fatalf("resolve journal: %v", err)
	}
	if resolved != firstPath {
		t.Fatalf("resolved = %q, want %q", resolved, firstPath)
	}
	recent, err := Recent(testContext, dir, "/workspace/project")
	if err != nil {
		t.Fatalf("recent journal: %v", err)
	}
	if recent != secondPath {
		t.Fatalf("recent = %q, want %q", recent, secondPath)
	}
}

func TestResolveJournalFilenameForms(t *testing.T) {
	dir := t.TempDir()
	store := newTestStore(t, dir)
	path, id := store.Path(), store.ID()
	closeStore(t, store)
	name := filepath.Base(path)
	for _, input := range []string{id, name, strings.TrimSuffix(name, ".jsonl")} {
		t.Run(input, func(t *testing.T) {
			got, err := Resolve(testContext, dir, "/workspace/project", input)
			if err != nil || got != path {
				t.Fatalf("Resolve(%q) = %q, %v; want %q", input, got, err, path)
			}
		})
	}
	if _, err := Resolve(testContext, dir, "/another/project", name); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("cross-workspace filename error = %v", err)
	}
}

func TestResolveRejectsAmbiguousIDWithFilenameHint(t *testing.T) {
	dir := t.TempDir()
	store := newTestStore(t, dir)
	path, id := store.Path(), store.ID()
	closeStore(t, store)
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(filepath.Dir(path), "20240103T030405Z_"+id+".jsonl")
	if err := os.WriteFile(other, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(testContext, dir, "/workspace/project", id); err == nil || !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "-resume "+filepath.Base(path)) || !strings.Contains(err.Error(), "-resume "+filepath.Base(other)) {
		t.Fatalf("ambiguous id error = %v", err)
	}
	for _, want := range []string{path, other} {
		got, err := Resolve(testContext, dir, "/workspace/project", filepath.Base(want))
		if err != nil || got != want {
			t.Fatalf("filename disambiguation = %q, %v; want %q", got, err, want)
		}
	}
}

func TestUnknownEntryTypeSkipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unknown.jsonl")
	headerJSON, err := json.Marshal(testHeader())
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	contents := string(headerJSON) + "\n{\"type\":\"future_kind\",\"id\":\"entry-1\",\"timestamp\":\"2024-01-02T03:04:05Z\"}\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write journal: %v", err)
	}
	loaded, err := Load(testContext, path)
	if err != nil {
		t.Fatalf("load journal: %v", err)
	}
	if len(loaded.Messages) != 0 || len(loaded.Diagnostics) != 1 || loaded.LastID != "entry-1" {
		t.Fatalf("loaded = %#v", loaded)
	}
}

func TestMessageRoundTripAllRoles(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	defer closeStore(t, store)
	messages := []ptypes.Message{
		userMessage("user"),
		assistantMessage("assistant"),
		ptypes.ToolResultMessage{
			ToolCallID: "call-1",
			ToolName:   "read",
			Content:    []ptypes.ContentBlock{ptypes.NewText("tool result")},
			Details:    json.RawMessage(`{"path":"file.txt"}`),
			Timestamp:  33,
		},
	}
	for _, message := range messages {
		if err := store.AppendMessage(testContext, message); err != nil {
			t.Fatalf("append %s message: %v", message.Role(), err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
	loaded, err := Load(testContext, store.Path())
	if err != nil {
		t.Fatalf("load journal: %v", err)
	}
	if !reflect.DeepEqual(loaded.Messages, messages) {
		t.Fatalf("messages = %#v, want %#v", loaded.Messages, messages)
	}
}

func TestHeaderOnlyAndEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "header-only.jsonl")
	headerJSON, err := json.Marshal(testHeader())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(headerJSON, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(testContext, path)
	if err != nil {
		t.Fatalf("load header-only journal: %v", err)
	}
	if len(loaded.Messages) != 0 || loaded.LastID != "" {
		t.Fatalf("header-only loaded = %#v", loaded)
	}
	empty := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(testContext, empty); err == nil {
		t.Fatal("Load succeeded for empty journal")
	}
}

func TestMalformedBaseEntries(t *testing.T) {
	headerJSON, err := json.Marshal(testHeader())
	if err != nil {
		t.Fatal(err)
	}
	valid, err := MarshalEntry(MessageEntry{
		Meta:    Meta{ID: "entry-1", Timestamp: testHeader().Timestamp},
		Message: userMessage("after"),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		record   string
		interior bool
	}{
		{name: "empty object final", record: `{}`},
		{name: "missing id and timestamp final", record: `{"type":"model_change"}`},
		{name: "empty object interior", record: `{}`, interior: true},
		{name: "missing id and timestamp interior", record: `{"type":"model_change"}`, interior: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "malformed.jsonl")
			contents := string(headerJSON) + "\n" + test.record + "\n"
			if test.interior {
				contents += string(valid) + "\n"
			}
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(testContext, path)
			if test.interior && err == nil {
				t.Fatal("Load succeeded for malformed interior record")
			}
			if !test.interior && err != nil {
				t.Fatalf("Load final malformed record: %v", err)
			}
		})
	}
}

func TestCompactionFirstEntryAndMissingFirstKeptID(t *testing.T) {
	header := testHeader()
	headerJSON, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	firstCompaction, err := MarshalEntry(CompactionEntry{
		Meta:    Meta{ID: "compact-1", Timestamp: header.Timestamp.Add(time.Second)},
		Summary: "first recap",
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "first-compaction.jsonl")
	if err := os.WriteFile(path, []byte(string(headerJSON)+"\n"+string(firstCompaction)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(testContext, path)
	if err != nil {
		t.Fatal(err)
	}
	wantFirst := []ptypes.Message{ptypes.UserMessage{
		Content:   ptypes.StringContent(compactionPrefix + "first recap" + compactionSuffix),
		Timestamp: header.Timestamp.Add(time.Second).UnixMilli(),
	}}
	if !reflect.DeepEqual(loaded.Messages, wantFirst) {
		t.Fatalf("first compaction messages = %#v, want %#v", loaded.Messages, wantFirst)
	}

	message, err := MarshalEntry(MessageEntry{
		Meta:    Meta{ID: "message-1", Timestamp: header.Timestamp},
		Message: userMessage("kept because id is absent"),
	})
	if err != nil {
		t.Fatal(err)
	}
	missingKept, err := MarshalEntry(CompactionEntry{
		Meta:        Meta{ID: "compact-2", ParentID: "message-1", Timestamp: header.Timestamp.Add(time.Second)},
		Summary:     "recap",
		FirstKeptID: "absent",
	})
	if err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(t.TempDir(), "missing-first-kept.jsonl")
	if err := os.WriteFile(path, []byte(string(headerJSON)+"\n"+string(message)+"\n"+string(missingKept)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err = Load(testContext, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 1 {
		t.Fatalf("missing FirstKeptID messages = %#v", loaded.Messages)
	}
	merged, ok := loaded.Messages[0].(ptypes.UserMessage)
	if !ok || len(merged.Content.Blocks) != 2 || merged.Content.Blocks[1].Text != "kept because id is absent" {
		t.Fatalf("missing FirstKeptID result = %#v", loaded.Messages)
	}
}

func TestResolveAndRecentRejectCollidingCwd(t *testing.T) {
	dir := t.TempDir()
	cwdA, cwdB := "/a-b/c", "/a/b-c"
	if encodeCwd(cwdA) != encodeCwd(cwdB) {
		t.Fatalf("test cwd values do not collide")
	}
	storeA, err := Create(testContext, dir, cwdA, Options{
		Clock: func() time.Time { return time.Date(2024, time.January, 2, 0, 0, 0, 0, time.UTC) },
		NewID: func() string { return "aaaaaaaaaaaaaaaa" },
	})
	if err != nil {
		t.Fatal(err)
	}
	pathA := storeA.Path()
	closeStore(t, storeA)
	storeB, err := Create(testContext, dir, cwdB, Options{
		Clock: func() time.Time { return time.Date(2024, time.January, 2, 0, 0, 1, 0, time.UTC) },
		NewID: func() string { return "bbbbbbbbbbbbbbbb" },
	})
	if err != nil {
		t.Fatal(err)
	}
	pathB := storeB.Path()
	closeStore(t, storeB)
	newer := time.Date(2030, time.January, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(pathA, newer, newer); err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(testContext, dir, cwdA, "aaaaaaaaaaaaaaaa")
	if err != nil || resolved != pathA {
		t.Fatalf("Resolve collision = %q, %v; want %q", resolved, err, pathA)
	}
	recent, err := Recent(testContext, dir, cwdA)
	if err != nil || recent != pathA {
		t.Fatalf("Recent collision = %q, %v; want %q (not %q)", recent, err, pathA, pathB)
	}
}

func TestRecentEqualMtimeTieBreakAndNotFound(t *testing.T) {
	dir := t.TempDir()
	first := newTestStore(t, dir)
	firstPath := first.Path()
	closeStore(t, first)
	second := newTestStoreAfter(t, dir, 1)
	secondPath := second.Path()
	closeStore(t, second)
	tied := time.Date(2030, time.January, 1, 0, 0, 0, 0, time.UTC)
	for _, path := range []string{firstPath, secondPath} {
		if err := os.Chtimes(path, tied, tied); err != nil {
			t.Fatal(err)
		}
	}
	recent, err := Recent(testContext, dir, "/workspace/project")
	if err != nil {
		t.Fatal(err)
	}
	want := firstPath
	if secondPath > firstPath {
		want = secondPath
	}
	if recent != want {
		t.Fatalf("equal-mtime Recent = %q, want %q", recent, want)
	}
	if _, err := Resolve(testContext, filepath.Join(dir, "missing"), "/workspace/project", "nope"); !errors.Is(err, ErrSessionNotFound) || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing Resolve error = %v", err)
	}
	emptyDir := filepath.Join(dir, "empty")
	if err := os.MkdirAll(filepath.Join(emptyDir, encodeCwd("/workspace/project")), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Recent(testContext, emptyDir, "/workspace/project"); !errors.Is(err, ErrSessionNotFound) || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("empty Recent error = %v", err)
	}
}

func TestParallelAppendsPreserveChain(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	const count = 32
	var group sync.WaitGroup
	errors := make(chan error, count)
	for index := range count {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			errors <- store.AppendMessage(testContext, userMessage(string(rune('a'+index))))
		}(index)
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("parallel append: %v", err)
		}
	}
	closeStore(t, store)
	lines := journalEntryLines(t, store.Path())
	if len(lines) != count+1 {
		t.Fatalf("line count = %d, want %d", len(lines), count+1)
	}
	var previous string
	for index, line := range lines[1:] {
		var entry struct {
			ID       string `json:"id"`
			ParentID string `json:"parent_id"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode entry %d: %v", index, err)
		}
		if entry.ID == "" || entry.ParentID != previous {
			t.Fatalf("entry %d = %#v, previous = %q", index, entry, previous)
		}
		previous = entry.ID
	}
}

func TestCanceledJournalOperations(t *testing.T) {
	ctx, cancel := context.WithCancel(testContext)
	cancel()
	if _, err := Create(ctx, t.TempDir(), "/workspace/project", Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Create cancellation error = %v", err)
	}
	if _, err := Load(ctx, filepath.Join(t.TempDir(), "missing.jsonl")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Load cancellation error = %v", err)
	}
}

func newTestStore(t *testing.T, dir string) *Store {
	return newTestStoreAfter(t, dir, 0)
}

func newTestStoreAfter(t *testing.T, dir string, initialID int) *Store {
	t.Helper()
	id := initialID
	clockCalls := 0
	store, err := Create(testContext, dir, "/workspace/project", Options{
		Clock: func() time.Time {
			value := time.Date(2024, time.January, 2, 3, 4, 5+clockCalls, 0, time.UTC)
			clockCalls++
			return value
		},
		NewID: func() string {
			id++
			return strings.Repeat("0", 15) + string(rune('0'+id))
		},
		Title: "test session",
	})
	if err != nil {
		t.Fatalf("create journal: %v", err)
	}
	return store
}

func closeStore(t *testing.T, store *Store) {
	t.Helper()
	if err := store.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
}

func testHeader() Header {
	return Header{
		Type:      "session",
		Version:   3,
		ID:        "0000000000000001",
		Timestamp: time.Date(2024, time.January, 2, 3, 4, 5, 0, time.UTC),
		Cwd:       "/workspace/project",
	}
}

func userMessage(text string) ptypes.UserMessage {
	return ptypes.UserMessage{Content: ptypes.StringContent(text), Timestamp: 11}
}

func assistantMessage(text string) ptypes.AssistantMessage {
	return ptypes.AssistantMessage{
		Content:   []ptypes.ContentBlock{ptypes.NewText(text)},
		API:       "example",
		Provider:  "example",
		Model:     "example/model",
		Timestamp: 22,
	}
}
