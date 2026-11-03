package model

import (
	"encoding/json"

	"go.harness.dev/harness/tempest/durable"
)

// ExtensionID is the built-in extension bundle selected, as required, for
// every worker session and for no other session.
const ExtensionID = "github"

// Worker tool names inside the bundle. The model sees them as
// ext__github__<name>.
const (
	ToolReport = "report"
	ToolReply  = "reply"
	ToolPush   = "push"
	ToolOpenPR = "open_pr"
	ToolStatus = "status"
	ToolThread = "thread"
)

// WorkerDocumentDefinition declares the WorkerTicket document. The initial
// value is an empty object, which readers treat as "not bound yet". A fork
// starts from that initial value so a forked transcript is never bound to
// the Ticket; a conversation reset keeps the binding.
func WorkerDocumentDefinition() durable.DocumentDefinition {
	return durable.DocumentDefinition{
		Namespace: WorkerNamespace,
		Version:   1,
		Schema:    json.RawMessage(`{"type":"object"}`),
		Initial:   json.RawMessage(`{}`),
		History:   durable.HistoryLatest,
		Fork:      durable.ForkInitial,
		Reset:     durable.ResetPreserve,
	}
}
