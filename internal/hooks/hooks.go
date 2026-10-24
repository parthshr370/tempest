// Package hooks runs operator-configured lifecycle commands using the Claude Code wire contract.
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// Events is the supported command-hook lifecycle, in display order.
var Events = []string{"PreToolUse", "PostToolUse", "UserPromptSubmit", "Stop", "SubagentStop", "SessionStart", "SessionEnd", "PreCompact", "Notification"}

// Command is a synchronous shell hook. Timeout is in seconds; zero selects the event default.
type Command struct {
	Type    string  `json:"type"`
	Command string  `json:"command"`
	Timeout float64 `json:"timeout,omitempty"`
}

// Group selects handlers by tool name or the event-specific matcher value.
type Group struct {
	Matcher string    `json:"matcher,omitempty"`
	Hooks   []Command `json:"hooks"`
}

// Settings contains only operator-controlled execution and trust settings.
type Settings struct {
	Hooks             map[string][]Group `json:"hooks,omitempty"`
	NoHooks           bool               `json:"no_hooks,omitempty"`
	TrustProjectHooks bool               `json:"-"`
	TrustedProjects   []string           `json:"trusted_hook_projects,omitempty"`
}

// Clone keeps a caller's later edits from changing an admitted session's hooks.
func (s Settings) Clone() Settings {
	out := s
	out.TrustedProjects = slices.Clone(s.TrustedProjects)
	if s.Hooks != nil {
		out.Hooks = make(map[string][]Group, len(s.Hooks))
		for event, groups := range s.Hooks {
			out.Hooks[event] = slices.Clone(groups)
			for i := range groups {
				out.Hooks[event][i].Hooks = slices.Clone(groups[i].Hooks)
			}
		}
	}
	return out
}

// Validate rejects unsupported events, malformed matchers and invalid commands.
func (s Settings) Validate() error {
	for event, groups := range s.Hooks {
		if !slices.Contains(Events, event) {
			return fmt.Errorf("unsupported hook event %q", event)
		}
		for _, group := range groups {
			if _, err := compileMatcher(group.Matcher); err != nil {
				return fmt.Errorf("%s matcher: %w", event, err)
			}
			for _, command := range group.Hooks {
				if command.Type != "command" || strings.TrimSpace(command.Command) == "" {
					return fmt.Errorf("%s hooks require type command and a nonempty command", event)
				}
				if math.IsNaN(command.Timeout) || math.IsInf(command.Timeout, 0) || command.Timeout < 0 || command.Timeout >= float64(math.MaxInt64)/float64(time.Second) {
					return fmt.Errorf("%s hook timeout must be a nonnegative number of seconds", event)
				}
			}
		}
	}
	return nil
}

func compileMatcher(pattern string) (*regexp.Regexp, error) {
	if pattern == "" || pattern == "*" {
		return regexp.Compile(".*")
	}
	// Claude treats simple names and lists as exact matches, and other patterns as regexes.
	if strings.IndexFunc(pattern, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_- ,|", r))
	}) == -1 {
		names := strings.FieldsFunc(pattern, func(r rune) bool { return r == '|' || r == ',' })
		for i := range names {
			names[i] = regexp.QuoteMeta(strings.TrimSpace(names[i]))
		}
		pattern = "^(?:" + strings.Join(names, "|") + ")$"
	}
	return regexp.Compile(pattern)
}

// ResolveProject appends explicitly trusted project and local hooks after user hooks.
// A repository can't grant itself trust or disable the operator's hooks.
func ResolveProject(s Settings, cwd string) (Settings, []string, error) {
	s = s.Clone()
	if s.NoHooks {
		return s, nil, nil
	}
	root, err := filepath.Abs(cwd)
	if err != nil {
		return s, nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if errors.Is(err, os.ErrNotExist) {
		// An unopened workspace has no project executable settings to discover.
		return s, nil, s.Validate()
	}
	if err != nil {
		return s, nil, err
	}
	trusted := s.TrustProjectHooks
	for _, path := range s.TrustedProjects {
		if !filepath.IsAbs(path) {
			continue
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil && resolved == root {
			trusted = true
		}
	}
	var skipped []string
	for _, name := range []string{"settings.json", "settings.local.json"} {
		path := filepath.Join(root, ".harness", name)
		if !trusted {
			if _, err := os.Lstat(path); err == nil {
				skipped = append(skipped, path)
			} else if !errors.Is(err, os.ErrNotExist) {
				return s, skipped, err
			}
			continue
		}
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return s, skipped, err
		}
		var project struct {
			Hooks map[string][]Group `json:"hooks"`
		}
		if err := json.Unmarshal(data, &project); err != nil {
			return s, skipped, fmt.Errorf("project hooks %s: %w", path, err)
		}
		if err := (Settings{Hooks: project.Hooks}).Validate(); err != nil {
			return s, skipped, fmt.Errorf("project hooks %s: %w", path, err)
		}
		if s.Hooks == nil {
			s.Hooks = map[string][]Group{}
		}
		for event, groups := range project.Hooks {
			s.Hooks[event] = append(s.Hooks[event], groups...)
		}
	}
	return s, skipped, s.Validate()
}

// Input is the stdin object shared with existing command-hook scripts.
type Input struct {
	Key                  string `json:"-"`
	SessionID            string `json:"session_id"`
	TranscriptPath       string `json:"transcript_path,omitempty"`
	Cwd                  string `json:"cwd"`
	Event                string `json:"hook_event_name"`
	PermissionMode       string `json:"permission_mode,omitempty"`
	ToolName             string `json:"tool_name,omitempty"`
	ToolInput            any    `json:"tool_input,omitempty"`
	ToolResponse         any    `json:"tool_response,omitempty"`
	ToolUseID            string `json:"tool_use_id,omitempty"`
	Prompt               string `json:"prompt,omitempty"`
	Source               string `json:"source,omitempty"`
	Reason               string `json:"reason,omitempty"`
	Trigger              string `json:"trigger,omitempty"`
	NotificationType     string `json:"notification_type,omitempty"`
	Message              string `json:"message,omitempty"`
	StopHookActive       bool   `json:"stop_hook_active"`
	LastAssistantMessage string `json:"last_assistant_message,omitempty"`
	AgentID              string `json:"agent_id,omitempty"`
	AgentType            string `json:"agent_type,omitempty"`
	AgentTranscriptPath  string `json:"agent_transcript_path,omitempty"`
}

// Result is the combined decision. Deny wins over ask, which wins over allow.
type Result struct {
	Decision     string
	Reason       string
	UpdatedInput json.RawMessage
	Context      []string
	Stop         bool
	StopReason   string
	Interrupted  bool
}

// Report exposes bounded command output on every host's event stream and log.
type Report struct {
	Event    string `json:"hook_event_name"`
	Command  string `json:"command"`
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	Error    string `json:"error,omitempty"`
	TimedOut bool   `json:"timed_out,omitempty"`
}

type compiledGroup struct {
	matcher  *regexp.Regexp
	commands []Command
}

// Runner owns immutable, compiled hooks for one workspace/session.
type Runner struct {
	groups  map[string][]compiledGroup
	base    Input
	started atomic.Bool
	ended   atomic.Bool
	// PermissionMode reads the host's live mode; set it before starting the runner.
	PermissionMode func() string
	// Once journals keyed lifecycle effects before execution and reuses settled results.
	Once func(context.Context, string, string, func(context.Context) (Result, error)) (Result, error)
}

// New compiles settings. Project resolution belongs to the root composition layer.
func New(s Settings, base Input) (*Runner, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if s.NoHooks || len(s.Hooks) == 0 {
		return nil, nil
	}
	r := &Runner{groups: map[string][]compiledGroup{}, base: base}
	for event, groups := range s.Hooks {
		for _, group := range groups {
			matcher, _ := compileMatcher(group.Matcher)
			r.groups[event] = append(r.groups[event], compiledGroup{matcher, slices.Clone(group.Hooks)})
		}
	}
	return r, nil
}

// Start runs once for a newly created session, never during journal resume.
func (r *Runner) Start(ctx context.Context, resume bool) (Result, error) {
	if r == nil || !r.started.CompareAndSwap(false, true) || resume {
		return Result{}, nil
	}
	return r.Run(ctx, Input{Event: "SessionStart", Source: "startup"})
}

// MarkResumed suppresses startup even when a loaded journal has no conversation yet.
func (r *Runner) MarkResumed() {
	if r != nil {
		r.started.Store(true)
	}
}

// End runs once when the live session closes.
func (r *Runner) End(ctx context.Context) error {
	if r == nil || !r.started.Load() || !r.ended.CompareAndSwap(false, true) {
		return nil
	}
	budget, cancel := context.WithTimeout(ctx, DefaultTimeout("SessionEnd"))
	defer cancel()
	_, err := r.Run(budget, Input{Event: "SessionEnd", Reason: "other"})
	if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

type runKey struct{}
type runState struct {
	stop   atomic.Bool
	report func(context.Context, Report) error
}

// WithRun gives parallel tool hooks a shared stop signal and serialized event sink.
func WithRun(ctx context.Context, report func(context.Context, Report) error) context.Context {
	return context.WithValue(ctx, runKey{}, &runState{report: report})
}

// Stopped reports whether a hook requested continue:false in this run.
func Stopped(ctx context.Context) bool {
	state, _ := ctx.Value(runKey{}).(*runState)
	return state != nil && state.stop.Load()
}

// Has reports whether the runner has handlers for an event.
func (r *Runner) Has(event string) bool { return r != nil && len(r.groups[event]) > 0 }

// Run executes matching commands in settings order. Each sees preceding updatedInput.
func (r *Runner) Run(ctx context.Context, input Input) (Result, error) {
	if !r.Has(input.Event) {
		return Result{}, nil
	}
	if input.Key != "" && r.Once != nil {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		result, err := r.Once(ctx, input.Key, input.Event, func(callCtx context.Context) (Result, error) { return r.run(callCtx, input) })
		if result.Stop {
			if state, _ := ctx.Value(runKey{}).(*runState); state != nil {
				state.stop.Store(true)
			}
		}
		return result, err
	}
	return r.run(ctx, input)
}

func (r *Runner) run(ctx context.Context, input Input) (Result, error) {
	var result Result
	if r == nil {
		return result, nil
	}
	input.SessionID, input.Cwd = r.base.SessionID, r.base.Cwd
	input.TranscriptPath = r.base.TranscriptPath
	input.AgentID, input.AgentType, input.AgentTranscriptPath = r.base.AgentID, r.base.AgentType, r.base.AgentTranscriptPath
	if r.PermissionMode != nil {
		input.PermissionMode = r.PermissionMode()
		if input.PermissionMode == "bypass" {
			input.PermissionMode = "bypassPermissions"
		}
	}
	match := input.ToolName
	switch input.Event {
	case "SessionStart":
		match = input.Source
	case "SessionEnd":
		match = input.Reason
	case "PreCompact":
		match = input.Trigger
	case "Notification":
		match = input.NotificationType
	case "SubagentStop":
		match = input.AgentType
	}
	for _, group := range r.groups[input.Event] {
		if input.Event != "Stop" && input.Event != "UserPromptSubmit" && !group.matcher.MatchString(match) {
			continue
		}
		for _, command := range group.commands {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			payload, err := json.Marshal(input)
			if err != nil {
				return result, err
			}
			if start, ok := ctx.Value(commandStartKey{}).(func() error); ok {
				if err := start(); err != nil {
					return result, err
				}
			}
			report := execute(ctx, command, input.Event, input.Cwd, payload)
			slog.Info("hook completed", "event", report.Event, "command", report.Command, "exit_code", report.ExitCode, "stdout", report.Stdout, "stderr", report.Stderr, "error", report.Error)
			if state, _ := ctx.Value(runKey{}).(*runState); state != nil && state.report != nil {
				if err := state.report(context.WithoutCancel(ctx), report); err != nil {
					return result, err
				}
			}
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			if report.ExitCode == 2 {
				if canBlock(input.Event) {
					mergeDecision(&result, "deny", first(report.Stderr, "Hook blocked "+input.Event))
				}
			}
			if report.ExitCode != 0 && report.ExitCode != 2 {
				continue
			}
			output := strings.TrimSpace(report.Stdout)
			if output == "" {
				continue
			}
			var wire struct {
				Continue           *bool           `json:"continue"`
				StopReason         string          `json:"stopReason"`
				Decision           string          `json:"decision"`
				PermissionDecision string          `json:"permissionDecision"`
				Reason             string          `json:"reason"`
				UpdatedInput       json.RawMessage `json:"updatedInput"`
				AdditionalContext  string          `json:"additionalContext"`
				HookSpecificOutput struct {
					Event        string          `json:"hookEventName"`
					Decision     string          `json:"permissionDecision"`
					Reason       string          `json:"permissionDecisionReason"`
					UpdatedInput json.RawMessage `json:"updatedInput"`
					Context      string          `json:"additionalContext"`
				} `json:"hookSpecificOutput"`
			}
			if !strings.HasPrefix(output, "{") {
				if input.Event == "SessionStart" || input.Event == "UserPromptSubmit" {
					result.Context = append(result.Context, output)
				}
				continue
			}
			if err := json.Unmarshal([]byte(output), &wire); err != nil {
				slog.Warn("invalid hook JSON", "event", input.Event, "error", err)
				continue
			}
			specific := wire.HookSpecificOutput
			if specific.Event != "" && specific.Event != input.Event {
				continue
			}
			if wire.Continue != nil && !*wire.Continue && input.Event != "Notification" && input.Event != "SessionEnd" {
				result.Stop, result.StopReason = true, wire.StopReason
				if state, _ := ctx.Value(runKey{}).(*runState); state != nil {
					state.stop.Store(true)
				}
			}
			if input.Event == "PreToolUse" {
				decision := first(specific.Decision, wire.PermissionDecision, wire.Decision)
				if decision == "approve" {
					decision = "allow"
				}
				if decision == "block" {
					decision = "deny"
				}
				mergeDecision(&result, decision, first(specific.Reason, wire.Reason))
				updated := specific.UpdatedInput
				if len(updated) == 0 {
					updated = wire.UpdatedInput
				}
				if len(updated) > 0 && !bytes.Equal(bytes.TrimSpace(updated), []byte("null")) {
					result.UpdatedInput = updated
					input.ToolInput = updated
				}
			} else if canBlock(input.Event) && (wire.Decision == "block" || wire.Decision == "deny") {
				mergeDecision(&result, "deny", first(wire.Reason, "Hook blocked "+input.Event))
			}
			if wire.AdditionalContext != "" {
				result.Context = append(result.Context, wire.AdditionalContext)
			}
			if specific.Context != "" {
				result.Context = append(result.Context, specific.Context)
			}
		}
	}
	return result, nil
}

func canBlock(event string) bool {
	return event == "PreToolUse" || event == "PostToolUse" || event == "UserPromptSubmit" || event == "Stop" || event == "SubagentStop" || event == "PreCompact"
}
func first(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
func mergeDecision(result *Result, decision, reason string) {
	rank := map[string]int{"allow": 1, "ask": 2, "deny": 3}
	if rank[decision] > 0 && rank[decision] >= rank[result.Decision] {
		result.Decision = decision
		result.Reason = reason
	}
}

const outputLimit = 64 << 10

type cappedBuffer struct{ bytes.Buffer }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := outputLimit - b.Len(); remaining > 0 {
		_, _ = b.Buffer.Write(p[:min(remaining, n)])
	}
	return n, nil
}

func execute(ctx context.Context, command Command, event, cwd string, payload []byte) Report {
	timeout := DefaultTimeout(event)
	if command.Timeout > 0 {
		timeout = max(time.Nanosecond, time.Duration(command.Timeout*float64(time.Second)))
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command.Command)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "TEMPEST_PROJECT_DIR="+cwd, "CLAUDE_PROJECT_DIR="+cwd)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	var stdout, stderr cappedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	// Descendants that closed their pipes must not escape the lifecycle command.
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	report := Report{Event: event, Command: command.Command, ExitCode: 0, Stdout: stdout.String(), Stderr: stderr.String(), TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded)}
	if err != nil {
		report.ExitCode = -1
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			report.ExitCode = exit.ExitCode()
		} else if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil {
			report.ExitCode = cmd.ProcessState.ExitCode()
		}
		report.Error = err.Error()
		if report.TimedOut {
			report.Error = "hook timed out after " + timeout.String()
		}
	}
	return report
}

// DefaultTimeout follows the lifecycle budgets of the command-hook contract.
func DefaultTimeout(event string) time.Duration {
	switch event {
	case "UserPromptSubmit":
		return 30 * time.Second
	case "SessionEnd":
		return 1500 * time.Millisecond
	default:
		return 600 * time.Second
	}
}

// UnmatchedToolMatchers identifies tool-event filters that won't run for any admitted tool.
func (s Settings) UnmatchedToolMatchers(names []string) []string {
	var warnings []string
	for _, event := range []string{"PreToolUse", "PostToolUse"} {
		for _, group := range s.Hooks[event] {
			matcher, err := compileMatcher(group.Matcher)
			if err == nil && !slices.ContainsFunc(names, matcher.MatchString) {
				warnings = append(warnings, fmt.Sprintf("%s matcher %q matches no known tool", event, group.Matcher))
			}
		}
	}
	return warnings
}

// HookSettingsPath reports whether a mutation can replace executable hook settings.
// We resolve existing ancestors too, so a new file below a symlink isn't a bypass.
func HookSettingsPath(path, cwd string) bool {
	home, _ := os.UserHomeDir()
	canonical := func(path string) string {
		for _, prefix := range []string{"~/", "$HOME/", "${HOME}/"} {
			if home != "" && strings.HasPrefix(path, prefix) {
				path = filepath.Join(home, strings.TrimPrefix(path, prefix))
				break
			}
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		path, _ = filepath.Abs(path)
		var suffix []string
		const maxSymlinkSteps = 40
		symlinkSteps := 0
		for {
			if real, err := filepath.EvalSymlinks(path); err == nil {
				for i := len(suffix) - 1; i >= 0; i-- {
					real = filepath.Join(real, suffix[i])
				}
				return real
			}
			info, err := os.Lstat(path)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return ""
			}
			if err == nil && info.Mode()&os.ModeSymlink != 0 {
				symlinkSteps++
				link, err := os.Readlink(path)
				if err != nil || symlinkSteps > maxSymlinkSteps {
					return ""
				}
				if filepath.IsAbs(link) {
					path = link
				} else {
					path = filepath.Join(filepath.Dir(path), link)
				}
				continue
			}
			parent := filepath.Dir(path)
			if parent == path {
				return path
			}
			suffix = append(suffix, filepath.Base(path))
			path = parent
		}
	}
	target := canonical(path)
	if target == "" {
		return true
	}
	protected := []string{filepath.Join(cwd, ".harness", "settings.json"), filepath.Join(cwd, ".harness", "settings.local.json")}
	if home != "" {
		protected = append(protected, filepath.Join(home, ".config", "harness", "settings.json"))
	}
	for _, setting := range protected {
		setting = canonical(setting)
		if setting == "" || target == setting || strings.HasPrefix(setting, target+string(filepath.Separator)) {
			return true
		}
	}
	// Nested project settings can become executable when that directory is opened.
	return strings.HasPrefix(filepath.Base(target), "settings") && strings.HasSuffix(target, ".json") && filepath.Base(filepath.Dir(target)) == ".harness"
}

type commandStartKey struct{}

// WithCommandStart defers a durable intent until immediately before the first command.
func WithCommandStart(ctx context.Context, start func() error) context.Context {
	return context.WithValue(ctx, commandStartKey{}, start)
}
