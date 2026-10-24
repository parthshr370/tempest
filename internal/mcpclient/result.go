package mcpclient

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/mcpwire"
	"go.harness.dev/harness/internal/refs"
)

const maxInlineContent = 256 << 10
const maxInlineText = 128 << 10
const maxDetails = 8 << 10
const spillReferenceReserve = 4096
const truncationMarker = "\n[MCP output truncated; full result saved locally]"

type callResult struct {
	Content           []json.RawMessage `json:"content"`
	StructuredContent json.RawMessage   `json:"structuredContent"`
	IsError           bool              `json:"isError"`
}
type contentItem struct {
	Type        string  `json:"type"`
	Text        *string `json:"text"`
	Data        string  `json:"data"`
	MimeType    string  `json:"mimeType"`
	URI         string  `json:"uri"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Resource    *struct {
		URI      string  `json:"uri"`
		MimeType string  `json:"mimeType"`
		Text     *string `json:"text"`
		Blob     *string `json:"blob"`
	} `json:"resource"`
}
type resultDetails struct {
	MCP mcpDetails `json:"mcp"`
}
type mcpDetails struct {
	Server                string          `json:"server"`
	Tool                  string          `json:"tool"`
	IsError               bool            `json:"isError"`
	StructuredContent     json.RawMessage `json:"structuredContent,omitempty"`
	StructuredContentPath string          `json:"structuredContentPath,omitempty"`
	Truncated             bool            `json:"truncated"`
	OutputPath            string          `json:"outputPath,omitempty"`
}

type resultSpill struct {
	base        string
	directory   string
	outputPath  string
	localRoot   string
	original    []byte
	binaryIndex int
}

func (s *resultSpill) reference(path string) string {
	if s.localRoot == "" {
		return path
	}
	relative, err := filepath.Rel(s.localRoot, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return path
	}
	escaped := (&url.URL{Path: "/" + filepath.ToSlash(relative)}).EscapedPath()
	return "local://" + strings.TrimPrefix(escaped, "/")
}

func (s *resultSpill) ensure() error {
	if s.directory != "" {
		return nil
	}
	if s.base == "" {
		return errors.New("mcp_spill: no session output directory")
	}
	if err := os.MkdirAll(s.base, 0700); err != nil {
		return fmt.Errorf("mcp_spill: %w", err)
	}
	directory, err := os.MkdirTemp(s.base, "")
	if err != nil {
		return fmt.Errorf("mcp_spill: %w", err)
	}
	s.directory = directory
	s.outputPath = filepath.Join(directory, "result.json")
	if err = exclusiveFile(s.outputPath, s.original); err != nil {
		return fmt.Errorf("mcp_spill: %w", err)
	}
	return nil
}
func exclusiveFile(path string, contents []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(contents)
	return errors.Join(writeErr, file.Close())
}
func (s *resultSpill) binary(encoded string) (string, error) {
	decoded, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return "", errors.New("mcp_protocol: invalid base64 content")
	}
	return s.binaryBytes(decoded)
}

func (s *resultSpill) binaryBytes(decoded []byte) (string, error) {
	if err := s.ensure(); err != nil {
		return "", err
	}
	s.binaryIndex++
	path := filepath.Join(s.directory, fmt.Sprintf("binary-%d", s.binaryIndex))
	if err := exclusiveFile(path, decoded); err != nil {
		return "", fmt.Errorf("mcp_spill: %w", err)
	}
	return s.reference(path), nil
}
func utf8Head(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	if limit < 0 {
		limit = 0
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}
func serializedSize(block types.ContentBlock) int {
	encoded, _ := json.Marshal(block)
	return len(encoded)
}
func fitText(text string, budget int) string {
	if budget <= serializedSize(types.NewText(truncationMarker)) {
		return ""
	}
	low, high := 0, min(len(text), budget)
	for low < high {
		middle := low + (high-low+1)/2
		candidate := utf8Head(text, middle) + truncationMarker
		if serializedSize(types.NewText(candidate)) <= budget {
			low = middle
		} else {
			high = middle - 1
		}
	}
	return utf8Head(text, low) + truncationMarker
}

func mapResult(spillDir, serverName, toolName string, raw json.RawMessage, outputSchema *jsonschema.Schema) (agent.AgentToolResult, error) {
	if len(raw) > mcpwire.MaxResponse {
		return agent.AgentToolResult{}, errors.New("mcp_output_limit: result exceeds 16 MiB; remote outcome may be unknown")
	}
	var remote callResult
	if !utf8.Valid(raw) || json.Unmarshal(raw, &remote) != nil || remote.Content == nil {
		return agent.AgentToolResult{}, errors.New("mcp_protocol: malformed tools/call result")
	}
	result := agent.AgentToolResult{IsError: remote.IsError}
	details := resultDetails{MCP: mcpDetails{Server: serverName, Tool: toolName, IsError: remote.IsError}}
	spill := resultSpill{base: spillDir, original: raw}
	if filepath.Base(filepath.Dir(spillDir)) == refs.LocalDirName {
		spill.localRoot = filepath.Dir(spillDir)
	}
	contentBytes, textBytes := 2, 0
	appendBlock := func(block types.ContentBlock) error {
		budget := maxInlineContent - spillReferenceReserve - contentBytes
		if block.Type == types.BlockText && maxInlineText-spillReferenceReserve-textBytes < budget {
			budget = maxInlineText - spillReferenceReserve - textBytes
		}
		size := budget + 1
		if block.Type != types.BlockText || len(block.Text) <= budget {
			size = serializedSize(block) + 1
		}
		if size > budget {
			if err := spill.ensure(); err != nil {
				return err
			}
			details.MCP.Truncated = true
			if block.Type == types.BlockText {
				block.Text = fitText(block.Text, budget-1)
				if block.Text == "" {
					return nil
				}
			} else {
				block = types.NewText("Image omitted from inline content; see full MCP result.")
			}
			size = serializedSize(block) + 1
			if size > budget {
				return nil
			}
		}
		result.Content = append(result.Content, block)
		contentBytes += size
		if block.Type == types.BlockText {
			textBytes += size
		}
		return nil
	}
	for _, rawItem := range remote.Content {
		var item contentItem
		if json.Unmarshal(rawItem, &item) != nil || item.Type == "" {
			return agent.AgentToolResult{}, errors.New("mcp_protocol: malformed content item")
		}
		var block types.ContentBlock
		switch item.Type {
		case "text":
			if item.Text == nil {
				return agent.AgentToolResult{}, errors.New("mcp_protocol: text item has no text")
			}
			block = types.NewText(*item.Text)
		case "image":
			imageBytes, err := base64.StdEncoding.Strict().DecodeString(item.Data)
			if err != nil {
				return agent.AgentToolResult{}, errors.New("mcp_protocol: invalid image base64")
			}
			supported := item.MimeType == "image/png" || item.MimeType == "image/jpeg" || item.MimeType == "image/gif" || item.MimeType == "image/webp"
			if supported && http.DetectContentType(imageBytes) != item.MimeType {
				return agent.AgentToolResult{}, errors.New("mcp_protocol: image MIME type doesn't match bytes")
			}
			block = types.NewImage(item.Data, item.MimeType)
			if !supported || len(item.Data) > maxInlineContent-spillReferenceReserve-contentBytes || serializedSize(block) > maxInlineContent-spillReferenceReserve-contentBytes {
				path, err := spill.binaryBytes(imageBytes)
				if err != nil {
					return agent.AgentToolResult{}, err
				}
				block = types.NewText(fmt.Sprintf("Image (%s) saved locally: %s; not passed inline to the model.", item.MimeType, path))
				details.MCP.Truncated = true
			}
		case "resource":
			resource := item.Resource
			if resource == nil || resource.URI == "" || (resource.Text == nil) == (resource.Blob == nil) {
				return agent.AgentToolResult{}, errors.New("mcp_protocol: malformed embedded resource")
			}
			if resource.Text != nil {
				block = types.NewText(fmt.Sprintf("Resource %s (%s):\n%s", resource.URI, resource.MimeType, *resource.Text))
			} else {
				path, err := spill.binary(*resource.Blob)
				if err != nil {
					return agent.AgentToolResult{}, err
				}
				block = types.NewText(fmt.Sprintf("Resource %s (%s) saved locally: %s", resource.URI, resource.MimeType, path))
			}
		case "resource_link":
			if item.URI == "" || item.Name == "" {
				return agent.AgentToolResult{}, errors.New("mcp_protocol: resource link requires URI and name")
			}
			block = types.NewText(fmt.Sprintf("Resource link %s: %s\n%s", item.Name, item.URI, item.Description))
		case "audio":
			path, err := spill.binary(item.Data)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			block = types.NewText(fmt.Sprintf("Audio (%s) saved locally: %s; audio was not passed natively to the model.", item.MimeType, path))
		default:
			if err := spill.ensure(); err != nil {
				return agent.AgentToolResult{}, err
			}
			block = types.NewText("Unsupported MCP content type " + utf8Head(item.Type, 256) + "; original item retained in local result.json.")
		}
		if err := appendBlock(block); err != nil {
			return agent.AgentToolResult{}, err
		}
	}
	if len(remote.StructuredContent) > 0 {
		structured := bytes.TrimSpace(remote.StructuredContent)
		if len(structured) == 0 || structured[0] != '{' {
			return agent.AgentToolResult{}, errors.New("mcp_protocol: structuredContent must be an object")
		}
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(structured))
		if err != nil {
			return agent.AgentToolResult{}, errors.New("mcp_protocol: invalid structuredContent")
		}
		canonical, err := json.Marshal(document)
		if err != nil {
			return agent.AgentToolResult{}, fmt.Errorf("mcp_protocol: structuredContent: %w", err)
		}
		details.MCP.StructuredContent = canonical
		if outputSchema != nil {
			if validationErr := outputSchema.Validate(document); validationErr != nil {
				result.IsError = true
				details.MCP.IsError = true
				if err = appendBlock(types.NewText("mcp_schema: structuredContent failed the tool's outputSchema")); err != nil {
					return agent.AgentToolResult{}, err
				}
			}
		}
		alreadyPresent := false
		for _, block := range result.Content {
			if block.Type == types.BlockText && strings.TrimSpace(block.Text) == string(canonical) {
				alreadyPresent = true
				break
			}
		}
		if !alreadyPresent {
			if err = appendBlock(types.NewText(string(canonical))); err != nil {
				return agent.AgentToolResult{}, err
			}
		}
	}
	encodedDetails, err := json.Marshal(details)
	if err != nil {
		return agent.AgentToolResult{}, err
	}
	if len(encodedDetails) > maxDetails-spillReferenceReserve {
		if err = spill.ensure(); err != nil {
			return agent.AgentToolResult{}, err
		}
		details.MCP.StructuredContent = nil
		details.MCP.StructuredContentPath = spill.reference(spill.outputPath)
	}
	if spill.outputPath != "" {
		outputReference := spill.reference(spill.outputPath)
		details.MCP.OutputPath = outputReference
		reference := types.NewText("Full MCP result saved locally: " + outputReference)
		// Space for this generated reference was reserved before accepting remote content.
		if serializedSize(reference)+contentBytes > maxInlineContent || serializedSize(reference)+textBytes > maxInlineText {
			return agent.AgentToolResult{}, errors.New("mcp_spill: generated output path exceeds inline budget")
		}
		result.Content = append(result.Content, reference)
	}
	result.Details, err = json.Marshal(details)
	if err != nil {
		return agent.AgentToolResult{}, err
	}
	if len(result.Details) > maxDetails {
		return agent.AgentToolResult{}, errors.New("mcp_spill: details exceed 8 KiB")
	}
	return result, nil
}
