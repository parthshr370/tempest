package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/mcpwire"
	"go.harness.dev/harness/internal/rpc"
)

const maxCatalogPages = 128
const maxServerTools = 2048
const maxDefinitions = 4 << 20
const retirementTimeout = 3 * time.Second
const maxSchema = 64 << 10
const maxModelName = 64
const maxProgressText = 4096

var remoteToolName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type toolDefinition struct {
	Name         string          `json:"name"`
	Title        string          `json:"title"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema"`
}

type noSchemaLoader struct{}

func (noSchemaLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("external schema resolution forbidden: %s", url)
}

func compileSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	if len(raw) > maxSchema || len(raw) == 0 {
		return nil, errors.New("schema absent or exceeds 64 KiB")
	}
	var schema map[string]json.RawMessage
	if json.Unmarshal(raw, &schema) != nil || schema == nil {
		return nil, errors.New("schema must be a non-null object")
	}
	var schemaType string
	if json.Unmarshal(schema["type"], &schemaType) != nil || schemaType != "object" {
		return nil, errors.New("schema type must be object")
	}
	if err := localReferences(raw); err != nil {
		return nil, err
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(noSchemaLoader{})
	if err = compiler.AddResource("mem://mcp-schema", document); err != nil {
		return nil, err
	}
	return compiler.Compile("mem://mcp-schema")
}
func localReferences(raw json.RawMessage) error {
	var schema map[string]json.RawMessage
	if json.Unmarshal(raw, &schema) != nil || schema == nil {
		return nil
	}
	for keyword, value := range schema {
		switch keyword {
		case "$ref", "$dynamicRef", "$recursiveRef":
			var reference string
			if json.Unmarshal(value, &reference) != nil || !strings.HasPrefix(reference, "#") {
				return errors.New("external or invalid schema reference")
			}
		case "$defs", "definitions", "properties", "patternProperties", "dependentSchemas", "dependencies":
			var children map[string]json.RawMessage
			if json.Unmarshal(value, &children) == nil {
				for _, child := range children {
					if err := localReferences(child); err != nil {
						return err
					}
				}
			}
		case "allOf", "anyOf", "oneOf", "prefixItems":
			var children []json.RawMessage
			if json.Unmarshal(value, &children) == nil {
				for _, child := range children {
					if err := localReferences(child); err != nil {
						return err
					}
				}
			}
		case "items":
			var children []json.RawMessage
			if json.Unmarshal(value, &children) == nil {
				for _, child := range children {
					if err := localReferences(child); err != nil {
						return err
					}
				}
			} else if err := localReferences(value); err != nil {
				return err
			}
		case "additionalProperties", "unevaluatedProperties", "propertyNames", "contains", "not", "if", "then", "else", "additionalItems", "unevaluatedItems", "contentSchema":
			if err := localReferences(value); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *server) discover(ctx context.Context, client *mcpwire.Client) ([]agent.AgentTool, error) {
	if client == nil {
		return nil, errors.New("connection unavailable")
	}
	if !client.HasTools() {
		return nil, nil
	}
	var tools []agent.AgentTool
	cursor := ""
	seenCursors := map[string]bool{}
	seenNames := map[string]bool{}
	totalBytes := 0
	for range maxCatalogPages {
		params := struct {
			Cursor string `json:"cursor,omitempty"`
		}{cursor}
		raw, err := client.Call(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var result struct {
			Tools      []json.RawMessage `json:"tools"`
			NextCursor string            `json:"nextCursor"`
		}
		if json.Unmarshal(raw, &result) != nil || result.Tools == nil {
			return nil, errors.New("mcp_protocol: invalid tools/list result")
		}
		for _, definition := range result.Tools {
			totalBytes += len(definition)
			if totalBytes > maxDefinitions || len(tools) >= maxServerTools {
				return nil, errors.New("mcp_catalog_limit: too many tool definitions")
			}
			var remote toolDefinition
			if json.Unmarshal(definition, &remote) != nil {
				return nil, errors.New("mcp_protocol: malformed tool definition")
			}
			if len(remote.Name) > maxModelName-len("mcp__")-len(s.config.Name)-len("__") || !remoteToolName.MatchString(remote.Name) {
				return nil, fmt.Errorf("mcp_tool_name: server %s has unrepresentable tool %q", s.config.Name, utf8Head(remote.Name, maxModelName))
			}
			fullName := "mcp__" + s.config.Name + "__" + remote.Name
			if seenNames[remote.Name] {
				return nil, fmt.Errorf("mcp_protocol: duplicate tool %q", remote.Name)
			}
			seenNames[remote.Name] = true
			if _, err = compileSchema(remote.InputSchema); err != nil {
				return nil, fmt.Errorf("mcp_schema: tool %s input: %w", fullName, err)
			}
			var outputSchema *jsonschema.Schema
			if len(remote.OutputSchema) > 0 {
				outputSchema, err = compileSchema(remote.OutputSchema)
				if err != nil {
					return nil, fmt.Errorf("mcp_schema: tool %s output: %w", fullName, err)
				}
			}
			label := remote.Title
			if label == "" {
				label = remote.Name
			}
			tools = append(tools, agent.AgentTool{Tool: types.Tool{Name: fullName, Description: remote.Description, Parameters: append(json.RawMessage(nil), remote.InputSchema...)}, Label: label, ExecutionMode: agent.ExecSequential, Execute: s.execution(remote.Name, outputSchema)})
		}
		cursor = result.NextCursor
		if cursor == "" {
			return tools, nil
		}
		if seenCursors[cursor] {
			return nil, errors.New("mcp_protocol: repeated tools/list cursor")
		}
		seenCursors[cursor] = true
	}
	return nil, errors.New("mcp_catalog_limit: tools/list exceeds 128 pages")
}

func failedTool(message string) (agent.AgentToolResult, error) {
	if len(message) > maxDetails {
		message = utf8Head(message, maxDetails-len(truncationMarker)) + truncationMarker
	}
	return agent.AgentToolResult{Content: []types.ContentBlock{types.NewText(message)}, IsError: true}, nil
}
func (s *server) execution(remoteName string, outputSchema *jsonschema.Schema) func(context.Context, string, json.RawMessage, agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
	return func(ctx context.Context, toolCallID string, args json.RawMessage, onUpdate agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
		callCtx, cancel := context.WithTimeout(ctx, s.callTimeout)
		defer cancel()
		select {
		case <-s.token:
			defer func() { s.token <- struct{}{} }()
		case <-callCtx.Done():
			return failedTool(fmt.Sprintf("mcp_timeout: server %s: %v; call was not dispatched", s.config.Name, callCtx.Err()))
		}
		s.mu.Lock()
		client := s.client
		unavailable := s.unavailable
		if s.closed || client == nil || unavailable != nil || !client.Alive() {
			s.mu.Unlock()
			return failedTool("mcp_disconnected: server " + s.config.Name + " unavailable; prepare a new run to reconnect")
		}
		progressToken := fmt.Sprintf("progress_%d", s.next.Add(1))
		s.progress[progressToken] = &progressCall{update: onUpdate}
		s.mu.Unlock()
		defer func() { s.mu.Lock(); delete(s.progress, progressToken); s.mu.Unlock() }()
		params := struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Meta      struct {
				ProgressToken string `json:"progressToken"`
			} `json:"_meta"`
		}{Name: remoteName, Arguments: args}
		params.Meta.ProgressToken = progressToken
		raw, err := client.Call(callCtx, "tools/call", params)
		if err != nil {
			var remoteErr *rpc.Error
			if !errors.As(err, &remoteErr) {
				s.retire(client, err)
			}
			return failedTool(fmt.Sprintf("MCP server %s tools/call %s: %v", s.config.Name, remoteName, err))
		}
		result, err := mapResult(s.manager.spillDir, s.config.Name, remoteName, raw, outputSchema)
		if err != nil {
			if strings.Contains(err.Error(), "mcp_protocol") || strings.Contains(err.Error(), "mcp_output_limit") {
				s.retire(client, err)
			}
			return failedTool(fmt.Sprintf("MCP server %s tool %s: %v", s.config.Name, remoteName, err))
		}
		return result, nil
	}
}

func (s *server) retire(client *mcpwire.Client, cause error) {
	s.mu.Lock()
	s.unavailable = cause
	s.mu.Unlock()
	closeCtx, cancel := context.WithTimeout(context.Background(), retirementTimeout)
	defer cancel()
	_ = client.Close(closeCtx)
}
func (s *server) notification(method string, params json.RawMessage) {
	switch method {
	case "notifications/tools/list_changed":
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			return
		}
		if s.client != nil && !s.client.ListChanged() {
			return
		}
		s.dirty = true
		if !s.refreshing && s.client != nil {
			s.startRefreshLocked(s.manager.ctx, false)
		}
	case "notifications/progress":
		var progress struct {
			Token    json.RawMessage `json:"progressToken"`
			Progress *float64        `json:"progress"`
			Total    *float64        `json:"total"`
			Message  string          `json:"message"`
		}
		if json.Unmarshal(params, &progress) != nil || progress.Progress == nil {
			s.manager.logger.Debug("ignored malformed MCP progress", "server", s.config.Name)
			return
		}
		var token string
		if json.Unmarshal(progress.Token, &token) != nil || math.IsInf(*progress.Progress, 0) || math.IsNaN(*progress.Progress) || *progress.Progress < 0 {
			s.manager.logger.Debug("ignored malformed MCP progress", "server", s.config.Name)
			return
		}
		s.mu.Lock()
		call := s.progress[token]
		if call == nil || (call.seen && *progress.Progress <= call.last) || (progress.Total != nil && (*progress.Total < *progress.Progress || math.IsInf(*progress.Total, 0) || math.IsNaN(*progress.Total))) {
			s.mu.Unlock()
			s.manager.logger.Debug("ignored MCP progress", "server", s.config.Name)
			return
		}
		call.last = *progress.Progress
		call.seen = true
		callback := call.update
		s.mu.Unlock()
		if callback != nil && progress.Message != "" {
			callback(agent.AgentToolResult{Content: []types.ContentBlock{types.NewText(utf8Head(progress.Message, maxProgressText))}})
		}
	}
}
