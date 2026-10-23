package tools

import (
	"encoding/json"
	"reflect"
	"testing"

	"go.harness.dev/harness/internal/document"
	"go.harness.dev/harness/internal/skills"
)

// assertSchemaParity checks the migrated builder output decodes to the exact
// same JSON Schema (structurally, key order aside) as the original raw string —
// including an explicit "required":[], which is asserted, not normalized away.
func assertSchemaParity(t *testing.T, name string, got json.RawMessage, wantRaw string) {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatalf("%s: unmarshal migrated schema: %v (%s)", name, err, got)
	}
	if err := json.Unmarshal([]byte(wantRaw), &b); err != nil {
		t.Fatalf("%s: unmarshal fixture: %v", name, err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("%s schema drift:\n migrated=%s\n original=%s", name, got, wantRaw)
	}
}

// originalToolSchemas is the exact raw JSON each tool advertised before the
// migration to the schema builder. The migrated builder output must remain
// semantically identical to these.
var originalToolSchemas = map[ToolName]string{
	ReadTool:  `{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"number"},"limit":{"type":"number"},"mode":{"type":"string","enum":["auto","raw"],"default":"auto"}},"required":["path"]}`,
	WriteTool: `{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`,
	EditTool:  `{"type":"object","properties":{"path":{"type":"string"},"edits":{"type":"array","items":{"type":"object","properties":{"old_text":{"type":"string"},"new_text":{"type":"string"},"anchor":{"type":"string"}},"required":["old_text","new_text"]}}},"required":["path","edits"]}`,
	BashTool:  `{"type":"object","properties":{"command":{"type":"string"},"timeout":{"type":"number"}},"required":["command"]}`,
	GrepTool:  `{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"},"glob":{"type":"string"},"ignore_case":{"type":"boolean"},"literal":{"type":"boolean"},"context":{"type":"number"},"limit":{"type":"number"}},"required":["pattern"]}`,
	FindTool:  `{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"},"limit":{"type":"number"}},"required":["pattern"]}`,
	LsTool:    `{"type":"object","properties":{"path":{"type":"string"},"limit":{"type":"number"}}}`,
	TodoTool:  `{"type":"object","properties":{"todos":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"content":{"type":"string"},"status":{"type":"string","enum":["pending","in_progress","completed"]},"priority":{"type":"string","enum":["high","medium","low"]}},"required":["content","status"]}}},"required":["todos"]}`,
	SkillTool: `{"type":"object","properties":{"name":{"type":"string","description":"the skill name to load"},"command":{"type":"string"}},"required":[]}`,
	WebFetch:  `{"type":"object","properties":{"url":{"type":"string"},"max_bytes":{"type":"number"}},"required":["url"]}`,
	WebSearch: `{"type":"object","properties":{"query":{"type":"string"},"num_results":{"type":"number"}},"required":["query"]}`,
}

func TestToolSchemaParity(t *testing.T) {
	all := AllTools(t.TempDir(), ToolsOptions{EnableWeb: true, Skills: []skills.Skill{{Name: "x"}}})
	for name, want := range originalToolSchemas {
		tool, ok := all[name]
		if !ok {
			t.Fatalf("tool %s not built", name)
		}
		assertSchemaParity(t, string(name), tool.Parameters, want)
	}
}

func TestAttachmentSchemaParity(t *testing.T) {
	reg := document.NewAttachmentRegistry()
	tool := newAttachmentTool(reg, document.NewCacheRootBlobReader(t.TempDir()), func(s string) string { return s })
	want := `{"type":"object","properties":{"op":{"type":"string","enum":["list","read","grep","stats"]},"id":{"type":"string","description":"attachment id or filename (read/grep/stats)"},"pattern":{"type":"string","description":"regex to match (grep)"},"offset":{"type":"number","description":"1-indexed start line (read)"},"limit":{"type":"number","description":"max lines (read)"},"group_by":{"type":"string","description":"column name or 1-indexed number to group rows by (stats)"},"value":{"type":"string","description":"numeric column name or 1-indexed number to sum/min/max/mean (stats)"},"top":{"type":"number","description":"max groups to report, default 20 (stats)"}},"required":["op"]}`
	assertSchemaParity(t, "attachment", tool.Parameters, want)
}
