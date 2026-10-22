package mcpwire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.harness.dev/harness/internal/mcpconfig"
	"go.harness.dev/harness/internal/rpc"
)

type httpMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpc.Error      `json:"error,omitempty"`
}

type httpTransport struct {
	cfg       mcpconfig.Server
	client    *http.Client
	handlers  Handlers
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	session   string
	version   string
	closed    bool
	invalid   bool
	workers   sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
}

func dialHTTP(cfg mcpconfig.Server, handlers Handlers) (*httpTransport, error) {
	startup, _ := time.ParseDuration(cfg.StartupTimeout)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = startup
	transport.DialContext = (&net.Dialer{Timeout: startup, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = startup
	ctx, cancel := context.WithCancel(context.Background())
	return &httpTransport{cfg: cfg, handlers: handlers, ctx: ctx, cancel: cancel, client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("mcp_protocol: redirects are forbidden") }}}, nil
}
func (h *httpTransport) setVersion(version string) { h.mu.Lock(); h.version = version; h.mu.Unlock() }
func (h *httpTransport) alive() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.closed && !h.invalid
}
func (h *httpTransport) invalidate() { h.mu.Lock(); h.invalid = true; h.mu.Unlock(); h.cancel() }
func (h *httpTransport) begin(ctx context.Context) (context.Context, func(), error) {
	h.mu.Lock()
	if h.closed || h.invalid {
		h.mu.Unlock()
		return nil, nil, errors.New("MCP HTTP session unavailable")
	}
	h.workers.Add(1)
	h.mu.Unlock()
	linked, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(h.ctx, cancel)
	return linked, func() { stop(); cancel(); h.workers.Done() }, nil
}
func (h *httpTransport) request(ctx context.Context, method string, body []byte, initial bool, cursor string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, h.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for key, value := range h.cfg.Headers {
		req.Header.Set(key, value)
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	if method == http.MethodGet {
		req.Header.Set("Accept", "text/event-stream")
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		// Even an operator-provided Idempotency-Key must not make Transport replay MCP.
		req.GetBody = nil
	}
	if !initial {
		h.mu.Lock()
		session, version := h.session, h.version
		h.mu.Unlock()
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		if version != "" {
			req.Header.Set("MCP-Protocol-Version", version)
		}
	}
	if cursor != "" {
		req.Header.Set("Last-Event-ID", cursor)
	}
	response, err := h.client.Do(req)
	if err != nil {
		var requestError *url.Error
		if errors.As(err, &requestError) {
			return nil, fmt.Errorf("MCP HTTP %s: %w", method, requestError.Err)
		}
		return nil, err
	}
	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		response.Body.Close()
		return nil, errors.New("mcp_auth: server rejected credentials")
	case http.StatusNotFound:
		response.Body.Close()
		h.invalidate()
		return nil, errors.New("mcp_disconnected: HTTP session expired; remote outcome may be unknown")
	}
	return response, nil
}
func responseType(response *http.Response) string {
	contentType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	return contentType
}
func boundedJSON(reader io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, MaxResponse+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxResponse {
		return nil, errors.New("mcp_output_limit: response exceeds 16 MiB; remote outcome may be unknown")
	}
	if !utf8.Valid(raw) || !json.Valid(raw) {
		return nil, errors.New("mcp_protocol: invalid response JSON")
	}
	return raw, nil
}
func decodeMessage(raw []byte) (httpMessage, error) {
	var message httpMessage
	if len(raw) == 0 || !utf8.Valid(raw) || json.Unmarshal(raw, &message) != nil || message.JSONRPC != "2.0" || bytes.TrimSpace(raw)[0] != '{' {
		return message, errors.New("mcp_protocol: invalid JSON-RPC message")
	}
	if len(message.ID) > 0 {
		id := bytes.TrimSpace(message.ID)
		if id[0] != '"' && id[0] != '-' && (id[0] < '0' || id[0] > '9') {
			return message, errors.New("mcp_protocol: request ID must be a string or number")
		}
	}
	if len(message.Params) > 0 && bytes.TrimSpace(message.Params)[0] != '{' {
		return message, errors.New("mcp_protocol: MCP params must be an object")
	}
	if message.Method != "" {
		if len(message.Result) > 0 || message.Error != nil {
			return message, errors.New("mcp_protocol: request contains a result")
		}
	} else if len(message.ID) == 0 || bytes.Equal(message.ID, []byte("null")) || (len(message.Result) == 0) == (message.Error == nil) {
		return message, errors.New("mcp_protocol: invalid JSON-RPC response")
	}
	return message, nil
}
func (h *httpTransport) serverMessage(ctx context.Context, message httpMessage) error {
	if message.Method == "" {
		return errors.New("mcp_protocol: unexpected response on notification stream")
	}
	h.mu.Lock()
	if h.closed || h.invalid {
		h.mu.Unlock()
		return errors.New("MCP connection closed")
	}
	h.workers.Add(1)
	h.mu.Unlock()
	go func() {
		defer h.workers.Done()
		if len(message.ID) == 0 {
			if h.handlers.Notify != nil {
				h.handlers.Notify(message.Method, message.Params)
			}
			return
		}
		result, rpcErr := h.handlers.Request(h.ctx, message.Method, message.Params)
		response := httpMessage{JSONRPC: "2.0", ID: message.ID, Error: rpcErr}
		if rpcErr == nil {
			var err error
			response.Result, err = json.Marshal(result)
			if err != nil {
				response.Error = rpc.NewError(-32603, "internal_error", "unable to encode callback result")
			}
		}
		callbackCtx, cancel := context.WithTimeout(h.ctx, mustDuration(h.cfg.CallTimeout))
		defer cancel()
		if err := h.postAccepted(callbackCtx, response); err != nil {
			h.invalidate()
		}
	}()
	return nil
}
func mustDuration(value string) time.Duration {
	duration, _ := time.ParseDuration(value)
	return duration
}
func (h *httpTransport) postAccepted(ctx context.Context, message httpMessage) error {
	encoded, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if len(encoded) > MaxRequest {
		return errors.New("mcp_output_limit: outbound message exceeds 1 MiB")
	}
	response, err := h.request(ctx, http.MethodPost, encoded, false, "")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		return fmt.Errorf("mcp_protocol: notification/response status %d, want 202", response.StatusCode)
	}
	var one [1]byte
	count, err := response.Body.Read(one[:])
	if count != 0 || !errors.Is(err, io.EOF) {
		return errors.New("mcp_protocol: 202 response must be empty")
	}
	return nil
}
func (h *httpTransport) notify(ctx context.Context, method string, params any) error {
	linked, done, err := h.begin(ctx)
	if err != nil {
		return err
	}
	defer done()
	message := httpMessage{JSONRPC: "2.0", Method: method}
	if params != nil {
		message.Params, err = json.Marshal(params)
		if err != nil {
			return err
		}
	}
	return h.postAccepted(linked, message)
}
func (h *httpTransport) call(ctx context.Context, id, method string, params any) (json.RawMessage, error) {
	linked, done, err := h.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	rawID, _ := json.Marshal(id)
	message := httpMessage{JSONRPC: "2.0", ID: rawID, Method: method}
	if params != nil {
		message.Params, err = json.Marshal(params)
		if err != nil {
			return nil, err
		}
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	if len(encoded) > MaxRequest {
		return nil, errors.New("mcp_output_limit: outbound request exceeds 1 MiB")
	}
	response, err := h.request(linked, http.MethodPost, encoded, method == "initialize", "")
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("mcp_protocol: request status %d; remote outcome may be unknown", response.StatusCode)
	}
	if method == "initialize" {
		session := response.Header.Get("Mcp-Session-Id")
		for _, character := range []byte(session) {
			if character < 0x21 || character > 0x7e {
				response.Body.Close()
				return nil, errors.New("mcp_protocol: invalid session id")
			}
		}
		h.mu.Lock()
		h.session = session
		h.mu.Unlock()
	}
	var result httpMessage
	switch responseType(response) {
	case "application/json":
		raw, readErr := boundedJSON(response.Body)
		response.Body.Close()
		if readErr != nil {
			h.invalidate()
			return nil, readErr
		}
		result, err = decodeMessage(raw)
		if err == nil && (result.Method != "" || !bytes.Equal(result.ID, rawID)) {
			err = errors.New("mcp_protocol: mismatched response id/type")
		}
	case "text/event-stream":
		result, err = h.readLogical(linked, response, rawID)
	default:
		response.Body.Close()
		err = errors.New("mcp_protocol: unsupported response content type")
	}
	if err != nil {
		if linked.Err() == nil {
			h.invalidate()
		}
		return nil, err
	}
	if result.Error != nil {
		return nil, result.Error
	}
	return result.Result, nil
}

func waitRetry(ctx context.Context, retry time.Duration) error {
	timer := time.NewTimer(retry)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// readLogical owns a cursor for exactly this POST stream. Resumption is GET-only.
func (h *httpTransport) readLogical(ctx context.Context, response *http.Response, wantedID json.RawMessage) (httpMessage, error) {
	cursor := ""
	retry := defaultSSERetry
	for {
		reader := newSSEReader(response.Body)
		for {
			event, err := reader.next()
			if err != nil {
				response.Body.Close()
				if ctx.Err() != nil {
					return httpMessage{}, ctx.Err()
				}
				if strings.HasPrefix(err.Error(), "mcp_protocol:") || strings.HasPrefix(err.Error(), "mcp_output_limit:") {
					return httpMessage{}, err
				}
				break
			}
			if event.hasID {
				cursor = event.id
			}
			if event.hasRetry {
				retry = event.retry
			}
			if len(event.data) == 0 {
				continue
			}
			message, err := decodeMessage(event.data)
			if err != nil {
				response.Body.Close()
				return message, err
			}
			if message.Method != "" {
				if err = h.serverMessage(ctx, message); err != nil {
					response.Body.Close()
					return message, err
				}
				continue
			}
			if !bytes.Equal(message.ID, wantedID) {
				response.Body.Close()
				return message, errors.New("mcp_protocol: response belongs to another logical stream")
			}
			response.Body.Close()
			return message, nil
		}
		if cursor == "" {
			return httpMessage{}, errors.New("mcp_disconnected: stream ended without cursor or terminal response; remote outcome may be unknown")
		}
		if err := waitRetry(ctx, retry); err != nil {
			return httpMessage{}, err
		}
		resumed, err := h.request(ctx, http.MethodGet, nil, false, cursor)
		if err != nil {
			return httpMessage{}, err
		}
		if resumed.StatusCode != http.StatusOK || responseType(resumed) != "text/event-stream" {
			resumed.Body.Close()
			return httpMessage{}, errors.New("mcp_disconnected: resume rejected; remote outcome may be unknown")
		}
		response = resumed
	}
}
func (h *httpTransport) initialized() {
	h.mu.Lock()
	if h.closed || h.invalid {
		h.mu.Unlock()
		return
	}
	h.workers.Add(1)
	h.mu.Unlock()
	go func() { defer h.workers.Done(); h.listen() }()
}
func (h *httpTransport) listen() {
	cursor := ""
	retry := defaultSSERetry
	for h.ctx.Err() == nil {
		response, err := h.request(h.ctx, http.MethodGet, nil, false, cursor)
		if err != nil {
			if h.ctx.Err() != nil || !h.alive() {
				return
			}
			if strings.Contains(err.Error(), "mcp_auth:") || strings.Contains(err.Error(), "mcp_protocol:") {
				h.invalidate()
				return
			}
			cursor = ""
			_ = h.serverMessage(h.ctx, httpMessage{JSONRPC: "2.0", Method: "notifications/tools/list_changed", Params: json.RawMessage(`{}`)})
			if waitRetry(h.ctx, retry) != nil {
				return
			}
			continue
		}
		if response.StatusCode == http.StatusMethodNotAllowed {
			response.Body.Close()
			if cursor != "" {
				_ = h.serverMessage(h.ctx, httpMessage{JSONRPC: "2.0", Method: "notifications/tools/list_changed", Params: json.RawMessage(`{}`)})
			}
			return
		}
		if response.StatusCode != http.StatusOK || responseType(response) != "text/event-stream" {
			response.Body.Close()
			if cursor != "" {
				cursor = ""
				_ = h.serverMessage(h.ctx, httpMessage{JSONRPC: "2.0", Method: "notifications/tools/list_changed", Params: json.RawMessage(`{}`)})
				if waitRetry(h.ctx, retry) != nil {
					return
				}
				continue
			}
			h.invalidate()
			return
		}
		reader := newSSEReader(response.Body)
		for {
			event, readErr := reader.next()
			if readErr != nil {
				err = readErr
				break
			}
			if event.hasID {
				cursor = event.id
			}
			if event.hasRetry {
				retry = event.retry
			}
			if len(event.data) == 0 {
				continue
			}
			message, decodeErr := decodeMessage(event.data)
			if decodeErr != nil {
				err = decodeErr
				break
			}
			if dispatchErr := h.serverMessage(h.ctx, message); dispatchErr != nil {
				err = dispatchErr
				break
			}
		}
		response.Body.Close()
		if h.ctx.Err() != nil {
			return
		}
		if strings.HasPrefix(err.Error(), "mcp_protocol:") || strings.HasPrefix(err.Error(), "mcp_output_limit:") {
			h.invalidate()
			return
		}
		if cursor == "" {
			_ = h.serverMessage(h.ctx, httpMessage{JSONRPC: "2.0", Method: "notifications/tools/list_changed", Params: json.RawMessage(`{}`)})
		}
		if waitRetry(h.ctx, retry) != nil {
			return
		}
	}
}
func (h *httpTransport) close(ctx context.Context) error {
	h.closeOnce.Do(func() {
		h.mu.Lock()
		h.closed = true
		session := h.session
		h.mu.Unlock()
		h.cancel()
		if session != "" {
			// DELETE is cleanup only; it must survive cancellation of the request streams.
			response, err := h.request(ctx, http.MethodDelete, nil, false, "")
			if err == nil {
				response.Body.Close()
				if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusMethodNotAllowed && response.StatusCode != http.StatusAccepted {
					h.closeErr = fmt.Errorf("MCP DELETE status %d", response.StatusCode)
				}
			}
		}
		joined := make(chan struct{})
		go func() { h.workers.Wait(); close(joined) }()
		select {
		case <-joined:
		case <-ctx.Done():
			h.closeErr = ctx.Err()
		}
		h.client.CloseIdleConnections()
	})
	return h.closeErr
}
