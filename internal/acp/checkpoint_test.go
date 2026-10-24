package acp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/tempest"
)

type checkpointBackend struct {
	Backend
	session *tempest.Session
}

func (backend checkpointBackend) Acquire(id string) (*tempest.Session, func(), error) {
	if id != backend.session.ID() {
		return nil, nil, os.ErrNotExist
	}
	return backend.session, func() {}, nil
}

func TestCheckpointACPMethodsRestoreFilesAndHistory(t *testing.T) {
	cwd := t.TempDir()
	path := filepath.Join(cwd, "file")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	provider := faux.New(faux.Options{})
	session, err := tempest.Open(t.Context(), tempest.Options{Cwd: cwd, SessionDir: t.TempDir(), New: &tempest.NewSession{PermissionMode: "bypass"}, Stack: &tempest.StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := session.Close(t.Context()); err != nil {
			t.Error(err)
		}
	})
	client := NewClient(nil, checkpointBackend{session: session}, nil)
	client.initialized = true
	arguments, err := json.Marshal(struct {
		SessionID string `json:"sessionId"`
		Goal      string `json:"goal"`
	}{session.ID(), "ACP investigation"})
	if err != nil {
		t.Fatal(err)
	}
	result, wireErr := client.Handle(t.Context(), "tempest/checkpoint", arguments)
	if wireErr != nil {
		t.Fatal(wireErr)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var saved tempest.Checkpoint
	if err := json.Unmarshal(encoded, &saved); err != nil || saved.ID == "" {
		t.Fatalf("checkpoint=%s %v", encoded, err)
	}
	listed, wireErr := client.Handle(t.Context(), "tempest/checkpoints", arguments)
	if wireErr != nil {
		t.Fatal(wireErr)
	}
	encoded, err = json.Marshal(listed)
	if err != nil {
		t.Fatal(err)
	}
	var listing struct {
		Checkpoints []tempest.Checkpoint `json:"checkpoints"`
	}
	if err := json.Unmarshal(encoded, &listing); err != nil || len(listing.Checkpoints) != 1 || listing.Checkpoints[0].ID != saved.ID {
		t.Fatalf("list=%s %v", encoded, err)
	}
	if err := os.WriteFile(path, []byte("operator edit"), 0600); err != nil {
		t.Fatal(err)
	}
	rewindArgs, err := json.Marshal(struct {
		SessionID  string `json:"sessionId"`
		Checkpoint string `json:"checkpoint"`
		Force      bool   `json:"force"`
		Report     string `json:"report"`
	}{session.ID(), saved.ID, false, "ACP retained finding"})
	if err != nil {
		t.Fatal(err)
	}
	if _, wireErr := client.Handle(t.Context(), "tempest/rewind", rewindArgs); wireErr == nil {
		t.Fatal("ACP silently forced outside edits")
	}
	rewindArgs, err = json.Marshal(struct {
		SessionID  string `json:"sessionId"`
		Checkpoint string `json:"checkpoint"`
		Force      bool   `json:"force"`
		Report     string `json:"report"`
	}{session.ID(), saved.ID, true, "ACP retained finding"})
	if err != nil {
		t.Fatal(err)
	}
	if _, wireErr := client.Handle(t.Context(), "tempest/rewind", rewindArgs); wireErr != nil {
		t.Fatal(wireErr)
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "original" {
		t.Fatalf("ACP restore=%q %v", contents, err)
	}
	if history, err := session.History(session.Snapshot().HistoryEpoch, 0, 100); err != nil || len(history) == 0 {
		t.Fatalf("ACP history=%+v %v", history, err)
	}
}
