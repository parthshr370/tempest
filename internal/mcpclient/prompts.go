package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode"

	"go.harness.dev/harness/internal/engine/types"
)

// Prompt is a server prompt, offered to the user as the slash command
// /<server>:<name>.
type Prompt struct {
	Server      string           `json:"server"`
	Name        string           `json:"name"`
	Title       string           `json:"title,omitempty"`
	Description string           `json:"description,omitempty"`
	Arguments   []PromptArgument `json:"arguments,omitempty"`
}

// PromptArgument is one named prompt argument.
type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// Command is the slash command without its leading slash.
func (p Prompt) Command() string { return p.Server + ":" + p.Name }

// Hint shows the arguments in declared order: <name> when required,
// [name] when optional.
func (p Prompt) Hint() string {
	words := make([]string, 0, len(p.Arguments))
	for _, argument := range p.Arguments {
		if argument.Required {
			words = append(words, "<"+argument.Name+">")
		} else {
			words = append(words, "["+argument.Name+"]")
		}
	}
	return strings.Join(words, " ")
}

// Summary is the title or description shown beside the command.
func (p Prompt) Summary() string {
	if p.Description != "" {
		return strings.Join(strings.Fields(p.Description), " ")
	}
	if p.Title != "" {
		return p.Title
	}
	return "MCP prompt " + p.Name + " from server " + p.Server
}

func decodePrompt(server string, raw json.RawMessage) (Prompt, error) {
	prompt := Prompt{Server: server}
	if json.Unmarshal(raw, &prompt) != nil || prompt.Name == "" || strings.IndexFunc(prompt.Name, unicode.IsSpace) >= 0 {
		return Prompt{}, errors.New("mcp_protocol: prompt needs a name without spaces")
	}
	prompt.Server = server
	seen := map[string]bool{}
	for _, argument := range prompt.Arguments {
		if argument.Name == "" || strings.ContainsAny(argument.Name, "= \t\n") || seen[argument.Name] {
			return Prompt{}, fmt.Errorf("mcp_protocol: prompt %s has an empty, duplicate or unusable argument name", prompt.Name)
		}
		seen[argument.Name] = true
	}
	return prompt, nil
}

// Prompts returns every available server's prompts, ordered by command.
func (m *Manager) Prompts() []Prompt {
	var prompts []Prompt
	for _, peer := range m.servers {
		peer.mu.Lock()
		for _, prompt := range peer.catalog.prompts {
			prompt.Arguments = slices.Clone(prompt.Arguments)
			prompts = append(prompts, prompt)
		}
		peer.mu.Unlock()
	}
	sort.Slice(prompts, func(i, j int) bool { return prompts[i].Command() < prompts[j].Command() })
	return prompts
}

// ExpandPrompt turns text that starts with /<server>:<prompt> into that
// prompt's messages from prompts/get. ok is false when text names no
// prompt of a configured server, so the caller sends it unchanged.
// Arguments follow the command as name=value words, or as words in the
// declared order with the last declared argument taking the rest of the
// line.
func (m *Manager) ExpandPrompt(ctx context.Context, text string) (blocks []types.ContentBlock, ok bool, err error) {
	body, isCommand := strings.CutPrefix(text, "/")
	if !isCommand {
		return nil, false, nil
	}
	command, rest := splitWord(body)
	serverName, promptName, found := strings.Cut(command, ":")
	if !found {
		return nil, false, nil
	}
	peer, err := m.server(serverName)
	if err != nil {
		return nil, false, nil
	}
	client, snapshot, err := peer.current(ctx)
	if err != nil {
		return nil, true, err
	}
	index := slices.IndexFunc(snapshot.prompts, func(prompt Prompt) bool { return prompt.Name == promptName })
	if index < 0 {
		return nil, false, nil
	}
	prompt := snapshot.prompts[index]
	arguments, err := promptArguments(prompt, rest)
	if err != nil {
		return nil, true, err
	}
	callCtx, cancel := context.WithTimeout(ctx, peer.callTimeout)
	defer cancel()
	raw, err := peer.call(callCtx, client, "prompts/get", struct {
		Name      string            `json:"name"`
		Arguments map[string]string `json:"arguments,omitempty"`
	}{prompt.Name, arguments})
	if err != nil {
		if retirable(err) {
			peer.retire(client, err)
		}
		return nil, true, fmt.Errorf("MCP server %s prompts/get %s: %w", serverName, prompt.Name, err)
	}
	blocks, err = promptBlocks(m.spillDir, prompt, raw)
	if err != nil {
		return nil, true, fmt.Errorf("MCP server %s prompt %s: %w", serverName, prompt.Name, err)
	}
	return blocks, true, nil
}

// splitWord cuts text at its first whitespace.
func splitWord(text string) (word, rest string) {
	end := strings.IndexFunc(text, unicode.IsSpace)
	if end < 0 {
		return text, ""
	}
	return text[:end], text[end:]
}

// promptArguments binds the words after a command to the prompt's
// declared arguments and checks the required ones.
func promptArguments(prompt Prompt, input string) (map[string]string, error) {
	input = strings.TrimSpace(input)
	values := map[string]string{}
	words := strings.Fields(input)
	allNamed := len(words) > 0
	for _, word := range words {
		name, _, isPair := strings.Cut(word, "=")
		allNamed = allNamed && isPair && slices.ContainsFunc(prompt.Arguments, func(argument PromptArgument) bool { return argument.Name == name })
	}
	usage := fmt.Sprintf("usage: /%s %s", prompt.Command(), prompt.Hint())
	switch {
	case allNamed:
		for _, word := range words {
			name, value, _ := strings.Cut(word, "=")
			values[name] = value
		}
	case input != "" && len(prompt.Arguments) == 0:
		return nil, fmt.Errorf("/%s takes no arguments", prompt.Command())
	default:
		remaining := input
		for position, argument := range prompt.Arguments {
			if remaining == "" {
				break
			}
			if position == len(prompt.Arguments)-1 {
				values[argument.Name] = remaining
				break
			}
			word, after := splitWord(remaining)
			values[argument.Name] = word
			remaining = strings.TrimSpace(after)
		}
	}
	var missing []string
	for _, argument := range prompt.Arguments {
		if argument.Required && values[argument.Name] == "" {
			missing = append(missing, argument.Name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("/%s needs %s; %s", prompt.Command(), strings.Join(missing, ", "), usage)
	}
	return values, nil
}

// promptBlocks maps prompts/get messages to user-turn content through the
// same content mapping as tool results. Each change of role is marked when
// the prompt also speaks as the assistant, since all of it becomes one user
// turn.
func promptBlocks(spillDir string, prompt Prompt, raw json.RawMessage) ([]types.ContentBlock, error) {
	var result struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(raw, &result) != nil || len(result.Messages) == 0 {
		return nil, errors.New("mcp_protocol: prompts/get returned no messages")
	}
	mixedRoles := false
	for _, message := range result.Messages {
		if message.Role != "user" && message.Role != "assistant" {
			return nil, fmt.Errorf("mcp_protocol: prompt message role %q", message.Role)
		}
		mixedRoles = mixedRoles || message.Role == "assistant"
	}
	content := make([]json.RawMessage, 0, len(result.Messages))
	previousRole := ""
	for _, message := range result.Messages {
		if mixedRoles && message.Role != previousRole {
			marker, err := json.Marshal(struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}{"text", "[" + message.Role + "]"})
			if err != nil {
				return nil, err
			}
			content = append(content, marker)
			previousRole = message.Role
		}
		content = append(content, message.Content)
	}
	wrapped, err := json.Marshal(struct {
		Content []json.RawMessage `json:"content"`
	}{content})
	if err != nil {
		return nil, err
	}
	mapped, err := mapResult(spillDir, prompt.Server, "prompt:"+prompt.Name, wrapped, nil)
	if err != nil {
		return nil, err
	}
	return mapped.Content, nil
}

func samePrompts(left, right []Prompt) bool {
	return slices.EqualFunc(left, right, func(a, b Prompt) bool {
		return a.Name == b.Name && a.Title == b.Title && a.Description == b.Description && slices.Equal(a.Arguments, b.Arguments)
	})
}
