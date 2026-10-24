package session

import (
	"context"
	"fmt"
	"maps"
	"sync"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/hooks"
	"go.harness.dev/harness/internal/session/journal"
)

type hookCache struct {
	mu    sync.Mutex
	store *journal.Store
	agent *agent.Agent
	calls map[string]journal.HookState
}

func newHookCache(store *journal.Store, loaded *journal.Loaded, agt *agent.Agent) *hookCache {
	calls := map[string]journal.HookState{}
	if loaded != nil {
		maps.Copy(calls, loaded.HookRuns)
	}
	return &hookCache{store: store, agent: agt, calls: calls}
}

func (c *hookCache) contains(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.calls[key]
	return ok
}

func (c *hookCache) run(ctx context.Context, key, event string, call func(context.Context) (hooks.Result, error)) (hooks.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if prior, ok := c.calls[key]; ok {
		if !prior.Finished {
			if event != "UserPromptSubmit" {
				return hooks.Result{}, fmt.Errorf("%s hook outcome_unknown; interrupted shell commands aren't retried", event)
			}
			return c.finish(ctx, key, event, hooks.Result{Decision: "deny", Interrupted: true, Reason: "UserPromptSubmit hook outcome unknown; prompt not submitted"})
		}
		return prior.Result, nil
	}
	meta := journal.Meta{}
	if c.agent != nil {
		meta.RunID = c.agent.RunState().RunID
	}
	started := false
	callCtx := hooks.WithCommandStart(ctx, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if started {
			return nil
		}
		if c.store != nil {
			if err := c.store.Append(context.WithoutCancel(ctx), journal.HookRecord{Meta: meta, Event: event, Key: key, Status: "started"}); err != nil {
				return err
			}
		}
		started = true
		c.calls[key] = journal.HookState{}
		return nil
	})
	result, err := call(callCtx)
	if !started {
		return result, err
	}
	if err != nil {
		if event != "UserPromptSubmit" {
			return result, err
		}
		result = hooks.Result{Decision: "deny", Interrupted: true, Reason: "UserPromptSubmit hook interrupted; prompt not submitted"}
	}
	return c.finish(ctx, key, event, result)
}

func (c *hookCache) finish(ctx context.Context, key, event string, result hooks.Result) (hooks.Result, error) {
	meta := journal.Meta{}
	if c.agent != nil {
		meta.RunID = c.agent.RunState().RunID
	}
	if c.store != nil {
		if err := c.store.Append(context.WithoutCancel(ctx), journal.HookRecord{Meta: meta, Event: event, Key: key, Status: "finished", Result: &result}); err != nil {
			return result, err
		}
	}
	c.calls[key] = journal.HookState{Finished: true, Result: result}
	return result, nil
}
