package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/editbatch"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/hashline"
	"go.harness.dev/harness/internal/hooks"
	"go.harness.dev/harness/internal/schema"
	"go.harness.dev/harness/internal/worktree"
)

const integrateDescription = "Three-way merge a finished general task's changes into the live workspace. The child's patch is applied against the commit it started from, onto a snapshot of the current files (your uncommitted edits included), so edits on both sides in different places of one file merge instead of colliding. Per file: clean (only the child changed it), merged (both changed it, git merged without overlap) or conflict (overlapping edits, shown with markers). Files are written only when nothing conflicts, and only if the live files still match the snapshot. A merged file is written only with accept_merged: true, after you have reviewed that both changes belong together. Never touches git's index or HEAD."

// liveParentLabel and childLabelPrefix name the sides in conflict markers.
const (
	liveParentLabel  = "parent (live workspace)"
	childLabelPrefix = "child "
	// executableBits are cleared when git's mode drops 100755; readBits
	// shifted right by two are granted when it sets it.
	executableBits fs.FileMode = 0o111
	readBits       fs.FileMode = 0o444
	newExecutable  fs.FileMode = 0o755
	// binaryProbe matches git's binary detection window.
	binaryProbe = 8000
)

// integrateInput is the integrate tool's arguments.
type integrateInput struct {
	JobID        string `json:"job_id"`
	AcceptMerged bool   `json:"accept_merged"`
}

// integrateDetails is the integrate tool's structured result.
type integrateDetails struct {
	JobID      string                    `json:"job_id"`
	State      string                    `json:"state"`
	MergedTree string                    `json:"merged_tree,omitempty"`
	Files      []worktree.IntegratedFile `json:"files"`
	Written    []string                  `json:"written"`
	Conflicts  int                       `json:"conflicts"`
}

func newIntegrateTool(cwd string, options ToolsOptions) agent.AgentTool {
	controller := options.Work
	getRoot := rootOnce(cwd)
	return agent.AgentTool{
		Tool: types.Tool{
			Name:        string(IntegrateTool),
			Description: integrateDescription,
			Parameters: schema.Object(schema.JSON{
				"job_id":        schema.JSON{"type": "string", "description": "the finished general task's job id"},
				"accept_merged": schema.JSON{"type": "boolean", "description": "write files git merged from both sides' changes; default false reports them without writing"},
			}, "job_id"),
		},
		Label: string(IntegrateTool),
		Execute: func(ctx context.Context, toolCallID string, params json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			var input integrateInput
			if err := decodeParams(params, &input); err != nil {
				return agent.AgentToolResult{}, err
			}
			if input.JobID == "" {
				return agent.AgentToolResult{}, errors.New("integrate requires job_id")
			}
			source, err := controller.ChildPatch(ctx, input.JobID)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			integration, err := worktree.Integrate(ctx, cwd, source.BaseCommit, source.Patch.Path, worktree.IntegrateLabels{Parent: liveParentLabel, Child: childLabelPrefix + input.JobID})
			if errors.Is(err, worktree.ErrNotGit) {
				return agent.AgentToolResult{}, fmt.Errorf("%w. Read the child's patch at %s and carry its hunks over with edit or hashline", err, source.Patch.Path)
			}
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			if integration.Repo != source.Repo {
				return agent.AgentToolResult{}, fmt.Errorf("job %s's patch is for the repository at %s, but this workspace is in %s", input.JobID, source.Repo, integration.Repo)
			}
			root, err := getRoot()
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			run := integrationRun{cwd: cwd, root: root, options: options, input: input, integration: integration, callID: toolCallID, origin: "integrate call " + toolCallID}
			text, written, err := run.finish(ctx)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			details := integrateDetails{JobID: input.JobID, State: source.Job.State, MergedTree: integration.Merged, Files: integration.Files, Written: written, Conflicts: integration.Conflicts()}
			if details.Files == nil {
				details.Files = []worktree.IntegratedFile{}
			}
			if details.Written == nil {
				details.Written = []string{}
			}
			header := fmt.Sprintf("integrate %s (job %s):", input.JobID, source.Job.State)
			return textResult(header+"\n"+text, details), nil
		},
	}
}

// integrationRun decides whether a computed integration is written and
// writes it through the file tools' containment, locks and edit journal.
type integrationRun struct {
	cwd         string
	root        *os.Root
	options     ToolsOptions
	input       integrateInput
	integration *worktree.Integration
	origin      string
	callID      string
}

// finish renders the per-file report and writes the merge when allowed. It
// returns the report and the repository-relative paths written.
func (run integrationRun) finish(ctx context.Context) (string, []string, error) {
	in := run.integration
	report := renderIntegration(in)
	if len(in.Files) == 0 {
		return "The child's patch changes nothing; nothing to integrate.", nil, nil
	}
	if conflicts := in.Conflicts(); conflicts > 0 {
		return report + fmt.Sprintf("\n\nNothing written: %d conflicted file(s). The live files still hold the parent's side; carry the child's side of each conflict over with edit or hashline. The full merged file with markers: git show %s:<path>", conflicts, in.Merged), nil, nil
	}
	if merged := countStatus(in, worktree.IntegrationMerged); merged > 0 && !run.input.AcceptMerged {
		return report + fmt.Sprintf("\n\nNothing written: git merged %d file(s) both sides changed. A textual merge is not proof the code still works together; review the changes, then call integrate again with accept_merged: true to write them.", merged), nil, nil
	}
	if len(in.Writes) == 0 {
		return report + "\n\nEvery change is already in the live files; nothing written.", nil, nil
	}
	tagged, written, err := run.write(ctx)
	if err != nil {
		return "", nil, err
	}
	return report + fmt.Sprintf("\n\nWrote %d file(s).", len(written)) + tagged, written, nil
}

// write applies the integration's writes to the live files, holding their
// mutation locks, after proving each still matches the snapshot.
func (run integrationRun) write(ctx context.Context) (string, []string, error) {
	var paths []string
	for _, write := range run.integration.Writes {
		paths = append(paths, filepath.Join(run.integration.Repo, filepath.FromSlash(write.Path)))
	}
	if run.options.ApproveHookSettings != nil {
		if err := run.options.ApproveHookSettings(ctx, string(IntegrateTool), run.callID, paths); err != nil {
			return "", nil, err
		}
	} else {
		for _, path := range paths {
			if hooks.HookSettingsPath(path, run.cwd) {
				return "", nil, errors.New("Changing hook settings requires per-call approval: " + path)
			}
		}
	}
	targets := map[string]hashlineTarget{}
	for _, write := range run.integration.Writes {
		abs := filepath.Join(run.integration.Repo, filepath.FromSlash(write.Path))
		rel, canonical, err := rootRelPaths(run.cwd, abs)
		if err != nil {
			return "", nil, fmt.Errorf("the child changed %s, outside the working directory; nothing written: %w", write.Path, err)
		}
		targets[write.Path] = hashlineTarget{abs: abs, rel: rel, canonical: canonical}
	}
	var tagged strings.Builder
	var written []string
	err := lockFiles(mutationQueue(run.options), targets, func() error {
		if err := abortErr(ctx); err != nil {
			return err
		}
		changes, before, err := run.prepare(ctx, targets)
		if err != nil {
			return err
		}
		if err := editbatch.New(editStateDir(run.options)).Apply(run.root, run.origin, changes); err != nil {
			return err
		}
		for index, write := range run.integration.Writes {
			target := targets[write.Path]
			notifyFileChanged(run.options, target.canonical)
			written = append(written, write.Path)
			if write.Delete || run.options.Hashline == nil || isBinary(write.Data) {
				continue
			}
			tag, preview := run.options.Hashline.Written(target.canonical, hashline.Decode(before[index]).Text, hashline.Decode(write.Data).Text)
			tagged.WriteString(taggedResult(target.rel, tag, preview))
		}
		return nil
	})
	return tagged.String(), written, err
}

// prepare checks every live file against the snapshot and builds the edit
// batch. It returns each path's live bytes for the result previews.
func (run integrationRun) prepare(ctx context.Context, targets map[string]hashlineTarget) ([]editbatch.Change, [][]byte, error) {
	var changes []editbatch.Change
	var before [][]byte
	var drifted, hashed []string
	for _, write := range run.integration.Writes {
		name := filepath.FromSlash(targets[write.Path].canonical)
		live, info, err := readLive(run.root, name)
		if err != nil {
			return nil, nil, err
		}
		switch {
		case info == nil && write.Snapshot != "":
			drifted = append(drifted, write.Path+" (deleted)")
		case info != nil && write.Snapshot == "":
			drifted = append(drifted, write.Path+" (created)")
		case info != nil && !info.Mode().IsRegular():
			drifted = append(drifted, write.Path+" (no longer a regular file)")
		case info != nil:
			hashed = append(hashed, write.Path)
		}
		before = append(before, live)
		changes = append(changes, editbatch.Change{Path: name, Delete: write.Delete, Data: write.Data, Perm: integratedPerm(write, info)})
	}
	ids, err := worktree.BlobIDs(ctx, run.integration.Repo, hashed)
	if err != nil {
		return nil, nil, err
	}
	for _, write := range run.integration.Writes {
		if id, ok := ids[write.Path]; ok && id != write.Snapshot {
			drifted = append(drifted, write.Path+" (modified)")
		}
	}
	if len(drifted) > 0 {
		return nil, nil, fmt.Errorf("live files changed after the integration snapshot: %s. Nothing was written; call integrate again to merge against the current files", strings.Join(drifted, ", "))
	}
	return changes, before, nil
}

// readLive reads a live regular file through the root; a missing file is
// nil info, and any other file type is returned unread for the caller to
// refuse.
func readLive(root *os.Root, name string) ([]byte, fs.FileInfo, error) {
	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return nil, info, err
	}
	data, err := readFileInRoot(root, name)
	return data, info, err
}

// integratedPerm is the permission a written file gets: its live bits, with
// the executable bits git's mode change asks for.
func integratedPerm(write worktree.FileWrite, live fs.FileInfo) fs.FileMode {
	if live == nil {
		if write.Executable {
			return newExecutable
		}
		return 0
	}
	if !write.ModeChanged {
		return 0
	}
	perm := live.Mode().Perm()
	if write.Executable {
		// Grant execute where read is granted, as git checkout does.
		return perm | (perm&readBits)>>2
	}
	return perm &^ executableBits
}

// renderIntegration lists each file's status, with conflict hunks.
func renderIntegration(in *worktree.Integration) string {
	var report strings.Builder
	for index, file := range in.Files {
		if index > 0 {
			report.WriteString("\n")
		}
		fmt.Fprintf(&report, "%-8s %s", file.Status, file.Path)
		if file.Message != "" {
			report.WriteString(" — " + file.Message)
		}
		if file.Conflict != "" {
			report.WriteString("\n" + file.Conflict)
		}
	}
	return report.String()
}

func countStatus(in *worktree.Integration, status string) int {
	count := 0
	for _, file := range in.Files {
		if file.Status == status {
			count++
		}
	}
	return count
}

func isBinary(data []byte) bool {
	return bytes.IndexByte(data[:min(len(data), binaryProbe)], 0) >= 0
}
