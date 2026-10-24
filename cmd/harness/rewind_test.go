package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/checkpoint"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/tools"
)

func TestRewindCommandOffline(t *testing.T) {
	cwd, sessionDir := t.TempDir(), t.TempDir()
	original := filepath.Join(cwd, "original")
	if err := os.WriteFile(original, []byte("before"), 0755); err != nil {
		t.Fatal(err)
	}
	store, err := journal.Create(t.Context(), sessionDir, cwd, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	checkpoints, err := checkpoint.New(t.Context(), cwd, store, tools.DefaultMutationQueue())
	if err != nil {
		t.Fatal(err)
	}
	saved, err := checkpoints.Create(t.Context(), "CLI baseline", []types.Message{types.UserMessage{Content: types.StringContent("saved prompt")}})
	if err != nil {
		t.Fatal(err)
	}
	journalPath := store.Path()
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(original, []byte("outside change"), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"rewind", "-session", journalPath, "-session-dir", sessionDir, "-checkpoint", saved.ID}
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "outside") {
		t.Fatalf("outside edit accepted: code=%d stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run(append(args, "-force"), &stdout, &stderr); code != 0 {
		t.Fatalf("offline rewind: code=%d stderr=%s", code, stderr.String())
	}
	content, err := os.ReadFile(original)
	if err != nil || string(content) != "before" {
		t.Fatalf("restored bytes=%q err=%v", content, err)
	}
	loaded, err := journal.Load(t.Context(), journalPath)
	if err != nil || len(loaded.Messages) != 1 {
		t.Fatalf("restored conversation=%+v err=%v", loaded, err)
	}
	if !strings.Contains(stdout.String(), "journal history retained") {
		t.Fatalf("output=%s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"rewind", "-help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("rewind help: code=%d stderr=%s", code, stderr.String())
	}
}

func TestRewindListIsReadOnlyWhileSessionIsOpen(t *testing.T) {
	cwd, sessionDir := t.TempDir(), t.TempDir()
	store, err := journal.Create(t.Context(), sessionDir, cwd, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager, err := checkpoint.New(t.Context(), cwd, store, tools.DefaultMutationQueue())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(t.Context(), "live checkpoint", nil); err != nil {
		t.Fatal(err)
	}
	pendingPath := filepath.Join(store.Path()+".d", "checkpoints", "pending.json")
	pending := []byte(`{"must":"remain untouched"}`)
	if err := os.WriteFile(pendingPath, pending, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"rewind", "-session", store.Path(), "-list"}, &stdout, &stderr); code != 0 {
		t.Fatalf("live read-only list: %d %s", code, stderr.String())
	}
	after, err := os.ReadFile(store.Path())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("list changed journal")
	}
	after, err = os.ReadFile(pendingPath)
	if err != nil || !bytes.Equal(pending, after) {
		t.Fatal("list settled pending rewind")
	}
	if !strings.Contains(stdout.String(), "live checkpoint") {
		t.Fatalf("list=%s", stdout.String())
	}
}
