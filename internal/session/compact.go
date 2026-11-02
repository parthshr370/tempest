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
	"go.harness.dev/harness/internal/retry"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/work"
	"go.harness.dev/harness/tempest/extension"
)

// compactionSummaryTimeout bounds a single summarization request so a
// stalled provider call cannot wedge the build loop between turns.
const compactionSummaryTimeout = 120 * time.Second

// compactionRunner summarizes older turns when the estimated context approaches
// the model's window. It is wired into the agent loop's between-turn hook so a
// long, tool-heavy build never silently overflows the context window.
type compactionRunner struct {
	// contextWindow and settings are the primary model's budget.
	contextWindow int
	model         ptypes.Model
	// backups are the models behind streamFn after model, in chain order. A
	// run that switched to one keeps it, and so does its compaction.
	backups  []ptypes.Model
	streamFn agent.StreamFn
	settings compaction.CompactionSettings
	// overridden marks settings given by the host rather than derived from
	// the primary; they then apply on every route.
	overridden       bool
	store            *journal.Store
	agt              *agent.Agent
	parts            map[string]journal.CompactionPart
	hooks            *hooks.Runner
	hookCache        *hookCache
	beforeCompaction func(context.Context, string) error
	background       *compactionState
}

// newCompactionRunner builds a runner for the given model and provider seam. It
// returns nil when compaction cannot run (no stream, or no usable context
// window to budget against), so the caller wires no hook.
func newCompactionRunner(model ptypes.Model, streamFn agent.StreamFn) *compactionRunner {
	if streamFn == nil || model.ContextWindow <= 0 {
		return nil
	}
	lifetime, cancel := context.WithCancel(context.Background())
	return &compactionRunner{
		contextWindow: model.ContextWindow,
		model:         model,
		streamFn:      streamFn,
		settings:      compaction.ModelSettings(model.ContextWindow, model.MaxTokens),
		background:    &compactionState{ctx: lifetime, cancel: cancel},
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

// route is the model a compaction for a run on model is sent to: the backup
// the run switched to, or the primary for a run on it, or outside a run.
func (r *compactionRunner) route(model ptypes.Model) ptypes.Model {
	for _, backup := range r.backups {
		if retry.SameModel(backup, model) {
			return backup
		}
	}
	return r.model
}

// routeNamed is the model a captured compaction named by modelName
// ("provider/id"), false when it is no longer in the chain.
func (r *compactionRunner) routeNamed(modelName string) (ptypes.Model, bool) {
	for _, model := range append([]ptypes.Model{r.model}, r.backups...) {
		if compactionModelName(model) == modelName {
			return model, true
		}
	}
	return ptypes.Model{}, false
}

func compactionModelName(model ptypes.Model) string { return model.Provider + "/" + model.ID }

// budget is the context window and settings compaction works within for a
// request to model, the route: the primary's for the primary, and for a
// backup with a known window, that window with settings derived from it as
// they are for the primary (or the host's, when it gave them). A backup's
// smaller window then compacts sooner, and its summary fits it.
func (r *compactionRunner) budget(model ptypes.Model) (int, compaction.CompactionSettings) {
	if model.ContextWindow <= 0 || retry.SameModel(model, r.model) {
		return r.contextWindow, r.settings
	}
	if r.overridden {
		return model.ContextWindow, r.settings
	}
	settings := compaction.ModelSettings(model.ContextWindow, model.MaxTokens)
	settings.Enabled = r.settings.Enabled
	return model.ContextWindow, settings
}

func (r *compactionRunner) hookE(commit func([]ptypes.Message)) func(context.Context, agent.ShouldStopAfterTurnContext) (*agent.AgentLoopTurnUpdate, error) {
	if r == nil {
		return nil
	}
	return func(ctx context.Context, c agent.ShouldStopAfterTurnContext) (*agent.AgentLoopTurnUpdate, error) {
		var compacted []ptypes.Message
		var changed bool
		var err error
		model := r.route(c.Model)
		if c.Overflow {
			compacted, changed, err = r.recoverOverflow(ctx, c.Context.Messages, model)
		} else if r.background.tasks != nil {
			compacted, changed, err = r.backgroundTurn(ctx, c)
		} else {
			compacted, changed, err = r.compact(ctx, c.Context.Messages, model)
		}
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

func (r *compactionRunner) compact(ctx context.Context, messages []ptypes.Message, model ptypes.Model) ([]ptypes.Message, bool, error) {
	return r.summarize(ctx, messages, false, true, model)
}

func (r *compactionRunner) summarize(ctx context.Context, messages []ptypes.Message, force, persist bool, model ptypes.Model) ([]ptypes.Message, bool, error) {
	if r == nil {
		return messages, false, nil
	}
	_, settings := r.budget(model)
	return r.summarizeUsing(ctx, messages, force, persist, settings, model)
}

// summarizeUsing summarizes messages on model, the route (see route), under
// settings, unless they fit the route's budget and force is false.
func (r *compactionRunner) summarizeUsing(ctx context.Context, messages []ptypes.Message, force, persist bool, settings compaction.CompactionSettings, model ptypes.Model) ([]ptypes.Message, bool, error) {
	if r == nil || len(messages) == 0 {
		return messages, false, nil
	}
	tokens := compaction.EstimateContextTokens(messages).Tokens
	if window, _ := r.budget(model); !force && !compaction.ShouldCompact(tokens, window, settings) {
		return messages, false, nil
	}
	entries := messagesToEntries(messages)
	if force && settings.KeepRecentTokens >= tokens {
		settings.KeepRecentTokens = tokens / 2
	}
	prep := compaction.PrepareCompaction(entries, settings)
	if prep == nil || len(prep.MessagesToSummarize)+len(prep.TurnPrefixMessages) == 0 {
		return messages, false, nil
	}
	epoch := ""
	if r.store != nil {
		var err error
		epoch, err = r.store.ContextEpoch(ctx)
		if err != nil {
			return nil, false, err
		}
		if epoch == "initial" {
			epoch = ""
		}
	}
	source, err := stableHash(struct {
		Epoch    string `json:"Epoch,omitempty"`
		Messages []ptypes.Message
		Model    string
		Settings compaction.CompactionSettings
	}{epoch, messages, model.ID, settings})
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
	summarizer := compaction.Summarizer{Stream: r.streamFn, Settings: func(backup ptypes.Model) compaction.CompactionSettings {
		_, backupSettings := r.budget(backup)
		return backupSettings
	}}
	history := compaction.SummaryResult{Text: "No prior history.", Model: model}
	if len(prep.MessagesToSummarize) > 0 || !prep.IsSplitTurn {
		history, err = r.phase(ctx, model, id, source, preparation, "history", func(callCtx context.Context, meter compaction.Meter) (compaction.SummaryResult, error) {
			metered := summarizer
			metered.Meter = meter
			return metered.Summary(callCtx, model, prep.Settings, prep.MessagesToSummarize, prep.PreviousSummary)
		})
		if err != nil {
			return nil, false, err
		}
	}
	summary := history.Text
	if prep.IsSplitTurn && len(prep.TurnPrefixMessages) > 0 {
		// A switch to a backup during the history summary carries over.
		prefixModel, prefixSettings := history.Model, prep.Settings
		if !retry.SameModel(prefixModel, model) {
			_, prefixSettings = r.budget(prefixModel)
		}
		prefix, err := r.phase(ctx, prefixModel, id, source, preparation, "prefix", func(callCtx context.Context, meter compaction.Meter) (compaction.SummaryResult, error) {
			metered := summarizer
			metered.Meter = meter
			return metered.TurnPrefix(callCtx, prefixModel, prefixSettings, prep.TurnPrefixMessages)
		})
		if err != nil {
			return nil, false, err
		}
		summary = history.Text + "\n\n---\n\n**Turn Context (split turn):**\n\n" + prefix.Text
	}
	read, modified := compaction.ComputeFileLists(prep.FileOps)
	summary += compaction.FormatFileOperations(read, modified)
	replacement := compaction.ApplyCompaction(entries, prep, summary)
	for _, text := range hookResult.Context {
		replacement = append(replacement, ptypes.UserMessage{Content: ptypes.StringContent("PreCompact hook context: " + text), Timestamp: 1})
	}
	// A deterministic timestamp lets an unreferenced pre-crash sidecar be reused.
	// A kept reminder is never merged into, so its timestamp is not the summary's.
	if len(replacement) > 0 {
		if user, ok := replacement[0].(ptypes.UserMessage); ok {
			originalTimestamp := int64(0)
			for _, entry := range entries {
				if entry.ID == prep.FirstKeptEntryID {
					if prior, ok := entry.Message.(ptypes.UserMessage); ok && prior.Reminder == "" {
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
	if persist && r.store != nil {
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

// phase runs one summary phase ("history" or "prefix") of compaction id on
// model, or returns its journaled outcome. call writes the summary, sending
// every request through the meter it is given, which treats each request as
// the agent loop treats a turn: admitted against the run's cost cap before
// it is sent, and its receipt journaled and charged before the next can be
// admitted. The cap therefore stops a summary of many requests before the
// request that would spend past it.
//
// The phase's started fact is journaled with its first admitted request, so
// a phase refused before any request leaves no trace. A finished fact
// records the outcome, the summary or the error that ended it (a cost stop
// included); it carries no usage, since each request's receipt is its own
// usage record, named after the phase's source ref and the request's number.
// A phase cut short by cancellation or a request timeout records no outcome.
func (r *compactionRunner) phase(ctx context.Context, model ptypes.Model, id, source, preparation, phase string, call func(context.Context, compaction.Meter) (compaction.SummaryResult, error)) (compaction.SummaryResult, error) {
	if r.parts == nil {
		r.parts = map[string]journal.CompactionPart{}
	}
	key := source + ":" + phase
	if part, ok := r.parts[key]; ok {
		if part.Started == nil || part.Started.PreparationHash != preparation {
			return compaction.SummaryResult{}, fmt.Errorf("compaction preparation mismatch")
		}
		if part.Finished == nil {
			return compaction.SummaryResult{}, fmt.Errorf("compaction outcome_unknown: %s", phase)
		}
		if part.Finished.Status != "ok" {
			return compaction.SummaryResult{}, fmt.Errorf("compaction cached error: %s", part.Finished.Error)
		}
		return compaction.SummaryResult{Text: part.Finished.Summary, Model: model}, nil
	}
	ownerRunID := ""
	if _, native := ctx.Value(compactionTaskKey{}).(string); native && r.agt != nil {
		ctx, ownerRunID = r.agt.BackgroundProviderContext(ctx)
	}
	_, routeSettings := r.budget(model)
	settings, _ := json.Marshal(routeSettings)
	start := journal.CompactionCall{CompactionID: id, SourceHash: source, Phase: phase, Model: model.ID, Settings: settings, PreparationHash: preparation}
	runID := ""
	if r.agt != nil {
		runID = r.agt.RunState().RunID
	}
	if taskID, ok := ctx.Value(compactionTaskKey{}).(string); ok {
		runID = taskID
	}
	if ownerRunID != "" {
		runID = ownerRunID
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
	sourceRef := "compaction:" + source + ":" + phase
	if r.store != nil {
		sourceRef = "compaction:" + r.store.ID() + ":" + id + ":" + phase
	}
	started, interrupted, requests := false, false, 0
	meter := func(ctx context.Context, request ptypes.Model, send func(context.Context) (compaction.SummaryResult, error)) (compaction.SummaryResult, error) {
		release, err := agent.AdmitProvider(ctx, request)
		defer release()
		if err != nil {
			return compaction.SummaryResult{}, err
		}
		if !started {
			if err := appendFact(journal.KindCompactionCallStarted, start); err != nil {
				return compaction.SummaryResult{}, err
			}
			r.parts[key] = journal.CompactionPart{Started: &start}
			started = true
		}
		requests++
		callCtx, cancel := context.WithTimeout(ctx, compactionSummaryTimeout)
		result, sendErr := send(callCtx)
		if sendErr != nil && (callCtx.Err() != nil || errors.Is(sendErr, context.Canceled) || errors.Is(sendErr, context.DeadlineExceeded)) {
			interrupted = true
		}
		cancel()
		if err := r.chargeSummaryRequest(ctx, runID, sourceRef+":"+strconv.Itoa(requests), result); err != nil {
			return result, errors.Join(sendErr, err)
		}
		return result, sendErr
	}
	result, callErr := call(ctx, meter)
	if !started {
		return compaction.SummaryResult{}, callErr
	}
	if interrupted || ctx.Err() != nil {
		// The requests' receipts are journaled, but this cut has no known
		// outcome. Don't turn suspension into a reusable failure receipt.
		return compaction.SummaryResult{}, errors.Join(callErr, ctx.Err())
	}
	finish := journal.CompactionCall{CompactionID: id, SourceHash: source, Phase: phase, Model: model.ID, Status: "ok", Summary: result.Text, SourceRef: sourceRef}
	if callErr != nil {
		finish.Status = "error"
		finish.Error = callErr.Error()
		finish.Summary = ""
	}
	if err := appendFact(journal.KindCompactionCallFinished, finish); err != nil {
		return compaction.SummaryResult{}, errors.Join(callErr, err)
	}
	r.parts[key] = journal.CompactionPart{Started: &start, Finished: &finish}
	if callErr != nil {
		return compaction.SummaryResult{}, callErr
	}
	return result, nil
}

// chargeSummaryRequest journals one summary request's usage as its receipt
// under source, on the model that answered, then charges it to the run.
func (r *compactionRunner) chargeSummaryRequest(ctx context.Context, runID, source string, result compaction.SummaryResult) error {
	if result.Usage == (ptypes.Usage{}) {
		return nil
	}
	ctx = context.WithoutCancel(ctx)
	if r.store != nil {
		if err := r.store.Append(ctx, journal.UsageRecord{Meta: journal.Meta{RunID: runID}, Model: result.Model.ID, SourceRef: source, Usage: result.Usage}); err != nil {
			return err
		}
	}
	if err := agent.AccountProviderUsage(ctx, source, result.Model.ID, result.Usage); err != nil {
		return err
	}
	if r.background != nil && r.background.notifyUsage != nil {
		r.background.notifyUsage(source, result.Model.ID, result.Usage)
	}
	return nil
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
		compacted, ok := r.maybeCompact(c.Context.Messages, r.route(c.Model))
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
func (r *compactionRunner) maybeCompact(messages []ptypes.Message, model ptypes.Model) ([]ptypes.Message, bool) {
	compacted, changed, err := r.compact(context.Background(), messages, model)
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
