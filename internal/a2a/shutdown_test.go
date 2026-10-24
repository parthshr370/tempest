package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	model "github.com/a2aproject/a2a-go/v2/a2a"
	"go.harness.dev/harness/internal/provider/faux"
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

func TestCancelStaleWorkingTaskDoesNotCancelAnotherSubmission(t *testing.T) {
	provider := faux.New(faux.Options{})
	provider.SetResponses(
		faux.Respond(faux.ToolCall("hold_current", json.RawMessage(`{}`), "current-call")),
		faux.Respond(faux.Text("current answer")),
	)
	started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	session, err := tempest.Open(t.Context(), tempest.Options{
		SessionDir: t.TempDir(), Cwd: t.TempDir(),
		New: &tempest.NewSession{PermissionMode: "bypass", Tools: []tempest.Tool{{
			Tool: tempest.ToolDefinition{Name: "hold_current", Parameters: json.RawMessage(`{"type":"object"}`)},
			Execute: func(ctx context.Context, _ string, _ json.RawMessage, _ tempest.ToolUpdateCallback) (tempest.ToolResult, error) {
				close(started)
				select {
				case <-ctx.Done():
					close(cancelled)
					return tempest.ToolResult{}, ctx.Err()
				case <-release:
					return tempest.ToolResult{}, nil
				}
			},
		}}},
		Stack: &tempest.StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(context.Background())
	defer close(release)
	if _, err := session.Submit(t.Context(), tempest.Input{RequestID: "current", Prompt: tempest.Prompt{Text: "current"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("current run did not start")
	}
	now := time.Now()
	stale := &task{requestID: "old", session: session, value: model.Task{ID: "old-task", ContextID: session.ID(), Status: model.TaskStatus{State: model.TaskStateWorking, Timestamp: &now}}, done: make(chan struct{})}
	server := &Server{tasks: map[model.TaskID]*task{"old-task": stale}, now: time.Now, retention: time.Minute}
	request := httptest.NewRequest("POST", "/a2a", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"CancelTask","params":{"id":"old-task"}}`))
	requestCtx, cancelRequest := context.WithTimeout(t.Context(), time.Second)
	defer cancelRequest()
	request = request.WithContext(requestCtx)
	request.Header.Set("A2A-Version", "1.0")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if !strings.Contains(response.Body.String(), `"code":-32002`) {
		t.Fatalf("stale cancellation accepted: %s", response.Body.String())
	}
	select {
	case <-cancelled:
		t.Fatal("stale task canceled the current submission")
	default:
	}
}
