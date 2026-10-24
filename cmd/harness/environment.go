package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"go.harness.dev/harness/tempest"
)

type environmentStdio struct {
	io.Reader
	io.Writer
}

func (stream *environmentStdio) Close() error {
	if closer, ok := stream.Reader.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func runEnvironment(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("tempest env", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var cwd, id string
	flags.StringVar(&cwd, "cwd", ".", "workspace root on this daemon host")
	flags.StringVar(&id, "id", "", "operator workspace identity (required)")
	if err := flags.Parse(args); err != nil {
		return parseExitCode(err)
	}
	if flags.NArg() != 0 || id == "" {
		fmt.Fprintln(stderr, "usage: tempest env -id <workspace> [-cwd <root>]")
		return exitUsage
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := tempest.ServeEnvironment(ctx, &environmentStdio{Reader: os.Stdin, Writer: stdout}, cwd, id); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
