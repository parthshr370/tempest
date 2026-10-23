package lsp

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestLSPCancelCrashCircuit(t *testing.T) {
	t.Run("cancel sends the exact ID, tolerates the late result and never resends", func(t *testing.T) {
		h := startHarness(t, setup{fixture: map[string]any{"hang": []string{"textDocument/definition"}}})
		h.ws.write(t, "a.go", goSource)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := h.queryCtx(ctx, definitionAt("a.go", 5, 9)); done <- err }()
		eventually(t, "definition", func() bool { return h.count("go", "textDocument/definition") == 1 })
		cancel()
		wantCode(t, <-done, CodeCancelled)
		eventually(t, "late response delivered", func() bool { return h.count("go", "$/cancelRequest") == 1 })
		var requestID, cancelledID any
		for _, event := range h.events("go") {
			if event["method"] == "textDocument/definition" {
				requestID = event["id"]
			}
			if event["method"] == "$/cancelRequest" {
				cancelledID = event["params"].(map[string]any)["id"]
			}
		}
		if requestID == nil || requestID != cancelledID {
			t.Fatalf("cancel id %v, request id %v", cancelledID, requestID)
		}
		time.Sleep(60 * time.Millisecond) // the fixture answers 20ms after the cancel
		h.mustQuery(Query{Method: MethodReferences, Path: "a.go", Position: Position{5, 9}})
		if h.count("go", "textDocument/definition") != 1 || h.count("go", "start") != 1 {
			t.Fatal("late result broke the connection or the request was resent")
		}
	})
	t.Run("deadline cancels with the same ID", func(t *testing.T) {
		h := startHarness(t, setup{fixture: map[string]any{"hang": []string{"textDocument/references"}}, timing: func(tm *timing) { tm.request = 300 * time.Millisecond }})
		h.ws.write(t, "a.go", goSource)
		_, err := h.query(Query{Method: MethodReferences, Path: "a.go", Position: Position{5, 9}})
		wantCode(t, err, CodeTimeout)
		eventually(t, "cancel", func() bool { return h.count("go", "$/cancelRequest") == 1 })
	})
	t.Run("crash settles the caller, cooldown, then one explicit restart without replay", func(t *testing.T) {
		clock := &fakeClock{now: time.Unix(1_000_000, 0)}
		h := startHarness(t, setup{
			fixture: map[string]any{"exit_on": "textDocument/definition", "stderr_text": "panic: boom token=abc123secretvalue"},
			timing:  func(tm *timing) { tm.now = clock.Now; tm.cooldown = 2 * time.Second },
		})
		h.ws.write(t, "a.go", goSource)
		_, err := h.query(definitionAt("a.go", 5, 9))
		crash := wantCode(t, err, CodeServerUnavailable)
		if strings.Contains(crash.Detail, "abc123secretvalue") || !strings.Contains(crash.Detail, "boom") {
			t.Fatalf("stderr tail must be present and redacted: %q", crash.Detail)
		}
		eventually(t, "crash noticed", func() bool {
			h.m.mu.Lock()
			defer h.m.mu.Unlock()
			return h.m.slots["go"].state == stateStopped
		})
		_, err = h.query(Query{Method: MethodReferences, Path: "a.go", Position: Position{5, 9}})
		if cooldown := wantCode(t, err, CodeServerUnavailable); cooldown.RetryAfter <= 0 || cooldown.RetryAfter > 2*time.Second {
			t.Fatalf("retry after %v", cooldown.RetryAfter)
		}
		if h.count("go", "start") != 1 {
			t.Fatal("restarted during cooldown")
		}
		clock.advance(2 * time.Second)
		h.mustQuery(Query{Method: MethodReferences, Path: "a.go", Position: Position{5, 9}})
		if h.count("go", "start") != 2 || h.count("go", "textDocument/definition") != 1 {
			t.Fatal("restart must be explicit and must not replay the crashed request")
		}
	})
	t.Run("three failures within a minute open the circuit", func(t *testing.T) {
		clock := &fakeClock{now: time.Unix(1_000_000, 0)}
		h := startHarness(t, setup{
			fixture: map[string]any{"exit_on": "initialize"},
			timing:  func(tm *timing) { tm.now = clock.Now; tm.cooldown = 2 * time.Second },
		})
		h.ws.write(t, "a.go", goSource)
		for i := range 3 {
			_, err := h.query(definitionAt("a.go", 5, 9))
			wantCode(t, err, CodeServerUnavailable)
			if i < 2 {
				clock.advance(3 * time.Second)
			}
		}
		_, err := h.query(definitionAt("a.go", 5, 9))
		open := wantCode(t, err, CodeServerUnavailable)
		if open.RetryAfter < 59*time.Second || h.count("go", "start") != 3 {
			t.Fatalf("circuit: retry after %v, starts %d", open.RetryAfter, h.count("go", "start"))
		}
		clock.advance(61 * time.Second)
		_, err = h.query(definitionAt("a.go", 5, 9))
		wantCode(t, err, CodeServerUnavailable)
		if h.count("go", "start") != 4 {
			t.Fatal("circuit must close after its open period")
		}
	})
	t.Run("malformed and oversized frames retire the server with stable codes", func(t *testing.T) {
		for name, test := range map[string]struct{ key, code string }{
			"malformed": {"malformed_on", CodeProtocol},
			"oversize":  {"oversize_on", CodeOutputLimit},
		} {
			t.Run(name, func(t *testing.T) {
				h := startHarness(t, setup{fixture: map[string]any{test.key: "textDocument/definition"}})
				h.ws.write(t, "a.go", goSource)
				_, err := h.query(definitionAt("a.go", 5, 9))
				wantCode(t, err, test.code)
				pid := h.pids("go")[0]
				eventually(t, "retired", func() bool { return processGone(pid) })
				eventually(t, "cooldown", func() bool {
					_, err := h.query(Query{Method: MethodReferences, Path: "a.go", Position: Position{5, 9}})
					return err == nil
				})
				if h.count("go", "start") != 2 {
					t.Fatal("expected exactly one restart")
				}
			})
		}
	})
	t.Run("stderr flood, blocked writer and descendant pipes can't deadlock Close", func(t *testing.T) {
		for name, fixture := range map[string]map[string]any{
			"stderr flood":                    {"stderr_flood_bytes": 8 << 20},
			"group descendant":                {"spawn_child": "group"},
			"setsid descendant holding pipes": {"spawn_child": "setsid"},
			"blocked writer":                  {"stop_reading_on": "textDocument/definition", "shutdown_hang": true},
		} {
			t.Run(name, func(t *testing.T) {
				h := startHarness(t, setup{fixture: fixture, timing: func(tm *timing) { tm.request = 500 * time.Millisecond; tm.sendBudget = 200 * time.Millisecond }})
				h.ws.write(t, "a.go", goSource)
				_, err := h.query(definitionAt("a.go", 5, 9))
				if name == "blocked writer" {
					wantCode(t, err, CodeTimeout)
					// A large change fills the pipe; the writer blocks.
					h.ws.write(t, "a.go", goSource+"// "+strings.Repeat("x", 900<<10)+"\n")
					_, err = h.query(definitionAt("a.go", 5, 9))
					if err == nil {
						t.Fatal("query against a server that stopped reading succeeded")
					}
				} else if err != nil {
					t.Fatal(err)
				}
				pid := h.pids("go")[0]
				start := time.Now()
				closeWithin(t, h.m, 8*time.Second)
				if elapsed := time.Since(start); elapsed > 6*time.Second {
					t.Fatalf("close took %s", elapsed)
				}
				if !processGone(pid) {
					t.Fatal("server survived Close")
				}
				if name == "group descendant" {
					for _, event := range h.events("go") {
						if event["event"] == "child" {
							child := int(event["pid"].(float64))
							eventually(t, "group descendant killed", func() bool { return processGone(child) })
						}
					}
				}
			})
		}
	})
}
