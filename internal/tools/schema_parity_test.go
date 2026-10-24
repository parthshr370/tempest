package tools

import (
	"encoding/json"
	"reflect"
	"testing"

	"go.harness.dev/harness/internal/checkpoint"
	"go.harness.dev/harness/internal/document"
	"go.harness.dev/harness/internal/irc"
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
	ReadTool:  `{"type":"object","properties":{"path":{"type":"string","description":"one file, directory or internal URL, with an optional :START-END or :START+COUNT selector"},"paths":{"type":"array","items":{"type":"string"},"minItems":1,"maxItems":20,"description":"several files read in one call, each with an optional selector; use instead of path"},"offset":{"type":"number"},"limit":{"type":"number"},"mode":{"type":"string","enum":["auto","raw","outline"],"default":"auto"}}}`,
	WriteTool: `{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`,
	EditTool:  `{"type":"object","properties":{"path":{"type":"string"},"edits":{"type":"array","items":{"type":"object","properties":{"old_text":{"type":"string"},"new_text":{"type":"string"},"anchor":{"type":"string"}},"required":["old_text","new_text"]}}},"required":["path","edits"]}`,
	BashTool:  `{"type":"object","properties":{"command":{"type":"string"},"timeout":{"type":"number"}},"required":["command"]}`,
	GrepTool:  `{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string","description":"one file, directory or internal URL to search; default the working directory"},"paths":{"type":"array","items":{"type":"string"},"minItems":1,"maxItems":20,"description":"several workspace files or directories searched in one call; use instead of path"},"glob":{"type":"string"},"ignore_case":{"type":"boolean"},"literal":{"type":"boolean"},"context":{"type":"number"},"limit":{"type":"number"}},"required":["pattern"]}`,
	FindTool:  `{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"},"limit":{"type":"number"}},"required":["pattern"]}`,
	LsTool:    `{"type":"object","properties":{"path":{"type":"string"},"limit":{"type":"number"}}}`,
	TodoTool:  `{"type":"object","properties":{"todos":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"content":{"type":"string"},"status":{"type":"string","enum":["pending","in_progress","completed","blocked","dropped"]},"priority":{"type":"string","enum":["high","medium","low"]},"phase":{"type":"string"},"blocker":{"type":"string"}},"required":["content","status"]}},"op":{"type":"string","enum":["init","start","done","block","append","drop"]},"phase":{"type":"string"},"items":{"type":"array","items":{"type":"string"}},"reason":{"type":"string"}},"required":[]}`,
	SkillTool: `{"type":"object","properties":{"name":{"type":"string","description":"the skill name to load"},"command":{"type":"string"}},"required":[]}`,
	WebFetch:  `{"type":"object","properties":{"url":{"type":"string"},"max_bytes":{"type":"number"}},"required":["url"]}`,
	WebSearch: `{"type":"object","properties":{"query":{"type":"string"},"num_results":{"type":"number"}},"required":["query"]}`,
	// hashline postdates the migration; its fixture freezes the one-argument surface.
	HashlineTool:   `{"type":"object","properties":{"input":{"type":"string","description":"the hashline patch: [PATH#TAG] sections with PUT/CUT/REM/MV ops"}},"required":["input"]}`,
	ViewImageTool:  `{"type":"object","properties":{"path":{"type":"string","description":"image file path, relative to the working directory or absolute inside it"}},"required":["path"]}`,
	CheckpointTool: `{"type":"object","properties":{"goal":{"type":"string"}},"required":["goal"],"additionalProperties":false}`,
	RewindTool:     `{"type":"object","properties":{"report":{"type":"string"},"checkpoint":{"type":"string"}},"required":["report"],"additionalProperties":false}`,
}

func TestToolSchemaParity(t *testing.T) {
	all := AllTools(t.TempDir(), ToolsOptions{EnableWeb: true, Skills: []skills.Skill{{Name: "x"}}, Checkpoints: new(checkpoint.Manager)})
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

// intelligenceToolSchemas are the exact schemas frozen in
// docs/design/07-intelligence.md sections 5.1 and 7.1.
var intelligenceToolSchemas = map[ToolName]string{
	LSPDefinitionTool:  `{"type":"object","additionalProperties":false,"properties":{"path":{"type":"string","minLength":1},"line":{"type":"integer","minimum":1},"column":{"type":"integer","minimum":1},"server":{"type":"string","enum":["go","yaml","terraform","helm"]},"max_results":{"type":"integer","minimum":1,"maximum":200,"default":50}},"required":["path","line","column"]}`,
	LSPReferencesTool:  `{"type":"object","additionalProperties":false,"properties":{"path":{"type":"string","minLength":1},"line":{"type":"integer","minimum":1},"column":{"type":"integer","minimum":1},"server":{"type":"string","enum":["go","yaml","terraform","helm"]},"include_declaration":{"type":"boolean","default":false},"max_results":{"type":"integer","minimum":1,"maximum":200,"default":50}},"required":["path","line","column"]}`,
	LSPRenameTool:      `{"type":"object","additionalProperties":false,"properties":{"path":{"type":"string","minLength":1},"line":{"type":"integer","minimum":1},"column":{"type":"integer","minimum":1},"server":{"type":"string","enum":["go","yaml","terraform","helm"]},"new_name":{"type":"string","minLength":1,"maxLength":256}},"required":["path","line","column","new_name"]}`,
	LSPDiagnosticsTool: `{"type":"object","additionalProperties":false,"properties":{"path":{"type":"string","minLength":1},"server":{"type":"string","enum":["go","yaml","terraform","helm"]},"severity":{"type":"string","enum":["all","error","warning","information","hint"],"default":"all"},"wait_ms":{"type":"integer","minimum":0,"maximum":10000,"default":2000},"max_results":{"type":"integer","minimum":1,"maximum":200,"default":100}},"required":["path"]}`,
	ManifestDiffTool:   `{"type":"object","additionalProperties":false,"properties":{"before":{"type":"string","minLength":1},"after":{"type":"string","minLength":1},"context_lines":{"type":"integer","minimum":0,"maximum":10,"default":3},"max_resources":{"type":"integer","minimum":1,"maximum":200,"default":50},"redact_secrets":{"type":"boolean","default":true}},"required":["before","after"]}`,
}

func TestIntelligenceSchemasMatchDesign(t *testing.T) {
	cwd := t.TempDir()
	all := AllTools(cwd, ToolsOptions{LSP: newInertLSPManager(t, cwd)})
	for name, want := range intelligenceToolSchemas {
		tool, ok := all[name]
		if !ok {
			t.Fatalf("tool %s not built", name)
		}
		assertSchemaParity(t, string(name), tool.Parameters, want)
	}
}

// coordinationToolSchemas are the message tool and the job tools of a durable
// root session, where bash gains async and service arguments.
var coordinationToolSchemas = map[ToolName]string{
	MessageTool:    `{"type":"object","properties":{"to":{"type":"string","description":"\"parent\", a running child's job id, or \"all\""},"body":{"type":"string","description":"message text, at most 8 KiB"},"await_reply_ms":{"type":"integer","minimum":0,"maximum":300000,"description":"wait this long for the recipient's first message back; 0 or absent returns at once"}},"required":["to","body"]}`,
	BashTool:       `{"type":"object","properties":{"command":{"type":"string"},"timeout":{"type":"number"},"async":{"type":"boolean","description":"true runs the command as an unnamed background job; name, pty, stdin, ready_log, ready_port and ready_timeout_ms already imply it"},"name":{"type":"string","description":"runs the command as a background job with this name, unique among running jobs, and returns at once; wait, cancel, logs and write_stdin accept the name in place of the job id"},"pty":{"type":"boolean","description":"runs the command as a background job on a pseudo-terminal (24x80, TERM=dumb): it sees a terminal, as REPLs and prompting tools need; stdout and stderr are one stream; drive it with write_stdin"},"stdin":{"type":"boolean","description":"runs the command as a background job and keeps stdin open so write_stdin can feed this job; without it (or pty) a background job's stdin is /dev/null"},"ready_log":{"type":"string","description":"runs the command as a background service: RE2 regex; the call returns once an output line matches"},"ready_port":{"type":"integer","minimum":1,"maximum":65535,"description":"runs the command as a background service: the call returns once 127.0.0.1:<port> accepts TCP connections"},"ready_timeout_ms":{"type":"integer","minimum":1,"maximum":300000,"description":"readiness deadline for ready_log/ready_port, default 30000; on timeout the process is killed"}},"required":["command"]}`,
	WaitTool:       `{"type":"object","properties":{"job_ids":{"type":"array","items":{"type":"string"},"minItems":1,"maxItems":8,"uniqueItems":true},"names":{"type":"array","items":{"type":"string"},"minItems":1,"maxItems":8,"uniqueItems":true,"description":"background bash job names, in place of or besides job_ids"},"timeout":{"type":"number"}},"required":[]}`,
	CancelTool:     `{"type":"object","properties":{"job_id":{"type":"string"},"name":{"type":"string","description":"a background bash job's name, in place of job_id"}},"required":[]}`,
	LogsTool:       `{"type":"object","properties":{"job_id":{"type":"string"},"name":{"type":"string","description":"a background bash job's name, in place of job_id"},"offset":{"type":"number"},"limit":{"type":"number"}},"required":[]}`,
	WriteStdinTool: `{"type":"object","properties":{"job_id":{"type":"string"},"name":{"type":"string","description":"a background bash job's name, in place of job_id"},"text":{"type":"string","description":"bytes to write; empty polls for output"},"close_stdin":{"type":"boolean","description":"end the job's input after text"},"wait_ms":{"type":"integer","minimum":0,"maximum":30000,"description":"longest wait for output, default 2000"}},"required":[]}`,
}

func TestCoordinationSchemas(t *testing.T) {
	bus := irc.New(nil, nil)
	all := AllTools(t.TempDir(), ToolsOptions{Work: &serviceController{}, Messenger: bus.Register(irc.Parent, &memoryMailbox{})})
	for name, want := range coordinationToolSchemas {
		tool, ok := all[name]
		if !ok {
			t.Fatalf("tool %s not built", name)
		}
		assertSchemaParity(t, string(name), tool.Parameters, want)
	}
}

func TestEvalSchemaParity(t *testing.T) {
	tool := newEvalTool(nil)
	assertSchemaParity(t, "eval", tool.Parameters, `{"type":"object","properties":{"language":{"type":"string","enum":["py","js"]},"code":{"type":"string"},"timeout":{"type":"number","minimum":0,"maximum":2147483,"default":30},"reset":{"type":"boolean"}},"required":["language","code"]}`)
}

func TestCodingExtensionSchemaParity(t *testing.T) {
	bin := t.TempDir()
	codingFakeBinary(t, bin, "ast-grep", "printf '[]'\n")
	t.Setenv("PATH", bin)
	all := AllTools(t.TempDir(), ToolsOptions{})
	fixtures := map[ToolName]string{
		ASTGrepTool:    `{"type":"object","additionalProperties":false,"properties":{"pat":{"type":"string"},"path":{"type":"string"},"skip":{"type":"integer","minimum":0},"lang":{"type":"string"}},"required":["pat"]}`,
		ASTEditTool:    `{"type":"object","additionalProperties":false,"properties":{"ops":{"type":"array","minItems":1,"items":{"type":"object","additionalProperties":false,"properties":{"pat":{"type":"string"},"out":{"type":"string"}},"required":["pat","out"]}},"paths":{"type":"array","minItems":1,"items":{"type":"string"}},"lang":{"type":"string"},"apply":{"type":"boolean","default":false},"preview":{"type":"string"}},"required":["ops","paths"]}`,
		GHTool:         `{"type":"object","additionalProperties":false,"properties":{"op":{"type":"string","enum":["issue_view","pr_view","pr_diff","repo_view","file_read","search_issues","search_prs","search_code","search_commits","search_repos","run_watch","run_logs"]},"repo":{"type":"string"},"number":{"type":"string"},"query":{"type":"string"},"path":{"type":"string"},"branch":{"type":"string"},"run":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":100},"timeout":{"type":"integer","minimum":1,"maximum":300}},"required":["op"]}`,
		GHCheckoutTool: `{"type":"object","additionalProperties":false,"properties":{"repo":{"type":"string"},"number":{"type":"string"},"path":{"type":"string"}},"required":["number","path"]}`,
	}
	for name, want := range fixtures {
		assertSchemaParity(t, string(name), all[name].Parameters, want)
	}
}
