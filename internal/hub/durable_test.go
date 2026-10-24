package hub

import (
	"encoding/json"
	"testing"

	"go.harness.dev/harness/tempest/durable"
)

func TestDurableSnapshotLiveBoundaryAndSlowSubscriber(t *testing.T) {
	h := New(Snapshot{SessionID: "s"}, nil)
	record := durable.Record{Change: durable.Change{SessionID: "s", Kind: durable.DocumentKind, ID: "doc", Version: 1, Value: json.RawMessage(`{"value":1}`)}, Seq: 1}
	page := durable.Page{Records: []durable.Record{record}, Seq: 1, NextCursor: "next"}
	h.SetDurable(1, page, durable.Page{}, durable.Page{})
	snapshot, updates, stop := h.Watch(1)
	defer stop()
	if snapshot.DurableSeq != 1 || snapshot.DocumentsCursor != "next" || len(snapshot.Documents) != 1 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	page.Records[0].Value[0] = 'x'
	snapshot.Documents[0].Value[0] = 'y'
	if !json.Valid(h.Snapshot().Documents[0].Value) {
		t.Fatal("snapshot aliases committed data")
	}
	h.SetDurable(2, durable.Page{}, durable.Page{}, durable.Page{})
	update := <-updates
	if update.Kind != "tempest/durable" || update.Seq != snapshot.Seq+1 {
		t.Fatalf("gap or overlap: snapshot=%d update=%+v", snapshot.Seq, update)
	}
	h.SetDurable(3, durable.Page{}, durable.Page{}, durable.Page{})
	h.SetDurable(4, durable.Page{}, durable.Page{}, durable.Page{})
	if gap := <-updates; gap.Kind != "tempest/gap" {
		t.Fatalf("slow subscriber=%+v", gap)
	}
	if _, ok := <-updates; ok {
		t.Fatal("slow subscriber retained")
	}
	h.SetDurable(2, durable.Page{}, durable.Page{}, durable.Page{})
	if h.Snapshot().DurableSeq != 4 {
		t.Fatal("stale commit regressed durable watermark")
	}
}
