package session

import (
	"context"
	"log/slog"
	"slices"
	"sync"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/discovery"
	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/work"
)

// nestedContextHook returns the AfterToolCall that appends nested context
// files after a successful read or listing. It is nil when
// context_files.nested is off or the workspace is not on this host, where
// the files would be read from the wrong filesystem.
//
// A file counts as shown only while the conversation holds the tool result
// that delivered it, so a rewind, reset, fork or compaction that drops the
// result lets the file surface again. The files it appends are staged on
// the journal under the call's id; the recorder writes them into the same
// entry as the tool result, so a crash before that commit loses the record
// with the text and a resumed session surfaces the files again.
func nestedContextHook(cfg StackConfig, cwd string, local bool) func(context.Context, agent.AfterToolCallContext) *agent.AfterToolCallResult {
	if !local {
		return nil
	}
	startupBytes := 0
	for _, file := range cfg.ContextFiles {
		startupBytes += len(file.Content)
	}
	nested := discovery.NewNested(discovery.NestedOptions{
		Root: cwd, Files: cfg.ContextFileSettings, StartupBytes: startupBytes,
	})
	if nested == nil {
		return nil
	}
	deliveries := &contextDeliveries{files: map[deliveryKey][]discovery.NestedFile{}}
	if cfg.Loaded != nil {
		for _, delivery := range cfg.Loaded.ContextFiles {
			key := deliveryKey{turn: delivery.Turn, call: delivery.ToolCallID}
			for _, record := range delivery.Files {
				deliveries.files[key] = append(deliveries.files[key], discovery.NestedFile{Dir: record.Dir, Path: record.Path, Bytes: record.Bytes, Withheld: record.Withheld})
			}
		}
	}
	return func(ctx context.Context, call agent.AfterToolCallContext) *agent.AfterToolCallResult {
		if call.IsError || len(call.Result.Paths) == 0 {
			return nil
		}
		// One call at a time: parallel reads in one batch must see each
		// other's files before their results reach the conversation.
		deliveries.mu.Lock()
		defer deliveries.mu.Unlock()
		shown := deliveries.shown(call.Context.Messages, call.AssistantMessage)
		var blocks []string
		var files []discovery.NestedFile
		for _, event := range call.Result.Paths {
			found, surfaced, err := nested.Surface(ctx, event.Path, event.Dir, event.Whole, shown)
			blocks = append(blocks, found...)
			files = append(files, surfaced...)
			shown = append(shown, surfaced...)
			if err != nil {
				slog.Warn("nested context discovery stopped", "path", event.Path, "error", err)
				break
			}
		}
		if len(files) > 0 {
			key := deliveryKey{turn: call.AssistantMessage.Timestamp, call: call.ToolCall.ID}
			deliveries.files[key] = append(deliveries.files[key], files...)
			if cfg.Journal != nil {
				records := make([]journal.ContextFileRecord, 0, len(files))
				for _, file := range files {
					records = append(records, journal.ContextFileRecord{Dir: file.Dir, Path: file.Path, Bytes: file.Bytes, Withheld: file.Withheld})
				}
				cfg.Journal.StageContextFiles(contextCallKey(ctx, call.ToolCall.ID), records)
			}
		}
		if len(blocks) == 0 {
			return nil
		}
		content := slices.Clone(call.Result.Content)
		for _, block := range blocks {
			content = append(content, ptypes.NewText(block))
		}
		return &agent.AfterToolCallResult{Content: content}
	}
}

// deliveryKey names a tool result by its assistant turn's timestamp and its
// call ID: provider call IDs alone may repeat across turns.
type deliveryKey struct {
	turn int64
	call string
}

// contextDeliveries maps every tool result that delivered nested context
// files in this session, journaled or live, to those files.
type contextDeliveries struct {
	mu    sync.Mutex
	files map[deliveryKey][]discovery.NestedFile
}

// shown returns the files the conversation holds: those of each tool result
// in messages, and those of the calls in current, the batch now running,
// whose results may not be in messages yet.
func (d *contextDeliveries) shown(messages []ptypes.Message, current ptypes.AssistantMessage) []discovery.NestedFile {
	if len(d.files) == 0 {
		return nil
	}
	var running []deliveryKey
	for _, block := range current.Content {
		if block.Type == ptypes.BlockToolCall {
			running = append(running, deliveryKey{turn: current.Timestamp, call: block.ID})
		}
	}
	var shown []discovery.NestedFile
	var turn int64
	for _, message := range messages {
		switch m := message.(type) {
		case ptypes.AssistantMessage:
			turn = m.Timestamp
		case ptypes.ToolResultMessage:
			if key := (deliveryKey{turn: turn, call: m.ToolCallID}); !slices.Contains(running, key) {
				shown = append(shown, d.files[key]...)
			}
		}
	}
	for _, key := range running {
		shown = append(shown, d.files[key]...)
	}
	return shown
}

// contextCallKey names a tool call the way the recorder names its result:
// the durable invocation id, or the provider's call id without one.
func contextCallKey(ctx context.Context, toolCallID string) string {
	if invocation, ok := work.InvocationFromContext(ctx); ok && invocation.CallID != "" {
		return invocation.CallID
	}
	return toolCallID
}
