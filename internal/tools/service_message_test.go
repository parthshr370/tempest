package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/irc"
	"go.harness.dev/harness/internal/work"
)

// serviceController resolves "web" to job_web and records the ids each
// management call received.
type serviceController struct {
	work.Controller
	bash    work.BashSpec
	targets []string
}

var errNoSuchService = errors.New("no such service")

func (controller *serviceController) StartBash(_ context.Context, spec work.BashSpec) (work.Job, error) {
	controller.bash = spec
	return work.Job{ID: "job_web", Kind: "bash", State: "running", Name: spec.Name, Readiness: "ready", ReadyMatch: "listening"}, nil
}
func (controller *serviceController) JobByName(_ context.Context, name string) (work.Job, error) {
	if name != "web" {
		return work.Job{}, errNoSuchService
	}
	return work.Job{ID: "job_web", Name: name}, nil
}
func (controller *serviceController) Cancel(_ context.Context, jobID string) (work.Job, error) {
	controller.targets = append(controller.targets, jobID)
	return work.Job{ID: jobID, State: "cancelled"}, nil
}
func (controller *serviceController) Logs(_ context.Context, jobID string, _ int64, _ int) (work.LogPage, error) {
	controller.targets = append(controller.targets, jobID)
	return work.LogPage{Text: "out"}, nil
}
func (controller *serviceController) Wait(_ context.Context, jobIDs []string, _ time.Duration) ([]work.Job, error) {
	controller.targets = append(controller.targets, jobIDs...)
	return []work.Job{{ID: jobIDs[0], State: "running"}}, nil
}

func TestAsyncBashServiceArguments(t *testing.T) {
	controller := &serviceController{}
	tool := newBashTool(t.TempDir(), ToolsOptions{Work: controller, BashTimeout: time.Minute})
	result, err := tool.Execute(context.Background(), "raw", json.RawMessage(`{"command":"serve","async":true,"name":"web","ready_log":"listening","ready_port":8080,"ready_timeout_ms":5000}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := work.BashSpec{Command: "serve", TimeoutSeconds: maxTimeoutSeconds, Name: "web", ReadyLog: "listening", ReadyPort: 8080, ReadyTimeoutMS: 5000}
	if controller.bash != want {
		t.Fatalf("spec=%+v want %+v", controller.bash, want)
	}
	if text := textOf(result); !strings.Contains(text, `"name":"web"`) || !strings.Contains(text, `"readiness":"ready"`) || !strings.Contains(text, `"ready_match":"listening"`) {
		t.Fatalf("handle=%s", text)
	}
	if _, err = tool.Execute(context.Background(), "raw", json.RawMessage(`{"command":"serve","async":true,"ready_port":8080,"timeout":30}`), nil); err != nil || controller.bash.TimeoutSeconds != 30 {
		t.Fatalf("explicit timeout=%d %v", controller.bash.TimeoutSeconds, err)
	}
	if _, err = tool.Execute(context.Background(), "raw", json.RawMessage(`{"command":"serve","async":true,"name":"plain"}`), nil); err != nil || controller.bash.TimeoutSeconds != 60 {
		t.Fatalf("a name alone is not a service: timeout=%d %v", controller.bash.TimeoutSeconds, err)
	}
	if _, err = tool.Execute(context.Background(), "raw", json.RawMessage(`{"command":"true","name":"web"}`), nil); err == nil || !strings.Contains(err.Error(), "async:true") {
		t.Fatalf("synchronous service=%v", err)
	}
}

func TestManagementToolsAcceptServiceNames(t *testing.T) {
	for _, test := range []struct {
		tool    ToolName
		args    string
		targets string
		failure string
	}{
		{CancelTool, `{"name":"web"}`, "job_web", ""},
		{LogsTool, `{"name":"web"}`, "job_web", ""},
		{WaitTool, `{"names":["web"],"job_ids":["job_other"],"timeout":0}`, "job_other,job_web", ""},
		{CancelTool, `{"name":"web","job_id":"job_web"}`, "", "not both"},
		{CancelTool, `{}`, "", "job_id or name is required"},
		{LogsTool, `{"name":"missing"}`, "", errNoSuchService.Error()},
		{WaitTool, `{}`, "", "job_ids or names"},
	} {
		controller := &serviceController{}
		_, err := newManagementTool(test.tool, controller).Execute(context.Background(), "raw", json.RawMessage(test.args), nil)
		if test.failure != "" {
			if err == nil || !strings.Contains(err.Error(), test.failure) {
				t.Errorf("%s %s: %v, want %q", test.tool, test.args, err, test.failure)
			}
			continue
		}
		if err != nil || strings.Join(controller.targets, ",") != test.targets {
			t.Errorf("%s %s: targets %v err %v, want %s", test.tool, test.args, controller.targets, err, test.targets)
		}
	}
}

// memoryMailbox is a non-durable mailbox for exercising the tool surface.
type memoryMailbox struct {
	mu      sync.Mutex
	pending []work.QueueItem
	changed chan struct{}
}

func (mailbox *memoryMailbox) Enqueue(_ context.Context, input work.QueueInput) (work.QueueItem, error) {
	mailbox.mu.Lock()
	defer mailbox.mu.Unlock()
	queued := work.QueueItem{ID: fmt.Sprint("q", len(mailbox.pending)), Kind: input.Kind, Text: input.Text, Key: input.Key, SourceAgent: input.SourceAgent}
	mailbox.pending = append(mailbox.pending, queued)
	return queued, nil
}
func (mailbox *memoryMailbox) Cancel(context.Context, string) error { return nil }
func (mailbox *memoryMailbox) Pending() (bool, <-chan struct{}) {
	mailbox.mu.Lock()
	defer mailbox.mu.Unlock()
	return len(mailbox.pending) > 0, mailbox.changed
}

func TestMessageToolSurface(t *testing.T) {
	bus := irc.New(nil, nil)
	parent := bus.Register(irc.Parent, &memoryMailbox{})
	child := &memoryMailbox{}
	bus.Register("job_a", child)
	registry := AllTools(t.TempDir(), ToolsOptions{Messenger: parent})
	tool, ok := registry[MessageTool]
	if !ok {
		t.Fatal("message tool not registered with a messenger")
	}
	if _, ok := AllTools(t.TempDir(), ToolsOptions{})[MessageTool]; ok {
		t.Fatal("message tool registered without a session tree")
	}
	for _, property := range schemaProperties(tool.Parameters) {
		if !snakeKey.MatchString(property) {
			t.Errorf("message argument %q is not snake_case", property)
		}
	}
	result, err := tool.Execute(context.Background(), "raw", json.RawMessage(`{"to":"job_a","body":"rebase first"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	collectKeys(json.RawMessage(textOf(result)), keys)
	for key := range keys {
		if !snakeKey.MatchString(key) {
			t.Errorf("message result key %q is not snake_case", key)
		}
	}
	if !keys["message_id"] || !keys["delivered_to"] || len(child.pending) != 1 {
		t.Fatalf("result=%s pending=%d", textOf(result), len(child.pending))
	}
	for args, want := range map[string]string{
		`{"to":"job_a","body":"x","await_reply_ms":-1}`:     "await_reply_ms",
		`{"to":"job_a","body":"x","await_reply_ms":300001}`: "await_reply_ms",
		`{"to":"job_missing","body":"x"}`:                   "Running peers: job_a",
		`{"to":"all","body":"x","await_reply_ms":1000}`:     "broadcast",
	} {
		if _, err := tool.Execute(context.Background(), "raw", json.RawMessage(args), nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", args, err, want)
		}
	}
}
