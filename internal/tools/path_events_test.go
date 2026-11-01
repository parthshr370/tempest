package tools

import (
	"path/filepath"
	"slices"
	"testing"

	"go.harness.dev/harness/internal/agent"
)

// File tools report the workspace paths they read or listed, and nothing for
// a failed read, so nested context discovery never parses tool output.
func TestFileToolsReportTouchedPaths(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pkg", "b.go"), "package b\n")
	writeFile(t, filepath.Join(dir, "a.txt"), "one\n")
	all := AllTools(dir, ToolsOptions{})
	for _, test := range []struct {
		name string
		tool ToolName
		args map[string]any
		want []agent.PathEvent
	}{
		{"read file", ReadTool, map[string]any{"path": "pkg/b.go"}, []agent.PathEvent{{Path: filepath.Join(dir, "pkg", "b.go")}}},
		{"read directory", ReadTool, map[string]any{"path": "pkg"}, []agent.PathEvent{{Path: filepath.Join(dir, "pkg"), Dir: true}}},
		{"read batch", ReadTool, map[string]any{"paths": []string{"a.txt", "missing.txt", "pkg/b.go"}}, []agent.PathEvent{{Path: filepath.Join(dir, "a.txt")}, {Path: filepath.Join(dir, "pkg", "b.go")}}},
		{"ls", LsTool, map[string]any{"path": "pkg"}, []agent.PathEvent{{Path: filepath.Join(dir, "pkg"), Dir: true}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := executeTool(t, all[test.tool], test.args)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(result.Paths, test.want) {
				t.Fatalf("paths = %+v, want %+v", result.Paths, test.want)
			}
		})
	}
	if result, err := executeTool(t, all[ReadTool], map[string]any{"path": "a.txt:9"}); err == nil && len(result.Paths) != 0 {
		t.Fatalf("failed read reported %+v", result.Paths)
	}
}
