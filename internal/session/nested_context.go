package session

import (
	"context"
	"log/slog"
	"os"
	"slices"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/discovery"
	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/session/journal"
)

// nestedContextHook returns the AfterToolCall that appends nested context
// files after a successful read or listing. It is nil when
// context_files.nested is off or the workspace is not on this host, where
// the files would be read from the wrong filesystem.
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
			surfaced = append(surfaced, discovery.NestedFile{Dir: record.Dir, Path: record.Path, Bytes: record.Bytes})
		}
	}
	var record func(context.Context, discovery.NestedFile) error
	if cfg.Journal != nil {
		record = func(ctx context.Context, file discovery.NestedFile) error {
			return cfg.Journal.Append(ctx, journal.ContextFileRecord{Dir: file.Dir, Path: file.Path, Bytes: file.Bytes})
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// Without a home directory, ~/ imports stay literal text.
		slog.Debug("nested context imports without home directory", "error", err)
	}
	nested := discovery.NewNested(discovery.NestedOptions{
		Root: cwd, Files: cfg.ContextFileSettings, HomeDir: home,
		StartupBytes: startupBytes, Surfaced: surfaced, Record: record,
	})
	if nested == nil {
		return nil
	}
	return func(ctx context.Context, call agent.AfterToolCallContext) *agent.AfterToolCallResult {
		if call.IsError || len(call.Result.Paths) == 0 {
			return nil
		}
		var blocks []string
		for _, event := range call.Result.Paths {
			found, err := nested.Surface(ctx, event.Path, event.Dir)
			blocks = append(blocks, found...)
			if err != nil {
				slog.Warn("nested context discovery stopped", "path", event.Path, "error", err)
				break
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
