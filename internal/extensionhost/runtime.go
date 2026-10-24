// Package extensionhost binds extension handlers to the agent's durable and permission boundaries.
package extensionhost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/outputschema"
	"go.harness.dev/harness/internal/subagent"
	"go.harness.dev/harness/internal/work"
	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/environment"
	"go.harness.dev/harness/tempest/extension"
)

// Store is the session-owned durable capability, not an extension-owned writer.
type Store interface {
	Read(context.Context, durable.Query) (durable.Page, error)
	Commit(context.Context, durable.Commit) (durable.Receipt, error)
	Memo(context.Context, durable.MemoScope, string, json.RawMessage) (json.RawMessage, error)
	ReadMemo(context.Context, durable.MemoScope, string) (json.RawMessage, bool, error)
}

// Runtime resolves selected operator bundles for each provider request.
type Runtime struct {
	Registry      *extension.Registry
	Selected      []extension.Selection
	Store         Store
	SessionID     string
	Cwd           string
	BasePrompt    string
	BaseTools     []agent.AgentTool
	EnvironmentID string
	Environment   environment.Lease
	Counter       atomic.Uint64
	CommandHooks  bool
	CallTool      func(context.Context, string, string, json.RawMessage) (extension.Result, error)
	AllowTool     func(string, extension.EffectClass) bool
	ExecuteTool   func(context.Context, []agent.AgentTool, types.ContentBlock) (agent.AgentToolResult, error)
	TaskOutput    func(context.Context, extension.Task, extension.OutputChunk) error
	resumeMu      sync.Mutex
	nestedMu      sync.Mutex
	nestedCalls   map[string]*nestedCall
}
type requestKey struct{}
type requestContext struct {
	context.Context
	request *request
}

func (ctx *requestContext) Value(key any) any {
	if _, ok := key.(requestKey); ok {
		return ctx.request
	}
	return ctx.Context.Value(key)
}

type request struct {
	lease       *extension.Lease
	definitions []extension.Definition
	id          string
	tools       []agent.AgentTool
	baseTools   []agent.AgentTool
	closed      atomic.Bool
}

func hash(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func (r *Runtime) fact(ctx context.Context, id string, value evidence) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = r.Store.Commit(ctx, durable.Commit{ID: "extension:" + id, Changes: []durable.Change{{SessionID: r.SessionID, Kind: durable.EntryKind, Namespace: "_host.extensions", ID: id, Version: 1, Value: raw}}})
	return err
}
func (r *Runtime) grant(ctx context.Context, req *request, d extension.Definition, handler, invocation string, attempt uint64, output func(extension.OutputChunk) error) *extension.Grant {
	if current, ok := ctx.Value(attemptKey{}).(uint64); ok {
		attempt = current
	}
	scope := durable.MemoScope{SessionID: r.SessionID, InvocationID: invocation, ExtensionID: d.ID, HookID: handler}
	var namespaces []string
	for _, doc := range d.Documents {
		namespaces = append(namespaces, doc.Namespace)
	}
	return extension.NewGrant(ctx, extension.GrantOptions{Identity: extension.InvocationIdentity{SessionID: r.SessionID, InvocationID: invocation, ExtensionID: d.ID, Revision: d.Revision, Handler: handler, Attempt: attempt}, Namespaces: namespaces, Read: r.Store.Read, Commit: r.Store.Commit, Memo: func(ctx context.Context, key string, value json.RawMessage) (json.RawMessage, error) {
		return r.Store.Memo(ctx, scope, key, value)
	}, MemoInserted: func(ctx context.Context, key string, value json.RawMessage) (json.RawMessage, bool, error) {
		store, ok := r.Store.(interface {
			MemoInserted(context.Context, durable.MemoScope, string, json.RawMessage) (json.RawMessage, bool, error)
		})
		if !ok {
			return nil, false, durable.ErrUnsupported
		}
		return store.MemoInserted(ctx, scope, key, value)
	}, ReadMemo: func(ctx context.Context, key string) (json.RawMessage, bool, error) {
		return r.Store.ReadMemo(ctx, scope, key)
	}, Output: func(ctx context.Context, ch extension.OutputChunk) error {
		if output == nil {
			return durable.ErrUnsupported
		}
		return output(ch)
	}, Environment: func(context.Context) (extension.EnvironmentLease, error) {
		return r.readEnvironment()
	}, CallTool: func(ctx context.Context, key, name string, args json.RawMessage) (extension.Result, error) {
		if r.CallTool != nil {
			return r.CallTool(ctx, key, name, args)
		}
		return r.nested(ctx, invocation, key, name, args)
	}})
}

type readView struct{ extension.ReadView }

// Prepare records the exact rendered frame before a provider can observe it.
func (r *Runtime) Prepare(ctx context.Context, current agent.AgentContext) (context.Context, agent.AgentContext, func(), error) {
	lease, err := r.Registry.Acquire(r.Selected)
	if err != nil {
		return ctx, current, nil, err
	}
	req := &request{lease: lease, definitions: lease.Definitions()}
	close := func() {
		if req.closed.CompareAndSwap(false, true) {
			_ = lease.Close(context.Background())
		}
	}
	fail := func(err error) (context.Context, agent.AgentContext, func(), error) {
		close()
		return ctx, current, nil, err
	}
	page, err := r.Store.Read(ctx, durable.Query{SessionID: r.SessionID, Limit: 1})
	if err != nil {
		return fail(err)
	}
	req.id = fmt.Sprintf("request:%d:%d", page.Seq, r.Counter.Add(1))
	current.SystemPrompt = r.BasePrompt
	var revisions []definitionRevision
	for _, d := range req.definitions {
		revisions = append(revisions, definitionRevision{ID: d.ID, Revision: d.Revision})
		if len(d.Documents) > 0 {
			registrar, ok := r.Store.(interface {
				RegisterDocuments(context.Context, []durable.DocumentDefinition) error
			})
			if !ok {
				return fail(errors.New("store cannot validate extension documents"))
			}
			if err := registrar.RegisterDocuments(ctx, d.Documents); err != nil {
				return fail(err)
			}
		}
		for _, s := range d.Sections {
			inv := r.grant(ctx, req, d, "section:"+s.Name, req.id, 1, nil)
			text, renderErr := func() (string, error) { defer inv.Revoke(); return s.Render(ctx, readView{inv}) }()
			if renderErr != nil {
				return fail(fmt.Errorf("section %s/%s: %w", d.ID, s.Name, renderErr))
			}
			if len(text) > 256<<10 {
				return fail(errors.New("section exceeds output limit"))
			}
			if text != "" {
				current.SystemPrompt += "\n\n" + text
			}
		}
	}
	req.baseTools = r.BaseTools
	if previous, ok := ctx.Value(requestKey{}).(*request); ok {
		req.baseTools = previous.baseTools
	} else if len(current.Tools) > 0 {
		// A run can install a refreshed MCP catalog before preparing its frame.
		req.baseTools = make([]agent.AgentTool, 0, len(r.BaseTools)+len(current.Tools))
		for _, tool := range r.BaseTools {
			if !strings.HasPrefix(tool.Name, "mcp__") {
				req.baseTools = append(req.baseTools, tool)
			}
		}
		for _, tool := range current.Tools {
			if strings.HasPrefix(tool.Name, "mcp__") {
				req.baseTools = append(req.baseTools, tool)
			}
		}
	}
	current.Tools = append([]agent.AgentTool(nil), req.baseTools...)
	presets := map[string]extension.AgentPreset{}
	var presetNames []string
	for _, d := range req.definitions {
		for _, preset := range d.Presets {
			id := d.ID + "/" + preset.ID
			presets[id] = preset
			presetNames = append(presetNames, id)
		}
	}
	for i := range current.Tools {
		if current.Tools[i].Name == "task" {
			current.Tools[i].Parameters = subagent.PresetSchema(presetNames)
			current.SystemPrompt += "\n\n" + subagent.PresetListing(presets)
		}
	}
	for _, d := range req.definitions {
		for _, t := range d.Tools {
			if r.AllowTool != nil && !r.AllowTool(extension.ToolName(d.ID, t.Name), t.Effect) {
				continue
			}
			current.Tools = append(current.Tools, r.tool(req, d, t))
		}
	}
	for i := range current.Tools {
		current.Tools[i] = r.wrapTool(req, current.Tools[i])
	}
	req.tools = current.Tools
	if previous, ok := ctx.(*requestContext); ok {
		ctx = previous.Context
	}
	ctx = &requestContext{Context: ctx, request: req}
	input, _, err := r.hooks(ctx, req, extension.HookInput{Event: extension.BeforeRequest, System: current.SystemPrompt}, req.id)
	if err != nil {
		return fail(err)
	}
	current.SystemPrompt = input.System
	schemas := make([]types.Tool, 0, len(current.Tools))
	for _, t := range current.Tools {
		schemas = append(schemas, t.Tool)
	}
	raw, _ := json.Marshal(schemas)
	if len(current.SystemPrompt) > 512<<10 {
		return fail(errors.New("request system frame exceeds bound"))
	}
	err = r.fact(ctx, req.id, evidence{Kind: "request_frame", System: current.SystemPrompt, ToolSchemaDigest: hash(raw), Tools: schemas, Definitions: revisions, Environment: r.EnvironmentID})
	if err != nil {
		return fail(err)
	}
	return ctx, current, close, nil
}
func (r *Runtime) tool(req *request, d extension.Definition, t extension.Tool) agent.AgentTool {
	return agent.AgentTool{Tool: types.Tool{Name: extension.ToolName(d.ID, t.Name), Description: t.Description, Parameters: append(json.RawMessage(nil), t.InputSchema...)}, Definition: agent.ToolDefinition{Identity: extension.ToolIdentity(d.ID, t.Name), Revision: d.Revision, Replay: t.Replay, Effect: string(t.Effect), Environment: r.EnvironmentID, SchemaHash: hash(t.InputSchema)}, ExecutionMode: agent.ExecSequential, Execute: func(ctx context.Context, id string, args json.RawMessage, onUpdate agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
		logical := r.callID(ctx, req, id)
		inv := r.grant(ctx, req, d, "tool:"+t.Name, logical, 1, func(ch extension.OutputChunk) error {
			if onUpdate != nil {
				onUpdate(toAgent(extension.Result{Content: ch.Content}))
			}
			return nil
		})
		defer inv.Revoke()
		result, err := t.Execute(ctx, extension.Call{ID: logical, Name: t.Name, Arguments: append(json.RawMessage(nil), args...)}, inv)
		if err != nil {
			return toAgent(result), err
		}
		if err = validateOutput(t, result); err != nil {
			return agent.AgentToolResult{}, err
		}
		return toAgent(result), nil
	}}
}
func (r *Runtime) callID(ctx context.Context, req *request, id string) string {
	if logical, ok := ctx.Value(logicalKey{}).(string); ok && logical != "" {
		return logical
	}
	if invocation, ok := work.InvocationFromContext(ctx); ok && invocation.CallID != "" {
		return invocation.CallID
	}
	return req.id + ":" + id
}
func validateOutput(t extension.Tool, result extension.Result) error {
	schema, err := outputschema.Compile(t.OutputSchema)
	if err != nil {
		return err
	}
	if schema != nil && !result.IsError {
		if _, err = schema.Validate(string(result.StructuredContent)); err != nil {
			return fmt.Errorf("structured tool output: %w", err)
		}
	}
	for _, block := range result.Content {
		switch block.Type {
		case "text", "image", "documentRef", "imageRef":
		default:
			return fmt.Errorf("unsupported model content type %q", block.Type)
		}
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if len(raw) > 1<<20 {
		return errors.New("tool result exceeds bound")
	}
	return nil
}
func toAgent(result extension.Result) agent.AgentToolResult {
	content := make([]types.ContentBlock, 0, len(result.Content))
	for _, c := range result.Content {
		content = append(content, types.ContentBlock{Type: types.BlockType(c.Type), Text: c.Text, Data: c.Data, MimeType: c.MimeType, RefStore: c.RefStore, RefKey: c.RefKey, RefMediaType: c.RefMediaType, RefFilename: c.RefFilename, RefSizeBytes: c.RefSizeBytes, RefPageCount: c.RefPageCount})
	}
	return agent.AgentToolResult{Content: content, Details: result.Details, StructuredContent: result.StructuredContent, Usage: result.Usage, Control: result.Control, IsError: result.IsError, Terminate: result.Control == "terminate"}
}
func fromAgent(result agent.AgentToolResult) extension.Result {
	content := make([]extension.Content, 0, len(result.Content))
	for _, c := range result.Content {
		content = append(content, extension.Content{Type: string(c.Type), Text: c.Text, Data: c.Data, MimeType: c.MimeType, RefStore: c.RefStore, RefKey: c.RefKey, RefMediaType: c.RefMediaType, RefFilename: c.RefFilename, RefSizeBytes: c.RefSizeBytes, RefPageCount: c.RefPageCount})
	}
	control := ""
	if result.Terminate {
		control = "terminate"
	}
	if control == "" {
		control = result.Control
	}
	return extension.Result{Content: content, Details: result.Details, StructuredContent: result.StructuredContent, Usage: result.Usage, IsError: result.IsError, Control: control}
}
func (r *Runtime) wrapTool(req *request, t agent.AgentTool) agent.AgentTool {
	identity := t.Definition.Identity
	if identity == "" {
		identity = "builtin/" + t.Name
		t.Definition.Identity = identity
		t.Definition.Revision = "builtin/1"
		t.Definition.Environment = r.EnvironmentID
		t.Definition.SchemaHash = hash(t.Parameters)
	}
	type selectedWrap struct {
		definition extension.Definition
		wrap       extension.Wrap
	}
	var wraps []selectedWrap
	replay := t.Definition.Replay
	for _, d := range req.definitions {
		for _, w := range d.Wraps {
			if w.Target == identity {
				wraps = append(wraps, selectedWrap{d, w})
				if w.Replay != extension.ReplaySafe {
					replay = extension.ReplayNever
				}
			}
		}
		for _, h := range d.Hooks {
			if (h.Event == extension.BeforeTool || h.Event == extension.AfterTool) && h.Replay != extension.ReplaySafe {
				replay = extension.ReplayNever
			}
		}
	}
	if r.CommandHooks {
		replay = extension.ReplayNever
	}
	t.Definition.Replay = replay
	chain, _ := json.Marshal(req.definitions)
	t.Definition.Revision += ":" + hash(chain)
	original := t.Execute
	t.Execute = func(ctx context.Context, id string, args json.RawMessage, update agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
		logical := r.callID(ctx, req, id)
		intentID := logical + ":intent"
		definition := t.Definition
		definition.ArgumentsHash = hash(args)
		if err := r.fact(ctx, intentID, evidence{Kind: "tool_intent", CallID: logical, Name: t.Name, Definition: definition, Arguments: args}); err != nil {
			return agent.AgentToolResult{}, err
		}
		var implementationUsage json.RawMessage
		base := extension.Tool{Name: t.Name, Execute: func(ctx context.Context, _ extension.Call, _ extension.Invocation) (extension.Result, error) {
			out, err := original(ctx, id, args, update)
			implementationUsage = append(json.RawMessage(nil), out.Usage...)
			return fromAgent(out), err
		}}
		stack := make([]extension.Wrap, 0, len(wraps))
		for _, selected := range wraps {
			d, w := selected.definition, selected.wrap
			handler := w.Handle
			w.Handle = func(ctx context.Context, c extension.Call, _ extension.Invocation, next extension.Next) (extension.Result, error) {
				inv := r.grant(ctx, req, d, "wrap:"+w.Name, logical, 1, nil)
				defer inv.Revoke()
				if err := r.fact(ctx, logical+":wrap:"+d.ID+":"+w.Name+":intent", evidence{Kind: "wrapper_intent", Revision: d.Revision}); err != nil {
					return extension.Result{}, err
				}
				out, err := handler(ctx, c, inv, next)
				if err != nil {
					return out, err
				}
				digest, digestErr := durable.Digest(out)
				if digestErr != nil {
					return extension.Result{}, digestErr
				}
				if err = r.fact(context.WithoutCancel(ctx), logical+":wrap:"+d.ID+":"+w.Name+":result", evidence{Kind: "wrapper_result", Hash: digest}); err != nil {
					return out, err
				}
				return out, nil
			}
			stack = append(stack, w)
		}
		result, err := extension.Execute(ctx, base, extension.Call{ID: logical, Name: t.Name, Arguments: append(json.RawMessage(nil), args...)}, nil, stack)
		// A result decorator can't erase the implementation's usage receipt.
		if implementationUsage != nil {
			result.Usage = implementationUsage
		}
		if err != nil {
			return toAgent(result), err
		}
		for _, d := range req.definitions {
			for _, tool := range d.Tools {
				if extension.ToolIdentity(d.ID, tool.Name) == identity {
					if err = validateOutput(tool, result); err != nil {
						return agent.AgentToolResult{}, err
					}
				}
			}
		}
		return toAgent(result), nil
	}
	return t
}
func (r *Runtime) hooks(ctx context.Context, req *request, input extension.HookInput, logical string) (extension.HookInput, string, error) {
	for _, d := range req.definitions {
		for _, h := range d.Hooks {
			if h.Event != input.Event {
				continue
			}
			handlerID := string(h.Event) + ":" + h.Name
			id := logical + ":" + d.ID + ":" + handlerID
			inv := r.grant(ctx, req, d, handlerID, logical, 1, nil)
			decision, err := func() (extension.HookDecision, error) {
				defer inv.Revoke()
				if settled, found, err := r.readFact(ctx, id+":result"); err != nil {
					return extension.HookDecision{}, err
				} else if found && input.Event != extension.AfterTool {
					if settled.Decision == nil {
						return extension.HookDecision{}, errors.New("invalid settled hook receipt")
					}
					return *settled.Decision, nil
				}
				if started, found, err := r.readFact(ctx, id+":intent"); err != nil {
					return extension.HookDecision{}, err
				} else if found && (h.Replay != extension.ReplaySafe || started.Revision != d.Revision) {
					return extension.HookDecision{}, errors.New("interrupted hook outcome unknown; compatible replay-safe handler required")
				}
				intent := evidence{Kind: "hook_intent", Revision: d.Revision, Input: &input}
				if input.Event == extension.AfterTool {
					digest, digestErr := durable.Digest(input)
					if digestErr != nil {
						return extension.HookDecision{}, digestErr
					}
					intent.Hash = digest
					intent.Input = nil
				}
				if err := r.fact(ctx, id+":intent", intent); err != nil {
					return extension.HookDecision{}, err
				}
				decision, err := h.Handle(ctx, cloneHookInput(input), inv)
				if err != nil {
					return decision, err
				}
				receipt := evidence{Kind: "hook_result", Decision: &decision}
				if input.Event == extension.AfterTool {
					receipt.Hash, err = durable.Digest(decision)
					receipt.Decision = nil
					if err != nil {
						return decision, err
					}
				}
				if err = r.fact(context.WithoutCancel(ctx), id+":result", receipt); err != nil {
					return decision, err
				}
				return decision, nil
			}()
			if err == nil {
				input, err = extension.ApplyDecision(input, decision)
			}
			if err != nil {
				if input.Event == extension.AfterResponse {
					slog.Warn("extension observer failed", "extension", d.ID, "hook", h.Name, "error", err)
					continue
				}
				return input, "", err
			}
			if input.Event == extension.OnYield && decision.Kind == "continue_with" {
				return input, decision.Continuation, nil
			}
		}
	}
	return input, "", nil
}
func requestFrom(ctx context.Context) (*request, error) {
	req, _ := ctx.Value(requestKey{}).(*request)
	if req == nil || req.closed.Load() {
		return nil, extension.ErrExpired
	}
	return req, nil
}
func (r *Runtime) BeforeTool(ctx context.Context, call types.ContentBlock) (json.RawMessage, error) {
	req, err := requestFrom(ctx)
	if err != nil {
		return nil, err
	}
	in := extension.HookInput{Event: extension.BeforeTool, Call: &extension.Call{ID: r.callID(ctx, req, call.ID), Name: call.Name, Arguments: call.Arguments}}
	out, _, err := r.hooks(ctx, req, in, in.Call.ID)
	return out.Call.Arguments, err
}
func (r *Runtime) AfterTool(ctx context.Context, call types.ContentBlock, result agent.AgentToolResult) (agent.AgentToolResult, error) {
	req, err := requestFrom(ctx)
	if err != nil {
		return agent.AgentToolResult{Content: []types.ContentBlock{types.NewText("After-tool extension unavailable")}, IsError: true}, err
	}
	logical := r.callID(ctx, req, call.ID)
	kind := "tool_result"
	value := fromAgent(result)
	if ctx.Err() != nil {
		kind = "tool_aborted"
		value = extension.Result{Content: []extension.Content{{Type: "text", Text: "Operation aborted"}}, IsError: true, Usage: value.Usage}
	} else {
		in := extension.HookInput{Event: extension.AfterTool, Call: &extension.Call{ID: logical, Name: call.Name, Arguments: call.Arguments}, Result: &value}
		out, _, hookErr := r.hooks(ctx, req, in, logical)
		err = hookErr
		if err == nil {
			for _, definition := range req.definitions {
				for _, tool := range definition.Tools {
					if extension.ToolName(definition.ID, tool.Name) == call.Name {
						err = validateOutput(tool, *out.Result)
						if err != nil {
							break
						}
					}
				}
				if err != nil {
					break
				}
			}
		}
		if err != nil {
			// A failed redactor must never publish the unapproved content or details.
			value = extension.Result{Content: []extension.Content{{Type: "text", Text: "After-tool extension failed; output withheld"}}, IsError: true, Usage: value.Usage}
		} else {
			value = *out.Result
		}
	}
	if ctx.Err() != nil {
		kind = "tool_aborted"
	}
	if commitErr := r.fact(context.WithoutCancel(ctx), logical+":result", evidence{Kind: kind, CallID: logical, Result: &value}); commitErr != nil {
		return agent.AgentToolResult{Content: []types.ContentBlock{types.NewText("Tool result persistence failed")}, IsError: true}, commitErr
	}
	return toAgent(value), err
}
func (r *Runtime) AfterResponse(ctx context.Context, message types.AssistantMessage) error {
	req, err := requestFrom(ctx)
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(message)
	_, _, err = r.hooks(ctx, req, extension.HookInput{Event: extension.AfterResponse, Response: raw}, req.id)
	return err
}
func (r *Runtime) Yield(ctx context.Context) (string, error) {
	req, err := requestFrom(ctx)
	if err != nil {
		return "", nil
	}
	_, text, err := r.hooks(ctx, req, extension.HookInput{Event: extension.OnYield}, fmt.Sprintf("%s:yield:%d", req.id, r.Counter.Add(1)))
	return text, err
}
func (r *Runtime) BeforeCompaction(ctx context.Context) error {
	req, err := requestFrom(ctx)
	if err != nil {
		return err
	}
	return r.BeforeCompactionSource(ctx, req.id)
}

// BeforeCompactionSource binds decisions to the durable compaction source, even
// when a resumed run hasn't prepared its first provider request.
func (r *Runtime) BeforeCompactionSource(ctx context.Context, source string) error {
	req, err := requestFrom(ctx)
	if err != nil {
		lease, acquireErr := r.Registry.Acquire(r.Selected)
		if acquireErr != nil {
			return acquireErr
		}
		defer lease.Close(context.Background())
		req = &request{lease: lease, definitions: lease.Definitions(), id: "compaction:" + source}
		ctx = context.WithValue(ctx, requestKey{}, req)
		for _, definition := range req.definitions {
			if len(definition.Documents) == 0 {
				continue
			}
			registrar, ok := r.Store.(interface {
				RegisterDocuments(context.Context, []durable.DocumentDefinition) error
			})
			if !ok {
				return errors.New("store cannot validate extension documents")
			}
			if err := registrar.RegisterDocuments(ctx, definition.Documents); err != nil {
				return err
			}
		}
	}
	_, _, err = r.hooks(ctx, req, extension.HookInput{Event: extension.BeforeCompaction}, "compaction:"+source)
	return err
}

// ResolvePreset resolves one selected immutable preset, never executable configuration.
func (r *Runtime) ResolvePreset(id string) (extension.AgentPreset, error) {
	lease, err := r.Registry.Acquire(r.Selected)
	if err != nil {
		return extension.AgentPreset{}, err
	}
	defer lease.Close(context.Background())
	for _, d := range lease.Definitions() {
		for _, p := range d.Presets {
			if d.ID+"/"+p.ID == id {
				return p, nil
			}
		}
	}
	return extension.AgentPreset{}, fmt.Errorf("agent preset unavailable: %s", id)
}

var _ agent.ExtensionRuntime = (*Runtime)(nil)

func cloneHookInput(input extension.HookInput) extension.HookInput {
	if input.Call != nil {
		call := *input.Call
		call.Arguments = append(json.RawMessage(nil), call.Arguments...)
		input.Call = &call
	}
	if input.Result != nil {
		result := *input.Result
		result.Content = append([]extension.Content(nil), result.Content...)
		result.StructuredContent = append(json.RawMessage(nil), result.StructuredContent...)
		result.Details = append(json.RawMessage(nil), result.Details...)
		result.Usage = append(json.RawMessage(nil), result.Usage...)
		input.Result = &result
	}
	input.Response = append(json.RawMessage(nil), input.Response...)
	return input
}
