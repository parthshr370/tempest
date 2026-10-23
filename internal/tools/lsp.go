package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/editdiff"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/lsp"
	"go.harness.dev/harness/internal/pathutil"
	"go.harness.dev/harness/internal/schema"
	"go.harness.dev/harness/internal/toolio"
)

// lsp_* argument bounds and defaults, matching the declared schemas.
const (
	maxLSPResults             = 200
	defaultLSPLocationResults = 50
	defaultLSPDiagnostics     = 100
	maxLSPNewNameRunes        = 256
	maxLSPWaitMS              = 10_000
	defaultLSPWaitMS          = 2000
	defaultLSPSeverity        = "all"
)

// lspServerNames are the routes a model may select explicitly.
var lspServerNames = []string{"go", "yaml", "terraform", "helm"}

// lspSeverities are the exact diagnostics categories a model may filter on.
var lspSeverities = []string{"all", "error", "warning", "information", "hint"}

// Workspace bounds. A single document is capped well above the language
// client's own per-file limit so the client reports its limit, not this one.
const (
	maxWorkspaceFileBytes   = 8 << 20
	maxSnapshotFiles        = 256
	maxSnapshotBytes        = 16 << 20
	maxSnapshotVisitedEntry = 8192
)

// snapshotExtensions are the source types any built-in route can open.
var snapshotExtensions = map[string]bool{".go": true, ".yaml": true, ".yml": true, ".tf": true, ".tfvars": true, ".tpl": true}

// lspWorkspace is the contained filesystem view a language client reads
// through. Reads go through an os.Root on the resolved cwd and hold the same
// per-file mutation lock as edit and write.
type lspWorkspace struct {
	cwd   string
	root  string
	files *os.Root
	queue *toolio.FileMutationQueue
}

// NewLSPWorkspace opens the contained workspace for cwd. queue must be the
// queue the session's edit and write tools use; nil selects the same shared
// default queue those tools fall back to.
func NewLSPWorkspace(cwd string, queue *toolio.FileMutationQueue) (lsp.Workspace, error) {
	absolute, err := filepath.Abs(cwd)
	if err != nil {
		return nil, err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, err
	}
	files, err := os.OpenRoot(canonical)
	if err != nil {
		return nil, err
	}
	if queue == nil {
		queue = defaultMutationQueue
	}
	return &lspWorkspace{cwd: absolute, root: canonical, files: files, queue: queue}, nil
}

func (w *lspWorkspace) Root() string { return w.root }

func (w *lspWorkspace) Close() error { return w.files.Close() }

// ReadFile reads a regular file named relative to cwd or by an absolute
// path inside it, returning its canonical root-relative name.
func (w *lspWorkspace) ReadFile(ctx context.Context, name string) (lsp.Document, error) {
	canonical, err := w.resolve(name)
	if err != nil {
		return lsp.Document{}, err
	}
	return w.read(ctx, canonical)
}

// resolve maps a requested path to its canonical root-relative name. Both
// the lexical cwd and its symlink-resolved form are accepted as prefixes of
// an absolute path.
func (w *lspWorkspace) resolve(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("invalid workspace path %q", name)
	}
	absolute := pathutil.ResolveToCwd(name, w.cwd)
	_, canonical, err := rootRelPaths(w.cwd, absolute)
	if errors.Is(err, ErrOutsideRoot) && w.cwd != w.root {
		_, canonical, err = rootRelPaths(w.root, absolute)
	}
	return canonical, err
}

// read opens canonical through the root under its mutation lock.
func (w *lspWorkspace) read(ctx context.Context, canonical string) (lsp.Document, error) {
	if err := ctx.Err(); err != nil {
		return lsp.Document{}, err
	}
	return toolio.DoValue(w.queue, filepath.Join(w.root, filepath.FromSlash(canonical)), func() (lsp.Document, error) {
		if err := ctx.Err(); err != nil {
			return lsp.Document{}, err
		}
		file, err := w.files.Open(canonical)
		if err != nil {
			return lsp.Document{}, wrapOutsideRoot(canonical, err)
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return lsp.Document{}, err
		}
		if !info.Mode().IsRegular() {
			return lsp.Document{}, fmt.Errorf("%s is not a regular file", canonical)
		}
		text, err := io.ReadAll(io.LimitReader(file, maxWorkspaceFileBytes+1))
		if err != nil {
			return lsp.Document{}, err
		}
		if len(text) > maxWorkspaceFileBytes {
			return lsp.Document{}, fmt.Errorf("%s exceeds %d bytes", canonical, maxWorkspaceFileBytes)
		}
		return lsp.Document{Path: canonical, Text: text, Anchor: editdiff.ContentAnchor(text)}, nil
	})
}

// Snapshot captures every source file a built-in route can open, for rename
// target verification. .git and symlinks are skipped; exceeding a bound
// fails the whole scan rather than returning a partial view.
func (w *lspWorkspace) Snapshot(ctx context.Context) ([]lsp.Document, error) {
	var documents []lsp.Document
	visited, totalBytes := 0, 0
	walkErr := fs.WalkDir(w.files.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		visited++
		if visited > maxSnapshotVisitedEntry {
			return scopeLimit(fmt.Sprintf("workspace has more than %d entries", maxSnapshotVisitedEntry))
		}
		switch {
		case entry.Type()&fs.ModeSymlink != 0:
			return nil
		case entry.IsDir():
			if entry.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		case !entry.Type().IsRegular() || !snapshotExtensions[strings.ToLower(path.Ext(name))]:
			return nil
		}
		if len(documents) == maxSnapshotFiles {
			return scopeLimit(fmt.Sprintf("workspace has more than %d source files", maxSnapshotFiles))
		}
		document, err := w.read(ctx, name)
		if err != nil {
			return err
		}
		totalBytes += len(document.Text)
		if totalBytes > maxSnapshotBytes {
			return scopeLimit(fmt.Sprintf("workspace source exceeds %d bytes", maxSnapshotBytes))
		}
		documents = append(documents, document)
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return documents, nil
}

func scopeLimit(detail string) error {
	return &lsp.Error{Code: lsp.CodeRenameScopeLimit, Detail: detail}
}

// lspPositionInput holds the arguments shared by the position-based tools.
// Each tool decodes into its own struct, so unknown arguments are rejected
// per schema.
type lspPositionInput struct {
	Path   string  `json:"path"`
	Line   *int    `json:"line"`
	Column *int    `json:"column"`
	Server *string `json:"server"`
}

type lspDefinitionInput struct {
	lspPositionInput
	MaxResults *int `json:"max_results"`
}

type lspReferencesInput struct {
	lspPositionInput
	IncludeDeclaration *bool `json:"include_declaration"`
	MaxResults         *int  `json:"max_results"`
}

type lspRenameInput struct {
	lspPositionInput
	NewName *string `json:"new_name"`
}

type lspDiagnosticsInput struct {
	Path       string  `json:"path"`
	Server     *string `json:"server"`
	Severity   *string `json:"severity"`
	WaitMS     *int    `json:"wait_ms"`
	MaxResults *int    `json:"max_results"`
}

// lspToolSpec describes one lsp_* tool: its model-facing text, schema
// properties and argument decoder.
type lspToolSpec struct {
	description string
	properties  schema.JSON
	required    []string
	decode      func(json.RawMessage) (lsp.Query, error)
}

func positionProperties() schema.JSON {
	return schema.JSON{
		"path":   schema.JSON{"type": "string", "minLength": 1},
		"line":   schema.JSON{"type": "integer", "minimum": 1},
		"column": schema.JSON{"type": "integer", "minimum": 1},
		"server": schema.JSON{"type": "string", "enum": lspServerNames},
	}
}

func maxResultsProperty(defaultValue int) schema.JSON {
	return schema.JSON{"type": "integer", "minimum": 1, "maximum": maxLSPResults, "default": defaultValue}
}

func lspToolSpecs() map[ToolName]lspToolSpec {
	definition := positionProperties()
	definition["max_results"] = maxResultsProperty(defaultLSPLocationResults)
	references := positionProperties()
	references["include_declaration"] = schema.JSON{"type": "boolean", "default": false}
	references["max_results"] = maxResultsProperty(defaultLSPLocationResults)
	rename := positionProperties()
	rename["new_name"] = schema.JSON{"type": "string", "minLength": 1, "maxLength": maxLSPNewNameRunes}
	positionText := "line and column are one-based; column counts Unicode characters from the start of the line, as in numbered read output."
	return map[ToolName]lspToolSpec{
		LSPDefinitionTool: {
			description: "Find where the symbol at a source position is defined, using the language server for the file. " + positionText + " Returns locations inside the working directory; locations outside it (such as dependency sources) are counted, not shown.",
			properties:  definition, required: []string{"path", "line", "column"},
			decode: func(params json.RawMessage) (lsp.Query, error) {
				var input lspDefinitionInput
				if err := decodeStrict(params, &input); err != nil {
					return lsp.Query{}, err
				}
				query, err := input.query(lsp.MethodDefinition)
				if err != nil {
					return lsp.Query{}, err
				}
				query.MaxResults, err = boundedInt("max_results", input.MaxResults, defaultLSPLocationResults, 1, maxLSPResults)
				return query, err
			},
		},
		LSPReferencesTool: {
			description: "Find references to the symbol at a source position, using the language server for the file. " + positionText + " Set include_declaration to also list the declaration.",
			properties:  references, required: []string{"path", "line", "column"},
			decode: func(params json.RawMessage) (lsp.Query, error) {
				var input lspReferencesInput
				if err := decodeStrict(params, &input); err != nil {
					return lsp.Query{}, err
				}
				query, err := input.query(lsp.MethodReferences)
				if err != nil {
					return lsp.Query{}, err
				}
				query.IncludeDeclaration = input.IncludeDeclaration != nil && *input.IncludeDeclaration
				query.MaxResults, err = boundedInt("max_results", input.MaxResults, defaultLSPLocationResults, 1, maxLSPResults)
				return query, err
			},
		},
		LSPRenameTool: {
			description: "Plan a rename of the symbol at a source position across the workspace. " + positionText + " It changes no file: it returns, per file, an anchor and exact edits (old_text/new_text/anchor) to apply with the edit tool, one file per edit call. Files are applied independently; if one fails because it changed, recompute the plan.",
			properties:  rename, required: []string{"path", "line", "column", "new_name"},
			decode: func(params json.RawMessage) (lsp.Query, error) {
				var input lspRenameInput
				if err := decodeStrict(params, &input); err != nil {
					return lsp.Query{}, err
				}
				query, err := input.query(lsp.MethodRename)
				if err != nil {
					return lsp.Query{}, err
				}
				if input.NewName == nil {
					return lsp.Query{}, errors.New("new_name is required")
				}
				if runes := utf8.RuneCountInString(*input.NewName); runes < 1 || runes > maxLSPNewNameRunes {
					return lsp.Query{}, fmt.Errorf("new_name must be 1 to %d characters", maxLSPNewNameRunes)
				}
				query.NewName = *input.NewName
				return query, nil
			},
		},
		LSPDiagnosticsTool: {
			description: "Report the language server's diagnostics for one file after synchronizing it with disk. Results state whether they are fresh for the current file version; pending or unversioned results are not proof the file is clean. severity filters one exact category. wait_ms bounds how long to wait for a fresh report.",
			properties: schema.JSON{
				"path":        schema.JSON{"type": "string", "minLength": 1},
				"server":      schema.JSON{"type": "string", "enum": lspServerNames},
				"severity":    schema.JSON{"type": "string", "enum": lspSeverities, "default": defaultLSPSeverity},
				"wait_ms":     schema.JSON{"type": "integer", "minimum": 0, "maximum": maxLSPWaitMS, "default": defaultLSPWaitMS},
				"max_results": maxResultsProperty(defaultLSPDiagnostics),
			},
			required: []string{"path"},
			decode: func(params json.RawMessage) (lsp.Query, error) {
				var input lspDiagnosticsInput
				if err := decodeStrict(params, &input); err != nil {
					return lsp.Query{}, err
				}
				query := lsp.Query{Method: lsp.MethodDiagnostics, Severity: defaultLSPSeverity}
				var err error
				if query.Path, err = lspPath(input.Path); err != nil {
					return lsp.Query{}, err
				}
				if query.Server, err = lspServer(input.Server); err != nil {
					return lsp.Query{}, err
				}
				if input.Severity != nil {
					if !slices.Contains(lspSeverities, *input.Severity) {
						return lsp.Query{}, fmt.Errorf("severity must be one of %s", strings.Join(lspSeverities, ", "))
					}
					query.Severity = *input.Severity
				}
				waitMS, err := boundedInt("wait_ms", input.WaitMS, defaultLSPWaitMS, 0, maxLSPWaitMS)
				if err != nil {
					return lsp.Query{}, err
				}
				query.Wait = time.Duration(waitMS) * time.Millisecond
				query.MaxResults, err = boundedInt("max_results", input.MaxResults, defaultLSPDiagnostics, 1, maxLSPResults)
				return query, err
			},
		},
	}
}

// query validates the shared path/position/server arguments.
func (input lspPositionInput) query(method string) (lsp.Query, error) {
	query := lsp.Query{Method: method}
	var err error
	if query.Path, err = lspPath(input.Path); err != nil {
		return lsp.Query{}, err
	}
	if query.Server, err = lspServer(input.Server); err != nil {
		return lsp.Query{}, err
	}
	if input.Line == nil || input.Column == nil {
		return lsp.Query{}, errors.New("line and column are required")
	}
	if *input.Line < 1 || *input.Column < 1 {
		return lsp.Query{}, errors.New("line and column are one-based and must be at least 1")
	}
	query.Position = lsp.Position{Line: *input.Line, Column: *input.Column}
	return query, nil
}

// lspPath accepts a literal workspace filename: not empty, no NUL, and not
// a URI or resource such as file:// or skill://. Containment is enforced
// when the workspace opens it.
func lspPath(requested string) (string, error) {
	switch {
	case requested == "":
		return "", errors.New("path is required")
	case strings.ContainsRune(requested, 0):
		return "", errors.New("path contains a NUL byte")
	case strings.Contains(requested, "://"):
		return "", errors.New("path must be a workspace file path, not a URI")
	}
	return requested, nil
}

func lspServer(requested *string) (string, error) {
	if requested == nil {
		return "", nil
	}
	if !slices.Contains(lspServerNames, *requested) {
		return "", fmt.Errorf("server must be one of %s", strings.Join(lspServerNames, ", "))
	}
	return *requested, nil
}

// boundedInt applies a default and checks an integer argument's range.
func boundedInt(name string, value *int, defaultValue, minimum, maximum int) (int, error) {
	if value == nil {
		return defaultValue, nil
	}
	if *value < minimum || *value > maximum {
		return 0, fmt.Errorf("%s must be between %d and %d", name, minimum, maximum)
	}
	return *value, nil
}

// newLSPTool builds one lsp_* tool over a session's manager. The result is
// the manager's normalized JSON, once as text and once as details.
func newLSPTool(name ToolName, manager *lsp.Manager) agent.AgentTool {
	spec := lspToolSpecs()[name]
	return agent.AgentTool{
		Tool: types.Tool{
			Name:        string(name),
			Description: spec.description,
			Parameters:  strictObjectSchema(spec.properties, spec.required...),
		},
		Label: string(name),
		Execute: func(ctx context.Context, _ string, params json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			query, err := spec.decode(params)
			if err != nil {
				return agent.AgentToolResult{}, fmt.Errorf("%s: %w", name, err)
			}
			result, err := manager.Query(ctx, query)
			if err != nil {
				return agent.AgentToolResult{}, lspToolError(name, err)
			}
			return agent.AgentToolResult{Content: []types.ContentBlock{types.NewText(string(result.Value))}, Details: result.Value}, nil
		},
	}
}

// lspToolError keeps the stable error code first and adds the retry hint a
// model needs after a crash cooldown or an open circuit.
func lspToolError(name ToolName, err error) error {
	var lspErr *lsp.Error
	if errors.As(err, &lspErr) && lspErr.RetryAfter > 0 {
		return fmt.Errorf("%s: %w (retry_after_ms: %d)", name, err, lspErr.RetryAfter.Milliseconds())
	}
	return fmt.Errorf("%s: %w", name, err)
}

// strictObjectSchema is an object schema that rejects undeclared arguments.
func strictObjectSchema(properties schema.JSON, required ...string) json.RawMessage {
	document := schema.JSON{"type": "object", "additionalProperties": false, "properties": properties, "required": required}
	encoded, err := json.Marshal(document)
	if err != nil {
		panic(fmt.Sprintf("tool schema is not encodable: %v", err))
	}
	return encoded
}
