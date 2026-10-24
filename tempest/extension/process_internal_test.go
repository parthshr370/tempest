package extension

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"go.harness.dev/harness/internal/rpc"
	"go.harness.dev/harness/tempest/durable"
)

func TestCallbackBurstWaitsForWorkersWithoutRetiring(t *testing.T) {
	const workerLimit = 32
	const callbackCount = workerLimit + 8
	release := make(chan struct{})
	entered := make(chan struct{}, callbackCount)
	grant := NewGrant(context.Background(), GrantOptions{Identity: InvocationIdentity{SessionID: "session"}, Namespaces: []string{"sample"}, Read: func(context.Context, durable.Query) (durable.Page, error) {
		entered <- struct{}{}
		<-release
		return durable.Page{}, nil
	}})
	defer grant.Revoke()
	peer := &process{done: make(chan struct{}), workers: make(chan struct{}, workerLimit), capabilities: map[string]bool{"read": true}, grants: map[string]processGrant{grant.Identity().Token: {view: grant}}}
	raw, err := json.Marshal(struct {
		Token string        `json:"token"`
		Query durable.Query `json:"query"`
	}{Token: grant.Identity().Token, Query: durable.Query{Namespace: "sample"}})
	if err != nil {
		t.Fatal(err)
	}
	var callbacks sync.WaitGroup
	errors := make(chan *rpc.Error, callbackCount)
	for range callbackCount {
		admitted, admissionErr := peer.admit(context.Background(), methodPrefix+"context/read", raw)
		if admissionErr != nil {
			close(release)
			t.Fatal("burst rejected", admissionErr)
		}
		deferred, ok := admitted.(rpc.Deferred)
		if !ok {
			close(release)
			t.Fatal("callback was not deferred")
		}
		callbacks.Add(1)
		go func() { defer callbacks.Done(); _, callbackErr := deferred(); errors <- callbackErr }()
	}
	for range workerLimit {
		<-entered
	}
	close(release)
	callbacks.Wait()
	for range callbackCount {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	if peer.closed.Load() {
		t.Fatal("callback burst retired peer")
	}
}
