package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"go.harness.dev/harness/internal/agentfiles"
	"go.harness.dev/harness/internal/config"
	"go.harness.dev/harness/internal/discovery"
)

const agentsListUsage = `Usage: tempest agents list [flags]

Lists the agent types defined as files that a run in this directory offers to
the task tool: the repository root's .harness/agents, then .claude/agents, then
<agent-dir>/agents.
The first definition of a name wins. Prints each agent's name, model and
source file; an invalid file is named with the field at fault and makes the
command exit 1.

Flags:
`

// runAgents handles `tempest agents list`.
func runAgents(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "list" {
		fmt.Fprintln(stderr, "usage: tempest agents list [flags]")
		return exitUsage
	}
	var values config.FlagValues
	fs := flag.NewFlagSet("tempest agents list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), agentsListUsage)
		fs.PrintDefaults()
	}
	addConfigFlags(fs, &values)
	if err := fs.Parse(args[1:]); err != nil {
		return parseExitCode(usageFailure(err))
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, unexpectedArgument("agents list", fs.Arg(0)))
		return exitUsage
	}
	fs.Visit(func(f *flag.Flag) { markConfigFlag(f.Name, &values) })
	resolved, err := (config.Config{Flags: values}).Resolve()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "cwd error: %v\n", err)
		return 1
	}
	// A run finds the repository's agent files at its root, wherever in the
	// repository it starts; so does the listing.
	projectDir, err := discovery.ProjectDir(cwd)
	if err != nil {
		fmt.Fprintf(stderr, "cwd error: %v\n", err)
		return 1
	}
	agents, diagnostics, err := agentfiles.Load(context.Background(), agentfiles.Options{ProjectDir: projectDir, AgentDir: resolved.AgentDir(), Providers: resolved.Providers()})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	table := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "NAME\tMODEL\tSOURCE")
	for _, agent := range agents {
		model := agent.Preset.Model
		if model == "" {
			model = agentfiles.RoleDefault
		}
		fmt.Fprintf(table, "%s\t%s\t%s\n", agent.Preset.ID, model, agent.Source)
	}
	if err := table.Flush(); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	status := 0
	for _, diagnostic := range diagnostics {
		if diagnostic.Err != nil {
			fmt.Fprintln(stderr, "error: "+diagnostic.Message)
			status = 1
			continue
		}
		fmt.Fprintf(stderr, "warning: %s: %s\n", diagnostic.Path, diagnostic.Message)
	}
	return status
}
