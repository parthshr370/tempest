package mcpclient

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"go.harness.dev/harness/internal/mcpwire"
)

func TestMCPResultsExactSpillAndEscapedLimits(t *testing.T) {
	original := struct {
		Content           []map[string]string `json:"content"`
		StructuredContent map[string]string   `json:"structuredContent"`
		IsError           bool                `json:"isError"`
	}{Content: []map[string]string{{"type": "text", "text": strings.Repeat("世\"\n\\", 100000)}}, StructuredContent: map[string]string{"large": strings.Repeat("x", 10000)}}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	result, err := mapResult(t.TempDir(), "fixture", "large", raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result.Content)
	if err != nil || len(encoded) > maxInlineContent || len(result.Details) > maxDetails {
		t.Fatalf("limits: %d %d %v", len(encoded), len(result.Details), err)
	}
	textSize := 0
	for _, block := range result.Content {
		if !utf8.ValidString(block.Text) {
			t.Fatal("invalid UTF-8 cut")
		}
		if block.Type == "text" {
			textSize += serializedSize(block) + 1
		}
	}
	if textSize > maxInlineText {
		t.Fatalf("escaped text limit %d", textSize)
	}
	var details resultDetails
	if err = json.Unmarshal(result.Details, &details); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(details.MCP.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(saved, raw) {
		t.Fatal("spilled prefix instead of complete original result")
	}
	if !details.MCP.Truncated || details.MCP.StructuredContentPath == "" || len(details.MCP.StructuredContent) > 0 {
		t.Fatalf("details %s", result.Details)
	}
	info, err := os.Stat(filepath.Dir(details.MCP.OutputPath))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("directory mode %v %v", info, err)
	}
}
func TestMCPResultsMalformedAndSpillFailure(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		raw       string
		code      string
		directory string
	}{
		{"invalid-base64", `{"content":[{"type":"audio","data":"!","mimeType":"audio/wav"}]}`, "mcp_protocol", t.TempDir()},
		{"null-content", `{"content":null}`, "mcp_protocol", t.TempDir()},
		{"structured-array", `{"content":[],"structuredContent":[]}`, "mcp_protocol", t.TempDir()},
		{"spill-error", `{"content":[{"type":"unknown","value":"must not lose"}]}`, "mcp_spill", file},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := mapResult(test.directory, "fixture", "tool", json.RawMessage(test.raw), nil)
			if err == nil || !strings.Contains(err.Error(), test.code) {
				t.Fatalf("result %v", err)
			}
		})
	}
	if _, err := mapResult(t.TempDir(), "fixture", "tool", json.RawMessage(strings.Repeat(" ", mcpwire.MaxResponse+1)), nil); err == nil || !strings.Contains(err.Error(), "mcp_output_limit") {
		t.Fatalf("oversize: %v", err)
	}
}
func TestMCPOutputSchemaFailureIsToolError(t *testing.T) {
	schema, err := compileSchema(json.RawMessage(`{"type":"object","required":["value"],"properties":{"value":{"type":"integer"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	result, err := mapResult(t.TempDir(), "fixture", "tool", json.RawMessage(`{"content":[{"type":"text","text":"retained"}],"structuredContent":{"value":"wrong"}}`), schema)
	if err != nil || !result.IsError || result.Content[0].Text != "retained" {
		t.Fatalf("schema failure lost content: %#v %v", result, err)
	}
}
