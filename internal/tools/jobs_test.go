package tools

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/document"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/skills"
	"go.harness.dev/harness/internal/subagent"
	"go.harness.dev/harness/internal/work"
)

type managementController struct {
	work.Controller
	bash   work.BashSpec
	action string
}

func (controller *managementController) StartBash(_ context.Context, spec work.BashSpec) (work.Job, error) {
	controller.bash = spec
	return work.Job{ID: "job_test", Kind: "bash", State: "starting"}, nil
}
func (controller *managementController) Jobs(context.Context, string, int) (work.JobPage, error) {
	controller.action = "list"
	return work.JobPage{Jobs: []work.Job{}}, nil
}
func (controller *managementController) Cleanup(context.Context, string) (work.Job, error) {
	controller.action = "cleanup"
	return work.Job{ID: "job_test", State: "completed"}, nil
}
func (controller *managementController) Wait(context.Context, []string, time.Duration) ([]work.Job, error) {
	controller.action = "wait"
	return []work.Job{{ID: "job_test", State: "running"}}, nil
}
func (controller *managementController) Cancel(context.Context, string) (work.Job, error) {
	controller.action = "cancel"
	return work.Job{ID: "job_test", State: "running"}, nil
}
func (controller *managementController) Logs(context.Context, string, int64, int) (work.LogPage, error) {
	controller.action = "logs"
	return work.LogPage{Text: "tail"}, nil
}
func TestAsyncBashUsesControllerAndChildRejects(t *testing.T) {
	controller := &managementController{}
	tool := newBashTool(t.TempDir(), ToolsOptions{Work: controller, BashTimeout: time.Minute})
	result, err := tool.Execute(context.Background(), "raw", json.RawMessage(`{"command":"echo test","async":true}`), nil)
	if err != nil || result.Terminate || controller.bash.Command != "echo test" || controller.bash.TimeoutSeconds != 60 || !strings.Contains(string(result.Details), "job_test") {
		t.Fatal(result, controller.bash, err)
	}
	child := newBashTool(t.TempDir(), ToolsOptions{Work: controller, Child: true})
	if strings.Contains(string(child.Parameters), `"async"`) {
		t.Fatal(string(child.Parameters))
	}
	if _, err = child.Execute(context.Background(), "raw", json.RawMessage(`{"command":"echo test","async":true}`), nil); err == nil {
		t.Fatal("child ran async")
	}
}
func TestManagementRegistryCannotBeShadowed(t *testing.T) {
	controller := &managementController{}
	registry := AllTools(t.TempDir(), ToolsOptions{Work: controller})
	for _, name := range []ToolName{JobsTool, WaitTool, CancelTool, LogsTool} {
		if _, ok := registry[name]; !ok || !AllToolNames[name] {
			t.Fatal(name)
		}
	}
	for _, name := range []ToolName{JobsTool, WaitTool, CancelTool, LogsTool} {
		if _, err := NewTool(name, t.TempDir(), ToolsOptions{Work: controller, Child: true}); err == nil {
			t.Fatal("child management tool", name)
		}
	}
}
func TestManagementActionsAndObservationalWait(t *testing.T) {
	controller := &managementController{}
	for _, test := range []struct {
		name         ToolName
		args, action string
	}{{JobsTool, `{}`, "list"}, {JobsTool, `{"action":"cleanup","job_id":"job_test"}`, "cleanup"}, {WaitTool, `{"job_ids":["job_test"],"timeout":0.001}`, "wait"}, {CancelTool, `{"job_id":"job_test"}`, "cancel"}, {LogsTool, `{"job_id":"job_test"}`, "logs"}} {
		tool := newManagementTool(test.name, controller)
		result, err := tool.Execute(context.Background(), "raw", json.RawMessage(test.args), nil)
		if err != nil || controller.action != test.action || result.Terminate {
			t.Fatal(result, controller.action, err)
		}
		if test.name == WaitTool && !strings.Contains(string(result.Details), `"timed_out":true`) {
			t.Fatal(result)
		}
	}
	for _, args := range []string{`{"action":"destroy"}`, `{"action":"cleanup"}`, `{"action":5}`} {
		if _, err := JobsAction(json.RawMessage(args)); err == nil {
			t.Fatal(args)
		}
	}
}

// populatedController returns jobs with every optional field set, so a
// camelCase key anywhere in a rendered job shows up.
type populatedController struct{ work.Controller }

func populatedJob() work.Job {
	exit, reasoning := 1, 2
	artifact := &work.Artifact{Path: "/tmp/a", SHA256: "x", Bytes: 1}
	return work.Job{ID: "job_test", Kind: "task", State: "completed", RunID: "run", LogicalRunID: "run", ToolCallID: "call", ChildSessionID: "child", Description: "d", StartedAt: time.Unix(1, 0), FinishedAt: time.Unix(2, 0), Cwd: "/w", Isolation: "worktree", BaseCommit: "abc", ResultText: "r", Result: artifact, Patch: artifact, Output: artifact, Value: json.RawMessage(`1`), ValueInResult: true, Usage: types.Usage{Input: 1, Reasoning: &reasoning, Cost: types.Cost{Unknown: true}}, ExitCode: &exit, Reason: "r", Cleanup: "retained"}
}
func (populatedController) StartTask(context.Context, work.TaskSpec) (work.Job, error) {
	return populatedJob(), nil
}
func (populatedController) StartBash(context.Context, work.BashSpec) (work.Job, error) {
	return populatedJob(), nil
}
func (populatedController) Jobs(context.Context, string, int) (work.JobPage, error) {
	return work.JobPage{Jobs: []work.Job{populatedJob()}, NextCursor: "job_test"}, nil
}
func (populatedController) Cleanup(context.Context, string) (work.Job, error) {
	return populatedJob(), nil
}
func (populatedController) Wait(context.Context, []string, time.Duration) ([]work.Job, error) {
	return []work.Job{populatedJob()}, nil
}
func (populatedController) Cancel(context.Context, string) (work.Job, error) {
	return populatedJob(), nil
}
func (populatedController) Logs(context.Context, string, int64, int) (work.LogPage, error) {
	return work.LogPage{Text: "t", Offset: 1, NextOffset: 2, EOF: true, Truncated: true}, nil
}

var snakeKey = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// collectKeys records every object key in a JSON document.
func collectKeys(document json.RawMessage, keys map[string]bool) {
	var object map[string]json.RawMessage
	if json.Unmarshal(document, &object) == nil {
		for key, nested := range object {
			keys[key] = true
			collectKeys(nested, keys)
		}
		return
	}
	var array []json.RawMessage
	if json.Unmarshal(document, &array) == nil {
		for _, nested := range array {
			collectKeys(nested, keys)
		}
	}
}

// TestModelToolSurfaceIsSnakeCase keeps one naming convention on the model
// surface: a job id the model reads back is passed under the same key the
// tools accept, and no argument name mixes in the wire's camelCase.
func TestModelToolSurfaceIsSnakeCase(t *testing.T) {
	controller := populatedController{}
	registry := AllTools(t.TempDir(), ToolsOptions{
		Work: controller, EnableWeb: true, Skills: []skills.Skill{{Name: "x"}}, TaskTool: subagent.NewTool(controller),
		AttachmentRegistry: document.NewAttachmentRegistry(), AttachmentReader: document.NewCacheRootBlobReader(t.TempDir()), Sanitize: func(s string) string { return s },
	})
	for _, name := range []ToolName{TaskTool, AttachmentTool, WebFetch, WebSearch, SkillTool, JobsTool} {
		if _, ok := registry[name]; !ok {
			t.Fatalf("%s not built", name)
		}
	}
	for name, tool := range registry {
		for _, property := range schemaProperties(tool.Parameters) {
			if !snakeKey.MatchString(property) {
				t.Errorf("%s argument %q is not snake_case", name, property)
			}
		}
	}
	for _, call := range []struct {
		name ToolName
		args string
	}{{TaskTool, `{"prompt":"p","subagent_type":"general"}`}, {BashTool, `{"command":"true","async":true}`}, {JobsTool, `{}`}, {JobsTool, `{"action":"cleanup","job_id":"job_test"}`}, {WaitTool, `{"job_ids":["job_test"]}`}, {CancelTool, `{"job_id":"job_test"}`}, {LogsTool, `{"job_id":"job_test"}`}} {
		result, err := registry[call.name].Execute(context.Background(), "call", json.RawMessage(call.args), nil)
		if err != nil {
			t.Fatal(call.name, err)
		}
		document := json.RawMessage(textOf(result))
		if !json.Valid(document) {
			t.Fatalf("%s result is not JSON: %s", call.name, document)
		}
		keys := map[string]bool{}
		collectKeys(document, keys)
		for key := range keys {
			if !snakeKey.MatchString(key) {
				t.Errorf("%s result key %q is not snake_case", call.name, key)
			}
		}
		if call.name != LogsTool && !keys["job_id"] {
			t.Errorf("%s result has no job_id for wait/cancel/logs: %s", call.name, textOf(result))
		}
	}
}

// schemaProperties lists every property name declared anywhere in a JSON Schema.
func schemaProperties(schemaDocument json.RawMessage) []string {
	var node struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Items      json.RawMessage            `json:"items"`
	}
	if json.Unmarshal(schemaDocument, &node) != nil {
		return nil
	}
	var names []string
	for name, nested := range node.Properties {
		names = append(names, name)
		names = append(names, schemaProperties(nested)...)
	}
	if len(node.Items) > 0 {
		names = append(names, schemaProperties(node.Items)...)
	}
	return names
}

func textOf(result agent.AgentToolResult) string {
	var text strings.Builder
	for _, block := range result.Content {
		text.WriteString(block.Text)
	}
	return text.String()
}
