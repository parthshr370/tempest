package taskstate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/extension"
)

type boundaryStore struct {
	Store
	prefix string
	after  bool
	fired  bool
}

func (store *boundaryStore) Commit(ctx context.Context, batch durable.Commit) (durable.Receipt, error) {
	if !store.fired && strings.HasPrefix(batch.ID, store.prefix) {
		store.fired = true
		if store.after {
			if _, err := store.Store.Commit(ctx, batch); err != nil {
				return durable.Receipt{}, err
			}
		}
		return durable.Receipt{}, errors.New("simulated process boundary")
	}
	return store.Store.Commit(ctx, batch)
}

func TestCheckoutEffectAndDecisionCrashBoundaries(t *testing.T) {
	for _, boundary := range []string{"before_effect", "after_effect", "before_decision", "after_decision"} {
		t.Run(boundary, func(t *testing.T) {
			var mu sync.Mutex
			ledger := map[string]string{}
			requests := 0
			service := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Error(err)
					response.WriteHeader(500)
					return
				}
				key := request.Header.Get("Idempotency-Key")
				mu.Lock()
				defer mu.Unlock()
				requests++
				if prior, exists := ledger[key]; exists && prior != string(body) {
					t.Error("effect key changed input")
					response.WriteHeader(409)
					return
				}
				ledger[key] = string(body)
				response.WriteHeader(200)
			}))
			defer service.Close()
			var store Store = durable.NewMemory()
			if strings.Contains(boundary, "decision") {
				store = &boundaryStore{Store: store, prefix: "task:decision:", after: boundary == "after_decision"}
			}
			first := true
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			definition := taskDefinition(func(ctx context.Context, task extension.Task, _ extension.Invocation) (extension.TaskDecision, error) {
				if first && boundary == "before_effect" {
					first = false
					cancel()
					return extension.TaskDecision{}, ctx.Err()
				}
				request, err := http.NewRequestWithContext(ctx, http.MethodPost, service.URL, strings.NewReader(`{"amount":100}`))
				if err != nil {
					return extension.TaskDecision{}, err
				}
				request.Header.Set("Idempotency-Key", task.ID+":"+task.Phase+":charge")
				response, err := http.DefaultClient.Do(request)
				if err != nil {
					return extension.TaskDecision{}, err
				}
				if err = response.Body.Close(); err != nil {
					return extension.TaskDecision{}, err
				}
				if response.StatusCode != 200 {
					return extension.TaskDecision{}, errors.New("charge rejected")
				}
				if first && boundary == "after_effect" {
					first = false
					cancel()
					return extension.TaskDecision{}, ctx.Err()
				}
				first = false
				return extension.TaskDecision{Kind: "complete", Memos: []extension.TaskMemo{{Key: "charged", Value: json.RawMessage(`true`)}}, Submissions: []extension.TaskSubmission{{Key: "receipt", Text: "paid"}}}, nil
			})
			definitions := map[string]extension.TaskDefinition{"shop/checkout": definition}
			runtime := newTestRuntime(t, store, nil, definitions, nil)
			admit(t, runtime, "checkout", "shop/checkout")
			err := runtime.Resume(ctx)
			if boundary == "after_decision" && err != nil {
				t.Fatal(err)
			}
			if boundary != "after_decision" && err == nil {
				t.Fatal("missing injected boundary")
			}
			runtime = newTestRuntime(t, store, nil, definitions, nil)
			if err = runtime.Resume(t.Context()); err != nil {
				t.Fatal(err)
			}
			task, err := runtime.Get("checkout")
			if err != nil || task.Status != "completed" {
				t.Fatalf("task=%+v err=%v", task, err)
			}
			mu.Lock()
			effects := len(ledger)
			attempts := requests
			mu.Unlock()
			if effects != 1 {
				t.Fatalf("charges=%d requests=%d", effects, attempts)
			}
			if (boundary == "after_effect" || boundary == "before_decision") && attempts != 2 {
				t.Fatalf("expected at-least-once external requests, got %d", attempts)
			}
			deliveries, err := store.Read(t.Context(), durable.Query{SessionID: "session", Kind: durable.SubmissionKind, Limit: 10})
			if err != nil || len(deliveries.Records) != 1 {
				t.Fatalf("deliveries=%d err=%v", len(deliveries.Records), err)
			}
			memos, err := store.Read(t.Context(), durable.Query{SessionID: "session", Kind: durable.MemoKind, Limit: 10})
			if err != nil || len(memos.Records) != 1 {
				t.Fatalf("memos=%d err=%v", len(memos.Records), err)
			}
		})
	}
}

func TestAbortIntentCrashBoundariesAndCleanupFault(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_commit", true: "after_commit"}[after], func(t *testing.T) {
			deadline := time.Now().Add(time.Hour)
			cleanups := 0
			cleanupFails := true
			definition := taskDefinition(func(context.Context, extension.Task, extension.Invocation) (extension.TaskDecision, error) {
				return extension.TaskDecision{Kind: "sleep", WakeAt: &deadline}, nil
			})
			definition.Abort = func(context.Context, extension.Task, extension.Invocation) (extension.TaskDecision, error) {
				cleanups++
				if cleanupFails {
					return extension.TaskDecision{}, errors.New("refund unavailable")
				}
				return extension.TaskDecision{Kind: "complete"}, nil
			}
			store := &boundaryStore{Store: durable.NewMemory(), prefix: "task:abort:", after: after}
			definitions := map[string]extension.TaskDefinition{"shop/checkout": definition}
			runtime := newTestRuntime(t, store, nil, definitions, nil)
			admit(t, runtime, "checkout", "shop/checkout")
			if err := runtime.Resume(t.Context()); err != nil {
				t.Fatal(err)
			}
			err := runtime.Abort(t.Context(), "checkout", "operator")
			if after && err != nil {
				t.Fatal(err)
			}
			if !after && err == nil {
				t.Fatal("missing abort boundary")
			}
			runtime = newTestRuntime(t, store, nil, definitions, nil)
			if !after {
				if err = runtime.Abort(t.Context(), "checkout", "operator"); err != nil {
					t.Fatal(err)
				}
			}
			if err = runtime.Resume(t.Context()); err != nil {
				t.Fatal(err)
			}
			task, _ := runtime.Get("checkout")
			if task.Status != "cleanup_faulted" || cleanups != 1 {
				t.Fatalf("task=%+v cleanups=%d", task, cleanups)
			}
			cleanupFails = false
			if err = runtime.Retry(t.Context(), "checkout"); err != nil {
				t.Fatal(err)
			}
			if err = runtime.Resume(t.Context()); err != nil {
				t.Fatal(err)
			}
			task, _ = runtime.Get("checkout")
			if task.Status != "aborted" || cleanups != 2 {
				t.Fatalf("retry task=%+v cleanups=%d", task, cleanups)
			}
		})
	}
}
