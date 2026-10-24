package config

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// EnvironmentProcess is trusted executable configuration, never journal state.
type EnvironmentProcess struct {
	Argv    []string `json:"argv"`
	Env     []string `json:"env,omitempty"`
	Cwd     string   `json:"cwd,omitempty"`
	Timeout string   `json:"timeout,omitempty"`
}

func resolveEnvironments(flags FlagValues, lookup func(string) string, settings fileSettings) (string, map[string]EnvironmentProcess, error) {
	name := resolveString(flags.Environment, lookup("HARNESS_ENVIRONMENT"), settings.Environment, "")
	processes := make(map[string]EnvironmentProcess, len(settings.Environments))
	for id, process := range settings.Environments {
		processes[id] = process
	}
	command := resolveString(flags.EnvironmentCommand, lookup("HARNESS_ENVIRONMENT_COMMAND"), nil, "")
	if command != "" {
		if name == "" {
			return "", nil, &Error{Field: "environment_command", Err: fmt.Errorf("environment name required")}
		}
		var argv []string
		if err := json.Unmarshal([]byte(command), &argv); err != nil {
			return "", nil, &Error{Field: "environment_command", Err: err}
		}
		processes[name] = EnvironmentProcess{Argv: argv}
	}
	for id, process := range processes {
		if strings.TrimSpace(id) == "" || strings.HasPrefix(id, "local:") || len(process.Argv) == 0 || process.Argv[0] == "" {
			return "", nil, &Error{Field: "environments", Err: fmt.Errorf("operator name and nonempty argv required for %q", id)}
		}
		if process.Timeout != "" {
			duration, err := time.ParseDuration(process.Timeout)
			if err != nil || duration <= 0 {
				return "", nil, &Error{Field: "environments", Err: fmt.Errorf("invalid timeout for %q", id)}
			}
		}
	}
	if name != "" {
		if _, ok := processes[name]; !ok {
			return "", nil, &Error{Field: "environment", Err: fmt.Errorf("environment %q is not operator-configured", name)}
		}
	}
	return name, processes, nil
}

// Environment returns the selected operator environment name, or empty for Local.
func (c *ResolvedConfig) Environment() string { return c.environment }

// Environments returns detached executable launch definitions.
func (c *ResolvedConfig) Environments() map[string]EnvironmentProcess {
	processes := make(map[string]EnvironmentProcess, len(c.environments))
	for id, process := range c.environments {
		process.Argv = append([]string(nil), process.Argv...)
		if process.Env != nil {
			process.Env = append([]string{}, process.Env...)
		}
		processes[id] = process
	}
	return processes
}
