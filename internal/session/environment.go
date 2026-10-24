package session

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.harness.dev/harness/internal/config"
	"go.harness.dev/harness/internal/envrpc"
	"go.harness.dev/harness/tempest/environment"
)

func configuredEnvironmentReference(cfg *config.ResolvedConfig) environment.Reference {
	if cfg.Environment() == "" {
		return environment.Reference{}
	}
	return environment.Reference{ID: cfg.Environment(), Cwd: "."}
}

func configuredEnvironmentResolver(cfg *config.ResolvedConfig) environment.Resolver {
	processes := cfg.Environments()
	return func(ctx context.Context, reference environment.Reference) (environment.Lease, error) {
		process, ok := processes[reference.ID]
		if !ok {
			return nil, fmt.Errorf("required environment %q unavailable", reference.ID)
		}
		var timeout time.Duration
		if process.Timeout != "" {
			var err error
			timeout, err = time.ParseDuration(process.Timeout)
			if err != nil {
				return nil, err
			}
		}
		return envrpc.Launch(ctx, envrpc.ProcessOptions{Argv: process.Argv, Env: process.Env, Cwd: process.Cwd, Timeout: timeout, Stderr: os.Stderr}, reference)
	}
}
