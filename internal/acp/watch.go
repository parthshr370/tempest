package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"

	"go.harness.dev/harness/internal/rpc"
	"go.harness.dev/harness/tempest"
)

func (c *Client) unwatch(id string) {
	c.watchMu.Lock()
	defer c.watchMu.Unlock()
	c.stopWatch(id)
}

func (c *Client) stopWatch(id string) {
	c.mu.Lock()
	w := c.watches[id]
	delete(c.watches, id)
	c.mu.Unlock()
	if w != nil {
		w.stop()
	}
}

func (c *Client) startWatch(ctx context.Context, s *tempest.Session, approver bool, buffer int, tail *int, replay bool) (tempest.Snapshot, func(), *rpc.Error) {
	c.watchMu.Lock()
	defer c.watchMu.Unlock()
	c.stopWatch(s.ID())
	_, release, err := c.backend.Acquire(s.ID())
	if err != nil {
		return tempest.Snapshot{}, nil, failure(err)
	}
	watchCtx, cancel := context.WithCancel(ctx)
	snapshot, updates, stopHub := s.Watch(watchCtx, tempest.WatchOptions{Buffer: buffer, Approver: approver, Replay: replay})
	removeApprover := func() {}
	if approver {
		removeApprover = c.broker.add(s.ID(), c.conn)
	}
	var once sync.Once
	w := &watch{seq: snapshot.Seq}
	w.cond = sync.NewCond(&w.mu)
	w.stop = func() {
		once.Do(func() {
			cancel()
			stopHub()
			removeApprover()
			release()
			w.mu.Lock()
			w.stopped = true
			w.cond.Broadcast()
			w.mu.Unlock()
		})
	}
	if tail != nil {
		offset := max(0, snapshot.HistoryLen-*tail)
		history, e := s.History(snapshot.HistoryEpoch, offset, snapshot.HistoryLen-offset)
		if e != nil {
			w.stop()
			return tempest.Snapshot{}, nil, failure(e)
		}
		snapshot.History = history
	}
	if !replay {
		encoded, e := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "snapshot", "result": map[string]any{"snapshot": snapshot}})
		if e != nil || len(encoded) > rpc.MaxFrame-128 {
			w.stop()
			return tempest.Snapshot{}, nil, rpc.NewError(-32000, "frame_too_large", "snapshot exceeds 1 MiB; request a smaller historyTail")
		}
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		w.stop()
		return tempest.Snapshot{}, nil, rpc.NewError(-32603, "internal_error", "connection closed")
	}
	c.watches[s.ID()] = w
	c.mu.Unlock()
	if replay {
		for _, update := range snapshot.History {
			if err := c.sendUpdate(watchCtx, update); err != nil {
				c.stopWatch(s.ID())
				return tempest.Snapshot{}, nil, failure(err)
			}
		}
	}
	after := func() {
		go func() {
			defer w.stop()
			defer func() {
				c.mu.Lock()
				if c.watches[s.ID()] == w {
					delete(c.watches, s.ID())
				}
				c.mu.Unlock()
			}()
			for {
				select {
				case <-watchCtx.Done():
					return
				case u, ok := <-updates:
					if !ok {
						return
					}
					if err := c.sendUpdate(watchCtx, u); err != nil {
						return
					}
					w.mu.Lock()
					w.seq = u.Seq
					w.cond.Broadcast()
					w.mu.Unlock()
					if u.Kind == "tempest/gap" {
						return
					}
				}
			}
		}()
	}
	return snapshot, after, nil
}

func (c *Client) flushWatch(ctx context.Context, id string, seq uint64) {
	c.mu.Lock()
	w := c.watches[id]
	c.mu.Unlock()
	if w == nil {
		return
	}
	stop := context.AfterFunc(ctx, func() { w.mu.Lock(); w.cond.Broadcast(); w.mu.Unlock() })
	defer stop()
	w.mu.Lock()
	defer w.mu.Unlock()
	for w.seq < seq && !w.stopped && ctx.Err() == nil {
		w.cond.Wait()
	}
}

func (c *Client) sendUpdate(ctx context.Context, u tempest.Update) error {
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(u.Update))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return err
	}
	if strings.HasPrefix(u.Kind, "tempest/") {
		payload["sessionId"] = u.SessionID
		payload["seq"] = u.Seq
		if u.Kind == "tempest/gap" {
			if _, ok := payload["lastSeq"]; !ok {
				payload["lastSeq"] = u.Seq
			}
		}
		method := u.Kind
		if method == "tempest/durable" {
			method = "_tempest/durable/update"
		}
		return c.conn.Notify(ctx, method, payload)
	}
	if u.Kind == "notice" {
		c.mu.Lock()
		notices := c.notices
		c.mu.Unlock()
		if !notices {
			return nil
		}
	}
	meta, ok := payload["_meta"].(map[string]any)
	if !ok {
		meta = make(map[string]any)
		payload["_meta"] = meta
	}
	extension, ok := meta["tempest"].(map[string]any)
	if !ok {
		extension = make(map[string]any)
		meta["tempest"] = extension
	}
	extension["seq"] = u.Seq
	return c.conn.Notify(ctx, "session/update", map[string]any{"sessionId": u.SessionID, "update": payload})
}
