package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/rpc"
)

// SamplingRequest is one server's sampling/createMessage, converted to the
// model's message types. Model preferences, temperature, stop sequences and
// includeContext are not carried: Tempest picks the model and its settings,
// and no other conversation is ever shared (includeContext is "none").
type SamplingRequest struct {
	Server string
	// ID is unique per request; usage receipts are named
	// mcp-sampling:<Server>:<ID>.
	ID           string
	SystemPrompt string
	Messages     []types.Message
	// MaxTokens is the server's requested limit, always positive. The
	// sampler applies its own cap.
	MaxTokens int
}

// Source names the request's usage receipt.
func (r SamplingRequest) Source() string { return "mcp-sampling:" + r.Server + ":" + r.ID }

// Sampler answers an approved or refused sampling request. It returns the
// model's completed message; an error wrapping ErrSamplingRefused tells
// the server the user (or policy) declined.
type Sampler func(context.Context, SamplingRequest) (types.AssistantMessage, error)

// ErrSamplingRefused marks a sampling request declined by the user or by
// policy; the server receives its message with code -1, the spec's
// user-rejection code.
var ErrSamplingRefused = errors.New("sampling refused")

// sampling answers sampling/createMessage for a server configured with
// sampling: true. Tool use in sampling is refused because the capability
// does not declare it.
func (s *server) sampling(ctx context.Context, params json.RawMessage) (any, *rpc.Error) {
	var request struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		SystemPrompt string          `json:"systemPrompt"`
		MaxTokens    int             `json:"maxTokens"`
		Tools        json.RawMessage `json:"tools"`
		ToolChoice   json.RawMessage `json:"toolChoice"`
	}
	if json.Unmarshal(params, &request) != nil || len(request.Messages) == 0 || request.MaxTokens <= 0 {
		return nil, rpc.NewError(-32602, "invalid_params", "sampling/createMessage needs messages and a positive maxTokens")
	}
	if nonNull(request.Tools) || nonNull(request.ToolChoice) {
		return nil, rpc.NewError(-32602, "invalid_params", "tool use in sampling is not supported: the client did not declare sampling.tools")
	}
	converted := SamplingRequest{Server: s.config.Name, SystemPrompt: request.SystemPrompt, MaxTokens: request.MaxTokens}
	now := time.Now().UnixMilli()
	for _, message := range request.Messages {
		blocks, err := samplingContent(message.Content)
		if err != nil {
			return nil, rpc.NewError(-32602, "invalid_params", err.Error())
		}
		switch message.Role {
		case "user":
			converted.Messages = append(converted.Messages, types.UserMessage{Content: types.BlockContent(blocks...), Timestamp: now})
		case "assistant":
			for _, block := range blocks {
				if block.Type != types.BlockText {
					return nil, rpc.NewError(-32602, "invalid_params", "assistant sampling messages may carry only text")
				}
			}
			converted.Messages = append(converted.Messages, types.AssistantMessage{Content: blocks, StopReason: types.StopStop, Timestamp: now})
		default:
			return nil, rpc.NewError(-32602, "invalid_params", `sampling message role must be "user" or "assistant"`)
		}
	}
	id, err := randomID()
	if err != nil {
		return nil, rpc.NewError(-32603, "sampling_failed", err.Error())
	}
	converted.ID = id
	reply, err := s.manager.sample(ctx, converted)
	if errors.Is(err, ErrSamplingRefused) {
		s.manager.logger.Info("refused MCP sampling", "server", s.config.Name, "reason", err.Error())
		return nil, rpc.NewError(-1, "sampling_refused", err.Error())
	}
	if err != nil {
		return nil, rpc.NewError(-32603, "sampling_failed", err.Error())
	}
	var text strings.Builder
	for _, block := range reply.Content {
		if block.Type == types.BlockText {
			text.WriteString(block.Text)
		}
	}
	stop := "endTurn"
	if reply.StopReason == types.StopLength {
		stop = "maxTokens"
	}
	return map[string]any{"role": "assistant", "content": map[string]string{"type": "text", "text": text.String()}, "model": reply.Model, "stopReason": stop}, nil
}

// samplingContent converts one message's content, a single block or (from
// 2025-11-25) an array of blocks. Text and images are accepted.
func samplingContent(raw json.RawMessage) ([]types.ContentBlock, error) {
	type block struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Data     string `json:"data"`
		MimeType string `json:"mimeType"`
	}
	var blocks []block
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "[") {
		if json.Unmarshal(raw, &blocks) != nil {
			return nil, errors.New("invalid sampling message content")
		}
	} else {
		var single block
		if json.Unmarshal(raw, &single) != nil {
			return nil, errors.New("invalid sampling message content")
		}
		blocks = []block{single}
	}
	if len(blocks) == 0 {
		return nil, errors.New("sampling message content is empty")
	}
	out := make([]types.ContentBlock, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text":
			out = append(out, types.NewText(b.Text))
		case "image":
			if b.Data == "" || !strings.HasPrefix(b.MimeType, "image/") {
				return nil, errors.New("sampling image content needs data and an image mimeType")
			}
			out = append(out, types.NewImage(b.Data, b.MimeType))
		case "tool_use", "tool_result":
			return nil, errors.New("tool use in sampling is not supported: the client did not declare sampling.tools")
		default:
			return nil, fmt.Errorf("sampling content type %q is not supported", b.Type)
		}
	}
	return out, nil
}

func nonNull(raw json.RawMessage) bool {
	value := strings.TrimSpace(string(raw))
	return value != "" && value != "null"
}
