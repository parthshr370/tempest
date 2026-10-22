package a2a

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	model "github.com/a2aproject/a2a-go/v2/a2a"
	"go.harness.dev/harness/tempest"
)

type terminalFlushWriter struct {
	*httptest.ResponseRecorder
	first, terminal, release chan struct{}
	flushed                  bool
}

func (w *terminalFlushWriter) FlushError() error {
	if !w.flushed {
		w.flushed = true
		close(w.first)
	} else {
		close(w.terminal)
		<-w.release
	}
	return nil
}

func TestCloseWaitsForTerminalStreamFlush(t *testing.T) {
	for _, expire := range []bool{false, true} {
		name := "drained"
		if expire {
			name = "deadline"
		}
		t.Run(name, func(t *testing.T) {
			s := &Server{}
			task := &task{
				value: model.Task{ID: "task", Status: model.TaskStatus{State: model.TaskStateWorking}},
				done:  make(chan struct{}), changed: make(chan struct{}), subscribers: make(map[*subscriber]struct{}),
			}
			writer := &terminalFlushWriter{ResponseRecorder: httptest.NewRecorder(), first: make(chan struct{}), terminal: make(chan struct{}), release: make(chan struct{})}
			streamDone := make(chan struct{})
			go func() {
				defer close(streamDone)
				s.stream(writer, httptest.NewRequest("POST", "/a2a", nil), []byte(`1`), task, nil, false)
			}()
			<-writer.first
			task.finish(tempest.RunResult{StopReason: "aborted"}, "")
			<-writer.terminal
			timeout := time.Second
			if expire {
				timeout = 25 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(t.Context(), timeout)
			defer cancel()
			if expire {
				if err := s.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("blocked writer ignored close context: %v", err)
				}
			} else {
				closed := make(chan struct{})
				var closeErr error
				go func() { closeErr = s.Close(ctx); close(closed) }()
				select {
				case <-closed:
					t.Errorf("Close returned before terminal flush: %v", closeErr)
				case <-time.After(20 * time.Millisecond):
				}
				close(writer.release)
				<-closed
				if closeErr != nil {
					t.Error(closeErr)
				}
			}
			if expire {
				close(writer.release)
			}
			<-streamDone
			if !strings.Contains(writer.Body.String(), "TASK_STATE_CANCELED") {
				t.Fatal("terminal status was not written")
			}
			rejected := httptest.NewRecorder()
			s.stream(rejected, httptest.NewRequest("POST", "/a2a", nil), []byte(`2`), task, nil, false)
			if !strings.Contains(rejected.Body.String(), "shutdown") {
				t.Fatal("Close still admitted a stream")
			}
		})
	}
}
