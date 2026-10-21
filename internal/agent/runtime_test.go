package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
)

type customRuntimeMessage struct{ role string }

func (m customRuntimeMessage) Role() string { return m.role }

func newRuntimeAgent(f *faux.Faux, mutate ...func(*AgentOptions)) *Agent {
	opts := AgentOptions{
		InitialState: &AgentState{Model: f.Model()},
		ConvertToLlm: func(messages []types.Message) []types.Message {
			return messages
		},
		StreamFn: func(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream {
			return f.StreamSimple(ctx, model, c, opts)
		},
	}
	for _, fn := range mutate {
		fn(&opts)
	}
	return NewAgent(opts)
}

func waitRuntimeEvent(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for runtime event")
	}
}

func waitRuntimeErr(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for runtime result")
		return nil
	}
}

func runtimeRoles(messages []types.Message) string {
	roles := make([]string, 0, len(messages))
	for _, message := range messages {
		roles = append(roles, message.Role())
	}
	return strings.Join(roles, ",")
}

func TestDefaultConvertToLlmFiltersKnownTranscriptRoles(t *testing.T) {
	user := types.UserMessage{Content: types.StringContent("hi")}
	assistant := types.AssistantMessage{Content: []types.ContentBlock{types.NewText("hello")}}
	toolResult := types.ToolResultMessage{ToolCallID: "tc1", ToolName: "read"}
	custom := customRuntimeMessage{role: "custom"}

	got := defaultConvertToLlm([]types.Message{user, custom, assistant, toolResult})

	if len(got) != 3 || got[0].Role() != "user" || got[1].Role() != "assistant" || got[2].Role() != "toolResult" {
		t.Fatalf("converted = %#v, want user/assistant/toolResult", got)
	}
}

func TestAgentPromptTextSingleTurnUpdatesStateAndNotifiesListeners(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("hi")))
	a := newRuntimeAgent(f)

	var mu sync.Mutex
	events := []AgentEventType{}
	unsubscribe := a.Subscribe(func(ctx context.Context, ev AgentEvent) error {
		_ = a.State()
		mu.Lock()
		events = append(events, ev.Type)
		mu.Unlock()
		return nil
	})
	defer unsubscribe()

	result, err := a.PromptText(context.Background(), "hello")
	if err != nil {
		t.Fatalf("PromptText error: %v", err)
	}
	if result.StopReason != StopCompleted || result.Turns != 1 {
		t.Fatalf("result = %+v, want completed after 1 turn", result)
	}

	state := a.State()
	if state.IsStreaming || state.StreamingMessage != nil || len(state.PendingToolCalls) != 0 || state.ErrorMessage != "" {
		t.Fatalf("runtime state after prompt = %#v", state)
	}
	if len(state.Messages) != 2 || state.Messages[0].Role() != "user" {
		t.Fatalf("messages = %#v, want user + assistant", state.Messages)
	}
	if got := assistantText(t, assistantMessageAt(t, state.Messages, 1)); got != "hi" {
		t.Fatalf("assistant text = %q, want hi", got)
	}

	mu.Lock()
	gotEvents := append([]AgentEventType(nil), events...)
	mu.Unlock()
	if len(gotEvents) < 8 || gotEvents[0] != EventAgentStart || gotEvents[1] != EventTurnStart || gotEvents[len(gotEvents)-1] != EventAgentEnd {
		t.Fatalf("events = %#v", gotEvents)
	}
	if countEventsFromTypes(gotEvents, EventMessageUpdate) == 0 {
		t.Fatalf("events = %#v, want message_update", gotEvents)
	}
}

func TestAgentPromptBusyGuard(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(
		faux.Respond(faux.ToolCall("wait", json.RawMessage(`{}`), "tc1")),
		faux.Respond(faux.Text("done")),
	)
	started := make(chan struct{})
	release := make(chan struct{})
	tool := executorTool("wait", `{"type":"object"}`, ExecParallel, func(ctx context.Context, id string, params json.RawMessage, onUpdate AgentToolUpdateCallback) (AgentToolResult, error) {
		close(started)
		select {
		case <-release:
			return AgentToolResult{Content: []types.ContentBlock{types.NewText("released")}}, nil
		case <-ctx.Done():
			return AgentToolResult{}, ctx.Err()
		}
	})
	a := newRuntimeAgent(f, func(opts *AgentOptions) {
		opts.InitialState.Tools = []AgentTool{tool}
	})
	done := make(chan error, 1)

	go func() {
		_, err := a.PromptText(context.Background(), "start")
		done <- err
	}()
	waitRuntimeEvent(t, started)

	if _, err := a.PromptText(context.Background(), "second"); !errors.Is(err, ErrRunActive) {
		t.Fatalf("busy Prompt err = %v, want ErrRunActive", err)
	}
	if _, err := a.Continue(context.Background()); !errors.Is(err, ErrRunActive) {
		t.Fatalf("busy Continue err = %v, want ErrRunActive", err)
	}
	close(release)
	if err := waitRuntimeErr(t, done); err != nil {
		t.Fatalf("first prompt error: %v", err)
	}
}

func TestAgentSteerAndFollowUpQueuesInjectMessages(t *testing.T) {
	f := faux.New(faux.Options{})
	seenRoles := make(chan string, 2)
	f.SetResponses(
		func(c types.Context, opts *types.StreamOptions, state faux.State, model types.Model) (types.AssistantMessage, error) {
			seenRoles <- runtimeRoles(c.Messages)
			return types.AssistantMessage{Content: []types.ContentBlock{types.NewText("first")}, StopReason: types.StopStop}, nil
		},
		func(c types.Context, opts *types.StreamOptions, state faux.State, model types.Model) (types.AssistantMessage, error) {
			seenRoles <- runtimeRoles(c.Messages)
			return types.AssistantMessage{Content: []types.ContentBlock{types.NewText("second")}, StopReason: types.StopStop}, nil
		},
	)
	a := newRuntimeAgent(f)
	a.Steer(userMessage("steer"))
	a.FollowUp(userMessage("follow"))

	if !a.HasQueuedMessages() {
		t.Fatal("HasQueuedMessages = false before prompt")
	}
	if _, err := a.PromptText(context.Background(), "start"); err != nil {
		t.Fatalf("PromptText error: %v", err)
	}

	if got := waitString(t, seenRoles); got != "user,user" {
		t.Fatalf("first provider roles = %q, want user,user", got)
	}
	if got := waitString(t, seenRoles); got != "user,user,assistant,user" {
		t.Fatalf("second provider roles = %q, want user,user,assistant,user", got)
	}
	if a.HasQueuedMessages() {
		t.Fatal("HasQueuedMessages = true after queues drained")
	}
	state := a.State()
	if got := runtimeRoles(state.Messages); got != "user,user,assistant,user,assistant" {
		t.Fatalf("state roles = %q", got)
	}
}

func TestAgentAbortWaitForIdleClearsRuntimeState(t *testing.T) {
	f := faux.New(faux.Options{TokensPerSecond: 1, MinTokenSize: 1, MaxTokenSize: 1})
	f.SetResponses(faux.Respond(faux.Text(strings.Repeat("slow ", 200))))
	a := newRuntimeAgent(f)
	assistantStarted := make(chan struct{})
	var closeStarted sync.Once
	a.Subscribe(func(ctx context.Context, ev AgentEvent) error {
		if ev.Type == EventMessageStart && ev.Message.Role() == "assistant" {
			closeStarted.Do(func() { close(assistantStarted) })
		}
		return nil
	})
	done := make(chan error, 1)

	go func() {
		_, err := a.PromptText(context.Background(), "start")
		done <- err
	}()
	waitRuntimeEvent(t, assistantStarted)
	a.Abort()
	a.WaitForIdle()
	if err := waitRuntimeErr(t, done); err != nil {
		t.Fatalf("PromptText after abort error = %v", err)
	}

	state := a.State()
	if state.IsStreaming || state.StreamingMessage != nil || len(state.PendingToolCalls) != 0 {
		t.Fatalf("runtime state after abort = %#v", state)
	}
	if state.ErrorMessage == "" {
		t.Fatal("ErrorMessage = empty, want aborted error message")
	}
	last := assistantMessageAt(t, state.Messages, len(state.Messages)-1)
	if last.StopReason != types.StopAborted || last.ErrorMessage == "" || state.ErrorMessage != last.ErrorMessage {
		t.Fatalf("last assistant = %#v, want aborted", last)
	}
}

func TestAgentContinueGuardsAndRunsFromLastUser(t *testing.T) {
	f := faux.New(faux.Options{})
	a := newRuntimeAgent(f)

	if _, err := a.Continue(context.Background()); err == nil || err.Error() != "No messages to continue from" {
		t.Fatalf("empty Continue err = %v", err)
	}
	a.SetMessages([]types.Message{types.AssistantMessage{Content: []types.ContentBlock{types.NewText("done")}}})
	if _, err := a.Continue(context.Background()); err == nil || err.Error() != "Cannot continue from message role: assistant" {
		t.Fatalf("assistant Continue err = %v", err)
	}

	f.SetResponses(faux.Respond(faux.Text("continued")))
	a.SetMessages([]types.Message{userMessage("resume")})
	if _, err := a.Continue(context.Background()); err != nil {
		t.Fatalf("Continue error: %v", err)
	}
	state := a.State()
	if len(state.Messages) != 2 || state.Messages[0].Role() != "user" {
		t.Fatalf("messages = %#v, want original user + assistant", state.Messages)
	}
	if got := assistantText(t, assistantMessageAt(t, state.Messages, 1)); got != "continued" {
		t.Fatalf("continued text = %q", got)
	}
}

func TestAgentContinueFromAssistantDrainsQueuedPromptMessages(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("after steer")))
	a := newRuntimeAgent(f)
	a.SetMessages([]types.Message{types.AssistantMessage{Content: []types.ContentBlock{types.NewText("done")}}})
	a.Steer(userMessage("steered"))

	if _, err := a.Continue(context.Background()); err != nil {
		t.Fatalf("Continue with queued steering error: %v", err)
	}
	state := a.State()
	if got := runtimeRoles(state.Messages); got != "assistant,user,assistant" {
		t.Fatalf("state roles = %q, want assistant,user,assistant", got)
	}
	if got := assistantText(t, assistantMessageAt(t, state.Messages, 2)); got != "after steer" {
		t.Fatalf("assistant text = %q", got)
	}
}

func TestAgentResetClearsTranscriptRuntimeStateAndQueues(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("hi")))
	a := newRuntimeAgent(f)
	if _, err := a.PromptText(context.Background(), "hello"); err != nil {
		t.Fatalf("PromptText error: %v", err)
	}
	a.Steer(userMessage("steer"))
	a.FollowUp(userMessage("follow"))

	a.Reset()

	state := a.State()
	if len(state.Messages) != 0 || state.IsStreaming || state.StreamingMessage != nil || len(state.PendingToolCalls) != 0 || state.ErrorMessage != "" {
		t.Fatalf("state after reset = %#v", state)
	}
	if a.HasQueuedMessages() {
		t.Fatal("queues not cleared by Reset")
	}
}

func TestAgentRuntimeConcurrentAccessRaceClean(t *testing.T) {
	f := faux.New(faux.Options{TokensPerSecond: 1, MinTokenSize: 1, MaxTokenSize: 1})
	f.SetResponses(faux.Respond(faux.Text(strings.Repeat("race ", 200))))
	a := newRuntimeAgent(f)
	assistantStarted := make(chan struct{})
	var closeStarted sync.Once
	a.Subscribe(func(ctx context.Context, ev AgentEvent) error {
		if ev.Type == EventMessageStart && ev.Message.Role() == "assistant" {
			closeStarted.Do(func() { close(assistantStarted) })
		}
		return nil
	})
	done := make(chan error, 1)
	stop := make(chan struct{})

	go func() {
		_, err := a.PromptText(context.Background(), "start")
		done <- err
	}()
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				unsubscribe := a.Subscribe(func(ctx context.Context, ev AgentEvent) error { return nil })
				_ = a.State()
				a.Steer(userMessage("steer"))
				a.FollowUp(userMessage("follow"))
				_ = a.HasQueuedMessages()
				unsubscribe()
				time.Sleep(time.Millisecond)
			}
		}
	}()

	waitRuntimeEvent(t, assistantStarted)
	a.Abort()
	a.WaitForIdle()
	close(stop)
	if err := waitRuntimeErr(t, done); err != nil {
		t.Fatalf("PromptText error = %v", err)
	}
}

func countEventsFromTypes(events []AgentEventType, typ AgentEventType) int {
	count := 0
	for _, event := range events {
		if event == typ {
			count++
		}
	}
	return count
}
