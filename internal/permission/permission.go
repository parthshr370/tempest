// Package permission provides tool approval modes and a destructive-command
// guard. The guard recognizes common shell operations, not arbitrary scripts.
package permission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Mode is a tool-gating mode: plan (block mutations, collect plan),
// acceptEdits (allow edits, block destructive), bypass (allow all),
// default (acceptEdits in headless).
type Mode string

const (
	// ModeDefault is equivalent to acceptEdits in the headless harness: there is
	// no interactive prompt surface, so non-destructive edits are allowed.
	ModeDefault Mode = "default"
	// ModePlan blocks mutations and commands outside the read-only allowlist.
	ModePlan Mode = "plan"
	// ModeAcceptEdits permits edits but keeps the destructive-command guard.
	ModeAcceptEdits Mode = "acceptEdits"
	// ModeBypass permits every tool call without permission checks.
	ModeBypass Mode = "bypass"
)

var destructivePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bnpm\s+(install|uninstall|update|ci|link|publish)`),
	regexp.MustCompile(`(?i)\byarn\s+(add|remove|install|publish)`),
	regexp.MustCompile(`(?i)\bpnpm\s+(add|remove|install|publish)`),
	regexp.MustCompile(`(?i)\bpip\s+(install|uninstall)`),
	regexp.MustCompile(`(?i)\bapt(-get)?\s+(install|remove|purge|update|upgrade)`),
	regexp.MustCompile(`(?i)\bbrew\s+(install|uninstall|upgrade)`),
	regexp.MustCompile(`(?i)\bsudo\b`),
	regexp.MustCompile(`(?i)\bsu\b`),
	regexp.MustCompile(`(?i)\bkill\b`),
	regexp.MustCompile(`(?i)\bpkill\b`),
	regexp.MustCompile(`(?i)\bkillall\b`),
	regexp.MustCompile(`(?i)\breboot\b`),
	regexp.MustCompile(`(?i)\bshutdown\b`),
	regexp.MustCompile(`(?i)\bsystemctl\s+(start|stop|restart|enable|disable)`),
	regexp.MustCompile(`(?i)\bservice\s+\S+\s+(start|stop|restart)`),
	regexp.MustCompile(`(?i)\b(vim?|nano|emacs|code|subl)\b`),
	regexp.MustCompile(`(?is)(^|[;[:space:]])[[:alnum:]_:.-]*\s*\(\)\s*\{[^}]*\|[^}]*&[^}]*\}\s*;`),
}

var safePatterns = []*regexp.Regexp{
	regexp.MustCompile(`^\s*cat\b`),
	regexp.MustCompile(`^\s*head\b`),
	regexp.MustCompile(`^\s*tail\b`),
	regexp.MustCompile(`^\s*less\b`),
	regexp.MustCompile(`^\s*more\b`),
	regexp.MustCompile(`^\s*grep\b`),
	regexp.MustCompile(`^\s*find\b`),
	regexp.MustCompile(`^\s*ls\b`),
	regexp.MustCompile(`^\s*pwd\b`),
	regexp.MustCompile(`^\s*echo\b`),
	regexp.MustCompile(`^\s*printf\b`),
	regexp.MustCompile(`^\s*wc\b`),
	regexp.MustCompile(`^\s*sort\b`),
	regexp.MustCompile(`^\s*uniq\b`),
	regexp.MustCompile(`^\s*diff\b`),
	regexp.MustCompile(`^\s*file\b`),
	regexp.MustCompile(`^\s*stat\b`),
	regexp.MustCompile(`^\s*du\b`),
	regexp.MustCompile(`^\s*df\b`),
	regexp.MustCompile(`^\s*tree\b`),
	regexp.MustCompile(`^\s*which\b`),
	regexp.MustCompile(`^\s*whereis\b`),
	regexp.MustCompile(`^\s*type\b`),
	regexp.MustCompile(`^\s*env\b`),
	regexp.MustCompile(`^\s*printenv\b`),
	regexp.MustCompile(`^\s*uname\b`),
	regexp.MustCompile(`^\s*whoami\b`),
	regexp.MustCompile(`^\s*id\b`),
	regexp.MustCompile(`^\s*date\b`),
	regexp.MustCompile(`^\s*cal\b`),
	regexp.MustCompile(`^\s*uptime\b`),
	regexp.MustCompile(`^\s*ps\b`),
	regexp.MustCompile(`^\s*top\b`),
	regexp.MustCompile(`^\s*htop\b`),
	regexp.MustCompile(`^\s*free\b`),
	regexp.MustCompile(`(?i)^\s*git\s+(status|log|diff|show|branch|remote|config\s+--get)`),
	regexp.MustCompile(`(?i)^\s*git\s+ls-`),
	regexp.MustCompile(`(?i)^\s*npm\s+(list|ls|view|info|search|outdated|audit)`),
	regexp.MustCompile(`(?i)^\s*yarn\s+(list|info|why|audit)`),
	regexp.MustCompile(`(?i)^\s*node\s+--version`),
	regexp.MustCompile(`(?i)^\s*python\s+--version`),
	regexp.MustCompile(`(?i)^\s*curl\s`),
	regexp.MustCompile(`(?i)^\s*wget\s+-O\s*-`),
	regexp.MustCompile(`^\s*jq\b`),
	regexp.MustCompile(`(?i)^\s*sed\s+-n`),
	regexp.MustCompile(`^\s*awk\b`),
	regexp.MustCompile(`^\s*rg\b`),
	regexp.MustCompile(`^\s*fd\b`),
	regexp.MustCompile(`^\s*bat\b`),
	regexp.MustCompile(`^\s*eza\b`),
}

var (
	boldOrItalicPattern = regexp.MustCompile(`\*{1,2}([^*]+)\*{1,2}`)
	codePattern         = regexp.MustCompile("`([^`]+)`")
	leadingVerbPattern  = regexp.MustCompile(`(?i)^(Use|Run|Execute|Create|Write|Read|Check|Verify|Update|Modify|Add|Remove|Delete|Install)\s+(the\s+)?`)
	spacesPattern       = regexp.MustCompile(`\s+`)
	planHeaderPattern   = regexp.MustCompile(`(?i)\*{0,2}Plan:\*{0,2}\s*\n`)
	numberedPlanPattern = regexp.MustCompile(`(?m)^\s*(\d+)[.)]\s+\*{0,2}([^*\n]+)`)
	trailingStars       = regexp.MustCompile(`\*{1,2}$`)
	doneStepPattern     = regexp.MustCompile(`(?i)\[DONE:(\d+)\]`)
	mkfsCommandPattern  = regexp.MustCompile(`(?i)(^|[;|&]\s*)\s*mkfs(\.[a-z0-9_-]+)?(\s|$)`)
)

// TodoItem is a single numbered plan step with completion state.
type TodoItem struct {
	Step      int
	Text      string
	Completed bool
}

// PlanCollector collects a plan from streaming text by extracting numbered
// todo items and tracking completed steps.
type PlanCollector struct {
	mu    sync.Mutex
	items []TodoItem
}

// NewPlanCollector starts an empty collector for streamed plan steps.
func NewPlanCollector() *PlanCollector {
	return &PlanCollector{}
}

// Observe feeds text into the collector. It extracts todo items from the
// text and marks completed steps from [DONE:N] markers. Returns the count
// of [DONE:N] markers found in this observation.
func (c *PlanCollector) Observe(text string) int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if items := ExtractTodoItems(text); len(items) > 0 {
		c.items = items
	}
	return MarkCompletedSteps(text, c.items)
}

// Items returns a copy of the collected todo items.
func (c *PlanCollector) Items() []TodoItem {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]TodoItem, len(c.items))
	copy(out, c.items)
	return out
}

// FormatPlan renders todo items as a checkbox checklist, one per line.
func FormatPlan(items []TodoItem) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	for _, item := range items {
		box := "[ ]"
		if item.Completed {
			box = "[x]"
		}
		fmt.Fprintf(&b, "%s %d. %s\n", box, item.Step, item.Text)
	}
	return strings.TrimRight(b.String(), "\n")
}

// ParseMode converts a configured mode value, treating blank values as [ModeDefault].
func ParseMode(value string) (Mode, error) {
	switch Mode(strings.TrimSpace(value)) {
	case "", ModeDefault:
		return ModeDefault, nil
	case ModePlan:
		return ModePlan, nil
	case ModeAcceptEdits:
		return ModeAcceptEdits, nil
	case ModeBypass:
		return ModeBypass, nil
	default:
		return "", fmt.Errorf("unknown permission mode %q", value)
	}
}

// Gate decides whether a tool invocation is allowed under the mode and policy.
func Gate(ctx context.Context, mode Mode, toolName string, args json.RawMessage, policy *Policy, toolCallID ...string) Outcome {
	return GateInDirectory(ctx, mode, toolName, args, policy, "", toolCallID...)
}

// GateInDirectory evaluates shell destinations relative to the tool's workspace.
func GateInDirectory(ctx context.Context, mode Mode, toolName string, args json.RawMessage, policy *Policy, cwd string, toolCallID ...string) Outcome {
	if ctx.Err() != nil {
		return Outcome{Reason: "Operation aborted", Source: SourcePromptCancelled}
	}
	if policy == nil {
		policy = &Policy{}
	}
	policyMu.Lock()
	decision, overridden := policy.Overrides[toolName]
	sessionDecision, sessionOverride := policy.promptOverrides[toolName]
	mcpDefault, hasMCPDefault := policy.MCPDefaults[toolName]
	policyMu.Unlock()
	deny := "Tool " + toolName + " denied by policy"
	if overridden && decision == DecideDeny {
		return Outcome{Reason: deny, Source: SourceOverride}
	}
	isMCP := strings.HasPrefix(toolName, "mcp__")
	if isMCP && mode == ModePlan {
		return Outcome{Reason: "Plan mode: MCP tools are disabled", Source: SourceMode}
	}
	if mode == ModeBypass {
		return Outcome{Allow: true, Source: SourceBypass}
	}
	tier := ClassifyTool(toolName)
	if toolName == "jobs" {
		var request struct {
			Action string `json:"action"`
		}
		if json.Unmarshal(args, &request) == nil && (request.Action == "" || request.Action == "list") {
			tier = TierRead
		}
	}
	command := ""
	if toolName == "bash" {
		command = commandFromArgs(args)
	}
	guardReason := ""
	if toolName == "bash" && isDestructiveCommand(command, cwd) {
		guardReason = "Command blocked by destructive-command guard (deletions, existing-file overwrites, in-place edits, and destructive git operations require approval; appends and new-file writes are allowed): " + command
	}
	req := PromptRequest{Tool: toolName, Tier: tier, Args: args, Reason: guardReason, Cwd: cwd}
	if len(toolCallID) != 0 {
		req.ToolCallID = toolCallID[0]
	}
	// A guard approval is per-call, even when the answer says "always".
	if guardReason != "" {
		if policy.Prompter == nil {
			return Outcome{Reason: guardReason, Source: SourceGuard}
		}
		return resolvePrompt(ctx, policy, req)
	}
	if sessionOverride {
		return Outcome{Allow: sessionDecision == DecideAllow, Source: SourceOverride}
	}
	if overridden {
		switch decision {
		case DecideAllow:
			return Outcome{Allow: true, Source: SourceOverride}
		case DecidePrompt:
			return resolvePrompt(ctx, policy, req)
		}
	}
	if mode == ModePlan && tier != TierRead {
		if toolName == "bash" && IsSafeCommand(command) {
			return Outcome{Allow: true, Source: SourceMode}
		}
		reason := "Plan mode: " + toolName + " is disabled because it can mutate the workspace"
		if toolName == "bash" {
			reason = "Plan mode: command blocked (not allowlisted). Disable plan mode or use a read-only command. Command: " + command
		}
		return Outcome{Reason: reason, Source: SourceMode}
	}
	if isMCP {
		if hasMCPDefault && mcpDefault == DecideAllow {
			return Outcome{Allow: true, Source: SourceMode}
		}
		if hasMCPDefault && mcpDefault == DecideDeny {
			return Outcome{Reason: deny, Source: SourceMode}
		}
		return resolvePrompt(ctx, policy, req)
	}
	return Outcome{Allow: true, Source: SourceMode}
}

func resolvePrompt(ctx context.Context, policy *Policy, req PromptRequest) Outcome {
	if policy.Prompter == nil {
		return Outcome{Reason: "Tool " + req.Tool + " requires approval but no interactive prompt is available", Source: SourceNoPrompter}
	}
	started := time.Now()
	choice, err := policy.Prompter.Approve(ctx, req)
	out := Outcome{Source: SourcePrompt, Prompted: true, Wait: time.Since(started)}
	if errors.Is(err, context.DeadlineExceeded) {
		out.Source = SourcePromptTimeout
	} else if err != nil || ctx.Err() != nil || choice == "" {
		out.Source = SourcePromptCancelled
	} else {
		out.Allow = choice == AllowOnce || choice == AllowAlways
		if req.Reason == "" && (choice == AllowAlways || choice == RejectAlways) {
			policyMu.Lock()
			if policy.promptOverrides == nil {
				policy.promptOverrides = make(map[string]Decision)
			}
			if out.Allow {
				policy.promptOverrides[req.Tool] = DecideAllow
			} else {
				policy.promptOverrides[req.Tool] = DecideDeny
			}
			policyMu.Unlock()
		}
	}
	if !out.Allow {
		out.Reason = "Tool " + req.Tool + " was not approved"
	}
	return out
}

// IsSafeCommand reports whether command is non-destructive and matches
// a safe (read-only / allowlisted) pattern.
func IsSafeCommand(command string) bool {
	if IsDestructiveCommand(command) {
		return false
	}
	if !hasReadOnlyShellSyntax(command) {
		return false
	}
	for _, pattern := range safePatterns {
		if pattern.MatchString(command) {
			return true
		}
	}
	return false
}

// CleanStepText strips markdown formatting (bold/italic/code), leading
// verb prefixes, and extra whitespace from a step description.
func CleanStepText(text string) string {
	cleaned := boldOrItalicPattern.ReplaceAllString(text, "$1")
	cleaned = codePattern.ReplaceAllString(cleaned, "$1")
	cleaned = leadingVerbPattern.ReplaceAllString(cleaned, "")
	cleaned = spacesPattern.ReplaceAllString(cleaned, " ")
	cleaned = strings.TrimSpace(cleaned)
	if cleaned != "" {
		runes := []rune(cleaned)
		runes[0] = unicode.ToUpper(runes[0])
		cleaned = string(runes)
	}
	runes := []rune(cleaned)
	if len(runes) > 50 {
		cleaned = string(runes[:47]) + "..."
	}
	return cleaned
}

// ExtractTodoItems reads numbered todo steps from a Plan section in message.
func ExtractTodoItems(message string) []TodoItem {
	items := []TodoItem{}
	loc := planHeaderPattern.FindStringIndex(message)
	if loc == nil {
		return items
	}
	planSection := message[loc[1]:]
	matches := numberedPlanPattern.FindAllStringSubmatch(planSection, -1)
	for _, match := range matches {
		if len(match) < 3 {
			continue
		}
		text := strings.TrimSpace(match[2])
		text = strings.TrimSpace(trailingStars.ReplaceAllString(text, ""))
		if len(text) <= 5 || strings.HasPrefix(text, "`") || strings.HasPrefix(text, "/") || strings.HasPrefix(text, "-") {
			continue
		}
		cleaned := CleanStepText(text)
		if len(cleaned) > 3 {
			items = append(items, TodoItem{Step: len(items) + 1, Text: cleaned, Completed: false})
		}
	}
	return items
}

// ExtractDoneSteps parses [DONE:N] markers from message and returns the
// step numbers.
func ExtractDoneSteps(message string) []int {
	steps := []int{}
	for _, match := range doneStepPattern.FindAllStringSubmatch(message, -1) {
		if len(match) < 2 {
			continue
		}
		step, err := strconv.Atoi(match[1])
		if err == nil {
			steps = append(steps, step)
		}
	}
	return steps
}

// MarkCompletedSteps marks matching todo items as completed using
// [DONE:N] markers found in text. Returns the count of [DONE:N]
// markers found.
func MarkCompletedSteps(text string, items []TodoItem) int {
	doneSteps := ExtractDoneSteps(text)
	for _, step := range doneSteps {
		for i := range items {
			if items[i].Step == step {
				items[i].Completed = true
				break
			}
		}
	}
	return len(doneSteps)
}

func isSafeRedirectSink(target string) bool {
	switch target {
	case "/dev/null", "/dev/stdin", "/dev/stdout", "/dev/stderr", "/dev/zero", "/dev/tty":
		return true
	default:
		return false
	}
}

func commandFromArgs(args json.RawMessage) string {
	var decoded struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(args, &decoded) != nil {
		return ""
	}
	return decoded.Command
}
