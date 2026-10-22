package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	model "github.com/a2aproject/a2a-go/v2/a2a"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/rpc"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/tempest"
)

func TestACPQueueReceiptsBoundsAndOrderedCancelAdmission(t *testing.T) {
	provider := faux.New(faux.Options{})
	host := newServer(t, provider, nil, permission.ModeDefault, nil)
	wire := connect(t, host)
	id := newSession(t, wire, t.TempDir())
	var first tempest.QueuedItem
	for index := range 32 {
		var reply struct {
			Item tempest.QueuedItem `json:"item"`
		}
		call(t, wire, "tempest/steer", struct {
			SessionID string `json:"sessionId"`
			Text      string `json:"text"`
			Key       string `json:"key"`
		}{id, fmt.Sprintf("queued %d", index), fmt.Sprintf("key%d", index)}, &reply)
		if index == 0 {
			first = reply.Item
		}
	}
	err := wire.conn.Call(t.Context(), "tempest/follow_up", struct {
		SessionID string `json:"sessionId"`
		Text      string `json:"text"`
	}{id, "overflow"}, nil)
	var wireError *rpc.Error
	if !errors.As(err, &wireError) || wireError.Code != -32014 {
		t.Fatalf("queue capacity error=%v", err)
	}
	call(t, wire, "session/cancel", struct {
		SessionID string `json:"sessionId"`
	}{id}, nil)
	if len(host.sessions[id].session.Snapshot().Queues) != 32 {
		t.Fatal("cancel discarded accepted input")
	}
	// Use a single raw write so deferred mutation would race ordered admission.
	raw := rawConnect(t, host)
	raw.write(fmt.Sprintf(`{"jsonrpc":"2.0","id":10,"method":"tempest/cancel_queued","params":{"sessionId":%q,"queueId":%q}}`, id, first.ID), fmt.Sprintf(`{"jsonrpc":"2.0","id":11,"method":"tempest/steer","params":{"sessionId":%q,"text":"replacement"}}`, id))
	raw.result(10)
	raw.result(11)
	snapshot := watchSession(t, wire, id, false)
	if len(snapshot.Queues) != 32 {
		t.Fatalf("ordered cancel/admit pending=%d", len(snapshot.Queues))
	}
	var page tempest.JobPage
	call(t, wire, "tempest/jobs", struct {
		SessionID string `json:"sessionId"`
	}{id}, &page)
	if len(page.Jobs) != 0 {
		t.Fatal("unexpected jobs")
	}
}
func TestA2AGetTaskReconstructsDurableExactRunAfterRestart(t *testing.T) {
	provider := faux.New(faux.Options{})
	provider.SetResponses(faux.Respond(faux.Text("earlier answer")), faux.Respond(faux.Text("later answer")))
	options := Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), A2ABaseURL: "http://127.0.0.1:9876", Token: "secret", DefaultNew: tempest.NewSession{PermissionMode: permission.ModeBypass}, Stack: &tempest.StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}}
	host, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	earlier := admitted(t, host, sendParams("first", "", false))
	later := admitted(t, host, sendParams("second", earlier.ContextID, false))
	if earlier.ID == later.ID {
		t.Fatal("runs reused task id")
	}
	if err = host.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	store, err := journal.Create(t.Context(), options.SessionDir, options.Cwd, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	interruptedID := store.NewRunID()
	if err = store.Append(t.Context(), journal.RunStartedRecord{Meta: journal.Meta{RunID: interruptedID}, Model: provider.Model().ID, LogicalRunID: interruptedID}); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := restarted.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	restored := getTask(t, restarted, earlier.ID)
	if restored.Status.State != model.TaskStateCompleted || restored.ContextID != earlier.ContextID || len(restored.Artifacts) != 1 || restored.Artifacts[0].Parts[0].Text() != "earlier answer" {
		t.Fatalf("restored exact run=%+v", restored)
	}
	interrupted := getTask(t, restarted, model.TaskID(interruptedID))
	encoded, err := json.Marshal(interrupted)
	if err != nil {
		t.Fatal(err)
	}
	if interrupted.Status.State != model.TaskStateFailed || !strings.Contains(string(encoded), `"interrupted":true`) || !strings.Contains(string(encoded), `"recoverable":true`) {
		t.Fatalf("interrupted projection=%s", encoded)
	}
	var withoutHistory model.Task
	a2aOK(t, restarted, "GetTask", struct {
		ID            model.TaskID `json:"id"`
		HistoryLength int          `json:"historyLength"`
	}{earlier.ID, 0}, &withoutHistory)
	if len(withoutHistory.History) != 0 {
		t.Fatal("historyLength0 ignored")
	}
	recorder := httptest.NewRecorder()
	restarted.Handler().ServeHTTP(recorder, a2aRequest("CancelTask", struct {
		ID model.TaskID `json:"id"`
	}{earlier.ID}))
	if !strings.Contains(recorder.Body.String(), "TaskNotFoundError") {
		t.Fatal("historical cancellation became a live handle", recorder.Body.String())
	}
	if len(restarted.sessions) != 0 {
		t.Fatal("durable GetTask opened writer sessions")
	}
}

func TestLookupRunHonorsWorkspaceAndLocalLiveOwner(t *testing.T) {
	for _, sameWorkspace := range []bool{true, false} {
		t.Run(fmt.Sprint(sameWorkspace), func(t *testing.T) {
			provider := faux.New(faux.Options{})
			provider.SetResponses(faux.Respond(faux.ToolCall("work", json.RawMessage(`{}`), "held")))
			entered := make(chan struct{}, 1)
			workspace := t.TempDir()
			host, err := New(Options{SessionDir: t.TempDir(), Cwd: workspace, DefaultNew: tempest.NewSession{PermissionMode: permission.ModeBypass}, Stack: &tempest.StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple, ConfiguredTools: []tempest.Tool{slowTool(time.Minute, entered)}}})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := host.Close(context.Background()); err != nil {
					t.Error(err)
				}
			}()
			cwd := workspace
			if !sameWorkspace {
				cwd = t.TempDir()
			}
			owner, err := host.New(t.Context(), cwd, &tempest.NewSession{PermissionMode: permission.ModeBypass})
			if err != nil {
				t.Fatal(err)
			}
			run, err := owner.Begin(t.Context(), tempest.Prompt{Text: "hold"})
			if err != nil {
				t.Fatal(err)
			}
			id, err := run.Started(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("tool didn't start")
			}
			detail, err := host.LookupRun(t.Context(), id)
			if sameWorkspace {
				if err != nil || detail.Finished || detail.Interrupted || detail.RunID != id {
					t.Fatalf("live lookup=%+v %v", detail, err)
				}
			} else if !errors.Is(err, journal.ErrRunNotFound) {
				t.Fatalf("foreign workspace lookup=%+v %v", detail, err)
			}
			run.Cancel()
			if _, err = run.Wait(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
