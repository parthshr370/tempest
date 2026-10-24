// Package mcpwire implements the two handshake-era MCP revisions and transports.
package mcpwire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"go.harness.dev/harness/internal/build"
	"go.harness.dev/harness/internal/mcpconfig"
	"go.harness.dev/harness/internal/rpc"
)

// PreferredVersion is the revision proposed during initialization.
const PreferredVersion = "2025-11-25"

// SupportedVersions lists implemented revisions, in preference order.
var SupportedVersions = [...]string{PreferredVersion, "2025-06-18"}

// MaxResponse bounds a complete inbound MCP frame before JSON decoding.
const MaxResponse = 16 << 20

// MaxRequest bounds outbound requests, including their JSON-RPC envelope.
const MaxRequest = 1 << 20

// Handlers receives server requests and notifications outside transport locks.
type Handlers struct {
	Request      func(context.Context, string, json.RawMessage) (any, *rpc.Error)
	Notify       func(string, json.RawMessage)
	AdmitProcess func(context.Context) (func(), error)
}

type transport interface {
	call(context.Context, string, string, any) (json.RawMessage, error)
	notify(context.Context, string, any) error
	initialized()
	close(context.Context) error
	alive() bool
}

// Client owns one negotiated MCP connection. Calls are never retried.
type Client struct {
	transport   transport
	version     string
	tools       bool
	listChanged bool
	next        atomic.Uint64
	callTimeout time.Duration
	mu          sync.Mutex
	closed      bool
	redactor    credentialRedactor
}

// Version returns the negotiated protocol revision.
func (c *Client) Version() string { return c.version }

// HasTools reports whether the server advertised the tools capability.
func (c *Client) HasTools() bool { return c.tools }

// ListChanged reports whether tool-list notifications were negotiated.
func (c *Client) ListChanged() bool { return c.listChanged }

// Alive reports whether the connection can accept future work.
func (c *Client) Alive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed && c.transport.alive()
}

// Dial starts the transport and completes initialize/initialized without guessing revisions.
func Dial(ctx context.Context, cfg mcpconfig.Server, cwd string, handlers Handlers) (_ *Client, err error) {
	redactor := newCredentialRedactor(cfg)
	defer func() { err = redactor.err(err) }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	configs, err := mcpconfig.Normalize([]mcpconfig.Server{cfg})
	if err != nil {
		return nil, err
	}
	cfg = configs[0]
	startup, _ := time.ParseDuration(cfg.StartupTimeout)
	startupCtx, cancel := context.WithTimeout(ctx, startup)
	defer cancel()
	root := (&url.URL{Scheme: "file", Path: cwd}).String()
	notify := handlers.Notify
	if notify != nil {
		handlers.Notify = func(method string, params json.RawMessage) {
			notify(redactor.text(method), redactor.json(params))
		}
	}
	supplied := handlers.Request
	handlers.Request = func(ctx context.Context, method string, params json.RawMessage) (any, *rpc.Error) {
		switch method {
		case "ping":
			return struct{}{}, nil
		case "roots/list":
			return struct {
				Roots []struct {
					URI  string `json:"uri"`
					Name string `json:"name"`
				} `json:"roots"`
			}{Roots: []struct {
				URI  string `json:"uri"`
				Name string `json:"name"`
			}{{URI: root, Name: "Workspace"}}}, nil
		default:
			if supplied != nil {
				return supplied(ctx, redactor.text(method), redactor.json(params))
			}
			return nil, rpc.NewError(-32601, "method_not_found", "unsupported MCP server request")
		}
	}
	var wire transport
	if cfg.Transport == "stdio" {
		wire, err = dialStdio(startupCtx, cfg, cwd, handlers, redactor)
	} else {
		wire, err = dialHTTP(cfg, handlers)
	}
	if err != nil {
		return nil, fmt.Errorf("mcp_startup: server %s transport: %w", cfg.Name, err)
	}
	callTimeout, _ := time.ParseDuration(cfg.CallTimeout)
	client := &Client{transport: wire, callTimeout: callTimeout, redactor: redactor}
	fail := func(cause error) (*Client, error) {
		closeCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		_ = wire.close(closeCtx)
		return nil, fmt.Errorf("mcp_startup: server %s initialize: %w", cfg.Name, cause)
	}
	params := struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Roots struct {
				ListChanged bool `json:"listChanged"`
			} `json:"roots"`
		} `json:"capabilities"`
		ClientInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"clientInfo"`
	}{ProtocolVersion: PreferredVersion}
	params.ClientInfo.Name = "tempest"
	params.ClientInfo.Version = build.Current().Version
	raw, err := wire.call(startupCtx, "mcp_1", "initialize", params)
	if err != nil {
		return fail(redactor.err(err))
	}
	client.next.Store(1)
	var result struct {
		ProtocolVersion string          `json:"protocolVersion"`
		Capabilities    json.RawMessage `json:"capabilities"`
		ServerInfo      json.RawMessage `json:"serverInfo"`
	}
	if json.Unmarshal(raw, &result) != nil || len(result.Capabilities) == 0 || len(result.ServerInfo) == 0 {
		return fail(errors.New("mcp_protocol: malformed initialize result"))
	}
	var serverInfo struct {
		Name    string  `json:"name"`
		Version *string `json:"version"`
	}
	capabilities := bytes.TrimSpace(result.Capabilities)
	if len(capabilities) == 0 || capabilities[0] != '{' || json.Unmarshal(result.ServerInfo, &serverInfo) != nil || serverInfo.Name == "" || serverInfo.Version == nil {
		return fail(errors.New("mcp_protocol: initialize capabilities/serverInfo must be objects with required fields"))
	}
	if result.ProtocolVersion != SupportedVersions[0] && result.ProtocolVersion != SupportedVersions[1] {
		return fail(fmt.Errorf("mcp_version: received %q; supported %s, %s", result.ProtocolVersion, SupportedVersions[0], SupportedVersions[1]))
	}
	client.version = result.ProtocolVersion
	var caps struct {
		Tools *struct {
			ListChanged bool `json:"listChanged"`
		} `json:"tools"`
	}
	if json.Unmarshal(result.Capabilities, &caps) != nil {
		return fail(errors.New("mcp_protocol: malformed capabilities"))
	}
	if caps.Tools != nil {
		client.tools = true
		client.listChanged = caps.Tools.ListChanged
	}
	if httpWire, ok := wire.(*httpTransport); ok {
		httpWire.setVersion(client.version)
	}
	if err = wire.notify(startupCtx, "notifications/initialized", nil); err != nil {
		return fail(err)
	}
	wire.initialized()
	return client, nil
}

// Call sends one request. Cancellation never replays it and retires uncertain tool calls.
func (c *Client) Call(ctx context.Context, method string, params any) (raw json.RawMessage, err error) {
	defer func() {
		raw = c.redactor.json(raw)
		err = c.redactor.err(err)
	}()
	callCtx, cancel := context.WithTimeout(ctx, c.callTimeout)
	defer cancel()
	id := fmt.Sprintf("mcp_%d", c.next.Add(1))
	raw, err = c.transport.call(callCtx, id, method, params)
	err = c.redactor.err(err)
	if err == nil {
		return raw, nil
	}
	if callCtx.Err() != nil && method != "initialize" {
		cancelCtx, stop := context.WithTimeout(context.Background(), 250*time.Millisecond)
		_ = c.transport.notify(cancelCtx, "notifications/cancelled", struct {
			RequestID string `json:"requestId"`
			Reason    string `json:"reason"`
		}{id, "client cancelled request"})
		stop()
		if method == "tools/call" {
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = c.Close(closeCtx)
			closeCancel()
		}
		code := "mcp_cancelled"
		if errors.Is(callCtx.Err(), context.DeadlineExceeded) {
			code = "mcp_timeout"
		}
		return nil, fmt.Errorf("%s: %w; cancellation does not prove external work stopped; remote outcome may be unknown", code, callCtx.Err())
	}
	var rpcErr *rpc.Error
	if errors.As(err, &rpcErr) {
		return nil, fmt.Errorf("%s: remote JSON-RPC code %d: %w", method, rpcErr.Code, rpcErr)
	}
	return nil, fmt.Errorf("mcp_disconnected: %s: %w; remote outcome may be unknown", method, err)
}

// Notify sends a notification with this connection's negotiated headers.
func (c *Client) Notify(ctx context.Context, method string, params any) error {
	return c.redactor.err(c.transport.notify(ctx, method, params))
}

// Close stops all streams and child processes. It doesn't delete result spills.
func (c *Client) Close(ctx context.Context) error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return c.redactor.err(c.transport.close(ctx))
}
