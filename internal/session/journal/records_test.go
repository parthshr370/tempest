package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	ptypes "go.harness.dev/harness/internal/engine/types"
)

// v1Fixture is a journal exactly as version 1 wrote it: no seq, lane, run id,
// or records. Its last assistant turn has a tool call with no result, which v1
// left as is.
const v1Fixture = `{"type":"session","version":1,"id":"00000000000000a1","timestamp":"2024-01-02T03:04:05Z","cwd":"/workspace/project"}
{"type":"model_change","id":"e1","timestamp":"2024-01-02T03:04:06Z","model":"faux-1","role":"default"}
{"type":"message","id":"e2","parent_id":"e1","timestamp":"2024-01-02T03:04:07Z","message":{"role":"user","content":"list files","timestamp":1}}
{"type":"message","id":"e3","parent_id":"e2","timestamp":"2024-01-02T03:04:08Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"call-1","name":"ls","arguments":{}}],"api":"faux","provider":"faux","model":"faux-1","usage":{"input":1,"output":2,"cacheRead":0,"cacheWrite":0,"totalTokens":3,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"toolUse","timestamp":2}}
`

func TestVersion1JournalLoadsUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.jsonl")
	if err := os.WriteFile(path, []byte(v1Fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(testContext, path)
	if err != nil {
		t.Fatalf("load v1 journal: %v", err)
	}
	if len(loaded.Diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v, want none", loaded.Diagnostics)
	}
	if loaded.Header.Version != 1 || loaded.Model != "faux-1" || loaded.LastID != "e3" {
		t.Fatalf("loaded = %#v", loaded)
	}
	if loaded.LastRun != nil || len(loaded.Interrupted) != 0 {
		t.Fatalf("v1 journal reported runs: %#v / %#v", loaded.LastRun, loaded.Interrupted)
	}
	if len(loaded.Messages) != 2 || loaded.Messages[1].Role() != "assistant" {
		t.Fatalf("v1 messages changed: %#v", loaded.Messages)
	}

	store, _, err := OpenForResume(testContext, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage(testContext, userMessage("again")); err != nil {
		t.Fatal(err)
	}
	closeStore(t, store)
	lines := journalLines(t, path)
	var appended struct {
		Seq      uint64 `json:"seq"`
		ParentID string `json:"parent_id"`
		Lane     string `json:"lane"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &appended); err != nil {
		t.Fatal(err)
	}
	// Three v1 lines precede it, so numbering continues past them.
	if appended.Seq != 4 || appended.ParentID != "e3" || appended.Lane != LaneMain {
		t.Fatalf("appended envelope = %#v", appended)
	}
	reloaded, err := Load(testContext, path)
	if err != nil || len(reloaded.Diagnostics) != 0 || len(reloaded.Messages) != 3 {
		t.Fatalf("reload = %#v, %v", reloaded, err)
	}
}

func TestRecordsShareOneSeqAndStayOutOfTheEntryChain(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	runID := store.NewRunID()
	run := Meta{RunID: runID}
	known := ptypes.Usage{Input: 10, Output: 5, TotalTokens: 15, Cost: ptypes.Cost{Input: 0.1, Output: 0.2, Total: 0.3}}
	unknown := ptypes.Usage{Input: 1, TotalTokens: 1, Cost: ptypes.Cost{Unknown: true}}
	appendAll(t, store,
		RunStartedRecord{Meta: run, Model: "faux-1", Provider: "faux", PromptSummary: "hi"},
		MessageEntry{Meta: run, Message: userMessage("hi")},
		MessageEntry{Meta: run, Message: assistantMessage("hello")},
		UsageRecord{Meta: run, Model: "faux-1", Usage: known},
		UsageRecord{Meta: run, Model: "faux-1", Usage: unknown},
		RunFinishedRecord{Meta: run, StopReason: "completed", Turns: 1},
	)
	closeStore(t, store)

	var previousID string
	for index, line := range journalLines(t, store.Path())[1:] {
		var wire wireEntry
		if err := json.Unmarshal([]byte(line), &wire); err != nil {
			t.Fatal(err)
		}
		if wire.Seq != uint64(index+1) || wire.Lane != LaneMain || wire.RunID != runID {
			t.Fatalf("line %d envelope = %+v", index+2, wire)
		}
		if isRecord(wire.Type) {
			if wire.ID != "" || wire.ParentID != "" || len(wire.Payload) == 0 {
				t.Fatalf("record line %d = %s", index+2, line)
			}
			continue
		}
		if wire.ParentID != previousID {
			t.Fatalf("entry line %d parent = %q, want %q", index+2, wire.ParentID, previousID)
		}
		previousID = wire.ID
	}

	loaded, err := Load(testContext, store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v", loaded.Diagnostics)
	}
	if loaded.LastRun == nil || loaded.LastRun.RunID != runID || loaded.LastRun.StopReason != "completed" || !loaded.LastRun.Finished || loaded.LastRun.FinishedAt.IsZero() {
		t.Fatalf("last run = %#v", loaded.LastRun)
	}
	if loaded.UsageTotal.TotalTokens != 16 || !loaded.UsageTotal.Cost.Unknown {
		t.Fatalf("usage total = %#v; one unknown price must make the total unknown", loaded.UsageTotal)
	}
}

func TestReconcileWordsEachMissingResultFromTheRecords(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	run := Meta{RunID: store.NewRunID()}
	calls := []ptypes.ContentBlock{
		ptypes.NewToolCall("done", "read", json.RawMessage(`{"path":"a"}`)),
		ptypes.NewToolCall("unknown", "bash", json.RawMessage(`{"command":"make"}`)),
		ptypes.NewToolCall("lost", "edit", json.RawMessage(`{}`)),
		ptypes.NewToolCall("never", "grep", json.RawMessage(`{}`)),
	}
	assistant := ptypes.AssistantMessage{Content: calls, StopReason: ptypes.StopToolUse, Timestamp: 5}
	aborted := ptypes.AssistantMessage{
		Content:    []ptypes.ContentBlock{ptypes.NewToolCall("half", "write", nil)},
		StopReason: ptypes.StopAborted,
	}
	appendAll(t, store,
		RunStartedRecord{Meta: run, Model: "m"},
		MessageEntry{Meta: run, Message: userMessage("go")},
		MessageEntry{Meta: run, Message: assistant},
		ToolStartedRecord{Meta: run, ToolCallID: "done", Tool: "read"},
		ToolStartedRecord{Meta: run, ToolCallID: "unknown", Tool: "bash", Args: json.RawMessage(`{"command":"make"}`)},
		ToolStartedRecord{Meta: run, ToolCallID: "lost", Tool: "edit"},
		ToolFinishedRecord{Meta: run, ToolCallID: "done", OK: true},
		ToolFinishedRecord{Meta: run, ToolCallID: "lost", OK: false, Error: "stale hash"},
		MessageEntry{Meta: run, Message: ptypes.ToolResultMessage{ToolCallID: "done", ToolName: "read", Content: []ptypes.ContentBlock{ptypes.NewText("a")}}},
		MessageEntry{Meta: run, Message: userMessage("steer")},
		MessageEntry{Meta: run, Message: aborted},
	)
	closeStore(t, store)

	loaded, err := Load(testContext, store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 8 {
		t.Fatalf("messages = %d, want user, assistant, 1 real + 3 synthesized results, steer, aborted: %#v", len(loaded.Messages), loaded.Messages)
	}
	want := []struct{ id, contains string }{
		{"done", "a"},
		{"unknown", "outcome unknown"},
		{"lost", "result lost: the tool failed (stale hash)"},
		{"never", "not executed"},
	}
	for index, expected := range want {
		result, ok := loaded.Messages[2+index].(ptypes.ToolResultMessage)
		if !ok || result.ToolCallID != expected.id || !strings.Contains(result.Content[0].Text, expected.contains) {
			t.Fatalf("message %d = %#v, want result %q containing %q", 2+index, loaded.Messages[2+index], expected.id, expected.contains)
		}
		if index > 0 && !result.IsError {
			t.Fatalf("synthesized result %q is not an error", expected.id)
		}
	}
	if text := loaded.Messages[3].(ptypes.ToolResultMessage).Content[0].Text; !strings.Contains(text, textNoRerun) {
		t.Fatalf("bash result lacks the no-rerun note: %q", text)
	}
	if text := loaded.Messages[5].(ptypes.ToolResultMessage).Content[0].Text; !strings.Contains(text, textSafeToRerun) {
		t.Fatalf("grep result lacks the safe-to-rerun note: %q", text)
	}
	if len(loaded.Interrupted) != 1 || loaded.Interrupted[0].ToolCallID != "unknown" || loaded.Interrupted[0].Tool != "bash" || string(loaded.Interrupted[0].Args) != `{"command":"make"}` {
		t.Fatalf("interrupted = %#v", loaded.Interrupted)
	}
	if loaded.LastRun == nil || loaded.LastRun.Finished || len(loaded.Diagnostics) != 1 || !strings.Contains(loaded.Diagnostics[0].Message, "interrupted") {
		t.Fatalf("last run = %#v, diagnostics = %#v", loaded.LastRun, loaded.Diagnostics)
	}
	again, err := Load(testContext, store.Path())
	if err != nil || !reflect.DeepEqual(again.Messages, loaded.Messages) {
		t.Fatalf("reduction is not deterministic: %v", err)
	}
}

// An interruption is reported until a later run starts. After that the session
// has been resumed past it, so every later load must stay quiet about it.
func TestInterruptedRunDiagnosticStopsOnceALaterRunStarts(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	crashed := Meta{RunID: store.NewRunID()}
	appendAll(t, store,
		RunStartedRecord{Meta: crashed, Model: "m"},
		MessageEntry{Meta: crashed, Message: userMessage("first")},
		ToolStartedRecord{Meta: crashed, ToolCallID: "c1", Tool: "bash"},
	)
	interruptions := func() []string {
		t.Helper()
		loaded, err := Load(testContext, store.Path())
		if err != nil {
			t.Fatal(err)
		}
		var found []string
		for _, diagnostic := range loaded.Diagnostics {
			if strings.Contains(diagnostic.Message, "interrupted") {
				found = append(found, diagnostic.Message)
			}
		}
		return found
	}
	if found := interruptions(); len(found) != 1 || !strings.Contains(found[0], crashed.RunID) {
		t.Fatalf("crashed last run: interruptions = %q, want one naming %s", found, crashed.RunID)
	}

	resumed := Meta{RunID: store.NewRunID()}
	appendAll(t, store, RunStartedRecord{Meta: resumed, Model: "m", ContinuationOf: crashed.RunID})
	if found := interruptions(); len(found) != 1 || !strings.Contains(found[0], resumed.RunID) {
		t.Fatalf("resume in progress: interruptions = %q, want only the open run %s", found, resumed.RunID)
	}
	appendAll(t, store, RunFinishedRecord{Meta: resumed, StopReason: "completed"})
	if found := interruptions(); len(found) != 0 {
		t.Fatalf("after a completed resume: interruptions = %q, want none", found)
	}

	later := Meta{RunID: store.NewRunID()}
	appendAll(t, store,
		RunStartedRecord{Meta: later, Model: "m"},
		RunFinishedRecord{Meta: later, StopReason: "completed"},
	)
	if found := interruptions(); len(found) != 0 {
		t.Fatalf("after another completed run: interruptions = %q, want none", found)
	}
	closeStore(t, store)
}

func TestTornTailIsTrimmedAndUnterminatedTailIsKept(t *testing.T) {
	for _, test := range []struct {
		name     string
		tail     func(line string) string
		wantKept bool
	}{
		{name: "partial json", tail: func(line string) string { return line[:len(line)/2] }},
		{name: "missing newline", tail: func(line string) string { return line }, wantKept: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newTestStore(t, t.TempDir())
			run := Meta{RunID: store.NewRunID()}
			appendAll(t, store,
				RunStartedRecord{Meta: run, Model: "m"},
				ToolStartedRecord{Meta: run, ToolCallID: "c1", Tool: "bash"},
			)
			closeStore(t, store)
			lines := journalLines(t, store.Path())
			last := lines[len(lines)-1]
			contents := strings.Join(lines[:len(lines)-1], "\n") + "\n" + test.tail(last)
			if err := os.WriteFile(store.Path(), []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}

			resumed, loaded, err := OpenForResume(testContext, store.Path(), Options{})
			if err != nil {
				t.Fatal(err)
			}
			if kept := len(loaded.Interrupted) == 1; kept != test.wantKept {
				t.Fatalf("tool_started kept = %v, want %v (diagnostics %#v)", kept, test.wantKept, loaded.Diagnostics)
			}
			if err := resumed.Append(testContext, RunFinishedRecord{Meta: run, StopReason: "aborted"}); err != nil {
				t.Fatal(err)
			}
			closeStore(t, resumed)
			reloaded, err := Load(testContext, store.Path())
			if err != nil {
				t.Fatalf("reload after repair: %v", err)
			}
			if reloaded.LastRun == nil || !reloaded.LastRun.Finished {
				t.Fatalf("appended run_finished lost: %#v", reloaded.LastRun)
			}
			for _, diagnostic := range reloaded.Diagnostics {
				if strings.Contains(diagnostic.Message, "torn") || strings.Contains(diagnostic.Message, "seq") {
					t.Fatalf("repair left damage: %#v", reloaded.Diagnostics)
				}
			}
		})
	}
}

func TestTornMiddleLineIsAnErrorWithItsLineNumber(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	run := Meta{RunID: store.NewRunID()}
	appendAll(t, store,
		RunStartedRecord{Meta: run, Model: "m"},
		ToolStartedRecord{Meta: run, ToolCallID: "c1", Tool: "bash"},
		ToolFinishedRecord{Meta: run, ToolCallID: "c1", OK: true},
	)
	closeStore(t, store)
	lines := journalLines(t, store.Path())
	lines[2] = lines[2][:len(lines[2])/2]
	if err := os.WriteFile(store.Path(), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(testContext, store.Path())
	if err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("Load error = %v, want an error naming line 3", err)
	}
	if _, _, err := OpenForResume(testContext, store.Path(), Options{}); err == nil {
		t.Fatal("OpenForResume accepted a journal with a torn middle line")
	}
}

func TestUnknownRecordKindAndSeqRegressionAreDiagnosed(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	run := Meta{RunID: store.NewRunID()}
	appendAll(t, store, RunStartedRecord{Meta: run, Model: "m"})
	closeStore(t, store)
	contents, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	future := `{"type":"future_record","seq":2,"timestamp":"2024-01-02T03:04:09Z","lane":"main","run_id":"r","payload":{}}` + "\n"
	replayed := `{"type":"run_finished","seq":2,"timestamp":"2024-01-02T03:04:10Z","lane":"main","run_id":"` + run.RunID + `","payload":{"stop_reason":"completed","turns":0,"usage_total":{}}}` + "\n"
	if err := os.WriteFile(store.Path(), append(contents, future+replayed...), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(testContext, store.Path())
	if err != nil {
		t.Fatal(err)
	}
	var sawUnknown, sawSeq bool
	for _, diagnostic := range loaded.Diagnostics {
		sawUnknown = sawUnknown || strings.Contains(diagnostic.Message, `"future_record"`)
		sawSeq = sawSeq || strings.Contains(diagnostic.Message, "seq 2 does not increase")
	}
	if !sawUnknown || !sawSeq || loaded.LastRun == nil || !loaded.LastRun.Finished {
		t.Fatalf("loaded = %#v", loaded)
	}
}

func TestInterruptedToolsOnlyDescribeLastRun(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	crashed := Meta{RunID: store.NewRunID()}
	appendAll(t, store,
		RunStartedRecord{Meta: crashed, Model: "m"},
		MessageEntry{Meta: crashed, Message: ptypes.AssistantMessage{
			Content:    []ptypes.ContentBlock{ptypes.NewToolCall("crashed", "bash", nil)},
			StopReason: ptypes.StopToolUse,
		}},
		ToolStartedRecord{Meta: crashed, ToolCallID: "crashed", Tool: "bash"},
	)
	closeStore(t, store)

	resumed, loaded, err := OpenForResume(testContext, store.Path(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(t, resumed)
	if len(loaded.Interrupted) != 1 || loaded.Interrupted[0].RunID != crashed.RunID {
		t.Fatalf("first resume interrupted = %#v, want the crashed call", loaded.Interrupted)
	}
	completed := Meta{RunID: resumed.NewRunID()}
	appendAll(t, resumed,
		RunStartedRecord{Meta: completed, Model: "m"},
		MessageEntry{Meta: completed, Message: userMessage("continue")},
		MessageEntry{Meta: completed, Message: assistantMessage("done")},
		RunFinishedRecord{Meta: completed, StopReason: "completed", Turns: 1},
	)
	closeStore(t, resumed)

	again, loaded, err := OpenForResume(testContext, store.Path(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(t, again)
	if len(loaded.Interrupted) != 0 {
		t.Fatalf("second resume interrupted = %#v, want none from the completed last run", loaded.Interrupted)
	}
	if loaded.LastRun == nil || loaded.LastRun.RunID != completed.RunID || !loaded.LastRun.Finished || loaded.LastRun.StopReason != "completed" {
		t.Fatalf("last run = %#v, want the completed resume", loaded.LastRun)
	}
	unknown := 0
	for _, message := range loaded.Messages {
		if result, ok := message.(ptypes.ToolResultMessage); ok && result.ToolCallID == "crashed" {
			if !result.IsError || !strings.HasPrefix(result.Content[0].Text, "outcome unknown") {
				t.Fatalf("historical call result = %#v, want outcome unknown", result)
			}
			unknown++
		}
	}
	if unknown != 1 {
		t.Fatalf("historical unknown-outcome results = %d, want one", unknown)
	}
}

func appendAll(t *testing.T, store *Store, entries ...Entry) {
	t.Helper()
	for _, entry := range entries {
		if err := store.Append(testContext, entry); err != nil {
			t.Fatalf("append %T: %v", entry, err)
		}
	}
}

func journalLines(t *testing.T, path string) []string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(contents), "\n"), "\n")
}
