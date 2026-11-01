package mcpclient

import (
	"context"
	"encoding/json"
	"errors"

	"go.harness.dev/harness/internal/question"
	"go.harness.dev/harness/internal/rpc"
)

// request answers requests the server sends to Tempest. ping and
// roots/list are answered by the wire layer; sampling/createMessage only
// for a server whose sampling capability was declared.
func (s *server) request(ctx context.Context, method string, params json.RawMessage) (any, *rpc.Error) {
	switch {
	case method == "elicitation/create":
		return s.elicitation(ctx, params)
	case method == "sampling/createMessage" && s.config.Sampling && s.manager.sample != nil:
		return s.sampling(ctx, params)
	default:
		return nil, rpc.NewError(-32601, "method_not_found", "unsupported MCP server request")
	}
}

// elicitation forwards a server's elicitation/create to the session's user
// through the same path as authorization URLs: ACP clients that declared
// the mode, the stdio MCP client of `tempest mcp`, or the terminal. With no
// user to ask, the request is declined.
func (s *server) elicitation(ctx context.Context, params json.RawMessage) (any, *rpc.Error) {
	var request struct {
		Mode            string          `json:"mode"`
		Message         string          `json:"message"`
		RequestedSchema json.RawMessage `json:"requestedSchema"`
		URL             string          `json:"url"`
		ElicitationID   string          `json:"elicitationId"`
	}
	if json.Unmarshal(params, &request) != nil || request.Message == "" {
		return nil, rpc.NewError(-32602, "invalid_params", "elicitation/create needs a message")
	}
	if request.Mode == "" {
		request.Mode = "form"
	}
	elicit := question.Elicit{Server: s.config.Name, Mode: request.Mode, Message: request.Message, Schema: request.RequestedSchema, URL: request.URL, ElicitationID: request.ElicitationID}
	if err := elicit.Validate(); err != nil {
		return nil, rpc.NewError(-32602, "invalid_params", err.Error())
	}
	decline := question.ElicitResult{Action: "decline"}
	if s.manager.elicit == nil {
		s.manager.logger.Info("declined MCP elicitation: no interactive user", "server", s.config.Name, "mode", request.Mode)
		return decline, nil
	}
	reply, err := s.manager.elicit(ctx, elicit)
	if errors.Is(err, question.Unavailable) {
		s.manager.logger.Info("declined MCP elicitation: no client accepts this mode", "server", s.config.Name, "mode", request.Mode)
		return decline, nil
	}
	if err != nil {
		return nil, rpc.NewError(-32603, "elicitation_failed", "the user could not be asked: "+err.Error())
	}
	return reply, nil
}
