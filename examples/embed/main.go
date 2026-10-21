// Command embed runs a durable session without importing Tempest's internal packages.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"go.harness.dev/harness/tempest"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() (runErr error) {
	model := flag.String("model", "", "provider:model route (default: hermetic faux provider)")
	prompt := flag.String("p", "Say hello", "prompt to run")
	sessionDir := flag.String("session-dir", "", "journal directory (default: Tempest configuration)")
	flag.Parse()

	ctx := context.Background()
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	options := tempest.Options{
		Cwd: cwd, SessionDir: *sessionDir,
		New: &tempest.NewSession{Model: *model},
	}
	if *model == "" {
		// The default needs no credentials or network. A model flag uses the
		// normal provider routing and credentials from the environment instead.
		scriptDir, err := os.MkdirTemp("", "tempest-embed-")
		if err != nil {
			return err
		}
		defer func() { runErr = errors.Join(runErr, os.RemoveAll(scriptDir)) }()
		options.FauxScript = filepath.Join(scriptDir, "faux.json")
		if err := os.WriteFile(options.FauxScript, []byte(`[{"text":"Hello from Tempest."}]`), 0o600); err != nil {
			return err
		}
	}
	session, err := tempest.Open(ctx, options)
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, session.Close(context.Background())) }()

	snapshot, updates, stopWatch := session.Watch(ctx, tempest.WatchOptions{})
	fmt.Printf("session %s at seq %d\n", snapshot.SessionID, snapshot.Seq)
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		for update := range updates {
			fmt.Printf("%s %s\n", update.Kind, update.Update)
		}
	}()
	result, err := session.Prompt(ctx, tempest.Prompt{Text: *prompt})
	stopWatch()
	<-watched
	if err != nil {
		return err
	}
	fmt.Printf("stop reason: %s\n", result.StopReason)
	return result.Err
}
