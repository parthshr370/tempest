package ghfake

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	MainIfFake()
	os.Exit(m.Run())
}

func runGH(t *testing.T, server *Server, stdin string, args ...string) (string, string, int) {
	t.Helper()
	command := exec.Command(Executable(), args...)
	command.Env = append(os.Environ(), server.Env()...)
	command.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return stdout.String(), stderr.String(), exitOK
	case errors.As(err, &exit):
		return stdout.String(), stderr.String(), exit.ExitCode()
	default:
		t.Fatalf("run fake gh: %v", err)
		return "", "", exitError
	}
}

func TestFakeGHForwardsRequestAndPrintsIncludeFormat(t *testing.T) {
	server := NewServer(t)
	server.HandleFunc("POST /repos/{owner}/{repo}/issues/{number}/comments", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		w.Header().Set("ETag", `"abc"`)
		WriteJSON(w, http.StatusCreated, map[string]string{"owner": r.PathValue("owner"), "echo": string(body)})
	})
	stdout, stderr, code := runGH(t, server, `{"body":"hi"}`, "api", "--method", "POST", "-i", "-H", "Accept: application/vnd.github+json", "--input", "-", "/repos/o/r/issues/7/comments")
	if code != exitOK {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	head, body, found := strings.Cut(stdout, "\r\n\r\n")
	if !found {
		t.Fatalf("no header terminator in %q", stdout)
	}
	if !strings.HasPrefix(head, "HTTP/2.0 201 Created\n") || !strings.Contains(head+"\r\n", "Etag: \"abc\"\r\n") {
		t.Fatalf("head = %q", head)
	}
	if !strings.Contains(body, `"owner":"o"`) || !strings.Contains(body, `{\"body\":\"hi\"}`) {
		t.Fatalf("body = %q", body)
	}
	requests := server.Requests()
	if len(requests) != 1 || requests[0].Header.Get("Accept") != "application/vnd.github+json" || requests[0].Method != http.MethodPost {
		t.Fatalf("requests = %+v", requests)
	}
}

func TestFakeGHErrorsMatchRealGH(t *testing.T) {
	server := NewServer(t)
	server.HandleFunc("GET /repos/o/r", func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]int{"id": 1})
	})
	server.Fail(http.MethodGet, "/repos/o/r", http.StatusBadGateway, 1, http.Header{"Retry-After": {"3"}})
	stdout, stderr, code := runGH(t, server, "", "api", "-i", "repos/o/r")
	if code != exitError || !strings.HasPrefix(stdout, "HTTP/2.0 502 Bad Gateway\n") || !strings.Contains(stdout, "Retry-After: 3\r\n") || stderr != "gh: Bad Gateway (HTTP 502)\n" {
		t.Fatalf("injected fault: code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if _, _, code = runGH(t, server, "", "api", "repos/o/r"); code != exitOK {
		t.Fatalf("fault consumed once, second call exit %d", code)
	}
	_, stderr, code = runGH(t, server, "", "api", "repos/o/missing")
	if code != exitError || stderr != "gh: Not Found (HTTP 404)\n" {
		t.Fatalf("unrouted path: code %d stderr %q", code, stderr)
	}
	if _, _, code = runGH(t, server, "", "pr", "list"); code != exitUsage {
		t.Fatalf("unsupported command exit %d", code)
	}
}

func TestWritePageLinksNextPage(t *testing.T) {
	server := NewServer(t)
	rows := []int{1, 2, 3, 4, 5}
	server.HandleFunc("GET /items", func(w http.ResponseWriter, r *http.Request) { WritePage(w, r, rows) })
	stdout, _, code := runGH(t, server, "", "api", "-i", "items?per_page=2&page=2")
	if code != exitOK || !strings.Contains(stdout, "Link: <https://api.github.com/items?page=3&per_page=2>; rel=\"next\"\r\n") || !strings.HasSuffix(stdout, "[3,4]\n") {
		t.Fatalf("page 2: %q", stdout)
	}
	stdout, _, _ = runGH(t, server, "", "api", "-i", "items?per_page=2&page=3")
	if strings.Contains(stdout, "Link:") || !strings.HasSuffix(stdout, "[5]\n") {
		t.Fatalf("last page: %q", stdout)
	}
}
