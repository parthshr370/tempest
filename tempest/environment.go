package tempest

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/editbatch"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/envlocal"
	"go.harness.dev/harness/internal/envrpc"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/toolio"
	"go.harness.dev/harness/internal/tools"
	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/environment"
)

// EnvironmentProcess contains an operator-selected daemon argv and launch settings.
type EnvironmentProcess = envrpc.ProcessOptions

// NewLocalEnvironment opens the existing bounded workspace tools on this host.
func NewLocalEnvironment(cwd string) (environment.Lease, error) {
	local, err := envlocal.New(cwd)
	if err != nil {
		return nil, err
	}
	if err = bindLocalEnvironment(local); err != nil {
		_ = local.Close()
		return nil, err
	}
	return local, nil
}

// NewRemoteEnvironment launches a daemon and verifies its workspace generation.
// An initial reference may name just ID. Persist the returned complete reference.
func NewRemoteEnvironment(ctx context.Context, reference environment.Reference, process EnvironmentProcess) (environment.Lease, error) {
	return envrpc.Launch(ctx, process, reference)
}

// ProcessEnvironmentResolver resolves only explicitly operator-registered names.
func ProcessEnvironmentResolver(processes map[string]EnvironmentProcess) environment.Resolver {
	registered := make(map[string]EnvironmentProcess, len(processes))
	for id, process := range processes {
		process.Argv = append([]string(nil), process.Argv...)
		if process.Env != nil {
			process.Env = append([]string{}, process.Env...)
		}
		registered[id] = process
	}
	return func(ctx context.Context, reference environment.Reference) (environment.Lease, error) {
		process, ok := registered[reference.ID]
		if !ok {
			return nil, fmt.Errorf("required environment %q unavailable", reference.ID)
		}
		return NewRemoteEnvironment(ctx, reference, process)
	}
}

// ServeEnvironment exposes the built-in file, search and exec services on stdio.
// It doesn't advertise host-only checkpoints, interpreters, language servers or Git.
func ServeEnvironment(ctx context.Context, stream io.ReadWriteCloser, cwd, id string) error {
	local, err := envlocal.NewNamed(cwd, id)
	if err != nil {
		return err
	}
	defer local.Close()
	if err = bindLocalEnvironment(local); err != nil {
		return err
	}
	return envrpc.Serve(ctx, stream, local, environment.Capabilities{environment.Files, environment.Search, environment.Exec, environment.Cancel, environment.Permissions})
}

func bindLocalEnvironment(local *envlocal.Local) error {
	directory, err := local.Artifacts()
	if err != nil {
		return err
	}
	stateRoot, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	identity := sha256.Sum256([]byte(local.LocalRoot()))
	stateDir := filepath.Join(stateRoot, "harness", "environments", fmt.Sprintf("%x", identity[:]))
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	queue, err := toolio.NewProcessFileMutationQueue(filepath.Join(stateDir, "locks"))
	if err != nil {
		return err
	}
	if err := recoverEnvironmentState(local.Reference().Generation, stateDir, queue); err != nil {
		return err
	}
	bindEnvironmentInspection(local)
	return tools.BindEnvironment(local, tools.ToolsOptions{SessionDataDir: directory, EditStateDir: stateDir, MutationQueue: queue})
}

func recoverEnvironmentState(generation, stateDir string, queue *toolio.FileMutationQueue) error {
	marker := filepath.Join(stateDir, "generation")
	return queue.Do(marker, func() error {
		previous, err := os.ReadFile(marker)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		batches, err := os.ReadDir(editbatch.Dir(stateDir))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if len(previous) != 0 && string(previous) != generation && len(batches) != 0 {
			return fmt.Errorf("%w: retained edit recovery records at %s need operator attention before changing workspace generation", environment.ErrIdentity, stateDir)
		}
		conflicts, err := os.ReadDir(filepath.Join(editbatch.Dir(stateDir), editbatch.ConflictsDirName))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if len(conflicts) != 0 {
			return fmt.Errorf("environment edit recovery conflicts at %s need operator attention", stateDir)
		}
		outcomes, err := tools.RecoverEditBatches([]string{stateDir}, queue)
		if err != nil {
			return err
		}
		for _, outcome := range outcomes {
			if outcome.Err != nil || len(outcome.Conflicts) != 0 {
				return fmt.Errorf("environment edit recovery needs operator attention: %s", outcome.String())
			}
		}
		if string(previous) != generation {
			root, err := os.OpenRoot(stateDir)
			if err != nil {
				return err
			}
			defer root.Close()
			return editbatch.WriteFile(root, "generation", []byte(generation), 0600)
		}
		return nil
	})
}

func resolveEnvironment(ctx context.Context, options Options, cfg *StackConfig, cwd string) (environment.Lease, error) {
	lease := cfg.Environment
	var err error
	if environment.IsLocation(cwd) && (options.Environment.ID == "" || strings.HasPrefix(options.Environment.ID, "local:")) {
		return nil, fmt.Errorf("%w: remote journal has no persisted environment reference", environment.ErrIdentity)
	}
	if lease == nil {
		if options.Environment.ID != "" && !strings.HasPrefix(options.Environment.ID, "local:") {
			if options.EnvironmentResolver == nil {
				return nil, fmt.Errorf("required environment %q has no resolver", options.Environment.ID)
			}
			lease, err = options.EnvironmentResolver(ctx, options.Environment)
		} else {
			lease, err = envlocal.New(cwd)
		}
	}
	if err != nil {
		return nil, err
	}
	if lease == nil {
		return nil, errors.New("environment resolver returned a nil lease")
	}
	if err = environment.Match(options.Environment, lease.Reference()); err != nil {
		return nil, errors.Join(err, lease.Close())
	}
	cfg.Environment = lease
	return lease, nil
}

func restoreEnvironmentReference(ctx context.Context, options *Options, remoteJournal bool, store interface {
	Read(context.Context, durable.Query) (durable.Page, error)
}) error {
	page, err := store.Read(ctx, durable.Query{Kind: durable.DocumentKind, Namespace: "_tempest/agent", ID: "selection", Limit: 1})
	if err != nil {
		return err
	}
	if len(page.Records) == 0 {
		if remoteJournal {
			return fmt.Errorf("%w: remote journal has no persisted environment reference", environment.ErrIdentity)
		}
		return nil
	}
	choice, err := decodeAgentSelection(page.Records[0].Value)
	if err != nil {
		return err
	}
	if choice.Environment.ID == "" {
		if remoteJournal {
			return fmt.Errorf("%w: remote journal has no persisted environment reference", environment.ErrIdentity)
		}
		return nil
	}
	if err = environment.Match(options.Environment, choice.Environment); err != nil {
		return err
	}
	options.Environment = choice.Environment
	return nil
}

func recoverEnvironmentResults(ctx context.Context, lease environment.Lease, store *journal.Store, loaded *journal.Loaded) (*journal.Loaded, error) {
	changed := false
	for _, call := range loaded.Interrupted {
		if call.CallID == "" || call.Environment != lease.Reference().Key() || environment.ToolCapability(call.Tool) == "" {
			continue
		}
		outcome, err := lease.Lookup(ctx, loaded.Header.ID+":"+call.CallID)
		if err != nil || !outcome.Found || !outcome.Complete {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		var result agent.AgentToolResult
		if outcome.Error != "" {
			result.Content = []types.ContentBlock{{Type: types.BlockText, Text: outcome.Error}}
			result.IsError = true
		} else if err := json.Unmarshal(outcome.Result, &result); err != nil {
			return nil, fmt.Errorf("invalid recovered environment result: %w", err)
		}
		raw, err := json.Marshal(struct {
			Kind   string                `json:"kind"`
			CallID string                `json:"callId"`
			Result agent.AgentToolResult `json:"result"`
		}{"tool_result", call.CallID, result})
		if err != nil {
			return nil, err
		}
		if _, err := store.Commit(ctx, durable.Commit{
			ID:      "environment-result:" + call.CallID,
			Changes: []durable.Change{{SessionID: store.ID(), Kind: durable.EntryKind, Namespace: "_host.environments", ID: call.CallID, Version: 1, Value: raw}},
		}); err != nil {
			return nil, err
		}
		changed = true
	}
	if changed {
		return store.Load(ctx)
	}
	return loaded, nil
}

// Environment returns the session's persisted workspace identity, not launch authority.
func (s *Session) Environment() environment.Reference { return s.core.environment.Reference() }

// EnvironmentLocation groups remote journals without treating remote cwd as a host path.
func EnvironmentLocation(reference environment.Reference) string {
	return environment.Location(reference)
}

func resolveSessionPath(ctx context.Context, dir, id string) (string, error) {
	entries, err := journal.Catalog(ctx, dir, "")
	if err != nil {
		return "", err
	}
	var found string
	for _, entry := range entries {
		if entry.Header.ID != id && strings.TrimSuffix(filepath.Base(entry.Path), ".jsonl") != strings.TrimSuffix(id, ".jsonl") {
			continue
		}
		if found != "" {
			return "", fmt.Errorf("ambiguous session %q", id)
		}
		found = entry.Path
	}
	if found == "" {
		return "", fmt.Errorf("resolve journal %q: %w", id, journal.ErrSessionNotFound)
	}
	return found, nil
}
