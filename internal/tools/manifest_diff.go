package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/manifest"
	"go.harness.dev/harness/internal/pathutil"
	"go.harness.dev/harness/internal/schema"
)

// manifest_diff argument defaults, matching the schema's declared defaults.
const (
	defaultManifestContextLines = 3
	defaultManifestMaxResources = 50
)

type manifestDiffInput struct {
	Before        string `json:"before"`
	After         string `json:"after"`
	ContextLines  *int   `json:"context_lines"`
	MaxResources  *int   `json:"max_resources"`
	RedactSecrets *bool  `json:"redact_secrets"`
}

// newManifestDiffTool compares two rendered manifest snapshots inside cwd.
// It reads through the working directory's os.Root and runs no process.
func newManifestDiffTool(cwd string) agent.AgentTool {
	getRoot := rootOnce(cwd)
	return agent.AgentTool{
		Tool: types.Tool{
			Name:        string(ManifestDiffTool),
			Description: "Compare two already-rendered Kubernetes manifest snapshots (a file or a directory of .yaml/.yml/.json files each) by resource identity (apiVersion, kind, namespace, name). Reports added, removed and changed resources with canonical-YAML display diffs; key order, comments and anchors are ignored, list order is not. It runs nothing: to compare chart or kustomize changes, first render both versions to files inside the working directory with bash, then pass those paths. Secret data values are redacted unless redact_secrets is false.",
			Parameters: strictObjectSchema(schema.JSON{
				"before":         schema.JSON{"type": "string", "minLength": 1},
				"after":          schema.JSON{"type": "string", "minLength": 1},
				"context_lines":  schema.JSON{"type": "integer", "minimum": 0, "maximum": manifest.MaxContextLines, "default": defaultManifestContextLines},
				"max_resources":  schema.JSON{"type": "integer", "minimum": 1, "maximum": manifest.MaxResources, "default": defaultManifestMaxResources},
				"redact_secrets": schema.JSON{"type": "boolean", "default": true},
			}, "before", "after"),
		},
		Label: string(ManifestDiffTool),
		Execute: func(ctx context.Context, _ string, params json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			var input manifestDiffInput
			if err := decodeStrict(params, &input); err != nil {
				return agent.AgentToolResult{}, err
			}
			options := manifest.Options{ContextLines: defaultManifestContextLines, MaxResources: defaultManifestMaxResources, RedactSecrets: true}
			if input.ContextLines != nil {
				options.ContextLines = *input.ContextLines
			}
			if input.MaxResources != nil {
				options.MaxResources = *input.MaxResources
			}
			if input.RedactSecrets != nil {
				options.RedactSecrets = *input.RedactSecrets
			}
			root, err := getRoot()
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			before, err := manifestSelection(cwd, "before", input.Before)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			after, err := manifestSelection(cwd, "after", input.After)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			fsys := root.FS()
			result, err := manifest.Compare(ctx, manifest.View(fsys, before), manifest.View(fsys, after), options)
			if err != nil {
				return agent.AgentToolResult{}, fmt.Errorf("manifest_diff: %w", err)
			}
			encoded, err := result.JSON()
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			return agent.AgentToolResult{Content: []types.ContentBlock{types.NewText(string(encoded))}, Details: encoded}, nil
		},
	}
}

// manifestSelection resolves a model-supplied path to its lexical name
// inside cwd. The lexical name is kept so the manifest reader sees, and
// rejects, any symlink along the way instead of its silent target.
func manifestSelection(cwd, side, requested string) (string, error) {
	if requested == "" {
		return "", fmt.Errorf("manifest_diff: %s is required", side)
	}
	if strings.ContainsRune(requested, 0) {
		return "", fmt.Errorf("manifest_diff: %s contains a NUL byte", side)
	}
	if strings.Contains(requested, "://") {
		return "", fmt.Errorf("manifest_diff: %s must be a local path, not a URL or resource", side)
	}
	relative, err := rootRelPath(cwd, pathutil.ResolveToCwd(requested, cwd))
	if err != nil {
		return "", fmt.Errorf("manifest_diff: %s: %w", side, err)
	}
	return relative, nil
}
