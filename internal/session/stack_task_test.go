package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/provider/faux"
)

func TestBuildAgentStackWithTaskTool(t *testing.T) {
	provider := faux.New(faux.Options{})
	stack, err := BuildAgentStack(StackConfig{
		Cwd:        t.TempDir(),
		Model:      provider.Model(),
		StreamFn:   provider.StreamSimple,
		EnableTask: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, tool := range stack.Agent.State().Tools {
		seen[tool.Name] = true
	}
	if !seen["task"] {
		t.Fatal("EnableTask stack does not register the task tool")
	}
}

func TestBuildAgentStackPlanModeOmitsTaskTool(t *testing.T) {
	provider := faux.New(faux.Options{})
	stack, err := BuildAgentStack(StackConfig{
		Cwd:        t.TempDir(),
		Model:      provider.Model(),
		StreamFn:   provider.StreamSimple,
		EnableTask: true,
		Plan:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range stack.Agent.State().Tools {
		if tool.Name == "task" {
			t.Fatal("plan stack must not register the task tool")
		}
	}
}

func TestResolveLocalAttachments(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(path, []byte("hello attachment"), 0o600); err != nil {
		t.Fatal(err)
	}
	cacheRoot := filepath.Join(dir, "cache")
	registry, reader, err := ResolveLocalAttachments(ctx, []string{path}, cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := registry.Get("notes.txt")
	if !ok {
		t.Fatal("attachment entry missing from registry")
	}
	if !entry.TextReadable {
		t.Fatalf("text file media type %q not marked TextReadable", entry.MediaType)
	}
	if entry.MediaType != "text/plain" {
		t.Fatalf("media type = %q, want text/plain", entry.MediaType)
	}
	size, err := reader.StatBlob(ctx, entry.Blob.Store, entry.Blob.Key)
	if err != nil {
		t.Fatalf("stored blob unreadable: %v", err)
	}
	if size != entry.SizeBytes {
		t.Fatalf("stored blob size = %d, want %d", size, entry.SizeBytes)
	}
}

func TestResolveLocalAttachmentsMissingPathErrors(t *testing.T) {
	dir := t.TempDir()
	_, _, err := ResolveLocalAttachments(context.Background(), []string{filepath.Join(dir, "nope.txt")}, filepath.Join(dir, "cache"))
	if err == nil {
		t.Fatal("missing attachment path must be a configuration error")
	}
	if !strings.Contains(err.Error(), "nope.txt") {
		t.Fatalf("error should name the missing file, got %v", err)
	}
}

func TestResolveLocalAttachmentsEmptyPaths(t *testing.T) {
	dir := t.TempDir()
	registry, reader, err := ResolveLocalAttachments(context.Background(), nil, filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	if registry == nil || reader == nil {
		t.Fatal("empty attachment list must still return a registry and reader")
	}
}
