package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"go.harness.dev/harness/internal/config"
)

const configShowUsage = `Usage: tempest config show [-json] [-key key.path] [flags]

Prints every effective setting with the layer and the file, environment
variable or flag that set it. Credentials are redacted. Layers, low to high:
default, managed, user, project, project-local, profile, env, flag, override
(-c); requirement marks a value pinned by managed requirements. Accepts the
run flags, so a run's -profile, -c and other flags show their effect.

Flags:
`

// shownSetting is one entry of `config show -json`, keyed by setting.
type shownSetting struct {
	Value  json.RawMessage `json:"value"`
	Layer  string          `json:"layer"`
	Source string          `json:"source,omitempty"`
}

// runConfig handles `tempest config show`. It prints the settings tree that
// config.Resolve decoded, so the output is what a run with the same flags uses.
func runConfig(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "show" {
		fmt.Fprintln(stderr, "usage: tempest config show [-json] [-key key.path] [flags]")
		return exitUsage
	}
	var values config.FlagValues
	fs := flag.NewFlagSet("tempest config show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), configShowUsage)
		fs.PrintDefaults()
	}
	asJSON := fs.Bool("json", false, "print one JSON object: settings keyed by dotted key, and warnings")
	key := fs.String("key", "", "show only this dotted key, or every key below it")
	addConfigFlags(fs, &values)
	if err := fs.Parse(args[1:]); err != nil {
		return parseExitCode(usageFailure(err))
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, unexpectedArgument("config show", fs.Arg(0)))
		return exitUsage
	}
	fs.Visit(func(f *flag.Flag) { markConfigFlag(f.Name, &values) })
	resolved, err := (config.Config{Flags: values}).Resolve()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	settings := resolved.Settings()
	if prefix := strings.TrimSpace(*key); prefix != "" {
		var selected []config.Setting
		for _, setting := range settings {
			if setting.Key == prefix || strings.HasPrefix(setting.Key, prefix+".") {
				selected = append(selected, setting)
			}
		}
		if len(selected) == 0 {
			fmt.Fprintf(stderr, "config show: no effective setting %q\n", prefix)
			return 1
		}
		settings = selected
	}
	warnings := resolved.Warnings()
	if *asJSON {
		shown := make(map[string]shownSetting, len(settings))
		for _, setting := range settings {
			shown[setting.Key] = shownSetting{Value: setting.Value, Layer: setting.Layer, Source: setting.Source}
		}
		if warnings == nil {
			warnings = []string{}
		}
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(struct {
			Settings map[string]shownSetting `json:"settings"`
			Warnings []string                `json:"warnings"`
		}{shown, warnings}); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	for _, setting := range settings {
		source := setting.Layer
		if setting.Source != "" {
			source += " " + setting.Source
		}
		fmt.Fprintf(stdout, "%s = %s  (%s)\n", setting.Key, setting.Value, source)
	}
	for _, warning := range warnings {
		fmt.Fprintln(stderr, "warning: "+warning)
	}
	return 0
}
