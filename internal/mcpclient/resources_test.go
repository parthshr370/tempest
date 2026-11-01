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
	if len(manager.Tools()) != 9 {
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
