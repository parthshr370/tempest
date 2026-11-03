package model

import (
	"encoding/json"
	"regexp"
	"testing"
	"time"

	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/tempest/durable"
)

func TestIdentitiesAreDeterministicAndBounded(t *testing.T) {
	request := CommentID(123456789, time.Unix(1_800_000_000, 0))
	if request != "comment:123456789:v1800000000" {
		t.Fatalf("comment id = %s", request)
	}
	ticket := NewTicketID(ThreadRef{Number: 42}, request)
	if ticket != NewTicketID(ThreadRef{Number: 42}, request) || !regexp.MustCompile(`^i42-[0-9a-f]{6}$`).MatchString(string(ticket)) {
		t.Fatalf("ticket id = %s", ticket)
	}
	if pr := NewTicketID(ThreadRef{Number: 42, IsPR: true}, request); pr[0] != 'p' || pr[1:] != ticket[1:] {
		t.Fatalf("pr ticket id = %s", pr)
	}
	if got := TicketBranch(ticket); got != "agent/"+string(ticket) {
		t.Fatalf("branch = %s", got)
	}
	widest := NewTicketID(ThreadRef{Number: 2_147_483_647, IsPR: true}, request)
	launch := LaunchRequestID(widest, ^uint64(0), ^uint32(0))
	if len(launch) >= 128 || launch != "gh:"+string(widest)+":d18446744073709551615:4294967295" {
		t.Fatalf("launch request id = %q (%d bytes)", launch, len(launch))
	}
	if got := DeliveryRequestID(ticket, 3, 2); got != "gh:"+string(ticket)+":i3:2" {
		t.Fatalf("delivery request id = %s", got)
	}
	outbox := NewOutboxID(ticket, 2, "toolu_01ABCdefGHIjklMNOpqrSTUvwx")
	if outbox != NewOutboxID(ticket, 2, "toolu_01ABCdefGHIjklMNOpqrSTUvwx") || OutboxMarker(outbox) != "<!-- tempest:"+string(outbox)+" -->" {
		t.Fatalf("outbox id = %s", outbox)
	}
}

func TestDurationRejectsBareNumbers(t *testing.T) {
	var parsed struct {
		Interval Duration `json:"interval"`
	}
	if err := json.Unmarshal([]byte(`{"interval":"90s"}`), &parsed); err != nil || time.Duration(parsed.Interval) != 90*time.Second {
		t.Fatalf("parse 90s: %v %v", parsed.Interval, err)
	}
	if err := json.Unmarshal([]byte(`{"interval":60}`), &parsed); err == nil {
		t.Fatal("bare number accepted")
	}
	raw, err := json.Marshal(parsed)
	if err != nil || string(raw) != `{"interval":"1m30s"}` {
		t.Fatalf("marshal = %s %v", raw, err)
	}
}

func TestWorkerDocumentDefinitionAcceptsWorkerTicket(t *testing.T) {
	store, err := journal.Create(t.Context(), t.TempDir(), t.TempDir(), journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	if err = store.RegisterDocuments(t.Context(), []durable.DocumentDefinition{WorkerDocumentDefinition()}); err != nil {
		t.Fatal(err)
	}
	ticket := NewTicketID(ThreadRef{Number: 7}, IssueOpenedID(7))
	raw, err := json.Marshal(WorkerTicket{Ticket: ticket, Repo: "o/r", Thread: ThreadRef{Number: 7}, Branch: TicketBranch(ticket)})
	if err != nil {
		t.Fatal(err)
	}
	change := durable.Change{SessionID: store.ID(), Kind: durable.DocumentKind, Namespace: WorkerNamespace, ID: WorkerDocument, Version: 1, Value: raw}
	if _, err = store.Commit(t.Context(), durable.Commit{ID: "bind:" + string(ticket), Changes: []durable.Change{change}}); err != nil {
		t.Fatal(err)
	}
}
