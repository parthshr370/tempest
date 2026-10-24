package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"go.harness.dev/harness/tempest/extension"
)

func loadExtensionProcesses(path string, stderr io.Writer) ([]extension.ProcessOptions, error) {
	if path == "" {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var processes []extension.ProcessOptions
	if err := decoder.Decode(&processes); err != nil {
		return nil, fmt.Errorf("extension config: %w", err)
	}
	if processes == nil {
		return nil, fmt.Errorf("extension config must contain one JSON array")
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("extension config must contain one JSON array")
	}
	seen := make(map[string]bool, len(processes))
	for i := range processes {
		process := &processes[i]
		if process.ID == "" || len(process.Argv) == 0 || process.Argv[0] == "" {
			return nil, fmt.Errorf("extension config entry %d requires ID and Argv", i)
		}
		if seen[process.ID] {
			return nil, fmt.Errorf("extension config repeats ID %q", process.ID)
		}
		seen[process.ID] = true
		process.Stderr = stderr
	}
	return processes, nil
}
