package extensionhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/taskstate"
	"go.harness.dev/harness/internal/work"
	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/extension"
)

// ResolveTask pins the same selected generation used by tools and hooks.
func (r *Runtime) ResolveTask(name string) (taskstate.Definition, error) {
	lease, err := r.Registry.Acquire(r.Selected)
	if err != nil {
		return taskstate.Definition{}, err
	}
	for _, bundle := range lease.Definitions() {
		for _, definition := range bundle.Tasks {
			if bundle.ID+"/"+definition.Name == name {
				namespaces := make([]string, 0, len(bundle.Documents))
				for _, doc := range bundle.Documents {
					namespaces = append(namespaces, doc.Namespace)
				}
				return taskstate.Definition{Task: definition, Revision: bundle.Revision, Namespaces: namespaces, Bundles: lease.Definitions(), Release: func() { _ = lease.Close(context.Background()) }}, nil
			}
		}
	}
	_ = lease.Close(context.Background())
	return taskstate.Definition{}, fmt.Errorf("task definition unavailable: %s", name)
}

// InvokeTask gives phases revocable callbacks and ordinary nested-tool gates.
func (r *Runtime) InvokeTask(ctx context.Context, scheduler *taskstate.Runtime, definition taskstate.Definition, task extension.Task, runID string, abort bool) (extension.TaskDecision, error) {
	bundleID, _, _ := strings.Cut(task.Definition, "/")
	req := &request{id: "task:" + task.ID, definitions: definition.Bundles, baseTools: r.BaseTools}
	req.tools = append([]agent.AgentTool(nil), r.BaseTools...)
	for _, bundle := range req.definitions {
		for _, tool := range bundle.Tools {
			if r.AllowTool == nil || r.AllowTool(extension.ToolName(bundle.ID, tool.Name), tool.Effect) {
				req.tools = append(req.tools, r.tool(req, bundle, tool))
			}
		}
	}
	for i := range req.tools {
		req.tools[i] = r.wrapTool(req, req.tools[i])
	}
	defer req.closed.Store(true)
	ctx = &requestContext{Context: ctx, request: req}
	ctx = work.WithInvocation(ctx, work.Invocation{RunID: runID, CallID: task.ID + ":" + task.Phase, ToolCallID: task.ID, OwnerTask: task.ID, OwnerRevision: task.Revision})
	scope := durable.MemoScope{SessionID: r.SessionID, InvocationID: task.ID, ExtensionID: bundleID, HookID: "task:" + task.Phase}
	memo := func(ctx context.Context, key string, candidate json.RawMessage) (json.RawMessage, bool, error) {
		if value, present, err := r.Store.ReadMemo(ctx, scope, key); err != nil || present {
			return value, false, err
		}
		id := durable.MemoID(scope, key)
		digest, err := durable.Digest(candidate)
		if err != nil {
			return nil, false, err
		}
		_, err = scheduler.FencedCommit(ctx, task, durable.Commit{ID: "task:memo:" + id + ":" + digest, Expected: []durable.RevisionCheck{{SessionID: r.SessionID, Kind: durable.MemoKind, Namespace: bundleID, ID: id}}, Changes: []durable.Change{{SessionID: r.SessionID, Kind: durable.MemoKind, Namespace: bundleID, ID: id, Version: 1, Value: candidate}}})
		inserted := err == nil
		if err != nil && !errors.Is(err, durable.ErrConflict) {
			return nil, false, err
		}
		value, present, err := r.Store.ReadMemo(ctx, scope, key)
		if err == nil && !present {
			err = errors.New("task memo winner missing")
		}
		return value, inserted, err
	}
	grant := extension.NewGrant(ctx, extension.GrantOptions{
		Identity:   extension.InvocationIdentity{SessionID: r.SessionID, InvocationID: task.ID, Phase: task.Phase, Attempt: task.Attempt, Revision: task.CodeRevision, ExtensionID: bundleID, Handler: "task:" + task.Phase},
		Namespaces: definition.Namespaces, Read: r.Store.Read,
		Commit: func(ctx context.Context, batch durable.Commit) (durable.Receipt, error) {
			return scheduler.FencedCommit(ctx, task, batch)
		},
		Memo: func(ctx context.Context, key string, value json.RawMessage) (json.RawMessage, error) {
			winner, _, err := memo(ctx, key, value)
			return winner, err
		}, MemoInserted: memo,
		ReadMemo: func(ctx context.Context, key string) (json.RawMessage, bool, error) {
			return r.Store.ReadMemo(ctx, scope, key)
		},
		Output: func(ctx context.Context, chunk extension.OutputChunk) error {
			value, err := json.Marshal(chunk)
			if err != nil {
				return err
			}
			id := fmt.Sprintf("task:output:%s:%d:%d", task.ID, task.Attempt, chunk.Number)
			if _, err = scheduler.FencedCommit(ctx, task, durable.Commit{ID: id, Changes: []durable.Change{{SessionID: task.SessionID, Kind: durable.EntryKind, Namespace: "_host.tasks", ID: id, Version: 1, Value: value}}}); err != nil {
				return err
			}
			if r.TaskOutput != nil {
				return r.TaskOutput(ctx, task, chunk)
			}
			return nil
		},
		CallTool: func(ctx context.Context, key, name string, args json.RawMessage) (extension.Result, error) {
			// Each task phase gets stable nested invocation keys; retries reuse receipts.
			if r.CallTool != nil {
				return r.CallTool(ctx, task.ID+":"+task.Phase+":"+key, name, args)
			}
			return r.nested(ctx, task.ID+":"+task.Phase, key, name, args)
		},
		Environment: func(context.Context) (extension.EnvironmentLease, error) {
			return localReadEnvironment{cwd: r.Cwd, identity: r.EnvironmentID}, nil
		},
	})
	defer grant.Revoke()
	if abort {
		if definition.Task.Abort == nil {
			return extension.TaskDecision{Kind: "complete"}, nil
		}
		return definition.Task.Abort(ctx, task, grant)
	}
	return definition.Task.Run(ctx, task, grant)
}
