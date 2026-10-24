package bedrock

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/conformance"
	"go.harness.dev/harness/internal/transport/sseparse"
)

func frame(name string, payload []byte) []byte {
	return messageFrame("event", ":event-type", name, payload)
}

func messageFrame(messageType, typeHeader, name string, payload []byte) []byte {
	var headers bytes.Buffer
	for _, pair := range [][2]string{{":message-type", messageType}, {typeHeader, name}, {":content-type", "application/json"}} {
		headers.WriteByte(byte(len(pair[0])))
		headers.WriteString(pair[0])
		headers.WriteByte(7)
		binary.Write(&headers, binary.BigEndian, uint16(len(pair[1])))
		headers.WriteString(pair[1])
	}
	raw := make([]byte, 16+headers.Len()+len(payload))
	binary.BigEndian.PutUint32(raw[:4], uint32(len(raw)))
	binary.BigEndian.PutUint32(raw[4:8], uint32(headers.Len()))
	binary.BigEndian.PutUint32(raw[8:12], crc32.ChecksumIEEE(raw[:8]))
	copy(raw[12:], headers.Bytes())
	copy(raw[12+headers.Len():], payload)
	binary.BigEndian.PutUint32(raw[len(raw)-4:], crc32.ChecksumIEEE(raw[:len(raw)-4]))
	return raw
}
func encodeReplay(replay string) string {
	var result bytes.Buffer
	err := sseparse.IterateSSE(context.Background(), strings.NewReader(replay), func(e sseparse.ServerSentEvent) error {
		name := e.Event
		if name == "" {
			name = "internalServerException"
		}
		result.Write(frame(name, []byte(e.Data)))
		return nil
	})
	if err != nil {
		panic(err)
	}
	return result.String()
}
func TestConformance(t *testing.T) {
	conformance.Run(t, conformance.Factory{Dialect: "bedrock", Stream: StreamSimple, Encode: encodeReplay})
}

func TestSigV4AWSVector(t *testing.T) {
	// AWS's IAM ListUsers SigV4 example: https://docs.aws.amazon.com/general/latest/gr/sigv4-calculate-signature.html
	request, err := http.NewRequest(http.MethodGet, "https://iam.amazonaws.com/?Action=ListUsers&Version=2010-05-08", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	sign(request, nil, credentials{AccessKey: "AKIDEXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}, "us-east-1", "iam", time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC))
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/iam/aws4_request, SignedHeaders=content-type;host;x-amz-date, Signature=5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7"
	if request.Header.Get("Authorization") != want {
		t.Fatalf("signature: %s", request.Header.Get("Authorization"))
	}
	key := mac(mac(mac(mac([]byte("AWS4wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"), "20150830"), "us-east-1"), "iam"), "aws4_request")
	if got := hex.EncodeToString(key); got != "c4afb1cc5771d871763a393e44b703571b55cc28424d1a5e86da6ed3c154a4b9" {
		t.Fatalf("signing key: %s", got)
	}
}
func TestEventStreamChecks(t *testing.T) {
	original := frame("messageStop", []byte(`{"stopReason":"end_turn"}`))
	headers, payload, err := readFrame(bytes.NewReader(original))
	if err != nil || headers[":event-type"] != "messageStop" || string(payload) != `{"stopReason":"end_turn"}` {
		t.Fatalf("frame: %v %s %v", headers, payload, err)
	}
	for _, index := range []int{8, 15, len(original) - 1} {
		bad := bytes.Clone(original)
		bad[index] ^= 1
		if _, _, err := readFrame(bytes.NewReader(bad)); err == nil {
			t.Errorf("accepted corrupt frame at %d", index)
		}
	}
	for _, size := range []int{1, 11, 15, len(original) - 1} {
		if _, _, err := readFrame(bytes.NewReader(original[:size])); err == nil {
			t.Errorf("accepted truncated frame at %d", size)
		}
	}
	invalid := bytes.Clone(original)
	binary.BigEndian.PutUint32(invalid[:4], 0xffffffff)
	if _, _, err := readFrame(bytes.NewReader(invalid)); err == nil {
		t.Fatal("accepted oversized frame")
	}
}
func TestSignedConverseWire(t *testing.T) {
	t.Setenv("AWS_REGION", "us-west-2")
	t.Setenv("AWS_ACCESS_KEY_ID", "access")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_SESSION_TOKEN", "session")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/model/anthropic.claude%3Atest/converse-stream" || r.URL.Query().Get("route") != "fixture" {
			t.Errorf("URL: %s", r.URL)
		}
		if !strings.Contains(r.Header.Get("Authorization"), "/us-west-2/bedrock/aws4_request") || r.Header.Get("X-Amz-Security-Token") != "session" {
			t.Errorf("auth: %v", r.Header)
		}
		var p map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Error(err)
		}
		if _, ok := p["model"]; ok {
			t.Error("model must be in URL, not body")
		}
		if !bytes.Contains(p["messages"], []byte(`"bytes":"aW1hZ2U="`)) || !bytes.Contains(p["toolConfig"], []byte(`"inputSchema":{"json":`)) || !bytes.Contains(p["additionalModelRequestFields"], []byte(`"budget_tokens":2048`)) {
			t.Errorf("wire: %s", p)
		}
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.Write(frame("contentBlockDelta", []byte(`{"contentBlockIndex":0,"delta":{"reasoningContent":{"text":"plan"}}}`)))
		w.Write(frame("contentBlockDelta", []byte(`{"contentBlockIndex":0,"delta":{"reasoningContent":{"signature":"signature"}}}`)))
		w.Write(frame("contentBlockStop", []byte(`{"contentBlockIndex":0}`)))
		w.Write(frame("messageStop", []byte(`{"stopReason":"end_turn"}`)))
		w.Write(frame("metadata", []byte(`{"usage":{"inputTokens":5,"outputTokens":4,"totalTokens":9}}`)))
	}))
	defer endpoint.Close()
	model := types.Model{ID: "anthropic.claude:test", Provider: "bedrock", API: "bedrock-converse-stream", BaseURL: endpoint.URL, Reasoning: true, MaxTokens: 8192, Input: []string{"text", "image"}}
	history := types.Context{Messages: []types.Message{types.UserMessage{Content: types.BlockContent(types.NewText("hi"), types.NewImage("aW1hZ2U=", "image/png"))}}, Tools: []types.Tool{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}}}
	opts := &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{Query: map[string]string{"route": "fixture"}}, Reasoning: "low"}
	result, err := StreamSimple(context.Background(), model, history, opts).Result(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != types.StopStop || len(result.Content) != 1 || result.Content[0].ThinkingSignature != "signature" {
		t.Fatalf("result: %+v", result)
	}
	history.Messages = append(history.Messages, *result)
	raw, err := json.Marshal(params(model, history, opts))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"reasoningText":{"signature":"signature","text":"plan"}`)) {
		t.Fatalf("reasoning replay: %s", raw)
	}
}
func TestProfileAndBearer(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_PROFILE", "work")
	path := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(path, []byte("[default]\naws_access_key_id = wrong\n[work]\naws_access_key_id = access\naws_secret_access_key = secret\naws_session_token = token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", path)
	creds, err := loadCredentials()
	if err != nil || creds.AccessKey != "access" || creds.SessionToken != "token" {
		t.Fatalf("profile: %+v %v", creds, err)
	}
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "bearer")
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer bearer" || r.URL.Path != "/model/test/converse" {
			t.Errorf("request: %s %v", r.URL, r.Header)
		}
		io.WriteString(w, `{"output":{"message":{"role":"assistant","content":[{"text":"hello"}]}},"stopReason":"end_turn","usage":{"inputTokens":2,"outputTokens":1,"totalTokens":3}}`)
	}))
	defer endpoint.Close()
	result, err := StreamSimple(context.Background(), types.Model{ID: "test", BaseURL: endpoint.URL}, types.Context{}, &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{Transport: "http"}}).Result(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != types.StopStop || result.Content[0].Text != "hello" {
		t.Fatalf("result: %+v", result)
	}
}

func TestRedactedReasoningFragments(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Write(frame("contentBlockDelta", []byte(`{"contentBlockIndex":0,"delta":{"reasoningContent":{"redactedContent":"YQ=="}}}`)))
		writer.Write(frame("contentBlockDelta", []byte(`{"contentBlockIndex":0,"delta":{"reasoningContent":{"redactedContent":"Yg=="}}}`)))
		writer.Write(frame("contentBlockStop", []byte(`{"contentBlockIndex":0}`)))
		writer.Write(frame("messageStop", []byte(`{"stopReason":"end_turn"}`)))
		writer.Write(frame("metadata", []byte(`{"usage":{"inputTokens":2,"outputTokens":1,"totalTokens":6,"cacheWriteInputTokens":3,"cacheDetails":[{"inputTokens":3,"ttl":"1h"}]}}`)))
	}))
	defer endpoint.Close()
	result, err := StreamSimple(context.Background(), types.Model{ID: "test", BaseURL: endpoint.URL}, types.Context{}, &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{NoAuth: true}}).Result(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 || !result.Content[0].Redacted || result.Content[0].ThinkingSignature != "YWI=" || result.Usage.CacheWrite1h == nil || *result.Usage.CacheWrite1h != 3 {
		t.Fatalf("redacted reasoning or cache details: %+v", result)
	}
}

func TestModeledStreamExceptions(t *testing.T) {
	for _, test := range []struct{ Event, Want string }{
		{"throttlingException", "rate_limited"},
		{"internalServerException", "server_failure"},
		{"validationException", "invalid_request"},
	} {
		t.Run(test.Event, func(t *testing.T) {
			endpoint := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Write(messageFrame("exception", ":exception-type", test.Event, []byte(`{"message":"request failed"}`)))
			}))
			defer endpoint.Close()
			result, err := StreamSimple(context.Background(), types.Model{ID: "test", BaseURL: endpoint.URL}, types.Context{}, &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{NoAuth: true}}).Result(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if result.ErrorCode != test.Want {
				t.Fatalf("error code: %s, want %s", result.ErrorCode, test.Want)
			}
		})
	}
}

func TestToolResultStatusSupport(t *testing.T) {
	for _, test := range []struct {
		Model  string
		Status bool
	}{
		{"anthropic.claude-3-haiku-20240307-v1:0", true},
		{"us.anthropic.claude-sonnet-4-5-20250929-v1:0", true},
		{"amazon.nova-pro-v1:0", true},
		{"meta.llama3-70b-instruct-v1:0", false},
		{"mistral.mistral-large-2402-v1:0", false},
		{"deepseek.r1-v1:0", false},
	} {
		t.Run(test.Model, func(t *testing.T) {
			history := types.Context{Messages: []types.Message{
				types.AssistantMessage{Content: []types.ContentBlock{types.NewToolCall("call_1", "lookup", json.RawMessage(`{}`))}},
				types.ToolResultMessage{ToolCallID: "call_1", IsError: true, Content: []types.ContentBlock{types.NewText("failed")}},
			}}
			raw, err := json.Marshal(params(types.Model{ID: test.Model}, history, &types.SimpleStreamOptions{}))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), `"status":"error"`) != test.Status {
				t.Fatalf("unsupported tool status: %s", raw)
			}
		})
	}
}

func TestConversePreservesBlockedStopReason(t *testing.T) {
	for _, reason := range []string{"guardrail_intervened", "content_filtered"} {
		t.Run(reason, func(t *testing.T) {
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"output":{"message":{"content":[{"text":"blocked"}]}},"stopReason":%q}`, reason)
			}))
			defer endpoint.Close()
			got, err := StreamSimple(context.Background(), types.Model{ID: "test", BaseURL: endpoint.URL}, types.Context{}, &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{NoAuth: true, Transport: "http"}}).Result(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got.StopReason != types.StopError || !strings.Contains(got.ErrorMessage, reason) {
				t.Fatalf("lost stop reason: %+v", got)
			}
		})
	}
}
