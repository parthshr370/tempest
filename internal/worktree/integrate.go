package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ErrNotGit reports a workspace integrate cannot merge into: no git binary
// or no repository around the working directory.
var ErrNotGit = errors.New("integration needs a git repository")

// Integration statuses of a path the child changed.
const (
	// IntegrationClean: only the child changed the file since its base.
	IntegrationClean = "clean"
	// IntegrationMerged: the parent changed it too and git merged both
	// without overlap; textually clean is not semantically proven.
	IntegrationMerged = "merged"
	// IntegrationConflict: overlapping changes, or a change (symlink,
	// submodule, file/directory swap) integrate will not write.
	IntegrationConflict = "conflict"
)

const (
	integrateTimeout = 60 * time.Second
	// maxMergedFileBytes bounds one conflicted file read back from git.
	maxMergedFileBytes = MaxPatchBytes
	// maxIntegrationBytes bounds every merged file of one integration read
	// back together.
	maxIntegrationBytes = 4 * MaxPatchBytes
	// maxConflictReport bounds the marker hunks shown per conflicted file.
	maxConflictReport = 4 << 10
	regularMode       = "100644"
	executableMode    = "100755"
	absentMode        = "000000"
	// binaryProbeBytes is how much of a file git itself inspects for NUL
	// bytes when deciding a file is binary.
	binaryProbeBytes = 8000
	// maxObjectIDLine bounds one line of git hash-object output: a SHA-256
	// id and its newline.
	maxObjectIDLine = 65
	// maxMergeOutput bounds merge-tree's tree id, paths and messages.
	maxMergeOutput = 1 << 20
)

// IntegratedFile is one path's integration outcome.
type IntegratedFile struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	// Conflict holds a conflicted text file's marker hunks with line
	// numbers, bounded; the full file is in the merged tree.
	Conflict string `json:"conflict,omitempty"`
	Message  string `json:"message,omitempty"`
}

// FileWrite is one live-workspace change that turns the parent snapshot
// into the merged result.
type FileWrite struct {
	// Path is relative to the repository root, slash separated.
	Path   string
	Delete bool
	// Data is the merged content with the repository's checkout filters
	// applied, as git checkout would write it.
	Data       []byte
	Executable bool
	// ModeChanged reports that the executable bit differs from the snapshot.
	ModeChanged bool
	// Snapshot is the blob id the path had in the parent snapshot, empty
	// when it did not exist there. The live file must still hash to it.
	Snapshot string
}

// Integration is a child patch three-way merged onto a snapshot of the
// parent's live files.
type Integration struct {
	Repo string
	// Merged is the merged tree, conflict markers included, for inspection
	// with git show <tree>:<path>.
	Merged string
	Files  []IntegratedFile
	// Writes is set only when no file conflicts.
	Writes []FileWrite
}

// Conflicts counts the conflicted files.
func (in *Integration) Conflicts() int {
	count := 0
	for _, file := range in.Files {
		if file.Status == IntegrationConflict {
			count++
		}
	}
	return count
}

// IntegrateLabels name the two sides in conflict markers.
type IntegrateLabels struct {
	Parent string
	Child  string
}

// Integrate three-way merges the child patch at patchPath, made against
// base, onto the current state of the repository around cwd. The parent
// state is snapshotted with a private index (tracked, untracked and
// uncommitted files; ignored files excluded), the child's tree is rebuilt by
// applying the patch to base in another private index, and git merge-tree
// merges the two in the object database. Nothing touches the user's index,
// HEAD, refs or working files; the only residue is unreferenced objects that
// git gc prunes.
func Integrate(ctx context.Context, cwd, base, patchPath string, labels IntegrateLabels) (*Integration, error) {
	ctx, cancel := context.WithTimeout(ctx, integrateTimeout)
	defer cancel()
	if _, err := exec.LookPath("git"); err != nil {
		return nil, fmt.Errorf("%w: git is not installed", ErrNotGit)
	}
	repo, err := git(ctx, cwd, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %s is not inside one", ErrNotGit, cwd)
	}
	if repo, err = filepath.EvalSymlinks(repo); err != nil {
		return nil, err
	}
	if _, err := git(ctx, repo, nil, "cat-file", "-e", base+"^{commit}"); err != nil {
		return nil, fmt.Errorf("the child's base commit %s is no longer in %s (pruned after its worktree was cleaned up?): %w", base, repo, err)
	}
	scratch, err := os.MkdirTemp("", "tempest-integrate-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)
	childTree, err := childTree(ctx, repo, base, patchPath, filepath.Join(scratch, "child.index"))
	if err != nil {
		return nil, err
	}
	parentTree, err := snapshotTree(ctx, repo, filepath.Join(scratch, "parent.index"))
	if err != nil {
		return nil, err
	}
	parentCommit, err := wrapTree(ctx, repo, parentTree, "tempest-integration-parent")
	if err != nil {
		return nil, err
	}
	childCommit, err := wrapTree(ctx, repo, childTree, "tempest-integration-child")
	if err != nil {
		return nil, err
	}
	merged, conflicted, messages, err := mergeTrees(ctx, repo, base, parentCommit, childCommit)
	if err != nil {
		return nil, err
	}
	integration := &Integration{Repo: repo, Merged: merged}
	childChanged, err := changedPaths(ctx, repo, base, childTree)
	if err != nil {
		return nil, err
	}
	parentChanged, err := changedPaths(ctx, repo, base, parentTree)
	if err != nil {
		return nil, err
	}
	markers := markerLabels{parent: parentCommit, base: base, child: childCommit, labels: labels}
	for _, path := range slices.Sorted(maps.Keys(childChanged)) {
		file := IntegratedFile{Path: path, Status: IntegrationClean, Message: messages[path]}
		if parentChanged[path] {
			file.Status = IntegrationMerged
		}
		if conflicted[path] {
			file.Status = IntegrationConflict
			if file.Conflict, err = conflictHunks(ctx, repo, merged, path, markers); err != nil {
				return nil, err
			}
		}
		integration.Files = append(integration.Files, file)
	}
	// A rename or directory conflict can name a path the child never touched.
	for _, path := range slices.Sorted(maps.Keys(conflicted)) {
		if !childChanged[path] {
			integration.Files = append(integration.Files, IntegratedFile{Path: path, Status: IntegrationConflict, Message: messages[path]})
		}
	}
	if integration.Conflicts() > 0 {
		return integration, nil
	}
	writes, err := liveWrites(ctx, repo, parentTree, merged)
	if err != nil {
		return nil, err
	}
	for _, write := range writes {
		if write.unsupported == "" {
			integration.Writes = append(integration.Writes, write.FileWrite)
			continue
		}
		for index := range integration.Files {
			if integration.Files[index].Path == write.Path {
				integration.Files[index].Status = IntegrationConflict
				integration.Files[index].Message = write.unsupported
			}
		}
	}
	if integration.Conflicts() > 0 {
		integration.Writes = nil
	}
	return integration, nil
}

// BlobIDs hashes live files the way git add would, clean filters and
// line-ending normalization included, without writing objects: one git
// process for every repository-relative path. The caller has checked that
// each path is a regular file.
func BlobIDs(ctx context.Context, repo string, paths []string) (map[string]string, error) {
	ids := make(map[string]string, len(paths))
	if len(paths) == 0 {
		return ids, nil
	}
	for _, path := range paths {
		if strings.ContainsAny(path, "\n") {
			return nil, fmt.Errorf("cannot hash a path containing a newline: %q", path)
		}
	}
	cmd := command(ctx, repo, nil, "hash-object", "--stdin-paths")
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\n") + "\n")
	out, err := runBounded(cmd, int64(len(paths))*maxObjectIDLine)
	if err != nil {
		return nil, fmt.Errorf("hash live files: %w", err)
	}
	lines := strings.Fields(string(out))
	if len(lines) != len(paths) {
		return nil, fmt.Errorf("hash live files: git returned %d ids for %d paths", len(lines), len(paths))
	}
	for index, path := range paths {
		ids[path] = lines[index]
	}
	return ids, nil
}

// childTree rebuilds the child's final tree: base plus its patch.
func childTree(ctx context.Context, repo, base, patchPath, index string) (string, error) {
	env := []string{"GIT_INDEX_FILE=" + index}
	if _, err := git(ctx, repo, env, "read-tree", base); err != nil {
		return "", err
	}
	info, err := os.Stat(patchPath)
	if err != nil {
		return "", err
	}
	if info.Size() > 0 {
		if _, err := git(ctx, repo, env, "apply", "--cached", "--binary", patchPath); err != nil {
			return "", fmt.Errorf("the child patch does not apply to its own base: %w", err)
		}
	}
	return git(ctx, repo, env, "write-tree")
}

// snapshotTree records the parent's nonignored working files, uncommitted
// and untracked ones included, the way Plan pins a dirty base.
func snapshotTree(ctx context.Context, repo, index string) (string, error) {
	env := []string{"GIT_INDEX_FILE=" + index}
	if _, err := git(ctx, repo, env, "read-tree", "HEAD"); err != nil {
		return "", err
	}
	if _, err := git(ctx, repo, env, "add", "-A", "--", "."); err != nil {
		return "", err
	}
	return git(ctx, repo, env, "write-tree")
}

// wrapTree makes an unreferenced commit of tree: merge-tree before git 2.45
// takes commits, not trees.
func wrapTree(ctx context.Context, repo, tree, message string) (string, error) {
	identity := []string{"GIT_AUTHOR_NAME=Tempest", "GIT_AUTHOR_EMAIL=tempest@localhost", "GIT_COMMITTER_NAME=Tempest", "GIT_COMMITTER_EMAIL=tempest@localhost"}
	return git(ctx, repo, identity, "commit-tree", tree, "-m", message)
}

// mergeTrees runs git merge-tree and parses its -z --name-only output: the
// merged tree, the conflicted paths and one message per path.
func mergeTrees(ctx context.Context, repo, base, parent, child string) (string, map[string]bool, map[string]string, error) {
	cmd := command(ctx, repo, nil, "-c", "merge.conflictStyle=diff3", "merge-tree", "--write-tree", "-z", "--name-only", "--messages", "--merge-base="+base, parent, child)
	out, err := runBounded(cmd, maxMergeOutput)
	var exit *exec.ExitError
	// Exit status 1 is a merge with conflicts, not a failure.
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) {
		return "", nil, nil, fmt.Errorf("git merge-tree (needs git 2.40 or newer): %w", err)
	}
	fields := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if len(fields) == 0 || fields[0] == "" {
		return "", nil, nil, errors.New("git merge-tree printed no tree")
	}
	merged := fields[0]
	conflicted := map[string]bool{}
	next := 1
	for ; next < len(fields) && fields[next] != ""; next++ {
		conflicted[fields[next]] = true
	}
	messages := map[string]string{}
	for next++; next < len(fields); {
		count, err := strconv.Atoi(fields[next])
		if err != nil || next+count+2 >= len(fields) {
			break
		}
		text := strings.TrimSpace(fields[next+count+2])
		// Routine "Auto-merging" notes repeat the status; keep conflicts.
		if !strings.HasPrefix(text, "CONFLICT") {
			next += count + 3
			continue
		}
		for _, path := range fields[next+1 : next+1+count] {
			messages[path] = text
		}
		next += count + 3
	}
	return merged, conflicted, messages, nil
}

// changedPaths lists the paths that differ between two trees, renames split
// into a delete and an add.
func changedPaths(ctx context.Context, repo, from, to string) (map[string]bool, error) {
	out, err := git(ctx, repo, nil, "diff-tree", "-r", "-z", "--no-renames", "--name-only", from, to)
	if err != nil {
		return nil, err
	}
	paths := map[string]bool{}
	for _, path := range strings.Split(out, "\x00") {
		if path != "" {
			paths[path] = true
		}
	}
	return paths, nil
}

// markerLabels rewrites merge-tree's object-id conflict labels into names a
// reader can act on.
type markerLabels struct {
	parent, base, child string
	labels              IntegrateLabels
}

func (m markerLabels) rewrite(line string) string {
	for _, pair := range [][2]string{
		{"<<<<<<< " + m.parent, "<<<<<<< " + m.labels.Parent},
		{"||||||| " + m.base, "||||||| base (the child's starting point)"},
		{">>>>>>> " + m.child, ">>>>>>> " + m.labels.Child},
	} {
		if strings.HasPrefix(line, pair[0]) {
			return pair[1] + strings.TrimPrefix(line, pair[0])
		}
	}
	return line
}

// conflictHunks returns a conflicted file's marker regions with line
// numbers, bounded to maxConflictReport.
func conflictHunks(ctx context.Context, repo, tree, path string, markers markerLabels) (string, error) {
	if _, err := git(ctx, repo, nil, "cat-file", "-e", tree+":"+path); err != nil {
		// A delete/modify conflict leaves no merged blob at the path; its
		// message says which side deleted it.
		return "", nil
	}
	data, err := runBounded(command(ctx, repo, nil, "cat-file", "blob", tree+":"+path), maxMergedFileBytes)
	if err != nil {
		return "", fmt.Errorf("read conflicted %s: %w", path, err)
	}
	if bytes.IndexByte(data[:min(len(data), binaryProbeBytes)], 0) >= 0 {
		return "binary file: both sides changed it", nil
	}
	var report strings.Builder
	inside := false
	for number, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "<<<<<<< ") {
			inside = true
		}
		if inside {
			row := fmt.Sprintf("%d:%s\n", number+1, markers.rewrite(line))
			if report.Len()+len(row) > maxConflictReport {
				report.WriteString("[more conflict lines omitted]\n")
				break
			}
			report.WriteString(row)
		}
		if strings.HasPrefix(line, ">>>>>>> ") {
			inside = false
		}
	}
	return strings.TrimSuffix(report.String(), "\n"), nil
}

// liveWrite is a FileWrite, or the reason integrate will not write a path.
type liveWrite struct {
	FileWrite
	unsupported string
}

// liveWrites lists the changes from the parent snapshot to the merged tree:
// exactly what writing the child's contribution into the live files means.
func liveWrites(ctx context.Context, repo, parentTree, merged string) ([]liveWrite, error) {
	out, err := git(ctx, repo, nil, "diff-tree", "-r", "-z", "--no-renames", parentTree, merged)
	if err != nil {
		return nil, err
	}
	fields := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	var writes []liveWrite
	var reads []blobAtPath
	var readers []int
	for index := 0; index+1 < len(fields); index += 2 {
		// :<old mode> <new mode> <old id> <new id> <status>
		meta := strings.Fields(strings.TrimPrefix(fields[index], ":"))
		path := fields[index+1]
		if len(meta) != 5 {
			return nil, fmt.Errorf("unexpected diff-tree line %q", fields[index])
		}
		oldMode, newMode, oldID, newID, status := meta[0], meta[1], meta[2], meta[3], meta[4]
		write := liveWrite{FileWrite: FileWrite{Path: path}}
		if oldMode != absentMode {
			write.Snapshot = oldID
		}
		if oldMode != absentMode && oldMode != regularMode && oldMode != executableMode {
			write.unsupported = "the live path is a symlink or submodule (mode " + oldMode + "); apply this change by hand"
		}
		switch {
		case status == "D":
			write.Delete = true
		case newMode != regularMode && newMode != executableMode:
			write.unsupported = "the merged path is a symlink or submodule (mode " + newMode + "); apply this change by hand"
		default:
			write.Executable = newMode == executableMode
			write.ModeChanged = oldMode != absentMode && oldMode != newMode
			reads = append(reads, blobAtPath{id: newID, path: path})
			readers = append(readers, len(writes))
		}
		writes = append(writes, write)
	}
	contents, err := filteredBlobs(ctx, repo, reads)
	if err != nil {
		return nil, err
	}
	for index, content := range contents {
		writes[readers[index]].Data = content
	}
	return writes, nil
}

// blobAtPath names a blob and the path whose attributes filter it.
type blobAtPath struct {
	id   string
	path string
}

// filteredBlobs reads blobs as git checkout would write them (each path's
// smudge and line-ending filters applied) in one git cat-file process.
func filteredBlobs(ctx context.Context, repo string, blobs []blobAtPath) ([][]byte, error) {
	if len(blobs) == 0 {
		return nil, nil
	}
	var input strings.Builder
	for _, blob := range blobs {
		if strings.ContainsAny(blob.path, "\n") {
			return nil, fmt.Errorf("integrate cannot write a path containing a newline: %q", blob.path)
		}
		input.WriteString(blob.id + " " + blob.path + "\n")
	}
	cmd := command(ctx, repo, nil, "cat-file", "--batch", "--filters")
	cmd.Stdin = strings.NewReader(input.String())
	out, err := runBounded(cmd, maxIntegrationBytes)
	if err != nil {
		return nil, fmt.Errorf("read merged files: %w", err)
	}
	contents := make([][]byte, 0, len(blobs))
	for _, blob := range blobs {
		// Each object is "<id> <type> <size>\n<content>\n".
		header, rest, found := bytes.Cut(out, []byte("\n"))
		fields := strings.Fields(string(header))
		if !found || len(fields) != 3 || fields[1] != "blob" {
			return nil, fmt.Errorf("read merged %s: unexpected cat-file header %q", blob.path, header)
		}
		size, err := strconv.Atoi(fields[2])
		if err != nil || size+1 > len(rest) {
			return nil, fmt.Errorf("read merged %s: truncated cat-file output", blob.path)
		}
		contents = append(contents, rest[:size:size])
		out = rest[size+1:]
	}
	return contents, nil
}

// runBounded runs cmd and returns its stdout, refusing more than limit bytes.
func runBounded(cmd *exec.Cmd, limit int64) ([]byte, error) {
	var out, diagnostic bytes.Buffer
	cmd.Stdout = &boundedWriter{w: &out, left: limit}
	cmd.Stderr = &boundedWriter{w: &diagnostic, left: 4096}
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("%w: %s", err, strings.TrimSpace(diagnostic.String()))
	}
	return out.Bytes(), nil
}
