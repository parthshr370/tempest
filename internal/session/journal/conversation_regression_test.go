package journal

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/tempest/durable"
)

func conversationSelection(t *testing.T, store *Store) {
	t.Helper()
	_, err := store.Commit(t.Context(), durable.Commit{ID: "agent-selection", Changes: []durable.Change{{SessionID: store.ID(), Kind: durable.DocumentKind, Namespace: "_tempest/agent", ID: "selection", Version: 1, Value: json.RawMessage(`{"model":"test-model","allowedTools":[],"environment":"local"}`)}}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestForkAfterInterruptedToolRound(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprint(restart), func(t *testing.T) {
			store := newTestStore(t, t.TempDir())
			defer closeStore(t, store)
			conversationSelection(t, store)
			run := Meta{RunID: store.NewRunID()}
			appendAll(t, store, MessageEntry{Meta: run, Message: userMessage("q1")}, MessageEntry{Meta: run, Message: types.AssistantMessage{Content: []types.ContentBlock{types.NewToolCall("crashed", "bash", json.RawMessage(`{}`))}, StopReason: types.StopToolUse}}, ToolStartedRecord{Meta: run, ToolCallID: "crashed", Tool: "bash", Args: json.RawMessage(`{}`)})
			if restart {
				run = Meta{RunID: store.NewRunID()}
				appendAll(t, store, RunStartedRecord{Meta: run})
			}
			answer, err := store.AppendAssigned(t.Context(), MessageEntry{Meta: run, Message: assistantMessage("later complete answer")})
			if err != nil {
				t.Fatal(err)
			}
			for _, preceding := range []bool{false, true} {
				seed, err := store.CaptureFork(t.Context(), answer.ID, preceding)
				if err != nil || seed.Prefix.EntryID != answer.ID {
					t.Fatalf("fork lost later answer: %+v %v", seed, err)
				}
			}
		})
	}
}

func TestForkChainKeepsOneAncestorAndAllRequestFrames(t *testing.T) {
	dir := t.TempDir()
	a := newTestStore(t, dir)
	defer closeStore(t, a)
	conversationSelection(t, a)
	appendAll(t, a, MessageEntry{Message: userMessage("ancestral needle")})
	for i := range 30 {
		if err := a.AppendModelChange(t.Context(), fmt.Sprint(i), "default"); err != nil {
			t.Fatal(err)
		}
	}
	_, err := a.Commit(t.Context(), durable.Commit{ID: "frame", Changes: []durable.Change{{SessionID: a.ID(), Kind: durable.EntryKind, Namespace: "_tempest/requests", ID: "frame", Version: 1, Value: json.RawMessage(`{"promptDigest":"test"}`)}}})
	if err != nil {
		t.Fatal(err)
	}
	answer, err := a.AppendAssigned(t.Context(), MessageEntry{Message: assistantMessage("a answer")})
	if err != nil {
		t.Fatal(err)
	}
	fork := func(parent *Store, entry string) *Store {
		t.Helper()
		seed, err := parent.CaptureFork(t.Context(), entry, false)
		if err != nil {
			t.Fatal(err)
		}
		child, err := Create(t.Context(), dir, "/workspace/project", Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := child.InitializeFork(t.Context(), seed); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { closeStore(t, child) })
		segment, err := child.readArchive(t.Context(), seed.Prefix)
		if err != nil || len(segment.Frames) != 1 {
			t.Fatalf("ancestor frames lost: %d %v", len(segment.Frames), err)
		}
		return child
	}
	b := fork(a, answer.ID)
	appendAll(t, b, MessageEntry{Message: userMessage("b input")})
	bAnswer, err := b.AppendAssigned(t.Context(), MessageEntry{Message: assistantMessage("b answer")})
	if err != nil {
		t.Fatal(err)
	}
	c := fork(b, bAnswer.ID)
	d := fork(c, bAnswer.ID)
	for _, store := range []*Store{a, b, c, d} {
		count, cursor := 0, ""
		for {
			page, err := store.SearchHistory(t.Context(), HistoryOptions{Query: "ancestral needle", Cursor: cursor, Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			count += len(page.Matches)
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
		if count != 1 {
			t.Fatalf("ancestor copied %d times", count)
		}
	}
}

func TestHistorySearchesLiteralVisibleTextAndCentersExcerpt(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	defer closeStore(t, store)
	for _, text := range []string{"if a && b { x -> y }", "<summary>tag</summary>", `say "quoted"`, "line one\nline two"} {
		appendAll(t, store, MessageEntry{Message: userMessage(text)})
	}
	appendAll(t, store, MessageEntry{Message: types.ToolResultMessage{Content: []types.ContentBlock{types.NewText("visible tool output")}, Details: json.RawMessage(`{"secret":"private-detail"}`)}}, MessageEntry{Message: types.AssistantMessage{Content: []types.ContentBlock{types.NewThinking("visible reasoning", "private-signature"), types.NewToolCall("call", "bash", json.RawMessage(`{"command":"a && b"}`))}}})
	for query, want := range map[string]int{"a && b": 2, "x -> y": 1, "<summary>": 1, `"quoted"`: 1, "one\nline": 1, "content": 0, "role": 0, "private-detail": 0, "private-signature": 0} {
		page, err := store.SearchHistory(t.Context(), HistoryOptions{Query: query, Limit: 100})
		if err != nil || len(page.Matches) != want {
			t.Fatalf("query %q: %d matches, want %d; %v", query, len(page.Matches), want, err)
		}
	}
	appendAll(t, store, MessageEntry{Message: userMessage(strings.Repeat("prefix ", 1000) + "literal needle" + strings.Repeat(" tail", 1000))})
	page, err := store.SearchHistory(t.Context(), HistoryOptions{Query: "literal needle", Limit: 100})
	if err != nil || len(page.Matches) != 1 || !page.Matches[0].Clipped || !strings.Contains(page.Matches[0].Excerpt, "literal needle") || len(page.Matches[0].Excerpt) > 512 {
		t.Fatalf("excerpt: %+v %v", page, err)
	}
}

func TestOverflowWithoutLocalInputDoesNotMaskProviderError(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	defer closeStore(t, store)
	claimed, err := store.ClaimOverflow(t.Context())
	if claimed || err != nil {
		t.Fatal(claimed, err)
	}
}

type heldArchiveStore struct {
	*durable.Log
	entered, release chan struct{}
	once             sync.Once
}

func (s *heldArchiveStore) Commit(ctx context.Context, batch durable.Commit) (durable.Receipt, error) {
	for _, change := range batch.Changes {
		if change.SessionID == archiveSession {
			s.once.Do(func() { close(s.entered); <-s.release })
			break
		}
	}
	return s.Log.Commit(ctx, batch)
}

func TestForkArchiveSyncDoesNotBlockParentAndSharesPayloads(t *testing.T) {
	backend := &heldArchiveStore{Log: durable.NewMemory(), entered: make(chan struct{}), release: make(chan struct{})}
	defer backend.Close(context.Background())
	store, err := CreateBackend(t.Context(), backend, t.TempDir(), t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(t, store)
	conversationSelection(t, store)
	appendAll(t, store, MessageEntry{Message: userMessage(strings.Repeat("shared payload ", 1000))})
	answer, err := store.AppendAssigned(t.Context(), MessageEntry{Message: assistantMessage("first answer")})
	if err != nil {
		t.Fatal(err)
	}
	forked := make(chan error, 1)
	go func() { _, err := store.CaptureFork(t.Context(), answer.ID, false); forked <- err }()
	<-backend.entered
	var release sync.Once
	defer release.Do(func() { close(backend.release) })
	appended := make(chan error, 1)
	go func() { appended <- store.AppendMessage(t.Context(), userMessage("parent continues")) }()
	select {
	case err := <-appended:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("archive sync held parent writer")
	}
	release.Do(func() { close(backend.release) })
	if err := <-forked; err != nil {
		t.Fatal(err)
	}
	first, err := store.CaptureFork(t.Context(), answer.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	later, err := store.AppendAssigned(t.Context(), MessageEntry{Message: assistantMessage("second answer")})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CaptureFork(t.Context(), later.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	manifests := make([]archiveSegment, 0, 2)
	for _, ref := range []ArchiveRef{first.Prefix, second.Prefix} {
		page, err := backend.Read(t.Context(), durable.Query{SessionID: archiveSession, Kind: durable.EntryKind, Namespace: archiveNamespace, ID: ref.Digest, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		manifest, err := decodeArchive(page.Records[0].Value, ref)
		if err != nil {
			t.Fatal(err)
		}
		manifests = append(manifests, manifest)
		if len(manifest.Records) != 0 || len(manifest.Messages) != 0 {
			t.Fatal("payload duplicated in manifest")
		}
	}
	if manifests[0].Objects["records"][0] != manifests[1].Objects["records"][0] || manifests[0].Objects["messages"][0] != manifests[1].Objects["messages"][0] {
		t.Fatal("different cuts didn't share immutable payloads")
	}
}
