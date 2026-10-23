package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/work"
)

type taskController struct {
	work.Controller
	spec work.TaskSpec
	err  error
}

func (controller *taskController) StartTask(_ context.Context, spec work.TaskSpec) (work.Job, error) {
	controller.spec = spec
	return work.Job{ID: "job_test", Kind: "task", State: "starting", ChildSessionID: "child"}, controller.err
}
func TestTaskReturnsDurableHandleWithoutTerminating(t *testing.T) {
	controller := &taskController{}
	tool := NewTool(controller)
	result, err := tool.Execute(context.Background(), "raw", json.RawMessage(`{"prompt":"work","subagent_type":"general","output_schema":{"type":"integer"}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Terminate || !strings.Contains(string(result.Details), `"job_id":"job_test"`) || result.Content[0].Text != string(result.Details) {
		t.Fatal(result)
	}
	if controller.spec.Prompt != "work" || controller.spec.SubagentType != "general" || string(controller.spec.OutputSchema) != `{"type":"integer"}` {
		t.Fatal(controller.spec)
	}
	var parameters struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err = json.Unmarshal(tool.Parameters, &parameters); err != nil {
		t.Fatal(err)
	}
	if _, ok := parameters.Properties["output_schema"]; !ok {
		t.Fatal("missing schema")
	}
}
func TestTaskRequiresDurableHostAndPropagatesAdmissionFailure(t *testing.T) {
	tool := NewTool(nil)
	if _, err := tool.Execute(context.Background(), "raw", json.RawMessage(`{"prompt":"work"}`), nil); err == nil {
		t.Fatal("raw child permitted")
	}
	failure := errors.New("job_limit")
	tool = NewTool(&taskController{err: failure})
	if _, err := tool.Execute(context.Background(), "raw", json.RawMessage(`{"prompt":"work"}`), nil); !errors.Is(err, failure) {
		t.Fatal(err)
	}
}
func TestRegistryListing(t *testing.T) {
	listing := RegistryListing(Registry{"general": {Description: "Writer"}, "explore": {Description: "Reader"}})
	for _, want := range []string{"<available_subagents>", "- explore: Reader", "- general: Writer", "general-purpose"} {
		if !strings.Contains(listing, want) {
			t.Fatal(listing)
		}
	}
	if strings.Index(listing, "explore") > strings.Index(listing, "general:") {
		t.Fatal("not sorted")
	}
	if RegistryListing(nil) != "" {
		t.Fatal("empty listing")
	}
}
