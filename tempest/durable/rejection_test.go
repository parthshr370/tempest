package durable

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestInvalidCommitDoesNotMasqueradeAsWriteFailure(t *testing.T) {
	writeFailure := errors.New("disk unavailable")
	writes := 0
	store := NewLog(func(Envelope) error { writes++; return writeFailure }, nil)
	batch := Commit{ID: "invalid", Changes: []Change{{SessionID: "session", Kind: DocumentKind, Namespace: "app", ID: "doc", Version: 2, Value: json.RawMessage(`{}`)}}}
	if _, err := store.Commit(context.Background(), batch); !errors.Is(err, ErrInvalidCommit) || errors.Is(err, writeFailure) {
		t.Fatalf("validation error=%v", err)
	}
	if writes != 0 {
		t.Fatal("invalid batch reached writer")
	}
	batch.ID, batch.Changes[0].Version = "valid", 1
	if _, err := store.Commit(context.Background(), batch); !errors.Is(err, writeFailure) || errors.Is(err, ErrInvalidCommit) {
		t.Fatalf("write error=%v", err)
	}
	if writes != 1 {
		t.Fatalf("writes=%d", writes)
	}
}
