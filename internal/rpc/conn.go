// Package rpc implements bidirectional JSON-RPC 2.0 connections.
package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
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

// Deferred is the long-running part of a synchronously admitted request.
type Deferred func() (any, *Error)

// NoResponse suppresses a cancelled request's reply, without skipping its join.
type NoResponse struct{}

type requestIDKey struct{}

// RequestID returns a detached exact JSON ID for the inbound request.
func RequestID(ctx context.Context) json.RawMessage {
	id, _ := ctx.Value(requestIDKey{}).(json.RawMessage)
	return append(json.RawMessage(nil), id...)
}

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

// ErrNotSent reports that an outbound call was not admitted to the writer queue.
var ErrNotSent = errors.New("rpc request not sent")

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
	transport     frameTransport
	ctx           context.Context
	cancel        context.CancelFunc
	once          sync.Once
	out           chan []byte
	mu            sync.Mutex
	pending       map[string]chan message
	next          atomic.Uint64
	accepting     bool
	handlers      sync.WaitGroup
	writerDone    chan struct{}
	maxFrame      int
	protocolError func(error)
	lateIDs       map[string]bool
	drainOnEOF    bool
	// protocol prefixes strict-mode fault codes, such as mcp_protocol or lsp_protocol.
	protocol string
}

func newConn(t frameTransport) *Conn {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Conn{transport: t, ctx: ctx, cancel: cancel, out: make(chan []byte, 32), pending: make(map[string]chan message), accepting: true, writerDone: make(chan struct{}), maxFrame: MaxFrame, protocol: "mcp"}
	go c.writeLoop()
	return c
}

// Done closes when the connection shuts down, not on EOF with SetDrainOnEOF.
func (c *Conn) Done() <-chan struct{} { return c.ctx.Done() }

// SetProtocolErrorHandler selects strict client framing. Call it before Serve.
// With no handler, malformed peer frames retain the server's recover-and-reply policy.
func (c *Conn) SetProtocolErrorHandler(handler func(error)) {
	c.protocolError = handler
	if stream, ok := c.transport.(*lineTransport); ok {
		stream.stopOnOversize = handler != nil
	}
}

// SetDrainOnEOF keeps the writer and handler contexts alive after reader EOF.
// Set it before Serve; the owner must then call Drain and Close.
func (c *Conn) SetDrainOnEOF(enabled bool) {
	c.drainOnEOF = enabled
}

// fault names a strict-mode violation with the connection's protocol prefix.
func (c *Conn) fault(kind, detail string) error {
	return fmt.Errorf("%s_%s: %s", c.protocol, kind, detail)
}

func (c *Conn) rejectProtocol(err error) bool {
	if c.protocolError == nil {
		return false
	}
	c.protocolError(err)
	return true
}

// Drain stops admitting requests and waits for their replies to enter the queue.
// The reader continues correlating responses to outbound calls.
func (c *Conn) Drain(ctx context.Context) error {
	c.mu.Lock()
	c.accepting = false
	c.mu.Unlock()
	done := make(chan struct{})
	go func() { c.handlers.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close rejects pending calls, drains queued frames for at most one second,
// then releases the transport, including a blocked writer.
func (c *Conn) Close() error {
	var err error
	c.once.Do(func() {
		c.cancel()
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case <-c.writerDone:
		case <-timer.C:
		}
		err = c.transport.close()
	})
	return err
}

// Serve reads until EOF. Malformed and oversized frames don't disconnect the peer.
func (c *Conn) Serve(handler Handler) error {
	return c.ServeOrdered(func(ctx context.Context, method string, params json.RawMessage) (any, *Error) {
		return Deferred(func() (any, *Error) {
			if handler == nil {
				return nil, NewError(-32601, "method_not_found", "unknown method: "+method)
			}
			return handler(ctx, method, params)
		}), nil
	})
}

// ServeOrdered admits requests in read order. Handlers must return promptly;
// return Deferred for work that can block, including writes and outbound calls.
func (c *Conn) ServeOrdered(handler Handler) error {
	readEOF := false
	defer func() {
		if !readEOF || !c.drainOnEOF {
			c.Close()
		}
	}()
	for {
		data, err := c.transport.read(c.ctx)
		var framing *framingError
		if errors.As(err, &framing) {
			// Content-Length framing can't resynchronize after a corrupt header or body.
			if c.ctx.Err() == nil {
				c.rejectProtocol(c.fault("protocol", framing.detail))
			}
			return err
		}
		if errors.Is(err, errOversize) {
			if c.rejectProtocol(c.fault("output_limit", strings.ToUpper(c.protocol)+" frame exceeds response cap; remote outcome may be unknown")) {
				return err
			}
			if err = c.reply(nil, nil, NewError(-32600, "frame_too_large", fmt.Sprintf("frame exceeds %d bytes", c.maxFrame))); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			readEOF = errors.Is(err, io.EOF)
			if errors.Is(err, io.EOF) || c.ctx.Err() != nil {
				return nil
			}
			return err
		}
		data = bytes.TrimSpace(data)
		var m message
		if !utf8.Valid(data) || !json.Valid(data) {
			if c.rejectProtocol(c.fault("protocol", "invalid JSON or UTF-8")) {
				return errors.New("invalid peer frame")
			}
			_ = c.reply(nil, nil, NewError(-32700, "parse_error", "invalid JSON"))
			continue
		}
		if err = json.Unmarshal(data, &m); err != nil || m.JSONRPC != "2.0" || len(data) == 0 || data[0] != '{' || !validID(m.ID) {
			if c.rejectProtocol(c.fault("protocol", "invalid JSON-RPC envelope")) {
				return errors.New("invalid peer frame")
			}
			_ = c.reply(nil, nil, NewError(-32600, "invalid_request", "invalid JSON-RPC request"))
			continue
		}
		if m.Method == "" {
			if len(m.ID) == 0 || (len(m.Result) == 0) == (m.Error == nil) {
				if c.rejectProtocol(c.fault("protocol", "invalid JSON-RPC response")) {
					return errors.New("invalid peer response")
				}
				_ = c.reply(nil, nil, NewError(-32600, "invalid_request", "invalid JSON-RPC response"))
				continue
			}
			id := string(m.ID)
			c.mu.Lock()
			ch := c.pending[id]
			isLate := c.lateIDs[id]
			if isLate {
				delete(c.lateIDs, id)
			}
			if ch != nil {
				select {
				case ch <- m:
				default:
				}
			}
			c.mu.Unlock()
			if ch == nil && !isLate && c.rejectProtocol(c.fault("protocol", "unexpected response ID")) {
				return errors.New("unexpected response ID")
			}
			continue
		}
		if len(m.Result) != 0 || m.Error != nil || (len(m.Params) > 0 && m.Params[0] != '{' && m.Params[0] != '[') {
			if c.rejectProtocol(c.fault("protocol", "invalid JSON-RPC request")) {
				return errors.New("invalid peer request")
			}
			_ = c.reply(m.ID, nil, NewError(-32600, "invalid_request", "invalid JSON-RPC request"))
			continue
		}
		c.mu.Lock()
		if !c.accepting {
			c.mu.Unlock()
			continue
		}
		c.handlers.Add(1)
		c.mu.Unlock()
		requestCtx := context.WithValue(c.ctx, requestIDKey{}, append(json.RawMessage(nil), m.ID...))
		value, rpcErr := handler(requestCtx, m.Method, m.Params)
		go func(m message, value any, rpcErr *Error) {
			defer c.handlers.Done()
			if work, ok := value.(Deferred); ok {
				value, rpcErr = work()
			}
			var after func()
			if response, ok := value.(Response); ok {
				value = response.Value
				after = response.After
			}
			_, suppressed := value.(NoResponse)
			if len(m.ID) > 0 && !suppressed {
				if c.reply(m.ID, value, rpcErr) != nil {
					return
				}
			}
			if after != nil && c.ctx.Err() == nil {
				after()
			}
		}(m, value, rpcErr)
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
	defer close(c.writerDone)
	write := func(frame []byte) bool {
		if c.transport.write(context.Background(), frame) != nil {
			c.cancel()
			_ = c.transport.close()
			return false
		}
		return true
	}
	for {
		select {
		case <-c.ctx.Done():
			for {
				select {
				case frame := <-c.out:
					if !write(frame) {
						return
					}
				default:
					return
				}
			}
		case frame := <-c.out:
			if !write(frame) {
				return
			}
		}
	}
}
func (c *Conn) send(ctx context.Context, frame []byte) error {
	if c.ctx.Err() != nil {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
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
	if len(b) > c.maxFrame {
		m.Result = nil
		m.Error = NewError(-32000, "frame_too_large", "response exceeds frame cap")
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
	if len(b) > c.maxFrame {
		size := len(b)
		p, _ = json.Marshal(map[string]any{"method": method, "bytes": size})
		b, _ = json.Marshal(message{JSONRPC: "2.0", Method: "tempest/frame_dropped", Params: p})
		if len(b) > c.maxFrame {
			return NewError(-32000, "frame_too_large", "notification method exceeds frame cap")
		}
	}
	return c.send(ctx, b)
}

// Call sends a request and waits for its matching response or cancellation.
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	var raw json.RawMessage
	raw, err := c.CallWithID(ctx, fmt.Sprintf("tempest_%d", c.next.Add(1)), method, params)
	if err != nil || result == nil {
		return err
	}
	return json.Unmarshal(raw, result)
}

// CallWithID sends a string-ID request, rejecting duplicate pending IDs.
func (c *Conn) CallWithID(ctx context.Context, id string, method string, params any) (json.RawMessage, error) {
	p, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	if params == nil {
		p = nil
	}
	rawID, _ := json.Marshal(id)
	b, err := json.Marshal(message{JSONRPC: "2.0", ID: rawID, Method: method, Params: p})
	if err != nil {
		return nil, err
	}
	if len(b) > c.maxFrame {
		return nil, fmt.Errorf("%w: %w", ErrNotSent, NewError(-32000, "frame_too_large", "request exceeds frame cap"))
	}
	ch := make(chan message, 1)
	c.mu.Lock()
	if _, exists := c.pending[string(rawID)]; exists {
		c.mu.Unlock()
		return nil, errors.New("request ID already pending")
	}
	c.pending[string(rawID)] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, string(rawID)); c.mu.Unlock() }()
	if err = c.send(ctx, b); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotSent, err)
	}
	var response message
	select {
	case <-ctx.Done():
		c.mu.Lock()
		select {
		case <-ch:
		default:
			if c.lateIDs == nil {
				c.lateIDs = make(map[string]bool)
			}
			c.lateIDs[string(rawID)] = true
		}
		c.mu.Unlock()
		return nil, ctx.Err()
	case <-c.ctx.Done():
		select {
		case response = <-ch:
		default:
			return nil, ErrClosed
		}
	case response = <-ch:
	}
	if response.Error != nil {
		return nil, response.Error
	}
	return response.Result, nil
}
