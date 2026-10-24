package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.harness.dev/harness/internal/config"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/tempest"
)

func runConversation(command string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	var sessionID, sessionDir, cwd, fauxScript, entryID, note, requestID, query, cursor, newCwd string
	var limit int
	var precedingSafe bool
	flags.StringVar(&sessionID, "session", "", "session id or journal path; defaults to the most recent session")
	flags.StringVar(&sessionDir, "session-dir", "", "session storage directory")
	flags.StringVar(&cwd, "cwd", "", "workspace used to find the session")
	flags.StringVar(&fauxScript, "faux-script", "", "hermetic provider response script")
	switch command {
	case "fork":
		flags.StringVar(&entryID, "entry", "", "durable transcript entry id")
		flags.BoolVar(&precedingSafe, "preceding-safe", false, "select the preceding complete tool boundary")
		flags.StringVar(&newCwd, "new-cwd", "", "explicit workspace for the sibling; default shares the parent's workspace")
	case "reset":
		flags.StringVar(&note, "note", "", "audit note for the context change")
		flags.StringVar(&requestID, "request-id", "", "required durable request id")
	case "history":
		flags.StringVar(&query, "query", "", "literal case-insensitive text")
		flags.StringVar(&cursor, "cursor", "", "nextCursor from a prior page")
		flags.IntVar(&limit, "limit", 20, "archive records per page, at most 100")
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return parseExitCode(err)
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, command+" does not accept positional arguments")
		return exitUsage
	}
	if command == "fork" && entryID == "" {
		fmt.Fprintln(stderr, "fork requires -entry")
		return exitUsage
	}
	if command == "reset" && requestID == "" {
		fmt.Fprintln(stderr, "reset requires -request-id")
		return exitUsage
	}
	ctx := context.Background()
	err := func() (operationErr error) {
		if cwd == "" {
			var err error
			cwd, err = os.Getwd()
			if err != nil {
				return err
			}
		}
		var err error
		cwd, err = filepath.Abs(cwd)
		if err != nil {
			return err
		}
		resolved, err := (config.Config{WorkingDir: cwd}).Resolve()
		if err != nil {
			return err
		}
		if sessionDir == "" {
			sessionDir = resolved.SessionDir()
		}
		if fauxScript == "" {
			fauxScript = resolved.FauxScript()
		}
		if sessionID == "" || filepath.Ext(sessionID) == ".jsonl" || filepath.IsAbs(sessionID) {
			path := sessionID
			if path == "" {
				path, err = journal.Recent(ctx, sessionDir, cwd)
				if err != nil {
					return err
				}
			}
			loaded, err := journal.Load(ctx, path)
			if err != nil {
				return err
			}
			sessionID = loaded.Header.ID
		}
		host, err := tempest.OpenHost(ctx, tempest.HostOptions{SessionDir: sessionDir, FauxScript: fauxScript})
		if err != nil {
			return err
		}
		defer func() { operationErr = errors.Join(operationErr, host.Close(context.Background())) }()
		session, err := host.Open(ctx, sessionID)
		if err != nil {
			return err
		}
		switch command {
		case "fork":
			child, err := host.Fork(ctx, sessionID, entryID, tempest.ForkOptions{Cwd: newCwd, PrecedingSafe: precedingSafe})
			if err != nil {
				return err
			}
			return json.NewEncoder(stdout).Encode(struct {
				SessionID string `json:"sessionId"`
			}{child.ID()})
		case "reset":
			receipt, err := session.Reset(ctx, note, requestID)
			if err != nil {
				return err
			}
			return json.NewEncoder(stdout).Encode(receipt)
		case "compact":
			task, err := session.Compact(ctx, tempest.CompactOptions{Wait: true})
			if err != nil {
				return err
			}
			return json.NewEncoder(stdout).Encode(task)
		case "history":
			page, err := session.SearchHistory(ctx, tempest.HistoryOptions{Query: query, Cursor: cursor, Limit: limit})
			if err != nil {
				return err
			}
			return json.NewEncoder(stdout).Encode(page)
		default:
			return fmt.Errorf("unknown conversation control %q", command)
		}
	}()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
