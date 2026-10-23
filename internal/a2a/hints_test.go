package a2a

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLegacyMethodAndVersionErrorsNameTheFix(t *testing.T) {
	server := &Server{backend: &durableBackend{}}
	call := func(method, version string) rpcError {
		t.Helper()
		r := httptest.NewRequest("POST", "/a2a", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"`+method+`","params":{}}`))
		r.Header.Set("Content-Type", "application/json")
		if version != "" {
			r.Header.Set("A2A-Version", version)
		}
		w := httptest.NewRecorder()
		server.handle(w, r)
		var reply struct{ Error *rpcError }
		if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil || reply.Error == nil {
			t.Fatalf("%s: %s", method, w.Body.String())
		}
		return *reply.Error
	}
	for legacy, current := range map[string]string{"message/send": "SendMessage", "message/stream": "SendStreamingMessage", "tasks/get": "GetTask", "tasks/cancel": "CancelTask"} {
		// 0.3 clients usually omit the version header; the method hint still wins.
		for _, version := range []string{"", "1.0"} {
			if e := call(legacy, version); e.Code != -32601 || !strings.Contains(e.Message, current) || !strings.Contains(e.Message, "A2A-Version: 1.0") {
				t.Fatalf("%s (version %q) = %+v", legacy, version, e)
			}
		}
	}
	if e := call("NoSuchMethod", "1.0"); e.Code != -32601 || e.Message != "Method not found" {
		t.Fatalf("unknown method = %+v", e)
	}
	for _, version := range []string{"", "0.3"} {
		if e := call("ListTasks", version); e.Code != -32009 || !strings.HasPrefix(e.Message, "VersionNotSupportedError") || !strings.Contains(e.Message, "send header A2A-Version: 1.0") {
			t.Fatalf("version %q = %+v", version, e)
		}
	}
}
