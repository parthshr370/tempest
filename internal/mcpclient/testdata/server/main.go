// Command server is the hermetic stdio peer used by MCP interop tests.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"go.harness.dev/harness/internal/mcpwire"
	"go.harness.dev/harness/internal/rpc"
)

type stdio struct{}

func (stdio) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (stdio) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (stdio) Close() error                { return os.Stdin.Close() }

type fixture struct {
	mode        string
	ledger      string
	conn        *rpc.Conn
	mu          sync.Mutex
	initialized bool
	active      map[string]context.CancelFunc
	generation  atomic.Uint64
	// resourceGeneration counts reads of fixture://bump, which adds a
	// resource and announces it.
	resourceGeneration atomic.Uint64
	// clientCaps holds the capabilities the client declared at initialize.
	clientCaps map[string]json.RawMessage
}

const pixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScLbtAAAAABJRU5ErkJggg=="

func main() {
	mode := flag.String("mode", "normal", "fixture behavior")
	ledger := flag.String("ledger", "", "dispatch ledger")
	flag.Parse()
	peer := &fixture{mode: *mode, ledger: *ledger, active: map[string]context.CancelFunc{}}
	peer.conn = rpc.NewStreamWithLimit(stdio{}, mcpwire.MaxResponse)
	if peer.mode == "stderr" {
		go func() {
			for range 2048 {
				fmt.Fprintln(os.Stderr, strings.Repeat("diagnostic", 128))
			}
		}()
	}
	_ = peer.conn.ServeOrdered(peer.handle)
}
func (f *fixture) record(method, name string) {
	if f.ledger == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	file, err := os.OpenFile(f.ledger, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		panic(err)
	}
	encoded, err := json.Marshal(struct {
		Method string `json:"method"`
		Name   string `json:"name,omitempty"`
	}{method, name})
	if err != nil {
		panic(err)
	}
	if _, err = file.Write(append(encoded, '\n')); err != nil {
		panic(err)
	}
	if err = file.Close(); err != nil {
		panic(err)
	}
}
func (f *fixture) handle(ctx context.Context, method string, params json.RawMessage) (any, *rpc.Error) {
	f.record(method, "")
	switch method {
	case "initialize":
		var hello struct {
			Capabilities map[string]json.RawMessage `json:"capabilities"`
		}
		_ = json.Unmarshal(params, &hello)
		f.mu.Lock()
		f.clientCaps = hello.Capabilities
		f.mu.Unlock()
		version := mcpwire.PreferredVersion
		if f.mode == "june" {
			version = "2025-06-18"
		}
		if f.mode == "modern" {
			version = "2026-07-28"
		}
		return map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]bool{"listChanged": true}, "resources": map[string]bool{"listChanged": true}}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}}, nil
	case "notifications/initialized":
		f.mu.Lock()
		f.initialized = true
		f.mu.Unlock()
		return nil, nil
	case "notifications/cancelled":
		var cancellation struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		_ = json.Unmarshal(params, &cancellation)
		f.mu.Lock()
		cancel := f.active[string(cancellation.RequestID)]
		f.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return nil, nil
	case "ping":
		return struct{}{}, nil
	case "tools/list":
		f.mu.Lock()
		initialized := f.initialized
		f.mu.Unlock()
		if !initialized {
			return nil, rpc.NewError(-32000, "order", "initialized must precede tools/list")
		}
		return rpc.Deferred(func() (any, *rpc.Error) {
			var root json.RawMessage
			if err := f.conn.Call(ctx, "roots/list", struct{}{}, &root); err != nil {
				return nil, rpc.NewError(-32000, "roots", err.Error())
			}
			f.record("roots", string(root))
			var page struct {
				Cursor string `json:"cursor"`
			}
			_ = json.Unmarshal(params, &page)
			names := []string{"echo", "count", "block", "error", "mixed", "large", "crash", "change", "elicit"}
			if f.generation.Load() > 0 {
				names = append(names, "added")
			}
			if page.Cursor == "" {
				names = names[:2]
			} else {
				names = names[2:]
			}
			if f.mode == "invalid-name" {
				names = []string{"not.a.tool"}
			}
			definitions := make([]map[string]any, 0, len(names))
			for _, name := range names {
				schema := json.RawMessage(`{"type":"object","$defs":{"value":{"oneOf":[{"type":"string"},{"type":"integer"}]}},"properties":{"value":{"$ref":"#/$defs/value","default":"ok"}},"additionalProperties":false}`)
				if f.mode == "external-ref" {
					schema = json.RawMessage(`{"type":"object","properties":{"value":{"$ref":"https://never.example/schema"}}}`)
				}
				if f.generation.Load() > 0 {
					schema = json.RawMessage(`{"type":"object","properties":{"changed":{"type":"boolean"}}}`)
				}
				definitions = append(definitions, map[string]any{"name": name, "description": "fixture " + name, "inputSchema": schema, "annotations": map[string]bool{"readOnlyHint": true}})
			}
			if f.mode == "duplicate" {
				definitions = append(definitions, definitions[0])
			}
			result := map[string]any{"tools": definitions}
			if page.Cursor == "" || f.mode == "repeated-cursor" {
				result["nextCursor"] = "page2"
			}
			return result, nil
		}), nil
	case "tools/call":
		var call struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Meta      struct {
				ProgressToken json.RawMessage `json:"progressToken"`
			} `json:"_meta"`
		}
		if json.Unmarshal(params, &call) != nil {
			return nil, rpc.NewError(-32602, "params", "invalid call")
		}
		callCtx, cancel := context.WithCancel(ctx)
		id := string(rpc.RequestID(ctx))
		f.mu.Lock()
		f.active[id] = cancel
		f.mu.Unlock()
		return rpc.Deferred(func() (any, *rpc.Error) {
			defer cancel()
			defer func() { f.mu.Lock(); delete(f.active, id); f.mu.Unlock() }()
			f.record("dispatch", call.Name)
			if len(call.Meta.ProgressToken) > 0 {
				_ = f.conn.Notify(callCtx, "notifications/progress", map[string]any{"progressToken": call.Meta.ProgressToken, "progress": 1, "message": "fixture working"})
			}
			text := string(call.Arguments)
			isError := false
			switch call.Name {
			case "block":
				<-callCtx.Done()
				return nil, rpc.NewError(-32000, "cancelled", "fixture cancelled")
			case "crash":
				os.Exit(9)
			case "error":
				text = "remote error content"
				isError = true
			case "large":
				text = strings.Repeat("世界\"\\\n", 40000)
			case "change":
				f.generation.Add(1)
				for range 5 {
					_ = f.conn.Notify(callCtx, "notifications/tools/list_changed", struct{}{})
				}
				text = "changed"
			case "elicit":
				text = f.elicit(callCtx, call.Arguments)
			case "mixed":
				return map[string]any{"content": []map[string]any{{"type": "text", "text": "first"}, {"type": "image", "data": pixelPNG, "mimeType": "image/png"}, {"type": "resource", "resource": map[string]string{"uri": "file:///do-not-open", "mimeType": "text/plain", "text": "embedded"}}, {"type": "resource_link", "uri": "https://do-not-fetch.example", "name": "link", "description": "reference"}, {"type": "audio", "data": base64.StdEncoding.EncodeToString([]byte("audio")), "mimeType": "audio/wav"}, {"type": "unknown", "value": "kept"}}, "structuredContent": map[string]string{"value": "structured"}, "isError": true}, nil
			}
			return map[string]any{"content": []map[string]string{{"type": "text", "text": text}}, "isError": isError}, nil
		}), nil
	case "resources/list":
		var page struct {
			Cursor string `json:"cursor"`
		}
		_ = json.Unmarshal(params, &page)
		if page.Cursor == "" {
			return map[string]any{"resources": []map[string]string{{"uri": "file:///notes.md", "name": "notes", "title": "Project notes", "mimeType": "text/markdown", "description": "fixture notes"}}, "nextCursor": "more"}, nil
		}
		resources := []map[string]string{{"uri": "fixture://pixel.png", "name": "pixel", "mimeType": "image/png"}, {"uri": "fixture://data.bin", "name": "data", "mimeType": "application/octet-stream"}}
		if f.resourceGeneration.Load() > 0 {
			resources = append(resources, map[string]string{"uri": "fixture://added.txt", "name": "added", "mimeType": "text/plain"})
		}
		return map[string]any{"resources": resources}, nil
	case "resources/templates/list":
		return map[string]any{"resourceTemplates": []map[string]string{{"uriTemplate": "fixture://logs/{day}", "name": "logs", "mimeType": "text/plain", "description": "daily log"}}}, nil
	case "resources/read":
		var read struct {
			URI string `json:"uri"`
		}
		_ = json.Unmarshal(params, &read)
		return rpc.Deferred(func() (any, *rpc.Error) {
			content := map[string]string{"uri": read.URI}
			switch {
			case read.URI == "file:///notes.md":
				content["mimeType"], content["text"] = "text/markdown", "line one\nline two\nline three\n"
			case read.URI == "fixture://pixel.png":
				content["mimeType"], content["blob"] = "image/png", pixelPNG
			case read.URI == "fixture://data.bin":
				content["mimeType"], content["blob"] = "application/octet-stream", base64.StdEncoding.EncodeToString([]byte{0, 1, 2, 3})
			case strings.HasPrefix(read.URI, "fixture://logs/"):
				content["mimeType"], content["text"] = "text/plain", "log for "+strings.TrimPrefix(read.URI, "fixture://logs/")
			case read.URI == "fixture://bump":
				f.resourceGeneration.Add(1)
				_ = f.conn.Notify(ctx, "notifications/resources/list_changed", struct{}{})
				content["mimeType"], content["text"] = "text/plain", "bumped"
			default:
				return nil, rpc.NewError(-32002, "not_found", "resource not found")
			}
			return map[string]any{"contents": []map[string]string{content}}, nil
		}), nil
	default:
		return nil, rpc.NewError(-32601, "method_not_found", "fixture doesn't implement method")
	}
}

var _ io.ReadWriteCloser = stdio{}

// elicit asks the client for input in the mode named by the call's value
// and returns the client's raw reply, or why there is none.
func (f *fixture) elicit(ctx context.Context, arguments json.RawMessage) string {
	var args struct {
		Value string `json:"value"`
	}
	_ = json.Unmarshal(arguments, &args)
	f.mu.Lock()
	declared := string(f.clientCaps["elicitation"])
	f.mu.Unlock()
	if declared == "" {
		return "elicitation not declared"
	}
	request := map[string]any{"mode": "form", "message": "Pick a color", "requestedSchema": json.RawMessage(`{"type":"object","properties":{"color":{"type":"string","enum":["red","blue"]}},"required":["color"]}`)}
	switch args.Value {
	case "url":
		request = map[string]any{"mode": "url", "message": "Connect the fixture account", "url": "https://fixture.example/connect", "elicitationId": "fixture-1"}
	case "nested":
		request["requestedSchema"] = json.RawMessage(`{"type":"object","properties":{"address":{"type":"object","properties":{"city":{"type":"string"}}}}}`)
	}
	var reply json.RawMessage
	if err := f.conn.Call(ctx, "elicitation/create", request, &reply); err != nil {
		return "elicitation error: " + err.Error()
	}
	return string(reply)
}
