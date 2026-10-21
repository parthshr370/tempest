// Package rpc implements bidirectional JSON-RPC 2.0 connections.
package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// MaxFrame is the largest JSON frame we accept or send, excluding the line ending.
const MaxFrame = 1 << 20

// Error is a JSON-RPC error with a stable machine-readable name.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

func (e *Error) Error() string { return e.Message }

// NewError adds the code name required by the Tempest wire contract.
func NewError(code int, name, message string) *Error {
	return &Error{Code: code, Message: message, Data: map[string]string{"code": name}}
}

// Handler handles requests and notifications. Its context ends on disconnect.
type Handler func(context.Context, string, json.RawMessage) (any, *Error)

// Response starts After only once its result has entered the writer queue.
// Watch handlers use this to keep live events behind their snapshot response.
type Response struct {
	Value any
	After func()
}

type frameTransport interface {
	read(context.Context) ([]byte, error)
	write(context.Context, []byte) error
	close() error
}

var errOversize = errors.New("frame too large")

// ErrClosed reports a disconnected peer, including calls still awaiting replies.
var ErrClosed = errors.New("rpc connection closed")

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Conn owns one reader and one writer. Start Serve before making outbound calls.
type Conn struct {
	transport frameTransport
	ctx       context.Context
	cancel    context.CancelFunc
	once      sync.Once
	out       chan []byte
	mu        sync.Mutex
	pending   map[string]chan message
	next      atomic.Uint64
}

func newConn(t frameTransport) *Conn {
	ctx, cancel := context.WithCancel(context.Background())
	return &Conn{transport: t, ctx: ctx, cancel: cancel, out: make(chan []byte, 32), pending: make(map[string]chan message)}
}

// Done closes when either transport direction shuts down.
func (c *Conn) Done() <-chan struct{} { return c.ctx.Done() }

// Close rejects pending calls and releases the transport.
func (c *Conn) Close() error {
	var err error
	c.once.Do(func() { c.cancel(); err = c.transport.close() })
	return err
}

// Serve reads until EOF. Malformed and oversized frames don't disconnect the peer.
func (c *Conn) Serve(handler Handler) error {
	defer c.Close()
	go c.writeLoop()
	for {
		data, err := c.transport.read(c.ctx)
		if errors.Is(err, errOversize) {
			if err = c.reply(nil, nil, NewError(-32600, "frame_too_large", "frame exceeds 1 MiB")); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) || c.ctx.Err() != nil {
				return nil
			}
			return err
		}
		data = bytes.TrimSpace(data)
		var m message
		if !json.Valid(data) {
			_ = c.reply(nil, nil, NewError(-32700, "parse_error", "invalid JSON"))
			continue
		}
		if err = json.Unmarshal(data, &m); err != nil || m.JSONRPC != "2.0" || len(data) == 0 || data[0] != '{' || !validID(m.ID) {
			_ = c.reply(nil, nil, NewError(-32600, "invalid_request", "invalid JSON-RPC request"))
			continue
		}
		if m.Method == "" {
			if len(m.ID) == 0 || (len(m.Result) == 0) == (m.Error == nil) {
				_ = c.reply(nil, nil, NewError(-32600, "invalid_request", "invalid JSON-RPC response"))
				continue
			}
			c.mu.Lock()
			ch := c.pending[string(m.ID)]
			c.mu.Unlock()
			if ch != nil {
				select {
				case ch <- m:
				default:
				}
			}
			continue
		}
		if len(m.Result) != 0 || m.Error != nil || (len(m.Params) > 0 && m.Params[0] != '{' && m.Params[0] != '[') {
			_ = c.reply(m.ID, nil, NewError(-32600, "invalid_request", "invalid JSON-RPC request"))
			continue
		}
		go func(m message) {
			var value any
			var rpcErr *Error
			if handler == nil {
				rpcErr = NewError(-32601, "method_not_found", "unknown method: "+m.Method)
			} else {
				value, rpcErr = handler(c.ctx, m.Method, m.Params)
			}
			var after func()
			if response, ok := value.(Response); ok {
				value = response.Value
				after = response.After
			}
			if len(m.ID) > 0 {
				if c.reply(m.ID, value, rpcErr) != nil {
					return
				}
			}
			if after != nil && c.ctx.Err() == nil {
				after()
			}
		}(m)
	}
}

func validID(id json.RawMessage) bool {
	if len(id) == 0 || bytes.Equal(id, []byte("null")) {
		return true
	}
	var v any
	if json.Unmarshal(id, &v) != nil {
		return false
	}
	switch v.(type) {
	case string, float64:
		return true
	}
	return false
}

func (c *Conn) writeLoop() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case frame := <-c.out:
			if c.transport.write(c.ctx, frame) != nil {
				_ = c.Close()
				return
			}
		}
	}
}
func (c *Conn) send(ctx context.Context, frame []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return ErrClosed
	case c.out <- frame:
		return nil
	}
}
func (c *Conn) reply(id json.RawMessage, result any, rpcErr *Error) error {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	m := message{JSONRPC: "2.0", ID: id, Error: rpcErr}
	if rpcErr == nil {
		b, err := json.Marshal(result)
		if err != nil {
			m.Error = NewError(-32603, "internal_error", err.Error())
		} else {
			m.Result = b
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(b) > MaxFrame {
		m.Result = nil
		m.Error = NewError(-32000, "frame_too_large", "response exceeds 1 MiB")
		b, _ = json.Marshal(m)
	}
	return c.send(c.ctx, b)
}

// Notify queues a notification, replacing oversized payloads with frame_dropped.
func (c *Conn) Notify(ctx context.Context, method string, params any) error {
	p, err := json.Marshal(params)
	if err != nil {
		return err
	}
	if params == nil {
		p = nil
	}
	b, err := json.Marshal(message{JSONRPC: "2.0", Method: method, Params: p})
	if err != nil {
		return err
	}
	if len(b) > MaxFrame {
		size := len(b)
		p, _ = json.Marshal(map[string]any{"method": method, "bytes": size})
		b, _ = json.Marshal(message{JSONRPC: "2.0", Method: "tempest/frame_dropped", Params: p})
		if len(b) > MaxFrame {
			return NewError(-32000, "frame_too_large", "notification method exceeds frame cap")
		}
	}
	return c.send(ctx, b)
}

// Call sends a request and waits for its matching response or cancellation.
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	p, err := json.Marshal(params)
	if err != nil {
		return err
	}
	if params == nil {
		p = nil
	}
	id := json.RawMessage(fmt.Sprintf("\"tempest_%d\"", c.next.Add(1)))
	b, err := json.Marshal(message{JSONRPC: "2.0", ID: id, Method: method, Params: p})
	if err != nil {
		return err
	}
	if len(b) > MaxFrame {
		return NewError(-32000, "frame_too_large", "request exceeds 1 MiB")
	}
	ch := make(chan message, 1)
	c.mu.Lock()
	c.pending[string(id)] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, string(id)); c.mu.Unlock() }()
	if err = c.send(ctx, b); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return ErrClosed
	case m := <-ch:
		if m.Error != nil {
			return m.Error
		}
		if result == nil {
			return nil
		}
		return json.Unmarshal(m.Result, result)
	}
}
