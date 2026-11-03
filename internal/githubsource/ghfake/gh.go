package ghfake

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
)

// Exit codes of the real gh that the client may observe.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// statusLineProto is what `gh api -i` prints before the status code.
const statusLineProto = "HTTP/2.0"

// Executable is the GHPath to configure: the running test binary.
func Executable() string { return os.Args[0] }

// MainIfFake turns the process into the fake gh when EnvURL is set and
// never returns in that case. Call it first in TestMain.
func MainIfFake() {
	base := os.Getenv(EnvURL)
	if base == "" {
		return
	}
	os.Exit(runFake(base, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// fakeCall is a parsed `gh api` command line.
type fakeCall struct {
	method  string
	path    string
	include bool
	stdin   bool
	headers http.Header
}

func parseAPIArgs(args []string) (fakeCall, error) {
	if len(args) == 0 || args[0] != "api" {
		return fakeCall{}, errors.New("fake gh supports only `gh api`")
	}
	call := fakeCall{method: http.MethodGet, headers: http.Header{}}
	rest := args[1:]
	for len(rest) > 0 {
		flag := rest[0]
		rest = rest[1:]
		flagValue := func() (string, error) {
			if len(rest) == 0 {
				return "", fmt.Errorf("flag %s needs a value", flag)
			}
			next := rest[0]
			rest = rest[1:]
			return next, nil
		}
		switch flag {
		case "-i", "--include":
			call.include = true
		case "-X", "--method":
			method, err := flagValue()
			if err != nil {
				return fakeCall{}, err
			}
			call.method = strings.ToUpper(method)
		case "-H", "--header":
			header, err := flagValue()
			if err != nil {
				return fakeCall{}, err
			}
			name, headerValue, found := strings.Cut(header, ":")
			if !found {
				return fakeCall{}, fmt.Errorf("header %q is not key:value", header)
			}
			call.headers.Add(strings.TrimSpace(name), strings.TrimSpace(headerValue))
		case "--input":
			source, err := flagValue()
			if err != nil {
				return fakeCall{}, err
			}
			if source != "-" {
				return fakeCall{}, fmt.Errorf("fake gh reads --input only from stdin, got %q", source)
			}
			call.stdin = true
		case "--hostname":
			if _, err := flagValue(); err != nil {
				return fakeCall{}, err
			}
		default:
			if strings.HasPrefix(flag, "-") {
				return fakeCall{}, fmt.Errorf("fake gh does not support flag %s", flag)
			}
			if call.path != "" {
				return fakeCall{}, fmt.Errorf("unexpected argument %q", flag)
			}
			call.path = strings.TrimPrefix(flag, "/")
		}
	}
	if call.path == "" {
		return fakeCall{}, errors.New("gh api needs an endpoint")
	}
	return call, nil
}

func runFake(base string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	call, err := parseAPIArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, "fake gh:", err)
		return exitUsage
	}
	var body io.Reader
	if call.stdin {
		body = stdin
	}
	request, err := http.NewRequest(call.method, strings.TrimSuffix(base, "/")+"/"+call.path, body)
	if err != nil {
		fmt.Fprintln(stderr, "fake gh:", err)
		return exitError
	}
	request.Header = call.headers
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		fmt.Fprintln(stderr, "gh:", err)
		return exitError
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		fmt.Fprintln(stderr, "gh:", err)
		return exitError
	}
	if call.include {
		writeHead(stdout, response)
	}
	if _, err := stdout.Write(payload); err != nil {
		fmt.Fprintln(stderr, "gh:", err)
		return exitError
	}
	if response.StatusCode >= http.StatusBadRequest {
		fmt.Fprintf(stderr, "gh: %s (HTTP %d)\n", replyMessage(payload, response.StatusCode), response.StatusCode)
		return exitError
	}
	return exitOK
}

// writeHead mirrors `gh api -i`: an LF-terminated status line, CRLF header
// lines in sorted order, then a CRLF blank line.
func writeHead(stdout io.Writer, response *http.Response) {
	fmt.Fprintf(stdout, "%s %d %s\n", statusLineProto, response.StatusCode, http.StatusText(response.StatusCode))
	names := make([]string, 0, len(response.Header))
	for name := range response.Header {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		fmt.Fprintf(stdout, "%s: %s\r\n", name, strings.Join(response.Header[name], ", "))
	}
	fmt.Fprint(stdout, "\r\n")
}

func replyMessage(payload []byte, status int) string {
	var reply struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(payload, &reply) == nil && reply.Message != "" {
		return reply.Message
	}
	return http.StatusText(status)
}
