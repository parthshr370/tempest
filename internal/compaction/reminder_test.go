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
		if user, ok := message.(ptypes.UserMessage); ok && user.Reminder != "" {
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
