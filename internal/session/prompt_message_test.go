package session

import (
	"strings"
	"testing"

	"go.harness.dev/harness/internal/document"
	"go.harness.dev/harness/internal/engine/types"
)

func TestBuildUserMessagePreservesOrderAndSharesDeliveryBudget(t *testing.T) {
	delivery := newTestDelivery(t, t.TempDir(), 200000)
	store := document.SessionNamespace("sess")
	media := []types.ContentBlock{
		{Type: types.BlockImage, Data: "aGVsbG8=", MimeType: "image/png"},
		types.NewDocumentRef(store, strings.Repeat("1", 64), "application/pdf", "a.pdf", 1000, 40),
		types.NewDocumentRef(store, strings.Repeat("2", 64), "application/pdf", "b.pdf", 1000, 40),
	}
	message := BuildUserMessage("inspect", media, delivery)
	blocks := message.Content.Blocks
	if len(blocks) != 4 || blocks[0].Text != "inspect" || blocks[1].Type != types.BlockImage || blocks[1].Data != media[0].Data {
		t.Fatalf("prompt order = %#v", blocks)
	}
	if blocks[2].Type != types.BlockDocumentRef || blocks[2].RefKey != media[1].RefKey {
		t.Fatalf("first PDF = %#v, want native reference", blocks[2])
	}
	if blocks[3].Type != types.BlockText || !strings.Contains(blocks[3].Text, "too large") {
		t.Fatalf("second PDF = %#v, want aggregate-budget note", blocks[3])
	}
	if media[2].Type != types.BlockDocumentRef {
		t.Fatal("building the message mutated caller media")
	}
	if entry, ok := delivery.Registry.Get("b.pdf"); !ok || entry.Blob.Key != media[2].RefKey {
		t.Fatalf("second PDF wasn't registered for tool access: %+v, %v", entry, ok)
	}
}
