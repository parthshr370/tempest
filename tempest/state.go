package tempest

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"go.harness.dev/harness/internal/outputschema"
	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/extension"
)

type documentSpec struct {
	definition durable.DocumentDefinition
	schema     *outputschema.Schema
}

// Read exposes bounded committed records. Foreign session reads are rejected.
func (s *Session) Read(ctx context.Context, q durable.Query) (durable.Page, error) {
	if err := s.authorize(ctx, "read", q.Namespace, q.ID); err != nil {
		return durable.Page{}, err
	}
	if q.SessionID != "" && q.SessionID != s.ID() {
		return durable.Page{}, fmt.Errorf("foreign session read")
	}
	q.SessionID = s.ID()
	originalID := q.CommitID
	if originalID != "" {
		digest, err := durable.Digest(originalID)
		if err != nil {
			return durable.Page{}, err
		}
		q.CommitID = "application:" + digest
	}
	page, err := s.core.store.Read(ctx, q)
	for i := range page.Receipts {
		page.Receipts[i].ID = originalID
	}
	return page, err
}

// Commit applies only granted application documents and application entries.
// The store compares every revision before appending any part of the batch.
func (s *Session) Commit(ctx context.Context, c durable.Commit) (durable.Receipt, error) {
	if err := s.authorize(ctx, "commit", "", c.ID); err != nil {
		return durable.Receipt{}, err
	}
	for i := range c.Changes {
		ch := &c.Changes[i]
		if ch.SessionID == "" {
			ch.SessionID = s.ID()
		}
		if ch.SessionID != s.ID() || strings.HasPrefix(ch.Namespace, "_") {
			return durable.Receipt{}, fmt.Errorf("commit outside application scope")
		}
		spec, ok := s.core.documents[ch.Namespace]
		if !ok {
			return durable.Receipt{}, fmt.Errorf("ungranted document namespace %q", ch.Namespace)
		}
		if ch.Kind != durable.DocumentKind && ch.Kind != durable.EntryKind {
			return durable.Receipt{}, fmt.Errorf("host control kind is read-only")
		}
		if ch.Version != spec.definition.Version {
			return durable.Receipt{}, fmt.Errorf("document version mismatch")
		}
		if ch.Kind == durable.DocumentKind && !ch.Delete {
			if _, err := spec.schema.Validate(string(ch.Value)); err != nil {
				return durable.Receipt{}, err
			}
		}
	}
	for i := range c.Expected {
		check := &c.Expected[i]
		if check.SessionID == "" {
			check.SessionID = s.ID()
		}
		if check.SessionID != s.ID() || strings.HasPrefix(check.Namespace, "_") {
			return durable.Receipt{}, fmt.Errorf("comparison outside application scope")
		}
	}
	if c.ID == "" {
		return durable.Receipt{}, fmt.Errorf("commit ID required")
	}
	originalID := c.ID
	digest, err := durable.Digest(originalID)
	if err != nil {
		return durable.Receipt{}, err
	}
	c.ID = "application:" + digest
	r, err := s.core.store.Commit(ctx, c)
	r.ID = originalID
	if err == nil {
		s.publishDurable(ctx)
	}
	return r, err
}

// Memo preserves the winning JSON value for one host-established invocation.
func (s *Session) Memo(ctx context.Context, scope durable.MemoScope, key string, candidate json.RawMessage) (json.RawMessage, error) {
	if scope.SessionID != "" && scope.SessionID != s.ID() {
		return nil, fmt.Errorf("foreign memo scope")
	}
	scope.SessionID = s.ID()
	return s.core.store.Memo(ctx, scope, key, candidate)
}

// ReadMemo reports presence independently of its JSON value.
func (s *Session) ReadMemo(ctx context.Context, scope durable.MemoScope, key string) (json.RawMessage, bool, error) {
	if scope.SessionID != "" && scope.SessionID != s.ID() {
		return nil, false, fmt.Errorf("foreign memo scope")
	}
	scope.SessionID = s.ID()
	return s.core.store.ReadMemo(ctx, scope, key)
}

func (s *Session) initializeDocuments(ctx context.Context) error {
	documents := make(map[string]documentSpec)
	query := durable.Query{Kind: durable.DocumentKind, Namespace: "_tempest/definitions", Limit: 100}
	for {
		page, err := s.core.store.Read(ctx, query)
		if err != nil {
			return err
		}
		for _, record := range page.Records {
			var d durable.DocumentDefinition
			if err = json.Unmarshal(record.Value, &d); err != nil {
				return err
			}
			if err = s.core.store.RegisterDocuments(ctx, []durable.DocumentDefinition{d}); err != nil {
				return err
			}
			schema, err := outputschema.Compile(d.Schema)
			if err != nil {
				return err
			}
			documents[d.Namespace] = documentSpec{d, schema}
		}
		if page.NextCursor == "" {
			break
		}
		query.Cursor = page.NextCursor
	}
	s.core.documents = documents
	return nil
}

func (s *Session) publishDurable(ctx context.Context) {
	s.core.durablePublishMu.Lock()
	defer s.core.durablePublishMu.Unlock()
	if s.core.store.Position().Seq <= s.core.durablePublished {
		return
	}
	s.core.notifySubmissions()
	if s.core.hub == nil {
		return
	}
	docs, err := s.core.store.Read(context.WithoutCancel(ctx), durable.Query{Kind: durable.DocumentKind, Limit: 100, Reverse: true, Application: true})
	if err != nil {
		return
	}
	submissions, err := s.core.store.Read(context.WithoutCancel(ctx), durable.Query{Kind: durable.SubmissionKind, AsOf: docs.Seq, Limit: 100, Reverse: true})
	if err != nil {
		return
	}
	tasks, err := s.core.store.Read(context.WithoutCancel(ctx), durable.Query{Kind: durable.TaskKind, AsOf: docs.Seq, Limit: 100, Reverse: true})
	if err != nil {
		return
	}
	for i := range docs.Records {
		docs.Records[i].Value = nil
		docs.Records[i].CommitID = ""
	}
	for i := range submissions.Records {
		record := &submissions.Records[i]
		var value durable.Submission
		if err := json.Unmarshal(record.Value, &value); err != nil {
			return
		}
		record.ID, record.Namespace = value.ID, ""
		record.CommitID = ""
		record.Value, err = json.Marshal(struct {
			ID    string `json:"id"`
			State string `json:"state"`
			RunID string `json:"runId,omitempty"`
		}{value.ID, value.State, value.RunID})
		if err != nil {
			return
		}
	}
	for index := range tasks.Records {
		record := &tasks.Records[index]
		var task extension.Task
		if err := json.Unmarshal(record.Value, &task); err != nil {
			return
		}
		record.CommitID = ""
		record.Value, err = json.Marshal(task.Summary())
		if err != nil {
			return
		}
	}
	const projectionBytes = 64 << 10
	for _, page := range []*durable.Page{&docs, &submissions, &tasks} {
		total := 0
		for i, record := range page.Records {
			raw, err := json.Marshal(record)
			if err != nil {
				return
			}
			total += len(raw)
			if total > projectionBytes {
				page.Records = page.Records[:i]
				break
			}
		}
		page.NextCursor = ""
	}
	s.core.hub.SetDurable(docs.Seq, docs, submissions, tasks)
	s.core.durablePublished = docs.Seq
}

type agentSelection struct {
	Model        string                `json:"model"`
	Reasoning    string                `json:"reasoning,omitempty"`
	Extensions   []extension.Selection `json:"extensions,omitempty"`
	AllowedTools []string              `json:"allowedTools"`
	ExcludeTools []string              `json:"excludeTools,omitempty"`
	Instructions []string              `json:"instructions,omitempty"`
	CustomPrompt string                `json:"customPrompt,omitempty"`
	Environment  string                `json:"environment"`
	HostManaged  bool                  `json:"hostManaged,omitempty"`
	MCPNames     []string              `json:"mcpNames,omitempty"`
}

func (s *Session) persistAgent(ctx context.Context, o Options, cfg StackConfig, model string, endpoints []MCPServerConfig) error {
	choice := agentSelection{Model: model, Reasoning: cfg.Reasoning, Extensions: effectiveExtensions(o), AllowedTools: cfg.AllowedTools, ExcludeTools: cfg.ExcludeTools, Instructions: cfg.PromptAdditions, CustomPrompt: cfg.CustomPrompt, Environment: "local"}
	choice.HostManaged = o.hostCreate
	for _, endpoint := range endpoints {
		choice.MCPNames = append(choice.MCPNames, endpoint.Name)
	}
	raw, err := json.Marshal(choice)
	if err != nil {
		return err
	}
	page, err := s.core.store.Read(ctx, durable.Query{Kind: durable.DocumentKind, Namespace: "_tempest/agent", ID: "selection", Limit: 1})
	if err != nil {
		return err
	}
	var revision uint64
	if len(page.Records) > 0 {
		revision = page.Records[0].Revision
		old, _ := durable.CanonicalJSON(page.Records[0].Value)
		next, _ := durable.CanonicalJSON(raw)
		if string(old) == string(next) {
			return s.core.store.MarkInitialized(choice.HostManaged)
		}
	}
	_, err = s.core.store.Commit(ctx, durable.Commit{ID: fmt.Sprintf("agent:%d", revision+1), Expected: []durable.RevisionCheck{{SessionID: s.ID(), Kind: durable.DocumentKind, Namespace: "_tempest/agent", ID: "selection", Revision: revision}}, Changes: []durable.Change{{SessionID: s.ID(), Kind: durable.DocumentKind, Namespace: "_tempest/agent", ID: "selection", Version: 1, Value: raw}}})
	if err != nil {
		return err
	}
	return s.core.store.MarkInitialized(choice.HostManaged)
}
func restoreAgent(ctx context.Context, o *Options, cfg *StackConfig, loadedStore interface {
	Read(context.Context, durable.Query) (durable.Page, error)
}, endpoints *[]MCPServerConfig) error {
	page, err := loadedStore.Read(ctx, durable.Query{Kind: durable.DocumentKind, Namespace: "_tempest/agent", ID: "selection", Limit: 1})
	if err != nil {
		return err
	}
	if len(page.Records) == 0 {
		return nil
	}
	var choice agentSelection
	if err = json.Unmarshal(page.Records[0].Value, &choice); err != nil {
		return fmt.Errorf("agent selection: %w", err)
	}
	if choice.Environment != "local" {
		return fmt.Errorf("required environment %q unavailable", choice.Environment)
	}
	if cfg.Reasoning == "" {
		cfg.Reasoning = choice.Reasoning
	}
	if cfg.AllowedTools == nil {
		cfg.AllowedTools = choice.AllowedTools
	} else if choice.AllowedTools != nil {
		allowed := make(map[string]bool, len(choice.AllowedTools))
		for _, name := range choice.AllowedTools {
			allowed[name] = true
		}
		current := make([]string, 0, len(cfg.AllowedTools))
		for _, name := range cfg.AllowedTools {
			if allowed[name] {
				current = append(current, name)
			}
		}
		cfg.AllowedTools = current
	}
	for _, name := range choice.ExcludeTools {
		if !slices.Contains(cfg.ExcludeTools, name) {
			cfg.ExcludeTools = append(cfg.ExcludeTools, name)
		}
	}
	if cfg.PromptAdditions == nil {
		cfg.PromptAdditions = choice.Instructions
	}
	if cfg.CustomPrompt == "" {
		cfg.CustomPrompt = choice.CustomPrompt
	}
	if o.Extensions == nil {
		o.Extensions = choice.Extensions
	} else {
		for _, saved := range choice.Extensions {
			if !saved.Required {
				continue
			}
			found := false
			for i := range o.Extensions {
				if o.Extensions[i].ID == saved.ID {
					o.Extensions[i].Required = true
					found = true
					break
				}
			}
			if !found {
				o.Extensions = append(o.Extensions, saved)
			}
		}
	}
	o.hostCreate = choice.HostManaged
	if o.Resume != nil && o.Resume.MCPServers == nil {
		available := make(map[string]MCPServerConfig)
		for _, endpoint := range *endpoints {
			available[endpoint.Name] = endpoint
		}
		selected := make([]MCPServerConfig, 0, len(choice.MCPNames))
		for _, name := range choice.MCPNames {
			endpoint, ok := available[name]
			if !ok {
				return fmt.Errorf("required MCP endpoint %q unavailable", name)
			}
			selected = append(selected, endpoint)
		}
		*endpoints = selected
	}
	return nil
}
