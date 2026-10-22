package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"go.harness.dev/harness/internal/obs"
	"go.harness.dev/harness/internal/rpc"
	"go.harness.dev/harness/tempest"
)

func supportsForm(revision string, raw json.RawMessage) bool {
	var modes map[string]json.RawMessage
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &modes) != nil || modes == nil {
		return false
	}
	if revision == "2025-06-18" {
		return len(modes) == 0
	}
	if len(modes) == 0 {
		return true
	}
	form, ok := modes["form"]
	var object map[string]json.RawMessage
	return ok && json.Unmarshal(form, &object) == nil && object != nil
}

var approvalIDs atomic.Uint64

type routingPrompter struct {
	mu      sync.Mutex
	active  *call
	conn    *rpc.Conn
	timeout time.Duration
}

func (p *routingPrompter) set(c *call) { p.mu.Lock(); p.active = c; p.mu.Unlock() }
func (p *routingPrompter) clear(c *call) {
	p.mu.Lock()
	if p.active == c {
		p.active = nil
	}
	p.mu.Unlock()
}

func (p *routingPrompter) Approve(ctx context.Context, ask tempest.Ask) (tempest.Choice, error) {
	p.mu.Lock()
	c := p.active
	p.mu.Unlock()
	if c == nil || !c.form {
		return "reject_once", nil
	}
	approveCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	stop := context.AfterFunc(c.ctx, cancel)
	defer stop()
	if err := approveCtx.Err(); err != nil {
		return "", err
	}
	message := fmt.Sprintf("Tempest session %s requests allow-once for %s (call %s). Tools run in the host workspace. Deny if you don't trust this operation.", safeLabel(ask.SessionID), safeLabel(ask.Tool), safeLabel(ask.ToolCallID))
	params := map[string]any{"message": message, "requestedSchema": map[string]any{"type": "object", "properties": map[string]any{"allow": map[string]any{"type": "boolean", "title": "Allow this one tool call?", "default": false}}, "required": []string{"allow"}}}
	if c.revision != "2025-06-18" {
		params["mode"] = "form"
	}
	id := fmt.Sprintf("approval_%d", approvalIDs.Add(1))
	raw, err := p.conn.CallWithID(approveCtx, id, "elicitation/create", params)
	if err != nil {
		if approveCtx.Err() != nil {
			notifyCtx, stop := context.WithTimeout(context.Background(), 10*time.Millisecond)
			_ = p.conn.Notify(notifyCtx, "notifications/cancelled", map[string]any{"requestId": id, "reason": "permission request ended"})
			stop()
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if c.ctx.Err() != nil {
				return "", c.ctx.Err()
			}
			return "", context.DeadlineExceeded
		}
		return "reject_once", nil
	}
	var result struct {
		Action  string `json:"action"`
		Content struct {
			Allow *bool `json:"allow"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return "reject_once", nil
	}
	if result.Action == "accept" && result.Content.Allow != nil && *result.Content.Allow {
		return "allow_once", nil
	}
	return "reject_once", nil
}

func safeLabel(text string) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, text)
	text = obs.Redact(text)
	if len(text) > 1024 {
		n := 1024
		for n > 0 && !utf8.RuneStart(text[n]) {
			n--
		}
		text = text[:n] + "[truncated]"
	}
	return text
}
