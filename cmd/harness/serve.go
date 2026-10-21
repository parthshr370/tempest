package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.harness.dev/harness/internal/config"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/serve"
	"go.harness.dev/harness/tempest"
)

type stdioStream struct {
	*io.PipeReader
	io.Writer
}

func newStdioStream(input io.Reader, output io.Writer) stdioStream {
	reader, writer := io.Pipe()
	// stdin may stay blocked after Close. Only this CLI-owned goroutine waits
	// for it; closing the pipe lets the server finish and the process exit.
	go func() {
		_, err := io.Copy(writer, input)
		_ = writer.CloseWithError(err)
	}()
	return stdioStream{PipeReader: reader, Writer: output}
}

func (s stdioStream) Close() error {
	err := s.PipeReader.Close()
	if closer, ok := s.Writer.(io.Closer); ok {
		if closeErr := closer.Close(); err == nil {
			err = closeErr
		}
	}
	return err
}

func runServe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var values config.FlagValues
	var listen, token string
	var idleTimeout, permissionTimeout, shutdownTimeout, maxWall time.Duration
	var maxTurns int
	var maxCost float64
	fs.StringVar(&listen, "listen", "", "WebSocket listen address (empty serves stdio)")
	fs.StringVar(&token, "token", os.Getenv("TEMPEST_TOKEN"), "WebSocket bearer token")
	fs.DurationVar(&idleTimeout, "idle-timeout", 30*time.Minute, "close idle sessions after this duration")
	fs.DurationVar(&permissionTimeout, "permission-timeout", 5*time.Minute, "deny unanswered asks after this duration")
	fs.DurationVar(&shutdownTimeout, "shutdown-timeout", 10*time.Second, "wait for runs during shutdown")
	fs.StringVar(&values.SessionDir.Value, "session-dir", "", "session journal directory")
	fs.StringVar(&values.Model.Value, "model", "", "default model")
	fs.StringVar(&values.PermissionMode.Value, "permission-mode", "", "default permission mode")
	fs.StringVar(&values.BashTimeout.Value, "bash-timeout", "", "default shell command timeout")
	fs.StringVar(&values.FauxScript.Value, "faux-script", "", "scripted provider responses")
	fs.StringVar(&values.LogFile.Value, "log-file", "", "diagnostic log file")
	fs.StringVar(&values.LogLevel.Value, "log-level", "", "diagnostic log level")
	fs.IntVar(&maxTurns, "max-turns", 0, "default maximum model turns")
	fs.Float64Var(&maxCost, "max-cost", 0, "default maximum model cost in USD")
	fs.DurationVar(&maxWall, "max-wall-time", 0, "default maximum run duration")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "serve accepts flags only")
		return 1
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "session-dir":
			values.SessionDir.Set = true
		case "model":
			values.Model.Set = true
		case "permission-mode":
			values.PermissionMode.Set = true
		case "bash-timeout":
			values.BashTimeout.Set = true
		case "faux-script":
			values.FauxScript.Set = true
		case "log-file":
			values.LogFile.Set = true
		case "log-level":
			values.LogLevel.Set = true
		}
	})
	cfg, err := (config.Config{Flags: values}).Resolve()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	setupLogging(cfg.LogLevel(), cfg.LogFile(), cfg.LogFileMaxBytes())
	mode, err := permission.ParseMode(cfg.PermissionModeDefault())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	stack := &tempest.StackConfig{BashTimeout: cfg.BashTimeout()}
	if cfg.FauxScript() != "" {
		provider, model, err := loadFauxScript(cfg.FauxScript())
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		stack.Model = model
		stack.StreamFn = provider.StreamSimple
	}
	budget := tempest.Budget{MaxTurns: maxTurns, MaxCostUSD: maxCost, MaxWallTime: maxWall}
	if maxTurns < 0 || maxCost < 0 || maxWall < 0 {
		fmt.Fprintln(stderr, "budget limits must not be negative")
		return 1
	}
	server, err := serve.New(serve.Options{SessionDir: cfg.SessionDir(), IdleTimeout: idleTimeout, PermissionTimeout: permissionTimeout, ShutdownTimeout: shutdownTimeout, Token: token, DefaultNew: tempest.NewSession{Model: cfg.Model(), PermissionMode: mode, Budget: budget}, Provider: tempest.ProviderConfig{AnthropicBaseURL: cfg.AnthropicBaseURL(), OpenAIBaseURL: cfg.OpenAIBaseURL()}, Stack: stack})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-signals:
		case <-finished:
			return
		}
		go func() {
			select {
			case <-signals:
				os.Exit(exitAborted)
			case <-finished:
			}
		}()
		_ = server.Close(context.Background())
	}()
	if listen == "" {
		err = server.ServeStdio(newStdioStream(os.Stdin, stdout))
	} else {
		err = server.ListenAndServe(listen)
	}
	closeErr := server.Close(context.Background())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if closeErr != nil {
		fmt.Fprintln(stderr, closeErr)
		return 1
	}
	return 0
}
