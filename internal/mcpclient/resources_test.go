package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/tools"
)

func TestMCPResourcesListReadAndRefresh(t *testing.T) {
	manager, _ := openFixture(t, "normal")
	ctx := context.Background()
	listing, images, err := manager.ReadResource(ctx, "mcp://fixture/")
	text := string(listing.Data)
	if err != nil || len(images) != 0 || !listing.Rendered || !strings.Contains(text, "3 resources, 1 resource templates") || !strings.Contains(text, "- file:///notes.md  notes (Project notes) [text/markdown]: fixture notes") || !strings.Contains(text, "- fixture://logs/{day}  logs") || strings.Contains(text, "added.txt") {
		t.Fatalf("listing = %q, %v", text, err)
	}
	if servers, _, err := manager.ReadResource(ctx, "mcp://"); err != nil || !strings.Contains(string(servers.Data), "- fixture: 3 resources, 1 resource templates") {
		t.Fatalf("server listing = %q, %v", servers.Data, err)
	}

	note, images, err := manager.ReadResource(ctx, "mcp://fixture/file:///notes.md")
	if err != nil || len(images) != 0 || note.Rendered || string(note.Data) != "line one\nline two\nline three\n" || note.URI != "mcp://fixture/file:///notes.md" {
		t.Fatalf("text resource = %+v %v", note, err)
	}
	if templated, _, err := manager.ReadResource(ctx, "mcp://fixture/fixture://logs/monday"); err != nil || string(templated.Data) != "log for monday" {
		t.Fatalf("template resource = %q %v", templated.Data, err)
	}
	pixel, images, err := manager.ReadResource(ctx, "mcp://fixture/fixture://pixel.png")
	if err != nil || len(images) != 1 || images[0].Type != types.BlockImage || images[0].MimeType != "image/png" || !strings.Contains(string(pixel.Data), "fixture://pixel.png") {
		t.Fatalf("image resource = %+v %v %v", pixel, images, err)
	}
	blob, _, err := manager.ReadResource(ctx, "mcp://fixture/fixture://data.bin")
	_, path, found := strings.Cut(string(blob.Data), "saved locally: ")
	if err != nil || !found {
		t.Fatalf("blob resource = %q %v", blob.Data, err)
	}
	if saved, err := os.ReadFile(strings.TrimSpace(path)); err != nil || !bytes.Equal(saved, []byte{0, 1, 2, 3}) {
		t.Fatalf("spilled blob = %v %v", saved, err)
	}

	if _, _, err := manager.ReadResource(ctx, "mcp://fixture/fixture://missing"); err == nil || !strings.Contains(err.Error(), "resource not found") {
		t.Fatalf("missing resource error = %v", err)
	}
	if _, _, err := manager.ReadResource(ctx, "mcp://nope/x"); err == nil || !strings.Contains(err.Error(), `unknown MCP server "nope"; configured: fixture`) {
		t.Fatalf("unknown server error = %v", err)
	}

	// A list_changed notification refreshes the one catalog.
	if _, _, err := manager.ReadResource(ctx, "mcp://fixture/fixture://bump"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		listing, _, err = manager.ReadResource(ctx, "mcp://fixture/")
		if err == nil && strings.Contains(string(listing.Data), "fixture://added.txt") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("resources/list_changed did not refresh: %q %v", listing.Data, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(manager.Tools()) != 10 {
		t.Fatalf("refresh lost tools: %d", len(manager.Tools()))
	}
}

func TestReadToolServesMCPResources(t *testing.T) {
	manager, _ := openFixture(t, "normal")
	read, err := tools.NewTool(tools.ReadTool, t.TempDir(), tools.ToolsOptions{MCP: manager})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read.Description, "mcp://<server>/") {
		t.Fatal("read description omits mcp:// with servers configured")
	}
	execute := func(path string) (string, []types.ContentBlock, error) {
		args, _ := json.Marshal(map[string]string{"path": path})
		result, err := read.Execute(context.Background(), "read", args, nil)
		var text strings.Builder
		for _, block := range result.Content {
			text.WriteString(block.Text)
		}
		return text.String(), result.Content, err
	}
	text, _, err := execute("mcp://fixture/file:///notes.md:2-2")
	if err != nil || !strings.HasPrefix(text, "[mcp://fixture/file:///notes.md#") || !strings.Contains(text, "2:line two") || strings.Contains(text, "line one") {
		t.Fatalf("ranged resource read = %q %v", text, err)
	}
	text, _, err = execute("mcp://fixture/")
	if err != nil || !strings.Contains(text, "fixture://logs/{day}") {
		t.Fatalf("listing read = %q %v", text, err)
	}
	_, blocks, err := execute("mcp://fixture/fixture://pixel.png")
	if err != nil || len(blocks) != 2 || blocks[1].Type != types.BlockImage {
		t.Fatalf("image read = %+v %v", blocks, err)
	}
	plain, err := tools.NewTool(tools.ReadTool, t.TempDir(), tools.ToolsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain.Description, "mcp://") {
		t.Fatal("read description mentions mcp:// without servers")
	}
	if _, err := plain.Execute(context.Background(), "read", json.RawMessage(`{"path":"mcp://fixture/"}`), nil); err == nil || !strings.Contains(err.Error(), "no MCP servers") {
		t.Fatalf("read without servers = %v", err)
	}
}

func TestReadToolBoundsAndRecoversMCPResources(t *testing.T) {
	manager, _ := openFixture(t, "oversized")
	read, err := tools.NewTool(tools.ReadTool, t.TempDir(), tools.ToolsOptions{MCP: manager})
	if err != nil {
		t.Fatal(err)
	}
	execute := func(t *testing.T, path string) (string, []types.ContentBlock) {
		t.Helper()
		args, _ := json.Marshal(map[string]string{"path": path})
		result, err := read.Execute(context.Background(), "read", args, nil)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var text strings.Builder
		for _, block := range result.Content {
			text.WriteString(block.Text)
		}
		return text.String(), result.Content
	}
	t.Run("text beside an image gets the read budget", func(t *testing.T) {
		text, blocks := execute(t, "mcp://fixture/fixture://big-with-image")
		if len(text) > 60000 || !strings.Contains(text, "Showing lines") || blocks[len(blocks)-1].Type != types.BlockImage {
			t.Fatalf("text+image read: %d text bytes, %d blocks, ends %q", len(text), len(blocks), text[max(0, len(text)-200):])
		}
	})
	t.Run("a line too long to show is saved for bash", func(t *testing.T) {
		text, _ := execute(t, "mcp://fixture/fixture://long-line.txt")
		_, saved, found := strings.Cut(text, "saved at ")
		saved, _, _ = strings.Cut(saved, ";")
		if !found {
			t.Fatalf("oversized line read names no saved copy: %q", text)
		}
		if data, err := os.ReadFile(saved); err != nil || len(data) != 100000 {
			t.Fatalf("saved copy %q: %d bytes, %v", saved, len(data), err)
		}
	})
	t.Run("a listed URI with its own ?q= is read verbatim", func(t *testing.T) {
		if text, _ := execute(t, "mcp://fixture/https://example/search?q=term"); !strings.Contains(text, "results for term") {
			t.Fatalf("verbatim ?q= resource read = %q", text)
		}
	})
	t.Run("?q= after a listed URI is a JSON query", func(t *testing.T) {
		if text, _ := execute(t, "mcp://fixture/fixture://data.json?q=.items[1].name"); !strings.Contains(text, `"b"`) || strings.Contains(text, `"a"`) {
			t.Fatalf("queried resource read = %q", text)
		}
	})
}
