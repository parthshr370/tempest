package journal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/tempest/durable"
)

func TestOverflowRetryReceiptSurvivesRestart(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	if err := store.AppendMessage(t.Context(), userMessage("original input")); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimOverflow(t.Context())
	if err != nil || !claimed {
		t.Fatal(claimed, err)
	}
	claimed, err = store.ClaimOverflow(t.Context())
	if err != nil || claimed {
		t.Fatal("retry budget reopened", claimed, err)
	}
	path := store.Path()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, _, err := OpenForResume(t.Context(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(t, resumed)
	claimed, err = resumed.ClaimOverflow(t.Context())
	if err != nil || claimed {
		t.Fatal("restart reopened retry budget", claimed, err)
	}
	if err := resumed.AppendMessage(t.Context(), userMessage("different admitted input")); err != nil {
		t.Fatal(err)
	}
	claimed, err = resumed.ClaimOverflow(t.Context())
	if err != nil || !claimed {
		t.Fatal("new input lost retry budget", claimed, err)
	}
}

func TestForkRejectsPartialToolRoundAndSelectsPrecedingBoundary(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	defer closeStore(t, store)
	raw := json.RawMessage(`{"model":"test-model","allowedTools":[],"environment":"local"}`)
	if _, err := store.Commit(t.Context(), durable.Commit{ID: "agent-selection", Changes: []durable.Change{{SessionID: store.ID(), Kind: durable.DocumentKind, Namespace: "_tempest/agent", ID: "selection", Version: 1, Value: raw}}}); err != nil {
		t.Fatal(err)
	}
	safe, err := store.AppendAssigned(t.Context(), MessageEntry{Message: userMessage("safe input")})
	if err != nil {
		t.Fatal(err)
	}
	calls := types.AssistantMessage{Content: []types.ContentBlock{{Type: types.BlockToolCall, ID: "first", Name: "read", Arguments: json.RawMessage(`{}`)}, {Type: types.BlockToolCall, ID: "second", Name: "read", Arguments: json.RawMessage(`{}`)}}, StopReason: types.StopToolUse}
	if err := store.AppendMessage(t.Context(), calls); err != nil {
		t.Fatal(err)
	}
	partial, err := store.AppendAssigned(t.Context(), MessageEntry{Message: types.ToolResultMessage{ToolCallID: "first", ToolName: "read", Content: []types.ContentBlock{types.NewText("one result")}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CaptureFork(t.Context(), partial.ID, false); err == nil {
		t.Fatal("partial round accepted")
	}
	seed, err := store.CaptureFork(t.Context(), partial.ID, true)
	if err != nil || seed.Prefix.EntryID != safe.ID || len(seed.Messages) != 1 {
		t.Fatal(seed, err)
	}
	finished, err := store.AppendAssigned(t.Context(), MessageEntry{Message: types.ToolResultMessage{ToolCallID: "second", ToolName: "read", Content: []types.ContentBlock{types.NewText("second result")}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CaptureFork(t.Context(), finished.ID, false); err != nil {
		t.Fatal("complete tool round rejected", err)
	}
}

func TestHistoryCursorRetainsPreResetCutAndRejectsChangedQuery(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	defer closeStore(t, store)
	for index := range 7 {
		if err := store.AppendMessage(t.Context(), userMessage(fmt.Sprintf("needle %d", index))); err != nil {
			t.Fatal(err)
		}
	}
	first, err := store.SearchHistory(t.Context(), HistoryOptions{Query: "needle", Limit: 2})
	if err != nil || len(first.Matches) != 2 || first.NextCursor == "" {
		t.Fatal(first, err)
	}
	if err := store.Append(t.Context(), ContextHeadEntry{Reason: "reset", Note: "new context"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage(t.Context(), userMessage("needle after cut")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SearchHistory(t.Context(), HistoryOptions{Query: "changed", Cursor: first.NextCursor, Limit: 2}); err == nil {
		t.Fatal("query change accepted")
	}
	count := len(first.Matches)
	cursor := first.NextCursor
	for cursor != "" {
		page, err := store.SearchHistory(t.Context(), HistoryOptions{Query: "needle", Cursor: cursor, Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		count += len(page.Matches)
		cursor = page.NextCursor
	}
	if count != 7 {
		t.Fatal("cursor lost its immutable cut", count)
	}
}

func TestMissingForkArchiveFailsAsCorruption(t *testing.T) {
	backend := durable.NewMemory()
	defer backend.Close(context.Background())
	store, err := CreateBackend(t.Context(), backend, t.TempDir(), t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(t, store)
	reference := ArchiveRef{Digest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ParentID: "bbbbbbbbbbbbbbbb", EntryID: "cccccccccccccccc"}
	if err := store.Append(t.Context(), ContextHeadEntry{Reason: "fork", Prefix: &reference}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(t.Context()); !errors.Is(err, durable.ErrCorrupt) {
		t.Fatal("missing prefix silently became empty history", err)
	}
}
