// Package tools defines the coding agent's tool set: Read, Write, Edit
// (MultiEdit), Bash, Grep backed by rg, Find backed by fd, LS, TodoWrite, Task (subagents),
// WebSearch, and WebFetch (the last two gated off by default). Each tool is
// an [agent.AgentTool] backed by local FS/exec calls. File mutations are
// serialized per real path via [toolio.FileMutationQueue]; reads are
// parallel-safe.
package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/document"
	"go.harness.dev/harness/internal/editdiff"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/hashline"
	"go.harness.dev/harness/internal/irc"
	"go.harness.dev/harness/internal/lsp"
	"go.harness.dev/harness/internal/progress"
	"go.harness.dev/harness/internal/refs"
	"go.harness.dev/harness/internal/schema"
	"go.harness.dev/harness/internal/skills"
	"go.harness.dev/harness/internal/toolio"
	"go.harness.dev/harness/internal/truncate"
	"go.harness.dev/harness/internal/work"
)

// ToolName identifies a coding tool by its canonical name.
type ToolName string

// ReadTool reads a file at a path, with optional offset and limit inputs.
// BashTool runs a command from the working directory, with an optional timeout.
// EditTool replaces exact text in a file through path and edits inputs.
// HashlineTool applies line-anchored PUT/CUT/REM/MV patches to existing files.
// WriteTool writes content to a path, creating parent directories when needed.
// GrepTool searches file contents with pattern, path, glob, and matching options.
// FindTool finds files by pattern, with optional path and limit inputs.
// LsTool lists a directory at path, with an optional entry limit.
// TodoTool updates the session todo list from todos input.
// TaskTool delegates a task to a configured subagent tool.
// SkillTool loads a bundled skill by name.
// WebSearch searches through the configured endpoint when web access is enabled.
// WebFetch fetches an HTTP(S) URL when web access is enabled.
// AttachmentTool reads resolved session attachments when an attachment reader is configured.
// LSPDefinitionTool, LSPReferencesTool, LSPRenameTool and LSPDiagnosticsTool
// query an enabled language server; rename only returns an edit plan.
// ManifestDiffTool compares two rendered local manifest trees by resource.
// MessageTool sends a message to the parent, a running child or all peers.
//
// These names keep tool lookup stable across prompts, clients, and persisted state.
const (
	ReadTool       ToolName = "read"
	BashTool       ToolName = "bash"
	EditTool       ToolName = "edit"
	HashlineTool   ToolName = "hashline"
	WriteTool      ToolName = "write"
	GrepTool       ToolName = "grep"
	FindTool       ToolName = "find"
	LsTool         ToolName = "ls"
	TodoTool       ToolName = "todo_write"
	TaskTool       ToolName = "task"
	SkillTool      ToolName = "skill"
	WebSearch      ToolName = "web_search"
	WebFetch       ToolName = "web_fetch"
	AttachmentTool ToolName = "attachment"
	JobsTool       ToolName = "jobs"
	WaitTool       ToolName = "wait"
	CancelTool     ToolName = "cancel"
	LogsTool       ToolName = "logs"
	MessageTool    ToolName = "message"

	LSPDefinitionTool  ToolName = "lsp_definition"
	LSPReferencesTool  ToolName = "lsp_references"
	LSPRenameTool      ToolName = "lsp_rename"
	LSPDiagnosticsTool ToolName = "lsp_diagnostics"
	ManifestDiffTool   ToolName = "manifest_diff"
)

// AllToolNames is the set of every known ToolName. Configured tools can
// never shadow these names, including the lsp_* names when LSP is disabled.
var AllToolNames = map[ToolName]bool{ReadTool: true, BashTool: true, EditTool: true, HashlineTool: true, WriteTool: true, GrepTool: true, FindTool: true, LsTool: true, TodoTool: true, TaskTool: true, SkillTool: true, WebSearch: true, WebFetch: true, AttachmentTool: true, JobsTool: true, WaitTool: true, CancelTool: true, LogsTool: true, MessageTool: true, LSPDefinitionTool: true, LSPReferencesTool: true, LSPRenameTool: true, LSPDiagnosticsTool: true, ManifestDiffTool: true}

// LSPToolNames are the language-server tools, all read-tier calculations.
var LSPToolNames = []ToolName{LSPDefinitionTool, LSPReferencesTool, LSPRenameTool, LSPDiagnosticsTool}

// ToolsOptions configures tool creation with optional overrides for shell,
// web access, progress tracking, and pre-configured tools.
type ToolsOptions struct {
	MutationQueue *toolio.FileMutationQueue
	ShellPath     string
	CommandPrefix string
	// BashTimeout defaults to ten minutes when unset.
	BashTimeout     time.Duration
	TodoStore       *progress.Store
	TaskTool        agent.AgentTool
	ConfiguredTools []agent.AgentTool
	Skills          []skills.Skill
	// Refs resolves skill://, job://, history:// and local:// for read,
	// grep, write, edit and bash. A nil resolver reports each URL as
	// unavailable.
	Refs       *refs.Resolver
	EnableWeb  bool
	HTTPClient *http.Client
	ResolveIP  func(context.Context, string) ([]net.IP, error)
	SearchURL  string
	// AttachmentRegistry + AttachmentReader enable the read-only attachment
	// tool over the session's resolved attachments. Both must be set for the
	// tool to register. Sanitize is applied to returned document text.
	AttachmentRegistry *document.AttachmentRegistry
	AttachmentReader   *document.CacheRootBlobReader
	Sanitize           func(string) string
	// Work is installed only on a journaled root session.
	Work  work.Controller
	Child bool
	// Messenger registers the message tool: the agent's endpoint on its
	// session tree's message bus.
	Messenger *irc.Endpoint
	// SessionDataDir is the journaled session's private directory
	// (<journal>.d). todo_write checkpoints progress.md there, bash spills
	// truncated output under tool-output/, and read may open exactly those
	// files plus the session's job artifacts under jobs/.
	SessionDataDir string
	// LSP registers the lsp_* tools when non-nil. Its owner closes it.
	LSP *lsp.Manager
	// OnFileChanged is called by write and edit after a successful disk
	// write, while the file's mutation lock is still held, with the
	// canonical slash-separated path relative to the resolved cwd. It must
	// only record the change: no I/O, waiting or error.
	OnFileChanged func(canonicalPath string)
	// Hashline is the session's hashline state: the file versions read,
	// grep, edit, write and hashline displayed (their [path#TAG] tags) and
	// the named clipboard registers. AllTools and the tool-list helpers
	// create one per call when it is nil, so the tools they build share it.
	Hashline *hashline.Store
}

// These limits keep web responses bounded and prevent impractical command timeouts.
const (
	defaultWebMaxBytes = 200 * 1024
	maxWebFetchBytes   = 5 * 1024 * 1024
	defaultWebTimeout  = 30 * time.Second
	maxTimeoutSeconds  = 2147483
)

// readToolInput holds the raw request until selector parsing and defaults have run.
type readToolInput struct {
	Path   string `json:"path"`
	Offset *int   `json:"offset"`
	Limit  *int   `json:"limit"`
	Mode   string `json:"mode"`
}

// editToolInput holds the normalized edit form after legacy arguments become edits.
type editToolInput struct {
	Path  string          `json:"path"`
	Edits []editdiff.Edit `json:"edits"`
}

// readToolDetails gives clients explicit summary and binary flags instead of making them infer output mode from text.
type readToolDetails struct {
	Truncation truncate.Result `json:"truncation"`
	Summary    bool            `json:"summary,omitempty"`
	Binary     bool            `json:"binary,omitempty"`
}

// editToolDetails keeps edit previews and the first changed line available to clients.
type editToolDetails struct {
	Diff             string `json:"diff"`
	Patch            string `json:"patch"`
	FirstChangedLine *int   `json:"firstChangedLine,omitempty"`
}

// readPathSelector keeps a parsed line range separate from a literal colon in a path.
type readPathSelector struct {
	Path  string
	Start int
	End   int
	Set   bool
}

// readSelectorError preserves why a numeric selector was rejected instead of treating it as part of a filename.
type readSelectorError struct {
	Selector string
	Reason   string
}

func (e readSelectorError) Error() string {
	return fmt.Sprintf("invalid line-range selector %q: %s", e.Selector, e.Reason)
}

var defaultMutationQueue = toolio.NewFileMutationQueue()

// NewTool creates the coding tool named toolName. It returns an error for
// unknown tool names and for tools whose prerequisites aren't met (e.g.
// web_search when EnableWeb is false).
func NewTool(toolName ToolName, cwd string, options ToolsOptions) (agent.AgentTool, error) {
	switch toolName {
	case ReadTool:
		return newReadTool(cwd, options), nil
	case BashTool:
		return newBashTool(cwd, options), nil
	case EditTool:
		return newEditTool(cwd, options), nil
	case WriteTool:
		return newWriteTool(cwd, options), nil
	case GrepTool:
		return newGrepTool(cwd, options), nil
	case FindTool:
		return newFindTool(cwd), nil
	case LsTool:
		return newLsTool(cwd), nil
	case TodoTool:
		return newTodoTool(options), nil
	case TaskTool:
		if options.TaskTool.Execute == nil {
			return agent.AgentTool{}, fmt.Errorf("task tool not configured")
		}
		return options.TaskTool, nil
	case JobsTool, WaitTool, CancelTool, LogsTool:
		if options.Work == nil || options.Child {
			return agent.AgentTool{}, fmt.Errorf("background management requires a durable root session")
		}
		return newManagementTool(toolName, options.Work), nil
	case MessageTool:
		if options.Messenger == nil {
			return agent.AgentTool{}, fmt.Errorf("message requires a session with task children")
		}
		return newMessageTool(options.Messenger), nil
	case SkillTool:
		if len(options.Skills) == 0 {
			return agent.AgentTool{}, fmt.Errorf("skill tool has no skills")
		}
		return newSkillTool(options.Skills), nil
	case WebSearch:
		if !options.EnableWeb {
			return agent.AgentTool{}, fmt.Errorf("web_search tool not enabled")
		}
		return newWebSearchTool(options), nil
	case WebFetch:
		if !options.EnableWeb {
			return agent.AgentTool{}, fmt.Errorf("web_fetch tool not enabled")
		}
		return newWebFetchTool(options), nil
	case LSPDefinitionTool, LSPReferencesTool, LSPRenameTool, LSPDiagnosticsTool:
		if options.LSP == nil {
			return agent.AgentTool{}, fmt.Errorf("%s requires language servers to be enabled", toolName)
		}
		return newLSPTool(toolName, options.LSP), nil
	case ManifestDiffTool:
		return newManifestDiffTool(cwd), nil
	case HashlineTool:
		return newHashlineTool(cwd, options), nil
	default:
		return agent.AgentTool{}, fmt.Errorf("Unknown tool name: %s", toolName)
	}
}

// CodingTools returns the core read/write/bash/edit tools.
func CodingTools(cwd string, options ToolsOptions) []agent.AgentTool {
	return mustTools(cwd, options, ReadTool, BashTool, EditTool, WriteTool)
}

// ReadOnlyTools returns the read-only tools: read, grep, find, and ls.
func ReadOnlyTools(cwd string, options ToolsOptions) []agent.AgentTool {
	return mustTools(cwd, options, ReadTool, GrepTool, FindTool, LsTool)
}

// AllTools creates every known tool plus any pre-configured tools from
// options, returning only the ones that were successfully created.
func AllTools(cwd string, options ToolsOptions) map[ToolName]agent.AgentTool {
	options = withHashlineStore(options)
	out := map[ToolName]agent.AgentTool{}
	for _, name := range []ToolName{ReadTool, BashTool, EditTool, HashlineTool, WriteTool, GrepTool, FindTool, LsTool, TodoTool, TaskTool, SkillTool, WebSearch, WebFetch, JobsTool, WaitTool, CancelTool, LogsTool, MessageTool, LSPDefinitionTool, LSPReferencesTool, LSPRenameTool, LSPDiagnosticsTool, ManifestDiffTool} {
		tool, err := NewTool(name, cwd, options)
		if err == nil {
			out[name] = tool
		}
	}
	for _, tool := range options.ConfiguredTools {
		if tool.Name == "" || tool.Execute == nil {
			continue
		}
		if AllToolNames[ToolName(tool.Name)] {
			// A configured tool must not shadow a built-in name. The permission
			// gate classifies tier by name, so an impostor sharing a built-in
			// name would inherit that name's trusted tier. Keep the built-in.
			continue
		}
		out[ToolName(tool.Name)] = tool
	}
	if options.AttachmentRegistry != nil && options.AttachmentReader != nil {
		out[AttachmentTool] = newAttachmentTool(options.AttachmentRegistry, options.AttachmentReader, options.Sanitize)
	}
	return out
}

// newSkillTool builds the on-demand skill loader from canonical metadata.
func newSkillTool(items []skills.Skill) agent.AgentTool {
	return agent.AgentTool{
		Tool: types.Tool{
			Name:        string(SkillTool),
			Description: "Load a bundled skill's full instructions by name. Call this when a listed skill matches the task; the skill body is returned as the tool result.",
			Parameters: schema.ObjectRequired(schema.JSON{
				"name":    schema.JSON{"type": "string", "description": "the skill name to load"},
				"command": schema.String(),
			}, []string{}),
		},
		Label: string(SkillTool),
		Execute: func(ctx context.Context, toolCallID string, params json.RawMessage, onUpdate agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			var input struct {
				Name    string `json:"name"`
				Command string `json:"command"`
			}
			if err := decodeParams(params, &input); err != nil {
				return agent.AgentToolResult{}, err
			}
			name := strings.TrimSpace(input.Name)
			if name == "" {
				name = strings.TrimSpace(input.Command)
			}
			if name == "" {
				return agent.AgentToolResult{}, fmt.Errorf("skill tool requires a name")
			}
			skill, ok := skills.Find(items, name)
			if !ok {
				names := make([]string, 0, len(items))
				for _, item := range items {
					names = append(names, item.Name)
				}
				return textResult(fmt.Sprintf("Unknown skill %q. Available: %s", name, strings.Join(names, ", ")), map[string]any{"skill": name, "found": false}), nil
			}
			body, err := skills.LoadBody(ctx, skill)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			return textResult(body, map[string]any{"skill": skill.Name, "found": true}), nil
		},
	}
}

// newWebFetchTool builds the HTTP(S) fetcher from options. It accepts url and
// optional max_bytes, and NewTool returns it only when web access is enabled.
func newWebFetchTool(options ToolsOptions) agent.AgentTool {
	client := webClient(options)
	resolveIP := webResolver(options)
	return agent.AgentTool{
		Tool: types.Tool{
			Name:        string(WebFetch),
			Description: "Fetch a URL over HTTP(S). This tool is gated and only registered when web access is explicitly enabled.",
			Parameters: schema.Object(schema.JSON{
				"url":       schema.String(),
				"max_bytes": schema.Number(),
			}, "url"),
		},
		Label: string(WebFetch),
		Execute: func(ctx context.Context, _ string, params json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			var input struct {
				URL      string `json:"url"`
				MaxBytes int    `json:"max_bytes"`
			}
			if err := decodeParams(params, &input); err != nil {
				return agent.AgentToolResult{}, err
			}
			maxBytes := clampWebMaxBytes(input.MaxBytes)
			parsed, err := validateWebURL(ctx, input.URL, resolveIP)
			if err != nil {
				return agent.AgentToolResult{}, fmt.Errorf("web_fetch rejected URL: %w", err)
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			resp, err := client.Do(req)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			defer resp.Body.Close()
			body, truncated, err := readLimited(resp.Body, maxBytes)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			text := fmt.Sprintf("Status: %s\nURL: %s\n\n%s", resp.Status, parsed.String(), body)
			if truncated {
				text += fmt.Sprintf("\n\n[Output truncated at %d bytes]", maxBytes)
			}
			return textResult(text, map[string]any{"status": resp.StatusCode, "url": parsed.String(), "truncated": truncated}), nil
		},
	}
}

// newWebSearchTool builds the configured HTTP search client from options. It
// accepts query and optional numResults, and NewTool returns it only when web access is enabled.
func newWebSearchTool(options ToolsOptions) agent.AgentTool {
	client := webClient(options)
	resolveIP := webResolver(options)
	searchURL := options.SearchURL
	return agent.AgentTool{
		Tool: types.Tool{
			Name:        string(WebSearch),
			Description: "Search the web through a configured HTTP search endpoint. This tool is gated and only registered when web access is explicitly enabled.",
			Parameters: schema.Object(schema.JSON{
				"query":       schema.String(),
				"num_results": schema.Number(),
			}, "query"),
		},
		Label: string(WebSearch),
		Execute: func(ctx context.Context, _ string, params json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			var input struct {
				Query      string `json:"query"`
				NumResults int    `json:"num_results"`
			}
			if err := decodeParams(params, &input); err != nil {
				return agent.AgentToolResult{}, err
			}
			if strings.TrimSpace(input.Query) == "" {
				return agent.AgentToolResult{}, fmt.Errorf("web_search query is required")
			}
			if searchURL == "" {
				return agent.AgentToolResult{}, fmt.Errorf("web_search requires ToolsOptions.SearchURL")
			}
			if input.NumResults <= 0 {
				input.NumResults = 8
			}
			parsed, err := validateWebURL(ctx, searchURL, resolveIP)
			if err != nil {
				return agent.AgentToolResult{}, fmt.Errorf("web_search rejected endpoint: %w", err)
			}
			query := parsed.Query()
			query.Set("q", input.Query)
			query.Set("limit", fmt.Sprintf("%d", input.NumResults))
			parsed.RawQuery = query.Encode()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			resp, err := client.Do(req)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			defer resp.Body.Close()
			body, truncated, err := readLimited(resp.Body, 200*1024)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			text := fmt.Sprintf("Status: %s\nQuery: %s\n\n%s", resp.Status, input.Query, body)
			if truncated {
				text += "\n\n[Output truncated at 204800 bytes]"
			}
			return textResult(text, map[string]any{"status": resp.StatusCode, "query": input.Query, "numResults": input.NumResults, "truncated": truncated}), nil
		},
	}
}

// mustTools builds every named tool through NewTool and panics when any one can't be configured.
func mustTools(cwd string, options ToolsOptions, names ...ToolName) []agent.AgentTool {
	options = withHashlineStore(options)
	tools := make([]agent.AgentTool, 0, len(names))
	for _, name := range names {
		tool, err := NewTool(name, cwd, options)
		if err != nil {
			panic(err)
		}
		tools = append(tools, tool)
	}
	return tools
}

// withHashlineStore gives tools built together one shared hashline store,
// so tags read and grep mint are the ones edit and hashline check.
func withHashlineStore(options ToolsOptions) ToolsOptions {
	if options.Hashline == nil {
		options.Hashline = hashline.NewStore()
	}
	return options
}

// newReadTool builds the file reader rooted at cwd. It accepts path plus optional
// offset and limit inputs so callers can page through text without loading it all at once.
func newReadTool(cwd string, options ToolsOptions) agent.AgentTool {
	// One os.Root per tool keeps every read inside the working directory.
	getRoot := rootOnce(cwd)
	store := options.Hashline
	return agent.AgentTool{
		Tool: types.Tool{
			Name:        string(ReadTool),
			Description: fmt.Sprintf("Read a file. Text output starts with [path#TAG], where TAG is a 4-hex tag of the file's content: hashline sections and edit anchors carry it to reject edits to a changed file, and hashline edits only lines a read or grep displayed under it. Rows are LINE:TEXT. Parseable Go files are returned as structural outlines by default; use mode=raw, a line selector, offset, or limit for numbered content. Use path:START-END or path:START+COUNT for a line range, or offset and limit for paging. Output is truncated to %d lines or %dKB (whichever is hit first).", truncate.DefaultMaxLines, truncate.DefaultMaxBytes/1024) + " " + readSelectorsHelp,
			Parameters: schema.Object(schema.JSON{
				"path":   schema.String(),
				"offset": schema.Number(),
				"limit":  schema.Number(),
				"mode":   schema.JSON{"type": "string", "enum": []string{"auto", "raw"}, "default": "auto"},
			}, "path"),
		},
		Label: string(ReadTool),
		Execute: func(ctx context.Context, _ string, params json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			var input readToolInput
			if err := decodeParams(params, &input); err != nil {
				return agent.AgentToolResult{}, err
			}
			if input.Mode == "" {
				input.Mode = "auto"
			}
			if input.Mode != "auto" && input.Mode != "raw" {
				return agent.AgentToolResult{}, fmt.Errorf("mode must be \"auto\" or \"raw\"")
			}
			if err := ctx.Err(); err != nil {
				return agent.AgentToolResult{}, errors.New("Operation aborted")
			}
			rootForRead, err := getRoot()
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			selector, err := parseReadPathSelector(input.Path)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			source, early, err := loadReadSource(ctx, cwd, rootForRead, options, selector.Path)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			if early != nil {
				return *early, nil
			}
			data, absolutePath, displayPath, isResource := source.data, source.absolutePath, source.displayPath, source.isResource
			header := fmt.Sprintf("[%s#%s]", displayPath, hashline.Tag(data))
			// observe records the rows shown in full for workspace files, the
			// ones hashline can edit; resources and session artifacts are not.
			observe := func(displayed []int) {}
			if store != nil && !isResource && absolutePath != "" {
				if _, canonical, err := rootRelPaths(cwd, absolutePath); err == nil {
					observe = func(displayed []int) { store.Observe(canonical, hashline.Decode(data).Text, displayed) }
				}
			}
			if containsNUL(data) {
				return textResult(fmt.Sprintf("%s\n[binary file, %d bytes, not shown]", header, len(data)), &readToolDetails{Binary: true}), nil
			}
			if !isResource && !selector.Set && input.Offset == nil && input.Limit == nil && input.Mode == "auto" {
				if outline, ok := goSourceOutline(displayPath, absolutePath, data); ok {
					truncation := truncate.Head(outline, truncate.Options{})
					output := header + "\n" + truncation.Content
					details := &readToolDetails{Truncation: truncation, Summary: true}
					if truncation.FirstLineExceedsLimit {
						output = fmt.Sprintf("%s\n[structural outline line exceeds %s limit; no raw continuation is available.]", header, truncate.FormatSize(truncate.DefaultMaxBytes))
					} else if truncation.Truncated {
						output += "\n\n[Structural outline truncated in summary mode; no raw offset continuation is available. Use the recovery selectors above to read raw regions.]"
					}
					observe(nil)
					return textResult(output, details), nil
				}
			}

			allLines := splitReadLines(string(data))
			if len(allLines) == 0 {
				observe(nil)
				return textResult(header, nil), nil
			}
			startLine := 0
			endLine := len(allLines)
			userLimitedLines := -1
			if selector.Set {
				startLine = selector.Start - 1
				endLine = min(selector.End, len(allLines))
			} else {
				if input.Offset != nil {
					startLine = max(0, *input.Offset-1)
				}
				if input.Limit != nil {
					if *input.Limit <= 0 {
						return agent.AgentToolResult{}, fmt.Errorf("limit must be greater than zero")
					}
					endLine = min(startLine+*input.Limit, len(allLines))
					userLimitedLines = endLine - startLine
				}
			}
			if startLine >= len(allLines) {
				return agent.AgentToolResult{}, fmt.Errorf("offset %d is beyond end of file (%d lines total)", startLine+1, len(allLines))
			}

			numberedLines := make([]string, endLine-startLine)
			for index := startLine; index < endLine; index++ {
				numberedLines[index-startLine] = fmt.Sprintf("%d:%s", index+1, allLines[index])
			}
			truncation := truncate.Head(strings.Join(numberedLines, "\n"), truncate.Options{})
			output := header + "\n" + truncation.Content
			var details *readToolDetails
			if truncation.FirstLineExceedsLimit {
				firstLineSize := truncate.FormatSize(len([]byte(allLines[startLine])))
				output = fmt.Sprintf("%s\n[%d: line is %s, exceeds %s limit. Use bash to read a bounded portion.]", header, startLine+1, firstLineSize, truncate.FormatSize(truncate.DefaultMaxBytes))
				details = &readToolDetails{Truncation: truncation}
			} else if truncation.Truncated {
				endDisplay := startLine + truncation.OutputLines
				nextOffset := endDisplay + 1
				if selector.Set {
					continuation := fmt.Sprintf("%s:%d-%d", displayPath, nextOffset, endLine)
					if truncation.TruncatedBy == truncate.TruncatedByLines {
						output += fmt.Sprintf("\n\n[Showing lines %d-%d of selected lines %d-%d. Use %s to continue.]", startLine+1, endDisplay, startLine+1, endLine, continuation)
					} else {
						output += fmt.Sprintf("\n\n[Showing lines %d-%d of selected lines %d-%d (%s limit). Use %s to continue.]", startLine+1, endDisplay, startLine+1, endLine, truncate.FormatSize(truncate.DefaultMaxBytes), continuation)
					}
				} else if truncation.TruncatedBy == truncate.TruncatedByLines {
					output += fmt.Sprintf("\n\n[Showing lines %d-%d of %d. Use offset=%d to continue.]", startLine+1, endDisplay, len(allLines), nextOffset)
				} else {
					output += fmt.Sprintf("\n\n[Showing lines %d-%d of %d (%s limit). Use offset=%d to continue.]", startLine+1, endDisplay, len(allLines), truncate.FormatSize(truncate.DefaultMaxBytes), nextOffset)
				}
				details = &readToolDetails{Truncation: truncation}
			} else if !selector.Set && userLimitedLines >= 0 && startLine+userLimitedLines < len(allLines) {
				remaining := len(allLines) - (startLine + userLimitedLines)
				nextOffset := startLine + userLimitedLines + 1
				output += fmt.Sprintf("\n\n[%d more lines in file. Use offset=%d to continue.]", remaining, nextOffset)
			}
			if !truncation.FirstLineExceedsLimit {
				shown := truncation.OutputLines
				if truncation.LastLinePartial {
					shown--
				}
				displayed := make([]int, 0, max(shown, 0))
				for line := startLine + 1; line <= startLine+shown; line++ {
					displayed = append(displayed, line)
				}
				observe(displayed)
			}
			return textResult(output, details), nil
		},
	}
}

// newWriteTool builds the file writer rooted at cwd. It accepts path and content,
// serializing mutations through options.MutationQueue when one is provided.
func newWriteTool(cwd string, options ToolsOptions) agent.AgentTool {
	queue := mutationQueue(options)
	store := options.Hashline
	return agent.AgentTool{
		Tool: types.Tool{
			Name:        string(WriteTool),
			Description: "Write content to a file. Creates the file if it doesn't exist, overwrites if it does. Automatically creates parent directories. " + localWriteHelp,
			Parameters: schema.Object(schema.JSON{
				"path":    schema.String(),
				"content": schema.String(),
			}, "path", "content"),
		},
		Label: string(WriteTool),
		Execute: func(ctx context.Context, _ string, params json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			var input struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := decodeParams(params, &input); err != nil {
				return agent.AgentToolResult{}, err
			}
			target, err := resolveMutationTarget(cwd, options, input.Path)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			defer target.release()
			rootForWrite, relPath, canonicalPath, absolutePath := target.root, target.relPath, target.canonicalPath, target.queueKey
			var tagged string
			err = queue.Do(absolutePath, func() error {
				if err := abortErr(ctx); err != nil {
					return err
				}
				before, err := readFileInRoot(rootForWrite, relPath)
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					return fmt.Errorf("could not read %s before writing: %w", input.Path, err)
				}
				if err := writeFileInRoot(rootForWrite, relPath, []byte(input.Content)); err != nil {
					return err
				}
				notifyFileChanged(options, canonicalPath)
				if store != nil && canonicalPath != "" {
					tag, preview := store.Written(canonicalPath, hashline.Decode(before).Text, hashline.Decode([]byte(input.Content)).Text)
					tagged = taggedResult(input.Path, tag, preview)
				}
				return abortErr(ctx)
			})
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			return textResult(fmt.Sprintf("Successfully wrote %d bytes to %s", len(input.Content), input.Path)+tagged, nil), nil
		},
	}
}

// newEditTool builds the exact-text editor rooted at cwd. It accepts path and
// edits, then serializes each file mutation through options.MutationQueue.
func newEditTool(cwd string, options ToolsOptions) agent.AgentTool {
	queue := mutationQueue(options)
	store := options.Hashline
	return agent.AgentTool{
		Tool: types.Tool{
			Name:        string(EditTool),
			Description: "Edit a single file using exact text replacement. Every edits[].old_text must match a unique, non-overlapping region of the original file. Supply the tag from the [path#TAG] header of read or grep output (or the full anchor an lsp_rename plan gives) as each edit's anchor to reject stale file contents before any change. " + localWriteHelp,
			Parameters: schema.Object(schema.JSON{
				"path": schema.String(),
				"edits": schema.JSON{
					"type": "array",
					"items": schema.JSON{
						"type": "object",
						"properties": schema.JSON{
							"old_text": schema.String(),
							"new_text": schema.String(),
							"anchor":   schema.String(),
						},
						"required": []string{"old_text", "new_text"},
					},
				},
			}, "path", "edits"),
		},
		Label:            string(EditTool),
		PrepareArguments: prepareEditArguments,
		Execute: func(ctx context.Context, _ string, params json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			var input editToolInput
			if err := decodeParams(params, &input); err != nil {
				return agent.AgentToolResult{}, err
			}
			if input.Path == "" || len(input.Edits) == 0 {
				return agent.AgentToolResult{}, errors.New("edit requires path and at least one edit")
			}
			target, err := resolveMutationTarget(cwd, options, input.Path)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			defer target.release()
			rootForEdit, relPath, canonicalPath, absolutePath := target.root, target.relPath, target.canonicalPath, target.queueKey
			var details *editToolDetails
			var tagged string
			err = queue.Do(absolutePath, func() error {
				if err := abortErr(ctx); err != nil {
					return err
				}
				rawContent, err := readFileInRoot(rootForEdit, relPath)
				if err != nil {
					return fmt.Errorf("could not edit %s: %w", input.Path, err)
				}
				if err := abortErr(ctx); err != nil {
					return err
				}
				actualAnchor := ""
				var staleErr error
				for index, edit := range input.Edits {
					if edit.Anchor == "" || staleErr != nil {
						continue
					}
					if hashline.IsTag(edit.Anchor) {
						if err := verifyEditTag(store, canonicalPath, input.Path, edit.Anchor, rawContent); err != nil {
							staleErr = fmt.Errorf("edit hunk %d: %w", index+1, err)
						}
						continue
					}
					if actualAnchor == "" {
						actualAnchor = editdiff.ContentAnchor(rawContent)
					}
					if edit.Anchor != actualAnchor {
						staleErr = fmt.Errorf("edit hunk %d: %w", index+1, &editdiff.StaleAnchorError{Path: input.Path, Expected: edit.Anchor, Actual: actualAnchor})
					}
				}
				if staleErr != nil {
					return staleErr
				}
				bom, content := editdiff.StripBOM(string(rawContent))
				ending := editdiff.DetectLineEnding(content)
				normalized := editdiff.NormalizeToLF(content)
				applied, err := editdiff.ApplyEditsToNormalizedContent(normalized, input.Edits, input.Path)
				if err != nil {
					return err
				}
				if err := abortErr(ctx); err != nil {
					return err
				}
				finalContent := bom + editdiff.RestoreLineEndings(applied.NewContent, ending)
				if err := writeFileInRoot(rootForEdit, relPath, []byte(finalContent)); err != nil {
					return err
				}
				// Disk changed even if the caller cancels from here on.
				notifyFileChanged(options, canonicalPath)
				if store != nil && canonicalPath != "" {
					tag, preview := store.Edited(canonicalPath, hashline.Decode(rawContent).Text, hashline.Decode([]byte(finalContent)).Text)
					tagged = taggedResult(input.Path, tag, preview)
				}
				if err := abortErr(ctx); err != nil {
					return err
				}
				diff := editdiff.GenerateDiffString(applied.BaseContent, applied.NewContent, 4)
				details = &editToolDetails{
					Diff:             diff.Diff,
					Patch:            editdiff.GenerateUnifiedPatch(input.Path, applied.BaseContent, applied.NewContent, 4),
					FirstChangedLine: diff.FirstChangedLine,
				}
				return nil
			})
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			return textResult(fmt.Sprintf("Successfully replaced %d block(s) in %s.", len(input.Edits), input.Path)+tagged, details), nil
		},
	}
}

// newBashTool builds the command runner rooted at cwd. It accepts command and
// optional timeout, honoring shell and command-prefix overrides from options.
func newBashTool(cwd string, options ToolsOptions) agent.AgentTool {
	shell := options.ShellPath
	if shell == "" {
		shell = "/bin/bash"
	}
	timeoutDefault := options.BashTimeout
	if timeoutDefault <= 0 {
		timeoutDefault = 10 * time.Minute
	}
	parameters := schema.JSON{"command": schema.String(), "timeout": schema.Number()}
	description := fmt.Sprintf("Execute a bash command in the current working directory. Output is truncated to last %d lines or %dKB.", truncate.DefaultMaxLines, truncate.DefaultMaxBytes/1024) + " " + bashURLHelp
	if options.Work != nil && !options.Child {
		parameters["async"] = schema.Bool()
		parameters["name"] = schema.JSON{"type": "string", "description": "async only: a name unique among running jobs; jobs, wait, cancel and logs accept it in place of the job id"}
		parameters["ready_log"] = schema.JSON{"type": "string", "description": "async only: RE2 regex; the call returns once an output line matches"}
		parameters["ready_port"] = schema.JSON{"type": "integer", "minimum": 1, "maximum": 65535, "description": "async only: the call returns once 127.0.0.1:<port> accepts TCP connections"}
		parameters["ready_timeout_ms"] = schema.JSON{"type": "integer", "minimum": 1, "maximum": work.MaxReadyTimeoutMS, "description": "readiness deadline, default 30000; on timeout the process is killed"}
		description += " async:true starts a background job and returns its handle. With ready_log and/or ready_port the job is a service: the call returns once every condition holds, or fails with the output tail if the process exits first or the deadline passes. A ready service runs until cancelled, until it exits or until the run ends, and does not keep the run open; restart one with cancel {\"name\":...} then start it again."
	}
	return agent.AgentTool{
		Tool: types.Tool{
			Name:        string(BashTool),
			Description: description,
			Parameters:  schema.Object(parameters, "command"),
		},
		Label: string(BashTool),
		// The permission gate sees prepared arguments, so it judges the
		// command with internal URLs already rewritten to paths.
		PrepareArguments: prepareBashArguments(options.Refs),
		Execute: func(ctx context.Context, _ string, params json.RawMessage, onUpdate agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			var input struct {
				Command        string `json:"command"`
				Timeout        *int   `json:"timeout"`
				Async          bool   `json:"async"`
				Name           string `json:"name"`
				ReadyLog       string `json:"ready_log"`
				ReadyPort      int    `json:"ready_port"`
				ReadyTimeoutMS int    `json:"ready_timeout_ms"`
			}
			if err := decodeParams(params, &input); err != nil {
				return agent.AgentToolResult{}, err
			}
			if err := checkCommandURLs(options.Refs, input.Command); err != nil {
				return agent.AgentToolResult{}, err
			}
			service := input.ReadyLog != "" || input.ReadyPort != 0
			if !input.Async && (service || input.Name != "" || input.ReadyTimeoutMS != 0) {
				return agent.AgentToolResult{}, errors.New("name, ready_log, ready_port and ready_timeout_ms need async:true")
			}
			if input.Async {
				if options.Work == nil || options.Child {
					return agent.AgentToolResult{}, errors.New("async bash requires a durable root session; child bash is synchronous only")
				}
				timeoutSeconds := int(timeoutDefault / time.Second)
				if service {
					// A service lives as long as the run unless the model bounds it.
					timeoutSeconds = maxTimeoutSeconds
				}
				if input.Timeout != nil && *input.Timeout > 0 {
					timeoutSeconds = *input.Timeout
				}
				job, err := options.Work.StartBash(ctx, work.BashSpec{Command: input.Command, TimeoutSeconds: timeoutSeconds, Name: input.Name, ReadyLog: input.ReadyLog, ReadyPort: input.ReadyPort, ReadyTimeoutMS: input.ReadyTimeoutMS})
				if err != nil {
					return agent.AgentToolResult{}, err
				}
				return jobHandleResult(job)
			}
			if _, err := os.Stat(cwd); err != nil {
				return agent.AgentToolResult{}, fmt.Errorf("Working directory does not exist: %s\nCannot execute bash commands.", cwd)
			}
			if input.Timeout != nil && *input.Timeout > maxTimeoutSeconds {
				return agent.AgentToolResult{}, fmt.Errorf("Invalid timeout: maximum is %d seconds", maxTimeoutSeconds)
			}
			command := input.Command
			if options.CommandPrefix != "" {
				command = options.CommandPrefix + "\n" + command
			}
			timeout := timeoutDefault
			if input.Timeout != nil && *input.Timeout > 0 {
				timeout = time.Duration(*input.Timeout) * time.Second
			}
			execCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			cmd := exec.CommandContext(execCtx, shell, "-c", command)
			cmd.Dir = cwd
			if runtime.GOOS != "windows" {
				cmd.WaitDelay = 2 * time.Second
				cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
				cmd.Cancel = func() error {
					err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
					if errors.Is(err, syscall.ESRCH) {
						return os.ErrProcessDone
					}
					return err
				}
			}
			acc := toolio.NewOutputAccumulator(toolio.OutputAccumulatorOptions{TempFilePrefix: "harness-bash", TempDir: ToolOutputDir(options.SessionDataDir)})
			writer := &bashOutputWriter{accumulator: acc, cancel: cancel}
			cmd.Stdout = writer
			cmd.Stderr = writer
			if onUpdate != nil {
				onUpdate(agent.AgentToolResult{Content: []types.ContentBlock{}})
			}
			if err := cmd.Start(); err != nil {
				return agent.AgentToolResult{}, err
			}
			waitErr := cmd.Wait()
			finishErr := acc.Finish()
			snapshot, snapshotErr := acc.Snapshot(toolio.SnapshotOptions{PersistIfTruncated: true})
			closeErr := acc.CloseTempFile()
			if outputErr := errors.Join(writer.err, finishErr, snapshotErr, closeErr); outputErr != nil {
				return agent.AgentToolResult{}, outputErr
			}
			text, details := formatBashOutput(snapshot, acc, "(no output)")
			if execCtx.Err() == context.DeadlineExceeded {
				return agent.AgentToolResult{}, errors.New(appendStatus(text, fmt.Sprintf("Command timed out after %g seconds", timeout.Seconds())))
			}
			if execCtx.Err() == context.Canceled || ctx.Err() != nil {
				return agent.AgentToolResult{}, errors.New(appendStatus(text, "Command aborted"))
			}
			if waitErr != nil {
				if exitErr, ok := waitErr.(*exec.ExitError); ok {
					return agent.AgentToolResult{}, errors.New(appendStatus(text, fmt.Sprintf("Command exited with code %d", exitErr.ExitCode())))
				}
				return agent.AgentToolResult{}, waitErr
			}
			return textResult(text, details), nil
		},
	}
}

// newGrepTool builds the ripgrep-backed content searcher rooted at cwd. It accepts
// pattern plus optional path, glob, case, literal, context, and limit inputs.
func newGrepTool(cwd string, options ToolsOptions) agent.AgentTool {
	return agent.AgentTool{
		Tool: types.Tool{
			Name:        string(GrepTool),
			Description: "Search file contents for a pattern. Returns matching lines with file paths and line numbers. path may be an internal URL (skill://, job://, history://, local://).",
			Parameters: schema.Object(schema.JSON{
				"pattern":     schema.String(),
				"path":        schema.String(),
				"glob":        schema.String(),
				"ignore_case": schema.Bool(),
				"literal":     schema.Bool(),
				"context":     schema.Number(),
				"limit":       schema.Number(),
			}, "pattern"),
		},
		Label: string(GrepTool),
		Execute: func(ctx context.Context, _ string, params json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			var input grepToolInput
			if err := decodeParams(params, &input); err != nil {
				return agent.AgentToolResult{}, err
			}
			if refs.IsURL(input.Path) {
				return grepRefs(ctx, options, input)
			}
			rootForGrep, err := rootOnce(cwd)()
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			relSearch, err := resolveRootPath(cwd, defaultString(input.Path, "."))
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			searchPath := filepath.Join(cwd, relSearch)
			if _, err := statInRoot(rootForGrep, relSearch); err != nil {
				return agent.AgentToolResult{}, fmt.Errorf("Path not found: %s", searchPath)
			}
			limit := input.Limit
			if limit <= 0 {
				limit = 100
			}
			contextLines := input.Context
			if contextLines < 0 {
				contextLines = 0
			}
			matches := []grepMatch{}
			matchLimitReached := false
			backend := "rg"
			if rgPath, ok := toolio.EnsureTool("rg"); ok {
				// --hidden searches dotfiles; .git is repository internals, never project content.
				args := []string{"--json", "--line-number", "--color=never", "--hidden", "--glob", "!.git"}
				if input.IgnoreCase {
					args = append(args, "--ignore-case")
				}
				if input.Literal {
					args = append(args, "--fixed-strings")
				}
				if input.Glob != "" {
					args = append(args, "--glob", input.Glob)
				}
				args = append(args, "--", input.Pattern, searchPath)
				cmd := exec.CommandContext(ctx, rgPath, args...)
				stdout, err := cmd.StdoutPipe()
				if err != nil {
					return agent.AgentToolResult{}, err
				}
				var stderr bytes.Buffer
				cmd.Stderr = &stderr
				if err := cmd.Start(); err != nil {
					return agent.AgentToolResult{}, fmt.Errorf("Failed to run ripgrep: %w", err)
				}
				killedDueToLimit := false
				scanner := bufio.NewScanner(stdout)
				scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
				for scanner.Scan() {
					if len(matches) >= limit {
						matchLimitReached = true
						if cmd.Process != nil {
							killedDueToLimit = true
							_ = cmd.Process.Kill()
						}
						continue
					}
					var event struct {
						Type string `json:"type"`
						Data struct {
							Path struct {
								Text string `json:"text"`
							} `json:"path"`
							LineNumber int `json:"line_number"`
							Lines      struct {
								Text string `json:"text"`
							} `json:"lines"`
						} `json:"data"`
					}
					if err := json.Unmarshal(scanner.Bytes(), &event); err != nil || event.Type != "match" {
						continue
					}
					if event.Data.Path.Text != "" && event.Data.LineNumber > 0 {
						matches = append(matches, grepMatch{filePath: event.Data.Path.Text, lineNumber: event.Data.LineNumber, lineText: event.Data.Lines.Text})
					}
					if len(matches) >= limit {
						matchLimitReached = true
						if cmd.Process != nil {
							killedDueToLimit = true
							_ = cmd.Process.Kill()
						}
					}
				}
				scanErr := scanner.Err()
				waitErr := cmd.Wait()
				if scanErr != nil && ctx.Err() == nil {
					return agent.AgentToolResult{}, scanErr
				}
				if ctx.Err() != nil {
					return agent.AgentToolResult{}, errors.New("Operation aborted")
				}
				if !killedDueToLimit && waitErr != nil {
					if exitErr, ok := waitErr.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
						// rg uses exit code 1 for no matches.
					} else {
						message := strings.TrimSpace(stderr.String())
						if message == "" {
							message = waitErr.Error()
						}
						return agent.AgentToolResult{}, errors.New(message)
					}
				}
			} else {
				// No rg on PATH: walk the root with the pure-Go fallback.
				backend = "go"
				var err error
				matches, matchLimitReached, err = goGrep(ctx, rootForGrep, relSearch, input.Pattern, input.Glob, input.IgnoreCase, input.Literal, limit)
				if err != nil {
					return agent.AgentToolResult{}, err
				}
			}
			return formatGrepOutput(matches, workspaceGrepSource(cwd, rootForGrep), options.Hashline, contextLines, limit, matchLimitReached, backend), nil
		},
	}
}

// newFindTool builds the fd-backed file finder rooted at cwd. It accepts pattern
// plus optional path and limit inputs.
func newFindTool(cwd string) agent.AgentTool {
	return agent.AgentTool{
		Tool: types.Tool{
			Name:        string(FindTool),
			Description: "Search for files by glob pattern. Returns matching file paths relative to the search directory.",
			Parameters: schema.Object(schema.JSON{
				"pattern": schema.String(),
				"path":    schema.String(),
				"limit":   schema.Number(),
			}, "pattern"),
		},
		Label: string(FindTool),
		Execute: func(ctx context.Context, _ string, params json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			var input struct {
				Pattern string `json:"pattern"`
				Path    string `json:"path"`
				Limit   int    `json:"limit"`
			}
			if err := decodeParams(params, &input); err != nil {
				return agent.AgentToolResult{}, err
			}
			rootForFind, err := rootOnce(cwd)()
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			relSearch, err := resolveRootPath(cwd, defaultString(input.Path, "."))
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			searchPath := filepath.Join(cwd, relSearch)
			if _, err := statInRoot(rootForFind, relSearch); err != nil {
				return agent.AgentToolResult{}, fmt.Errorf("Path not found: %s", searchPath)
			}
			limit := input.Limit
			if limit <= 0 {
				limit = 1000
			}
			var rawLines []string
			backend := "fd"
			if fdPath, ok := toolio.EnsureTool("fd"); ok {
				args := []string{"--glob", "--color=never", "--hidden", "--exclude", ".git"}
				if !insideGitRepo(searchPath) {
					args = append(args, "--no-require-git")
				}
				args = append(args, "--max-results", fmt.Sprintf("%d", limit))
				effectivePattern := input.Pattern
				if strings.Contains(input.Pattern, "/") {
					args = append(args, "--full-path")
					if !strings.HasPrefix(input.Pattern, "/") && !strings.HasPrefix(input.Pattern, "**/") && input.Pattern != "**" {
						effectivePattern = "**/" + input.Pattern
					}
				}
				args = append(args, "--", effectivePattern, searchPath)
				cmd := exec.CommandContext(ctx, fdPath, args...)
				var stderr bytes.Buffer
				cmd.Stderr = &stderr
				stdoutBytes, err := cmd.Output()
				if ctx.Err() != nil {
					return agent.AgentToolResult{}, errors.New("Operation aborted")
				}
				if err != nil && len(stdoutBytes) == 0 {
					message := strings.TrimSpace(stderr.String())
					if message == "" {
						message = err.Error()
					}
					return agent.AgentToolResult{}, errors.New(message)
				}
				rawLines = strings.Split(strings.TrimRight(string(stdoutBytes), "\n"), "\n")
			} else {
				// No fd on PATH: walk the root with the pure-Go fallback.
				backend = "go"
				var err error
				rawLines, _, err = goFind(ctx, rootForFind, relSearch, input.Pattern, limit)
				if err != nil {
					return agent.AgentToolResult{}, err
				}
			}
			results := []string{}
			for _, line := range rawLines {
				line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
				if line == "" {
					continue
				}
				hadTrailingSlash := strings.HasSuffix(line, "/") || strings.HasSuffix(line, "\\")
				relativePath := formatSearchPath(searchPath, line, true)
				if hadTrailingSlash && !strings.HasSuffix(relativePath, "/") {
					relativePath += "/"
				}
				results = append(results, relativePath)
			}
			if len(results) == 0 {
				return textResult("No files found matching pattern", nil), nil
			}
			limitReached := len(results) >= limit
			rawOutput := strings.Join(results, "\n")
			truncation := truncate.Head(rawOutput, truncate.Options{MaxLines: truncate.Int(int(^uint(0) >> 1))})
			output := truncation.Content
			details := map[string]any{}
			notices := []string{}
			if limitReached {
				notices = append(notices, fmt.Sprintf("%d results limit reached", limit))
				details["resultLimitReached"] = limit
			}
			if truncation.Truncated {
				notices = append(notices, truncate.FormatSize(truncate.DefaultMaxBytes)+" limit reached")
				details["truncation"] = truncation
			}
			details["backend"] = backend
			if len(notices) > 0 {
				output += "\n\n[" + strings.Join(notices, ". ") + "]"
			}
			return textResult(output, optionalMap(details)), nil
		},
	}
}

// newLsTool builds the directory lister rooted at cwd. It accepts optional path
// and limit inputs, including hidden entries so agents see the real workspace.
func newLsTool(cwd string) agent.AgentTool {
	return agent.AgentTool{
		Tool: types.Tool{
			Name:        string(LsTool),
			Description: "List directory contents. Returns entries sorted alphabetically, with '/' suffix for directories. Includes dotfiles.",
			Parameters: schema.Object(schema.JSON{
				"path":  schema.String(),
				"limit": schema.Number(),
			}),
		},
		Label: string(LsTool),
		Execute: func(ctx context.Context, _ string, params json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			var input struct {
				Path  string `json:"path"`
				Limit int    `json:"limit"`
			}
			if err := decodeParams(params, &input); err != nil {
				return agent.AgentToolResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return agent.AgentToolResult{}, errors.New("Operation aborted")
			}
			rootForLs, err := rootOnce(cwd)()
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			relPath, err := resolveRootPath(cwd, defaultString(input.Path, "."))
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			dirPath := filepath.Join(cwd, relPath)
			stat, err := statInRoot(rootForLs, relPath)
			if err != nil {
				return agent.AgentToolResult{}, fmt.Errorf("Path not found: %s", dirPath)
			}
			if !stat.IsDir() {
				return agent.AgentToolResult{}, fmt.Errorf("Not a directory: %s", dirPath)
			}
			entries, err := fs.ReadDir(rootForLs.FS(), relPath)
			if err != nil {
				return agent.AgentToolResult{}, fmt.Errorf("Cannot read directory: %s", err.Error())
			}
			sort.Slice(entries, func(i, j int) bool { return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name()) })
			limit := input.Limit
			if limit <= 0 {
				limit = 500
			}
			results := []string{}
			limitReached := false
			for _, entry := range entries {
				if len(results) >= limit {
					limitReached = true
					break
				}
				name := entry.Name()
				info, err := statInRoot(rootForLs, path.Join(relPath, name))
				if err != nil {
					continue
				}
				if info.IsDir() {
					name += "/"
				}
				results = append(results, name)
			}
			if len(results) == 0 {
				return textResult("(empty directory)", nil), nil
			}
			rawOutput := strings.Join(results, "\n")
			truncation := truncate.Head(rawOutput, truncate.Options{MaxLines: truncate.Int(int(^uint(0) >> 1))})
			output := truncation.Content
			details := map[string]any{}
			notices := []string{}
			if limitReached {
				notices = append(notices, fmt.Sprintf("%d entries limit reached. Use limit=%d for more", limit, limit*2))
				details["entryLimitReached"] = limit
			}
			if truncation.Truncated {
				notices = append(notices, truncate.FormatSize(truncate.DefaultMaxBytes)+" limit reached")
				details["truncation"] = truncation
			}
			if len(notices) > 0 {
				output += "\n\n[" + strings.Join(notices, ". ") + "]"
			}
			return textResult(output, optionalMap(details)), nil
		},
	}
}

// prepareEditArguments accepts legacy single-edit fields and JSON-encoded edits,
// normalizing both forms before the edit tool sees them.
func prepareEditArguments(args json.RawMessage) json.RawMessage {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(args, &raw); err != nil {
		return args
	}
	if encodedEdits, ok := raw["edits"]; ok {
		var editsText string
		if json.Unmarshal(encodedEdits, &editsText) == nil && json.Valid([]byte(editsText)) {
			raw["edits"] = json.RawMessage(editsText)
		}
	}
	if _, ok := raw["edits"]; !ok {
		oldText, hasOld := raw["old_text"]
		newText, hasNew := raw["new_text"]
		if hasOld && hasNew {
			edit := map[string]json.RawMessage{"old_text": oldText, "new_text": newText}
			if anchor, hasAnchor := raw["anchor"]; hasAnchor {
				edit["anchor"] = anchor
			}
			edits, err := json.Marshal([]map[string]json.RawMessage{edit})
			if err != nil {
				return args
			}
			raw["edits"] = edits
		}
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return args
	}
	return out
}

// parseReadPathSelector only claims a colon suffix once it starts with a digit, so colon-containing paths still read literally.
func parseReadPathSelector(input string) (readPathSelector, error) {
	separator := strings.LastIndex(input, ":")
	if separator < 0 {
		return readPathSelector{Path: input}, nil
	}
	path, selector := input[:separator], input[separator+1:]
	if selector == "" || selector[0] < '0' || selector[0] > '9' {
		return readPathSelector{Path: input}, nil
	}
	for _, operator := range []string{"-", "+"} {
		if index := strings.Index(selector, operator); index >= 0 {
			start, startErr := parseReadSelectorNumber(selector[:index], selector)
			if startErr != nil {
				return readPathSelector{}, startErr
			}
			value, valueErr := parseReadSelectorNumber(selector[index+1:], selector)
			if valueErr != nil {
				return readPathSelector{}, valueErr
			}
			if operator == "-" {
				if value < start {
					return readPathSelector{}, readSelectorError{Selector: selector, Reason: "end must be greater than or equal to start"}
				}
				return readPathSelector{Path: path, Start: start, End: value, Set: true}, nil
			}
			if value > int(^uint(0)>>1)-start+1 {
				return readPathSelector{}, readSelectorError{Selector: selector, Reason: "range is too large"}
			}
			return readPathSelector{Path: path, Start: start, End: start + value - 1, Set: true}, nil
		}
	}
	return readPathSelector{Path: input}, nil
}

// parseReadSelectorNumber centralizes the one-based line invariant shared by both selector forms.
func parseReadSelectorNumber(value string, selector string) (int, error) {
	number, err := strconv.Atoi(value)
	if err != nil || number < 1 {
		return 0, readSelectorError{Selector: selector, Reason: "line numbers must be positive integers"}
	}
	return number, nil
}

// splitReadLines makes line-oriented reads platform-neutral without inventing a trailing empty line.
func splitReadLines(content string) []string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	if content == "" {
		return nil
	}
	lines := strings.Split(content, "\n")
	if strings.HasSuffix(content, "\n") {
		return lines[:len(lines)-1]
	}
	return lines
}

// readDisplayPath keeps workspace paths relocatable while leaving outside requests recognizable.
func readDisplayPath(cwd string, absolutePath string, requestedPath string) string {
	relativePath, err := filepath.Rel(cwd, absolutePath)
	if err == nil && relativePath != "" && relativePath != "." && !strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) && relativePath != ".." {
		return filepath.ToSlash(relativePath)
	}
	return filepath.ToSlash(requestedPath)
}

// formatBashOutput adds truncation details and a full-output path when needed.
func formatBashOutput(snapshot toolio.OutputSnapshot, acc *toolio.OutputAccumulator, emptyText string) (string, any) {
	truncation := snapshot.Truncation
	text := snapshot.Content
	if text == "" {
		text = emptyText
	}
	if truncation.Truncated {
		startLine := truncation.TotalLines - truncation.OutputLines + 1
		endLine := truncation.TotalLines
		if truncation.LastLinePartial {
			text += fmt.Sprintf("\n\n[Showing last %s of line %d (line is %s). Full output: %s]", truncate.FormatSize(truncation.OutputBytes), endLine, truncate.FormatSize(acc.GetLastLineBytes()), snapshot.FullOutputPath)
		} else if truncation.TruncatedBy == truncate.TruncatedByLines {
			text += fmt.Sprintf("\n\n[Showing lines %d-%d of %d. Full output: %s]", startLine, endLine, truncation.TotalLines, snapshot.FullOutputPath)
		} else {
			text += fmt.Sprintf("\n\n[Showing lines %d-%d of %d (%s limit). Full output: %s]", startLine, endLine, truncation.TotalLines, truncate.FormatSize(truncate.DefaultMaxBytes), snapshot.FullOutputPath)
		}
		return text, map[string]any{"truncation": truncation, "fullOutputPath": snapshot.FullOutputPath}
	}
	return text, nil
}

// splitFileLines normalizes line endings before grep adds surrounding context.
func splitFileLines(content string) []string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	return strings.Split(content, "\n")
}

// formatSearchPath keeps search results relative to root when that remains possible.
func formatSearchPath(root, file string, rootIsDir bool) string {
	if !rootIsDir {
		return filepath.Base(file)
	}
	cleanRoot := filepath.Clean(root)
	cleanFile := filepath.Clean(file)
	if rel, err := filepath.Rel(cleanRoot, cleanFile); err == nil && rel != "" && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(cleanFile)
}

// insideGitRepo reports whether searchPath sits inside a Git worktree.
func insideGitRepo(searchPath string) bool {
	current := filepath.Clean(searchPath)
	for {
		if _, err := os.Stat(filepath.Join(current, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
		current = parent
	}
}

// imageMimeType returns a MIME type only for image extensions the read tool can render.
func imageMimeType(filePath string) string {
	mimeType := mime.TypeByExtension(strings.ToLower(filepath.Ext(filePath)))
	if strings.HasPrefix(mimeType, "image/") {
		return strings.Split(mimeType, ";")[0]
	}
	return ""
}

// textResult wraps text and optional details in the tool result shape every caller expects.
func textResult(text string, details any) agent.AgentToolResult {
	var raw json.RawMessage
	if details != nil {
		encoded, err := json.Marshal(details)
		if err != nil {
			// Details are internal, controlled values, so a marshal failure is a
			// programming error. Surface it on the JSON boundary instead of
			// silently dropping the details. The string-map fallback cannot fail.
			encoded, _ = json.Marshal(map[string]string{"details_marshal_error": err.Error()})
		}
		raw = encoded
	}
	return agent.AgentToolResult{Content: []types.ContentBlock{types.NewText(text)}, Details: raw}
}

// mutationQueue picks the caller's queue or the shared queue that keeps file writes ordered.
func mutationQueue(options ToolsOptions) *toolio.FileMutationQueue {
	if options.MutationQueue != nil {
		return options.MutationQueue
	}
	return defaultMutationQueue
}

// decodeParams treats missing parameters as an empty object so no-input tools stay easy to call.
func decodeParams(params json.RawMessage, target any) error {
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	return json.Unmarshal(params, target)
}

// decodeStrict decodes one JSON object, rejecting unknown arguments,
// trailing data and non-integer values for integer fields. The tools whose
// schemas declare additionalProperties:false use it.
func decodeStrict(params json.RawMessage, target any) error {
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(params))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("invalid arguments: trailing data after the arguments object")
	}
	return nil
}

// notifyFileChanged reports a completed disk write to options.OnFileChanged.
func notifyFileChanged(options ToolsOptions, canonicalPath string) {
	if options.OnFileChanged != nil && canonicalPath != "" {
		options.OnFileChanged(canonicalPath)
	}
}

// abortErr turns a canceled context into the tool-layer error callers expect.
func abortErr(ctx context.Context) error {
	if ctx.Err() != nil {
		return errors.New("Operation aborted")
	}
	return nil
}

// appendStatus keeps a command's captured output ahead of its final status.
func appendStatus(text string, status string) string {
	if text != "" {
		return text + "\n\n" + status
	}
	return status
}

// optionalMap avoids emitting empty details maps in tool results.
func optionalMap(m map[string]any) any {
	if len(m) == 0 {
		return nil
	}
	return m
}

// defaultString fills an omitted optional string with its caller's fallback.
func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// max picks the larger bound while paging search and read results.
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// min picks the smaller bound while paging search and read results.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// readAllText preserves line breaks while reading command output for callers that need text.
func readAllText(r io.Reader) string {
	var b strings.Builder
	s := bufio.NewScanner(r)
	for s.Scan() {
		b.WriteString(s.Text())
		b.WriteByte('\n')
	}
	return b.String()
}

// webClient adds redirect validation and a timeout to the configured client, keeping
// web tools from following a safe URL into a private address.
func webClient(options ToolsOptions) *http.Client {
	resolveIP := webResolver(options)
	checkRedirect := func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if _, err := validateWebURL(req.Context(), req.URL.String(), resolveIP); err != nil {
			return err
		}
		if options.HTTPClient != nil && options.HTTPClient.CheckRedirect != nil {
			return options.HTTPClient.CheckRedirect(req, via)
		}
		return nil
	}
	if options.HTTPClient != nil {
		client := *options.HTTPClient
		if client.Timeout == 0 {
			client.Timeout = defaultWebTimeout
		}
		client.CheckRedirect = checkRedirect
		return &client
	}
	return &http.Client{Timeout: defaultWebTimeout, CheckRedirect: checkRedirect}
}

// webResolver uses the caller's resolver when supplied, which keeps web address checks testable.
func webResolver(options ToolsOptions) func(context.Context, string) ([]net.IP, error) {
	if options.ResolveIP != nil {
		return options.ResolveIP
	}
	return func(ctx context.Context, host string) ([]net.IP, error) {
		return net.DefaultResolver.LookupIP(ctx, "ip", host)
	}
}

// validateWebURL accepts only public HTTP(S) URLs, resolving hostnames before a
// web tool connects so local and private addresses stay out of reach.
func validateWebURL(ctx context.Context, raw string, resolveIP func(context.Context, string) ([]net.IP, error)) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("requires an http(s) URL")
	}
	host := parsed.Hostname()
	if host == "" {
		return nil, fmt.Errorf("missing URL host")
	}
	if ip := net.ParseIP(host); ip != nil {
		if isPrivateWebIP(ip) {
			return nil, fmt.Errorf("private or local address %s is blocked", ip.String())
		}
		return parsed, nil
	}
	ips, err := resolveIP(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", host)
	}
	for _, ip := range ips {
		if isPrivateWebIP(ip) {
			return nil, fmt.Errorf("private or local resolved address %s is blocked", ip.String())
		}
	}
	return parsed, nil
}

// isPrivateWebIP reports whether ip names a local, private, link-local, or unspecified address.
func isPrivateWebIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

// clampWebMaxBytes keeps a caller's fetch limit within the safe response bounds.
func clampWebMaxBytes(maxBytes int) int {
	if maxBytes <= 0 {
		return defaultWebMaxBytes
	}
	if maxBytes > maxWebFetchBytes {
		return maxWebFetchBytes
	}
	return maxBytes
}

// readLimited reads at most maxBytes plus one byte so callers can report truncation.
func readLimited(r io.Reader, maxBytes int) (string, bool, error) {
	data, err := io.ReadAll(io.LimitReader(r, int64(maxBytes)+1))
	if err != nil {
		return "", false, err
	}
	if len(data) > maxBytes {
		return string(data[:maxBytes]), true, nil
	}
	return string(data), false, nil
}
