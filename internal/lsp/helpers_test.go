package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.harness.dev/harness/internal/editdiff"
)

var fixtureBinary string

func TestMain(suite *testing.M) {
	directory, err := os.MkdirTemp("", "tempest-lsp-fixture-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fixtureBinary = filepath.Join(directory, "server")
	// The parent test command already holds the machine-wide heavy lock.
	command := exec.Command("go", "build", "-o", fixtureBinary, "./testdata/server")
	command.Stdout, command.Stderr = os.Stderr, os.Stderr
	if err = command.Run(); err != nil {
		_ = os.RemoveAll(directory)
		os.Exit(1)
	}
	code := suite.Run()
	_ = os.RemoveAll(directory)
	os.Exit(code)
}

// dirWorkspace is a contained test Workspace over os.Root that records reads.
type dirWorkspace struct {
	root string
	r    *os.Root

	mu          sync.Mutex
	reads       []string
	snapshots   int
	closed      bool
	snapshotErr error
	exts        map[string]bool
}

func newDirWorkspace(t *testing.T) *dirWorkspace {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return &dirWorkspace{root: root, r: r, exts: map[string]bool{".go": true, ".yaml": true, ".yml": true, ".tf": true, ".tfvars": true, ".tpl": true}}
}

func (w *dirWorkspace) Root() string { return w.root }

func (w *dirWorkspace) ReadFile(ctx context.Context, p string) (Document, error) {
	if err := ctx.Err(); err != nil {
		return Document{}, err
	}
	rel := p
	if filepath.IsAbs(p) {
		var err error
		if rel, err = filepath.Rel(w.root, p); err != nil {
			return Document{}, err
		}
	}
	rel = filepath.ToSlash(filepath.Clean(rel))
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return Document{}, errors.New("path escapes the workspace")
	}
	w.mu.Lock()
	w.reads = append(w.reads, rel)
	w.mu.Unlock()
	data, err := w.r.ReadFile(rel)
	if err != nil {
		return Document{}, err
	}
	return Document{Path: rel, Text: data, Anchor: editdiff.ContentAnchor(data)}, nil
}

func (w *dirWorkspace) Snapshot(ctx context.Context) ([]Document, error) {
	w.mu.Lock()
	w.snapshots++
	snapshotErr := w.snapshotErr
	w.mu.Unlock()
	if snapshotErr != nil {
		return nil, snapshotErr
	}
	var docs []Document
	err := filepath.WalkDir(w.root, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}
		if !entry.Type().IsRegular() || !w.exts[filepath.Ext(p)] {
			return nil
		}
		rel, _ := filepath.Rel(w.root, p)
		doc, err := w.ReadFile(ctx, rel)
		if err != nil {
			return err
		}
		docs = append(docs, doc)
		return nil
	})
	return docs, err
}

func (w *dirWorkspace) Close() error {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
	return nil
}

func (w *dirWorkspace) readCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.reads)
}

func (w *dirWorkspace) write(t *testing.T, rel, content string) {
	t.Helper()
	full := filepath.Join(w.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (w *dirWorkspace) hashes(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.WalkDir(w.root, func(p string, entry fs.DirEntry, err error) error {
		if err == nil && entry.Type().IsRegular() {
			data, _ := os.ReadFile(p)
			out[p] = editdiff.ContentAnchor(data)
		}
		return nil
	})
	return out
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func testTiming() timing {
	t := defaultTiming()
	t.idle = time.Hour
	t.cooldown = 20 * time.Millisecond
	t.startup = 10 * time.Second
	t.request = 10 * time.Second
	t.sendBudget = 2 * time.Second
	return t
}

// setup describes one fixture-backed manager.
type setup struct {
	fixture  map[string]any
	routes   []string
	timing   func(*timing)
	settings string
	options  string
	ws       *dirWorkspace
}

type harness struct {
	t       *testing.T
	ws      *dirWorkspace
	m       *Manager
	ledgers map[string]string
}

func startHarness(t *testing.T, s setup) *harness {
	t.Helper()
	ws := s.ws
	if ws == nil {
		ws = newDirWorkspace(t)
	}
	private := t.TempDir()
	configPath := filepath.Join(private, "fixture.json")
	raw, _ := json.Marshal(s.fixture)
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if len(s.routes) == 0 {
		s.routes = []string{"go"}
	}
	h := &harness{t: t, ws: ws, ledgers: map[string]string{}}
	var servers []ServerConfig
	for _, name := range s.routes {
		h.ledgers[name] = filepath.Join(private, "ledger-"+name)
		server := ServerConfig{Name: name, Command: fixtureBinary, Args: []string{"-ledger", h.ledgers[name], "-config", configPath}}
		if s.settings != "" {
			server.Settings = json.RawMessage(s.settings)
		}
		if s.options != "" {
			server.InitializationOptions = json.RawMessage(s.options)
		}
		servers = append(servers, server)
	}
	tm := testTiming()
	if s.timing != nil {
		s.timing(&tm)
	}
	m, err := newManager(ws, Config{Enabled: true, Servers: servers}, nil, tm)
	if err != nil {
		t.Fatal(err)
	}
	h.m = m
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := m.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
		for _, name := range s.routes {
			for _, event := range h.events(name) {
				if event["event"] == "child" {
					_ = syscall.Kill(int(event["pid"].(float64)), syscall.SIGKILL)
				}
			}
		}
	})
	return h
}

func (h *harness) query(q Query) (map[string]any, error) {
	h.t.Helper()
	return h.queryCtx(context.Background(), q)
}

func (h *harness) queryCtx(ctx context.Context, q Query) (map[string]any, error) {
	h.t.Helper()
	result, err := h.m.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if err = json.Unmarshal(result.Value, &value); err != nil {
		h.t.Fatalf("result isn't a JSON object: %s", result.Value)
	}
	if cost := encodedCost(result.Value); cost > maxRenamePlanBytes {
		h.t.Fatalf("result cost %d exceeds every budget", cost)
	}
	return value, nil
}

func (h *harness) mustQuery(q Query) map[string]any {
	h.t.Helper()
	value, err := h.query(q)
	if err != nil {
		h.t.Fatalf("query %+v: %v", q, err)
	}
	return value
}

func (h *harness) events(route string) []map[string]any {
	file, err := os.Open(h.ledgers[route])
	if err != nil {
		return nil
	}
	defer file.Close()
	var events []map[string]any
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	for scanner.Scan() {
		var event map[string]any
		if json.Unmarshal(scanner.Bytes(), &event) == nil {
			events = append(events, event)
		}
	}
	return events
}

// received lists inbound methods in ledger order.
func (h *harness) received(route string) []string {
	var methods []string
	for _, event := range h.events(route) {
		if event["event"] == "recv" {
			methods = append(methods, event["method"].(string))
		}
	}
	return methods
}

func (h *harness) count(route, kind string) int {
	n := 0
	for _, event := range h.events(route) {
		if event["event"] == kind || (event["event"] == "recv" && event["method"] == kind) {
			n++
		}
	}
	return n
}

// eventually polls the ledger-backed predicate; fixtures report asynchronously.
func eventually(t *testing.T, what string, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !predicate() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func errorCode(err error) string {
	var lspErr *Error
	if errors.As(err, &lspErr) {
		return lspErr.Code
	}
	return ""
}

func wantCode(t *testing.T, err error, code string) *Error {
	t.Helper()
	var lspErr *Error
	if !errors.As(err, &lspErr) || lspErr.Code != code {
		t.Fatalf("error %v, want code %s", err, code)
	}
	return lspErr
}

func processGone(pid int) bool {
	err := syscall.Kill(pid, 0)
	return errors.Is(err, syscall.ESRCH)
}

func (h *harness) pids(route string) []int {
	var pids []int
	for _, event := range h.events(route) {
		if event["event"] == "start" {
			pids = append(pids, int(event["pid"].(float64)))
		}
	}
	return pids
}

// params returns the params of each recorded method call in order.
func (h *harness) params(route, method string) []map[string]any {
	var out []map[string]any
	for _, event := range h.events(route) {
		if event["event"] == "recv" && event["method"] == method {
			params, _ := event["params"].(map[string]any)
			out = append(out, params)
		}
	}
	return out
}

func definitionAt(path string, line, column int) Query {
	return Query{Method: MethodDefinition, Path: path, Position: Position{Line: line, Column: column}}
}
