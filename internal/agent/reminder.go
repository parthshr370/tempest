package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"go.harness.dev/harness/internal/engine/estimate"
	"go.harness.dev/harness/internal/engine/types"
)

// ReminderScope says how long a fired reminder stays in front of the model.
type ReminderScope string

const (
	// ReminderRequest shows a firing in the request it fires on only. It is
	// added to that request's messages and never to the transcript, so it is
	// not journaled, compacted or replayed on resume.
	ReminderRequest ReminderScope = "request"
	// ReminderConversation appends a firing to the transcript as a message:
	// it is journaled and sent with every later request of the conversation.
	ReminderConversation ReminderScope = "conversation"
)

// Reminder is a short instruction the loop adds at the tail of a model
// request when its cadence matches and every condition it sets holds, at most
// MaxFires times per run. It never touches the system prompt or the tools, so
// the cached prefix of earlier requests stays valid.
//
// Requests are counted from 1 per logical run, the count a max-turns budget
// uses, so a resumed run keeps its cadence. A request fires on request n when
// n >= First and (n-First) is a multiple of Every.
type Reminder struct {
	// Name identifies the reminder in the journal and in the transcript; it
	// must be unique and non-empty (settings: name).
	Name string `json:"name"`
	// Text is sent as <system-reminder>\nText\n</system-reminder> (text).
	Text string `json:"text"`
	// Scope is ReminderRequest (the default) or ReminderConversation (scope).
	Scope ReminderScope `json:"scope,omitempty"`
	// Every is the cadence in requests; zero means 1 (every).
	Every int `json:"every,omitempty"`
	// First is the first request that may fire; zero means Every (first).
	First int `json:"first,omitempty"`
	// MaxFires bounds the firings per run; zero means 1 (max_fires).
	MaxFires int `json:"max_fires,omitempty"`
	// When adds conditions; all that are set must hold (when).
	When ReminderCondition `json:"when,omitzero"`
}

// ReminderCondition narrows when a reminder fires. Zero fields are not
// checked; every set field must hold.
type ReminderCondition struct {
	// FinalTurn holds on the last request a max-turns budget allows; never
	// without one (final_turn).
	FinalTurn bool `json:"final_turn,omitempty"`
	// ContextPercent holds when the context the conversation's latest
	// response with usage reported is at least this percent of the model's
	// context window, in (0, 100] (context_percent). The reading spans runs:
	// a new prompt resends the whole conversation, so the previous run's
	// last response bounds its first request from below. It never holds
	// before any response reported usage or for a model with no known window.
	ContextPercent float64 `json:"context_percent,omitempty"`
	// ToolCalls holds once the run has made at least this many tool calls
	// since the reminder last fired, and since WithoutTool was last called
	// when that is set (tool_calls). Calls are counted from the start of
	// the run in this process, so a resumed run counts from the resume.
	ToolCalls int `json:"tool_calls,omitempty"`
	// WithoutTool names the tool whose call restarts the ToolCalls count,
	// such as "todo_write" (without_tool). It needs ToolCalls.
	WithoutTool string `json:"without_tool,omitempty"`
}

// ReminderFire records one firing for the journal: the reminder's name and
// scope, never its text.
type ReminderFire struct {
	Name  string        `json:"name"`
	Scope ReminderScope `json:"scope"`
}

// ValidateReminders checks names, scopes and limits. Errors name the
// reminder and the setting at fault.
func ValidateReminders(reminders []Reminder) error {
	seen := make(map[string]bool, len(reminders))
	for i, r := range reminders {
		label := fmt.Sprintf("reminders[%d]", i)
		if strings.TrimSpace(r.Name) == "" {
			return fmt.Errorf("%s.name: must not be empty", label)
		}
		label = fmt.Sprintf("reminder %q", r.Name)
		if seen[r.Name] {
			return fmt.Errorf("%s: name is used twice", label)
		}
		seen[r.Name] = true
		var err error
		switch {
		case strings.TrimSpace(r.Text) == "":
			err = errors.New("text: must not be empty")
		case r.Scope != "" && r.Scope != ReminderRequest && r.Scope != ReminderConversation:
			err = fmt.Errorf("scope: %q is not %q or %q", r.Scope, ReminderRequest, ReminderConversation)
		case r.Every < 0:
			err = errors.New("every: must not be negative")
		case r.First < 0:
			err = errors.New("first: must not be negative")
		case r.MaxFires < 0:
			err = errors.New("max_fires: must not be negative")
		case math.IsNaN(r.When.ContextPercent) || r.When.ContextPercent < 0 || r.When.ContextPercent > 100:
			err = errors.New("when.context_percent: must be within (0, 100]")
		case r.When.ToolCalls < 0:
			err = errors.New("when.tool_calls: must not be negative")
		case r.When.WithoutTool != "" && r.When.ToolCalls == 0:
			err = errors.New("when.without_tool: needs when.tool_calls")
		}
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
	}
	return nil
}

// scope is the reminder's scope with the default applied.
func (r Reminder) scope() ReminderScope {
	if r.Scope == "" {
		return ReminderRequest
	}
	return r.Scope
}

// cadenceMatches reports whether request n (1-based) is on the reminder's
// cadence.
func (r Reminder) cadenceMatches(n int) bool {
	every := max(r.Every, 1)
	first := r.First
	if first == 0 {
		first = every
	}
	return n >= first && (n-first)%every == 0
}

// message is the reminder's firing as a transcript or request message.
func (r Reminder) message() types.UserMessage {
	return types.UserMessage{
		Content:   types.StringContent("<system-reminder>\n" + strings.TrimSpace(r.Text) + "\n</system-reminder>"),
		Timestamp: time.Now().UnixMilli(),
		Reminder:  r.Name,
	}
}

// reminderRun is one run's reminder state: the firings so far, restored from
// the journal on resume, and the run's tool-call counts.
type reminderRun struct {
	reminders []Reminder
	fires     map[string]int
	// toolCalls counts the tool calls this run made; firedAt and usedAt hold
	// its value when a reminder last fired and after a tool was last called.
	toolCalls int
	firedAt   map[string]int
	usedAt    map[string]int
}

func newReminderRun(reminders []Reminder, fires map[string]int) *reminderRun {
	run := &reminderRun{reminders: reminders, fires: make(map[string]int, len(fires)), firedAt: map[string]int{}, usedAt: map[string]int{}}
	for name, count := range fires {
		run.fires[name] = count
	}
	return run
}

// countToolCalls adds the tool calls message made to the run's count.
func (s *reminderRun) countToolCalls(message types.AssistantMessage) {
	if s == nil {
		return
	}
	for _, block := range message.Content {
		if block.Type == types.BlockToolCall {
			s.toolCalls++
			s.usedAt[block.Name] = s.toolCalls
		}
	}
}

// requestContext is what a reminder's conditions read for one request.
type requestContext struct {
	// request is the 1-based request number in the logical run.
	request  int
	maxTurns int
	model    types.Model
	messages []types.Message
}

// due returns the reminders that fire on the request described by c and
// counts each firing. A conversation reminder whose copy the transcript
// already holds after its last response does not fire again: that copy was
// committed before a crash, and the resumed request carries it.
func (s *reminderRun) due(c requestContext) []Reminder {
	if s == nil {
		return nil
	}
	var fired []Reminder
	for _, r := range s.reminders {
		if s.fires[r.Name] >= max(r.MaxFires, 1) || !r.cadenceMatches(c.request) || !s.holds(r, c) {
			continue
		}
		if r.scope() == ReminderConversation && pendingCopy(c.messages, r.Name) {
			continue
		}
		s.fires[r.Name]++
		s.firedAt[r.Name] = s.toolCalls
		fired = append(fired, r)
	}
	return fired
}

// holds reports whether every condition r sets holds for c.
func (s *reminderRun) holds(r Reminder, c requestContext) bool {
	when := r.When
	if when.FinalTurn && (c.maxTurns <= 0 || c.request != c.maxTurns) {
		return false
	}
	if when.ContextPercent > 0 {
		tokens := estimate.EstimateMessages(c.messages).UsageTokens
		if c.model.ContextWindow <= 0 || tokens <= 0 || float64(tokens)*100 < when.ContextPercent*float64(c.model.ContextWindow) {
			return false
		}
	}
	if when.ToolCalls > 0 {
		since := s.firedAt[r.Name]
		if when.WithoutTool != "" {
			since = max(since, s.usedAt[when.WithoutTool])
		}
		if s.toolCalls-since < when.ToolCalls {
			return false
		}
	}
	return true
}

// pendingCopy reports whether messages hold a copy of the named reminder
// after their last assistant message.
func pendingCopy(messages []types.Message, name string) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		switch message := messages[i].(type) {
		case types.AssistantMessage, *types.AssistantMessage:
			return false
		case types.UserMessage:
			if message.Reminder == name {
				return true
			}
		}
	}
	return false
}

// requestTail is what the reminders that fired add to one request only: the
// messages sent after its transcript, and their firings, recorded with the
// request's final message.
type requestTail struct {
	messages []types.Message
	fires    []ReminderFire
}

// fireReminders fires the reminders due on the request about to be sent.
// A conversation reminder is appended to the transcript like any message,
// its firing on its own message_end. A request reminder goes into the
// returned tail, never into current.
func (run *loopRun) fireReminders(ctx context.Context, current *AgentContext) (requestTail, error) {
	request := run.initialTurns + run.result.Turns
	due := run.reminders.due(requestContext{request: request, maxTurns: run.config.Budget.MaxTurns, model: run.config.Model, messages: current.Messages})
	var tail requestTail
	for _, r := range due {
		fire := ReminderFire{Name: r.Name, Scope: r.scope()}
		slog.Info("reminder fired", "name", r.Name, "scope", string(fire.Scope), "request", request)
		message := r.message()
		if fire.Scope == ReminderRequest {
			tail.messages = append(tail.messages, message)
			tail.fires = append(tail.fires, fire)
			continue
		}
		if err := run.emit(ctx, MessageStart(message)); err != nil {
			return requestTail{}, err
		}
		end := MessageEnd(message)
		end.Reminders = []ReminderFire{fire}
		if err := run.emit(ctx, end); err != nil {
			return requestTail{}, err
		}
		current.Messages = append(current.Messages, message)
		run.messages = append(run.messages, message)
	}
	return tail, nil
}
