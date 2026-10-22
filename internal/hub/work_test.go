package hub

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/work"
)

func TestWorkSnapshotReducesOnceAndStripsPrivateResults(t *testing.T) {
	root := New(Snapshot{SessionID: "parent"}, nil)
	initial, updates, stop := root.Watch(16)
	defer stop()
	if len(initial.Queues) != 0 || len(initial.Jobs) != 0 {
		t.Fatal("unexpected seed")
	}
	queued := work.QueueItem{ID: "queue_one", Kind: work.Steer, Text: "accepted"}
	raw, err := json.Marshal(struct {
		Action string         `json:"action"`
		Item   work.QueueItem `json:"item"`
	}{"enqueued", queued})
	if err != nil {
		t.Fatal(err)
	}
	root.Emit(Update{Kind: "tempest/queue", Update: raw})
	job := work.Job{ID: "job_one", Kind: "task", State: "running", Cwd: "/private/worktree", Value: json.RawMessage(`{"secret":true}`), ResultText: strings.Repeat("界", 1000), Result: &work.Artifact{Path: "/private/result"}}
	raw, err = json.Marshal(struct {
		Job work.Job `json:"job"`
	}{job})
	if err != nil {
		t.Fatal(err)
	}
	root.Emit(Update{Kind: "tempest/job", Update: raw})
	snapshot := root.Snapshot()
	if snapshot.Seq != initial.Seq+2 || len(snapshot.Queues) != 1 || len(snapshot.Jobs) != 1 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	summary := snapshot.Jobs[0]
	if summary.Cwd != "" || summary.Result != nil || len(summary.Value) > 0 || len(summary.ResultText) > 512 {
		t.Fatalf("private/oversized summary=%+v", summary)
	}
	for index := range 2 {
		if update := <-updates; update.Seq != initial.Seq+uint64(index)+1 {
			t.Fatal("snapshot/update seam duplicated or lost sequence")
		}
	}
	snapshot.Queues[0].Text = "mutated"
	snapshot.Jobs[0].State = "mutated"
	if root.Snapshot().Queues[0].Text != "accepted" || root.Snapshot().Jobs[0].State != "running" {
		t.Fatal("snapshot aliases hub state")
	}
	root.Emit(Update{Kind: "tempest/queue", Update: json.RawMessage(`{"action":"consumed","item":{"id":"queue_one"}}`)})
	if len(root.Snapshot().Queues) != 0 {
		t.Fatal("consumption didn't remove pending queue item")
	}
}
func TestSeededJobsBoundedWithStableCursor(t *testing.T) {
	seed := Snapshot{SessionID: "parent"}
	for index := range 65 {
		seed.Jobs = append(seed.Jobs, work.Job{ID: fmt.Sprintf("job_%03d", index), State: "completed", Cwd: "/private"})
	}
	snapshot := New(seed, nil).Snapshot()
	if len(snapshot.Jobs) != 20 || !snapshot.JobsTruncated || snapshot.JobsCursor != "" || snapshot.Jobs[0].ID != "job_064" {
		t.Fatalf("bounded cursor snapshot=%+v", snapshot)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "/private") {
		t.Fatal("seed exposed private path")
	}
	for index := range 8 {
		seed.Jobs = append(seed.Jobs, work.Job{ID: fmt.Sprintf("active_%d", index), State: "running"})
	}
	snapshot = New(seed, nil).Snapshot()
	if len(snapshot.Jobs) != 28 || !snapshot.JobsTruncated {
		t.Fatalf("snapshot omitted an older active job: %+v", snapshot)
	}
}
