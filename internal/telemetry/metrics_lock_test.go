package telemetry

import (
	"testing"
	"time"
)

// A collection that is copying one series holds no lock that recording into
// any other series, or adding a series, needs.
func TestRecordingDoesNotWaitForCollection(t *testing.T) {
	set := &metricSet{start: time.Now()}
	reached := make(chan struct{})
	set.observed("first", "1", "Signals that collection started.", kindGauge, func() []observation {
		close(reached)
		return nil
	})
	set.toolCalls = set.counter("tempest.tool.calls", "{call}", "Tool calls.")
	set.duration = set.histogram("gen_ai.client.operation.duration", "s", "Durations.", durationBuckets)
	held, other := stringAttribute("gen_ai.tool.name", "held"), stringAttribute("gen_ai.tool.name", "other")
	set.increment(set.toolCalls, held)
	set.increment(set.toolCalls, other)
	copying := set.toolCalls.seriesFor([]attribute{held})
	copying.mu.Lock()
	collected := make(chan snapshot, 1)
	go func() { collected <- set.collect(time.Now()) }()
	<-reached
	// Collection now reaches the held series and waits there.
	time.Sleep(20 * time.Millisecond)
	recorded := make(chan struct{})
	go func() {
		set.increment(set.toolCalls, other)
		set.increment(set.toolCalls, stringAttribute("gen_ai.tool.name", "new"))
		set.observe(set.duration, 1.5)
		close(recorded)
	}()
	select {
	case <-recorded:
	case <-time.After(5 * time.Second):
		t.Fatal("recording waited for a collection in progress")
	}
	copying.mu.Unlock()
	if got := <-collected; len(got.instruments) != 3 {
		t.Fatalf("collected %d instruments", len(got.instruments))
	}
}
