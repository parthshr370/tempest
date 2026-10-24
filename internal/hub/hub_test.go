package hub

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/agent"
)

func messageMapper(ev agent.AgentEvent) []Update {
	raw, _ := json.Marshal(map[string]any{"sessionUpdate": "user_message_chunk", "messageId": ev.ToolCallID, "content": map[string]any{"type": "text", "text": ev.ToolName}})
	return []Update{{Kind: "user_message_chunk", Update: raw}}
}
func TestWatchAtomicSnapshotThenLiveTenThousandEvents(t *testing.T) {
	const events = 10000
	const watchers = 32
	h := New(Snapshot{SessionID: "s"}, messageMapper)
	rng := rand.New(rand.NewPCG(73, 19))
	joins := make(map[int][]int)
	for i := range watchers {
		at := rng.IntN(events)
		joins[at] = append(joins[at], i)
	}
	type observation struct {
		snap   Snapshot
		prefix []Update
		live   []Update
	}
	observations := make([]observation, watchers)
	var attached, consumed sync.WaitGroup
	attached.Add(watchers)
	consumed.Add(watchers)
	listener := h.Listener()
	for i := range events {
		for _, index := range joins[i] {
			go func() {
				defer consumed.Done()
				snap, ch, stop := h.Watch(events + 1)
				defer stop()
				prefix, err := h.History(snap.HistoryEpoch, 0, snap.HistoryLen)
				if err != nil {
					t.Error(err)
				}
				observations[index] = observation{snap: snap, prefix: prefix}
				attached.Done()
				for u := range ch {
					observations[index].live = append(observations[index].live, u)
				}
			}()
		}
		if err := listener(context.Background(), agent.AgentEvent{ToolCallID: "m", ToolName: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	attached.Wait()
	h.Close()
	consumed.Wait()
	final := h.Snapshot()
	history, err := h.History(final.HistoryEpoch, 0, final.HistoryLen)
	if err != nil {
		t.Fatal(err)
	}
	for i, o := range observations {
		expected := o.snap.Seq + 1
		for _, u := range o.live {
			if u.Kind == "tempest/gap" || u.Seq != expected {
				t.Fatalf("watcher %d: got %s seq %d, expected %d", i, u.Kind, u.Seq, expected)
			}
			expected++
		}
		if expected != final.Seq+1 {
			t.Fatalf("watcher %d ended at %d, final %d", i, expected-1, final.Seq)
		}
		reconstructed := append(o.prefix, o.live...)
		if !reflect.DeepEqual(reconstructed, history) {
			t.Fatalf("watcher %d reconstructed %d updates, final %d", i, len(reconstructed), len(history))
		}
		if got := o.prefix[max(0, len(o.prefix)-200):]; !reflect.DeepEqual(got, o.snap.History) {
			t.Fatalf("watcher %d snapshot tail overlaps live stream", i)
		}
	}
}
func TestSlowSubscriberCannotFailOrBlockListener(t *testing.T) {
	h := New(Snapshot{SessionID: "s"}, messageMapper)
	_, slow, stopSlow := h.Watch(1)
	defer stopSlow()
	_, fast, stopFast := h.Watch(10000)
	defer stopFast()
	done := make(chan error, 1)
	go func() {
		listener := h.Listener()
		for range 10000 {
			if err := listener(context.Background(), agent.AgentEvent{}); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	gaps := 0
	for u := range slow {
		if u.Kind != "tempest/gap" {
			t.Fatalf("slow channel retained stale update: %s", u.Kind)
		}
		gaps++
	}
	if gaps != 1 {
		t.Fatalf("gap count = %d, want one", gaps)
	}
	h.Close()
	count := 0
	for u := range fast {
		count++
		if u.Seq != uint64(count) {
			t.Fatalf("healthy watcher seq = %d at %d", u.Seq, count)
		}
	}
	if count != 10000 {
		t.Fatalf("healthy watcher received %d updates", count)
	}
}
func TestSnapshotsAndUpdatesOwnTheirBytes(t *testing.T) {
	h := New(Snapshot{SessionID: "s"}, nil)
	h.AddAsk(Ask{ToolCallID: "call", RawInput: json.RawMessage(`{"x":1}`), Options: []Option{{OptionID: "allow_once"}}})
	s, ch, stop := h.Watch(1)
	defer stop()
	s.PendingAsks[0].RawInput[0] = '!'
	s.PendingAsks[0].Options[0].OptionID = "mutated"
	s.Tools[0].RawInput[0] = '!'
	s.History[0].Update[0] = '!'
	actual := h.Snapshot()
	if !json.Valid(actual.PendingAsks[0].RawInput) || !json.Valid(actual.Tools[0].RawInput) || !json.Valid(actual.History[0].Update) || actual.PendingAsks[0].Options[0].OptionID != "allow_once" {
		t.Fatal("snapshot aliases hub storage")
	}
	h.Emit(Update{Kind: "user_message_chunk", Update: json.RawMessage(`{"content":{"type":"text","text":"hi"}}`)})
	u := <-ch
	u.Update[0] = '!'
	actual = h.Snapshot()
	if !json.Valid(actual.History[len(actual.History)-1].Update) {
		t.Fatal("subscriber update aliases history")
	}
}

func TestReplacementHistoryChangesEpochWithoutBreakingLiveSequence(t *testing.T) {
	h := New(Snapshot{SessionID: "s"}, nil)
	h.Emit(Update{Kind: "user_message_chunk", Update: json.RawMessage(`{"content":{"type":"text","text":"old"}}`)})
	before, ch, stop := h.Watch(2)
	defer stop()
	h.ReplaceHistory(func() []Update {
		return []Update{{Kind: "user_message_chunk", Update: json.RawMessage(`{"content":{"type":"text","text":"summary"}}`)}}
	})
	after := h.Snapshot()
	if after.HistoryEpoch != before.HistoryEpoch+1 || after.HistoryLen != 1 || after.Seq != before.Seq {
		t.Fatalf("replacement snapshot=%+v", after)
	}
	if _, err := h.History(before.HistoryEpoch, 0, 1); err != ErrStaleEpoch {
		t.Fatalf("stale history=%v", err)
	}
	h.Emit(Update{Kind: "user_message_chunk", Update: json.RawMessage(`{"content":{"type":"text","text":"new"}}`)})
	if u := <-ch; u.Seq != before.Seq+1 {
		t.Fatalf("live sequence=%d after %d", u.Seq, before.Seq)
	}
}

func TestReplayCapturesStreamingAndReservesLiveSlack(t *testing.T) {
	h := New(Snapshot{SessionID: "s"}, nil)
	defer h.Close()
	h.Emit(Update{Kind: "user_message_chunk", Update: json.RawMessage(`{"messageId":"user","content":{"text":"question"}}`)})
	h.Emit(Update{Kind: "agent_thought_chunk", Update: json.RawMessage(`{"messageId":"answer","content":{"text":"thinking"}}`)})
	h.Emit(Update{Kind: "agent_message_chunk", Update: json.RawMessage(`{"messageId":"answer","content":{"text":"prefix"}}`)})
	snapshot, live, stop := h.WatchReplay(1)
	defer stop()
	if snapshot.HistoryLen != 1 || len(snapshot.History) != 3 {
		t.Fatalf("replay=%+v", snapshot)
	}
	if snapshot.Streaming == nil || snapshot.Streaming.Text != "prefix" || snapshot.Streaming.Thought != "thinking" {
		t.Fatalf("streaming=%+v", snapshot.Streaming)
	}
	for i, update := range snapshot.History {
		if update.Seq != uint64(i+1) {
			t.Fatalf("replay sequence=%d at index %d", update.Seq, i)
		}
	}
	// One live frame per replayed frame, plus the requested live slack.
	for range len(snapshot.History) + 1 {
		h.Emit(Update{Kind: "agent_message_chunk", Update: json.RawMessage(`{"messageId":"answer","content":{"text":"next"}}`)})
	}
	for i := range len(snapshot.History) + 1 {
		update := <-live
		if update.Kind == "tempest/gap" || update.Seq != snapshot.Seq+uint64(i)+1 {
			t.Fatalf("live update=%+v at index %d", update, i)
		}
	}
	snapshot.History[0].Update[0] = '!'
	if !json.Valid(h.Snapshot().History[0].Update) {
		t.Fatal("replay aliases hub history")
	}
}

func TestSnapshotBudgetUsesWireCasing(t *testing.T) {
	snap := Snapshot{SessionID: "s", PermissionMode: "plan", Budget: agent.Budget{MaxTurns: 3, MaxCostUSD: 0.5, MaxWallTime: 90 * time.Second}}
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		SessionID string          `json:"sessionId"`
		Budget    json.RawMessage `json:"budget"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.SessionID != "s" || string(wire.Budget) != `{"maxTurns":3,"maxCostUsd":0.5,"maxWallSeconds":90}` {
		t.Fatalf("snapshot = %s", raw)
	}
	var decoded Snapshot
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SessionID != "s" || decoded.PermissionMode != "plan" || decoded.Budget != snap.Budget {
		t.Fatalf("round trip = %+v", decoded)
	}
}

func TestSubscriberQueueBoundsBytesAndReclaimsConsumedBytes(t *testing.T) {
	h := New(Snapshot{SessionID: "s"}, nil)
	defer h.Close()
	_, slow, stop := h.Watch(100)
	defer stop()
	_, fast, stopFast := h.Watch(100)
	defer stopFast()
	raw, _ := json.Marshal(map[string]string{"text": strings.Repeat("x", maxSubscriberBytes/3)})
	for range 4 {
		h.Emit(Update{Kind: "notice", Update: raw})
		if u := <-fast; u.Kind == "tempest/gap" {
			t.Fatal("consumed bytes were not reclaimed")
		}
	}
	if u := <-slow; u.Kind != "tempest/gap" {
		t.Fatalf("byte limit did not evict queued data: %s", u.Kind)
	}
	if _, open := <-slow; open {
		t.Fatal("byte overflow did not terminate watcher")
	}
}

func TestSnapshotHistoryTailBoundsBytesWithoutLosingPagedHistory(t *testing.T) {
	h := New(Snapshot{SessionID: "s"}, nil)
	defer h.Close()
	raw, _ := json.Marshal(map[string]string{"text": strings.Repeat("x", 12000)})
	for range 120 {
		h.Emit(Update{Kind: "user_message_chunk", Update: raw})
	}
	snapshot := h.Snapshot()
	encoded, err := json.Marshal(snapshot)
	if err != nil || len(encoded) > maxSnapshotHistoryBytes+4096 || len(snapshot.History) == 0 || snapshot.HistoryLen != 120 {
		t.Fatalf("snapshot tail: bytes=%d updates=%d total=%d error=%v", len(encoded), len(snapshot.History), snapshot.HistoryLen, err)
	}
	history, err := h.History(snapshot.HistoryEpoch, 0, snapshot.HistoryLen)
	if err != nil || len(history) != 120 {
		t.Fatalf("byte bound discarded paged history: %d %v", len(history), err)
	}
	if snapshot.History[len(snapshot.History)-1].Seq != history[len(history)-1].Seq {
		t.Fatal("snapshot omitted newest update")
	}
}
