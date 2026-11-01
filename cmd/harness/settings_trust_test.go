package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// settingsRun runs the built CLI in a repository with its own HOME and no
// inherited HARNESS_* or provider variables.
type settingsRun struct {
	t    *testing.T
	exe  string
	home string
	repo string
	env  []string
}

func newSettingsRun(t *testing.T, exe string) *settingsRun {
	t.Helper()
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo, ".harness"), 0o700); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".config", "harness"), 0o700); err != nil {
		t.Fatal(err)
	}
	return &settingsRun{t: t, exe: exe, home: home, repo: repo, env: []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}}
}

func (r *settingsRun) write(path, content string) {
	r.t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func (r *settingsRun) writeUser(content string) {
	r.write(filepath.Join(r.home, ".config", "harness", "settings.json"), content)
}

func (r *settingsRun) writeProject(content string) {
	r.write(filepath.Join(r.repo, ".harness", "settings.json"), content)
}

// oneShot runs a faux one-shot prompt with extra arguments.
func (r *settingsRun) oneShot(args ...string) (string, int) {
	r.t.Helper()
	script := filepath.Join(r.t.TempDir(), "faux.json")
	r.write(script, `[{"text":"done"}]`)
	return r.run(append([]string{"-p", "go", "-faux-script", script, "-session-dir", filepath.Join(r.t.TempDir(), "sessions")}, args...)...)
}

// run returns combined output and the exit code.
func (r *settingsRun) run(args ...string) (string, int) {
	r.t.Helper()
	cmd := exec.Command(r.exe, args...)
	cmd.Dir = r.repo
	cmd.Env = r.env
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		return string(out), exitErr.ExitCode()
	case err != nil:
		r.t.Fatalf("run %v: %v", args, err)
	}
	return string(out), 0
}

func (r *settingsRun) markerExists() bool {
	_, err := os.Stat(filepath.Join(r.repo, "marker"))
	return err == nil
}

// requestCapture records request bodies and answers every request with an
// error, so a run makes its first model request and stops.
type requestCapture struct {
	mu     sync.Mutex
	bodies []string
	server *httptest.Server
}

func newRequestCapture(t *testing.T) *requestCapture {
	capture := &requestCapture{}
	capture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		capture.mu.Lock()
		capture.bodies = append(capture.bodies, string(body))
		capture.mu.Unlock()
		http.Error(w, `{"error":{"message":"captured"}}`, http.StatusBadRequest)
	}))
	t.Cleanup(capture.server.Close)
	return capture
}

func (c *requestCapture) requests() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.bodies...)
}

func buildSettingsHarness(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "harness-test")
	if out, err := exec.Command("go", "build", "-o", exe, ".").CombinedOutput(); err != nil {
		t.Fatalf("build harness: %v\n%s", err, out)
	}
	return exe
}

const sessionStartMarker = `{"SessionStart":[{"hooks":[{"type":"command","command":"printf ran > marker"}]}]}`

// Repository settings reach a run only through the settings resolver and
// its trust rules: no key spelling, second hook reader, bad file or
// diagnostic gets around them.
func TestCLIRepositorySettingsTrust(t *testing.T) {
	exe := buildSettingsHarness(t)

	t.Run("case-variant hooks never run", func(t *testing.T) {
		r := newSettingsRun(t, exe)
		r.writeProject(`{"Hooks":` + sessionStartMarker + `}`)
		if out, code := r.oneShot(); code != 0 || r.markerExists() {
			t.Fatalf("exit=%d marker=%t\n%s", code, r.markerExists(), out)
		}
	})

	t.Run("a one-shot prints each config warning once", func(t *testing.T) {
		r := newSettingsRun(t, exe)
		r.writeProject(`{"modle":"anthropic:typo"}`)
		out, code := r.oneShot()
		if count := strings.Count(out, `unknown setting \"modle\"`); code != 0 || count != 1 {
			t.Fatalf("exit=%d, warning printed %d times:\n%s", code, count, out)
		}
	})

	t.Run("case-variant endpoint never receives the credential", func(t *testing.T) {
		operator, repository := newRequestCapture(t), newRequestCapture(t)
		r := newSettingsRun(t, exe)
		r.writeUser(fmt.Sprintf(`{"providers":{"openai":{"base_url":%q}}}`, operator.server.URL+"/v1"))
		r.writeProject(fmt.Sprintf(`{"OPENAI_BASE_URL":%q}`, repository.server.URL+"/v1"))
		r.env = append(r.env, "OPENAI_API_KEY=p1-dummy-credential")
		out, _ := r.run("-p", "go", "-model", "openai:verification-model", "-c", "retry_enabled=false", "-session-dir", filepath.Join(t.TempDir(), "sessions"))
		if len(repository.requests()) != 0 || len(operator.requests()) == 0 {
			t.Fatalf("repository endpoint got %d requests, operator endpoint %d\n%s", len(repository.requests()), len(operator.requests()), out)
		}
	})

	t.Run("project skill_paths outside the repository need trust", func(t *testing.T) {
		capture := newRequestCapture(t)
		skill := filepath.Join(t.TempDir(), "SKILL.md")
		const description = "P1_PRIVATE_DESCRIPTION_FIXTURE"
		r := newSettingsRun(t, exe)
		r.write(skill, "---\nname: private\ndescription: "+description+"\n---\nbody")
		r.writeUser(fmt.Sprintf(`{"providers":{"capture":{"protocol":"openai-chat","base_url":%q,"no_auth":true}}}`, capture.server.URL+"/v1"))
		r.writeProject(fmt.Sprintf(`{"skill_paths":[%q]}`, skill))
		args := []string{"-p", "go", "-model", "capture:m", "-c", "retry_enabled=false", "-session-dir", filepath.Join(t.TempDir(), "sessions")}
		out, _ := r.run(args...)
		requests := capture.requests()
		if len(requests) == 0 || strings.Contains(strings.Join(requests, "\n"), description) {
			t.Fatalf("untrusted run: %d requests, description sent=%t\n%s", len(requests), strings.Contains(strings.Join(requests, "\n"), description), out)
		}
		out, _ = r.run(append(args, "-trust-project-hooks")...)
		if requests := capture.requests(); !strings.Contains(strings.Join(requests[1:], "\n"), description) {
			t.Fatalf("trusted run did not load the skill path\n%s", out)
		}
	})

	t.Run("project hooks follow layers and show in config", func(t *testing.T) {
		r := newSettingsRun(t, exe)
		r.writeProject(`{"hooks":` + sessionStartMarker + `}`)
		if out, code := r.oneShot("-trust-project-hooks", "-c", "hooks=null"); code != 0 || r.markerExists() {
			t.Fatalf("-c hooks=null: exit=%d marker=%t\n%s", code, r.markerExists(), out)
		}
		if out, code := r.oneShot("-trust-project-hooks"); code != 0 || !r.markerExists() {
			t.Fatalf("trusted project hook: exit=%d marker=%t\n%s", code, r.markerExists(), out)
		}
		out, code := r.run("config", "show", "-trust-project-hooks", "-key", "hooks")
		if code != 0 || !strings.Contains(out, "hooks.SessionStart") || !strings.Contains(out, "(project "+filepath.Join(r.repo, ".harness", "settings.json")) {
			t.Fatalf("config show trusted hooks: exit=%d\n%s", code, out)
		}
		out, code = r.run("config", "show", "-key", "model")
		if code != 0 || !strings.Contains(out, "warning: project settings ignored") || !strings.Contains(out, "hooks (needs -trust-project-hooks") {
			t.Fatalf("config show untrusted hooks: exit=%d\n%s", code, out)
		}
	})

	t.Run("malformed untrusted project file is skipped", func(t *testing.T) {
		r := newSettingsRun(t, exe)
		r.writeProject(`{ broken json`)
		out, code := r.run("config", "show", "-key", "model")
		if code != 0 || !strings.Contains(out, filepath.Join(r.repo, ".harness", "settings.json")) {
			t.Fatalf("config show: exit=%d\n%s", code, out)
		}
		if out, code := r.oneShot(); code != 0 {
			t.Fatalf("run: exit=%d\n%s", code, out)
		}
		if out, code := r.run("config", "show", "-strict-config"); code == 0 {
			t.Fatalf("strict config show accepted a malformed project file\n%s", out)
		}
	})

	t.Run("malformed -c never echoes its value", func(t *testing.T) {
		r := newSettingsRun(t, exe)
		out, code := r.run("config", "show", "-c", "providers.relay.api_key sk-dummykey123456789")
		if code == 0 || strings.Contains(out, "dummykey") || !strings.Contains(out, "key.path=value") {
			t.Fatalf("exit=%d\n%s", code, out)
		}
	})
}
