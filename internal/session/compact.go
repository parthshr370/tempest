package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/compaction"
	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/hooks"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/work"
	"go.harness.dev/harness/tempest/extension"
)

// compactionSummaryTimeout bounds a single summarization turn so a stalled
// provider call cannot wedge the build loop between turns.
const compactionSummaryTimeout = 120 * time.Second

// compactionRunner summarizes older turns when the estimated context approaches
// the model's window. It is wired into the agent loop's between-turn hook so a
// long, tool-heavy build never silently overflows the context window.
type compactionRunner struct {
	contextWindow    int
	model            ptypes.Model
	streamFn         agent.StreamFn
	settings         compaction.CompactionSettings
	store            *journal.Store
	agt              *agent.Agent
	parts            map[string]journal.CompactionPart
	hooks            *hooks.Runner
	hookCache        *hookCache
	beforeCompaction func(context.Context, string) error
}

// newCompactionRunner builds a runner for the given model and provider seam. It
// returns nil when compaction cannot run (no stream, or no usable context
// window to budget against), so the caller wires no hook.
func newCompactionRunner(model ptypes.Model, streamFn agent.StreamFn) *compactionRunner {
	if streamFn == nil || model.ContextWindow <= 0 {
		return nil
	}
	return &compactionRunner{
		contextWindow: model.ContextWindow,
		model:         model,
		streamFn:      streamFn,
		settings:      compaction.DefaultCompactionSettings,
	}
}
func (r *compactionRunner) configureJournal(store *journal.Store, loaded *journal.Loaded, agt *agent.Agent) {
	if r == nil {
		return
	}
	r.store = store
	r.agt = agt
	r.parts = map[string]journal.CompactionPart{}
	r.hookCache = newHookCache(store, loaded, agt)
	if loaded != nil {
		for key, value := range loaded.CompactionParts {
			r.parts[key] = value
		}
	}
}

func (r *compactionRunner) hookE(commit func([]ptypes.Message)) func(context.Context, agent.ShouldStopAfterTurnContext) (*agent.AgentLoopTurnUpdate, error) {
	if r == nil {
		return nil
	}
	return func(ctx context.Context, c agent.ShouldStopAfterTurnContext) (*agent.AgentLoopTurnUpdate, error) {
		compacted, changed, err := r.compact(ctx, c.Context.Messages)
		if err != nil {
			return nil, err
		}
		if !changed {
			return nil, nil
		}
		if commit != nil {
			commit(compacted)
		}
		next := c.Context
		next.Messages = compacted
		return &agent.AgentLoopTurnUpdate{Context: &next}, nil
	}
}

func stableHash(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}

func (r *compactionRunner) compact(ctx context.Context, messages []ptypes.Message) ([]ptypes.Message, bool, error) {
	if r == nil || len(messages) == 0 {
		return messages, false, nil
	}
	tokens := compaction.EstimateContextTokens(messages).Tokens
	if !compaction.ShouldCompact(tokens, r.contextWindow, r.settings) {
		return messages, false, nil
	}
	entries := messagesToEntries(messages)
	prep := compaction.PrepareCompaction(entries, r.settings)
	if prep == nil || len(prep.MessagesToSummarize)+len(prep.TurnPrefixMessages) == 0 {
		return messages, false, nil
	}
	source, err := stableHash(struct {
		Messages []ptypes.Message
		Model    string
		Settings compaction.CompactionSettings
	}{messages, r.model.ID, r.settings})
	if err != nil {
		return nil, false, err
	}
	preparation, err := stableHash(prep)
	if err != nil {
		return nil, false, err
	}
	id := "cmp_" + source[:32]
	hookResult, err := r.preCompactHook(ctx, source)
	if err != nil {
		return nil, false, err
	}
	if hookResult.Decision == "deny" || hookResult.Stop {
		return messages, false, nil
	}
	if r.beforeCompaction != nil {
		if err := r.beforeCompaction(ctx, source); err != nil {
			if errors.Is(err, extension.ErrDenied) {
				return messages, false, nil
			}
			return nil, false, err
		}
	}
	history := "No prior history."
	if len(prep.MessagesToSummarize) > 0 || !prep.IsSplitTurn {
		history, err = r.phase(ctx, id, source, preparation, "history", func(callCtx context.Context) (compaction.SummaryResult, error) {
			return compaction.GenerateSummaryResult(callCtx, r.streamFn, r.model, prep.Settings, prep.MessagesToSummarize, prep.PreviousSummary)
		})
		if err != nil {
			return nil, false, err
		}
	}
	summary := history
	if prep.IsSplitTurn && len(prep.TurnPrefixMessages) > 0 {
		prefix, err := r.phase(ctx, id, source, preparation, "prefix", func(callCtx context.Context) (compaction.SummaryResult, error) {
			return compaction.GenerateTurnPrefixSummaryResult(callCtx, r.streamFn, r.model, prep.Settings, prep.TurnPrefixMessages)
		})
		if err != nil {
			return nil, false, err
		}
		summary = history + "\n\n---\n\n**Turn Context (split turn):**\n\n" + prefix
	}
	read, modified := compaction.ComputeFileLists(prep.FileOps)
	summary += compaction.FormatFileOperations(read, modified)
	replacement := compaction.ApplyCompaction(entries, prep, summary)
	for _, text := range hookResult.Context {
		replacement = append(replacement, ptypes.UserMessage{Content: ptypes.StringContent("PreCompact hook context: " + text), Timestamp: 1})
	}
	// A deterministic timestamp lets an unreferenced pre-crash sidecar be reused.
	if len(replacement) > 0 {
		if user, ok := replacement[0].(ptypes.UserMessage); ok {
			originalTimestamp := int64(0)
			for _, entry := range entries {
				if entry.ID == prep.FirstKeptEntryID {
					if prior, ok := entry.Message.(ptypes.UserMessage); ok {
						originalTimestamp = prior.Timestamp
					}
					break
				}
			}
			if originalTimestamp == 0 {
				stamp := int64(1)
				for _, message := range messages {
					if a, ok := recordAssistant(message); ok && a.Timestamp > stamp {
						stamp = a.Timestamp
					}
				}
				user.Timestamp = stamp
				replacement[0] = user
			}
		}
	}
	if r.store != nil {
		snapshot, err := r.store.WriteSnapshot(id, replacement)
		if err != nil {
			return nil, false, err
		}
		state := r.agt.RunState()
		if err := r.store.Append(context.WithoutCancel(ctx), journal.CompactionEntry{Meta: journal.Meta{RunID: state.RunID}, Summary: summary, TokensBefore: tokens, SourceHash: source, CompactionID: id, Snapshot: snapshot}); err != nil {
			return nil, false, err
		}
		if err := r.agt.PublishWork(context.WithoutCancel(ctx), work.Event{Kind: "compaction", Action: "committed", RunID: state.RunID, CompactionID: id, SourceHash: source}); err != nil {
			return nil, false, err
		}
	}
	return replacement, true, nil
}

func (r *compactionRunner) preCompactHook(ctx context.Context, source string) (hooks.Result, error) {
	if !r.hooks.Has("PreCompact") {
		return hooks.Result{}, nil
	}
	if r.hookCache == nil {
		r.hookCache = newHookCache(r.store, nil, r.agt)
	}
	key := "precompact:" + source
	// Older journals may already contain a paid phase but no hook intent.
	if !r.hookCache.contains(key) {
		if _, ok := r.parts[source+":history"]; ok {
			return hooks.Result{}, nil
		}
		if _, ok := r.parts[source+":prefix"]; ok {
			return hooks.Result{}, nil
		}
	}
	return r.hookCache.run(ctx, key, "PreCompact", func(callCtx context.Context) (hooks.Result, error) {
		return r.hooks.Run(callCtx, hooks.Input{Event: "PreCompact", Trigger: "auto"})
	})
}

func (r *compactionRunner) phase(ctx context.Context, id, source, preparation, phase string, call func(context.Context) (compaction.SummaryResult, error)) (string, error) {
	if r.parts == nil {
		r.parts = map[string]journal.CompactionPart{}
	}
	key := source + ":" + phase
	if part, ok := r.parts[key]; ok {
		if part.Started == nil || part.Started.PreparationHash != preparation {
			return "", fmt.Errorf("compaction preparation mismatch")
		}
		if part.Finished == nil {
			return "", fmt.Errorf("compaction outcome_unknown: %s", phase)
		}
		if part.Finished.Status != "ok" {
			return "", fmt.Errorf("compaction cached error: %s", part.Finished.Error)
		}
		return part.Finished.Summary, nil
	}
	release, err := agent.AdmitProvider(ctx, r.model)
	defer release()
	if err != nil {
		return "", err
	}
	settings, _ := json.Marshal(r.settings)
	start := journal.CompactionCall{CompactionID: id, SourceHash: source, Phase: phase, Model: r.model.ID, Settings: settings, PreparationHash: preparation}
	runID := ""
	if r.agt != nil {
		runID = r.agt.RunState().RunID
	}
	appendFact := func(kind journal.EntryType, value journal.CompactionCall) error {
		if r.store == nil {
			return nil
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		return r.store.AppendWorkFact(context.WithoutCancel(ctx), work.Fact{Kind: string(kind), RunID: runID, Payload: raw})
	}
	if err := appendFact(journal.KindCompactionCallStarted, start); err != nil {
		return "", err
	}
	r.parts[key] = journal.CompactionPart{Started: &start}
	callCtx, cancel := context.WithTimeout(ctx, compactionSummaryTimeout)
	result, callErr := call(callCtx)
	cancel()
	finish := journal.CompactionCall{CompactionID: id, SourceHash: source, Phase: phase, Model: r.model.ID, Status: "ok", Summary: result.Text, Usage: result.Usage}
	if r.store != nil {
		finish.SourceRef = "compaction:" + r.store.ID() + ":" + id + ":" + phase
	} else {
		finish.SourceRef = "compaction:" + source + ":" + phase
	}
	if callErr != nil {
		finish.Status = "error"
		finish.Error = callErr.Error()
		finish.Summary = ""
	}
	if err := appendFact(journal.KindCompactionCallFinished, finish); err != nil {
		return "", err
	}
	r.parts[key] = journal.CompactionPart{Started: &start, Finished: &finish}
	if err := agent.AccountProviderUsage(context.WithoutCancel(ctx), finish.SourceRef, r.model.ID, result.Usage); err != nil {
		return "", err
	}
	if callErr != nil {
		return "", callErr
	}
	return result.Text, nil
}

// hook returns a next-turn callback that compacts between turns. commit, when
// non-nil, persists the compacted history to the agent so a later Prompt/
// Continue starts from the summary rather than the full transcript; subagents
// pass nil because their child agent runs a single Prompt.
func (r *compactionRunner) hook(commit func([]ptypes.Message)) func(agent.ShouldStopAfterTurnContext) *agent.AgentLoopTurnUpdate {
	if r == nil {
		return nil
	}
	return func(c agent.ShouldStopAfterTurnContext) *agent.AgentLoopTurnUpdate {
		compacted, ok := r.maybeCompact(c.Context.Messages)
		if !ok {
			return nil
		}
		if commit != nil {
			commit(compacted)
		}
		next := c.Context
		next.Messages = compacted
		return &agent.AgentLoopTurnUpdate{Context: &next}
	}
}

// maybeCompact summarizes older turns when the estimate exceeds the window minus
// the reserve. It returns the compacted messages and true on success, or the
// input and false when compaction does not run: under budget, nothing to
// summarize, or the summarizer failed. On failure the loop keeps the original
// history rather than replaying an over-budget request blindly.
func (r *compactionRunner) maybeCompact(messages []ptypes.Message) ([]ptypes.Message, bool) {
	compacted, changed, err := r.compact(context.Background(), messages)
	if err != nil {
		return messages, false
	}
	return compacted, changed
}

// messagesToEntries bridges the loop's flat message slice into the SessionEntry
// list the compaction engine reasons over. Every entry carries a non-empty ID
// (PrepareCompaction rejects empty IDs) and type "message" so role-based cut
// points resolve.
func messagesToEntries(messages []ptypes.Message) []compaction.SessionEntry {
	entries := make([]compaction.SessionEntry, 0, len(messages))
	for i, m := range messages {
		if m == nil {
			continue
		}
		entries = append(entries, compaction.SessionEntry{ID: strconv.Itoa(i + 1), Type: "message", Message: m})
	}
	return entries
}
