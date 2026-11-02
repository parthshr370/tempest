package tempest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/extension"
)

// TestHostExtensionsSelectPerSession checks that AgentOptions.Extensions
// gives one created session a bundle the host does not select for others,
// and that reopening each session after a host restart restores exactly its
// own journaled selection.
func TestHostExtensionsSelectPerSession(t *testing.T) {
	provider := faux.New(faux.Options{})
	sessionDir := t.TempDir()
	document := durable.DocumentDefinition{Namespace: "worker.ticket", Version: 1, Schema: json.RawMessage(`{"type":"object"}`), Initial: json.RawMessage(`{}`), History: durable.HistoryLatest, Fork: durable.ForkInitial, Reset: durable.ResetPreserve}
	registry := extension.NewRegistry()
	if err := registry.Install(t.Context(), extension.Definition{ID: "worker", Revision: "1", Documents: []durable.DocumentDefinition{document}}); err != nil {
		t.Fatal(err)
	}
	open := func() *Host {
		host, err := OpenHost(t.Context(), HostOptions{SessionDir: sessionDir, Registry: registry, Stack: &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}})
		if err != nil {
			t.Fatal(err)
		}
		return host
	}
	commit := func(session *Session, id string) error {
		page, err := session.Read(t.Context(), durable.Query{Kind: durable.DocumentKind, Namespace: document.Namespace, ID: "ticket", Limit: 1})
		if err != nil {
			return err
		}
		var revision uint64
		if len(page.Records) == 1 {
			revision = page.Records[0].Revision
		}
		_, err = session.Commit(t.Context(), durable.Commit{ID: id, Expected: []durable.RevisionCheck{{Kind: durable.DocumentKind, Namespace: document.Namespace, ID: "ticket", Revision: revision}}, Changes: []durable.Change{{Kind: durable.DocumentKind, Namespace: document.Namespace, ID: "ticket", Version: 1, Value: json.RawMessage(`{"ticket":"` + id + `"}`)}}})
		return err
	}
	host := open()
	plain, err := host.Create(t.Context(), AgentOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := host.Create(t.Context(), AgentOptions{Cwd: t.TempDir(), Extensions: []extension.Selection{{ID: "worker", Required: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := commit(plain, "plain-1"); err == nil || !strings.Contains(err.Error(), "ungranted") {
		t.Fatalf("plain session wrote the worker document: %v", err)
	}
	if err := commit(worker, "worker-1"); err != nil {
		t.Fatalf("worker session document: %v", err)
	}
	plainID, workerID := plain.ID(), worker.ID()
	if err := host.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	host = open()
	defer host.Close(context.Background())
	reopenedWorker, err := host.Open(t.Context(), workerID)
	if err != nil {
		t.Fatal(err)
	}
	if err := commit(reopenedWorker, "worker-2"); err != nil {
		t.Fatalf("reopened worker lost its selection: %v", err)
	}
	reopenedPlain, err := host.Open(t.Context(), plainID)
	if err != nil {
		t.Fatal(err)
	}
	if err := commit(reopenedPlain, "plain-2"); err == nil || !strings.Contains(err.Error(), "ungranted") {
		t.Fatalf("reopened plain session gained the worker document: %v", err)
	}

	bare, err := OpenHost(t.Context(), HostOptions{SessionDir: t.TempDir(), Stack: &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}})
	if err != nil {
		t.Fatal(err)
	}
	defer bare.Close(context.Background())
	if _, err := bare.Create(t.Context(), AgentOptions{Cwd: t.TempDir(), Extensions: []extension.Selection{{ID: "worker", Required: true}}}); err == nil {
		t.Fatal("a required bundle missing from the registry was dropped silently")
	}
}
