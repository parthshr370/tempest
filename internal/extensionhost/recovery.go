package extensionhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/work"
	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/extension"
)

type logicalKey struct{}
type depthKey struct{}
type attemptKey struct{}

// Recovery reports honest terminal unknowns separately from parked missing code.
type Recovery struct {
	CallID string           `json:"callId"`
	State  string           `json:"state"`
	Reason string           `json:"reason,omitempty"`
	Result extension.Result `json:"result,omitempty"`
}
type toolIntent struct {
	Kind       string               `json:"kind"`
	CallID     string               `json:"callId"`
	Name       string               `json:"name"`
	Definition agent.ToolDefinition `json:"definition"`
	Arguments  json.RawMessage      `json:"arguments"`
}
type nestedCall struct {
	done   chan struct{}
	result extension.Result
	err    error
}

func (r *Runtime) readFact(ctx context.Context, id string) (evidence, bool, error) {
	page, err := r.Store.Read(ctx, durable.Query{SessionID: r.SessionID, Kind: durable.EntryKind, Namespace: "_host.extensions", ID: id, Limit: 1})
	if err != nil {
		return evidence{}, false, err
	}
	if len(page.Records) == 0 {
		return evidence{}, false, nil
	}
	var fact evidence
	if err = json.Unmarshal(page.Records[0].Value, &fact); err != nil {
		return evidence{}, false, err
	}
	return fact, true, nil
}

// ResumePending is explicit scheduling. Reads and reduction never call it.
func (r *Runtime) ResumePending(ctx context.Context) ([]Recovery, error) {
	r.resumeMu.Lock()
	defer r.resumeMu.Unlock()
	var records []durable.Record
	q := durable.Query{SessionID: r.SessionID, Kind: durable.EntryKind, Namespace: "_host.extensions", Limit: 256}
	for {
		page, err := r.Store.Read(ctx, q)
		if err != nil {
			return nil, err
		}
		records = append(records, page.Records...)
		if page.NextCursor == "" {
			break
		}
		q.Cursor = page.NextCursor
	}
	intents := map[string]toolIntent{}
	finished := map[string]bool{}
	attempts := map[string]uint64{}
	for _, record := range records {
		var header struct {
			Kind    string `json:"kind"`
			CallID  string `json:"callId"`
			Attempt uint64 `json:"attempt"`
		}
		if err := json.Unmarshal(record.Value, &header); err != nil {
			return nil, err
		}
		switch header.Kind {
		case "tool_intent":
			var intent toolIntent
			if err := json.Unmarshal(record.Value, &intent); err != nil {
				return nil, err
			}
			intents[intent.CallID] = intent
		case "tool_result", "tool_aborted":
			finished[header.CallID] = true
		case "tool_attempt":
			if header.Attempt > attempts[header.CallID] {
				attempts[header.CallID] = header.Attempt
			}
		}
	}
	var ids []string
	for id := range intents {
		if !finished[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	lease, err := r.Registry.Acquire(r.Selected)
	if err != nil {
		return nil, err
	}
	defer lease.Close(context.Background())
	req := &request{lease: lease, definitions: lease.Definitions(), id: "recovery"}
	req.tools = append([]agent.AgentTool(nil), r.BaseTools...)
	for _, d := range req.definitions {
		if len(d.Documents) > 0 {
			registrar, ok := r.Store.(interface {
				RegisterDocuments(context.Context, []durable.DocumentDefinition) error
			})
			if !ok {
				return nil, errors.New("store cannot validate extension documents")
			}
			if err := registrar.RegisterDocuments(ctx, d.Documents); err != nil {
				return nil, err
			}
		}
		for _, t := range d.Tools {
			if r.AllowTool == nil || r.AllowTool(extension.ToolName(d.ID, t.Name), t.Effect) {
				req.tools = append(req.tools, r.tool(req, d, t))
			}
		}
	}
	for i := range req.tools {
		req.tools[i] = r.wrapTool(req, req.tools[i])
	}
	ctx = context.WithValue(ctx, requestKey{}, req)
	var results []Recovery
	for _, id := range ids {
		intent := intents[id]
		out := Recovery{CallID: id, State: "parked"}
		var current *agent.AgentTool
		for i := range req.tools {
			if req.tools[i].Definition.Identity == intent.Definition.Identity {
				current = &req.tools[i]
				break
			}
		}
		if intent.Definition.Replay != durable.ReplaySafe {
			out.State = "unknown"
			out.Reason = "interrupted Never call is not replayed"
			results = append(results, out)
			continue
		}
		if current == nil || !extension.CanReplay(intent.Definition.Replay, current.Definition.Replay, intent.Definition.Revision, current.Definition.Revision, intent.Definition.Environment, current.Definition.Environment) || intent.Definition.SchemaHash != current.Definition.SchemaHash {
			out.Reason = "matching replay-safe definition/environment unavailable"
			results = append(results, out)
			continue
		}
		if r.ExecuteTool == nil {
			return nil, errors.New("extension recovery executor unavailable")
		}
		attempt := attempts[id] + 1
		if attempt < 2 {
			attempt = 2
		}
		if err = r.fact(ctx, fmt.Sprintf("%s:attempt:%d", id, attempt), evidence{Kind: "tool_attempt", CallID: id, Attempt: attempt}); err != nil {
			return nil, err
		}
		callCtx := context.WithValue(ctx, logicalKey{}, id)
		callCtx = context.WithValue(callCtx, attemptKey{}, attempt)
		occurrence, _ := work.InvocationFromContext(callCtx)
		occurrence.CallID, occurrence.ToolCallID = id, id
		callCtx = work.WithInvocation(callCtx, occurrence)
		result, executeErr := r.ExecuteTool(callCtx, req.tools, types.NewToolCall(id, intent.Name, intent.Arguments))
		out.Result = fromAgent(result)
		if executeErr != nil {
			out.State = "failed"
			out.Reason = executeErr.Error()
		} else {
			out.State = "completed"
		}
		results = append(results, out)
	}
	return results, nil
}
func (r *Runtime) nested(ctx context.Context, parent, key, name string, args json.RawMessage) (extension.Result, error) {
	depth, _ := ctx.Value(depthKey{}).(int)
	if depth >= 8 {
		return extension.Result{}, errors.New("nested tool depth exceeded")
	}
	req, err := requestFrom(ctx)
	if err != nil {
		return extension.Result{}, err
	}
	if r.ExecuteTool == nil {
		return extension.Result{}, durable.ErrUnsupported
	}
	id := parent + ":child:" + hash([]byte(key))
	fingerprint, err := durable.Digest(struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}{Name: name, Arguments: args})
	if err != nil {
		return extension.Result{}, err
	}
	r.nestedMu.Lock()
	if r.nestedCalls == nil {
		r.nestedCalls = map[string]*nestedCall{}
	}
	pending := r.nestedCalls[id]
	if pending != nil {
		r.nestedMu.Unlock()
		select {
		case <-ctx.Done():
			return extension.Result{}, ctx.Err()
		case <-pending.done:
		}
		admitted, _, readErr := r.readFact(ctx, id+":admission")
		if readErr != nil {
			return extension.Result{}, readErr
		}
		if admitted.Hash != fingerprint {
			return extension.Result{}, durable.ErrConflict
		}
		return pending.result, pending.err
	}
	pending = &nestedCall{done: make(chan struct{})}
	r.nestedCalls[id] = pending
	r.nestedMu.Unlock()
	defer func() { close(pending.done); r.nestedMu.Lock(); delete(r.nestedCalls, id); r.nestedMu.Unlock() }()
	admission, present, err := r.readFact(ctx, id+":admission")
	if err != nil {
		pending.err = err
		return pending.result, err
	}
	if present && admission.Hash != fingerprint {
		pending.err = durable.ErrConflict
		return pending.result, pending.err
	}
	if prior, found, err := r.readFact(ctx, id+":result"); err != nil {
		pending.err = err
		return pending.result, err
	} else if found && prior.Result != nil {
		pending.result = *prior.Result
		return pending.result, nil
	}
	if present {
		pending.err = errors.New("nested call interrupted; explicit resume required")
		return pending.result, pending.err
	}
	if err = r.fact(ctx, id+":admission", evidence{Kind: "nested_admission", Hash: fingerprint}); err != nil {
		pending.err = err
		return pending.result, err
	}
	callCtx := context.WithValue(ctx, logicalKey{}, id)
	callCtx = context.WithValue(callCtx, depthKey{}, depth+1)
	occurrence, _ := work.InvocationFromContext(callCtx)
	occurrence.CallID, occurrence.ToolCallID = id, id
	callCtx = work.WithInvocation(callCtx, occurrence)
	result, err := r.ExecuteTool(callCtx, req.tools, types.NewToolCall(id, name, args))
	pending.result, pending.err = fromAgent(result), err
	return pending.result, err
}

// ResolvePresetContext uses the provider frame's generation rather than latest code.
func (r *Runtime) ResolvePresetContext(ctx context.Context, id string) (extension.AgentPreset, error) {
	req, err := requestFrom(ctx)
	if err != nil {
		return extension.AgentPreset{}, err
	}
	for _, d := range req.definitions {
		for _, p := range d.Presets {
			if d.ID+"/"+p.ID == id {
				return p, nil
			}
		}
	}
	return extension.AgentPreset{}, fmt.Errorf("agent preset unavailable: %s", id)
}
