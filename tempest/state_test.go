package tempest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/extension"
)

func TestLatestDocumentsRejectHistoricalReads(t *testing.T) {
	for _, backend := range []string{"jsonl", "memory"} {
		t.Run(backend, func(t *testing.T) {
			provider := faux.New(faux.Options{})
			registry := extension.NewRegistry()
			t.Cleanup(func() { _ = registry.Close(context.Background()) })
			var queries []durable.Query
			callbackCount := 0
			definition := durable.DocumentDefinition{Namespace: "latest", Version: 1, Schema: json.RawMessage(`{"type":"integer"}`), Initial: json.RawMessage(`0`), History: durable.HistoryLatest, Fork: durable.ForkCurrent, Reset: durable.ResetPreserve}
			if err := registry.Install(t.Context(), extension.Definition{ID: "latest", Revision: "v1", Documents: []durable.DocumentDefinition{definition}, Sections: []extension.Section{{Name: "check", Render: func(ctx context.Context, view extension.ReadView) (string, error) {
				callbackCount++
				for _, query := range queries {
					want := "rejects historical reads"
					if query.Namespace == "" {
						want = "read outside granted documents"
					}
					if _, err := view.Read(ctx, query); err == nil || !strings.Contains(err.Error(), want) {
						return "", fmt.Errorf("callback accepted historical query %+v: %v", query, err)
					}
				}
				page, err := view.Read(ctx, durable.Query{Kind: durable.DocumentKind, Namespace: "latest"})
				if err != nil || len(page.Records) != 2 {
					return "", fmt.Errorf("callback current read: %+v %v", page, err)
				}
				return "Current state only", nil
			}}}}); err != nil {
				t.Fatal(err)
			}
			options := Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &NewSession{}, Registry: registry, Extensions: []extension.Selection{{ID: "latest", Required: true}}, Stack: &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}}
			if backend == "memory" {
				options.Store = durable.NewMemory()
				t.Cleanup(func() { _ = options.Store.Close(context.Background()) })
			}
			owner, err := Open(t.Context(), options)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = owner.Close(context.Background()) })
			first, err := owner.Read(t.Context(), durable.Query{Kind: durable.DocumentKind, Namespace: "latest"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = owner.Commit(t.Context(), durable.Commit{ID: "second-document", Changes: []durable.Change{{Kind: durable.DocumentKind, Namespace: "latest", ID: "second", Version: 1, Value: json.RawMessage(`1`)}}}); err != nil {
				t.Fatal(err)
			}
			paged, err := owner.Read(t.Context(), durable.Query{Kind: durable.DocumentKind, Namespace: "latest", Limit: 1})
			if err != nil || paged.NextCursor == "" {
				t.Fatalf("current first page: %+v %v", paged, err)
			}
			if _, err = owner.Read(t.Context(), durable.Query{Kind: durable.DocumentKind, Namespace: "latest", Limit: 1, Cursor: paged.NextCursor}); err != nil {
				t.Fatalf("unchanged current cursor rejected: %v", err)
			}
			if _, err = owner.Commit(t.Context(), durable.Commit{ID: "update", Changes: []durable.Change{{Kind: durable.DocumentKind, Namespace: "latest", ID: "default", Version: 1, Value: json.RawMessage(`2`)}}}); err != nil {
				t.Fatal(err)
			}
			queries = []durable.Query{
				{Kind: durable.DocumentKind, Namespace: "latest", AsOf: first.Seq},
				{Namespace: "latest", AsOf: first.Seq},
				{AsOf: first.Seq},
				{Kind: durable.DocumentKind, AsOf: first.Seq},
				{Kind: durable.DocumentKind, Namespace: "latest", History: true},
				{History: true},
				{Kind: durable.DocumentKind, Namespace: "latest", Limit: 1, Cursor: paged.NextCursor},
			}
			for _, query := range queries {
				if _, err = owner.Read(t.Context(), query); err == nil || !strings.Contains(err.Error(), "rejects historical reads") {
					t.Errorf("SDK accepted historical query %+v: %v", query, err)
				}
			}
			if _, err = owner.Read(t.Context(), durable.Query{Kind: durable.EntryKind, AsOf: first.Seq}); err != nil {
				t.Fatalf("non-document history rejected: %v", err)
			}
			provider.SetResponses(faux.Respond(faux.Text("done")))
			if result, err := owner.Prompt(t.Context(), Prompt{Text: "Read current state"}); err != nil || result.Err != nil {
				t.Fatalf("extension callback run: %+v %v", result, err)
			}
			if callbackCount == 0 {
				t.Fatal("extension read callback did not run")
			}
		})
	}
}
