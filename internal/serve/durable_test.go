package serve

import (
	"context"
	"slices"
	"sync"
	"testing"

	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/tempest"
	"go.harness.dev/harness/tempest/durable"
)

func TestDurableACPRequestIdentityAndCompletion(t *testing.T) {
	provider := faux.New(faux.Options{})
	provider.SetResponses(faux.Respond(faux.Text("original answer")))
	host := newServer(t, provider, nil, permission.ModeDefault, nil)
	wire := connect(t, host)
	id := newSession(t, wire, t.TempDir())
	params := map[string]any{"sessionId": id, "requestId": "stable", "prompt": []map[string]string{{"type": "text", "text": "hello"}}, "placement": "input"}
	var wg sync.WaitGroup
	receipts := make([]tempest.Submission, 8)
	for i := range receipts {
		wg.Go(func() {
			if err := wire.conn.Call(t.Context(), "_tempest/durable/submit", params, &receipts[i]); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	for _, receipt := range receipts {
		if receipt.ID == "" || receipt.ID != receipts[0].ID {
			t.Fatalf("duplicate identity: %+v", receipts)
		}
	}
	session, release, err := host.Acquire(id)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	settled, err := session.WaitSubmission(t.Context(), "stable")
	if err != nil || settled.Answer != "original answer" {
		t.Fatalf("terminal=%+v err=%v", settled, err)
	}
	var completion struct {
		StopReason string `json:"stopReason"`
	}
	call(t, wire, "session/prompt", map[string]any{"sessionId": id, "prompt": params["prompt"], "_meta": map[string]any{"tempest": map[string]any{"requestId": "stable"}}}, &completion)
	if completion.StopReason != "end_turn" {
		t.Fatalf("completion=%+v", completion)
	}
	params["prompt"] = []map[string]string{{"type": "text", "text": "different"}}
	if err := wire.conn.Call(t.Context(), "_tempest/durable/submit", params, nil); err == nil {
		t.Fatal("changed payload accepted")
	}
}

func TestDurableACPWithdrawAndDocumentRead(t *testing.T) {
	host := newServer(t, faux.New(faux.Options{}), nil, permission.ModeDefault, nil)
	wire := connect(t, host)
	id := newSession(t, wire, t.TempDir())
	params := map[string]any{"sessionId": id, "requestId": "pending", "prompt": []map[string]string{{"type": "text", "text": "later"}}, "placement": "follow_up"}
	var first, duplicate tempest.Submission
	call(t, wire, "_tempest/durable/submit", params, &first)
	call(t, wire, "_tempest/durable/withdraw", map[string]any{"sessionId": id, "requestId": "pending"}, nil)
	call(t, wire, "_tempest/durable/submit", params, &duplicate)
	if first.ID == "" || duplicate.ID != first.ID || duplicate.State != "withdrawn" {
		t.Fatalf("withdraw retry=%+v first=%+v", duplicate, first)
	}
	var page durable.Page
	call(t, wire, "_tempest/durable/read", map[string]any{"sessionId": id, "query": durable.Query{Limit: 1}}, &page)
	if len(page.Records) > 1 {
		t.Fatal("read ignored bound")
	}
	var capability struct {
		AgentCapabilities struct {
			Meta struct {
				Tempest struct {
					Durable struct {
						Version       int      `json:"version"`
						Tasks         bool     `json:"tasks"`
						Methods       []string `json:"methods"`
						Notifications []string `json:"notifications"`
					} `json:"durable"`
				} `json:"tempest"`
			} `json:"_meta"`
		} `json:"agentCapabilities"`
	}
	call(t, wire, "initialize", map[string]any{"protocolVersion": 1}, &capability)
	durable := capability.AgentCapabilities.Meta.Tempest.Durable
	if durable.Version != 1 || !durable.Tasks || !slices.Contains(durable.Notifications, "_tempest/durable/update") {
		t.Fatalf("durable capabilities=%+v", durable)
	}
	for _, method := range []string{
		"submit", "get_submission", "withdraw", "commit", "read",
		"create_task", "get_task", "tasks", "cancel_task", "retry_task",
		"cancel_all", "resume_pending", "next_wake",
	} {
		if !slices.Contains(durable.Methods, "_tempest/durable/"+method) {
			t.Errorf("durable capabilities missing method %q: %+v", method, durable)
		}
	}
}

func TestDurableA2AMessageIdentity(t *testing.T) {
	provider := faux.New(faux.Options{})
	provider.SetResponses(faux.Respond(faux.Text("original answer")), faux.Respond(faux.Text("must not run")))
	host := interopServer(t, provider, nil, permission.ModeBypass)
	first := admitted(t, host, sendParams("hello", "", false))
	params := sendParams("hello", first.ContextID, false)
	retry := admitted(t, host, params)
	if retry.ID != first.ID || len(retry.Artifacts) != 1 || retry.Artifacts[0].Parts[0].Text() != "original answer" {
		t.Fatalf("retry=%+v first=%+v", retry, first)
	}
	params["message"].(map[string]any)["parts"] = []any{map[string]any{"text": "changed"}}
	if reply := a2aCall(t, host, "SendMessage", params); reply.Error == nil {
		t.Fatal("changed message accepted under original identity")
	}
}

func TestDurableACPReceiptSurvivesServerRestart(t *testing.T) {
	provider := faux.New(faux.Options{})
	provider.SetResponses(faux.Respond(faux.Text("original answer")), faux.Respond(faux.Text("must not run")))
	opts := Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), DefaultNew: tempest.NewSession{PermissionMode: permission.ModeBypass}, Stack: &tempest.StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}}
	host, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close(context.Background())
	wire := connect(t, host)
	id := newSession(t, wire, opts.Cwd)
	params := map[string]any{"sessionId": id, "requestId": "restart", "prompt": []map[string]string{{"type": "text", "text": "hello"}}}
	var first tempest.Submission
	call(t, wire, "_tempest/durable/submit", params, &first)
	session, release, err := host.Acquire(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.WaitSubmission(t.Context(), first.RequestID); err != nil {
		t.Fatal(err)
	}
	release()
	if err := host.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close(context.Background())
	client := connect(t, restarted)
	call(t, client, "session/resume", map[string]any{"sessionId": id, "cwd": opts.Cwd}, nil)
	var duplicate tempest.Submission
	call(t, client, "_tempest/durable/submit", params, &duplicate)
	if duplicate.ID != first.ID || duplicate.RunID != first.RunID || duplicate.Answer != "original answer" || duplicate.State != "completed" {
		t.Fatalf("original=%+v restart=%+v", first, duplicate)
	}
}
