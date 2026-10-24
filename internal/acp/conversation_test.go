package acp

import (
	"context"
	"encoding/json"
	"testing"

	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/tempest"
	"go.harness.dev/harness/tempest/durable"
)

type conversationBackend struct {
	Backend
	host *tempest.Host
}

func (backend conversationBackend) Acquire(id string) (*tempest.Session, func(), error) {
	owner, err := backend.host.Open(context.Background(), id)
	return owner, func() {}, err
}
func (backend conversationBackend) Fork(ctx context.Context, parent, entry string, options tempest.ForkOptions) (*tempest.Session, error) {
	return backend.host.Fork(ctx, parent, entry, options)
}

func TestACPConversationControlsShareSDKState(t *testing.T) {
	provider := faux.New(faux.Options{})
	provider.SetResponses(faux.Respond(faux.Text("archived answer")), faux.Respond(faux.Text("manual summary")))
	host, err := tempest.OpenHost(t.Context(), tempest.HostOptions{SessionDir: t.TempDir(), Stack: &tempest.StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close(context.Background())
	owner, err := host.Create(t.Context(), tempest.AgentOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Prompt(t.Context(), tempest.Prompt{Text: "archived needle"}); err != nil {
		t.Fatal(err)
	}
	entries, err := owner.Read(t.Context(), durable.Query{Kind: durable.EntryKind, Namespace: "_tempest/journal", Reverse: true, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	entryID := ""
	for _, entry := range entries.Records {
		var value struct{ Type, ID string }
		if err := json.Unmarshal(entry.Value, &value); err != nil {
			t.Fatal(err)
		}
		if value.Type == "message" {
			entryID = value.ID
			break
		}
	}
	client := NewClient(nil, conversationBackend{host: host}, nil)
	client.initialized = true
	arguments, err := json.Marshal(struct {
		SessionID string `json:"sessionId"`
		EntryID   string `json:"entryId"`
	}{owner.ID(), entryID})
	if err != nil {
		t.Fatal(err)
	}
	result, wireErr := client.Handle(t.Context(), "_tempest/durable/fork", arguments)
	if wireErr != nil {
		t.Fatal(wireErr)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var fork struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(raw, &fork); err != nil || fork.SessionID == "" || fork.SessionID == owner.ID() {
		t.Fatal(string(raw), err)
	}
	arguments, err = json.Marshal(struct {
		SessionID string `json:"sessionId"`
		Note      string `json:"note"`
		RequestID string `json:"requestId"`
	}{owner.ID(), "ACP reset", "acp-reset"})
	if err != nil {
		t.Fatal(err)
	}
	if _, wireErr := client.Handle(t.Context(), "_tempest/durable/reset", arguments); wireErr != nil {
		t.Fatal(wireErr)
	}
	receipt, err := owner.Submission(t.Context(), "acp-reset")
	if err != nil || receipt.State != "completed" {
		t.Fatal(receipt, err)
	}
	arguments, err = json.Marshal(struct {
		SessionID string                 `json:"sessionId"`
		History   tempest.HistoryOptions `json:"history"`
	}{owner.ID(), tempest.HistoryOptions{Query: "archived needle", Limit: 100}})
	if err != nil {
		t.Fatal(err)
	}
	result, wireErr = client.Handle(t.Context(), "_tempest/durable/search_history", arguments)
	if wireErr != nil {
		t.Fatal(wireErr)
	}
	raw, err = json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var page tempest.HistoryPage
	if err := json.Unmarshal(raw, &page); err != nil || len(page.Matches) != 1 {
		t.Fatal(string(raw), err)
	}
	arguments, err = json.Marshal(struct {
		SessionID string                 `json:"sessionId"`
		Options   tempest.CompactOptions `json:"compactOptions"`
	}{fork.SessionID, tempest.CompactOptions{Wait: true}})
	if err != nil {
		t.Fatal(err)
	}
	result, wireErr = client.Handle(t.Context(), "_tempest/durable/compact", arguments)
	if wireErr != nil {
		t.Fatal(wireErr)
	}
	raw, err = json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var task tempest.Task
	if err := json.Unmarshal(raw, &task); err != nil || task.Status != "completed" {
		t.Fatal(string(raw), err)
	}
}
