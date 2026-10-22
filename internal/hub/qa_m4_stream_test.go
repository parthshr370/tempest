package hub

import (
	"encoding/json"
	"testing"
)

func TestQAM4SerializedBackgroundUpdatesKeepTheirKind(t *testing.T) {
	for _, kind := range []string{"tempest/queue", "tempest/job", "tempest/job_output", "tempest/compaction"} {
		t.Run(kind, func(t *testing.T) {
			encoded, err := json.Marshal(Update{Seq: 1, Kind: kind, Update: json.RawMessage(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			var envelope struct {
				Kind string `json:"kind"`
			}
			if err := json.Unmarshal(encoded, &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Kind != kind {
				t.Fatalf("stream-json loses the event discriminator: got %s, want kind %q", encoded, kind)
			}
		})
	}
}
