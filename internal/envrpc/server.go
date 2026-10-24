package envrpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"go.harness.dev/harness/internal/rpc"
	"go.harness.dev/harness/tempest/environment"
)

const (
	maxCallResultBytes  = 1 << 20
	maxImageResultBytes = 7 << 20
)

func hasImageContent(result json.RawMessage) bool {
	var value struct {
		Content []struct {
			Type string `json:"type"`
		} `json:"content"`
	}
	if json.Unmarshal(result, &value) != nil {
		return false
	}
	for _, block := range value.Content {
		if block.Type == "image" {
			return true
		}
	}
	return false
}

type activeCall struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// Serve exposes only explicitly granted workspace services on a dedicated stream.
// The caller owns the lease. Disconnect cancels and joins its live handlers.
func Serve(ctx context.Context, stream io.ReadWriteCloser, lease environment.Lease, granted environment.Capabilities) error {
	conn := rpc.NewStreamWithLimit(stream, maxFrameSize)
	conn.SetProtocolErrorHandler(func(error) { go conn.Close() })
	var mu sync.Mutex
	initialized, ready := false, false
	var caps environment.Capabilities
	activeCalls := make(map[string]*activeCall)
	workers := make(chan struct{}, 32)
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	cancellations := make(chan struct{}, 32)
	defer close(done)
	defer conn.Close()
	return conn.ServeOrdered(func(callCtx context.Context, method string, raw json.RawMessage) (any, *rpc.Error) {
		fail := func(err error) (any, *rpc.Error) { return nil, wireError(err) }
		mu.Lock()
		if method == "initialize" {
			if initialized {
				mu.Unlock()
				return fail(errors.New("environment already initialized"))
			}
			var in initialize
			if err := json.Unmarshal(raw, &in); err != nil {
				mu.Unlock()
				return fail(err)
			}
			if in.ProtocolVersion != ProtocolVersion || in.MaxFrameSize != maxFrameSize {
				mu.Unlock()
				return fail(errors.New("environment protocol version or frame limit mismatch"))
			}
			if err := environment.Match(in.Environment, lease.Reference()); err != nil {
				mu.Unlock()
				return fail(err)
			}
			for _, c := range in.Capabilities {
				if granted.Has(c) && lease.Capabilities().Has(c) && !caps.Has(c) {
					caps = append(caps, c)
				}
			}
			initialized = true
			out := initialize{ProtocolVersion: ProtocolVersion, MaxFrameSize: maxFrameSize, Environment: lease.Reference(), Capabilities: caps}
			mu.Unlock()
			return out, nil
		}
		if method == "notifications/initialized" && initialized {
			ready = true
			mu.Unlock()
			return nil, nil
		}
		allowed := ready
		negotiated := caps
		mu.Unlock()
		if !allowed {
			return fail(errors.New("environment handshake required"))
		}
		switch method {
		case prefix + "call":
			var c environment.Call
			if err := json.Unmarshal(raw, &c); err != nil {
				return fail(err)
			}
			if err := environment.Match(lease.Reference(), c.Environment); err != nil {
				return fail(err)
			}
			cap := environment.ToolCapability(c.Operation)
			if cap == "" || !negotiated.Has(cap) {
				return fail(environment.ErrUnsupported)
			}
			if c.Key == "" {
				return fail(errors.New("environment invocation key required"))
			}
			select {
			case workers <- struct{}{}:
			default:
				return nil, rpc.NewError(-32000, "busy", "environment worker limit reached")
			}
			invocationCtx, cancelCall := context.WithCancel(callCtx)
			active := &activeCall{cancel: cancelCall, done: make(chan struct{})}
			mu.Lock()
			if _, exists := activeCalls[c.Key]; exists {
				mu.Unlock()
				cancelCall()
				<-workers
				return fail(errors.New("environment invocation already running"))
			}
			activeCalls[c.Key] = active
			mu.Unlock()
			return rpc.Deferred(func() (any, *rpc.Error) {
				defer func() {
					cancelCall()
					mu.Lock()
					close(active.done)
					if activeCalls[c.Key] == active {
						delete(activeCalls, c.Key)
					}
					mu.Unlock()
					<-workers
				}()
				out, err := lease.Call(invocationCtx, c)
				if err != nil {
					return fail(err)
				}
				limit := maxCallResultBytes
				if c.Operation == "view_image" || c.Operation == "read" && hasImageContent(out) {
					limit = maxImageResultBytes
				}
				if len(out) > limit {
					return fail(environment.ErrUnknown)
				}
				return out, nil
			}), nil
		case prefix + "cancel", prefix + "lookup":
			var in keyRequest
			if err := json.Unmarshal(raw, &in); err != nil {
				return fail(err)
			}
			if err := environment.Match(lease.Reference(), in.Environment); err != nil {
				return fail(err)
			}
			if in.Key == "" {
				return fail(errors.New("environment invocation key required"))
			}
			cap := environment.Cancel
			if method == prefix+"lookup" {
				cap = environment.Lookup
			}
			if !negotiated.Has(cap) {
				return fail(environment.ErrUnsupported)
			}
			admission := workers
			if cap == environment.Cancel {
				admission = cancellations
			}
			select {
			case admission <- struct{}{}:
			default:
				return nil, rpc.NewError(-32000, "busy", "environment worker limit reached")
			}
			return rpc.Deferred(func() (any, *rpc.Error) {
				defer func() { <-admission }()
				if cap == environment.Cancel {
					mu.Lock()
					active := activeCalls[in.Key]
					mu.Unlock()
					if active != nil {
						active.cancel()
						select {
						case <-active.done:
							return environment.Cancellation{Requested: true, Confirmed: true}, nil
						case <-callCtx.Done():
							return fail(callCtx.Err())
						}
					}
					out, err := lease.Cancel(callCtx, in.Key)
					if err != nil {
						return fail(err)
					}
					return out, nil
				}
				out, err := lease.Lookup(callCtx, in.Key)
				if err != nil {
					return fail(err)
				}
				return out, nil
			}), nil
		default:
			return nil, rpc.NewError(-32601, "unsupported", "unknown environment method")
		}
	})
}
