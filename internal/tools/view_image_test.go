package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/document"
	"go.harness.dev/harness/internal/engine/types"
)

func writePNG(t *testing.T, path string, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func runViewImage(t *testing.T, cwd string, imageInput bool, path string) (types.ContentBlock, types.ContentBlock, error) {
	t.Helper()
	tool := newViewImageTool(cwd, ToolsOptions{ImageInput: imageInput})
	args, _ := json.Marshal(map[string]string{"path": path})
	result, err := tool.Execute(context.Background(), "call", args, nil)
	if err != nil {
		return types.ContentBlock{}, types.ContentBlock{}, err
	}
	if len(result.Content) != 2 {
		t.Fatalf("content = %#v", result.Content)
	}
	return result.Content[0], result.Content[1], nil
}

func TestViewImageReturnsImageBlock(t *testing.T) {
	cwd := t.TempDir()
	raw := writePNG(t, filepath.Join(cwd, "shot.png"), 3, 2)
	text, img, err := runViewImage(t, cwd, true, "shot.png")
	if err != nil {
		t.Fatal(err)
	}
	if img.Type != types.BlockImage || img.MimeType != "image/png" || img.Data != base64.StdEncoding.EncodeToString(raw) {
		t.Fatalf("image block = %+v", img)
	}
	if !strings.Contains(text.Text, "shot.png") || !strings.Contains(text.Text, "3x2") {
		t.Fatalf("caption = %q", text.Text)
	}
}

func TestViewImageRefusals(t *testing.T) {
	cwd := t.TempDir()
	writePNG(t, filepath.Join(cwd, "ok.png"), 1, 1)
	writePNG(t, filepath.Join(cwd, "wide.png"), document.MaxInlineImageSidePx+1, 1)
	if err := os.WriteFile(filepath.Join(cwd, "notes.png"), []byte("plain text, not an image"), 0o600); err != nil {
		t.Fatal(err)
	}
	huge := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, document.MaxInlineImageBytes)...)
	if err := os.WriteFile(filepath.Join(cwd, "huge.png"), huge, 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(filepath.Dir(cwd), "outside.png")
	writePNG(t, outside, 1, 1)
	t.Cleanup(func() { _ = os.Remove(outside) })

	if _, _, err := runViewImage(t, cwd, false, "ok.png"); !errors.Is(err, errNoImageInput) {
		t.Fatalf("text-only model: %v", err)
	}
	for path, want := range map[string]string{
		"wide.png":    "px per side",
		"notes.png":   "not a PNG, JPEG, GIF or WebP",
		"huge.png":    "inline images are limited to",
		"missing.png": "no such file",
	} {
		if _, _, err := runViewImage(t, cwd, true, path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", path, err, want)
		}
	}
	if _, _, err := runViewImage(t, cwd, true, outside); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("outside path: %v", err)
	}
}
