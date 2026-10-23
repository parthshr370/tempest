package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/editdiff"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/hashline"
	"go.harness.dev/harness/internal/pathutil"
	"go.harness.dev/harness/internal/schema"
	"go.harness.dev/harness/internal/toolio"
)

// hashlineDescription is the model-facing grammar, a compact form of omp's
// hashline prompt.
const hashlineDescription = "Edit existing files with a line-anchored patch. `input` holds one or more sections, each a `[PATH#TAG]` header copied from your latest read or grep output (TAG: 4 hex) followed by ops. Line numbers are the ones that output showed; they name original lines and never shift between hunks of one patch.\n" +
	"\n" +
	"Ops (headers ending in `:` take `+` body rows; the others take none):\n" +
	"PUT N.=M:  replace lines N-M with the body (one line: PUT N.=N:)\n" +
	"PUT N*:    replace the construct opening on line N (function, statement block, Markdown section)\n" +
	"PUT <N:    insert the body before line N (PUT <1: is the file head)\n" +
	"PUT >N:    insert the body after line N (PUT >$: is the file tail)\n" +
	"PUT >N*:   insert the body after the construct opening on line N, at its depth\n" +
	"CUT N.=M / CUT N*  delete lines or a construct, capturing them; add ` @name` to fill a named register\n" +
	"PUT <N / PUT >N / PUT >N* / PUT >$  paste the last unlabeled CUT of this call; add ` @name` to paste a named register\n" +
	"PUT N.=M @name / PUT N* @name  replace lines or a construct with a named register\n" +
	"REM        delete the file\n" +
	"MV DEST    move the file (quote paths with spaces); the section's edits apply first\n" +
	"\n" +
	"Body rows are `+TEXT`, verbatim, indentation after the `+`; a lone `+` is a blank line. There are no `-` or context rows: the range removes the old lines and the body is their complete final content. A literal line starting with `-` or `+` is written `+- item` / `++x`.\n" +
	"\n" +
	"Rules:\n" +
	"- Touch only lines read or grep displayed under the tag; edits to other lines are rejected (re-read them).\n" +
	"- Keep ranges to the changed lines; add lines with PUT <N:/PUT >N: rather than widening a range over lines you keep; separate non-adjacent changes into separate hunks; hunks may not overlap.\n" +
	"- `N*` anchors on the construct's opening line (point at its doc comment, decorator or attribute to include it), never a closing brace or a single-line statement. Supported for Go, Markdown, JS/TS, Java, C/C++, Rust, C#, Kotlin, Swift, PHP, Scala, JSON, Python and YAML; elsewhere use explicit ranges.\n" +
	"- Every edit changes the tag and renumbers lines: chain the next edit from the [path#TAG] header and numbers in this tool's result, or re-read. A stale tag is rejected.\n" +
	"- Moves: `CUT 10.=20 @fn` then `PUT >40 @fn`, in one section, across sections, or across calls (named registers persist; the unlabeled register lasts one call).\n" +
	"- Every section is validated before any file is written. Create new files with write; format with the project formatter, not this tool.\n" +
	"\n" +
	"Example:\n" +
	"[greet.py#A1B2]\n" +
	"PUT 1*:\n" +
	"+def greet(name):\n" +
	"+    print(f\"Hi, {name}\")\n" +
	"CUT 9.=12 @helper\n" +
	"[util.py#3C4D]\n" +
	"PUT >$ @helper"

// hashlineTarget is a patch path resolved inside the workspace.
type hashlineTarget struct {
	abs       string
	rel       string
	canonical string
}

// hashlineFS gives hashline.Prepare containment-checked reads.
type hashlineFS struct {
	root    *os.Root
	targets map[string]hashlineTarget
}

func (f hashlineFS) Key(path string) (string, error) {
	return f.targets[path].canonical, nil
}

func (f hashlineFS) Read(path string) ([]byte, error) {
	return readFileInRoot(f.root, f.targets[path].rel)
}

// hashlineFileResult is one file's entry in the tool's details.
type hashlineFileResult struct {
	Path      string `json:"path"`
	Op        string `json:"op"`
	Tag       string `json:"tag,omitempty"`
	MovedFrom string `json:"moved_from,omitempty"`
}

// hashlineDetails is the hashline tool's structured result.
type hashlineDetails struct {
	Files    []hashlineFileResult `json:"files"`
	Warnings []string             `json:"warnings,omitempty"`
}

func newHashlineTool(cwd string, options ToolsOptions) agent.AgentTool {
	queue := mutationQueue(options)
	store := options.Hashline
	if store == nil {
		store = hashline.NewStore()
	}
	getRoot := rootOnce(cwd)
	return agent.AgentTool{
		Tool: types.Tool{
			Name:        string(HashlineTool),
			Description: hashlineDescription,
			Parameters: schema.Object(schema.JSON{
				"input": schema.JSON{"type": "string", "description": "the hashline patch: [PATH#TAG] sections with PUT/CUT/REM/MV ops"},
			}, "input"),
		},
		Label: string(HashlineTool),
		Execute: func(ctx context.Context, _ string, params json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			var input struct {
				Input string `json:"input"`
			}
			if err := decodeParams(params, &input); err != nil {
				return agent.AgentToolResult{}, err
			}
			patch, err := hashline.Parse(input.Input)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			root, err := getRoot()
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			targets := map[string]hashlineTarget{}
			for _, path := range patch.Paths() {
				abs := pathutil.ResolveToCwd(path, cwd)
				rel, canonical, err := rootRelPaths(cwd, abs)
				if err != nil {
					return agent.AgentToolResult{}, fmt.Errorf("%s: %w", path, err)
				}
				targets[path] = hashlineTarget{abs: abs, rel: rel, canonical: canonical}
			}
			var text string
			var details hashlineDetails
			err = lockFiles(queue, targets, func() error {
				if err := abortErr(ctx); err != nil {
					return err
				}
				plan, err := hashline.Prepare(patch, hashlineFS{root: root, targets: targets}, store)
				if err != nil {
					return err
				}
				if err := abortErr(ctx); err != nil {
					return err
				}
				text, details, err = commitHashlinePlan(plan, root, targets, store, options)
				return err
			})
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			return textResult(text, details), nil
		},
	}
}

// lockFiles runs fn holding the mutation lock of every target, taken in
// canonical-path order so concurrent multi-file patches cannot deadlock.
// Targets naming the same file share one lock.
func lockFiles(queue *toolio.FileMutationQueue, targets map[string]hashlineTarget, fn func() error) error {
	byCanonical := map[string]string{}
	for _, target := range targets {
		byCanonical[target.canonical] = target.abs
	}
	canonicals := make([]string, 0, len(byCanonical))
	for canonical := range byCanonical {
		canonicals = append(canonicals, canonical)
	}
	sort.Strings(canonicals)
	var lockNext func(i int) error
	lockNext = func(i int) error {
		if i == len(canonicals) {
			return fn()
		}
		return queue.Do(byCanonical[canonicals[i]], func() error { return lockNext(i + 1) })
	}
	return lockNext(0)
}

// commitHashlinePlan writes a validated plan in patch order and renders the
// result. A write failure stops the commit and names what landed.
func commitHashlinePlan(plan *hashline.Plan, root *os.Root, targets map[string]hashlineTarget, store *hashline.Store, options ToolsOptions) (string, hashlineDetails, error) {
	var sections []string
	details := hashlineDetails{Files: []hashlineFileResult{}, Warnings: plan.Warnings}
	for index, change := range plan.Changes {
		source := targets[change.Path]
		if err := commitHashlineChange(change, root, source, targets[change.Dest]); err != nil {
			var written, pending []string
			for _, done := range plan.Changes[:index] {
				written = append(written, done.Path)
			}
			for _, rest := range plan.Changes[index+1:] {
				pending = append(pending, rest.Path)
			}
			message := fmt.Sprintf("writing %s failed: %v.", change.Path, err)
			if len(written) > 0 {
				message += " Already written: " + strings.Join(written, ", ") + "."
			}
			if len(pending) > 0 {
				message += " Not written: " + strings.Join(pending, ", ") + "."
			}
			return "", details, errors.New(message)
		}
		notifyFileChanged(options, source.canonical)
		tag, preview := store.Committed(change)
		switch change.Op {
		case hashline.OpRemove:
			sections = append(sections, "Deleted "+change.Path)
			details.Files = append(details.Files, hashlineFileResult{Path: change.Path, Op: "delete"})
			continue
		case hashline.OpMove:
			notifyFileChanged(options, targets[change.Dest].canonical)
			details.Files = append(details.Files, hashlineFileResult{Path: change.Dest, Op: "move", Tag: tag, MovedFrom: change.Path})
		default:
			details.Files = append(details.Files, hashlineFileResult{Path: change.Path, Op: "update", Tag: tag})
		}
		path := change.Path
		if change.Op == hashline.OpMove {
			path = change.Dest
		}
		lines := []string{fmt.Sprintf("[%s#%s]", path, tag)}
		if change.Op == hashline.OpMove {
			lines = append(lines, "Moved from "+change.Path)
		}
		lines = append(lines, change.Notes...)
		if preview != "" {
			lines = append(lines, preview)
		}
		sections = append(sections, strings.Join(lines, "\n"))
	}
	store.CommitRegisters(plan)
	text := strings.Join(sections, "\n\n")
	if len(plan.Warnings) > 0 {
		text += "\n\nWarnings:\n" + strings.Join(plan.Warnings, "\n")
	}
	return text, details, nil
}

// commitHashlineChange applies one change to the disk through the root.
func commitHashlineChange(change *hashline.Change, root *os.Root, source, dest hashlineTarget) error {
	switch change.Op {
	case hashline.OpRemove:
		return wrapOutsideRoot(source.rel, root.Remove(source.rel))
	case hashline.OpMove:
		if err := root.MkdirAll(filepath.Dir(dest.rel), 0o755); err != nil {
			return wrapOutsideRoot(dest.rel, err)
		}
		// Rename first so the file keeps its mode, then write the edited
		// content in place.
		if err := root.Rename(source.rel, dest.rel); err != nil {
			return wrapOutsideRoot(dest.rel, err)
		}
		if change.After == change.Before.Text {
			return nil
		}
		return writeFileInRoot(root, dest.rel, change.Encoded)
	default:
		return writeFileInRoot(root, source.rel, change.Encoded)
	}
}

// verifyEditTag checks an edit's 4-hex anchor against the file's live
// content. With a store it applies the same rule as hashline sections (a
// recorded version with exactly this content, or a tag the content hashes
// to); without one, the content hash alone.
func verifyEditTag(store *hashline.Store, canonicalPath, path, tag string, raw []byte) error {
	current := hashline.Tag(raw)
	stale := !strings.EqualFold(tag, current)
	if store != nil && canonicalPath != "" {
		stale = store.Verify(canonicalPath, path, tag, hashline.Decode(raw).Text) != nil
	}
	if stale {
		return &editdiff.StaleAnchorError{Path: path, Expected: strings.ToUpper(tag), Actual: current}
	}
	return nil
}

// taggedResult renders the [path#TAG] header and changed-line preview that
// write and edit append to their results, so the next edit can chain from
// them without a re-read.
func taggedResult(path, tag, preview string) string {
	text := fmt.Sprintf("\n[%s#%s]", path, tag)
	if preview != "" {
		text += "\n" + preview
	}
	return text
}
