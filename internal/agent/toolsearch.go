package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.harness.dev/harness/internal/engine/estimate"
	"go.harness.dev/harness/internal/engine/types"
)

// ToolSearchName is the tool that loads deferred tools.
const ToolSearchName = "tool_search"

// ToolSearchMode says when the loop defers a request's deferrable tools.
type ToolSearchMode string

const (
	// ToolSearchAuto defers them once their schemas reach the threshold.
	ToolSearchAuto ToolSearchMode = "auto"
	// ToolSearchAlways defers them whenever there are any.
	ToolSearchAlways ToolSearchMode = "always"
	// ToolSearchNever sends every schema with every request.
	ToolSearchNever ToolSearchMode = "never"
)

// DefaultToolSearchThreshold is the estimated size, in tokens, of the
// deferrable tools' schemas at which [ToolSearchAuto] defers them.
const DefaultToolSearchThreshold = 10000

const (
	toolSearchDefaultLimit = 5
	toolSearchMaxLimit     = 20
	// toolSearchListingBytes bounds the deferred names listed in the
	// tool_search description.
	toolSearchListingBytes = 8 << 10
	// toolSearchSummaryRunes bounds one match's description line.
	toolSearchSummaryRunes = 200
)

// ToolSearch configures deferred tool loading. The zero value is automatic
// deferral at [DefaultToolSearchThreshold].
//
// When deferral applies, a request offers the tools that are not
// [AgentTool.Deferrable] in their usual order, then tool_search, then the
// deferred tools the model loaded, in the order it first loaded them. A
// load only ever appends to the list, and a loaded tool stays for the rest
// of the conversation, so the provider's cached prefix changes once per
// load and never otherwise. Which tools are loaded is read from the
// transcript (see [LoadedTools]), which the journal already holds: a
// resumed, compacted, rewound or forked conversation offers the list its
// transcript shows.
//
// Auto mode is sticky: once a conversation defers, it keeps deferring while
// it has deferrable tools, even when a catalog change takes their schemas
// back under the threshold, so the request layout does not flip with the
// catalog. The latch lives in the [Agent] until [Agent.Reset]; a
// conversation reopened in a new Agent (resume, fork) also defers from its
// first request when its transcript shows a tool_search call, and otherwise
// decides by the threshold again.
type ToolSearch struct {
	// Mode is when to defer; empty means [ToolSearchAuto].
	Mode ToolSearchMode
	// ThresholdTokens is the estimated schema size at which auto mode
	// defers; 0 means [DefaultToolSearchThreshold].
	ThresholdTokens int
}

// Validate rejects an unknown mode and a negative threshold.
func (s ToolSearch) Validate() error {
	switch s.Mode {
	case "", ToolSearchAuto, ToolSearchAlways, ToolSearchNever:
	default:
		return fmt.Errorf("tool search mode %q: want auto, always or never", s.Mode)
	}
	if s.ThresholdTokens < 0 {
		return errors.New("tool search threshold_tokens must not be negative")
	}
	return nil
}

// toolSearchDetails is a tool_search result's journaled details: the
// tools it loaded, which the next requests offer.
type toolSearchDetails struct {
	Loaded []string `json:"loaded"`
}

// toolView returns the tools one request offers, and whether it defers:
// tools unchanged when nothing is deferred, otherwise the eager tools,
// tool_search and the loaded deferred tools, followed by the deferred tools
// not loaded yet, marked unloaded so they are neither sent nor run.
// isSticky reports that this conversation deferred before (see
// [ToolSearch]). It reads only its arguments, so the same catalog,
// transcript and latch give the same view.
func toolView(tools []AgentTool, messages []types.Message, policy *ToolSearch, isSticky bool) ([]AgentTool, bool) {
	var settings ToolSearch
	if policy != nil {
		settings = *policy
	}
	if settings.Mode == ToolSearchNever {
		return tools, false
	}
	deferred, chars := 0, 0
	for _, tool := range tools {
		if tool.Name == ToolSearchName {
			// A tool already has the name: offering a second one would
			// make calls ambiguous, so nothing is deferred.
			return tools, false
		}
		if tool.Deferrable {
			deferred++
			chars += len(tool.Name) + len(tool.Description) + len(tool.Parameters)
		}
	}
	if deferred == 0 {
		return tools, false
	}
	names := LoadedTools(messages)
	threshold := cmp.Or(settings.ThresholdTokens, DefaultToolSearchThreshold)
	isOverThreshold := (chars+estimate.CharsPerToken-1)/estimate.CharsPerToken >= threshold
	if settings.Mode != ToolSearchAlways && !isOverThreshold && !isSticky && !slices.Contains(names, ToolSearchName) {
		return tools, false
	}

	view := make([]AgentTool, 0, len(tools)+1)
	catalog := make([]AgentTool, 0, deferred)
	index := make(map[string]int, deferred)
	for _, tool := range tools {
		if tool.Deferrable {
			index[tool.Name] = len(catalog)
			catalog = append(catalog, tool)
		} else {
			view = append(view, tool)
		}
	}
	loaded := make(map[string]bool)
	var order []int
	for _, name := range names {
		if i, ok := index[name]; ok && !loaded[name] {
			loaded[name] = true
			order = append(order, i)
		}
	}
	view = append(view, searchTool(catalog, loaded))
	for _, i := range order {
		view = append(view, catalog[i])
	}
	for _, tool := range catalog {
		if !loaded[tool.Name] {
			tool.unloaded = true
			view = append(view, tool)
		}
	}
	return view, true
}

// offeredTools is the schemas a request sends: every tool in the view
// except the unloaded ones.
func offeredTools(view []AgentTool) []types.Tool {
	tools := make([]types.Tool, 0, len(view))
	for _, tool := range view {
		if !tool.unloaded {
			tools = append(tools, tool.Tool)
		}
	}
	return tools
}

// LoadedTools lists, in first-appearance order, the tools messages show
// loaded: the names a compaction carried on its summary message
// ([types.UserMessage.LoadedTools]), the names a successful tool_search
// result loaded, and every tool with a successful result, tool_search
// itself included, which records that the conversation searched. A request
// offers the deferred ones. A compaction puts this list for the messages it
// replaces on its summary message, so the list, and the tools a request
// offers, are the same after the checkpoint as before it, and a loaded tool
// whose search and calls were summarized stays loaded.
func LoadedTools(messages []types.Message) []string {
	var names []string
	seen := make(map[string]bool)
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	for _, message := range messages {
		switch message := message.(type) {
		case types.UserMessage:
			for _, name := range message.LoadedTools {
				add(name)
			}
		case types.ToolResultMessage:
			if message.IsError {
				continue
			}
			add(message.ToolName)
			if message.ToolName != ToolSearchName {
				continue
			}
			// Every tool_search result this package writes has these
			// details; a result without them loaded nothing.
			var details toolSearchDetails
			if json.Unmarshal(message.Details, &details) == nil {
				for _, name := range details.Loaded {
					add(name)
				}
			}
		}
	}
	return names
}

// notLoadedMessage answers a call to a deferred tool the model has not
// loaded.
func notLoadedMessage(name string) string {
	return "Tool " + name + " is not loaded. Call " + ToolSearchName + " with its name to load it, then call it."
}

const toolSearchParameters = `{"type":"object","properties":{"query":{"type":"string","description":"Words from the name or description of the tools you need, or exact tool names separated by spaces."},"limit":{"type":"integer","minimum":1,"maximum":20,"description":"Most tools to load. Default 5."}},"required":["query"],"additionalProperties":false}`

// searchTool builds the tool_search tool over one request's deferred
// catalog. loaded is read only.
func searchTool(catalog []AgentTool, loaded map[string]bool) AgentTool {
	return AgentTool{
		Tool:  types.Tool{Name: ToolSearchName, Description: searchDescription(catalog), Parameters: json.RawMessage(toolSearchParameters)},
		Label: ToolSearchName,
		Execute: func(_ context.Context, _ string, params json.RawMessage, _ AgentToolUpdateCallback) (AgentToolResult, error) {
			var args struct {
				Query string `json:"query"`
				Limit int    `json:"limit"`
			}
			if err := json.Unmarshal(params, &args); err != nil {
				return AgentToolResult{}, err
			}
			if strings.TrimSpace(args.Query) == "" {
				return AgentToolResult{}, errors.New("query is empty: give words from a tool's name or description, or exact tool names")
			}
			limit := args.Limit
			if limit <= 0 {
				limit = toolSearchDefaultLimit
			}
			return searchResult(args.Query, rankTools(catalog, loaded, args.Query, min(limit, toolSearchMaxLimit)), loaded)
		},
	}
}

// searchDescription is tool_search's description: how to use it and the
// names of the deferred tools, within a byte budget. It changes only when
// the catalog does, never when a tool loads.
func searchDescription(catalog []AgentTool) string {
	var b strings.Builder
	b.WriteString("Load tools that are not in your tool list yet. The tools below are available but deferred: only their names are shown until you load them. ")
	b.WriteString("Search with words from the name or description of what you need, or give exact tool names separated by spaces to load just those. ")
	b.WriteString("The best matches, up to limit, are loaded and can be called from your next response. Loaded tools stay available for the rest of the conversation; search again only for a different capability. ")
	b.WriteString("If nothing matches, the capability is not available.\n\nDeferred tools: ")
	listed, listedBytes := 0, 0
	for i, tool := range catalog {
		if listed > 0 && listedBytes+len(tool.Name)+2 > toolSearchListingBytes {
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(tool.Name)
		listed++
		listedBytes += len(tool.Name) + 2
	}
	if rest := len(catalog) - listed; rest > 0 {
		fmt.Fprintf(&b, ", and %d more", rest)
	}
	b.WriteString(".")
	return b.String()
}

// rankTools returns up to limit catalog tools matching query, best first.
// A query that names catalog tools exactly selects just those, in query
// order. Otherwise tools score by query words: a word in the name counts
// twice a word only in the description. Tools not loaded yet come before
// loaded ones, then higher scores, then catalog order.
func rankTools(catalog []AgentTool, loaded map[string]bool, query string, limit int) []AgentTool {
	var exact []AgentTool
	for _, word := range strings.FieldsFunc(query, func(r rune) bool { return unicode.IsSpace(r) || r == ',' }) {
		i := slices.IndexFunc(catalog, func(tool AgentTool) bool { return strings.EqualFold(tool.Name, word) })
		if i >= 0 && !slices.ContainsFunc(exact, func(tool AgentTool) bool { return tool.Name == catalog[i].Name }) {
			exact = append(exact, catalog[i])
		}
	}
	if len(exact) > 0 {
		return exact[:min(limit, len(exact))]
	}
	terms := searchTerms(query)
	type match struct{ index, score int }
	var matches []match
	for i, tool := range catalog {
		name, description := searchTerms(nameWords(tool.Name)), searchTerms(tool.Description)
		m := match{index: i}
		for term := range terms {
			if name[term] {
				m.score += 2
			} else if description[term] {
				m.score++
			}
		}
		if m.score > 0 {
			matches = append(matches, m)
		}
	}
	slices.SortStableFunc(matches, func(a, b match) int {
		if la, lb := loaded[catalog[a.index].Name], loaded[catalog[b.index].Name]; la != lb {
			if lb {
				return -1
			}
			return 1
		}
		return cmp.Or(b.score-a.score, a.index-b.index)
	})
	ranked := make([]AgentTool, 0, min(limit, len(matches)))
	for _, m := range matches[:min(limit, len(matches))] {
		ranked = append(ranked, catalog[m.index])
	}
	return ranked
}

// nameWords drops the namespace marker every MCP or extension tool name
// starts with, which would match every tool: mcp__github__create_issue
// searches as github__create_issue.
func nameWords(name string) string {
	for _, prefix := range []string{"mcp__", "ext__"} {
		if rest, ok := strings.CutPrefix(name, prefix); ok {
			return rest
		}
	}
	return name
}

// searchTerms lowercases text and splits it into its runs of letters and
// digits, so github__create_issue has the terms github, create and issue.
func searchTerms(text string) map[string]bool {
	terms := make(map[string]bool)
	for _, term := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		terms[term] = true
	}
	return terms
}

// searchResult reports the matches, one line each, and records them as
// loaded in the details the next requests read.
func searchResult(query string, matches []AgentTool, loaded map[string]bool) (AgentToolResult, error) {
	details := toolSearchDetails{Loaded: make([]string, 0, len(matches))}
	var b strings.Builder
	if len(matches) == 0 {
		fmt.Fprintf(&b, "No deferred tool matches %q. The deferred tools are listed in the %s description; search with words from one of those names, or the capability is not available.", query, ToolSearchName)
	} else {
		fmt.Fprintf(&b, "Loaded %d tool(s); call them from your next response. They stay loaded for the rest of the conversation.", len(matches))
	}
	for _, tool := range matches {
		details.Loaded = append(details.Loaded, tool.Name)
		b.WriteString("\n- " + tool.Name)
		if loaded[tool.Name] {
			b.WriteString(" (already loaded)")
		}
		if summary := summaryLine(tool.Description); summary != "" {
			b.WriteString(": " + summary)
		}
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return AgentToolResult{}, err
	}
	return AgentToolResult{Content: []types.ContentBlock{types.NewText(b.String())}, Details: raw}, nil
}

// summaryLine is the first non-empty line of a description, cut to
// toolSearchSummaryRunes.
func summaryLine(description string) string {
	for line := range strings.SplitSeq(description, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if utf8.RuneCountInString(line) > toolSearchSummaryRunes {
			line = string([]rune(line)[:toolSearchSummaryRunes]) + "…"
		}
		return line
	}
	return ""
}
