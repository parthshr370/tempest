package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.harness.dev/harness/internal/checkpoint"
	"go.harness.dev/harness/internal/config"
	"go.harness.dev/harness/internal/editbatch"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/tools"
)

func runRewind(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("rewind", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var sessionID, dir, cwd, id, report string
	var force, list bool
	fs.StringVar(&sessionID, "session", "", "session id or journal path; defaults to the most recent session")
	fs.StringVar(&dir, "session-dir", "", "session storage directory")
	fs.StringVar(&cwd, "cwd", "", "workspace directory (defaults to current directory)")
	fs.StringVar(&id, "checkpoint", "", "checkpoint id (defaults to newest explicit, otherwise newest automatic)")
	fs.StringVar(&report, "report", "", "findings retained in the rewound conversation")
	fs.BoolVar(&force, "force", false, "allow losing outside workspace edits")
	fs.BoolVar(&list, "list", false, "list retained checkpoints without rewinding")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return parseExitCode(err)
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "rewind does not accept positional arguments")
		return exitUsage
	}
	ctx := context.Background()
	err := func() error {
		if cwd == "" {
			var e error
			cwd, e = os.Getwd()
			if e != nil {
				return e
			}
		}
		var e error
		cwd, e = filepath.Abs(cwd)
		if e != nil {
			return e
		}
		if dir == "" {
			cfg, e := (config.Config{}).Resolve()
			if e != nil {
				return e
			}
			dir = cfg.SessionDir()
		}
		path := sessionID
		if path == "" {
			path, e = journal.Recent(ctx, dir, cwd)
		} else if !filepath.IsAbs(path) && filepath.Ext(path) != ".jsonl" {
			path, e = journal.Resolve(ctx, dir, cwd, path)
		}
		if e != nil {
			return e
		}
		if list {
			checkpoints, err := checkpoint.ReadList(path)
			if err != nil {
				return err
			}
			return json.NewEncoder(stdout).Encode(checkpoints)
		}
		store, loaded, e := journal.OpenForResume(ctx, path, journal.Options{})
		if e != nil {
			return e
		}
		defer store.Close()
		for _, j := range loaded.Jobs {
			switch j.State {
			case "starting", "running", "recovering":
				return fmt.Errorf("rewind requires settled jobs and children; resume and settle %s first", j.ID)
			}
		}
		if outcomes, e := editbatch.Recover(store.Path()+".d", tools.DefaultMutationQueue()); e != nil {
			return e
		} else {
			for _, o := range outcomes {
				if o.Err != nil || len(o.Conflicts) > 0 {
					return fmt.Errorf("%s", o.String())
				}
			}
		}
		manager, e := checkpoint.New(ctx, loaded.Header.Cwd, store, tools.DefaultMutationQueue())
		if e != nil {
			return e
		}
		if _, e = manager.Rewind(ctx, id, report, force); e != nil {
			return e
		}
		fmt.Fprintln(stdout, "Rewound workspace and conversation; journal history retained.")
		return nil
	}()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
