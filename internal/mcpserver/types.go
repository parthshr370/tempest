// Package mcpserver exposes durable Tempest runs as one stdio MCP tool.
package mcpserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.harness.dev/harness/internal/adapter"
	"go.harness.dev/harness/internal/obs"
	"go.harness.dev/harness/tempest"
)

// Options supplies host policy and the sessions owned by one MCP connection.
type Options struct {
	SessionDir, Cwd                                 string
	DefaultNew                                      tempest.NewSession
	Provider                                        tempest.ProviderConfig
	Stack                                           *tempest.StackConfig
	PermissionTimeout, IdleTimeout, ShutdownTimeout time.Duration
	Logger                                          *slog.Logger
}

// RunInput contains caller-controlled conversation settings, never host authority.
type RunInput struct {
	Prompt    string     `json:"prompt"`
	Cwd       string     `json:"cwd,omitempty"`
	SessionID string     `json:"sessionId,omitempty"`
	Model     string     `json:"model,omitempty"`
	Budget    *RunBudget `json:"budget,omitempty"`
}

// RunBudget may tighten, but can't remove, the host's limits.
type RunBudget struct {
	MaxTurns       *int     `json:"maxTurns,omitempty"`
	MaxCostUSD     *float64 `json:"maxCostUsd,omitempty"`
	MaxWallSeconds *float64 `json:"maxWallSeconds,omitempty"`
}

// RunOutput reports one admitted run, including its durable identity.
type RunOutput struct {
	Text       string             `json:"text"`
	StopReason tempest.StopReason `json:"stopReason"`
	Usage      tempest.Usage      `json:"usage"`
	SessionID  string             `json:"sessionId"`
	RunID      string             `json:"runId"`
	Turns      int                `json:"turns"`
	CostUSD    *float64           `json:"costUsd"`
	ElapsedMS  int64              `json:"elapsedMs"`
	Error      string             `json:"error,omitempty"`
	Truncated  bool               `json:"truncated,omitempty"`
	OutputPath string             `json:"outputPath,omitempty"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
type toolResult struct {
	Content           []textContent `json:"content"`
	StructuredContent *RunOutput    `json:"structuredContent,omitempty"`
	IsError           bool          `json:"isError"`
}

var toolDefinition = json.RawMessage(`{"name":"tempest_run","description":"Run a bounded Tempest coding-agent session in the server host workspace. Reuse sessionId for conversation continuity. Tools remain subject to host permissions.","inputSchema":{"type":"object","properties":{"prompt":{"type":"string","minLength":1},"cwd":{"type":"string"},"sessionId":{"type":"string"},"model":{"type":"string"},"budget":{"type":"object","properties":{"maxTurns":{"type":"integer","minimum":0},"maxCostUsd":{"type":"number","minimum":0},"maxWallSeconds":{"type":"number","minimum":0}},"additionalProperties":false}},"required":["prompt"],"additionalProperties":false}}`)

var inputValidator = sync.OnceValues(func() (*jsonschema.Schema, error) {
	var tool struct {
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	if err := json.Unmarshal(toolDefinition, &tool); err != nil {
		return nil, err
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(tool.InputSchema))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("mem://tempest-run", document); err != nil {
		return nil, err
	}
	return compiler.Compile("mem://tempest-run")
})

func decodeRunInput(raw json.RawMessage, in *RunInput) error {
	schema, err := inputValidator()
	if err != nil {
		return err
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	if err := schema.Validate(document); err != nil {
		return fmt.Errorf("invalid tempest_run arguments: %w", err)
	}
	return decodeStrict(raw, in)
}

func decodeStrict(raw json.RawMessage, dst any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return fmt.Errorf("expected an object")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	return nil
}

func (in RunInput) settings(host tempest.NewSession) (tempest.NewSession, error) {
	if strings.TrimSpace(in.Prompt) == "" {
		return host, fmt.Errorf("prompt must not be blank")
	}
	if in.Cwd != "" {
		info, err := os.Stat(in.Cwd)
		if !filepath.IsAbs(in.Cwd) || err != nil || !info.IsDir() {
			return host, fmt.Errorf("cwd must be an absolute existing directory")
		}
	}
	if in.SessionID != "" && (strings.ContainsAny(in.SessionID, `/\\`) || in.SessionID == "." || in.SessionID == "..") {
		return host, fmt.Errorf("sessionId must be an id, not a path")
	}
	if in.Model != "" {
		if strings.Contains(in.Model, "@") {
			return host, fmt.Errorf("model must not override the host provider URL")
		}
		if err := adapter.ValidateModelSpec(in.Model); err != nil {
			return host, err
		}
		host.Model = in.Model
	}
	if b := in.Budget; b != nil {
		if b.MaxTurns != nil {
			if *b.MaxTurns < 0 || (host.Budget.MaxTurns > 0 && (*b.MaxTurns == 0 || *b.MaxTurns > host.Budget.MaxTurns)) {
				return host, fmt.Errorf("maxTurns must not relax the host cap")
			}
			host.Budget.MaxTurns = *b.MaxTurns
		}
		if b.MaxCostUSD != nil {
			v := *b.MaxCostUSD
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || (host.Budget.MaxCostUSD > 0 && (v == 0 || v > host.Budget.MaxCostUSD)) {
				return host, fmt.Errorf("maxCostUsd must not relax the host cap")
			}
			host.Budget.MaxCostUSD = v
		}
		if b.MaxWallSeconds != nil {
			v := *b.MaxWallSeconds
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v >= float64(math.MaxInt64)/float64(time.Second) {
				return host, fmt.Errorf("invalid maxWallSeconds")
			}
			d := time.Duration(v * float64(time.Second))
			if v > 0 && d == 0 {
				return host, fmt.Errorf("maxWallSeconds is below one nanosecond")
			}
			if host.Budget.MaxWallTime > 0 && (d == 0 || d > host.Budget.MaxWallTime) {
				return host, fmt.Errorf("maxWallSeconds must not relax the host cap")
			}
			host.Budget.MaxWallTime = d
		}
	}
	return host, nil
}

func failure(err error) toolResult {
	return toolResult{Content: []textContent{{Type: "text", Text: obs.RedactTruncate(err.Error(), 8192)}}, IsError: true}
}

func makeResult(dir string, sess *tempest.Session, run *tempest.Run, result tempest.RunResult) toolResult {
	out := RunOutput{Text: run.Text(), StopReason: result.StopReason, Usage: result.Usage, SessionID: sess.ID(), RunID: result.RunID, Turns: result.Turns, ElapsedMS: result.ElapsedMS}
	if result.Err != nil {
		out.Error = obs.RedactTruncate(result.Err.Error(), 8192)
	}
	if !result.Usage.Cost.Unknown {
		cost := result.Usage.Cost.Total
		out.CostUSD = &cost
	}
	spillFull := func() {
		if out.Truncated {
			return
		}
		out.Truncated = true
		err := os.MkdirAll(dir, 0700)
		var spill string
		if err == nil {
			spill, err = os.MkdirTemp(dir, "mcp-result-")
		}
		if err == nil {
			out.OutputPath = filepath.Join(spill, "answer.txt")
			err = os.WriteFile(out.OutputPath, []byte(out.Text), 0600)
		}
		if err != nil {
			if spill != "" {
				_ = os.RemoveAll(spill)
			}
			out.OutputPath = ""
			out.Error = obs.RedactTruncate(fmt.Sprintf("spill run text: %v", err), 8192)
			out.Text = "[assistant output omitted: writing the full text failed]"
		}
	}
	if len(out.Text) > 128<<10 {
		spillFull()
		if out.OutputPath != "" {
			out.Text = truncateText(out.Text, 128<<10)
		}
	}
	build := func() toolResult {
		encoded, _ := json.Marshal(out)
		content := []textContent{}
		if out.Text != "" {
			content = append(content, textContent{Type: "text", Text: out.Text})
		}
		content = append(content, textContent{Type: "text", Text: string(encoded)})
		return toolResult{Content: content, StructuredContent: &out, IsError: string(result.StopReason) != "completed" || out.Error != ""}
	}
	response := build()
	// Text appears in both content and structured output, and JSON escaping counts.
	for encoded, _ := json.Marshal(response); len(encoded) > (1<<20)-4096; encoded, _ = json.Marshal(response) {
		spillFull()
		if len(out.Text) < 1024 {
			out.Error = "run result exceeds MCP response limit"
			out.Text = ""
			response = build()
			break
		}
		out.Text = truncateText(out.Text, len(out.Text)/2)
		response = build()
	}
	return response
}

func truncateText(text string, limit int) string {
	const marker = "\n[truncated; full text in outputPath]"
	n := max(0, limit-len(marker))
	if n >= len(text) {
		return text
	}
	for n > 0 && !utf8.RuneStart(text[n]) {
		n--
	}
	return text[:n] + marker
}
