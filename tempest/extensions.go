package tempest

import (
	"context"
	"errors"

	"go.harness.dev/harness/internal/extensionhost"
	"go.harness.dev/harness/internal/session"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/extension"
)

// configureExtensions binds executable operator configuration to one session store.
// It never discovers commands in workspace files or persists launch credentials.
func configureExtensions(ctx context.Context, o Options, cfg *session.StackConfig, store *journal.Store, loaded *journal.Loaded) (func() error, error) {
	o.Extensions = effectiveExtensions(o)
	registry := o.Registry
	owned := registry == nil
	if registry == nil && len(o.ExtensionProcesses) == 0 && len(o.Extensions) == 0 {
		return func() error { return nil }, nil
	}
	if registry == nil {
		registry = extension.NewRegistry()
	}
	close := func() error {
		if owned {
			return registry.Close(context.Background())
		}
		return nil
	}
	for _, process := range o.ExtensionProcesses {
		if o.admitProcess != nil {
			process.AdmitProcess = o.admitProcess
		}
		if err := registry.InstallProcess(ctx, process); err != nil {
			return nil, errors.Join(err, close())
		}
	}
	selected := o.Extensions
	// Required safety bundles are checked at open and again before every request.
	lease, err := registry.Acquire(selected)
	if err != nil {
		return nil, errors.Join(err, close())
	}
	var documents []durable.DocumentDefinition
	for _, definition := range lease.Definitions() {
		documents = append(documents, definition.Documents...)
	}
	_ = lease.Close(ctx)
	if err = store.RegisterDocuments(ctx, documents); err != nil {
		return nil, errors.Join(err, close())
	}
	cfg.Extensions = &extensionhost.Runtime{Registry: registry, Selected: selected, Store: store, SessionID: store.ID(), Cwd: cfg.Cwd, EnvironmentID: cfg.Environment.Reference().Key(), Environment: cfg.Environment}
	cfg.ResolvePreset = cfg.Extensions.ResolvePreset
	return close, nil
}

func effectiveExtensions(o Options) []extension.Selection {
	selected := append([]extension.Selection(nil), o.Extensions...)
	for _, process := range o.ExtensionProcesses {
		found := false
		for i := range selected {
			if selected[i].ID == process.ID {
				selected[i].Required = true
				found = true
				break
			}
		}
		if !found {
			selected = append(selected, extension.Selection{ID: process.ID, Required: true})
		}
	}
	return selected
}
