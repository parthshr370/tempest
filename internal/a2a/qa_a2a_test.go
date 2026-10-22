package a2a

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	model "github.com/a2aproject/a2a-go/v2/a2a"
)

func TestRetentionAndCapacityUseDocumentedBoundaries(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	server := &Server{tasks: make(map[model.TaskID]*task), now: func() time.Time { return now }, capacity: maxRetainedTasks, retention: completedRetention}
	for i := range maxRetainedTasks {
		id := model.TaskID(time.Unix(int64(i), 0).Format(time.RFC3339))
		server.tasks[id] = projection(string(id), model.TaskStateWorking, now.Add(-48*time.Hour))
	}
	request, prompt, wireErr := decodeSend(json.RawMessage(`{"message":{"messageId":"m","role":"ROLE_USER","parts":[{"text":"go"}]}}`))
	if wireErr != nil {
		t.Fatal(wireErr)
	}
	if _, wireErr := server.send(t.Context(), request, prompt); wireErr == nil || wireErr.Code != -32004 || wireErr.Data[0].Metadata["reason"] != "capacity" {
		t.Fatalf("10,000 active tasks must reject admission: %+v", wireErr)
	}
	if len(server.tasks) != maxRetainedTasks {
		t.Fatal("capacity enforcement evicted active tasks")
	}
	for id := range server.tasks {
		delete(server.tasks, id)
	}
	server.tasks["boundary"] = projection("boundary", model.TaskStateCompleted, now.Add(-24*time.Hour))
	server.tasks["inside"] = projection("inside", model.TaskStateCompleted, now.Add(-24*time.Hour+time.Nanosecond))
	server.tasks["active"] = projection("active", model.TaskStateWorking, now.Add(-48*time.Hour))
	server.expireLocked()
	if server.tasks["boundary"] != nil || server.tasks["inside"] == nil || server.tasks["active"] == nil {
		t.Fatal("24-hour retention boundary or active-task preservation violated")
	}
}

type cancelAfterSnapshot struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (writer cancelAfterSnapshot) Flush() {
	writer.ResponseRecorder.Flush()
	writer.cancel()
}

func TestA2ACanceledStreamsDetachSubscribers(t *testing.T) {
	projected := projection("run", model.TaskStateWorking, time.Now().UTC())
	server := &Server{}
	for range 100 {
		ctx, cancel := context.WithCancel(t.Context())
		request := httptest.NewRequest("POST", "/a2a", nil).WithContext(ctx)
		writer := cancelAfterSnapshot{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}
		server.stream(writer, request, json.RawMessage(`"subscriber"`), projected, nil, false)
		cancel()
		if len(projected.subscribers) != 0 {
			t.Fatal("disconnected stream retained its task subscriber")
		}
		if projected.value.Status.State != model.TaskStateWorking {
			t.Fatal("disconnected stream changed the active task state")
		}
	}
}
