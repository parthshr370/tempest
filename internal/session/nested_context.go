package session

import (
	"context"
	"log/slog"
	"slices"

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
// The files it appends are staged on the journal under the call's id; the
// recorder writes them into the same entry as the tool result, so a crash
// before that commit loses the record with the text and a resumed session
// surfaces the files again.
func nestedContextHook(cfg StackConfig, cwd string, local bool) func(context.Context, agent.AfterToolCallContext) *agent.AfterToolCallResult {
	if !local {
		return nil
	}
	startupBytes := 0
	for _, file := range cfg.ContextFiles {
		startupBytes += len(file.Content)
	}
	var surfaced []discovery.NestedFile
	if cfg.Loaded != nil {
		for _, record := range cfg.Loaded.ContextFiles {
			surfaced = append(surfaced, discovery.NestedFile{Dir: record.Dir, Path: record.Path, Bytes: record.Bytes, Withheld: record.Withheld})
		}
	}
	nested := discovery.NewNested(discovery.NestedOptions{
		Root: cwd, Files: cfg.ContextFileSettings, StartupBytes: startupBytes, Surfaced: surfaced,
	})
	if nested == nil {
		return nil
	}
	return func(ctx context.Context, call agent.AfterToolCallContext) *agent.AfterToolCallResult {
		if call.IsError || len(call.Result.Paths) == 0 {
			return nil
		}
		var blocks []string
		var records []journal.ContextFileRecord
		for _, event := range call.Result.Paths {
			found, files, err := nested.Surface(ctx, event.Path, event.Dir, event.Whole)
			blocks = append(blocks, found...)
			for _, file := range files {
				records = append(records, journal.ContextFileRecord{Dir: file.Dir, Path: file.Path, Bytes: file.Bytes, Withheld: file.Withheld})
			}
			if err != nil {
				slog.Warn("nested context discovery stopped", "path", event.Path, "error", err)
				break
			}
		}
		if len(records) > 0 && cfg.Journal != nil {
			cfg.Journal.StageContextFiles(contextCallKey(ctx, call.ToolCall.ID), records)
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

// contextCallKey names a tool call the way the recorder names its result:
// the durable invocation id, or the provider's call id without one.
func contextCallKey(ctx context.Context, toolCallID string) string {
	if invocation, ok := work.InvocationFromContext(ctx); ok && invocation.CallID != "" {
		return invocation.CallID
	}
	return toolCallID
}
