package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"

	"go.harness.dev/harness/internal/outputschema"
	"go.harness.dev/harness/tempest/durable"
)

const MaxHandlers = 256

var identifier = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)
var ErrUnavailable = errors.New("required extension unavailable")
var ErrExpired = errors.New("extension invocation expired")
var ErrNextUsed = errors.New("extension continuation already used or expired")
var ErrDenied = errors.New("extension denied operation")

// Registry publishes complete validated generations. It is safe to share across hosts.
type Registry struct {
	mu      sync.Mutex
	current map[string]*generation
	closed  bool
	serial  uint64
}
type generation struct {
	definition Definition
	number     uint64
	refs       int
	retired    bool
	once       sync.Once
}

// Lease pins executable definitions until Close. Definitions returned by Definitions are detached.
type Lease struct {
	registry    *Registry
	generations []*generation
	once        sync.Once
}

func NewRegistry() *Registry { return &Registry{current: make(map[string]*generation)} }

func validateDefinition(d Definition) error {
	if !identifier.MatchString(d.ID) || strings.TrimSpace(d.Revision) == "" || len(d.Revision) > 256 {
		return errors.New("extension ID or revision invalid")
	}
	if len(d.Tasks)+len(d.Tools)+len(d.Sections)+len(d.Hooks)+len(d.Wraps)+len(d.Presets) > MaxHandlers {
		return errors.New("extension handler limit exceeded")
	}
	seen := map[string]bool{}
	check := func(kind, name string) error {
		key := kind + ":" + name
		if !identifier.MatchString(name) || seen[key] {
			return fmt.Errorf("invalid or duplicate %s %q", kind, name)
		}
		seen[key] = true
		return nil
	}
	for _, t := range d.Tasks {
		if err := check("task", t.Name); err != nil {
			return err
		}
		if t.Version == 0 || t.Run == nil || len(t.InputSchema) == 0 || len(t.StateSchema) == 0 {
			return errors.New("task version, schemas and run handler required")
		}
		versions := make(map[uint32]bool)
		for _, version := range t.Migrations {
			if version == 0 || version >= t.Version || versions[version] || t.Migrate == nil {
				return errors.New("invalid task migration versions")
			}
			versions[version] = true
		}
		inputSchema, err := outputschema.Compile(t.InputSchema)
		if err != nil {
			return err
		}
		if inputSchema == nil {
			return errors.New("task input schema required")
		}
		schema, err := outputschema.Compile(t.StateSchema)
		if err != nil {
			return err
		}
		if schema == nil {
			return errors.New("task state schema required")
		}
		if _, err = schema.Validate(string(t.Initial)); err != nil {
			return err
		}
	}
	for _, t := range d.Tools {
		if err := check("tool", t.Name); err != nil {
			return err
		}
		if t.Execute == nil {
			return errors.New("tool handler missing")
		}
		if t.Effect != EffectRead && t.Effect != EffectWrite && t.Effect != EffectExecute {
			return errors.New("invalid effect class")
		}
		if t.Replay != ReplayNever && t.Replay != ReplaySafe {
			return errors.New("invalid replay policy")
		}
		if len(t.InputSchema) == 0 {
			return errors.New("tool input schema required")
		}
		if _, err := outputschema.Compile(t.InputSchema); err != nil {
			return err
		}
		var root struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(t.InputSchema, &root) != nil || root.Type != "object" {
			return errors.New("tool input schema must describe an object")
		}
		if _, err := outputschema.Compile(t.OutputSchema); err != nil {
			return err
		}
	}
	for _, s := range d.Sections {
		if err := check("section", s.Name); err != nil {
			return err
		}
		if s.Render == nil {
			return errors.New("section handler missing")
		}
	}
	for _, h := range d.Hooks {
		if err := check("hook", h.Name); err != nil {
			return err
		}
		if h.Handle == nil {
			return errors.New("hook handler missing")
		}
		switch h.Event {
		case BeforeRequest, AfterResponse, BeforeTool, AfterTool, OnYield, BeforeCompaction:
		default:
			return errors.New("invalid hook event")
		}
		if h.Replay != ReplayNever && h.Replay != ReplaySafe {
			return errors.New("invalid hook replay")
		}
	}
	for _, w := range d.Wraps {
		if err := check("wrap", w.Name); err != nil {
			return err
		}
		if w.Handle == nil || w.Target == "" {
			return errors.New("wrapper target or handler missing")
		}
		if w.Replay != ReplayNever && w.Replay != ReplaySafe {
			return errors.New("invalid wrapper replay")
		}
	}
	for _, p := range d.Presets {
		if err := check("preset", p.ID); err != nil {
			return err
		}
		if p.Version == 0 {
			return errors.New("preset version required")
		}
	}
	for _, document := range d.Documents {
		if document.Namespace != d.ID && !strings.HasPrefix(document.Namespace, d.ID+".") {
			return errors.New("extension document namespace must belong to its bundle")
		}
		if document.Version != 1 {
			return errors.New("document migrations are unsupported; version must be 1")
		}
		schema, err := outputschema.Compile(document.Schema)
		if err != nil {
			return err
		}
		if schema == nil {
			return errors.New("document schema required")
		}
		if _, err = schema.Validate(string(document.Initial)); err != nil {
			return err
		}
	}
	return nil
}
func cloneDefinition(d Definition) Definition {
	d.Tasks = append([]TaskDefinition(nil), d.Tasks...)
	for i := range d.Tasks {
		d.Tasks[i].InputSchema = slices.Clone(d.Tasks[i].InputSchema)
		d.Tasks[i].StateSchema = slices.Clone(d.Tasks[i].StateSchema)
		d.Tasks[i].Initial = slices.Clone(d.Tasks[i].Initial)
		d.Tasks[i].Migrations = slices.Clone(d.Tasks[i].Migrations)
	}
	d.Tools = append([]Tool(nil), d.Tools...)
	for i := range d.Tools {
		d.Tools[i].InputSchema = append(json.RawMessage(nil), d.Tools[i].InputSchema...)
		d.Tools[i].OutputSchema = append(json.RawMessage(nil), d.Tools[i].OutputSchema...)
	}
	d.Sections = append([]Section(nil), d.Sections...)
	d.Hooks = append([]Hook(nil), d.Hooks...)
	d.Wraps = append([]Wrap(nil), d.Wraps...)
	d.Presets = append([]AgentPreset(nil), d.Presets...)
	for i := range d.Presets {
		d.Presets[i].AllowedTools = slices.Clone(d.Presets[i].AllowedTools)
	}
	d.Documents = append(d.Documents[:0:0], d.Documents...)
	for i := range d.Documents {
		d.Documents[i].Schema = append(json.RawMessage(nil), d.Documents[i].Schema...)
		d.Documents[i].Initial = append(json.RawMessage(nil), d.Documents[i].Initial...)
	}
	return d
}
func (g *generation) close(ctx context.Context) error {
	var err error
	g.once.Do(func() {
		if g.definition.Close != nil {
			err = g.definition.Close(ctx)
		}
	})
	return err
}

// Install validates off-lock, then atomically replaces one bundle.
func (r *Registry) Install(ctx context.Context, d Definition) error {
	d = cloneDefinition(d)
	if err := validateDefinition(d); err != nil {
		return err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrExpired
	}
	if r.current == nil {
		r.current = map[string]*generation{}
	}
	old := r.current[d.ID]
	if old != nil {
		for _, previous := range old.definition.Documents {
			for _, replacement := range d.Documents {
				if previous.Namespace != replacement.Namespace {
					continue
				}
				before, beforeErr := durable.Digest(previous)
				after, afterErr := durable.Digest(replacement)
				if beforeErr != nil || afterErr != nil || before != after {
					r.mu.Unlock()
					return errors.New("extension document changes require an explicit migration")
				}
			}
		}
	}
	r.serial++
	r.current[d.ID] = &generation{definition: d, number: r.serial}
	closeOld := false
	if old != nil {
		old.retired = true
		closeOld = old.refs == 0
	}
	r.mu.Unlock()
	if closeOld {
		if err := old.close(ctx); err != nil {
			slog.Warn("retired extension close failed", "extension", d.ID, "error", err)
		}
	}
	return nil
}

// Uninstall blocks future required selections without interrupting admitted work.
func (r *Registry) Uninstall(ctx context.Context, id string) error {
	r.mu.Lock()
	old := r.current[id]
	delete(r.current, id)
	closeOld := false
	if old != nil {
		old.retired = true
		closeOld = old.refs == 0
	}
	r.mu.Unlock()
	if closeOld {
		return old.close(ctx)
	}
	return nil
}

// Acquire resolves all selected names under one lock and pins the resulting snapshot.
func (r *Registry) Acquire(selected []Selection) (*Lease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrExpired
	}
	l := &Lease{registry: r}
	seen := map[string]bool{}
	for _, s := range selected {
		if seen[s.ID] {
			return nil, fmt.Errorf("duplicate extension selection %s", s.ID)
		}
		seen[s.ID] = true
		g := r.current[s.ID]
		if g == nil || g.definition.available != nil && !g.definition.available() {
			if s.Required {
				return nil, fmt.Errorf("%w: %s", ErrUnavailable, s.ID)
			}
			continue
		}
		l.generations = append(l.generations, g)
	}
	for _, g := range l.generations {
		g.refs++
	}
	return l, nil
}
func (l *Lease) Definitions() []Definition {
	d := make([]Definition, 0, len(l.generations))
	for _, g := range l.generations {
		d = append(d, cloneDefinition(g.definition))
	}
	return d
}
func (l *Lease) Close(ctx context.Context) error {
	var errs []error
	l.once.Do(func() {
		r := l.registry
		r.mu.Lock()
		var closing []*generation
		for _, g := range l.generations {
			g.refs--
			if g.retired && g.refs == 0 {
				closing = append(closing, g)
			}
		}
		r.mu.Unlock()
		for _, g := range closing {
			errs = append(errs, g.close(ctx))
		}
	})
	return errors.Join(errs...)
}

// Close retires current generations. Caller-owned leases drain independently.
func (r *Registry) Close(ctx context.Context) error {
	r.mu.Lock()
	r.closed = true
	var closing []*generation
	for id, g := range r.current {
		delete(r.current, id)
		g.retired = true
		if g.refs == 0 {
			closing = append(closing, g)
		}
	}
	r.mu.Unlock()
	var errs []error
	for _, g := range closing {
		errs = append(errs, g.close(ctx))
	}
	return errors.Join(errs...)
}

// ToolIdentity is independent of any permission tier suggested by a display name.
func ToolIdentity(bundle, name string) string { return bundle + "/" + name }

// ToolName is the namespaced model-facing name.
func ToolName(bundle, name string) string {
	// Escape the escape character too, so separators can't alias identifiers.
	escape := func(value string) string {
		value = strings.ReplaceAll(value, "-", "-h")
		return strings.ReplaceAll(value, "_", "-u")
	}
	return "ext__" + escape(bundle) + "__" + escape(name)
}

// CanReplay never retroactively upgrades an interrupted receipt.
func CanReplay(stored, current ReplayPolicy, storedRevision, currentRevision, storedEnvironment, currentEnvironment string) bool {
	return stored == ReplaySafe && current == ReplaySafe && storedRevision != "" && storedRevision == currentRevision && storedEnvironment == currentEnvironment
}
