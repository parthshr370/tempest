package lsp

import (
	"context"
	"fmt"
	"syscall"
	"testing"
	"time"
)

func diagnosticsQuery(path string, wait time.Duration) Query {
	return Query{Method: MethodDiagnostics, Path: path, Wait: wait}
}

func diagnosticSummary(value map[string]any) string {
	var items []string
	for _, item := range value["diagnostics"].([]any) {
		d := item.(map[string]any)
		items = append(items, fmt.Sprintf("%v@%v:%v-%v:%v %v %v", d["severity"], d["line"], d["column"], d["end_line"], d["end_column"], d["code"], d["message"]))
	}
	return fmt.Sprintf("fresh=%v freshness=%v total=%v omitted=%v %v", value["fresh"], value["freshness"], value["total"], value["omitted"], items)
}

func TestLSPDiagnosticsFreshness(t *testing.T) {
	const wait = 2 * time.Second
	t.Run("versioned push is current only for the captured version", func(t *testing.T) {
		h := startHarness(t, setup{})
		h.ws.write(t, "a.go", "package p\nvar x = ERR\n")
		value := h.mustQuery(diagnosticsQuery("a.go", wait))
		if got := diagnosticSummary(value); got != "fresh=true freshness=current total=1 omitted=0 [error@2:9-2:12 F1 ERR found]" {
			t.Fatal(got)
		}
		if value["version"].(float64) != 1 || value["anchor"] != anchorOf("package p\nvar x = ERR\n") {
			t.Fatalf("version/anchor %v", value)
		}
		h.ws.write(t, "a.go", "package p\nvar x = ERR // WARN\n")
		h.m.Changed("a.go")
		value = h.mustQuery(diagnosticsQuery("a.go", wait))
		if got := diagnosticSummary(value); got != "fresh=true freshness=current total=2 omitted=0 [error@2:9-2:12 F1 ERR found warning@2:16-2:20 F2 WARN found]" {
			t.Fatal(got)
		}
		filtered := h.mustQuery(Query{Method: MethodDiagnostics, Path: "a.go", Wait: wait, Severity: "warning"})
		if got := diagnosticSummary(filtered); got != "fresh=true freshness=current total=1 omitted=0 [warning@2:16-2:20 F2 WARN found]" {
			t.Fatal(got)
		}
		limited := h.mustQuery(Query{Method: MethodDiagnostics, Path: "a.go", Wait: wait, MaxResults: 1})
		if limited["omitted"].(float64) != 1 || limited["truncated"] != true {
			t.Fatal(diagnosticSummary(limited))
		}
	})
	t.Run("empty publication is current and clean", func(t *testing.T) {
		h := startHarness(t, setup{})
		h.ws.write(t, "a.go", "package p\n")
		if got := diagnosticSummary(h.mustQuery(diagnosticsQuery("a.go", wait))); got != "fresh=true freshness=current total=0 omitted=0 []" {
			t.Fatal(got)
		}
	})
	t.Run("unversioned publications are never fresh", func(t *testing.T) {
		h := startHarness(t, setup{fixture: map[string]any{"push": "unversioned"}})
		h.ws.write(t, "a.go", "ERR\n")
		if got := diagnosticSummary(h.mustQuery(diagnosticsQuery("a.go", wait))); got != "fresh=false freshness=unversioned total=1 omitted=0 [error@1:1-1:4 F1 ERR found]" {
			t.Fatal(got)
		}
	})
	t.Run("no publication means pending, not zero errors", func(t *testing.T) {
		for _, mode := range []string{"none", "older"} {
			h := startHarness(t, setup{fixture: map[string]any{"push": mode}})
			h.ws.write(t, "a.go", "ERR\n")
			if got := diagnosticSummary(h.mustQuery(diagnosticsQuery("a.go", 50*time.Millisecond))); got != "fresh=false freshness=pending total=0 omitted=0 []" {
				t.Fatalf("%s: %s", mode, got)
			}
		}
	})
	t.Run("wait zero synchronizes but doesn't wait", func(t *testing.T) {
		h := startHarness(t, setup{fixture: map[string]any{"push": "manual"}})
		h.ws.write(t, "a.go", "ERR\n")
		value := h.mustQuery(diagnosticsQuery("a.go", 0))
		if got := diagnosticSummary(value); got != "fresh=false freshness=pending total=0 omitted=0 []" {
			t.Fatal(got)
		}
		if value["version"].(float64) != 1 || value["anchor"] != anchorOf("ERR\n") {
			t.Fatalf("wait 0 didn't capture the opened document: %v", value)
		}
		h.m.mu.Lock()
		srv := h.m.slots["go"].srv
		h.m.mu.Unlock()
		publish := func() {
			t.Helper()
			// The reply follows the publication in read order. Unlike a ledger
			// read after Notify, this proves the server consumed earlier changes.
			ctx, cancel := context.WithTimeout(context.Background(), wait)
			defer cancel()
			if _, err := srv.call(ctx, "fixture/publishDiagnostics", map[string]any{"textDocument": textDocumentID{URI: srv.uri("a.go")}}); err != nil {
				t.Fatal(err)
			}
		}
		publish()
		if h.count("go", "textDocument/didOpen") != 1 {
			t.Fatal("wait 0 must still synchronize")
		}
		value = h.mustQuery(diagnosticsQuery("a.go", wait))
		if value["freshness"] != "current" {
			t.Fatal(diagnosticSummary(value))
		}
		// After an edit, the cached publication for version 1 is outdated, not current.
		h.ws.write(t, "a.go", "ERR WARN\n")
		h.m.Changed("a.go")
		value = h.mustQuery(diagnosticsQuery("a.go", 0))
		if got := diagnosticSummary(value); got != "fresh=false freshness=outdated total=1 omitted=0 [error@1:1-1:4 F1 ERR found]" {
			t.Fatal(got)
		}
		if value["version"].(float64) != 2 || value["anchor"] != anchorOf("ERR WARN\n") {
			t.Fatalf("wait 0 didn't synchronize didChange: %v", value)
		}
		publish()
		if h.count("go", "textDocument/didChange") != 1 {
			t.Fatal("wait 0 must synchronize pending didChange")
		}
		if got := diagnosticSummary(h.mustQuery(diagnosticsQuery("a.go", 0))); got != "fresh=true freshness=current total=2 omitted=0 [error@1:1-1:4 F1 ERR found warning@1:5-1:9 F2 WARN found]" {
			t.Fatal(got)
		}
	})
	t.Run("future version is a protocol error that retires the server", func(t *testing.T) {
		h := startHarness(t, setup{fixture: map[string]any{"push": "future"}})
		h.ws.write(t, "a.go", "ERR\n")
		_, err := h.query(diagnosticsQuery("a.go", wait))
		wantCode(t, err, CodeProtocol)
		pid := h.pids("go")[0]
		eventually(t, "retired process", func() bool { return processGone(pid) })
	})
	t.Run("an edit racing the response is never labeled current", func(t *testing.T) {
		h := startHarness(t, setup{fixture: map[string]any{"push_delay_ms": 200}})
		h.ws.write(t, "a.go", "ERR\n")
		go func() {
			for h.count("go", "textDocument/didOpen") == 0 {
				time.Sleep(5 * time.Millisecond)
			}
			h.ws.write(t, "a.go", "clean\n")
			h.m.Changed("a.go")
		}()
		value := h.mustQuery(diagnosticsQuery("a.go", wait))
		if value["fresh"] != false || value["freshness"] == "current" {
			t.Fatal(diagnosticSummary(value))
		}
	})
	t.Run("unannounced change of a file the server isn't told about makes earlier publications uncertain", func(t *testing.T) {
		h := startHarness(t, setup{fixture: map[string]any{"push_delay_ms": 0}})
		h.ws.write(t, "a.go", "ERR\n")
		h.mustQuery(diagnosticsQuery("a.go", wait))
		h.ws.write(t, "go.mod", "module m\n")
		h.m.Changed("go.mod")
		if got := diagnosticSummary(h.mustQuery(diagnosticsQuery("a.go", 0))); got != "fresh=false freshness=outdated total=1 omitted=0 [error@1:1-1:4 F1 ERR found]" {
			t.Fatal(got)
		}
	})
	t.Run("crash clears the cache", func(t *testing.T) {
		h := startHarness(t, setup{fixture: map[string]any{"push_delay_ms": 150}})
		h.ws.write(t, "a.go", "ERR\n")
		if h.mustQuery(diagnosticsQuery("a.go", wait))["freshness"] != "current" {
			t.Fatal("setup")
		}
		pid := h.pids("go")[0]
		_ = syscall.Kill(pid, syscall.SIGKILL)
		eventually(t, "crash noticed", func() bool {
			h.m.mu.Lock()
			defer h.m.mu.Unlock()
			return h.m.slots["go"].state == stateStopped
		})
		time.Sleep(30 * time.Millisecond) // cooldown is 20ms in tests
		if got := diagnosticSummary(h.mustQuery(diagnosticsQuery("a.go", 0))); got != "fresh=false freshness=pending total=0 omitted=0 []" {
			t.Fatal(got)
		}
	})
	t.Run("pull full, unchanged and refresh", func(t *testing.T) {
		h := startHarness(t, setup{fixture: map[string]any{"pull_diagnostics": true, "diagnostic_identifier": "fixture-id"}})
		h.ws.write(t, "a.go", "ERR\n")
		first := h.mustQuery(diagnosticsQuery("a.go", wait))
		second := h.mustQuery(diagnosticsQuery("a.go", wait))
		want := "fresh=true freshness=current total=1 omitted=0 [error@1:1-1:4 F1 ERR found]"
		if diagnosticSummary(first) != want || diagnosticSummary(second) != want {
			t.Fatalf("%s / %s", diagnosticSummary(first), diagnosticSummary(second))
		}
		pulls := h.params("go", "textDocument/diagnostic")
		if len(pulls) != 2 || pulls[0]["identifier"] != "fixture-id" || pulls[0]["previousResultId"] != nil || pulls[1]["previousResultId"] == nil {
			t.Fatalf("pull params %v", pulls)
		}
		h.m.slots["go"].srv.onRefresh()
		h.mustQuery(diagnosticsQuery("a.go", wait))
		if pulls = h.params("go", "textDocument/diagnostic"); pulls[2]["previousResultId"] != nil {
			t.Fatal("refresh must invalidate the retained resultId")
		}
		cached := h.mustQuery(diagnosticsQuery("a.go", 0))
		if got := diagnosticSummary(cached); got != "fresh=false freshness=outdated total=1 omitted=0 [error@1:1-1:4 F1 ERR found]" || len(h.params("go", "textDocument/diagnostic")) != 3 {
			t.Fatalf("wait 0 must return the cache without pulling: %s", got)
		}
	})
	t.Run("pull timeout returns pending, never clean", func(t *testing.T) {
		h := startHarness(t, setup{fixture: map[string]any{"pull_diagnostics": true, "hang": []string{"textDocument/diagnostic"}}})
		h.ws.write(t, "a.go", "ERR\n")
		if got := diagnosticSummary(h.mustQuery(diagnosticsQuery("a.go", 100*time.Millisecond))); got != "fresh=false freshness=pending total=0 omitted=0 []" {
			t.Fatal(got)
		}
		eventually(t, "cancel", func() bool { return h.count("go", "$/cancelRequest") == 1 })
	})
	t.Run("unchanged without a retained resultId is an error", func(t *testing.T) {
		h := startHarness(t, setup{fixture: map[string]any{"pull_diagnostics": true, "responses": map[string]any{"textDocument/diagnostic": map[string]any{"kind": "unchanged", "resultId": "x"}}}})
		h.ws.write(t, "a.go", "ERR\n")
		_, err := h.query(diagnosticsQuery("a.go", wait))
		wantCode(t, err, CodeProtocol)
	})
	t.Run("Changed never blocks on a slow or missing server", func(t *testing.T) {
		h := startHarness(t, setup{fixture: map[string]any{"hang": []string{"textDocument/definition"}}})
		h.ws.write(t, "a.go", goSource)
		start := time.Now()
		h.m.Changed("a.go") // no server yet: nothing to start
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() { _, _ = h.queryCtx(ctx, definitionAt("a.go", 5, 9)) }()
		eventually(t, "hung query holds the lane", func() bool { return h.count("go", "textDocument/definition") == 1 })
		for range 500 {
			h.m.Changed("a.go")
		}
		if elapsed := time.Since(start); elapsed > time.Second || h.count("go", "start") != 1 {
			t.Fatalf("Changed blocked for %s or started a server", elapsed)
		}
	})
}
