package serve

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/rpc"
)

const serveAnswerSchema = `{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"]}`

func TestSessionPromptOutputSchema(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text(`{"answer":7}`)), faux.Respond(faux.Text("seven")), faux.Respond(faux.Text("still seven")))
	s := newServer(t, f, nil, permission.ModeBypass, nil)
	a := connect(t, s)
	id := newSession(t, a, t.TempDir())
	prompt := func(text string, schema string) map[string]any {
		params := map[string]any{"sessionId": id, "prompt": []any{map[string]string{"type": "text", "text": text}}}
		if schema != "" {
			params["_meta"] = map[string]any{"tempest": map[string]any{"outputSchema": json.RawMessage(schema)}}
		}
		return params
	}

	var reply struct {
		StopReason string `json:"stopReason"`
		Meta       struct {
			Tempest struct {
				Output json.RawMessage `json:"output"`
			} `json:"tempest"`
		} `json:"_meta"`
	}
	call(t, a, "session/prompt", prompt("answer", serveAnswerSchema), &reply)
	if reply.StopReason != "end_turn" || string(reply.Meta.Tempest.Output) != `{"answer":7}` {
		t.Fatalf("reply = %+v output=%s", reply, reply.Meta.Tempest.Output)
	}

	for _, test := range []struct {
		params map[string]any
		code   int
	}{
		{prompt("bad schema", `{"$ref":"https://example.com/schema.json"}`), -32602},
		{prompt("prose twice", serveAnswerSchema), -32022},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := a.conn.Call(ctx, "session/prompt", test.params, nil)
		cancel()
		var wire *rpc.Error
		if !errors.As(err, &wire) || wire.Code != test.code {
			t.Fatalf("%v: %v, want %d", test.params["prompt"], err, test.code)
		}
	}
}
