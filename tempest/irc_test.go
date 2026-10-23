package tempest

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/stream"
	types "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
)

var childSender = regexp.MustCompile(`\[Message from (job_[0-9a-f]+) \(child\)\]`)

// rootParkDelay lets a root that just ended its turn reach its idle wait on
// live children before a child's message arrives.
const rootParkDelay = 200 * time.Millisecond

// transcript is the provider-visible context as one searchable string.
func transcript(t *testing.T, input types.Context) string {
	t.Helper()
	encoded, err := json.Marshal(input.Messages)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func respondWith(blocks ...types.ContentBlock) types.AssistantMessage {
	return types.AssistantMessage{Content: blocks}
}

type messagingTree struct {
	owner   *Session
	updates <-chan Update
	stop    func()
}

// openTree opens a root whose provider calls go to root and whose task
// children's calls go to child.
func openTree(t *testing.T, root, child *faux.Faux) messagingTree {
	t.Helper()
	dispatch := func(ctx context.Context, model types.Model, input types.Context, options *types.SimpleStreamOptions) *stream.AssistantStream {
		if strings.Contains(input.SystemPrompt, "You are a delegated child.") {
			return child.StreamSimple(ctx, model, input, options)
		}
		return root.StreamSimple(ctx, model, input, options)
	}
	owner, err := Open(t.Context(), Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &NewSession{PermissionMode: permission.ModeBypass}, Stack: &StackConfig{Model: root.Model(), StreamFn: dispatch, EnableTask: true}})
	if err != nil {
		t.Fatal(err)
	}
	_, updates, stop := owner.Watch(t.Context(), WatchOptions{Buffer: 1024})
	t.Cleanup(func() {
		stop()
		if err := owner.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return messagingTree{owner: owner, updates: updates, stop: stop}
}

// finish runs the root prompt and returns its final text and the one child job's result.
func (tree messagingTree) finish(t *testing.T) (string, string) {
	t.Helper()
	run, err := tree.owner.Begin(t.Context(), Prompt{Text: "coordinate"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := run.Wait()
	if err != nil || result.StopReason != agent.StopCompleted {
		t.Fatalf("root run=%+v %v", result, err)
	}
	page, err := tree.owner.Jobs(t.Context(), "", 20)
	if err != nil || len(page.Jobs) != 1 {
		t.Fatalf("jobs=%+v %v", page, err)
	}
	jobs, err := tree.owner.WaitJobs(t.Context(), []string{page.Jobs[0].ID}, 10*time.Second)
	if err != nil || len(jobs) != 1 || jobs[0].State != "completed" {
		t.Fatalf("child job=%+v %v", jobs, err)
	}
	return run.Text(), jobs[0].ResultText
}

// messageUpdates drains the buffered stream's tempest/message payloads.
func (tree messagingTree) messageUpdates(t *testing.T) []string {
	t.Helper()
	var found []string
	for {
		select {
		case update := <-tree.updates:
			if update.Kind == "tempest/message" {
				found = append(found, string(update.Update))
			}
		default:
			return found
		}
	}
}

func taskCall() types.ContentBlock {
	return faux.ToolCall("task", json.RawMessage(`{"prompt":"delegated work","subagent_type":"explore"}`), "task_call")
}

// The child asks while the parent is idle waiting on it; the parent is
// woken, replies, and the child's awaiting message call returns the reply.
func TestChildMessageWakesIdleParentAndAwaitsItsReply(t *testing.T) {
	rootIdle := make(chan struct{})
	root, child := faux.New(faux.Options{}), faux.New(faux.Options{})
	root.SetResponses(
		faux.Respond(taskCall()),
		func(types.Context, *types.StreamOptions, faux.State, types.Model) (types.AssistantMessage, error) {
			close(rootIdle)
			return respondWith(faux.Text("waiting for the child")), nil
		},
		func(input types.Context, _ *types.StreamOptions, _ faux.State, _ types.Model) (types.AssistantMessage, error) {
			match := childSender.FindStringSubmatch(transcript(t, input))
			if match == nil || !strings.Contains(transcript(t, input), "which port should I use?") {
				return types.AssistantMessage{}, fmt.Errorf("parent was not woken by the child's message: %s", transcript(t, input))
			}
			return respondWith(faux.ToolCall("message", json.RawMessage(fmt.Sprintf(`{"to":%q,"body":"use port 8080"}`, match[1])), "reply_call")), nil
		},
		faux.Respond(faux.Text("replied")),
		faux.Respond(faux.Text("root final")),
	)
	child.SetResponses(
		func(types.Context, *types.StreamOptions, faux.State, types.Model) (types.AssistantMessage, error) {
			// Ask once the root has ended its turn and parked on its live child.
			select {
			case <-rootIdle:
			case <-time.After(10 * time.Second):
				return types.AssistantMessage{}, fmt.Errorf("root never went idle")
			}
			time.Sleep(rootParkDelay)
			return respondWith(faux.ToolCall("message", json.RawMessage(`{"to":"parent","body":"which port should I use?","await_reply_ms":20000}`), "ask_call")), nil
		},
		func(input types.Context, _ *types.StreamOptions, _ faux.State, _ types.Model) (types.AssistantMessage, error) {
			text := transcript(t, input)
			if !strings.Contains(text, `use port 8080`) || !strings.Contains(text, `reply`) {
				return types.AssistantMessage{}, fmt.Errorf("child did not get the reply in its tool result: %s", text)
			}
			if strings.Contains(text, "[Message from parent]") {
				return types.AssistantMessage{}, fmt.Errorf("awaited reply also arrived as an aside: %s", text)
			}
			return respondWith(faux.Text("CHILD DONE on 8080")), nil
		},
	)
	tree := openTree(t, root, child)
	final, childResult := tree.finish(t)
	if final != "root final" || !strings.Contains(childResult, "CHILD DONE on 8080") {
		t.Fatalf("root=%q child=%q", final, childResult)
	}
	updates := strings.Join(tree.messageUpdates(t), "\n")
	if !strings.Contains(updates, `"to":"parent"`) || !strings.Contains(updates, `"delivery":"mailbox"`) || !strings.Contains(updates, `"from":"parent"`) || !strings.Contains(updates, `"delivery":"reply"`) {
		t.Fatalf("message updates=%s", updates)
	}
}

// The parent messages a child that is busy in a provider call. The message
// reaches the child at its next step boundary: after a tool step, or after
// what would have been its final answer, in which case it takes another turn.
func TestParentMessageReachesBusyChildAtNextBoundary(t *testing.T) {
	for name, firstStep := range map[string]types.ContentBlock{
		"after a tool step":    faux.ToolCall("ls", json.RawMessage(`{}`), "ls_call"),
		"instead of finishing": faux.Text("finished without the news"),
	} {
		t.Run(name, func(t *testing.T) {
			childBusy, delivered := make(chan struct{}), make(chan struct{})
			var deliveredOnce sync.Once
			defer deliveredOnce.Do(func() { close(delivered) })
			root, child := faux.New(faux.Options{}), faux.New(faux.Options{})
			root.SetResponses(
				faux.Respond(taskCall()),
				func(input types.Context, _ *types.StreamOptions, _ faux.State, _ types.Model) (types.AssistantMessage, error) {
					select {
					case <-childBusy:
					case <-time.After(10 * time.Second):
						return types.AssistantMessage{}, fmt.Errorf("child never started")
					}
					match := regexp.MustCompile(`job_[0-9a-f]{32}`).FindString(transcript(t, input))
					if match == "" {
						return types.AssistantMessage{}, fmt.Errorf("no job handle in %s", transcript(t, input))
					}
					return respondWith(faux.ToolCall("message", json.RawMessage(fmt.Sprintf(`{"to":%q,"body":"switch to plan B"}`, match)), "news_call")), nil
				},
				func(types.Context, *types.StreamOptions, faux.State, types.Model) (types.AssistantMessage, error) {
					deliveredOnce.Do(func() { close(delivered) })
					return respondWith(faux.Text("sent")), nil
				},
				faux.Respond(faux.Text("root final")),
			)
			child.SetResponses(
				func(input types.Context, _ *types.StreamOptions, _ faux.State, _ types.Model) (types.AssistantMessage, error) {
					// Busy: the parent's message lands while this call is in flight.
					close(childBusy)
					select {
					case <-delivered:
					case <-time.After(10 * time.Second):
						return types.AssistantMessage{}, fmt.Errorf("parent never sent")
					}
					if strings.Contains(transcript(t, input), "switch to plan B") {
						return types.AssistantMessage{}, fmt.Errorf("message arrived before the busy call")
					}
					return respondWith(firstStep), nil
				},
				func(input types.Context, _ *types.StreamOptions, _ faux.State, _ types.Model) (types.AssistantMessage, error) {
					text := transcript(t, input)
					if !strings.Contains(text, "[Message from parent]") || !strings.Contains(text, "switch to plan B") {
						return types.AssistantMessage{}, fmt.Errorf("child never read the message: %s", text)
					}
					return respondWith(faux.Text("CHILD ON PLAN B")), nil
				},
			)
			tree := openTree(t, root, child)
			final, childResult := tree.finish(t)
			if final != "root final" || childResult != "CHILD ON PLAN B" {
				t.Fatalf("root=%q child=%q", final, childResult)
			}
		})
	}
}

// The child reports while the parent is busy in a provider call; the
// parent reads it at its next step boundary, before the completion notice.
func TestChildMessageReachesBusyParentAtNextBoundary(t *testing.T) {
	rootBusy, childSent := make(chan struct{}), make(chan struct{})
	root, child := faux.New(faux.Options{}), faux.New(faux.Options{})
	root.SetResponses(
		faux.Respond(taskCall()),
		func(input types.Context, _ *types.StreamOptions, _ faux.State, _ types.Model) (types.AssistantMessage, error) {
			close(rootBusy)
			select {
			case <-childSent:
			case <-time.After(10 * time.Second):
				return types.AssistantMessage{}, fmt.Errorf("child never sent")
			}
			if strings.Contains(transcript(t, input), "halfway there") {
				return types.AssistantMessage{}, fmt.Errorf("message arrived before the busy call")
			}
			return respondWith(faux.ToolCall("ls", json.RawMessage(`{}`), "ls_call")), nil
		},
		func(input types.Context, _ *types.StreamOptions, _ faux.State, _ types.Model) (types.AssistantMessage, error) {
			text := transcript(t, input)
			if childSender.FindString(text) == "" || !strings.Contains(text, "halfway there") {
				return types.AssistantMessage{}, fmt.Errorf("parent never read the progress message: %s", text)
			}
			return respondWith(faux.Text("noted")), nil
		},
		faux.Respond(faux.Text("root final")),
	)
	child.SetResponses(
		func(types.Context, *types.StreamOptions, faux.State, types.Model) (types.AssistantMessage, error) {
			select {
			case <-rootBusy:
			case <-time.After(10 * time.Second):
				return types.AssistantMessage{}, fmt.Errorf("root never got busy")
			}
			return respondWith(faux.ToolCall("message", json.RawMessage(`{"to":"parent","body":"halfway there"}`), "progress_call")), nil
		},
		func(input types.Context, _ *types.StreamOptions, _ faux.State, _ types.Model) (types.AssistantMessage, error) {
			if !strings.Contains(transcript(t, input), `delivered_to`) {
				return types.AssistantMessage{}, fmt.Errorf("send receipt missing: %s", transcript(t, input))
			}
			close(childSent)
			return respondWith(faux.Text("child done")), nil
		},
	)
	tree := openTree(t, root, child)
	final, childResult := tree.finish(t)
	if final != "root final" || childResult != "child done" {
		t.Fatalf("root=%q child=%q", final, childResult)
	}
}
