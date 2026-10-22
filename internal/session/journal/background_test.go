package journal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	types "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/work"
)

func bgFact(t *testing.T, store *Store, run string, kind EntryType, payload any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.AppendWorkFact(context.Background(), work.Fact{Kind: string(kind), RunID: run, Payload: raw}); err != nil {
		t.Fatal(err)
	}
}
func boundaryTable(t *testing.T, store *Store, check func(*testing.T, int, *Loaded)) {
	t.Helper()
	data, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	offset := 0
	line := 0
	for _, part := range bytes.SplitAfter(data, []byte{'\n'}) {
		if len(part) == 0 {
			continue
		}
		offset += len(part)
		line++
		t.Run(fmt.Sprintf("boundary_%02d", line-1), func(t *testing.T) {
			if err := os.WriteFile(store.Path(), data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(store.Path(), int64(offset)); err != nil {
				t.Fatal(err)
			}
			loaded, err := Load(context.Background(), store.Path())
			if err != nil {
				t.Fatal(err)
			}
			check(t, line-1, loaded)
		})
	}
	if err := os.WriteFile(store.Path(), data, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestBackgroundQueueLineBoundaryCrashes(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	bgFact(t, s, "", KindQueueEnqueued, QueueEnqueued{QueueID: "q1", Kind: work.Steer, Text: "first", Key: "first"})
	bgFact(t, s, "", KindQueueEnqueued, QueueEnqueued{QueueID: "q2", Kind: work.FollowUp, Text: "second", Key: "second"})
	bgFact(t, s, "", KindQueueCancelled, QueueCancelled{QueueID: "q2"})
	appendAll(t, s, RunStartedRecord{Meta: Meta{RunID: "r"}, Model: "model"}, MessageEntry{Meta: Meta{RunID: "r"}, QueueID: "q1", Message: userMessage("first")})
	closeStore(t, s)
	boundaryTable(t, s, func(t *testing.T, n int, l *Loaded) {
		want := 0
		if n == 1 {
			want = 1
		}
		if n == 2 {
			want = 2
		}
		if n == 3 || n == 4 {
			want = 1
		}
		if len(l.Queued) != want {
			t.Fatalf("pending=%d want %d", len(l.Queued), want)
		}
		if n >= 2 && len(l.QueueReceipts) != 2 {
			t.Fatal("lost durable keys")
		}
		if n == 5 && !l.QueueConsumed["q1"] {
			t.Fatal("message did not consume")
		}
	})
}
func TestBackgroundJobLineBoundaryCrashes(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	run := Meta{RunID: "r"}
	appendAll(t, s, RunStartedRecord{Meta: run, Model: "m"})
	assigned, err := s.AppendAssigned(context.Background(), MessageEntry{Meta: run, Metered: true, Message: types.AssistantMessage{Content: []types.ContentBlock{types.NewToolCall("raw", "task", json.RawMessage(`{}`))}, StopReason: types.StopToolUse}})
	if err != nil {
		t.Fatal(err)
	}
	call := assigned.ID + ":0"
	appendAll(t, s, ToolStartedRecord{Meta: run, ToolCallID: "raw", CallID: call, Tool: "task"})
	bgFact(t, s, "r", KindJobStarted, JobStarted{JobID: "job_one", IdentityDigest: strings.Repeat("a", 64), CallID: call, ToolCallID: "raw", LogicalRunID: "r", Kind: "task", Spec: json.RawMessage(`{"prompt":"work","subagent_type":"explore"}`), Environment: &JobEnvironment{Model: "m", Provider: "f"}, ChildSessionID: "0000000000000001", ChildJournal: s.Path() + ".d/children/0000000000000001/session.jsonl", Cwd: "/tmp"})
	bgFact(t, s, "r", KindJobRunning, JobRunning{JobID: "job_one", ChildRunID: "child-run"})
	bgFact(t, s, "r", KindJobFinished, JobFinished{JobID: "job_one", State: "completed", FinishedAt: time.Now(), ResultPreview: "done", Cleanup: "none"})
	bgFact(t, s, "r", KindQueueEnqueued, QueueEnqueued{QueueID: "completion", Kind: work.FollowUp, Text: "job done", Key: "job:job_one:terminal", SourceJobID: "job_one"})
	appendAll(t, s, MessageEntry{Meta: run, QueueID: "completion", Message: userMessage("job done")})
	closeStore(t, s)
	boundaryTable(t, s, func(t *testing.T, n int, l *Loaded) {
		if n < 4 {
			if len(l.Jobs) != 0 {
				t.Fatal("job before intent")
			}
			return
		}
		r := l.Recoveries[0]
		want := "recovering"
		if n >= 6 {
			want = "completed"
		}
		if r.Job.State != want {
			t.Fatalf("state=%s want=%s", r.Job.State, want)
		}
		if r.DeliveryPending != (n < 8) {
			t.Fatalf("delivery=%v at %d", r.DeliveryPending, n)
		}
		if n >= 4 {
			last := l.Messages[len(l.Messages)-1]
			_ = last
			found := false
			for _, m := range l.Messages {
				if v, ok := m.(types.ToolResultMessage); ok && strings.Contains(messageTextTool(v), "job_one") {
					found = true
				}
			}
			if !found {
				t.Fatal("lost intent must restore existing handle")
			}
		}
	})
}
func messageTextTool(m types.ToolResultMessage) string {
	var s string
	for _, b := range m.Content {
		s += b.Text
	}
	return s
}
func TestBackgroundCompactionLineBoundaryCrashes(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	run := Meta{RunID: "r"}
	source := strings.Repeat("b", 64)
	prep := strings.Repeat("c", 64)
	appendAll(t, s, RunStartedRecord{Meta: run, Model: "m"}, MessageEntry{Meta: run, Message: userMessage("source")})
	start := CompactionCall{CompactionID: "cmp_one", SourceHash: source, Phase: "history", Model: "m", Settings: json.RawMessage(`{}`), PreparationHash: prep}
	bgFact(t, s, "r", KindCompactionCallStarted, start)
	finish := CompactionCall{CompactionID: "cmp_one", SourceHash: source, Phase: "history", Model: "m", Status: "ok", Summary: "summary", Usage: types.Usage{Input: 3, TotalTokens: 3}, SourceRef: "compaction:" + s.ID() + ":cmp_one:history"}
	bgFact(t, s, "r", KindCompactionCallFinished, finish)
	replacement := []types.Message{userMessage("exact snapshot")}
	snapshot, err := s.WriteSnapshot("cmp_one", replacement)
	if err != nil {
		t.Fatal(err)
	}
	appendAll(t, s, CompactionEntry{Meta: run, Summary: "summary", CompactionID: "cmp_one", SourceHash: source, Snapshot: snapshot}, MessageEntry{Meta: run, Message: assistantMessage("after")})
	closeStore(t, s)
	boundaryTable(t, s, func(t *testing.T, n int, l *Loaded) {
		p := l.CompactionParts[source+":history"]
		if (p.Started != nil) != (n >= 3) || (p.Finished != nil) != (n >= 4) {
			t.Fatal("phase boundaries")
		}
		want := 0
		if n >= 4 {
			want = 3
		}
		if l.UsageTotal.TotalTokens != want {
			t.Fatalf("usage=%+v", l.UsageTotal)
		}
		if n >= 5 && messageText(l.Messages[0]) != "exact snapshot" {
			t.Fatal("snapshot not exact")
		}
		if n == 2 || n == 3 || n == 4 {
			if messageText(l.Messages[0]) != "source" {
				t.Fatal("uncommitted compaction changed history")
			}
		}
	})
	if err := os.WriteFile(snapshot.Path, []byte(`[]`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(context.Background(), s.Path()); err == nil {
		t.Fatal("corrupt snapshot accepted")
	}
}
func TestMeteredFallbackAndSourceDeduplication(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	run := Meta{RunID: "r"}
	usage := types.Usage{Input: 7, TotalTokens: 7}
	appendAll(t, s, RunStartedRecord{Meta: run, Model: "m"})
	m := assistantMessage("paid")
	m.Usage = usage
	meta, err := s.AppendAssigned(context.Background(), MessageEntry{Meta: run, Message: m, Metered: true})
	if err != nil {
		t.Fatal(err)
	}
	l, err := Load(context.Background(), s.Path())
	if err != nil || l.UsageTotal.TotalTokens != 7 {
		t.Fatalf("fallback: %+v %v", l, err)
	}
	receipt := UsageRecord{Meta: run, Model: "m", MessageID: meta.ID, SourceRef: "message:" + s.ID() + ":" + meta.ID, Usage: usage}
	appendAll(t, s, receipt, receipt)
	l, err = Load(context.Background(), s.Path())
	if err != nil || l.UsageTotal.TotalTokens != 7 || len(l.UsageSources) != 1 {
		t.Fatalf("dedupe: %+v %v", l, err)
	}
	receipt.Usage.Input++
	appendAll(t, s, receipt)
	if _, err = Load(context.Background(), s.Path()); err == nil {
		t.Fatal("conflicting receipt accepted")
	}
}
func TestUpgradePreservesInodeAndChildPath(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "old.jsonl")
			fixture := strings.Replace(v1Fixture, `"version":1`, fmt.Sprintf(`"version":%d`, version), 1)
			if err := os.WriteFile(path, []byte(fixture), 0600); err != nil {
				t.Fatal(err)
			}
			before, _ := os.Stat(path)
			s, l, err := OpenForResume(context.Background(), path, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			after, _ := os.Stat(path)
			if !os.SameFile(before, after) || l.Header.Version != 3 {
				t.Fatal("upgrade replaced locked inode")
			}
			raw, _ := os.ReadFile(path)
			if !bytes.Contains(raw, []byte(`"version":3`)) {
				t.Fatal("version not committed")
			}
			if _, _, err := OpenForResume(context.Background(), path, Options{}); !errors.Is(err, ErrSessionLocked) {
				t.Fatalf("second writer: %v", err)
			}
		})
	}
	s := newTestStore(t, t.TempDir())
	child, err := CreateChild(context.Background(), s.Path(), "0000000000000011", "/tmp", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	if child.ID() != "0000000000000011" {
		t.Fatal("child identity changed")
	}
	if _, err := CreateChild(context.Background(), s.Path(), child.ID(), "/tmp", Options{}); err == nil {
		t.Fatal("fixed child path reused")
	}
}
func TestLookupOriginalRunAfterCompactionAndForeignWriter(t *testing.T) {
	dir := t.TempDir()
	s, err := Create(context.Background(), dir, "/tmp/workspace", Options{})
	if err != nil {
		t.Fatal(err)
	}
	appendAll(t, s, RunStartedRecord{Meta: Meta{RunID: "first"}, Model: "m"}, MessageEntry{Meta: Meta{RunID: "first"}, Message: userMessage("question")}, MessageEntry{Meta: Meta{RunID: "first"}, Message: assistantMessage("first answer")}, RunFinishedRecord{Meta: Meta{RunID: "first"}, StopReason: "completed", Turns: 1})
	snapshot, err := s.WriteSnapshot("cmp_lookup", []types.Message{userMessage("collapsed")})
	if err != nil {
		t.Fatal(err)
	}
	appendAll(t, s, CompactionEntry{CompactionID: "cmp_lookup", SourceHash: strings.Repeat("a", 64), Snapshot: snapshot}, RunStartedRecord{Meta: Meta{RunID: "second"}, Model: "m"}, MessageEntry{Meta: Meta{RunID: "second"}, Message: assistantMessage("second answer")})
	first, err := LookupRun(context.Background(), dir, "/tmp/workspace", "first")
	if err != nil || first.FinalText != "first answer" || first.SessionID != s.ID() {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if _, err = LookupRun(context.Background(), dir, "/tmp/workspace", "second"); !errors.Is(err, ErrRunBusy) {
		t.Fatalf("foreign active=%v", err)
	}
	closeStore(t, s)
	second, err := LookupRun(context.Background(), dir, "/tmp/workspace", "second")
	if err != nil || !second.Interrupted || !second.Recoverable {
		t.Fatalf("interrupted=%+v %v", second, err)
	}
	firstAgain, err := LookupRun(context.Background(), dir, "/tmp/workspace", "first")
	if err != nil || !reflect.DeepEqual(first, firstAgain) {
		t.Fatal("later run rewrote old outcome")
	}
	if _, err = LookupRun(context.Background(), dir, "/tmp/workspace", "missing"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("missing=%v", err)
	}
}

func TestAttachedStopAndCleanupLineBoundaryCrashes(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	appendAll(t, s, RunStartedRecord{Meta: Meta{RunID: "original"}, Model: "m", LogicalRunID: "logical"})
	bgFact(t, s, "original", KindJobStarted, JobStarted{JobID: "job_attached", IdentityDigest: strings.Repeat("a", 64), CallID: "message:0", ToolCallID: "raw", LogicalRunID: "logical", Kind: "task", Spec: json.RawMessage(`{"prompt":"work","subagent_type":"general"}`), Environment: &JobEnvironment{Model: "m", Provider: "f"}, ChildSessionID: "0000000000000022", ChildJournal: s.Path() + ".d/children/0000000000000022/session.jsonl", Cwd: "/tmp/tree/sub", Worktree: &JobWorkspace{Repo: "/tmp/repo", CommonDir: "/tmp/repo/.git", BaseCommit: strings.Repeat("a", 40), Path: "/tmp/tree", RelativeCwd: "sub", Marker: "/tmp/marker"}})
	appendAll(t, s, RunStartedRecord{Meta: Meta{RunID: "continuation"}, Model: "m", LogicalRunID: "logical", ContinuationOf: "original"})
	bgFact(t, s, "continuation", KindJobAttached, JobAttached{JobID: "job_attached", RunID: "continuation", LogicalRunID: "logical"})
	bgFact(t, s, "continuation", KindRunStopRequested, RunStopRequested{Reason: "max_cost"})
	bgFact(t, s, "continuation", KindJobCancelRequested, JobCancelRequested{JobID: "job_attached", Reason: "max_cost"})
	bgFact(t, s, "continuation", KindJobFinished, JobFinished{JobID: "job_attached", State: "cancelled", FinishedAt: time.Now(), Reason: "max_cost", Cleanup: "retained"})
	bgFact(t, s, "", KindJobCleaned, JobCleaned{JobID: "job_attached", Path: "/tmp/tree"})
	closeStore(t, s)
	boundaryTable(t, s, func(t *testing.T, n int, l *Loaded) {
		if n < 2 {
			return
		}
		r := l.Recoveries[0]
		if r.Job.RunID != "original" || r.Job.Cwd != "/tmp/tree/sub" {
			t.Fatalf("lost original identity/cwd: %+v", r.Job)
		}
		if r.CancelRequested != (n >= 5) {
			t.Fatalf("continuation stop ignored at %d: %+v", n, r)
		}
		if n >= 7 && r.Job.State != "cancelled" {
			t.Fatal("cancel terminal not restored")
		}
		if n == 8 && r.Job.Cleanup != "removed" {
			t.Fatal("cleanup receipt not restored")
		}
		if n >= 3 && (len(l.Runs) != 2 || l.Runs[1].ContinuationOf != "original" || l.Runs[1].LogicalRunID != "logical") {
			t.Fatal("run provenance lost")
		}
	})
}

func TestBashIntentWithoutRunningIsUnknown(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	appendAll(t, s, RunStartedRecord{Meta: Meta{RunID: "root"}, Model: "m"})
	bgFact(t, s, "root", KindJobStarted, JobStarted{JobID: "job_bash", IdentityDigest: strings.Repeat("a", 64), CallID: "message:0", ToolCallID: "raw", LogicalRunID: "root", Kind: "bash", Spec: json.RawMessage(`{"command":"increment-ledger","timeout_seconds":60}`), Cwd: "/tmp", OutputPath: s.Path() + ".d/jobs/job_bash/output.log"})
	l, err := Load(context.Background(), s.Path())
	if err != nil || len(l.Jobs) != 1 || l.Jobs[0].State != "unknown" || l.Jobs[0].ExitCode != nil || !l.Recoveries[0].DeliveryPending {
		t.Fatalf("bash outcome=%+v err=%v", l, err)
	}
}

func TestLookupBoundAndDuplicateRunIdentity(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Create(ctx, dir, "/tmp/lookup", Options{})
	if err != nil {
		t.Fatal(err)
	}
	appendAll(t, s, RunStartedRecord{Meta: Meta{RunID: "same"}, Model: "m"}, RunFinishedRecord{Meta: Meta{RunID: "same"}, StopReason: "completed"})
	closeStore(t, s)
	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(filepath.Dir(s.Path()), "duplicate.jsonl")
	if err := os.WriteFile(copyPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LookupRun(ctx, dir, "/tmp/lookup", "same"); !errors.Is(err, ErrRunAmbiguous) {
		t.Fatalf("ambiguity=%v", err)
	}
	if err := os.Truncate(copyPath, 257<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := LookupRun(ctx, dir, "/tmp/lookup", "missing"); !errors.Is(err, ErrLookupLimit) {
		t.Fatalf("lookup limit=%v", err)
	}
}
