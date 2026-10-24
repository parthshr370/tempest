package permission

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"go.harness.dev/harness/internal/cloudcmd"
	"go.harness.dev/harness/internal/hashline"
	"go.harness.dev/harness/internal/hooks"
)

func hookSettingsReason(tool string, args json.RawMessage, cwd string) string {
	if ClassifyTool(tool) == TierRead {
		return ""
	}
	var input struct {
		Path       string `json:"path"`
		Input      string `json:"input"`
		Command    string `json:"command"`
		Code       string `json:"code"`
		Text       string `json:"text"`
		Action     string `json:"action"`
		Expression string `json:"expression"`
	}
	if json.Unmarshal(args, &input) != nil {
		return ""
	}
	paths := []string{}
	switch tool {
	case "hashline":
		if patch, err := hashline.Parse(input.Input); err == nil {
			paths = patch.Paths()
		}
	case "bash":
		if inspectCommand(input.Command, cwd).planReadable() {
			return ""
		}
		if mentionsHookSettings(input.Command) {
			return "Changing hook settings requires per-call approval"
		}
		if shellChangesHookSettings(input.Command, cwd) {
			return "Changing hook settings requires per-call approval"
		}
	case "eval":
		if mentionsHookSettings(input.Code) {
			return "Interpreter access to hook settings requires per-call approval"
		}
	case "debug":
		if input.Action == "evaluate" && mentionsHookSettings(input.Expression) {
			return "Interpreter access to hook settings requires per-call approval"
		}
	case "write_stdin":
		if mentionsHookSettings(input.Text) || shellChangesHookSettings(input.Text, cwd) {
			return "Process input can change hook settings and requires per-call approval"
		}
	default:
		if input.Path != "" {
			paths = append(paths, input.Path)
		}
	}
	for _, path := range paths {
		if hooks.HookSettingsPath(path, cwd) {
			return "Changing hook settings requires per-call approval: " + path
		}
	}
	return ""
}

func mentionsHookSettings(text string) bool {
	return strings.Contains(text, ".harness") && strings.Contains(text, "settings") ||
		strings.Contains(text, ".config") && strings.Contains(text, "harness") && strings.Contains(text, "settings")
}

// ApproveHookSettings gates computed mutation targets, such as a child's patch.
func ApproveHookSettings(ctx context.Context, policy *Policy, tool, callID, cwd string, paths []string) Outcome {
	for _, path := range paths {
		if hooks.HookSettingsPath(path, cwd) {
			if policy == nil {
				policy = &Policy{}
			}
			return resolvePrompt(ctx, policy, PromptRequest{Tool: tool, ToolCallID: callID, Cwd: cwd, Tier: TierWrite, Reason: "Changing hook settings requires per-call approval: " + path})
		}
	}
	return Outcome{Allow: true}
}

func shellChangesHookSettings(command, cwd string) bool {
	words := shellWords(command)
	var invocation []cloudcmd.Token
	for i := 0; i < len(words); i++ {
		token := words[i]
		if token.Operator && strings.ContainsAny(token.Text, "<>") {
			if i+1 < len(words) {
				i++
				if strings.Contains(token.Text, ">") && hooks.HookSettingsPath(words[i].Text, cwd) {
					return true
				}
			}
			continue
		}
		if token.Operator {
			if invocationChangesHookSettings(invocation, cwd) {
				return true
			}
			cwd = directoryAfter(invocation, cwd)
			invocation = nil
			continue
		}
		invocation = append(invocation, token)
	}
	return invocationChangesHookSettings(invocation, cwd)
}

func invocationChangesHookSettings(words []cloudcmd.Token, cwd string) bool {
	if len(words) == 0 {
		return false
	}
	name, args := filepath.Base(words[0].Text), words[1:]
	switch name {
	case "env", "command", "exec", "xargs":
		for i := range args {
			if invocationChangesHookSettings(args[i:], cwd) {
				return true
			}
		}
		return false
	case "cp", "install":
		var paths []cloudcmd.Token
		destination := ""
		for i := 0; i < len(args); i++ {
			arg := args[i]
			if (arg.Text == "-t" || arg.Text == "--target-directory") && i+1 < len(args) {
				i++
				destination = args[i].Text
			} else if strings.HasPrefix(arg.Text, "--target-directory=") {
				destination = strings.TrimPrefix(arg.Text, "--target-directory=")
			} else if arg.Text != "" && !strings.HasPrefix(arg.Text, "-") {
				paths = append(paths, arg)
			}
		}
		if destination == "" {
			if len(paths) < 2 {
				return false
			}
			destination = paths[len(paths)-1].Text
			paths = paths[:len(paths)-1]
		}
		if info, err := os.Stat(resolveTarget(destination, cwd)); err == nil && info.IsDir() {
			for _, source := range paths {
				target := filepath.Join(destination, filepath.Base(source.Text))
				if source.Dynamic {
					target = destination
				}
				if hooks.HookSettingsPath(target, cwd) {
					return true
				}
			}
			return false
		}
		return hooks.HookSettingsPath(destination, cwd)
	case "rm", "rmdir", "unlink", "shred", "truncate", "mv", "tee", "touch", "dd":
	case "sed", "perl", "find":
		if !destructiveInvocation(words, cwd, map[string]bool{}) {
			return false
		}
	default:
		return false
	}
	for _, arg := range args {
		path := strings.TrimPrefix(arg.Text, "of=")
		if path != "" && !strings.HasPrefix(path, "-") && hooks.HookSettingsPath(path, cwd) {
			return true
		}
	}
	return false
}
