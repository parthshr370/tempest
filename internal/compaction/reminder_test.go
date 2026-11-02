package compaction

import (
	"strings"
	"testing"

	ptypes "go.harness.dev/harness/internal/engine/types"
)

func reminderMsg(name, text string) ptypes.UserMessage {
	return ptypes.UserMessage{Content: ptypes.StringContent("<system-reminder>\n" + text + "\n</system-reminder>"), Reminder: name}
}

func reminderNames(messages []ptypes.Message) []string {
	var names []string
	for _, message := range messages {
		if user, ok := asUser(message); ok && user.Reminder != "" {
			names = append(names, user.Reminder+":"+user.Content.Text)
		}
	}
	return names
}

// Conversation reminders in the summarized part are not summarized as the
// user's words; the newest copy of each one the kept part does not hold
// follows the summary verbatim, still marked as a reminder.
func TestApplyCompactionCarriesConversationReminders(t *testing.T) {
	summarized := []ptypes.Message{
		userMsg("first ask"),
		reminderMsg("rule", "old rule"),
		assistantMsg("did stuff"),
		reminderMsg("nudge", "nudge"),
		reminderMsg("rule", "new rule"),
		assistantMsg("more stuff"),
	}
	entries := []SessionEntry{}
	for i, message := range summarized {
		entries = append(entries, msgEntry(string(rune('a'+i)), message))
	}
	entries = append(entries,
		msgEntry("k1", assistantMsg("kept reply")),
		msgEntry("k2", reminderMsg("nudge", "nudge")),
		msgEntry("k3", assistantMsg("last")),
	)
	if text := SerializeConversation(summarized); strings.Contains(text, "system-reminder") || !strings.Contains(text, "first ask") {
		t.Fatalf("summarizer input:\n%s", text)
	}
	out := ApplyCompaction(entries, &CompactionPreparation{FirstKeptEntryID: "k1", MessagesToSummarize: summarized}, "THE SUMMARY")
	if !strings.Contains(userAllText(t, out[0]), "THE SUMMARY") {
		t.Fatalf("first message = %+v", out[0])
	}
	got := reminderNames(out)
	want := []string{"rule:<system-reminder>\nnew rule\n</system-reminder>", "nudge:<system-reminder>\nnudge\n</system-reminder>"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("reminders after compaction = %q, want %q", got, want)
	}
	if user, ok := out[1].(ptypes.UserMessage); !ok || user.Reminder != "rule" {
		t.Fatalf("carried reminder does not follow the summary: %+v", out[1])
	}
	if len(out) != 5 {
		t.Fatalf("compacted = %d messages, want summary, rule, and the 3 kept", len(out))
	}
}

// A cut that lands on a reminder keeps it as its own message: merging the
// summary into it would drop its marker, and a later compaction would
// summarize it as user text.
func TestApplyCompactionDoesNotMergeSummaryIntoReminder(t *testing.T) {
	entries := []SessionEntry{
		msgEntry("1", userMsg("ask")),
		msgEntry("2", assistantMsg("did stuff")),
		msgEntry("3", reminderMsg("rule", "rule")),
		msgEntry("4", assistantMsg("more")),
	}
	out := ApplyCompaction(entries, &CompactionPreparation{FirstKeptEntryID: "3", MessagesToSummarize: []ptypes.Message{userMsg("ask"), assistantMsg("did stuff")}}, "THE SUMMARY")
	if len(out) != 3 {
		t.Fatalf("compacted = %+v", out)
	}
	if user := out[0].(ptypes.UserMessage); user.Reminder != "" || !strings.Contains(userAllText(t, user), "THE SUMMARY") {
		t.Fatalf("summary message = %+v", user)
	}
	if user, ok := out[1].(ptypes.UserMessage); !ok || user.Reminder != "rule" || strings.Contains(user.Content.Text, "SUMMARY") {
		t.Fatalf("kept reminder = %+v", out[1])
	}
}

// Copies already in the kept part collapse too: only the newest copy of a
// reminder survives, in its place, and ordinary kept messages are untouched.
func TestApplyCompactionCollapsesKeptReminderCopies(t *testing.T) {
	entries := []SessionEntry{
		msgEntry("1", userMsg("ask")),
		msgEntry("2", assistantMsg("did stuff")),
		msgEntry("k1", reminderMsg("rule", "first")),
		msgEntry("k2", assistantMsg("more")),
		msgEntry("k3", reminderMsg("rule", "newest")),
		msgEntry("k4", assistantMsg("last")),
	}
	summarized := []ptypes.Message{userMsg("ask"), assistantMsg("did stuff"), reminderMsg("rule", "summarized")}
	out := ApplyCompaction(entries, &CompactionPreparation{FirstKeptEntryID: "k1", MessagesToSummarize: summarized}, "THE SUMMARY")
	got := reminderNames(out)
	if want := "rule:<system-reminder>\nnewest\n</system-reminder>"; len(got) != 1 || got[0] != want {
		t.Fatalf("reminders after compaction = %q, want only %q", got, want)
	}
	if len(out) != 4 || !strings.Contains(userAllText(t, out[0]), "THE SUMMARY") {
		t.Fatalf("compacted = %+v, want summary, more, newest rule, last", out)
	}
	if a, ok := out[1].(ptypes.AssistantMessage); !ok || a.Content[0].Text != "more" {
		t.Fatalf("out[1] = %+v, want the kept reply", out[1])
	}
	if a, ok := out[3].(ptypes.AssistantMessage); !ok || a.Content[0].Text != "last" {
		t.Fatalf("out[3] = %+v, want the last reply", out[3])
	}
}

// Pointer-form user messages are reminders too: a summarized pointer copy is
// carried (it never reached the summarizer), and a pointer copy in the kept
// part counts as the newest copy.
func TestApplyCompactionKeepsPointerFormReminders(t *testing.T) {
	pointer := func(name, text string) *ptypes.UserMessage {
		message := reminderMsg(name, text)
		return &message
	}
	t.Run("summarized", func(t *testing.T) {
		summarized := []ptypes.Message{userMsg("ask"), pointer("rule", "rule"), assistantMsg("did stuff")}
		entries := []SessionEntry{msgEntry("1", summarized[0]), msgEntry("2", summarized[1]), msgEntry("3", summarized[2]), msgEntry("k1", assistantMsg("kept"))}
		if text := SerializeConversation(summarized); strings.Contains(text, "system-reminder") {
			t.Fatalf("summarizer input holds the reminder:\n%s", text)
		}
		out := ApplyCompaction(entries, &CompactionPreparation{FirstKeptEntryID: "k1", MessagesToSummarize: summarized}, "THE SUMMARY")
		if got := reminderNames(out); len(got) != 1 || got[0] != "rule:<system-reminder>\nrule\n</system-reminder>" {
			t.Fatalf("reminders after compaction = %q, want the pointer-form rule carried", got)
		}
		if len(out) != 3 {
			t.Fatalf("compacted = %+v, want summary, rule, kept", out)
		}
	})
	t.Run("kept", func(t *testing.T) {
		summarized := []ptypes.Message{userMsg("ask"), reminderMsg("rule", "old"), assistantMsg("did stuff")}
		entries := []SessionEntry{
			msgEntry("1", summarized[0]), msgEntry("2", summarized[1]), msgEntry("3", summarized[2]),
			msgEntry("k1", assistantMsg("kept")),
			msgEntry("k2", reminderMsg("rule", "older kept")),
			msgEntry("k3", assistantMsg("more")),
			msgEntry("k4", pointer("rule", "newest")),
			msgEntry("k5", assistantMsg("last")),
		}
		out := ApplyCompaction(entries, &CompactionPreparation{FirstKeptEntryID: "k1", MessagesToSummarize: summarized}, "THE SUMMARY")
		if got := reminderNames(out); len(got) != 1 || got[0] != "rule:<system-reminder>\nnewest\n</system-reminder>" {
			t.Fatalf("reminders after compaction = %q, want only the newest pointer-form copy", got)
		}
		if len(out) != 5 {
			t.Fatalf("compacted = %d messages, want summary and 4 kept", len(out))
		}
	})
}
