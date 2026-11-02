package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
)

// scriptedRequests answers each request with the next scripted message and
// keeps every request it was sent. It keeps the request's messages as sent,
// not a copy: a request that shares the transcript's backing array would
// show the transcript's later appends.
type scriptedRequests struct {
	mu       sync.Mutex
	script   []types.AssistantMessage
	requests []types.Context
}

func (s *scriptedRequests) stream(ctx context.Context, model types.Model, c types.Context, _ *types.SimpleStreamOptions) *stream.AssistantStream {
	s.mu.Lock()
	s.requests = append(s.requests, c)
	message := types.AssistantMessage{Content: []types.ContentBlock{types.NewText("done")}, StopReason: types.StopStop}
	if len(s.script) > 0 {
		message, s.script = s.script[0], s.script[1:]
	}
	s.mu.Unlock()
	message.API, message.Provider, message.Model = model.API, model.Provider, model.ID
	out := stream.NewAssistantStream(model.API, model.Provider, model.ID)
	go func() {
		out.Push(types.StreamEvent{Type: types.EvStart, Partial: &message})
		out.Push(types.StreamEvent{Type: types.EvDone, Reason: message.StopReason, Message: &message})
	}()
	return out
}

// tails returns the reminder names in each request's request-scope tail; a
// message there that is not a reminder shows as "!<role>".
func (s *scriptedRequests) tails() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]string, len(s.requests))
	for i, request := range s.requests {
		for _, message := range request.Messages[len(request.Messages)-request.RequestScoped:] {
			user, ok := message.(types.UserMessage)
			if !ok {
				out[i] = append(out[i], "!"+message.Role())
				continue
			}
			out[i] = append(out[i], user.Reminder)
		}
	}
	return out
}

// firedOn lists the 1-based requests whose tail carried name.
func firedOn(tails [][]string, name string) []int {
	var requests []int
	for i, names := range tails {
		if slices.Contains(names, name) {
			requests = append(requests, i+1)
		}
	}
	return requests
}

func toolCallMessage(names ...string) types.AssistantMessage {
	message := types.AssistantMessage{StopReason: types.StopToolUse}
	for i, name := range names {
		message.Content = append(message.Content, types.NewToolCall(fmt.Sprintf("call-%d", i), name, json.RawMessage(`{}`)))
	}
	return message
}

// toolTurns scripts count responses that each call tool once, then a text
// answer.
func toolTurns(tool string, count int) []types.AssistantMessage {
	script := make([]types.AssistantMessage, 0, count+1)
	for range count {
		script = append(script, toolCallMessage(tool))
	}
	return append(script, types.AssistantMessage{Content: []types.ContentBlock{types.NewText("done")}, StopReason: types.StopStop})
}

func newReminderAgent(model types.Model, script *scriptedRequests, budget Budget, reminders ...Reminder) *Agent {
	return NewAgent(AgentOptions{
		InitialState: &AgentState{SystemPrompt: "system", Model: model, Tools: []AgentTool{okTool("work"), okTool("todo_write")}},
		ConvertToLlm: func(messages []types.Message) []types.Message { return messages },
		StreamFn:     script.stream,
		Budget:       budget,
		Reminders:    reminders,
	})
}

var reminderModel = types.Model{API: "test", Provider: "test", ID: "m", ContextWindow: 1000}

func TestReminderCadenceBoundaries(t *testing.T) {
	for _, tc := range []struct {
		every, first int
		want         []int
	}{
		{every: 3, want: []int{3, 6}},
		{every: 3, first: 2, want: []int{2, 5}},
		{every: 4, first: 1, want: []int{1, 5}},
		{first: 7, want: []int{7}},
		{want: []int{1, 2, 3, 4, 5, 6, 7}},
	} {
		t.Run(fmt.Sprintf("every=%d,first=%d", tc.every, tc.first), func(t *testing.T) {
			script := &scriptedRequests{script: toolTurns("work", 6)}
			a := newReminderAgent(reminderModel, script, Budget{}, Reminder{Name: "r", Text: "stay on task", Every: tc.every, First: tc.first, MaxFires: 100})
			if _, err := a.PromptText(context.Background(), "go"); err != nil {
				t.Fatal(err)
			}
			tails := script.tails()
			if len(tails) != 7 {
				t.Fatalf("requests = %d, want 7", len(tails))
			}
			if got := firedOn(tails, "r"); !slices.Equal(got, tc.want) {
				t.Fatalf("fired on requests %v, want %v", got, tc.want)
			}
		})
	}
}

// MaxFires caps firings per run; unset means once. A new run starts over.
func TestReminderFireBound(t *testing.T) {
	script := &scriptedRequests{script: toolTurns("work", 4)}
	a := newReminderAgent(reminderModel, script, Budget{},
		Reminder{Name: "twice", Text: "a", MaxFires: 2},
		Reminder{Name: "once", Text: "b"},
	)
	if _, err := a.PromptText(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	tails := script.tails()
	if got := firedOn(tails, "twice"); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("twice fired on %v, want [1 2]", got)
	}
	if got := firedOn(tails, "once"); !slices.Equal(got, []int{1}) {
		t.Fatalf("once fired on %v, want [1]", got)
	}
	script.script = toolTurns("work", 1)
	if _, err := a.PromptText(context.Background(), "again"); err != nil {
		t.Fatal(err)
	}
	if got := firedOn(script.tails()[5:], "twice"); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("second run fired twice on %v, want [1 2]", got)
	}
}

// A final-turn reminder fires on the last request the budget allows, counted
// in the logical run, and never without a max-turns budget.
func TestReminderFinalTurn(t *testing.T) {
	final := Reminder{Name: "final", Text: "last turn: answer now", When: ReminderCondition{FinalTurn: true}}
	script := &scriptedRequests{script: toolTurns("work", 10)}
	a := newReminderAgent(reminderModel, script, Budget{MaxTurns: 4}, final)
	result, err := a.PromptText(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != StopMaxTurns || len(script.tails()) != 4 {
		t.Fatalf("result = %+v requests = %d", result, len(script.tails()))
	}
	if got := firedOn(script.tails(), "final"); !slices.Equal(got, []int{4}) {
		t.Fatalf("final fired on %v, want [4]", got)
	}

	// A continued logical run counts the turns before it: with 2 already
	// spent, the 2nd request of this process is the 4th of the run.
	script = &scriptedRequests{script: toolTurns("work", 10)}
	a = newReminderAgent(reminderModel, script, Budget{MaxTurns: 4}, final)
	a.Subscribe(func(_ context.Context, ev AgentEvent) error {
		if ev.Type == EventAgentStart {
			a.SetRunState(RunState{RunID: "r2", LogicalRunID: "r1", Turns: 2})
		}
		return nil
	})
	if _, err := a.PromptText(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if got := firedOn(script.tails(), "final"); !slices.Equal(got, []int{2}) {
		t.Fatalf("continued run fired on %v, want [2]", got)
	}

	script = &scriptedRequests{script: toolTurns("work", 3)}
	a = newReminderAgent(reminderModel, script, Budget{}, final)
	if _, err := a.PromptText(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if got := firedOn(script.tails(), "final"); len(got) != 0 {
		t.Fatalf("fired without a budget on %v", got)
	}
}

// The context condition reads the last response's reported tokens and fires
// at exactly the threshold, not below it.
func TestReminderContextPercentThreshold(t *testing.T) {
	withTokens := func(total int) types.AssistantMessage {
		message := toolCallMessage("work")
		message.Usage = types.Usage{Input: total - 10, Output: 10, TotalTokens: total}
		return message
	}
	script := &scriptedRequests{script: []types.AssistantMessage{withTokens(500), withTokens(799), withTokens(800), withTokens(950)}}
	a := newReminderAgent(reminderModel, script, Budget{}, Reminder{Name: "ctx", Text: "context is filling up", MaxFires: 5, When: ReminderCondition{ContextPercent: 80}})
	if _, err := a.PromptText(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	// Request n sees the usage of response n-1.
	if got := firedOn(script.tails(), "ctx"); !slices.Equal(got, []int{4, 5}) {
		t.Fatalf("fired on %v, want [4 5]", got)
	}

	unknownWindow := reminderModel
	unknownWindow.ContextWindow = 0
	script = &scriptedRequests{script: []types.AssistantMessage{withTokens(950)}}
	a = newReminderAgent(unknownWindow, script, Budget{}, Reminder{Name: "ctx", Text: "x", When: ReminderCondition{ContextPercent: 1}})
	if _, err := a.PromptText(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if got := firedOn(script.tails(), "ctx"); len(got) != 0 {
		t.Fatalf("fired for a model without a window on %v", got)
	}
}

// The tool-call condition counts calls since the reminder last fired, and a
// call to WithoutTool restarts the count.
func TestReminderToolCallsWithoutTool(t *testing.T) {
	script := func() *scriptedRequests {
		return &scriptedRequests{script: []types.AssistantMessage{
			toolCallMessage("work"), toolCallMessage("work"), toolCallMessage("work"),
			toolCallMessage("todo_write"),
			toolCallMessage("work"), toolCallMessage("work"), toolCallMessage("work"),
			{Content: []types.ContentBlock{types.NewText("done")}, StopReason: types.StopStop},
		}}
	}
	for _, tc := range []struct {
		name    string
		without string
		want    []int
	}{
		// Calls before request n: n-1. Fires at 3 calls (request 4); the
		// todo_write call (the 4th) restarts the count, so the next fire
		// needs calls 5, 6, 7.
		{name: "todo resets", without: "todo_write", want: []int{4, 8}},
		// Without a reset tool only the fire restarts the count.
		{name: "fire resets", want: []int{4, 7}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := script()
			a := newReminderAgent(reminderModel, s, Budget{}, Reminder{Name: "todo", Text: "update your todo list", MaxFires: 5, When: ReminderCondition{ToolCalls: 3, WithoutTool: tc.without}})
			if _, err := a.PromptText(context.Background(), "go"); err != nil {
				t.Fatal(err)
			}
			if got := firedOn(s.tails(), "todo"); !slices.Equal(got, tc.want) {
				t.Fatalf("fired on %v, want %v", got, tc.want)
			}
		})
	}
}

// A request-scope reminder goes after the transcript of its own request and
// nowhere else: not in the agent's messages, not in any message event, and
// its firing rides on the request's final message_end.
func TestReminderRequestScopeStaysOutOfTranscript(t *testing.T) {
	script := &scriptedRequests{script: toolTurns("work", 2)}
	a := newReminderAgent(reminderModel, script, Budget{}, Reminder{Name: "nudge", Text: "NUDGE-TEXT", Every: 2})
	var fires [][]ReminderFire
	a.Subscribe(func(_ context.Context, ev AgentEvent) error {
		if ev.Type == EventMessageStart || ev.Type == EventMessageEnd {
			if user, ok := ev.Message.(types.UserMessage); ok && user.Reminder != "" {
				t.Errorf("%s event for request-scope reminder %q", ev.Type, user.Reminder)
			}
		}
		if ev.Type == EventMessageEnd {
			if _, ok := ev.Message.(types.AssistantMessage); ok {
				fires = append(fires, ev.Reminders)
			}
		}
		return nil
	})
	if _, err := a.PromptText(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if got := firedOn(script.tails(), "nudge"); !slices.Equal(got, []int{2}) {
		t.Fatalf("fired on %v, want [2]", got)
	}
	request := script.requests[1]
	last := request.Messages[len(request.Messages)-1].(types.UserMessage)
	if request.RequestScoped != 1 || last.Content.Text != "<system-reminder>\nNUDGE-TEXT\n</system-reminder>" {
		t.Fatalf("request 2 tail = %d %+v", request.RequestScoped, last)
	}
	for _, message := range a.State().Messages {
		raw, _ := json.Marshal(message)
		if bytes.Contains(raw, []byte("NUDGE-TEXT")) {
			t.Fatalf("transcript holds the request-scope reminder: %s", raw)
		}
	}
	if len(fires) != 3 || len(fires[0]) != 0 || !slices.Equal(fires[1], []ReminderFire{{Name: "nudge", Scope: ReminderRequest}}) || len(fires[2]) != 0 {
		t.Fatalf("firings on assistant message_end = %+v", fires)
	}
}

// A conversation reminder is appended once, sent with every later request
// in the same place, and its firing is on its own message_end.
func TestReminderConversationScopeAppendsOnce(t *testing.T) {
	script := &scriptedRequests{script: toolTurns("work", 3)}
	a := newReminderAgent(reminderModel, script, Budget{}, Reminder{Name: "rule", Text: "RULE-TEXT", Scope: ReminderConversation, First: 2})
	var ends int
	a.Subscribe(func(_ context.Context, ev AgentEvent) error {
		if user, ok := ev.Message.(types.UserMessage); ok && ev.Type == EventMessageEnd && user.Reminder == "rule" {
			ends++
			if !slices.Equal(ev.Reminders, []ReminderFire{{Name: "rule", Scope: ReminderConversation}}) {
				t.Errorf("reminder message_end firings = %+v", ev.Reminders)
			}
		}
		return nil
	})
	if _, err := a.PromptText(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if ends != 1 || countReminders(a.State().Messages, "rule") != 1 {
		t.Fatalf("message_end=%d transcript copies=%d, want 1 and 1", ends, countReminders(a.State().Messages, "rule"))
	}
	for i, request := range script.requests {
		want := 0
		if i >= 1 {
			want = 1
		}
		if got := countReminders(request.Messages, "rule"); got != want || request.RequestScoped != 0 {
			t.Fatalf("request %d copies=%d scoped=%d, want %d", i+1, got, request.RequestScoped, want)
		}
	}
}

func countReminders(messages []types.Message, name string) int {
	count := 0
	for _, message := range messages {
		if user, ok := message.(types.UserMessage); ok && user.Reminder == name {
			count++
		}
	}
	return count
}

// The cached prefix: across turns with both scopes firing, the system prompt
// and tools never change, and each request's messages, without its
// request-scope tail, are a byte-for-byte prefix of the next request's.
func TestReminderKeepsRequestPrefixStable(t *testing.T) {
	script := &scriptedRequests{script: toolTurns("work", 5)}
	a := newReminderAgent(reminderModel, script, Budget{},
		Reminder{Name: "nudge", Text: "nudge", MaxFires: 10},
		Reminder{Name: "rule", Text: "rule", Scope: ReminderConversation, Every: 2, MaxFires: 2},
	)
	if _, err := a.PromptText(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if len(script.requests) != 6 {
		t.Fatalf("requests = %d", len(script.requests))
	}
	marshal := func(v any) []byte {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	for i := 1; i < len(script.requests); i++ {
		prev, next := script.requests[i-1], script.requests[i]
		if prev.SystemPrompt != next.SystemPrompt || !bytes.Equal(marshal(prev.Tools), marshal(next.Tools)) {
			t.Fatalf("request %d changed the system prompt or tools", i+1)
		}
		if prev.RequestScoped != 1 {
			t.Fatalf("request %d scoped = %d, want the nudge", i, prev.RequestScoped)
		}
		kept := prev.Messages[:len(prev.Messages)-prev.RequestScoped]
		if len(next.Messages) < len(kept) {
			t.Fatalf("request %d is shorter than its predecessor's prefix", i+1)
		}
		for j := range kept {
			if a, b := marshal(kept[j]), marshal(next.Messages[j]); !bytes.Equal(a, b) {
				t.Fatalf("request %d message %d changed:\n%s\n%s", i+1, j, a, b)
			}
		}
	}
	if got := countReminders(script.requests[5].Messages, "rule"); got != 2 {
		t.Fatalf("last request holds %d conversation copies, want 2", got)
	}
}

// A mid-run rewind (a turn update that drops the reminder's copy) does not
// refund the firing: the reminder does not come back.
func TestReminderRewindDoesNotRefund(t *testing.T) {
	script := &scriptedRequests{script: toolTurns("work", 3)}
	a := newReminderAgent(reminderModel, script, Budget{}, Reminder{Name: "rule", Text: "rule", Scope: ReminderConversation})
	rewound := false
	a.PrepareNextTurnE = func(_ context.Context, c ShouldStopAfterTurnContext) (*AgentLoopTurnUpdate, error) {
		if rewound || countReminders(c.Context.Messages, "rule") == 0 || c.Message.StopReason == "" {
			return nil, nil
		}
		rewound = true
		next := c.Context
		next.Messages = slices.Clone(next.Messages[:1])
		return &AgentLoopTurnUpdate{Context: &next}, nil
	}
	if _, err := a.PromptText(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if !rewound {
		t.Fatal("rewind never ran")
	}
	for i, request := range script.requests[1:] {
		if got := countReminders(request.Messages, "rule"); got != 0 {
			t.Fatalf("request %d after the rewind holds %d copies", i+2, got)
		}
	}
}

// An invalid list fails the run before any request and names the reminder.
func TestReminderInvalidListFailsRun(t *testing.T) {
	script := &scriptedRequests{}
	a := newReminderAgent(reminderModel, script, Budget{}, Reminder{Name: "bad", Text: "x", MaxFires: -1})
	result, _ := a.PromptText(context.Background(), "go")
	if result.StopReason != StopError || result.Err == nil || !strings.Contains(result.Err.Error(), `reminder "bad": max_fires`) || len(script.requests) != 0 {
		t.Fatalf("result = %+v requests = %d", result, len(script.requests))
	}
}

func TestValidateReminders(t *testing.T) {
	for _, tc := range []struct {
		reminders []Reminder
		want      string
	}{
		{[]Reminder{{Text: "x"}}, "reminders[0].name"},
		{[]Reminder{{Name: "a", Text: "x"}, {Name: "a", Text: "y"}}, "used twice"},
		{[]Reminder{{Name: "a", Text: " "}}, "text"},
		{[]Reminder{{Name: "a", Text: "x", Scope: "run"}}, "scope"},
		{[]Reminder{{Name: "a", Text: "x", Every: -1}}, "every"},
		{[]Reminder{{Name: "a", Text: "x", First: -1}}, "first"},
		{[]Reminder{{Name: "a", Text: "x", When: ReminderCondition{ContextPercent: 101}}}, "context_percent"},
		{[]Reminder{{Name: "a", Text: "x", When: ReminderCondition{ToolCalls: -1}}}, "tool_calls"},
		{[]Reminder{{Name: "a", Text: "x", When: ReminderCondition{WithoutTool: "todo_write"}}}, "without_tool"},
	} {
		if err := ValidateReminders(tc.reminders); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ValidateReminders(%+v) = %v, want %q", tc.reminders, err, tc.want)
		}
	}
	if err := ValidateReminders([]Reminder{{Name: "a", Text: "x", Scope: ReminderConversation, Every: 3, First: 1, MaxFires: 2, When: ReminderCondition{ContextPercent: 100, ToolCalls: 2, WithoutTool: "todo_write", FinalTurn: true}}}); err != nil {
		t.Fatal(err)
	}
}
