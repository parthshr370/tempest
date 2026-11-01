package compaction

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
)

func msgEntry(id string, m ptypes.Message) SessionEntry {
	return SessionEntry{ID: id, Type: "message", Message: m}
}

func userMsg(text string) ptypes.UserMessage {
	return ptypes.UserMessage{Content: ptypes.StringContent(text)}
}

func assistantMsg(text string) ptypes.AssistantMessage {
	return ptypes.AssistantMessage{Content: []ptypes.ContentBlock{ptypes.NewText(text)}}
}

func assertNoConsecutiveUsers(t *testing.T, msgs []ptypes.Message) {
	t.Helper()
	prev := ""
	for i, m := range msgs {
		role := m.Role()
		if role == "user" && prev == "user" {
			t.Fatalf("consecutive user messages at index %d", i)
		}
		prev = role
	}
}

func userAllText(t *testing.T, m ptypes.Message) string {
	t.Helper()
	u, ok := m.(ptypes.UserMessage)
	if !ok {
		t.Fatalf("message is %T, want UserMessage", m)
	}
	if u.Content.IsBlocks() {
		var b strings.Builder
		for _, blk := range u.Content.Blocks {
			b.WriteString(blk.Text)
		}
		return b.String()
	}
	return u.Content.Text
}

// A non-split cut lands on a user message; the summary must merge into it so the
// rebuilt context never has two consecutive user turns (Anthropic rejects that).
func TestApplyCompactionMergesIntoKeptUser(t *testing.T) {
	entries := []SessionEntry{
		msgEntry("1", userMsg("first ask")),
		msgEntry("2", assistantMsg("did stuff")),
		msgEntry("3", userMsg("keep this ask")),
		msgEntry("4", assistantMsg("more")),
	}
	out := ApplyCompaction(entries, &CompactionPreparation{FirstKeptEntryID: "3"}, "THE SUMMARY")

	assertNoConsecutiveUsers(t, out)
	if len(out) != 2 {
		t.Fatalf("len(out) = %d, want 2 (merged user, assistant)", len(out))
	}
	text := userAllText(t, out[0])
	if !strings.Contains(text, "THE SUMMARY") || !strings.Contains(text, "keep this ask") {
		t.Fatalf("merged user missing summary or kept ask: %q", text)
	}
	if out[1].Role() != "assistant" {
		t.Fatalf("out[1] role = %q, want assistant", out[1].Role())
	}
}

// A split turn cut lands on an assistant message; the summary stands alone as a
// user turn before it, which is valid alternation.
func TestApplyCompactionStandaloneBeforeAssistant(t *testing.T) {
	entries := []SessionEntry{
		msgEntry("1", userMsg("u1")),
		msgEntry("2", assistantMsg("a1")),
		msgEntry("3", assistantMsg("kept assistant")),
	}
	out := ApplyCompaction(entries, &CompactionPreparation{FirstKeptEntryID: "3", IsSplitTurn: true}, "SUM")

	assertNoConsecutiveUsers(t, out)
	if len(out) != 2 {
		t.Fatalf("len(out) = %d, want 2 (summary user, assistant)", len(out))
	}
	if !strings.Contains(userAllText(t, out[0]), "SUM") {
		t.Fatalf("standalone summary missing text")
	}
	if out[1].Role() != "assistant" {
		t.Fatalf("out[1] role = %q, want assistant", out[1].Role())
	}
}

func TestCompactGeneratesSummaryWithFileOps(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("## Goal\nbuild the thing")))
	model := ptypes.Model{ID: "m", ContextWindow: 200000, MaxTokens: 4096}
	prep := &CompactionPreparation{
		FirstKeptEntryID:    "2",
		MessagesToSummarize: []ptypes.Message{userMsg("do X"), assistantMsg("working on X")},
		Settings:            DefaultCompactionSettings,
		FileOps:             CreateFileOps(),
	}

	summary, err := Compact(context.Background(), f.StreamSimple, model, prep)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "## Goal") {
		t.Fatalf("summary missing model output: %q", summary)
	}
}

// A summarizer that yields no text must surface an error, so a cancelled or empty
// turn never installs a blank checkpoint.
func TestCompactRejectsEmptySummary(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("   ")))
	model := ptypes.Model{ID: "m", ContextWindow: 200000, MaxTokens: 4096}
	prep := &CompactionPreparation{
		FirstKeptEntryID:    "2",
		MessagesToSummarize: []ptypes.Message{userMsg("do X")},
		Settings:            DefaultCompactionSettings,
		FileOps:             CreateFileOps(),
	}

	if _, err := Compact(context.Background(), f.StreamSimple, model, prep); err == nil {
		t.Fatal("expected error for empty summary output")
	}
}

// A conversation too large for one request to the model is summarized in
// requests that each fit its window: a message longer than a request is cut
// between characters, and each later request updates the summary so far.
func TestSummaryInPiecesFitsTheModelsWindow(t *testing.T) {
	const window, output, runes = 2000, 200, 6000
	model := ptypes.Model{ID: "small", ContextWindow: window, MaxTokens: output}
	messages := []ptypes.Message{userMsg(strings.Repeat("é", runes)), assistantMsg("done")}
	var prompts []string
	step := func(c ptypes.Context, opts *ptypes.StreamOptions, state faux.State, m ptypes.Model) (ptypes.AssistantMessage, error) {
		prompt := userContentText(c.Messages[0].(ptypes.UserMessage).Content)
		if size := EstimateContextTokens(c.Messages).Tokens + (len(c.SystemPrompt)+3)/4 + opts.MaxTokens; size > window {
			t.Errorf("request %d needs %d estimated tokens, over the %d-token window", len(prompts), size, window)
		}
		if !utf8.ValidString(prompt) {
			t.Errorf("request %d cut a character", len(prompts))
		}
		if len(prompts) > 0 && !strings.Contains(prompt, "<previous-summary>\nsummary "+strconv.Itoa(len(prompts))+"\n") {
			t.Errorf("request %d does not update the summary so far", len(prompts))
		}
		prompts = append(prompts, prompt)
		return faux.Respond(faux.Text("summary "+strconv.Itoa(len(prompts))))(c, opts, state, m)
	}
	f := faux.New(faux.Options{})
	steps := make([]faux.ResponseStep, 32)
	for i := range steps {
		steps[i] = step
	}
	f.SetResponses(steps...)
	result, err := Summarizer{Stream: f.StreamSimple}.Summary(context.Background(), model, DefaultCompactionSettings, messages, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts) < 2 || result.Text != "summary "+strconv.Itoa(len(prompts)) {
		t.Fatalf("%d requests answered %q, want several, the last one's summary kept", len(prompts), result.Text)
	}
	conversation := ""
	for _, prompt := range prompts {
		conversation += prompt[:strings.Index(prompt, "\n</conversation>")]
	}
	if strings.Count(conversation, "é") != runes || !strings.Contains(conversation, "done") {
		t.Fatal("the requests together do not carry the whole conversation")
	}
}
