package extensionhost

import (
	"encoding/json"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/tempest/extension"
)

// evidence is the versioned, serializable vocabulary written by this adapter.
// Executable handlers and capability tokens never enter it.
type evidence struct {
	Kind             string                  `json:"kind"`
	CallID           string                  `json:"callId,omitempty"`
	Name             string                  `json:"name,omitempty"`
	Revision         string                  `json:"revision,omitempty"`
	Attempt          uint64                  `json:"attempt,omitempty"`
	Definition       agent.ToolDefinition    `json:"definition,omitempty"`
	Arguments        json.RawMessage         `json:"arguments,omitempty"`
	Result           *extension.Result       `json:"result,omitempty"`
	Decision         *extension.HookDecision `json:"decision,omitempty"`
	Input            *extension.HookInput    `json:"input,omitempty"`
	System           string                  `json:"system,omitempty"`
	ToolSchemaDigest string                  `json:"toolSchemaDigest,omitempty"`
	Tools            []types.Tool            `json:"tools,omitempty"`
	Definitions      []definitionRevision    `json:"definitions,omitempty"`
	Environment      string                  `json:"environment,omitempty"`
	Hash             string                  `json:"hash,omitempty"`
	Error            string                  `json:"error,omitempty"`
}
type definitionRevision struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
}
