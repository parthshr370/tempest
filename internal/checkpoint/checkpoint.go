// Package checkpoint keeps workspace snapshots in a session-private Git store.
package checkpoint

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.harness.dev/harness/internal/editbatch"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/toolio"
)

const (
	maxFile        = 8 << 20
	maxCheckpoints = 32
)

type file struct {
	Hash   string      `json:"hash"`
	Object string      `json:"object"`
	Perm   os.FileMode `json:"perm"`
}

type snapshot struct {
	ID           string            `json:"id"`
	Goal         string            `json:"goal"`
	Created      time.Time         `json:"created"`
	Files        map[string]file   `json:"files"`
	Conversation string            `json:"conversation"`
	Automatic    bool              `json:"automatic,omitempty"`
	Outside      bool              `json:"outside,omitempty"`
	Gap          string            `json:"gap,omitempty"`
	Ignored      []string          `json:"ignored,omitempty"`
	Skipped      map[string]string `json:"skipped,omitempty"`
}

type state struct {
	Snapshots     []snapshot      `json:"snapshots"`
	Expected      map[string]file `json:"expected"`
	Uncertain     bool            `json:"uncertain,omitempty"`
	RewindRequest string          `json:"rewind_request,omitempty"`
	LastGap       string          `json:"last_gap,omitempty"`
}

type pending struct {
	Entry  journal.RewindEntry `json:"entry"`
	Before map[string]file     `json:"before"`
	After  map[string]file     `json:"after"`
	// JournalOffset identifies this transaction's append even after later turns.
	JournalOffset int64 `json:"journal_offset,omitempty"`
}

// Info identifies a retained workspace and conversation checkpoint.
type Info struct {
	ID        string            `json:"id"`
	Goal      string            `json:"goal"`
	Created   time.Time         `json:"created"`
	Automatic bool              `json:"automatic,omitempty"`
	Skipped   map[string]string `json:"skipped,omitempty"`
	Gap       string            `json:"gap,omitempty"`
}

// Manager owns a session's private object store. The session journal owns its lifetime.
type Manager struct {
	mu             sync.Mutex
	cwd, data, dir string
	excludedDir    string
	store          *journal.Store
	queue          *toolio.FileMutationQueue
	state          state
	runtime        Runtime
	request        *rewindRequest
	turn           string
	active         int
	skipped        map[string]string
	unavailable    error
	recovered      []types.Message
	didRecover     bool
}

// New loads checkpoint metadata. Checkpoint failures mustn't prevent session resume.
func New(ctx context.Context, cwd string, store *journal.Store, queue *toolio.FileMutationQueue) (*Manager, error) {
	real, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		real, err = filepath.Abs(cwd)
		if err != nil {
			return nil, err
		}
	}
	m := &Manager{cwd: real, data: store.Path() + ".d", dir: filepath.Join(store.Path()+".d", "checkpoints"), store: store, queue: queue}
	m.excludedDir = filepath.Dir(store.Path())
	if canonical, err := filepath.EvalSymlinks(m.excludedDir); err == nil {
		m.excludedDir = canonical
	}
	if queue == nil {
		m.queue = toolio.NewFileMutationQueue()
	}
	for _, storage := range []string{m.data, m.dir, filepath.Join(m.dir, "git")} {
		info, err := os.Lstat(storage)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			m.unavailable = err
			break
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			m.unavailable = fmt.Errorf("unsafe checkpoint storage directory %s", storage)
			break
		}
	}
	if m.unavailable != nil {
		slog.Warn("checkpoint unavailable", "error", m.unavailable)
		return m, nil
	}
	if err := m.load(); err != nil {
		m.unavailable = err
		slog.Warn("checkpoint metadata unavailable", "error", err)
		return m, nil
	}
	if err := m.recover(ctx); err != nil {
		m.gap("interrupted rewind: " + err.Error())
		// Preserve conflicting workspace bytes, but don't leave a hot transaction that
		// could fork a later conversation on the next open.
		if removeErr := os.Remove(filepath.Join(m.dir, "pending.json")); removeErr != nil && !os.IsNotExist(removeErr) {
			slog.Warn("retiring interrupted rewind", "error", removeErr)
		}
	}
	if m.state.Uncertain {
		m.gap("a mutating call was interrupted before its outcome was captured")
	}
	if err := m.settleRequest(ctx); err != nil {
		slog.Warn("settling abandoned rewind request", "error", err)
	}
	return m, nil
}

func (m *Manager) load() error {
	contents, err := os.ReadFile(filepath.Join(m.dir, "state.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(contents, &m.state)
}

// ReadList reads metadata only: no journal writer, recovery, Git or workspace access.
func ReadList(journalPath string) ([]Info, error) {
	m := &Manager{dir: filepath.Join(journalPath+".d", "checkpoints")}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m.List(), nil
}

func cleanEnv() []string {
	var out []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(key, "GIT_") {
			out = append(out, entry)
		}
	}
	return append(out, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
}

func (m *Manager) git(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir=" + filepath.Join(m.dir, "git"), "--work-tree=" + m.cwd, "-c", "core.fsmonitor=false", "-c", "core.filemode=true", "-c", "core.ignorestat=false"}, args...)...)
	cmd.Env = append(cleanEnv(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ATTR_NOSYSTEM=1", "GIT_INDEX_FILE="+filepath.Join(m.dir, "index"))
	cmd.Dir = m.cwd
	cmd.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	result, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("checkpoint git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return result, nil
}

func (m *Manager) init(ctx context.Context) error {
	if m.unavailable != nil {
		return m.unavailable
	}
	if err := os.MkdirAll(m.dir, 0700); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(m.dir, "git", "HEAD")); os.IsNotExist(err) {
		cmd := exec.CommandContext(ctx, "git", "init", "--bare", "--template=", filepath.Join(m.dir, "git"))
		cmd.Env = append(cleanEnv(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("checkpoint requires git: %w: %s", err, output)
		}
	}
	// Checkpoints keep exact bytes, not the user's clean filters or EOL conversion.
	attributes := filepath.Join(m.dir, "git", "info", "attributes")
	if err := os.MkdirAll(filepath.Dir(attributes), 0700); err != nil {
		return err
	}
	return os.WriteFile(attributes, []byte("* -text -filter -ident -working-tree-encoding\n"), 0600)
}

func (m *Manager) names(ctx context.Context, ignored bool) ([]string, error) {
	args := []string{"ls-files", "-z", "--others", "--exclude-standard"}
	if ignored {
		args = append(args, "--ignored", "--directory")
	} else {
		args = append(args, "--cached")
	}
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.fsmonitor=false", "-C", m.cwd}, append(args, "--full-name", "--", ".")...)...)
	// User ignore configuration belongs to enumeration, not our private index.
	cmd.Env = append(cleanEnv(), "GIT_OPTIONAL_LOCKS=0")
	result, err := cmd.Output()
	if err != nil {
		var commandError *exec.ExitError
		if !errors.As(err, &commandError) || !strings.Contains(string(commandError.Stderr), "not a git repository") {
			return nil, fmt.Errorf("enumerating checkpoint workspace: %w", err)
		}
		private := exec.CommandContext(ctx, "git", append([]string{"--git-dir=" + filepath.Join(m.dir, "git"), "--work-tree=" + m.cwd}, args...)...)
		private.Dir = m.cwd
		private.Env = append(cleanEnv(), "GIT_INDEX_FILE="+filepath.Join(m.dir, "index"), "GIT_OPTIONAL_LOCKS=0")
		result, err = private.Output()
		if err != nil {
			return nil, err
		}
	} else {
		top := exec.CommandContext(ctx, "git", "-C", m.cwd, "rev-parse", "--show-toplevel")
		top.Env = cleanEnv()
		root, err := top.Output()
		if err != nil {
			return nil, err
		}
		prefix, err := filepath.Rel(strings.TrimSpace(string(root)), m.cwd)
		if err != nil {
			return nil, err
		}
		if prefix != "." {
			var relative []string
			for _, path := range strings.Split(string(result), "\x00") {
				if path == "" {
					continue
				}
				name, err := filepath.Rel(prefix, path)
				if err != nil {
					return nil, err
				}
				if strings.HasSuffix(path, "/") {
					name += "/"
				}
				relative = append(relative, name)
			}
			return relative, nil
		}
	}
	return strings.Split(strings.TrimSuffix(string(result), "\x00"), "\x00"), nil
}

func (m *Manager) scan(ctx context.Context, known map[string]file) (map[string]file, error) {
	if err := m.init(ctx); err != nil {
		return nil, err
	}
	names, err := m.names(ctx, false)
	if err != nil {
		return nil, err
	}
	for path := range known {
		names = append(names, path)
	}
	slices.Sort(names)
	names = slices.Compact(names)
	root, err := os.OpenRoot(m.cwd)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	out := map[string]file{}
	m.skipped = map[string]string{}
	var add bytes.Buffer
	for _, path := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if path == "" {
			continue
		}
		path = filepath.ToSlash(path)
		if !filepath.IsLocal(path) || strings.Contains(path, "\x00") {
			return nil, fmt.Errorf("unsafe checkpoint path %q", path)
		}
		parts := strings.Split(path, "/")
		if slices.Contains(parts, ".git") {
			continue
		}
		absolute := filepath.Join(m.cwd, filepath.FromSlash(path))
		if relative, err := filepath.Rel(m.excludedDir, absolute); err == nil && (relative == "." || filepath.IsLocal(relative)) {
			continue
		}
		var info os.FileInfo
		for i := range parts {
			info, err = root.Lstat(strings.Join(parts[:i+1], "/"))
			if os.IsNotExist(err) {
				break
			}
			if err != nil {
				return nil, err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				m.skipped[path] = "symlink (not followed)"
				break
			}
		}
		if os.IsNotExist(err) || m.skipped[path] != "" {
			continue
		}
		if !info.Mode().IsRegular() {
			m.skipped[path] = "directory, nested repository or non-regular file"
			continue
		}
		if info.Size() > maxFile {
			m.skipped[path] = "file exceeds 8 MiB"
			continue
		}
		out[path] = file{Perm: info.Mode().Perm()}
		add.WriteString(path)
		add.WriteByte(0)
	}
	// Keep Git's index and its stat cache across captures. Only changed files get
	// rehashed, and a single process handles the whole path batch.
	cached, err := m.git(ctx, nil, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	var remove bytes.Buffer
	for _, path := range strings.Split(string(cached), "\x00") {
		if _, exists := out[path]; path != "" && !exists {
			remove.WriteString(path)
			remove.WriteByte(0)
		}
	}
	if remove.Len() > 0 {
		if _, err := m.git(ctx, remove.Bytes(), "update-index", "--force-remove", "-z", "--stdin"); err != nil {
			return nil, err
		}
	}
	if add.Len() > 0 {
		if _, err := m.git(ctx, add.Bytes(), "update-index", "--add", "--remove", "-z", "--stdin"); err != nil {
			return nil, err
		}
	}
	staged, err := m.git(ctx, nil, "ls-files", "--stage", "-z")
	if err != nil {
		return nil, err
	}
	for _, record := range strings.Split(string(staged), "\x00") {
		metadata, path, ok := strings.Cut(record, "\t")
		if !ok {
			continue
		}
		fields := strings.Fields(metadata)
		version, exists := out[path]
		if !exists || len(fields) != 3 {
			return nil, fmt.Errorf("invalid checkpoint index record %q", record)
		}
		version.Hash, version.Object = fields[1], fields[1]
		out[path] = version
	}
	return out, nil
}

func same(a, b map[string]file) bool {
	if len(a) != len(b) {
		return false
	}
	for path, version := range a {
		other, exists := b[path]
		if !exists || version.Hash != other.Hash || version.Perm != other.Perm {
			return false
		}
	}
	return true
}

func persist[T state | pending](m *Manager, name string, value T) error {
	contents, err := json.Marshal(value)
	if err != nil {
		return err
	}
	staged, err := os.CreateTemp(m.dir, ".state-")
	if err != nil {
		return err
	}
	defer os.Remove(staged.Name())
	if err = staged.Chmod(0600); err == nil {
		_, err = staged.Write(contents)
	}
	if err == nil {
		err = staged.Sync()
	}
	err = errors.Join(err, staged.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(staged.Name(), filepath.Join(m.dir, name)); err != nil {
		return err
	}
	directory, err := os.Open(m.dir)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func (m *Manager) lock(fn func() error) error {
	return m.queue.Do(filepath.Join(m.cwd, ".tempest-workspace-mutation"), func() error {
		// The per-user directory and O_NOFOLLOW prevent another user from planting
		// a lock symlink in the shared temporary directory.
		lockDir := filepath.Join(os.TempDir(), fmt.Sprintf("tempest-locks-%d", os.Getuid()))
		if err := os.Mkdir(lockDir, 0700); err != nil && !os.IsExist(err) {
			return err
		}
		info, err := os.Lstat(lockDir)
		if err != nil {
			return err
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode().Perm() != 0700 || owner.Uid != uint32(os.Getuid()) {
			return fmt.Errorf("unsafe checkpoint lock directory %s", lockDir)
		}
		hash := sha256.Sum256([]byte(m.cwd))
		lockPath := filepath.Join(lockDir, hex.EncodeToString(hash[:])+".lock")
		fd, err := syscall.Open(lockPath, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
		if err != nil {
			return err
		}
		lockFile := os.NewFile(uintptr(fd), lockPath)
		defer lockFile.Close()
		if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			return fmt.Errorf("checkpoint workspace busy: %w", err)
		}
		defer syscall.Flock(fd, syscall.LOCK_UN)
		return fn()
	})
}

func (m *Manager) capture(ctx context.Context, goal string, messages []types.Message, automatic ...bool) (Info, error) {
	files, err := m.scan(ctx, m.state.Expected)
	if err != nil {
		return Info{}, err
	}
	if m.active == 0 {
		m.observeOutside(files)
	}
	raw, err := journal.NewRewindEntry("checkpoint", "", messages)
	if err != nil {
		return Info{}, err
	}
	conversation, err := json.Marshal(raw.Messages)
	if err != nil {
		return Info{}, err
	}
	blob, err := m.git(ctx, conversation, "hash-object", "-w", "--stdin")
	if err != nil {
		return Info{}, err
	}
	ignored, err := m.names(ctx, true)
	if err != nil {
		return Info{}, err
	}
	var entropy [12]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return Info{}, err
	}
	id := hex.EncodeToString(entropy[:])
	tree, err := m.git(ctx, nil, "write-tree")
	if err != nil {
		return Info{}, err
	}
	for ref, object := range map[string]string{"refs/checkpoints/" + id: strings.TrimSpace(string(tree)), "refs/conversations/" + id: strings.TrimSpace(string(blob))} {
		if _, err := m.git(ctx, nil, "update-ref", ref, object); err != nil {
			return Info{}, err
		}
	}
	cp := snapshot{ID: id, Goal: goal, Created: time.Now().UTC(), Files: files, Conversation: strings.TrimSpace(string(blob)), Ignored: ignored, Skipped: m.skipped, Automatic: len(automatic) > 0 && automatic[0]}
	m.state.Snapshots = append(m.state.Snapshots, cp)
	var expired []snapshot
	count := 0
	for _, retained := range m.state.Snapshots {
		if retained.Automatic == cp.Automatic {
			count++
		}
	}
	retained := make([]snapshot, 0, len(m.state.Snapshots))
	for _, old := range m.state.Snapshots {
		if count > maxCheckpoints && old.Automatic == cp.Automatic {
			expired = append(expired, old)
			count--
			continue
		}
		retained = append(retained, old)
	}
	m.state.Snapshots = retained
	m.state.Expected = files
	if err := persist(m, "state.json", m.state); err != nil {
		return Info{}, err
	}
	for _, old := range expired {
		for _, namespace := range []string{"checkpoints", "conversations"} {
			if _, err := m.git(ctx, nil, "update-ref", "-d", "refs/"+namespace+"/"+old.ID); err != nil {
				return Info{}, err
			}
		}
	}
	if len(expired) > 0 {
		if _, err := m.git(ctx, nil, "prune", "--expire=now"); err != nil {
			return Info{}, err
		}
	}
	return cp.info(), nil
}

func (s snapshot) info() Info {
	return Info{ID: s.ID, Goal: s.Goal, Created: s.Created, Automatic: s.Automatic, Skipped: s.Skipped, Gap: s.Gap}
}

func (m *Manager) gap(reason string) {
	for i := range m.state.Snapshots {
		m.state.Snapshots[i].Gap = reason
	}
	m.state.LastGap = reason
	m.state.Uncertain = false
	m.turn = ""
	if err := persist(m, "state.json", m.state); err != nil {
		slog.Warn("persisting checkpoint capture gap", "reason", reason, "error", err)
	}
	slog.Warn("checkpoint capture gap; tools continue, older rewinds disabled", "reason", reason)
}

// Create captures the workspace and a safe transcript boundary.
func (m *Manager) Create(ctx context.Context, goal string, messages []types.Message) (info Info, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	err = m.lock(func() error {
		var captureErr error
		info, captureErr = m.capture(ctx, goal, messages)
		return captureErr
	})
	if err != nil {
		m.gap(err.Error())
	}
	return
}

// List returns retained checkpoints, oldest first.
func (m *Manager) List() []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Info, 0, len(m.state.Snapshots))
	for _, cp := range m.state.Snapshots {
		info := cp.info()
		info.Skipped = make(map[string]string, len(cp.Skipped))
		for path, reason := range cp.Skipped {
			info.Skipped[path] = reason
		}
		out = append(out, info)
	}
	return out
}

func decodeMessages(raw []json.RawMessage) ([]types.Message, error) {
	var out []types.Message
	for _, encoded := range raw {
		message, err := types.UnmarshalMessage(encoded)
		if err != nil {
			return nil, err
		}
		out = append(out, message)
	}
	return out, nil
}

func (m *Manager) target(id string) (snapshot, error) {
	if len(m.state.Snapshots) == 0 {
		return snapshot{}, fmt.Errorf("no checkpoint available")
	}
	if id != "" {
		for _, cp := range m.state.Snapshots {
			if cp.ID == id {
				return cp, nil
			}
		}
		return snapshot{}, fmt.Errorf("unknown or expired checkpoint %q", id)
	}
	for i := len(m.state.Snapshots) - 1; i >= 0; i-- {
		if !m.state.Snapshots[i].Automatic {
			return m.state.Snapshots[i], nil
		}
	}
	return m.state.Snapshots[len(m.state.Snapshots)-1], nil
}

func (m *Manager) validate(ctx context.Context, target snapshot, force bool) (map[string]file, error) {
	if target.Gap != "" {
		return nil, fmt.Errorf("cannot rewind across checkpoint capture gap: %s", target.Gap)
	}
	if m.state.Uncertain || m.active > 0 {
		return nil, fmt.Errorf("rewind requires settled mutating calls")
	}
	current, err := m.scan(ctx, m.state.Expected)
	if err != nil {
		return nil, err
	}
	if !force && (target.Outside || !same(current, m.state.Expected)) {
		return nil, fmt.Errorf("workspace changed outside the agent; rewind would lose those changes; only an operator may force rewind")
	}
	return current, nil
}

func protected(path string, skipped map[string]string) bool {
	for skippedPath := range skipped {
		if path == skippedPath || strings.HasPrefix(path, strings.TrimSuffix(skippedPath, "/")+"/") {
			return true
		}
	}
	return false
}

func changedPaths(before, after map[string]file) []string {
	var paths []string
	for path, version := range before {
		if wanted, exists := after[path]; !exists || wanted.Hash != version.Hash || wanted.Perm != version.Perm {
			paths = append(paths, path)
		}
	}
	for path := range after {
		if _, exists := before[path]; !exists {
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)
	return paths
}

func (m *Manager) desiredFiles(target snapshot, current map[string]file) (map[string]file, error) {
	desired := make(map[string]file, len(target.Files))
	for path, version := range target.Files {
		if protected(path, m.skipped) {
			return nil, fmt.Errorf("rewind won't overwrite uncaptured path %s", path)
		}
		desired[path] = version
	}
	for path, version := range current {
		_, captured := m.state.Expected[path]
		if _, exists := target.Files[path]; !exists && (target.ignores(path) || protected(path, target.Skipped) || !captured) {
			desired[path] = version
		}
	}
	return desired, nil
}

func (m *Manager) changes(ctx context.Context, before, after map[string]file) ([]editbatch.Change, []string, error) {
	paths := changedPaths(before, after)
	var changes []editbatch.Change
	for _, path := range paths {
		want, exists := after[path]
		if !exists {
			changes = append(changes, editbatch.Change{Path: path, Delete: true})
			continue
		}
		contents, err := m.git(ctx, nil, "cat-file", "blob", want.Object)
		if err != nil {
			return nil, nil, err
		}
		changes = append(changes, editbatch.Change{Path: path, Data: contents, Perm: want.Perm})
	}
	return changes, paths, nil
}

// Rewind restores files and appends the conversation fork. Force can't cross a capture gap.
func (m *Manager) Rewind(ctx context.Context, id, report string, force bool) (messages []types.Message, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.request = nil
	if m.state.RewindRequest != "" {
		m.state.RewindRequest = ""
		if err := persist(m, "state.json", m.state); err != nil {
			return nil, fmt.Errorf("clearing deferred rewind request: %w", err)
		}
	}
	if m.unavailable != nil {
		return nil, m.unavailable
	}
	err = m.lock(func() error {
		target, err := m.target(id)
		if err != nil {
			return err
		}
		current, err := m.validate(ctx, target, force)
		if err != nil {
			return err
		}
		desired, err := m.desiredFiles(target, current)
		if err != nil {
			return err
		}
		encoded, err := m.git(ctx, nil, "cat-file", "blob", target.Conversation)
		if err != nil {
			return err
		}
		var raw []json.RawMessage
		if err = json.Unmarshal(encoded, &raw); err != nil {
			return err
		}
		messages, err = decodeMessages(raw)
		if err != nil {
			return err
		}
		if strings.TrimSpace(report) != "" {
			messages = append(messages, types.UserMessage{Content: types.StringContent("Rewind findings: " + report), Timestamp: time.Now().UnixMilli()})
		}
		entry, err := journal.NewRewindEntry(target.ID, report, messages)
		if err != nil {
			return err
		}
		changes, paths, err := m.changes(ctx, current, desired)
		if err != nil {
			return err
		}
		rollback, _, err := m.changes(ctx, desired, current)
		if err != nil {
			return err
		}
		return withPaths(m.queue, m.cwd, paths, func() error {
			if m.runtime.Rewound != nil {
				m.runtime.Rewound()
			}
			verified, err := m.scan(ctx, m.state.Expected)
			if err != nil {
				return err
			}
			if !same(current, verified) {
				return fmt.Errorf("workspace changed while preparing rewind; no files restored")
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			root, err := os.OpenRoot(m.cwd)
			if err != nil {
				return err
			}
			defer root.Close()
			journalInfo, err := os.Stat(m.store.Path())
			if err != nil {
				return err
			}
			transaction := pending{Entry: entry, Before: current, After: desired, JournalOffset: journalInfo.Size()}
			if err := persist(m, "pending.json", transaction); err != nil {
				return err
			}
			retire := func() error {
				err := os.Remove(filepath.Join(m.dir, "pending.json"))
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if err := editbatch.New(m.data).Apply(root, "rewind "+target.ID, changes); err != nil {
				// Apply rolls back an ordinary failure. Never leave an already
				// abandoned transaction eligible for a delayed conversation fork.
				return errors.Join(err, retire())
			}
			if appendErr := m.store.Append(context.WithoutCancel(ctx), entry); appendErr != nil {
				committed, checkErr := m.committed(transaction)
				if !committed {
					rollbackErr := editbatch.New(m.data).Apply(root, "rollback refused rewind "+target.ID, rollback)
					if rollbackErr != nil {
						m.gap("rewind journal append and workspace rollback failed: " + rollbackErr.Error())
					}
					return errors.Join(appendErr, checkErr, rollbackErr, retire())
				}
				slog.Warn("rewind append reported an error after writing its fork", "error", appendErr)
			}
			m.state.Expected = desired
			m.state.Uncertain = false
			m.turn = ""
			if err := persist(m, "state.json", m.state); err != nil {
				// The fork is already committed. Keep pending for metadata recovery,
				// but return the committed transcript to the live caller.
				slog.Warn("rewind committed; checkpoint metadata needs recovery", "error", err)
				return nil
			}
			if err := retire(); err != nil {
				slog.Warn("rewind committed; pending metadata remains", "error", err)
			}
			return nil
		})
	})
	return
}

func withPaths(queue *toolio.FileMutationQueue, cwd string, paths []string, fn func() error) error {
	if len(paths) == 0 {
		return fn()
	}
	return queue.Do(filepath.Join(cwd, paths[0]), func() error { return withPaths(queue, cwd, paths[1:], fn) })
}

func (m *Manager) recover(ctx context.Context) error {
	contents, err := os.ReadFile(filepath.Join(m.dir, "pending.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var transaction pending
	if err = json.Unmarshal(contents, &transaction); err != nil {
		return err
	}
	known := map[string]file{}
	for path, version := range transaction.Before {
		known[path] = version
	}
	for path, version := range transaction.After {
		known[path] = version
	}
	current, err := m.scan(ctx, known)
	if err != nil {
		return err
	}
	committed, err := m.committed(transaction)
	if err != nil {
		return err
	}
	if committed || same(current, transaction.After) {
		if !committed {
			if err := m.store.Append(ctx, transaction.Entry); err != nil {
				return err
			}
		}
		loaded, err := journal.Load(ctx, m.store.Path())
		if err != nil {
			return err
		}
		m.state.Expected = transaction.After
		m.state.Uncertain = false
		m.state.RewindRequest = ""
		m.observeOutside(current)
		m.recovered = loaded.Messages
		m.didRecover = true
	} else if !same(current, transaction.Before) {
		return fmt.Errorf("interrupted rewind has conflicting workspace changes; preserved files, abandoned rewind")
	}
	if err := persist(m, "state.json", m.state); err != nil {
		return err
	}
	return os.Remove(filepath.Join(m.dir, "pending.json"))
}

func (m *Manager) committed(transaction pending) (bool, error) {
	journalFile, err := os.Open(m.store.Path())
	if err != nil {
		return false, err
	}
	defer journalFile.Close()
	if _, err := journalFile.Seek(transaction.JournalOffset, 0); err != nil {
		return false, err
	}
	var envelope struct {
		Type    string              `json:"type"`
		Payload journal.RewindEntry `json:"payload"`
	}
	if err := json.NewDecoder(journalFile).Decode(&envelope); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return false, nil
		}
		return false, err
	}
	return envelope.Type == "rewind" && envelope.Payload.Checkpoint == transaction.Entry.Checkpoint &&
		envelope.Payload.Report == transaction.Entry.Report && reflect.DeepEqual(envelope.Payload.Messages, transaction.Entry.Messages), nil
}

func (m *Manager) observeOutside(files map[string]file) {
	if m.state.Expected != nil && !same(files, m.state.Expected) {
		for i := range m.state.Snapshots {
			m.state.Snapshots[i].Outside = true
		}
	}
	m.state.Expected = files
}

// Recovered reports a transcript fork completed while opening the session.
func (m *Manager) Recovered() ([]types.Message, bool) { return m.recovered, m.didRecover }

func (s snapshot) ignores(path string) bool {
	for _, ignored := range s.Ignored {
		if path == ignored || (strings.HasSuffix(ignored, "/") && strings.HasPrefix(path, ignored)) {
			return true
		}
	}
	return false
}

func (m *Manager) settleRequest(ctx context.Context) error {
	if m.state.RewindRequest == "" {
		return nil
	}
	notice := types.UserMessage{Content: types.StringContent("Rewind request " + m.state.RewindRequest + " was interrupted before its safe turn boundary. It was abandoned, not replayed. Inspect the workspace and explicitly request rewind again if needed.")}
	loaded, err := journal.Load(ctx, m.store.Path())
	if err != nil {
		return err
	}
	if len(loaded.Messages) == 0 || !reflect.DeepEqual(loaded.Messages[len(loaded.Messages)-1], notice) {
		if err := m.store.AppendMessage(ctx, notice); err != nil {
			return err
		}
		loaded, err = journal.Load(ctx, m.store.Path())
		if err != nil {
			return err
		}
	}
	m.state.RewindRequest = ""
	if err := persist(m, "state.json", m.state); err != nil {
		return err
	}
	m.recovered, m.didRecover = loaded.Messages, true
	return nil
}
