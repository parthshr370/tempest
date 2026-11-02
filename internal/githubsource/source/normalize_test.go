package source

import (
	"encoding/json"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/githubsource/model"
)

func mustMarshal(t *testing.T, batch model.IntakeBatch) []byte {
	t.Helper()
	raw, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestMentions(t *testing.T) {
	reader := newText("acme-bot", defaultMaxBody)
	cases := map[string]bool{
		"@acme-bot fix this":            true,
		"hey @ACME-BOT, please":         true,
		"(@acme-bot)":                   true,
		"done\n@acme-bot":               true,
		"@acme-bot.":                    true,
		"@acme-bot-two please":          false,
		"@acme-bots please":             false,
		"mail me at x@acme-bot":         false,
		"@@acme-bot":                    false,
		"acme-bot without at":           false,
		"see @acme-bot_ for the config": false,
	}
	for body, want := range cases {
		if got := reader.Mentions(body); got != want {
			t.Errorf("Mentions(%q) = %v, want %v", body, got, want)
		}
	}
}

func TestCommands(t *testing.T) {
	reader := newText("acme-bot", defaultMaxBody)
	cases := map[string]model.Command{
		"@acme-bot stop":                    model.CommandStop,
		"  @Acme-Bot   STOP  ":              model.CommandStop,
		"\n\n@acme-bot status\nwhy so slow": model.CommandStatus,
		"@acme-bot stop now":                model.CommandNone,
		"please @acme-bot stop":             model.CommandNone,
		"@acme-bot stop.":                   model.CommandNone,
		"hello\n@acme-bot stop":             model.CommandNone,
		"@other stop":                       model.CommandNone,
		"":                                  model.CommandNone,
	}
	for body, want := range cases {
		if got := reader.Command(body); got != want {
			t.Errorf("Command(%q) = %q, want %q", body, got, want)
		}
	}
}

func TestBodyTruncationKeepsRunesAndMention(t *testing.T) {
	const limit = 10
	reader := newText("acme-bot", limit)
	body := "ééééé" + strings.Repeat(" padding", 3) + " @acme-bot"
	event := reader.withBody(model.Event{}, body, true)
	if !event.Truncated || event.BodyBytes != len(body) || len(event.Body) != limit || event.Body != "ééééé" {
		t.Fatalf("truncated event = %+v", event)
	}
	if !event.Mention {
		t.Fatal("a mention past the cut must still count")
	}
	odd := reader.withBody(model.Event{}, "aééééé", false)
	if odd.Body != "aéééé" || len(odd.Body) != 9 {
		t.Fatalf("cut inside a rune: %q", odd.Body)
	}
	short := reader.withBody(model.Event{}, "hi", false)
	if short.Truncated || short.Body != "hi" || short.BodyBytes != 2 {
		t.Fatalf("short = %+v", short)
	}
}

func TestSplitBounds(t *testing.T) {
	const bigBody = 200 << 10
	event := func(body string) []model.Event { return []model.Event{{Body: body}} }
	var entries []entry
	for range 250 {
		entries = append(entries, entry{events: event("x")})
	}
	runs := split(entries)
	if len(runs) != 3 || len(runs[0]) != model.MaxBatchEvents || len(runs[2]) != 50 {
		t.Fatalf("event split = %d runs", len(runs))
	}
	entries = []entry{{events: event(strings.Repeat("a", bigBody))}, {events: event(strings.Repeat("b", bigBody))}, {events: event(strings.Repeat("c", bigBody))}, {}}
	runs = split(entries)
	if len(runs) != 2 || len(runs[0]) != 2 || len(runs[1]) != 2 {
		t.Fatalf("byte split = %v", len(runs))
	}
	huge := split([]entry{{events: event(strings.Repeat("h", model.MaxBatchBodyBytes+1))}, {events: event("x")}})
	if len(huge) != 2 {
		t.Fatalf("an oversized entry must travel alone: %d runs", len(huge))
	}
	pair := split([]entry{{events: make([]model.Event, model.MaxBatchEvents-1)}, {events: make([]model.Event, 2)}})
	if len(pair) != 2 {
		t.Fatal("an entry's events were divided across batches")
	}
}

func TestSnapshotSettlement(t *testing.T) {
	if _, ok := snapshot("a", nil); ok {
		t.Fatal("no checks is not settled")
	}
	if _, ok := snapshot("a", []ciOutcome{{name: "x", outcome: "success"}, {name: "y", pending: true}}); ok {
		t.Fatal("a pending check is not settled")
	}
	first, ok := snapshot("a", []ciOutcome{{name: "b", outcome: "failure"}, {name: "a", outcome: "success"}, {name: "c", outcome: "skipped"}})
	second, _ := snapshot("a", []ciOutcome{{name: "a", outcome: "success"}, {name: "c", outcome: "skipped"}, {name: "b", outcome: "failure"}})
	if !ok || first.Fingerprint != second.Fingerprint || first.Passing != 1 || first.Skipped != 1 || len(first.Failing) != 1 || first.Failing[0] != "b" {
		t.Fatalf("snapshots = %+v %+v", first, second)
	}
	green, _ := snapshot("a", []ciOutcome{{name: "b", outcome: "success"}, {name: "a", outcome: "success"}, {name: "c", outcome: "skipped"}})
	if green.Fingerprint == first.Fingerprint {
		t.Fatal("a changed outcome kept the fingerprint")
	}
}
