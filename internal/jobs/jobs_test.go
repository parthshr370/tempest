package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.harness.dev/harness/internal/work"
)

type memoryHost struct {
	mu       sync.Mutex
	facts    []work.Fact
	queued   map[string]work.QueueInput
	changed  chan struct{}
	failKind string
	runner   work.TaskRunner
	manager  *Manager
}

func newHost(t *testing.T, runner work.TaskRunner) *memoryHost {
	t.Helper()
	host := &memoryHost{queued: make(map[string]work.QueueInput), changed: make(chan struct{}), runner: runner}
	directory := t.TempDir()
	callbacks := work.Host{SessionID: "parent", JournalPath: filepath.Join(directory, "parent.jsonl"), Cwd: directory, Queues: host, Append: host.append, Enqueue: host.enqueue, PrepareTask: func(context.Context, work.TaskSpec) (work.TaskEnvironment, error) {
		return work.TaskEnvironment{Ceiling: work.Ceiling{Mode: "default", Tools: []string{"read", "bash"}}, Model: "test", Provider: "test"}, nil
	}, RunTask: runner}
	manager, err := New(callbacks, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	host.manager = manager
	if err = manager.Start(context.Background(), work.RunInfo{SessionID: "parent", RunID: "run", LogicalRunID: "run"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Finish(context.Background(), "aborted"); err != nil && host.failKind == "" {
			t.Error(err)
		}
	})
	return host
}
func (host *memoryHost) append(ctx context.Context, fact work.Fact) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if fact.Kind == host.failKind {
		return errors.New("injected persistence failure")
	}
	if err := strictFact(fact); err != nil {
		return err
	}
	host.facts = append(host.facts, fact)
	return nil
}
func (host *memoryHost) enqueue(_ context.Context, input work.QueueInput) (work.QueueItem, error) {
	host.mu.Lock()
	defer host.mu.Unlock()
	if input.SourceJobID == "" || input.Key != "job:"+input.SourceJobID+":terminal" || input.Kind != work.FollowUp || len(input.Text) > maxCompletionBytes {
		return work.QueueItem{}, errors.New("invalid internal completion")
	}
	if prior, ok := host.queued[input.Key]; ok && prior != input {
		return work.QueueItem{}, errors.New("conflicting completion")
	}
	host.queued[input.Key] = input
	close(host.changed)
	host.changed = make(chan struct{})
	return work.QueueItem{ID: input.Key, Kind: input.Kind, Text: input.Text, SourceJobID: input.SourceJobID}, nil
}
func (host *memoryHost) Pending() (bool, <-chan struct{}) {
	host.mu.Lock()
	defer host.mu.Unlock()
	return len(host.queued) > 0, host.changed
}
func (host *memoryHost) PendingJobIDs() []string {
	host.mu.Lock()
	defer host.mu.Unlock()
	ids := make([]string, 0, len(host.queued))
	for _, input := range host.queued {
		ids = append(ids, input.SourceJobID)
	}
	return ids
}
func (host *memoryHost) Take(context.Context, work.QueueKind) ([]work.QueuedMessage, error) {
	return nil, nil
}
func (host *memoryHost) consume() {
	host.mu.Lock()
	host.queued = make(map[string]work.QueueInput)
	close(host.changed)
	host.changed = make(chan struct{})
	host.mu.Unlock()
}
func invocation(index int) context.Context {
	return work.WithInvocation(context.Background(), work.Invocation{RunID: "run", CallID: fmt.Sprintf("message:%d", index), ToolCallID: "reused"})
}
func awaitJob(t *testing.T, manager *Manager, jobID string) work.Job {
	t.Helper()
	record, err := manager.find(jobID)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-record.done:
	case <-time.After(30 * time.Second): // 16 MiB output-limit jobs need this under -race with the full suite
		t.Fatal("job failed to join")
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return cloneJob(record.recovery.Job)
}

func TestIdentityUsesOccurrence(t *testing.T) {
	first, child, digest := Identity("parent", "message:0")
	again, againChild, againDigest := Identity("parent", "message:0")
	second, _, _ := Identity("parent", "message:1")
	if first != again || child != againChild || digest != againDigest || first == second || len(first) != 36 || len(child) != 16 || len(digest) != 64 {
		t.Fatal(first, child, digest, second)
	}
}
func TestAdmissionIsNonblockingBoundedAndJoined(t *testing.T) {
	entered := make(chan struct{}, 4)
	var active atomic.Int32
	host := newHost(t, func(ctx context.Context, _ work.TaskLaunch) (work.TaskResult, error) {
		active.Add(1)
		defer active.Add(-1)
		entered <- struct{}{}
		<-ctx.Done()
		return work.TaskResult{}, ctx.Err()
	})
	jobs := make([]work.Job, 0, 4)
	for index := range 4 {
		job, err := host.manager.StartTask(invocation(index), work.TaskSpec{Prompt: "work", SubagentType: "explore"})
		if err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, job)
		if job.State != "starting" {
			t.Fatal(job)
		}
	}
	for range jobs {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("runner did not start")
		}
	}
	if _, err := host.manager.StartTask(invocation(4), work.TaskSpec{Prompt: "work", SubagentType: "explore"}); !errors.Is(err, ErrJobLimit) {
		t.Fatal(err)
	}
	if err := host.manager.Finish(context.Background(), "max_cost"); err != nil {
		t.Fatal(err)
	}
	if active.Load() != 0 {
		t.Fatal("unjoined child")
	}
	for _, job := range jobs {
		settled := awaitJob(t, host.manager, job.ID)
		if settled.State != "cancelled" {
			t.Fatal(settled)
		}
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.queued) != 4 {
		t.Fatal(host.queued)
	}
	stopIndex, cancelIndex := -1, -1
	for index, fact := range host.facts {
		if fact.Kind == "run_stop_requested" && stopIndex < 0 {
			stopIndex = index
		}
		if fact.Kind == "job_cancel_requested" && cancelIndex < 0 {
			cancelIndex = index
		}
	}
	if stopIndex < 0 || cancelIndex < stopIndex {
		t.Fatal("stop intent did not precede cancellation")
	}
}
func TestCompletionCreditsPersistUntilConsumption(t *testing.T) {
	host := newHost(t, func(context.Context, work.TaskLaunch) (work.TaskResult, error) {
		return work.TaskResult{Text: "done", StopReason: "completed"}, nil
	})
	for index := range 8 {
		job, err := host.manager.StartTask(invocation(index), work.TaskSpec{Prompt: "work", SubagentType: "explore"})
		if err != nil {
			t.Fatal(index, err)
		}
		awaitJob(t, host.manager, job.ID)
	}
	if _, err := host.manager.StartTask(invocation(8), work.TaskSpec{Prompt: "work", SubagentType: "explore"}); !errors.Is(err, ErrJobLimit) {
		t.Fatal(err)
	}
	host.consume()
	job, err := host.manager.StartTask(invocation(8), work.TaskSpec{Prompt: "work", SubagentType: "explore"})
	if err != nil {
		t.Fatal(err)
	}
	awaitJob(t, host.manager, job.ID)
}
func TestTypedYieldStrictAndEvidencePreserved(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","$defs":{"n":{"type":"integer"}},"properties":{"count":{"$ref":"#/$defs/n"}},"required":["count"],"additionalProperties":false}`)
	cases := []struct{ name, text, state string }{{"valid", `{"count":3}`, "completed"}, {"coercion", `{"count":"3"}`, "failed"}, {"prose", `{"count":3} trailing`, "failed"}, {"markdown", "```json\n{}\n```", "failed"}, {"oversized", `"` + strings.Repeat("x", 64<<10) + `"`, "failed"}}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			host := newHost(t, func(context.Context, work.TaskLaunch) (work.TaskResult, error) {
				return work.TaskResult{Text: test.text}, nil
			})
			job, err := host.manager.StartTask(invocation(1), work.TaskSpec{Prompt: "work", SubagentType: "explore", OutputSchema: schema})
			if err != nil {
				t.Fatal(err)
			}
			job = awaitJob(t, host.manager, job.ID)
			if job.State != test.state || job.Result == nil {
				t.Fatal(job)
			}
			original, err := os.ReadFile(job.Result.Path)
			if err != nil || string(original) != test.text {
				t.Fatal("lost evidence", err)
			}
			if job.State == "failed" && job.Reason != "yield_invalid" {
				t.Fatal(job)
			}
		})
	}
	for _, invalid := range []string{`true`, `{"$ref":"https://example.test/schema"}`, `{"$id":"file:///tmp/schema"}`, `{"type":"not-a-type"}`} {
		host := newHost(t, func(context.Context, work.TaskLaunch) (work.TaskResult, error) {
			t.Fatal("invalid schema started child")
			return work.TaskResult{}, nil
		})
		if _, err := host.manager.StartTask(invocation(1), work.TaskSpec{Prompt: "work", SubagentType: "explore", OutputSchema: json.RawMessage(invalid)}); err == nil {
			t.Fatal(invalid)
		}
	}
}
func TestSharedGeneralQuotaAndNoPatch(t *testing.T) {
	release := make(chan struct{})
	host := newHost(t, func(ctx context.Context, launch work.TaskLaunch) (work.TaskResult, error) {
		if launch.Job.Isolation != "shared" {
			t.Error(launch.Job)
		}
		select {
		case <-release:
		case <-ctx.Done():
			return work.TaskResult{}, ctx.Err()
		}
		return work.TaskResult{Text: "shared result"}, nil
	})
	job, err := host.manager.StartTask(invocation(1), work.TaskSpec{Prompt: "work", SubagentType: "general"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = host.manager.StartTask(invocation(2), work.TaskSpec{Prompt: "work", SubagentType: "general"}); !errors.Is(err, ErrJobLimit) {
		t.Fatal(err)
	}
	close(release)
	job = awaitJob(t, host.manager, job.ID)
	if job.Patch != nil || job.Cleanup != "none" || job.Cwd != host.manager.host.Cwd {
		t.Fatal(job)
	}
}
func TestPersistFailurePreventsLaunch(t *testing.T) {
	var calls atomic.Int32
	host := newHost(t, func(context.Context, work.TaskLaunch) (work.TaskResult, error) {
		calls.Add(1)
		return work.TaskResult{}, nil
	})
	host.mu.Lock()
	host.failKind = "job_started"
	host.mu.Unlock()
	if _, err := host.manager.StartTask(invocation(1), work.TaskSpec{Prompt: "work", SubagentType: "explore"}); err == nil {
		t.Fatal("missing sync failure")
	}
	if calls.Load() != 0 {
		t.Fatal("effect before intent")
	}
}
func TestPanicSettlesAndDelivers(t *testing.T) {
	host := newHost(t, func(context.Context, work.TaskLaunch) (work.TaskResult, error) { panic("injected") })
	job, err := host.manager.StartTask(invocation(1), work.TaskSpec{Prompt: "work", SubagentType: "explore"})
	if err != nil {
		t.Fatal(err)
	}
	job = awaitJob(t, host.manager, job.ID)
	if job.State != "failed" || job.Reason != "worker_panic" {
		t.Fatal(job)
	}
}
func TestWaitTimeoutDoesNotCancelAndJobsAreSummaries(t *testing.T) {
	host := newHost(t, func(ctx context.Context, _ work.TaskLaunch) (work.TaskResult, error) {
		<-ctx.Done()
		return work.TaskResult{}, ctx.Err()
	})
	job, err := host.manager.StartTask(invocation(1), work.TaskSpec{Prompt: "work", SubagentType: "explore"})
	if err != nil {
		t.Fatal(err)
	}
	states, err := host.manager.Wait(context.Background(), []string{job.ID}, time.Millisecond)
	if err != nil || len(states) != 1 || terminal(states[0].State) {
		t.Fatal(states, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = host.manager.Wait(ctx, []string{job.ID}, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	page, err := host.manager.Jobs(context.Background(), "", 20)
	if err != nil || len(page.Jobs) != 1 || page.Jobs[0].Cwd != "" {
		t.Fatal(page, err)
	}
	if _, err = host.manager.Cancel(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	awaitJob(t, host.manager, job.ID)
}
func TestRecoveryOnlyExecutesOnContinueAndNeverRestartsBash(t *testing.T) {
	host := newHost(t, nil)
	var calls atomic.Int32
	parent := host.manager.host
	parent.RunTask = func(_ context.Context, launch work.TaskLaunch) (work.TaskResult, error) {
		calls.Add(1)
		if !launch.Resume {
			t.Error("recovery not marked")
		}
		return work.TaskResult{Text: "resumed"}, nil
	}
	recoveries := make([]work.Recovery, 0, 2)
	for index, kind := range []string{"task", "bash"} {
		call := fmt.Sprintf("old:%d", index)
		jobID, childID, digest := Identity(parent.SessionID, call)
		job := work.Job{ID: jobID, Kind: kind, State: "running", RunID: "old", LogicalRunID: "old", ChildSessionID: childID, Cwd: parent.Cwd, Cleanup: "none"}
		recovery := work.Recovery{Job: job, Invocation: work.Invocation{RunID: "old", CallID: call}, IdentityDigest: digest, DeliveryPending: true}
		if kind == "task" {
			recovery.Task = &work.TaskSpec{Prompt: "original", SubagentType: "explore"}
			recovery.Environment = &work.TaskEnvironment{}
			recovery.ChildJournal = filepath.Join(parent.JournalPath+".d", "children", childID, "session.jsonl")
		} else {
			recovery.Job.ChildSessionID = ""
			recovery.Bash = &work.BashSpec{Command: "must not run", TimeoutSeconds: 10}
		}
		recoveries = append(recoveries, recovery)
	}
	manager, err := New(parent, Options{}, recoveries)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Finish(context.Background(), "aborted"); err != nil {
			t.Error(err)
		}
	})
	if calls.Load() != 0 || !manager.RecoveryRequired() {
		t.Fatal("opening executed recovery")
	}
	if err = manager.Start(context.Background(), work.RunInfo{RunID: "new"}); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatal(err)
	}
	if err = manager.Start(context.Background(), work.RunInfo{RunID: "continued", LogicalRunID: "old", Resume: true}); err != nil {
		t.Fatal(err)
	}
	for _, recovery := range recoveries {
		job := awaitJob(t, manager, recovery.Job.ID)
		if job.RunID != "old" {
			t.Fatal(job)
		}
		if job.Kind == "bash" && (job.State != "unknown" || job.ExitCode != nil) {
			t.Fatal(job)
		}
	}
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
	if err = manager.Finish(context.Background(), "completed"); err != nil {
		t.Fatal(err)
	}
}

func TestAcceptedLimitSurvivesContinuationAndFinishIsIdempotent(t *testing.T) {
	host := newHost(t, func(context.Context, work.TaskLaunch) (work.TaskResult, error) {
		return work.TaskResult{Text: "done"}, nil
	})
	for index := range maxAccepted {
		job, err := host.manager.StartTask(invocation(index), work.TaskSpec{Prompt: "work", SubagentType: "explore"})
		if err != nil {
			t.Fatal(index, err)
		}
		awaitJob(t, host.manager, job.ID)
		host.consume()
	}
	if err := host.manager.Finish(context.Background(), "max_turns"); err != nil {
		t.Fatal(err)
	}
	if err := host.manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	stops := 0
	for _, fact := range host.facts {
		if fact.Kind == "run_stop_requested" {
			stops++
			if !strings.Contains(string(fact.Payload), "max_turns") {
				t.Fatal(string(fact.Payload))
			}
		}
	}
	host.mu.Unlock()
	if stops != 1 {
		t.Fatal(stops)
	}
	if err := host.manager.Start(context.Background(), work.RunInfo{RunID: "continued", LogicalRunID: "run", Resume: true}); err != nil {
		t.Fatal(err)
	}
	ctx := work.WithInvocation(context.Background(), work.Invocation{RunID: "continued", CallID: "continued:0", ToolCallID: "raw"})
	if _, err := host.manager.StartTask(ctx, work.TaskSpec{Prompt: "work", SubagentType: "explore"}); !errors.Is(err, ErrJobLimit) {
		t.Fatal(err)
	}
}

func TestCancelDuringStartedPublicationCannotLaunchUncancelledWork(t *testing.T) {
	host := newHost(t, func(ctx context.Context, _ work.TaskLaunch) (work.TaskResult, error) {
		if ctx.Err() == nil {
			t.Error("cancelled intent launched an uncancelled child")
		}
		return work.TaskResult{}, ctx.Err()
	})
	host.manager.host.Publish = func(ctx context.Context, event work.Event) error {
		if event.Kind == "job" && event.Action == "started" {
			_, err := host.manager.Cancel(ctx, event.Job.ID)
			return err
		}
		return nil
	}
	job, err := host.manager.StartTask(invocation(1), work.TaskSpec{Prompt: "work", SubagentType: "explore"})
	if err != nil {
		t.Fatal(err)
	}
	job = awaitJob(t, host.manager, job.ID)
	if job.State != "cancelled" {
		t.Fatal(job)
	}
}

func TestTypedCompletionAndWaitFrameBounds(t *testing.T) {
	host := newHost(t, func(context.Context, work.TaskLaunch) (work.TaskResult, error) {
		return work.TaskResult{Text: `"` + strings.Repeat("<", 60<<10) + `"`}, nil
	})
	ids := make([]string, 0, maxCredits)
	for index := range maxCredits {
		job, err := host.manager.StartTask(invocation(index), work.TaskSpec{Prompt: "work", SubagentType: "explore", OutputSchema: json.RawMessage(`{"type":"string"}`)})
		if err != nil {
			t.Fatal(err)
		}
		awaitJob(t, host.manager, job.ID)
		ids = append(ids, job.ID)
	}
	jobs, err := host.manager.Wait(context.Background(), ids, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(jobs)
	if err != nil || len(encoded) >= 1<<20 {
		t.Fatal(len(encoded), err)
	}
	for _, job := range jobs {
		if !job.ValueInResult || job.Result == nil || len(job.Value) > 0 {
			t.Fatal(job)
		}
	}
	one, err := host.manager.Wait(context.Background(), ids[:1], time.Second)
	if err != nil || len(one[0].Value) == 0 {
		t.Fatal(one, err)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	for _, completion := range host.queued {
		if len(completion.Text) > maxCompletionBytes || !strings.Contains(completion.Text, "result") {
			t.Fatal("lost or oversized completion reference")
		}
	}
}

func TestSchemaDataKeysAreNotReferencesAndExternalLoaderIsDisabled(t *testing.T) {
	valid := []string{`{"type":"object","properties":{"$ref":{"type":"string"}}}`, `{"const":{"$ref":"ordinary data"}}`, `{"properties":{"ok":true,"no":false}}`}
	for _, document := range valid {
		if _, err := compileSchema(json.RawMessage(document)); err != nil {
			t.Fatal(document, err)
		}
	}
	for _, document := range []string{`{"$defs":{"bad":{"$ref":"file:///etc/passwd"}}}`, `{"$schema":"file:///etc/passwd"}`} {
		if _, err := compileSchema(json.RawMessage(document)); err == nil {
			t.Fatal(document)
		}
	}
}

func TestRetainedWorktreeCapDoesNotBlockExplore(t *testing.T) {
	host := newHost(t, func(context.Context, work.TaskLaunch) (work.TaskResult, error) {
		return work.TaskResult{Text: "read-only result"}, nil
	})
	host.manager.mu.Lock()
	for index := range maxRetained {
		id := fmt.Sprintf("retained_%d", index)
		host.manager.entries[id] = &entry{recovery: work.Recovery{Job: work.Job{ID: id, Kind: "task", State: "completed", Isolation: "worktree", Cleanup: "retained"}}, joined: true}
	}
	host.manager.mu.Unlock()
	if _, err := host.manager.StartTask(invocation(1), work.TaskSpec{Prompt: "work", SubagentType: "general"}); !errors.Is(err, ErrJobLimit) {
		t.Fatal(err)
	}
	job, err := host.manager.StartTask(invocation(2), work.TaskSpec{Prompt: "inspect", SubagentType: "explore"})
	if err != nil {
		t.Fatal(err)
	}
	awaitJob(t, host.manager, job.ID)
}

func TestRecoveredCancelIntentAndIdleCancelNeverRunChild(t *testing.T) {
	for _, idle := range []bool{false, true} {
		t.Run(fmt.Sprint("idle_", idle), func(t *testing.T) {
			host := newHost(t, nil)
			parent := host.manager.host
			parent.RunTask = func(context.Context, work.TaskLaunch) (work.TaskResult, error) {
				t.Error("cancelled recovery executed")
				return work.TaskResult{}, nil
			}
			jobID, childID, digest := Identity(parent.SessionID, "interrupted:0")
			recovery := work.Recovery{Job: work.Job{ID: jobID, Kind: "task", State: "running", RunID: "old", LogicalRunID: "old", ChildSessionID: childID, Cwd: parent.Cwd, Cleanup: "none"}, Invocation: work.Invocation{RunID: "old", CallID: "interrupted:0", ToolCallID: "raw"}, IdentityDigest: digest, Task: &work.TaskSpec{Prompt: "original", SubagentType: "explore"}, Environment: &work.TaskEnvironment{}, ChildJournal: filepath.Join(parent.JournalPath+".d", "children", childID, "session.jsonl"), CancelRequested: !idle}
			manager, err := New(parent, Options{}, []work.Recovery{recovery})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := manager.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})
			if idle {
				_, err = manager.Cancel(context.Background(), jobID)
			} else {
				err = manager.Start(context.Background(), work.RunInfo{RunID: "continued", LogicalRunID: "old", Resume: true})
			}
			if err != nil {
				t.Fatal(err)
			}
			job := awaitJob(t, manager, jobID)
			if job.State != "cancelled" || manager.RecoveryRequired() {
				t.Fatal(job)
			}
			host.mu.Lock()
			count := len(host.queued)
			host.mu.Unlock()
			if count != 1 {
				t.Fatal(count)
			}
		})
	}
}
