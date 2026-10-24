package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/editbatch"
	"go.harness.dev/harness/internal/editdiff"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/hashline"
	"go.harness.dev/harness/internal/hooks"
	"go.harness.dev/harness/internal/pathutil"
	"go.harness.dev/harness/internal/schema"
	"go.harness.dev/harness/internal/toolio"
	"go.harness.dev/harness/internal/truncate"
)

// ASTBinary finds ast-grep, not an unrelated Unix sg executable.
func ASTBinary() (string, error) {
	if name, err := exec.LookPath("ast-grep"); err == nil {
		return name, nil
	}
	if name, err := exec.LookPath("sg"); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, name, "--version").Output()
		if err == nil && strings.HasPrefix(string(out), "ast-grep ") {
			return name, nil
		}
	}
	return "", errors.New("ast_grep and ast_edit require ast-grep (or its sg alias) on PATH; install with brew install ast-grep")
}

func admitCodingProcess(ctx context.Context, options ToolsOptions) (func(), error) {
	if options.AdmitProcess != nil {
		return options.AdmitProcess(ctx)
	}
	return func() {}, nil
}

type astOperation struct {
	Pattern string  `json:"pat"`
	Output  *string `json:"out"`
}
type astInput struct {
	Pattern string         `json:"pat"`
	Path    string         `json:"path"`
	Skip    int            `json:"skip"`
	Lang    string         `json:"lang"`
	Ops     []astOperation `json:"ops"`
	Paths   []string       `json:"paths"`
	Apply   bool           `json:"apply"`
	Preview string         `json:"preview"`
}
type astPosition struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}
type astRange struct {
	ByteOffset struct {
		Start int `json:"start"`
		End   int `json:"end"`
	} `json:"byteOffset"`
	Start astPosition `json:"start"`
	End   astPosition `json:"end"`
}
type astMatch struct {
	Text               string   `json:"text"`
	Range              astRange `json:"range"`
	Replacement        *string  `json:"replacement"`
	ReplacementOffsets *struct {
		Start int `json:"start"`
		End   int `json:"end"`
	} `json:"replacementOffsets"`
}
type astFile struct {
	target        hashlineTarget
	before, after []byte
	matchCount    int
	matches       []astMatch
}

func newASTTool(name ToolName, cwd string, options ToolsOptions) (agent.AgentTool, error) {
	binary, err := ASTBinary()
	if err != nil {
		return agent.AgentTool{}, err
	}
	properties := schema.JSON{"pat": schema.String(), "path": schema.String(), "skip": schema.JSON{"type": "integer", "minimum": 0}, "lang": schema.String()}
	required := []string{"pat"}
	description := "Search workspace code structurally with ast-grep. pat is an AST pattern with $NAME and $$$ARGS captures. path is a file, directory, glob or semicolon-separated scopes (default .); lang overrides language detection. Returns at most 50 matches, paged with skip."
	if name == ASTEditTool {
		properties = schema.JSON{
			"ops":   schema.JSON{"type": "array", "minItems": 1, "items": schema.JSON{"type": "object", "properties": schema.JSON{"pat": schema.String(), "out": schema.String()}, "required": []string{"pat", "out"}, "additionalProperties": false}},
			"paths": schema.JSON{"type": "array", "minItems": 1, "items": schema.String()}, "lang": schema.String(), "apply": schema.JSON{"type": "boolean", "default": false}, "preview": schema.String(),
		}
		required = []string{"ops", "paths"}
		description = "Rewrite workspace code structurally with ast-grep. ops are ordered {pat,out} rewrites; paths are files, directories or globs. First call previews a diff and returns a preview token without writing. To apply, repeat the exact ops/paths/lang with apply:true and that preview token. Changed source or request invalidates the token. All changes pass through workspace mutation locks and the rollback journal. No external paths or URLs are accepted."
	}
	return agent.AgentTool{Tool: types.Tool{Name: string(name), Description: description, Parameters: strictObjectSchema(properties, required...)}, Label: string(name), Execute: func(ctx context.Context, id string, raw json.RawMessage, _ agent.AgentToolUpdateCallback) (toolResult agent.AgentToolResult, toolErr error) {
		defer func() { toolErr = codingToolError(toolErr, options) }()
		var input astInput
		if err := decodeParams(raw, &input); err != nil {
			return agent.AgentToolResult{}, err
		}
		if name == ASTGrepTool {
			if strings.TrimSpace(input.Pattern) == "" || input.Skip < 0 {
				return agent.AgentToolResult{}, errors.New("pat must be non-empty and skip non-negative")
			}
			input.Paths = strings.Split(input.Path, ";")
			if input.Path == "" {
				input.Paths = []string{"."}
			}
		} else {
			if len(input.Ops) == 0 || len(input.Paths) == 0 {
				return agent.AgentToolResult{}, errors.New("ast_edit requires ops and paths")
			}
			for _, op := range input.Ops {
				if strings.TrimSpace(op.Pattern) == "" || op.Output == nil {
					return agent.AgentToolResult{}, errors.New("rewrite requires non-empty pat and out (empty out deletes matches)")
				}
			}
			if input.Apply && input.Preview == "" {
				return agent.AgentToolResult{}, errors.New("preview the rewrite first, then pass its preview token with apply:true")
			}
		}
		root, err := os.OpenRoot(cwd)
		if err != nil {
			return agent.AgentToolResult{}, err
		}
		defer root.Close()
		targets, err := astTargets(ctx, root, cwd, input.Paths, input.Lang, name == ASTEditTool)
		if err != nil {
			return agent.AgentToolResult{}, err
		}
		// Approval must not hold file locks or a process slot: Notification hooks
		// need admission too. Apply re-reads and re-plans under locks afterwards.
		if name == ASTEditTool && input.Apply {
			paths := make([]string, 0, len(targets))
			for path := range targets {
				paths = append(paths, path)
			}
			sort.Strings(paths)
			if options.ApproveHookSettings != nil {
				if err := options.ApproveHookSettings(ctx, string(ASTEditTool), id, paths); err != nil {
					return agent.AgentToolResult{}, err
				}
			} else {
				for _, path := range paths {
					if hooks.HookSettingsPath(path, cwd) {
						return agent.AgentToolResult{}, fmt.Errorf("Changing hook settings requires per-call approval: %s", path)
					}
				}
			}
		}
		var result agent.AgentToolResult
		lockTargets := targets
		if name == ASTGrepTool {
			lockTargets = nil
		}
		err = lockFiles(mutationQueue(options), lockTargets, func() error {
			// stdin and an empty private cwd prevent ast-grep from opening paths or
			// loading untrusted sgconfig.yml/custom parser libraries from the workspace.
			dir, err := os.MkdirTemp("", "tempest-ast-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(dir)
			release, err := admitCodingProcess(ctx, options)
			if err != nil {
				return err
			}
			files, err := astPlan(ctx, binary, dir, root, targets, input, name == ASTEditTool)
			release()
			if err != nil {
				return err
			}
			if name == ASTGrepTool {
				var rows []string
				total := 0
				for _, file := range files {
					for _, match := range file.matches {
						rows = append(rows, fmt.Sprintf("%s:%d:%d: %s", file.target.rel, match.Range.Start.Line+1, match.Range.Start.Column+1, match.Text))
					}
					total += file.matchCount
				}
				text := strings.Join(rows, "\n")
				if total == 0 {
					text = "No structural matches."
				}
				if total > input.Skip+len(rows) {
					text += fmt.Sprintf("\n[%d more matches; use skip=%d]", total-input.Skip-len(rows), input.Skip+len(rows))
				}
				var err error
				result, err = codingTextResult(text, codingToolDetails{Matches: total, Skip: input.Skip}, options)
				return err
			}
			var diff strings.Builder
			var changes []editbatch.Change
			var paths []string
			digest := sha256.New()
			request := input
			request.Apply = false
			request.Preview = ""
			encoded, _ := json.Marshal(request)
			digest.Write(encoded)
			for _, file := range files {
				fmt.Fprintf(digest, "\x00%s\x00%d\x00", file.target.canonical, len(file.before))
				digest.Write(file.before)
				if bytes.Equal(file.before, file.after) {
					continue
				}
				diff.WriteString(editdiff.GenerateUnifiedPatch(file.target.rel, string(file.before), string(file.after), 4))
				diff.WriteByte('\n')
				changes = append(changes, editbatch.Change{Path: filepath.FromSlash(file.target.canonical), Data: file.after})
				paths = append(paths, file.target.canonical)
			}
			token := hex.EncodeToString(digest.Sum(nil))
			text := diff.String()
			if len(changes) == 0 {
				text = "No structural changes."
			}
			if input.Apply {
				if input.Preview != token {
					return errors.New("stale ast_edit preview: source or request changed; preview again before applying")
				}
				// A process outside the harness can write while ast-grep runs. Refuse it.
				for _, file := range files {
					live, err := readFileInRoot(root, filepath.FromSlash(file.target.canonical))
					if err != nil {
						return err
					}
					if !bytes.Equal(live, file.before) {
						return fmt.Errorf("%s changed during rewrite; preview again", file.target.rel)
					}
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := editbatch.New(options.SessionDataDir).Apply(root, "ast_edit call "+id, changes); err != nil {
					return err
				}
				for _, file := range files {
					if bytes.Equal(file.before, file.after) {
						continue
					}
					notifyFileChanged(options, file.target.canonical)
					if options.Hashline != nil {
						tag, preview := options.Hashline.Edited(file.target.canonical, hashline.Decode(file.before).Text, hashline.Decode(file.after).Text)
						text += taggedResult(file.target.rel, tag, preview)
					}
				}
				text = "Applied structural rewrite.\n" + text
			} else {
				text += "\nPreview only. Repeat with apply:true and preview:" + token
			}
			result, err = codingTextResult(text, codingToolDetails{Preview: token, Applied: &input.Apply, Files: paths}, options)
			return err
		})
		return result, err
	}}, nil
}

func astTargets(ctx context.Context, root *os.Root, cwd string, scopes []string, lang string, edit bool) (map[string]hashlineTarget, error) {
	targets := map[string]hashlineTarget{}
	visited := 0
	ignore := loadGitignore(root)
	for _, scope := range scopes {
		if scope == "" || strings.Contains(scope, "://") {
			return nil, errors.New("AST scopes must be workspace paths, not URLs or empty paths")
		}
		glob := ""
		base := scope
		if index := strings.IndexAny(scope, "*?["); index >= 0 {
			slash := strings.LastIndex(scope[:index], "/")
			base = "."
			glob = scope
			if slash >= 0 {
				base = scope[:slash]
				glob = scope[slash+1:]
			}
		}
		abs := pathutil.ResolveToCwd(base, cwd)
		rel, _, err := rootRelPaths(cwd, abs)
		if err != nil {
			return nil, err
		}
		err = fs.WalkDir(root.FS(), filepath.ToSlash(rel), func(p string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			visited++
			if edit && visited > maxSnapshotVisitedEntry {
				return errors.New("AST scope exceeds 8192 entries; narrow paths")
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if entry.IsDir() {
				if entry.Name() == ".git" || ignore.Ignore(p, true) {
					return fs.SkipDir
				}
				return nil
			}
			if !entry.Type().IsRegular() || ignore.Ignore(p, false) || !matchGlobPattern(glob, searchRelative(filepath.ToSlash(rel), p)) {
				return nil
			}
			if lang == "" && astLanguage(p) == "" {
				if p == filepath.ToSlash(rel) {
					return fmt.Errorf("unknown language for %s; supply lang", p)
				}
				return nil
			}
			abs := filepath.Join(cwd, filepath.FromSlash(p))
			r, canonical, err := rootRelPaths(cwd, abs)
			if err != nil {
				return err
			}
			targets[canonical] = hashlineTarget{abs: abs, rel: r, canonical: canonical}
			if edit && len(targets) > maxSnapshotFiles {
				return errors.New("AST scope exceeds 256 source files; narrow paths")
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return targets, nil
}

func astPlan(ctx context.Context, binary, dir string, root *os.Root, targets map[string]hashlineTarget, input astInput, edit bool) ([]astFile, error) {
	keys := make([]string, 0, len(targets))
	for key := range targets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	files := make([]astFile, 0, len(keys))
	total := 0
	retained := 0
	for _, key := range keys {
		target := targets[key]
		data, err := readFileInRootLimit(root, filepath.FromSlash(target.canonical), maxWorkspaceFileBytes)
		if err != nil {
			return nil, err
		}
		total += len(data)
		if len(data) > maxWorkspaceFileBytes || edit && total > maxSnapshotBytes {
			return nil, errors.New("AST scope exceeds source byte limit; narrow paths")
		}
		file := astFile{target: target, before: data, after: data}
		lang := input.Lang
		if lang == "" {
			lang = astLanguage(target.rel)
		}
		ops := input.Ops
		if !edit {
			ops = []astOperation{{Pattern: input.Pattern}}
		}
		for _, op := range ops {
			args := []string{"run", "--stdin", "--lang=" + lang, "--pattern=" + op.Pattern, "--json=compact", "--color", "never", "--threads", "1"}
			if edit {
				args = append(args, "--rewrite="+*op.Output)
			}
			out, err := runCodingBinary(ctx, binary, dir, args, file.after)
			if err != nil {
				return nil, err
			}
			var matches []astMatch
			if len(bytes.TrimSpace(out)) > 0 {
				if err := json.Unmarshal(out, &matches); err != nil {
					return nil, fmt.Errorf("invalid ast-grep JSON: %w", err)
				}
			}
			if !edit {
				file.matchCount = len(matches)
				// Search retains only the requested page, not source snapshots or
				// every matching syntax tree across the repository.
				for _, match := range matches {
					if input.Skip > 0 {
						input.Skip--
					} else if retained < 50 {
						file.matches = append(file.matches, match)
						retained++
					}
				}
				continue
			}
			// Rewrite offsets are byte offsets, including punctuation removed by a fix.
			sort.Slice(matches, func(i, j int) bool { return matches[i].Range.ByteOffset.Start < matches[j].Range.ByteOffset.Start })
			var next bytes.Buffer
			cursor := 0
			for _, match := range matches {
				start, end := match.Range.ByteOffset.Start, match.Range.ByteOffset.End
				if match.ReplacementOffsets != nil {
					start, end = match.ReplacementOffsets.Start, match.ReplacementOffsets.End
				}
				if match.Replacement == nil || start < cursor || end < start || end > len(file.after) {
					return nil, fmt.Errorf("invalid or overlapping ast-grep rewrite in %s", target.rel)
				}
				next.Write(file.after[cursor:start])
				next.WriteString(*match.Replacement)
				cursor = end
			}
			if len(matches) > 0 {
				next.Write(file.after[cursor:])
				file.after = next.Bytes()
			}
		}
		if !edit {
			file.before, file.after = nil, nil
		}
		files = append(files, file)
	}
	return files, nil
}

type codingBinaryOutput struct{ bytes.Buffer }

func (b *codingBinaryOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 8<<20 {
		return 0, errors.New("coding tool output exceeds 8 MiB; narrow the request")
	}
	return b.Buffer.Write(p)
}
func runCodingBinary(ctx context.Context, binary, dir string, args []string, input []byte) ([]byte, error) {
	return runCodingBinaryTimeout(ctx, binary, dir, args, input, 30*time.Second)
}

func runCodingBinaryTimeout(ctx context.Context, binary, dir string, args []string, input []byte, budget time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.Stdin = bytes.NewReader(input)
	var out, stderr codingBinaryOutput
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		var exit *exec.ExitError
		isAST := filepath.Base(binary) == "ast-grep" || filepath.Base(binary) == "sg"
		if !isAST || !errors.As(err, &exit) || exit.ExitCode() != 1 || !json.Valid(out.Bytes()) {
			return nil, fmt.Errorf("%s: %w: %s", filepath.Base(binary), err, strings.TrimSpace(stderr.String()))
		}
	}
	return out.Bytes(), nil
}

type codingToolDetails struct {
	Op             string           `json:"op,omitempty"`
	Repo           string           `json:"repo,omitempty"`
	Preview        string           `json:"preview,omitempty"`
	Applied        *bool            `json:"applied,omitempty"`
	Files          []string         `json:"files,omitempty"`
	Matches        int              `json:"matches,omitempty"`
	Skip           int              `json:"skip,omitempty"`
	FullOutputPath string           `json:"fullOutputPath,omitempty"`
	Truncation     *truncate.Result `json:"truncation,omitempty"`
}

// codingTextResult uses the same bounded, readable spill contract as bash.
func codingTextResult(text string, details codingToolDetails, options ToolsOptions) (agent.AgentToolResult, error) {
	acc := toolio.NewOutputAccumulator(toolio.OutputAccumulatorOptions{TempFilePrefix: "tempest-coding", TempDir: ToolOutputDir(options.SessionDataDir)})
	defer acc.CloseTempFile()
	if err := acc.Append([]byte(text)); err != nil {
		return agent.AgentToolResult{}, err
	}
	if err := acc.Finish(); err != nil {
		return agent.AgentToolResult{}, err
	}
	snapshot, err := acc.Snapshot(toolio.SnapshotOptions{PersistIfTruncated: true})
	if err != nil {
		return agent.AgentToolResult{}, err
	}
	text, _ = formatBashOutput(snapshot, acc, "")
	if snapshot.Truncation.Truncated {
		details.FullOutputPath = snapshot.FullOutputPath
		snapshot.Truncation.Content = ""
		details.Truncation = &snapshot.Truncation
	}
	return textResult(text, details), nil
}

type codingOutputError struct {
	cause error
	text  string
}

func (err *codingOutputError) Error() string { return err.text }
func (err *codingOutputError) Unwrap() error { return err.cause }

func codingToolError(err error, options ToolsOptions) error {
	if err == nil {
		return nil
	}
	diagnostic := err.Error()
	if len(diagnostic) <= truncate.DefaultMaxBytes && strings.Count(diagnostic, "\n") < truncate.DefaultMaxLines {
		return err
	}
	result, spillErr := codingTextResult(diagnostic, codingToolDetails{}, options)
	if spillErr != nil {
		return fmt.Errorf("large coding-tool diagnostic could not be stored: %w", spillErr)
	}
	return &codingOutputError{cause: err, text: result.Content[0].Text}
}

func astLanguage(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".go":
		return "go"
	case ".js", ".jsx", ".mjs", ".cjs":
		return "javascript"
	case ".ts", ".mts", ".cts":
		return "typescript"
	case ".tsx":
		return "tsx"
	case ".py":
		return "python"
	case ".rs":
		return "rust"
	case ".c", ".h":
		return "c"
	case ".cc", ".cpp", ".hpp", ".cxx":
		return "cpp"
	case ".java":
		return "java"
	case ".json":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	case ".html":
		return "html"
	case ".css":
		return "css"
	case ".rb":
		return "ruby"
	case ".sh":
		return "bash"
	case ".cs":
		return "csharp"
	case ".kt", ".kts":
		return "kotlin"
	case ".swift":
		return "swift"
	case ".php":
		return "php"
	case ".scala":
		return "scala"
	case ".lua":
		return "lua"
	}
	return ""
}
