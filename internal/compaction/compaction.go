// Package compaction implements the context-compaction layer: when the
// estimated context approaches the model's window, older turns are summarized
// and replaced by one summary message, keeping recent turns verbatim.
// Compaction is the only rewrite of history the model has seen: between
// compactions every request extends the previous one, so the provider's prompt
// cache keeps the whole prefix. Prior-compacted entries are skipped in token
// counting so the estimate doesn't inflate across multiple compaction passes.
package compaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"go.harness.dev/harness/internal/agent"
	ptypes "go.harness.dev/harness/internal/engine/types"
)

const (
	// ToolResultMaxChars is the maximum characters kept from a tool result when serializing for summarization.
	ToolResultMaxChars = 2000
	// EstimatedImageChars is the estimated character count for an image block, used in token estimation.
	EstimatedImageChars = 4800
	// EstimatedDocumentPageChars is the character count for one page of a
	// natively delivered (non-text) document such as a PDF, matching
	// estimate.EstimatedDocumentPageChars so ShouldCompact and the context
	// clamp weight attachments the same way.
	EstimatedDocumentPageChars = 3000 * 4

	compactionSummaryPrefix = "The conversation history before this point was compacted into the following summary:\n\n<summary>\n"
	compactionSummarySuffix = "\n</summary>"
	branchSummaryPrefix     = "The following is a summary of a branch that this conversation came back from:\n\n<summary>\n"
	branchSummarySuffix     = "</summary>"
)

// FileOperations tracks file paths that have been read, written, or edited across a conversation.
type FileOperations struct {
	Read    map[string]struct{}
	Written map[string]struct{}
	Edited  map[string]struct{}
}

// CompactionDetails carries the read and modified file lists included in a compaction summary.
type CompactionDetails struct {
	ReadFiles     []string `json:"readFiles"`
	ModifiedFiles []string `json:"modifiedFiles"`
}

// CompactionSettings controls whether and how aggressively context compaction runs.
type CompactionSettings struct {
	Enabled          bool
	ReserveTokens    int
	KeepRecentTokens int
}

// DefaultCompactionSettings is the default compaction config: enabled, 16384 reserve tokens, 20000 keep-recent tokens.
var DefaultCompactionSettings = CompactionSettings{Enabled: true, ReserveTokens: 16384, KeepRecentTokens: 20000}

// ContextUsageEstimate breaks down estimated token usage for a conversation context.
type ContextUsageEstimate struct {
	Tokens         int
	UsageTokens    int
	TrailingTokens int
	LastUsageIndex *int
}

// SessionEntry is a single entry in a conversation session, carrying a message and optional compaction metadata.
type SessionEntry struct {
	ID               string
	Type             string
	Message          ptypes.Message
	Summary          string
	TokensBefore     int
	FirstKeptEntryID string
	Details          *CompactionDetails
	FromHook         bool
	CustomType       string
	Content          []ptypes.ContentBlock
	Display          string
	Timestamp        int64
}

// CutPointResult describes where a conversation was cut during compaction and whether a turn was split.
type CutPointResult struct {
	FirstKeptEntryIndex int
	TurnStartIndex      int
	IsSplitTurn         bool
}

// CompactionPreparation holds everything needed to execute a compaction: messages to summarize, turn prefix, file ops, and settings.
type CompactionPreparation struct {
	FirstKeptEntryID    string
	MessagesToSummarize []ptypes.Message
	TurnPrefixMessages  []ptypes.Message
	IsSplitTurn         bool
	TokensBefore        int
	PreviousSummary     string
	FileOps             *FileOperations
	Settings            CompactionSettings
}

// CreateFileOps starts empty read, written, and edited file sets for one conversation.
func CreateFileOps() *FileOperations {
	return &FileOperations{Read: map[string]struct{}{}, Written: map[string]struct{}{}, Edited: map[string]struct{}{}}
}

// ExtractFileOpsFromMessage scans an assistant message for read, write, and edit tool calls, recording their path arguments in fileOps.
func ExtractFileOpsFromMessage(message ptypes.Message, fileOps *FileOperations) {
	assistant, ok := asAssistant(message)
	if !ok || fileOps == nil {
		return
	}
	for _, block := range assistant.Content {
		if block.Type != ptypes.BlockToolCall || block.Name == "" || len(block.Arguments) == 0 {
			continue
		}
		var args map[string]any
		if json.Unmarshal(block.Arguments, &args) != nil {
			continue
		}
		path, ok := args["path"].(string)
		if !ok || path == "" {
			continue
		}
		switch block.Name {
		case "read":
			fileOps.Read[path] = struct{}{}
		case "write":
			fileOps.Written[path] = struct{}{}
		case "edit":
			fileOps.Edited[path] = struct{}{}
		}
	}
}

// ComputeFileLists separates file ops into read-only and modified files. Modified includes both written and edited paths; a file that appears in both read and modified is reported only in modified. Both lists are sorted.
func ComputeFileLists(fileOps *FileOperations) (readFiles, modifiedFiles []string) {
	if fileOps == nil {
		return nil, nil
	}
	modified := map[string]struct{}{}
	for file := range fileOps.Edited {
		modified[file] = struct{}{}
	}
	for file := range fileOps.Written {
		modified[file] = struct{}{}
	}
	for file := range fileOps.Read {
		if _, ok := modified[file]; !ok {
			readFiles = append(readFiles, file)
		}
	}
	for file := range modified {
		modifiedFiles = append(modifiedFiles, file)
	}
	sort.Strings(readFiles)
	sort.Strings(modifiedFiles)
	return readFiles, modifiedFiles
}

// FormatFileOperations formats read and modified file lists as XML-tagged sections for inclusion in a compaction summary. Returns an empty string when both lists are empty.
func FormatFileOperations(readFiles, modifiedFiles []string) string {
	sections := []string{}
	if len(readFiles) > 0 {
		sections = append(sections, "<read-files>\n"+strings.Join(readFiles, "\n")+"\n</read-files>")
	}
	if len(modifiedFiles) > 0 {
		sections = append(sections, "<modified-files>\n"+strings.Join(modifiedFiles, "\n")+"\n</modified-files>")
	}
	if len(sections) == 0 {
		return ""
	}
	return "\n\n" + strings.Join(sections, "\n\n")
}

// SerializeConversation serializes messages into a compact text representation for LLM summarization. Tool-result content is truncated to ToolResultMaxChars.
func SerializeConversation(messages []ptypes.Message) string {
	parts := []string{}
	for _, msg := range messages {
		switch value := msg.(type) {
		case ptypes.UserMessage:
			if content := userContentText(value.Content); content != "" {
				parts = append(parts, "[User]: "+content)
			}
		case *ptypes.UserMessage:
			if value != nil {
				if content := userContentText(value.Content); content != "" {
					parts = append(parts, "[User]: "+content)
				}
			}
		case ptypes.AssistantMessage:
			parts = appendAssistantParts(parts, value)
		case *ptypes.AssistantMessage:
			if value != nil {
				parts = appendAssistantParts(parts, *value)
			}
		case ptypes.ToolResultMessage:
			if content := toolResultText(value); content != "" {
				parts = append(parts, "[Tool result]: "+truncateForSummary(content, ToolResultMaxChars))
			}
		case *ptypes.ToolResultMessage:
			if value != nil {
				if content := toolResultText(*value); content != "" {
					parts = append(parts, "[Tool result]: "+truncateForSummary(content, ToolResultMaxChars))
				}
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// CalculateContextTokens returns the total token count from a usage struct, preferring TotalTokens when nonzero and falling back to summing per-category fields.
func CalculateContextTokens(usage ptypes.Usage) int {
	if usage.TotalTokens != 0 {
		return usage.TotalTokens
	}
	return usage.Input + usage.Output + usage.CacheRead + usage.CacheWrite
}

// GetLastAssistantUsage scans entries backward for the most recent message entry that carries assistant usage info.
func GetLastAssistantUsage(entries []SessionEntry) *ptypes.Usage {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Type == "message" {
			if usage := getAssistantUsage(entries[i].Message); usage != nil {
				return usage
			}
		}
	}
	return nil
}

// EstimateContextTokens combines the latest provider usage with estimates for later messages, falling back to estimates for the whole conversation when usage is unavailable.
func EstimateContextTokens(messages []ptypes.Message) ContextUsageEstimate {
	usage, index := lastAssistantUsageInfo(messages)
	if usage == nil {
		estimated := 0
		for _, message := range messages {
			estimated += EstimateTokens(message)
		}
		return ContextUsageEstimate{Tokens: estimated, UsageTokens: 0, TrailingTokens: estimated, LastUsageIndex: nil}
	}
	usageTokens := CalculateContextTokens(*usage)
	trailingTokens := 0
	for i := index + 1; i < len(messages); i++ {
		trailingTokens += EstimateTokens(messages[i])
	}
	lastIndex := index
	return ContextUsageEstimate{Tokens: usageTokens + trailingTokens, UsageTokens: usageTokens, TrailingTokens: trailingTokens, LastUsageIndex: &lastIndex}
}

// ShouldCompact reports whether context compaction should trigger: tokens exceed the window minus the reserve margin, and settings allow it.
func ShouldCompact(contextTokens, contextWindow int, settings CompactionSettings) bool {
	if !settings.Enabled {
		return false
	}
	return contextTokens > contextWindow-settings.ReserveTokens
}

// EstimateTokens estimates the token count of an agent message by dividing character counts by 4 (≈ chars per token).
func EstimateTokens(message ptypes.Message) int {
	if message == nil {
		return 0
	}
	switch value := message.(type) {
	case ptypes.UserMessage:
		return ceilDiv(estimateUserContentChars(value.Content), 4)
	case *ptypes.UserMessage:
		if value == nil {
			return 0
		}
		return ceilDiv(estimateUserContentChars(value.Content), 4)
	case ptypes.AssistantMessage:
		return estimateAssistantTokens(value)
	case *ptypes.AssistantMessage:
		if value == nil {
			return 0
		}
		return estimateAssistantTokens(*value)
	case ptypes.ToolResultMessage:
		return ceilDiv(estimateBlocksChars(value.Content), 4)
	case *ptypes.ToolResultMessage:
		if value == nil {
			return 0
		}
		return ceilDiv(estimateBlocksChars(value.Content), 4)
	default:
		return 0
	}
}

// FindTurnStartIndex searches backward from entryIndex to find the nearest turn start: a user or bashExecution message, a branch summary, or a custom message. Returns -1 when no start is found in range.
func FindTurnStartIndex(entries []SessionEntry, entryIndex, startIndex int) int {
	for i := entryIndex; i >= startIndex; i-- {
		entry := entries[i]
		if entry.Type == "branch_summary" || entry.Type == "custom_message" {
			return i
		}
		if entry.Type == "message" && entry.Message != nil {
			role := entry.Message.Role()
			if role == "user" || role == "bashExecution" {
				return i
			}
		}
	}
	return -1
}

// FindCutPoint chooses a safe compaction boundary while retaining the requested recent-token budget.
func FindCutPoint(entries []SessionEntry, startIndex, endIndex, keepRecentTokens int) CutPointResult {
	cutPoints := findValidCutPoints(entries, startIndex, endIndex)
	if len(cutPoints) == 0 {
		return CutPointResult{FirstKeptEntryIndex: startIndex, TurnStartIndex: -1, IsSplitTurn: false}
	}
	accumulatedTokens := 0
	cutIndex := cutPoints[0]
	for i := endIndex - 1; i >= startIndex; i-- {
		entry := entries[i]
		if entry.Type != "message" {
			continue
		}
		accumulatedTokens += EstimateTokens(entry.Message)
		if accumulatedTokens >= keepRecentTokens {
			for c := 0; c < len(cutPoints); c++ {
				if cutPoints[c] >= i {
					cutIndex = cutPoints[c]
					break
				}
			}
			break
		}
	}
	for cutIndex > startIndex {
		prevEntry := entries[cutIndex-1]
		if prevEntry.Type == "compaction" {
			break
		}
		if prevEntry.Type == "message" {
			break
		}
		cutIndex--
	}
	cutEntry := entries[cutIndex]
	isUserMessage := cutEntry.Type == "message" && cutEntry.Message != nil && cutEntry.Message.Role() == "user"
	turnStartIndex := -1
	if !isUserMessage {
		turnStartIndex = FindTurnStartIndex(entries, cutIndex, startIndex)
	}
	return CutPointResult{FirstKeptEntryIndex: cutIndex, TurnStartIndex: turnStartIndex, IsSplitTurn: !isUserMessage && turnStartIndex != -1}
}

// PrepareCompaction gathers the messages, file operations, and boundary details needed for a compaction pass.
func PrepareCompaction(pathEntries []SessionEntry, settings CompactionSettings) *CompactionPreparation {
	if len(pathEntries) > 0 && pathEntries[len(pathEntries)-1].Type == "compaction" {
		return nil
	}
	prevCompactionIndex := -1
	for i := len(pathEntries) - 1; i >= 0; i-- {
		if pathEntries[i].Type == "compaction" {
			prevCompactionIndex = i
			break
		}
	}
	previousSummary := ""
	boundaryStart := 0
	if prevCompactionIndex >= 0 {
		prevCompaction := pathEntries[prevCompactionIndex]
		previousSummary = prevCompaction.Summary
		firstKeptEntryIndex := -1
		for i, entry := range pathEntries {
			if entry.ID == prevCompaction.FirstKeptEntryID {
				firstKeptEntryIndex = i
				break
			}
		}
		if firstKeptEntryIndex >= 0 {
			boundaryStart = firstKeptEntryIndex
		} else {
			boundaryStart = prevCompactionIndex + 1
		}
	}
	boundaryEnd := len(pathEntries)
	tokensBefore := EstimateContextTokens(buildSessionContext(pathEntries)).Tokens
	cutPoint := FindCutPoint(pathEntries, boundaryStart, boundaryEnd, settings.KeepRecentTokens)
	if cutPoint.FirstKeptEntryIndex < 0 || cutPoint.FirstKeptEntryIndex >= len(pathEntries) || pathEntries[cutPoint.FirstKeptEntryIndex].ID == "" {
		return nil
	}
	firstKeptEntryID := pathEntries[cutPoint.FirstKeptEntryIndex].ID
	historyEnd := cutPoint.FirstKeptEntryIndex
	if cutPoint.IsSplitTurn {
		historyEnd = cutPoint.TurnStartIndex
	}
	messagesToSummarize := []ptypes.Message{}
	for i := boundaryStart; i < historyEnd; i++ {
		if msg := getMessageFromEntryForCompaction(pathEntries[i]); msg != nil {
			messagesToSummarize = append(messagesToSummarize, msg)
		}
	}
	turnPrefixMessages := []ptypes.Message{}
	if cutPoint.IsSplitTurn {
		for i := cutPoint.TurnStartIndex; i < cutPoint.FirstKeptEntryIndex; i++ {
			if msg := getMessageFromEntryForCompaction(pathEntries[i]); msg != nil {
				turnPrefixMessages = append(turnPrefixMessages, msg)
			}
		}
	}
	if len(messagesToSummarize) == 0 && len(turnPrefixMessages) == 0 {
		return nil
	}
	fileOps := extractFileOperations(messagesToSummarize, pathEntries, prevCompactionIndex)
	if cutPoint.IsSplitTurn {
		for _, msg := range turnPrefixMessages {
			ExtractFileOpsFromMessage(msg, fileOps)
		}
	}
	return &CompactionPreparation{FirstKeptEntryID: firstKeptEntryID, MessagesToSummarize: messagesToSummarize, TurnPrefixMessages: turnPrefixMessages, IsSplitTurn: cutPoint.IsSplitTurn, TokensBefore: tokensBefore, PreviousSummary: previousSummary, FileOps: fileOps, Settings: settings}
}

// findValidCutPoints never starts a kept slice on a tool result, so its call
// stays in context with the result that answers it.

func findValidCutPoints(entries []SessionEntry, startIndex, endIndex int) []int {
	cutPoints := []int{}
	for i := startIndex; i < endIndex; i++ {
		entry := entries[i]
		switch entry.Type {
		case "message":
			if entry.Message == nil {
				break
			}
			switch entry.Message.Role() {
			case "bashExecution", "custom", "branchSummary", "compactionSummary", "user", "assistant":
				cutPoints = append(cutPoints, i)
			case "toolResult":
			}
		case "thinking_level_change", "model_change", "compaction", "branch_summary", "custom", "custom_message", "label", "session_info":
		}
		if entry.Type == "branch_summary" || entry.Type == "custom_message" {
			cutPoints = append(cutPoints, i)
		}
	}
	return cutPoints
}

// extractFileOperations carries files from the earlier in-process summary
// forward, since another pass only scans the new portion of history.

func extractFileOperations(messages []ptypes.Message, entries []SessionEntry, prevCompactionIndex int) *FileOperations {
	fileOps := CreateFileOps()
	if prevCompactionIndex >= 0 {
		prevCompaction := entries[prevCompactionIndex]
		if !prevCompaction.FromHook && prevCompaction.Details != nil {
			for _, file := range prevCompaction.Details.ReadFiles {
				fileOps.Read[file] = struct{}{}
			}
			for _, file := range prevCompaction.Details.ModifiedFiles {
				fileOps.Edited[file] = struct{}{}
			}
		}
	}
	for _, msg := range messages {
		ExtractFileOpsFromMessage(msg, fileOps)
	}
	return fileOps
}

func getMessageFromEntry(entry SessionEntry) ptypes.Message {
	switch entry.Type {
	case "message":
		return entry.Message
	case "custom_message":
		return ptypes.UserMessage{Content: ptypes.BlockContent(entry.Content...), Timestamp: entry.Timestamp}
	case "branch_summary":
		return ptypes.UserMessage{Content: ptypes.StringContent(branchSummaryPrefix + entry.Summary + branchSummarySuffix), Timestamp: entry.Timestamp}
	case "compaction":
		return ptypes.UserMessage{Content: ptypes.StringContent(compactionSummaryPrefix + entry.Summary + compactionSummarySuffix), Timestamp: entry.Timestamp}
	default:
		return nil
	}
}

// getMessageFromEntryForCompaction leaves prior summaries out because the update
// prompt receives that summary separately.

func getMessageFromEntryForCompaction(entry SessionEntry) ptypes.Message {
	if entry.Type == "compaction" {
		return nil
	}
	return getMessageFromEntry(entry)
}

func buildSessionContext(entries []SessionEntry) []ptypes.Message {
	messages := []ptypes.Message{}
	for _, entry := range buildContextEntries(entries) {
		if msg := getMessageFromEntry(entry); msg != nil {
			messages = append(messages, msg)
		}
	}
	return messages
}

// buildContextEntries reconstructs the live suffix around the latest compaction
// entry, which records its kept boundary without rewriting the session log.

func buildContextEntries(entries []SessionEntry) []SessionEntry {
	compactionIndex := -1
	for i := range entries {
		if entries[i].Type == "compaction" {
			compactionIndex = i
		}
	}
	if compactionIndex < 0 {
		return entries
	}
	compactionEntry := entries[compactionIndex]
	contextEntries := []SessionEntry{compactionEntry}
	foundFirstKept := false
	for i := 0; i < compactionIndex; i++ {
		entry := entries[i]
		if entry.ID == compactionEntry.FirstKeptEntryID {
			foundFirstKept = true
		}
		if foundFirstKept {
			contextEntries = append(contextEntries, entry)
		}
	}
	contextEntries = append(contextEntries, entries[compactionIndex+1:]...)
	return contextEntries
}

// getAssistantUsage ignores aborted and failed replies: their usage doesn't
// describe a settled provider context we can use as an anchor.

func getAssistantUsage(msg ptypes.Message) *ptypes.Usage {
	assistant, ok := asAssistant(msg)
	if !ok || assistant.StopReason == ptypes.StopAborted || assistant.StopReason == ptypes.StopError || CalculateContextTokens(assistant.Usage) <= 0 {
		return nil
	}
	usage := assistant.Usage
	return &usage
}

func lastAssistantUsageInfo(messages []ptypes.Message) (*ptypes.Usage, int) {
	for i := len(messages) - 1; i >= 0; i-- {
		if usage := getAssistantUsage(messages[i]); usage != nil {
			return usage, i
		}
	}
	return nil, -1
}

func estimateAssistantTokens(message ptypes.AssistantMessage) int {
	chars := 0
	for _, block := range message.Content {
		switch block.Type {
		case ptypes.BlockText:
			chars += len(block.Text)
		case ptypes.BlockThinking:
			chars += len(block.Thinking)
		case ptypes.BlockToolCall:
			chars += len(block.Name) + len(jsonString(block.Arguments))
		}
	}
	return ceilDiv(chars, 4)
}

func estimateUserContentChars(content ptypes.UserContent) int {
	if content.IsBlocks() {
		return estimateBlocksChars(content.Blocks)
	}
	return len(content.Text)
}

func estimateBlocksChars(blocks []ptypes.ContentBlock) int {
	chars := 0
	for _, block := range blocks {
		switch block.Type {
		case ptypes.BlockText:
			chars += len(block.Text)
		case ptypes.BlockImage, ptypes.BlockImageRef:
			chars += EstimatedImageChars
		case ptypes.BlockDocumentRef:
			chars += documentRefChars(block)
		}
	}
	return chars
}

// documentRefChars weights a document reference block by its delivered form:
// text-derived documents (OOXML extraction, plain text) count their byte size,
// native paged documents (PDF) count EstimatedDocumentPageChars per page. This
// mirrors estimate.documentRefEstimatedChars so both estimators agree; a flat
// image weight here would hide a large attachment from compaction.
func documentRefChars(block ptypes.ContentBlock) int {
	switch block.RefMediaType {
	case "text/plain", "text/markdown", "text/csv":
		if block.RefSizeBytes <= 0 {
			return 0
		}
		return int(block.RefSizeBytes)
	default:
		pages := block.RefPageCount
		if pages < 1 {
			pages = 1
		}
		return pages * EstimatedDocumentPageChars
	}
}

func userContentText(content ptypes.UserContent) string {
	if !content.IsBlocks() {
		return content.Text
	}
	parts := []string{}
	for _, block := range content.Blocks {
		if block.Type == ptypes.BlockText {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "")
}

func appendAssistantParts(parts []string, message ptypes.AssistantMessage) []string {
	textParts := []string{}
	thinkingParts := []string{}
	toolCalls := []string{}
	for _, block := range message.Content {
		switch block.Type {
		case ptypes.BlockText:
			textParts = append(textParts, block.Text)
		case ptypes.BlockThinking:
			thinkingParts = append(thinkingParts, block.Thinking)
		case ptypes.BlockToolCall:
			toolCalls = append(toolCalls, fmt.Sprintf("%s(%s)", block.Name, formatToolArgs(block.Arguments)))
		}
	}
	if len(thinkingParts) > 0 {
		parts = append(parts, "[Assistant thinking]: "+strings.Join(thinkingParts, "\n"))
	}
	if len(textParts) > 0 {
		parts = append(parts, "[Assistant]: "+strings.Join(textParts, "\n"))
	}
	if len(toolCalls) > 0 {
		parts = append(parts, "[Assistant tool calls]: "+strings.Join(toolCalls, "; "))
	}
	return parts
}

func toolResultText(message ptypes.ToolResultMessage) string {
	parts := []string{}
	for _, block := range message.Content {
		if block.Type == ptypes.BlockText {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "")
}

func truncateForSummary(text string, maxChars int) string {
	if len(text) <= maxChars {
		return text
	}
	truncatedChars := len(text) - maxChars
	return fmt.Sprintf("%s\n\n[... %d more characters truncated]", text[:maxChars], truncatedChars)
}

func formatToolArgs(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	values, ok := orderedJSONObject(raw)
	if !ok {
		return string(raw)
	}
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, fmt.Sprintf("%s=%s", value.key, jsonStringValue(value.value)))
	}
	return strings.Join(parts, ", ")
}

type orderedJSONValue struct {
	key   string
	value any
}

// orderedJSONObject preserves the caller's argument order so summaries don't
// shuffle tool calls merely because they were serialized.
func orderedJSONObject(raw json.RawMessage) ([]orderedJSONValue, bool) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, false
	}
	values := []orderedJSONValue{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, false
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, false
		}
		var value any
		if err := decoder.Decode(&value); err != nil {
			return nil, false
		}
		values = append(values, orderedJSONValue{key: key, value: value})
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim('}') {
		return nil, false
	}
	return values, true
}

func jsonString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	return string(raw)
}

func jsonStringValue(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return "null"
	}
	return string(data)
}

func asAssistant(message ptypes.Message) (ptypes.AssistantMessage, bool) {
	switch value := message.(type) {
	case ptypes.AssistantMessage:
		return value, true
	case *ptypes.AssistantMessage:
		if value != nil {
			return *value, true
		}
	}
	return ptypes.AssistantMessage{}, false
}

func ceilDiv(n, d int) int {
	return int(math.Ceil(float64(n) / float64(d)))
}

// SummarizationSystemPrompt is the system prompt sent to the LLM when summarizing compacted conversation history.
const SummarizationSystemPrompt = `You are a context summarization assistant. Your task is to read a conversation between a user and an AI assistant, then produce a structured summary following the exact format specified.

Do NOT continue the conversation. Do NOT respond to any questions in the conversation. ONLY output the structured summary.`

// summarizationPrompt is appended after the serialized conversation to steer the
// model toward a structured checkpoint summary.
const summarizationPrompt = `The messages above are a conversation to summarize. Create a structured context checkpoint summary that another LLM will use to continue the work.

Use this EXACT format:

## Goal
[What is the user trying to accomplish? Can be multiple items if the session covers different tasks.]

## Constraints & Preferences
- [Any constraints, preferences, or requirements mentioned by user]
- [Or "(none)" if none were mentioned]

## Progress
### Done
- [x] [Completed tasks/changes]

### In Progress
- [ ] [Current work]

### Blocked
- [Issues preventing progress, if any]

## Key Decisions
- **[Decision]**: [Brief rationale]

## Next Steps
1. [Ordered list of what should happen next]

## Critical Context
- [Any data, examples, or references needed to continue]
- [Or "(none)" if not applicable]

Keep each section concise. Preserve exact file paths, function names, and error messages.`

// updateSummarizationPrompt merges new messages into an existing summary carried
// in <previous-summary> tags.
const updateSummarizationPrompt = `The messages above are NEW conversation messages to incorporate into the existing summary provided in <previous-summary> tags.

Update the existing structured summary with new information. RULES:
- PRESERVE all existing information from the previous summary
- ADD new progress, decisions, and context from the new messages
- UPDATE the Progress section: move items from "In Progress" to "Done" when completed
- UPDATE "Next Steps" based on what was accomplished
- PRESERVE exact file paths, function names, and error messages
- If something is no longer relevant, you may remove it

Use this EXACT format:

## Goal
[Preserve existing goals, add new ones if the task expanded]

## Constraints & Preferences
- [Preserve existing, add new ones discovered]

## Progress
### Done
- [x] [Include previously done items AND newly completed items]

### In Progress
- [ ] [Current work - update based on progress]

### Blocked
- [Current blockers - remove if resolved]

## Key Decisions
- **[Decision]**: [Brief rationale] (preserve all previous, add new)

## Next Steps
1. [Update based on current state]

## Critical Context
- [Preserve important context, add new if needed]

Keep each section concise. Preserve exact file paths, function names, and error messages.`

// turnPrefixSummarizationPrompt summarizes the dropped prefix of a split turn so
// the retained suffix keeps its context.
const turnPrefixSummarizationPrompt = `This is the PREFIX of a turn that was too large to keep. The SUFFIX (recent work) is retained.

Summarize the prefix to provide context for the retained suffix:

## Original Request
[What did the user ask for in this turn?]

## Early Progress
- [Key decisions and work done in the prefix]

## Context for Suffix
- [Information needed to understand the retained recent work]

Be concise. Focus on what's needed to understand the kept suffix.`

// GenerateSummary runs one summarization turn over messages, returning the
// model's structured summary. When previousSummary is non-empty it uses the
// update prompt so the new summary supersedes the old one.
func GenerateSummary(ctx context.Context, streamFn agent.StreamFn, model ptypes.Model, settings CompactionSettings, messages []ptypes.Message, previousSummary string) (string, error) {
	return meterSummary(ctx, model, func() (SummaryResult, error) {
		return GenerateSummaryResult(ctx, streamFn, model, settings, messages, previousSummary)
	})
}

// SummaryResult retains usage even when the provider returns an error.
type SummaryResult struct {
	Text  string
	Usage ptypes.Usage
}

// GenerateSummaryResult returns a summary and its receipt data. Durable callers
// hold shared admission until they've persisted and forwarded this receipt.
func GenerateSummaryResult(ctx context.Context, streamFn agent.StreamFn, model ptypes.Model, settings CompactionSettings, messages []ptypes.Message, previousSummary string) (SummaryResult, error) {
	basePrompt := summarizationPrompt
	if strings.TrimSpace(previousSummary) != "" {
		basePrompt = updateSummarizationPrompt
	}
	var promptText strings.Builder
	promptText.WriteString("<conversation>\n")
	promptText.WriteString(SerializeConversation(messages))
	promptText.WriteString("\n</conversation>\n\n")
	if strings.TrimSpace(previousSummary) != "" {
		promptText.WriteString("<previous-summary>\n")
		promptText.WriteString(previousSummary)
		promptText.WriteString("\n</previous-summary>\n\n")
	}
	promptText.WriteString(basePrompt)
	return runSummarizationResult(ctx, streamFn, model, summaryMaxTokens(model, settings), promptText.String())
}

// GenerateTurnPrefixSummary summarizes the dropped prefix of a split turn.
func GenerateTurnPrefixSummary(ctx context.Context, streamFn agent.StreamFn, model ptypes.Model, settings CompactionSettings, messages []ptypes.Message) (string, error) {
	return meterSummary(ctx, model, func() (SummaryResult, error) {
		return GenerateTurnPrefixSummaryResult(ctx, streamFn, model, settings, messages)
	})
}

// GenerateTurnPrefixSummaryResult retains the split-prefix call's usage.
func GenerateTurnPrefixSummaryResult(ctx context.Context, streamFn agent.StreamFn, model ptypes.Model, settings CompactionSettings, messages []ptypes.Message) (SummaryResult, error) {
	promptText := "<conversation>\n" + SerializeConversation(messages) + "\n</conversation>\n\n" + turnPrefixSummarizationPrompt
	return runSummarizationResult(ctx, streamFn, model, summaryMaxTokens(model, settings), promptText)
}

func meterSummary(ctx context.Context, model ptypes.Model, call func() (SummaryResult, error)) (string, error) {
	release, err := agent.AdmitProvider(ctx, model)
	defer release()
	if err != nil {
		return "", err
	}
	result, callErr := call()
	if err := agent.AccountProviderUsage(ctx, "", model.ID, result.Usage); err != nil {
		return "", err
	}
	return result.Text, callErr
}

// summaryMaxTokens caps a summarization response at 80% of the reserve budget,
// never exceeding the model's own max.
func summaryMaxTokens(model ptypes.Model, settings CompactionSettings) int {
	maxTokens := int(0.8 * float64(settings.ReserveTokens))
	if model.MaxTokens > 0 && model.MaxTokens < maxTokens {
		maxTokens = model.MaxTokens
	}
	return maxTokens
}

// runSummarization issues a single stateless summarization request and joins the
// returned text blocks. An error stop reason surfaces as an error.
func runSummarizationResult(ctx context.Context, streamFn agent.StreamFn, model ptypes.Model, maxTokens int, promptText string) (SummaryResult, error) {
	if streamFn == nil {
		return SummaryResult{}, errors.New("compaction: streamFn is nil")
	}
	reqCtx := ptypes.Context{SystemPrompt: SummarizationSystemPrompt, Messages: []ptypes.Message{ptypes.UserMessage{Content: ptypes.StringContent(promptText), Timestamp: time.Now().UnixMilli()}}}
	opts := &ptypes.SimpleStreamOptions{}
	opts.MaxTokens = maxTokens
	streamCtx, cancel := context.WithCancel(ctx)
	response := streamFn(streamCtx, model, reqCtx, opts)
	if response == nil {
		cancel()
		return SummaryResult{}, errors.New("compaction: nil stream")
	}
	stop := context.AfterFunc(ctx, cancel)
	defer func() { stop(); cancel() }()
	// Attempts the retry layer abandoned can still be billed; count the usage they reported.
	var discarded ptypes.Usage
	for event := range response.Events() {
		if event.Type == ptypes.EvRetry && event.Err != nil {
			discarded.Add(event.Err.Usage)
		}
	}
	final, err := response.Result(context.WithoutCancel(ctx))
	if err != nil {
		return SummaryResult{}, err
	}
	if final == nil {
		return SummaryResult{}, errors.New("compaction: nil summarization result")
	}
	result := SummaryResult{Usage: discarded}
	result.Usage.Add(final.Usage)
	if final.StopReason == ptypes.StopError || final.StopReason == ptypes.StopAborted {
		msg := final.ErrorMessage
		if msg == "" {
			msg = string(final.StopReason)
		}
		return result, fmt.Errorf("compaction: summarization failed: %s", msg)
	}
	var b strings.Builder
	for _, block := range final.Content {
		if block.Type == ptypes.BlockText {
			if b.Len()+len(block.Text)+1 > 512<<10 {
				return result, errors.New("compaction: summary exceeds 512 KiB")
			}
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(block.Text)
		}
	}
	result.Text = b.String()
	if strings.TrimSpace(result.Text) == "" {
		return result, errors.New("compaction: summarization produced empty output")
	}
	return result, nil
}

// Compact runs the summarization turn(s) for a prepared compaction and returns
// the merged summary: the history summary, a split-turn prefix summary when the
// cut fell mid-turn, and the read/modified file lists.
func Compact(ctx context.Context, streamFn agent.StreamFn, model ptypes.Model, prep *CompactionPreparation) (string, error) {
	if prep == nil {
		return "", errors.New("compaction: nil preparation")
	}
	var summary string
	if prep.IsSplitTurn && len(prep.TurnPrefixMessages) > 0 {
		history := "No prior history."
		if len(prep.MessagesToSummarize) > 0 {
			var err error
			history, err = GenerateSummary(ctx, streamFn, model, prep.Settings, prep.MessagesToSummarize, prep.PreviousSummary)
			if err != nil {
				return "", err
			}
		}
		prefix, err := GenerateTurnPrefixSummary(ctx, streamFn, model, prep.Settings, prep.TurnPrefixMessages)
		if err != nil {
			return "", err
		}
		summary = history + "\n\n---\n\n**Turn Context (split turn):**\n\n" + prefix
	} else {
		var err error
		summary, err = GenerateSummary(ctx, streamFn, model, prep.Settings, prep.MessagesToSummarize, prep.PreviousSummary)
		if err != nil {
			return "", err
		}
	}
	readFiles, modifiedFiles := ComputeFileLists(prep.FileOps)
	summary += FormatFileOperations(readFiles, modifiedFiles)
	return summary, nil
}

// ApplyCompaction rebuilds the conversation after a compaction: the summary as a
// synthetic user turn, then every kept entry from prep.FirstKeptEntryID onward.
// When the first kept message is itself a user message the summary is merged into
// it, since Anthropic rejects two consecutive user turns and a non-split cut
// commonly lands on a user message.
func ApplyCompaction(pathEntries []SessionEntry, prep *CompactionPreparation, summary string) []ptypes.Message {
	if prep == nil {
		return buildSessionContext(pathEntries)
	}
	kept := keptMessages(pathEntries, prep.FirstKeptEntryID)
	summaryText := compactionSummaryPrefix + summary + compactionSummarySuffix
	if len(kept) > 0 {
		if user, ok := kept[0].(ptypes.UserMessage); ok {
			return append([]ptypes.Message{mergeSummaryIntoUser(summaryText, user)}, kept[1:]...)
		}
		if user, ok := kept[0].(*ptypes.UserMessage); ok && user != nil {
			return append([]ptypes.Message{mergeSummaryIntoUser(summaryText, *user)}, kept[1:]...)
		}
	}
	result := []ptypes.Message{ptypes.UserMessage{Content: ptypes.StringContent(summaryText), Timestamp: time.Now().UnixMilli()}}
	return append(result, kept...)
}

// keptMessages returns the messages from firstKeptEntryID to the end of the path.
func keptMessages(pathEntries []SessionEntry, firstKeptEntryID string) []ptypes.Message {
	start := -1
	for i := range pathEntries {
		if pathEntries[i].ID == firstKeptEntryID {
			start = i
			break
		}
	}
	if start < 0 {
		return nil
	}
	msgs := []ptypes.Message{}
	for i := start; i < len(pathEntries); i++ {
		if msg := getMessageFromEntry(pathEntries[i]); msg != nil {
			msgs = append(msgs, msg)
		}
	}
	return msgs
}

// mergeSummaryIntoUser prepends the summary as a text block to a user message so
// the rebuilt context avoids two consecutive user turns.
func mergeSummaryIntoUser(summaryText string, user ptypes.UserMessage) ptypes.UserMessage {
	blocks := []ptypes.ContentBlock{ptypes.NewText(summaryText)}
	if user.Content.IsBlocks() {
		blocks = append(blocks, user.Content.Blocks...)
	} else if strings.TrimSpace(user.Content.Text) != "" {
		blocks = append(blocks, ptypes.NewText(user.Content.Text))
	}
	ts := user.Timestamp
	if ts == 0 {
		ts = time.Now().UnixMilli()
	}
	return ptypes.UserMessage{Content: ptypes.BlockContent(blocks...), Timestamp: ts}
}
