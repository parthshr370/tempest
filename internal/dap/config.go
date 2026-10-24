package dap

import (
	"encoding/json"
	"errors"
	"os/exec"
	"slices"
	"sort"
	"strings"
)

// Adapter describes an operator-trusted adapter executable and transport.
type Adapter struct {
	Command   string                     `json:"command"`
	Args      []string                   `json:"args,omitempty"`
	Transport string                     `json:"transport,omitempty"`
	Launch    map[string]json.RawMessage `json:"launch,omitempty"`
	Attach    map[string]json.RawMessage `json:"attach,omitempty"`
}

// Defaults uses Delve's reverse connection so no TCP port reservation races exist.
func Defaults() map[string]Adapter {
	return map[string]Adapter{
		"dlv":     {Command: "dlv", Args: []string{"dap", "--client-addr={address}"}, Transport: "reverse_tcp", Launch: map[string]json.RawMessage{"mode": json.RawMessage(`"debug"`), "stopOnEntry": json.RawMessage(`true`)}, Attach: map[string]json.RawMessage{"mode": json.RawMessage(`"local"`)}},
		"debugpy": {Command: "debugpy-adapter", Launch: map[string]json.RawMessage{"stopOnEntry": json.RawMessage(`true`), "justMyCode": json.RawMessage(`false`)}},
	}
}

// Resolve overlays explicit settings without starting an adapter.
func Resolve(overrides map[string]Adapter) (map[string]Adapter, error) {
	adapters := Defaults()
	for name, adapter := range overrides {
		defaults := adapters[name]
		if adapter.Command == "" {
			adapter.Command = defaults.Command
		}
		if adapter.Args == nil {
			adapter.Args = defaults.Args
		}
		adapter.Args = slices.Clone(adapter.Args)
		if adapter.Transport == "" {
			adapter.Transport = defaults.Transport
		}
		adapter.Launch = mergeAdapterArguments(defaults.Launch, adapter.Launch)
		adapter.Attach = mergeAdapterArguments(defaults.Attach, adapter.Attach)
		if name == "" || adapter.Command == "" {
			return nil, errors.New("debug adapters require a name and command")
		}
		if adapter.Transport != "" && adapter.Transport != "stdio" && adapter.Transport != "reverse_tcp" {
			return nil, errors.New("debug adapter transport must be stdio or reverse_tcp")
		}
		if adapter.Transport == "reverse_tcp" && !slices.ContainsFunc(adapter.Args, func(arg string) bool { return strings.Contains(arg, "{address}") }) {
			return nil, errors.New("reverse_tcp debug adapter args must contain {address}")
		}
		adapters[name] = adapter
	}
	return adapters, nil
}

func mergeAdapterArguments(defaults, overrides map[string]json.RawMessage) map[string]json.RawMessage {
	if defaults == nil && overrides == nil {
		return nil
	}
	result := make(map[string]json.RawMessage, len(defaults)+len(overrides))
	for key, value := range defaults {
		result[key] = slices.Clone(value)
	}
	for key, value := range overrides {
		result[key] = slices.Clone(value)
	}
	return result
}

// Availability reports configured commands, including missing optional adapters.
type Availability struct {
	Name      string
	Command   string
	Available bool
}

// Available doesn't execute adapters or import Python modules.
func Available(overrides map[string]Adapter) ([]Availability, error) {
	adapters, err := Resolve(overrides)
	if err != nil {
		return nil, err
	}
	var result []Availability
	for name, adapter := range adapters {
		_, err := exec.LookPath(adapter.Command)
		result = append(result, Availability{Name: name, Command: adapter.Command, Available: err == nil})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}
