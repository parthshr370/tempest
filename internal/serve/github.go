package serve

import (
	"context"
	"errors"

	"go.harness.dev/harness/internal/githubsource"
	"go.harness.dev/harness/tempest/extension"
)

// openGitHub runs the GitHub runner's startup checks, opens its control
// journal and returns the registry holding its worker bundle, which the
// Host must be opened with. Worker sessions select the bundle themselves;
// ACP sessions keep an empty selection and never see it.
func openGitHub(ctx context.Context, opts Options) (*githubsource.Service, *extension.Registry, error) {
	if len(opts.ExtensionProcesses) > 0 {
		// Process bundles are installed into the session's registry at
		// every session open; with the runner's shared registry each open
		// would replace the bundle under the other sessions.
		return nil, nil, errors.New("serve: -extension-config cannot be combined with -github")
	}
	options := *opts.GitHub
	if options.Config.Workers.Model == "" {
		options.Config.Workers.Model = opts.DefaultNew.Model
	}
	service, err := githubsource.Open(ctx, options)
	if err != nil {
		return nil, nil, err
	}
	registry := extension.NewRegistry()
	if err := registry.Install(ctx, service.Extension()); err != nil {
		return nil, nil, errors.Join(err, registry.Close(ctx), service.Close(ctx))
	}
	return service, registry, nil
}

// closeGitHub releases the runner's journal and registry after the Host
// closed; both may be nil.
func closeGitHub(ctx context.Context, service *githubsource.Service, registry *extension.Registry) error {
	var err error
	if service != nil {
		err = service.Close(ctx)
	}
	if registry != nil {
		err = errors.Join(err, registry.Close(ctx))
	}
	return err
}

// GitHub is the running GitHub ticket runner, nil without -github.
func (s *Server) GitHub() *githubsource.Service { return s.github }
