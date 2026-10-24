package mcpclient

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/work"
	"go.harness.dev/harness/tempest/extension"
)

// Bundles exposes MCP tools through the same handlers as Go and subprocess peers.
// Remote hints don't authorize replay or grant read access.
func (manager *Manager) Bundles() []extension.Definition {
	bundles := make([]extension.Definition, 0, len(manager.servers))
	for _, peer := range manager.servers {
		peer.mu.Lock()
		tools := append([]agent.AgentTool(nil), peer.tools...)
		name := peer.config.Name
		peer.mu.Unlock()
		schemas := make([]types.Tool, 0, len(tools))
		for _, tool := range tools {
			schemas = append(schemas, tool.Tool)
		}
		raw, err := json.Marshal(schemas)
		if err != nil {
			panic(err)
		}
		digest := sha256.Sum256(raw)
		bundle := extension.Definition{ID: "mcp_" + name, Revision: fmt.Sprintf("catalog-%x", digest[:])}
		for _, tool := range tools {
			local := strings.TrimPrefix(tool.Name, "mcp__"+name+"__")
			bundle.Tools = append(bundle.Tools, extension.Tool{Name: local, Title: tool.Label, Description: tool.Description, InputSchema: append(json.RawMessage(nil), tool.Parameters...), OutputSchema: append(json.RawMessage(nil), tool.OutputSchema...), Effect: extension.EffectExecute, Replay: extension.ReplayNever, Execute: func(ctx context.Context, call extension.Call, inv extension.Invocation) (extension.Result, error) {
				var progressMu sync.Mutex
				var number uint64
				var outputStopped bool
				result, executeErr := tool.Execute(ctx, call.ID, call.Arguments, func(partial agent.AgentToolResult) {
					progressMu.Lock()
					defer progressMu.Unlock()
					if outputStopped {
						return
					}
					number++
					if err := inv.Output(ctx, extension.OutputChunk{Number: number, Content: bundleContent(partial.Content)}); err != nil {
						outputStopped = true
						slog.Warn("MCP progress observation stopped", "tool", tool.Name, "error", err)
					}
				})
				control := ""
				if result.Terminate {
					control = "terminate"
				}
				return extension.Result{Content: bundleContent(result.Content), Details: result.Details, StructuredContent: result.StructuredContent, IsError: result.IsError, Control: control}, executeErr
			}})
		}
		bundles = append(bundles, bundle)
	}
	return bundles
}

func (manager *Manager) bundleTools() []agent.AgentTool {
	var tools []agent.AgentTool
	for _, bundle := range manager.Bundles() {
		server := strings.TrimPrefix(bundle.ID, "mcp_")
		for _, tool := range bundle.Tools {
			tools = append(tools, agent.AgentTool{Tool: types.Tool{Name: "mcp__" + server + "__" + tool.Name, Description: tool.Description, Parameters: append(json.RawMessage(nil), tool.InputSchema...)}, OutputSchema: append(json.RawMessage(nil), tool.OutputSchema...), Label: tool.Title, Definition: agent.ToolDefinition{Identity: extension.ToolIdentity(bundle.ID, tool.Name), Revision: bundle.Revision, Effect: string(tool.Effect), Replay: extension.ReplayNever}, ExecutionMode: agent.ExecSequential, Execute: func(ctx context.Context, id string, args json.RawMessage, update agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
				occurrence, _ := work.InvocationFromContext(ctx)
				logical := occurrence.CallID
				if logical == "" {
					logical = id
				}
				grant := extension.NewGrant(ctx, extension.GrantOptions{Identity: extension.InvocationIdentity{InvocationID: logical, ExtensionID: bundle.ID, Revision: bundle.Revision, Handler: "tool:" + tool.Name, Attempt: 1}, Output: func(_ context.Context, chunk extension.OutputChunk) error {
					if update != nil {
						update(agent.AgentToolResult{Content: agentContent(chunk.Content)})
					}
					return nil
				}})
				defer grant.Revoke()
				result, err := tool.Execute(ctx, extension.Call{ID: id, Name: tool.Name, Arguments: args}, grant)
				return agent.AgentToolResult{Content: agentContent(result.Content), Details: result.Details, StructuredContent: result.StructuredContent, IsError: result.IsError, Terminate: result.Control == "terminate"}, err
			}})
		}
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools
}
func bundleContent(blocks []types.ContentBlock) []extension.Content {
	content := make([]extension.Content, 0, len(blocks))
	for _, block := range blocks {
		content = append(content, extension.Content{Type: string(block.Type), Text: block.Text, Data: block.Data, MimeType: block.MimeType, RefStore: block.RefStore, RefKey: block.RefKey, RefMediaType: block.RefMediaType, RefFilename: block.RefFilename, RefSizeBytes: block.RefSizeBytes, RefPageCount: block.RefPageCount})
	}
	return content
}
func agentContent(blocks []extension.Content) []types.ContentBlock {
	content := make([]types.ContentBlock, 0, len(blocks))
	for _, block := range blocks {
		content = append(content, types.ContentBlock{Type: types.BlockType(block.Type), Text: block.Text, Data: block.Data, MimeType: block.MimeType, RefStore: block.RefStore, RefKey: block.RefKey, RefMediaType: block.RefMediaType, RefFilename: block.RefFilename, RefSizeBytes: block.RefSizeBytes, RefPageCount: block.RefPageCount})
	}
	return content
}
