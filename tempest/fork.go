package tempest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/tempest/environment"
)

// ForkOptions selects an explicit workspace policy and safe transcript cut.
// Forking shares the workspace unless Cwd or Environment selects another one.
type ForkOptions struct {
	PrecedingSafe bool                   `json:"precedingSafe,omitempty"`
	Cwd           string                 `json:"cwd,omitempty"`
	Environment   *environment.Reference `json:"environment,omitempty"`
}

// Fork creates an ownerless sibling with a pinned transcript and a fresh ledger.
func (host *Host) Fork(ctx context.Context, parentID, entryID string, options ForkOptions) (*Session, error) {
	parent, err := host.Open(ctx, parentID)
	if err != nil {
		return nil, err
	}
	if err := parent.authorize(ctx, "fork", "", entryID); err != nil {
		return nil, err
	}
	seed, err := parent.core.store.CaptureFork(ctx, entryID, options.PrecedingSafe)
	if err != nil {
		return nil, err
	}
	choice, err := decodeAgentSelection(seed.Agent)
	if err != nil {
		return nil, err
	}
	// Runtime implementations come from the host. Stored choices come from the cut.
	configured := host.sessionOptions()
	configured.Environment = choice.Environment
	if options.Environment != nil {
		configured.Environment = *options.Environment
	}
	if options.Cwd != "" {
		if options.Environment != nil {
			return nil, fmt.Errorf("choose cwd or environment, not both")
		}
		if !strings.HasPrefix(choice.Environment.ID, "local:") {
			return nil, fmt.Errorf("remote forks need an explicit environment reference, not a local cwd")
		}
		configured.Environment = environment.Reference{}
	}
	var stack StackConfig
	if configured.Stack != nil {
		stack = *configured.Stack
	}
	stack.Environment = nil
	stack.EnvironmentReference = configured.Environment
	if configured.EnvironmentResolver == nil {
		configured.EnvironmentResolver = stack.EnvironmentResolver
	}
	stack.Plan = choice.Plan
	stack.Reasoning = choice.Reasoning
	stack.AllowedTools = choice.AllowedTools
	stack.ExcludeTools = choice.ExcludeTools
	stack.PromptAdditions = choice.Instructions
	stack.CustomPrompt = choice.CustomPrompt
	cwd := seed.Cwd
	if options.Cwd != "" {
		cwd = options.Cwd
	}
	return host.createFork(ctx, seed, configured, stack, choice, cwd)
}

func (host *Host) createFork(ctx context.Context, seed *journal.ForkSeed, configured Options, stack StackConfig, choice agentSelection, cwd string) (*Session, error) {
	host.mu.Lock()
	if host.closed {
		host.mu.Unlock()
		return nil, ErrClosed
	}
	host.openers.Add(1)
	host.mu.Unlock()
	defer host.openers.Done()
	ctx, cancel := host.operationContext(ctx)
	defer cancel()
	configured.Stack = &stack
	configured.Cwd = cwd
	endpoints := make([]MCPServerConfig, 0, len(choice.MCPNames))
	available := make(map[string]MCPServerConfig)
	for _, endpoint := range host.options.MCPServers {
		available[endpoint.Name] = endpoint
	}
	for _, name := range choice.MCPNames {
		endpoint, ok := available[name]
		if !ok {
			return nil, fmt.Errorf("required fork MCP endpoint %q unavailable", name)
		}
		endpoints = append(endpoints, endpoint)
	}
	configured.New = &NewSession{Model: choice.Model, PermissionMode: choice.PermissionMode, MCPServers: endpoints}
	configured.Resume = nil
	configured.Extensions = choice.Extensions
	configured.Documents = seed.Definitions
	configured.forkSeed = seed
	configured.hostCreate = true
	created, err := Open(ctx, configured)
	if err != nil {
		return nil, err
	}
	catalog, owned, err := host.openCatalog(ctx)
	if err == nil {
		path := ""
		if host.options.Store == nil {
			path = created.core.store.Path()
		}
		now := time.Now()
		err = publishSession(ctx, catalog, catalogSession{SessionID: created.ID(), Cwd: created.Snapshot().Cwd, Path: path, CreatedAt: now, UpdatedAt: now})
		if owned {
			err = errors.Join(err, catalog.Close(context.Background()))
		}
	}
	host.mu.Lock()
	closed := host.closed || host.ctx.Err() != nil
	if err == nil && !closed {
		host.sessions[created.ID()] = created
	}
	host.mu.Unlock()
	if closed && err == nil {
		err = ErrClosed
	}
	if err != nil {
		return nil, fmt.Errorf("publish fork: %w", errors.Join(err, created.Close(context.Background())))
	}
	return created, nil
}
