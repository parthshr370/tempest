package a2a

import (
	"encoding/json"
	"testing"
	"time"

	model "github.com/a2aproject/a2a-go/v2/a2a"
)

func TestListCursorBindsPreciseTimestampAndStatusPresence(t *testing.T) {
	timestamp := time.Date(2026, 10, 22, 10, 0, 0, 123456789, time.UTC)
	server := &Server{tasks: make(map[model.TaskID]*task), now: func() time.Time { return timestamp }, retention: completedRetention}
	for index, id := range []string{"a", "z"} {
		projected := projection(id, model.TaskStateCompleted, timestamp)
		projected.serial = uint64(index + 1)
		server.tasks[projected.value.ID] = projected
		server.serial = projected.serial
	}
	raw, err := json.Marshal(map[string]string{"statusTimestampAfter": timestamp.Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatal(err)
	}
	equal, wireErr := server.list(raw)
	if wireErr != nil || equal.TotalSize != 2 {
		t.Fatal("timestamp filter must include equality", equal, wireErr)
	}
	first, wireErr := server.list(json.RawMessage(`{"pageSize":1,"statusTimestampAfter":"2026-10-22T10:00:00.123456780Z"}`))
	if wireErr != nil || len(first.Tasks) != 1 || first.Tasks[0].ID != "z" || first.NextPageToken == "" {
		t.Fatal(first, wireErr)
	}
	changedFilter, _ := json.Marshal(map[string]any{"pageSize": 1, "statusTimestampAfter": "2026-10-22T10:00:00.123456781Z", "pageToken": first.NextPageToken})
	if _, wireErr = server.list(changedFilter); wireErr == nil || wireErr.Code != -32602 {
		t.Fatal("cursor accepted different nanosecond filter", wireErr)
	}
	unspecified, wireErr := server.list(json.RawMessage(`{"status":"TASK_STATE_UNSPECIFIED"}`))
	if wireErr != nil || unspecified.TotalSize != 0 {
		t.Fatal("explicit unspecified state became no filter", unspecified, wireErr)
	}
}
