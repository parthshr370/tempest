package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.harness.dev/harness/internal/config"
	"go.harness.dev/harness/internal/mcpconfig"
	"go.harness.dev/harness/internal/mcpoauth"
)

func runMCP(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "login", "logout", "list":
			return runMCPClient(args[0], args[1:], stdout, stderr)
		}
	}
	return runServerCommand("mcp", args, stdout, stderr)
}

// runMCPClient runs the MCP client commands against the resolved server list.
func runMCPClient(command string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tempest mcp "+command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var values config.FlagValues
	fs.StringVar(&values.MCPConfig.Value, "mcp-config", "", "operator-selected MCP server JSON file")
	fs.StringVar(&values.AgentDir.Value, "agent-dir", "", "agent data directory (holds the MCP OAuth token store)")
	noBrowser := false
	if command == "login" {
		fs.BoolVar(&noBrowser, "no-browser", false, "print the authorization URL without opening a browser")
	}
	if err := fs.Parse(args); err != nil {
		return parseExitCode(usageFailure(err))
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "mcp-config":
			values.MCPConfig.Set = true
		case "agent-dir":
			values.AgentDir.Set = true
		}
	})
	wantName := command != "list"
	if (wantName && fs.NArg() != 1) || (!wantName && fs.NArg() != 0) {
		if wantName {
			fmt.Fprintf(stderr, "usage: tempest mcp %s [flags] <server>\n", command)
		} else {
			fmt.Fprintln(stderr, "usage: tempest mcp list [flags]")
		}
		return exitUsage
	}
	cfg, err := (config.Config{Flags: values}).Resolve()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	store := mcpoauth.NewStore(mcpoauth.DefaultDir(cfg.AgentDir()))
	servers := cfg.MCPServers()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch command {
	case "list":
		return printMCPServers(stdout, stderr, servers, store)
	case "logout":
		name := fs.Arg(0)
		server, found := findMCPServer(servers, name)
		if !found || !server.UsesOAuth() {
			// The record is keyed by name; forget it even for a removed server.
			unlock, err := store.Lock(ctx, name)
			if err == nil {
				err = store.Delete(name)
				unlock()
			}
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
		} else if err := mcpLogout(ctx, server, store); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "Signed out of MCP server %s.\n", name)
		return 0
	}
	server, found := findMCPServer(servers, fs.Arg(0))
	if !found {
		fmt.Fprintf(stderr, "no MCP server named %q is configured (%s); select the file with -mcp-config\n", fs.Arg(0), mcpServerNames(servers))
		return 1
	}
	if err := mcpLogin(ctx, server, store, noBrowser, stdout, stderr); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func mcpLogin(ctx context.Context, server mcpconfig.Server, store *mcpoauth.Store, noBrowser bool, stdout, stderr io.Writer) error {
	if !server.UsesOAuth() {
		return fmt.Errorf("MCP server %s uses %s; OAuth applies to HTTP servers without a configured Authorization header", server.Name, mcpCredentialSource(server))
	}
	flow, err := mcpoauth.New(server, store, nil)
	if err != nil {
		return err
	}
	challenge, challenged, err := flow.Probe(ctx)
	if err != nil {
		return fmt.Errorf("contacting MCP server %s: %w", server.Name, err)
	}
	if !challenged && server.Auth != "oauth" {
		return fmt.Errorf(`MCP server %s did not ask for authorization (no 401 or 403 answer); set "auth": "oauth" on it to sign in through its well-known OAuth metadata`, server.Name)
	}
	record, err := flow.Login(ctx, challenge, func(ctx context.Context, authorizationURL string) error {
		fmt.Fprintf(stdout, "Sign in to MCP server %s. Open this URL in a browser on this host:\n%s\n", server.Name, authorizationURL)
		if !noBrowser {
			if err := openSignInBrowser(ctx, authorizationURL); err != nil {
				fmt.Fprintln(stderr, "Could not open the browser. Open the URL above manually on this host.")
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Signed in to MCP server %s%s.\n", server.Name, describeGrant(record.Scope, record.ExpiresAt, record.RefreshToken != ""))
	return nil
}

func mcpLogout(ctx context.Context, server mcpconfig.Server, store *mcpoauth.Store) error {
	flow, err := mcpoauth.New(server, store, nil)
	if err != nil {
		return err
	}
	return flow.Logout(ctx)
}

func printMCPServers(stdout, stderr io.Writer, servers []mcpconfig.Server, store *mcpoauth.Store) int {
	if len(servers) == 0 {
		fmt.Fprintln(stdout, "No MCP servers configured; select a file with -mcp-config or set mcp_servers in settings.")
		return 0
	}
	for _, server := range servers {
		target := server.URL
		if server.Transport == "stdio" {
			target = server.Command
		}
		state := mcpCredentialSource(server)
		if server.UsesOAuth() {
			flow, err := mcpoauth.New(server, store, nil)
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			status, err := flow.Status()
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			state = "oauth: not signed in (tempest mcp login " + server.Name + " if the server requires it)"
			if status.SignedIn {
				state = "oauth: signed in" + describeGrant(status.Scope, status.ExpiresAt, status.Refreshable)
			}
			if status.PendingScope != "" {
				state += "; needs login for more scope: " + status.PendingScope
			}
		}
		extras := ""
		if server.Sampling {
			extras = ", sampling on"
		}
		fmt.Fprintf(stdout, "%s\t%s %s\t%s%s\n", server.Name, server.Transport, target, state, extras)
	}
	return 0
}

func mcpCredentialSource(server mcpconfig.Server) string {
	if server.Transport == "stdio" {
		return "stdio (credentials come from its environment)"
	}
	return "a static Authorization header"
}

func describeGrant(scope string, expires time.Time, refreshable bool) string {
	var parts []string
	if scope != "" {
		parts = append(parts, "scope "+scope)
	}
	if !expires.IsZero() {
		parts = append(parts, "expires "+expires.UTC().Format(time.RFC3339))
	}
	if refreshable {
		parts = append(parts, "refreshable")
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

func findMCPServer(servers []mcpconfig.Server, name string) (mcpconfig.Server, bool) {
	for _, server := range servers {
		if server.Name == name {
			return server, true
		}
	}
	return mcpconfig.Server{}, false
}

func mcpServerNames(servers []mcpconfig.Server) string {
	if len(servers) == 0 {
		return "none configured"
	}
	names := make([]string, len(servers))
	for i, server := range servers {
		names[i] = server.Name
	}
	return "configured: " + strings.Join(names, ", ")
}
