package serve

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	model "github.com/a2aproject/a2a-go/v2/a2a"
)

func TestA2ALoopbackAddressAndCardPolicy(t *testing.T) {
	bound := make(chan string, 1)
	s, err := New(Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), OnListen: func(addr net.Addr) { bound <- addr.String() }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.ListenAndServe("127.0.0.1:0") }()
	var address string
	select {
	case address = <-bound:
	case <-time.After(5 * time.Second):
		t.Fatal("listener didn't bind")
	}
	if _, port, _ := net.SplitHostPort(address); port == "0" || port == "" {
		t.Fatalf("OnListen got unresolved address %q", address)
	}
	base := "http://" + address
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(base + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	var card model.AgentCard
	err = json.NewDecoder(response.Body).Decode(&card)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if card.SupportedInterfaces[0].URL != base+"/a2a" || len(card.SecurityRequirements) != 0 || len(card.SecuritySchemes) != 0 {
		t.Fatalf("bad loopback card: %+v", card)
	}
	request := a2aRequest("ListTasks", map[string]any{})
	request.URL.Host = address
	request.RequestURI = ""
	request.Header.Del("Authorization")
	request.Header.Set("A2A-Version", "1.0.123")
	request.Header.Set("Origin", base)
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var reply a2aReply
	_ = json.NewDecoder(response.Body).Decode(&reply)
	_ = response.Body.Close()
	if reply.Error != nil || response.StatusCode != 200 {
		t.Fatalf("patch/origin refused: %+v HTTP %d", reply, response.StatusCode)
	}
	request = httptest.NewRequest("GET", base+"/.well-known/agent-card.json", nil)
	request.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, request)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("listener didn't stop")
	}
}

func TestA2AAdvertisedAddressValidation(t *testing.T) {
	for _, base := range []string{"http://0.0.0.0", "http://[::]", "http://*", "relative", "http://user:secret@example.com", "https://example.com/", "https://example.com?secret=yes"} {
		s, err := New(Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), A2ABaseURL: base})
		if err == nil {
			_ = s.Close(context.Background())
			t.Fatalf("invalid base accepted: %s", base)
		}
	}
	s, err := New(Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), Token: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	if err := s.ListenAndServe("0.0.0.0:0"); err == nil {
		t.Fatal("nonloopback bind accepted without public base")
	}
}
