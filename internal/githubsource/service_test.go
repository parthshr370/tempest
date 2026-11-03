package githubsource

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/githubsource/ghfake"
	"go.harness.dev/harness/internal/githubsource/model"
	"go.harness.dev/harness/tempest/durable"
)

// TestMain serves both this package's tests and the external e2e tests,
// which share the binary.
func TestMain(m *testing.M) {
	ghfake.MainIfFake()
	os.Exit(m.Run())
}

var (
	startupStart = time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	startupBot   = ghfake.User{Login: "tempest-bot", ID: 100, Type: ghfake.TypeUser}
)

type startup struct {
	options Options
	server  *ghfake.Server
}

// newStartup is a valid configuration over a fake GitHub and a clone whose
// origin names the repository.
func newStartup(t *testing.T, serve func(*ghfake.Server)) *startup {
	t.Helper()
	dir := t.TempDir()
	clone := filepath.Join(dir, "clone")
	for _, args := range [][]string{{"init", "--quiet", clone}, {"-C", clone, "remote", "add", "origin", "https://github.com/acme/widgets.git"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	server := ghfake.NewServer(t)
	if serve == nil {
		ghfake.NewRepo(server, "acme/widgets", startupBot, startupStart)
	} else {
		serve(server)
	}
	config := validConfig()
	config.Account = model.AccountConfig{Login: startupBot.Login, ID: startupBot.ID, GHPath: ghfake.Executable(), GHConfigDir: filepath.Join(dir, "gh")}
	config.Clone, config.StateDir = clone, filepath.Join(dir, "state")
	return &startup{server: server, options: Options{Config: config, Environ: []string{"PATH=" + os.Getenv("PATH")}, GHEnv: server.Env()}}
}

func (s *startup) open(t *testing.T) (*Service, error) {
	t.Helper()
	service, err := Open(context.Background(), s.options)
	if err == nil {
		t.Cleanup(func() { _ = service.Close(context.Background()) })
	}
	return service, err
}

func TestGitHubStartupOpensWithValidSetup(t *testing.T) {
	s := newStartup(t, nil)
	service, err := s.open(t)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	status, err := service.Control().Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if service.Repo() != "acme/widgets" || status.Repo != "acme/widgets" {
		t.Fatalf("repo = %q, status = %+v", service.Repo(), status)
	}
}

func TestGitHubStartupRefusesTokenVariables(t *testing.T) {
	for _, name := range tokenVariables {
		t.Run(name, func(t *testing.T) {
			s := newStartup(t, nil)
			s.options.Environ = append(s.options.Environ, name+"=secret")
			_, err := s.open(t)
			if err == nil || !strings.Contains(err.Error(), name+" is set") {
				t.Fatalf("open with %s: %v", name, err)
			}
			if len(s.server.Requests()) != 0 {
				t.Fatalf("GitHub was called before the environment check: %d requests", len(s.server.Requests()))
			}
		})
	}
}

func TestGitHubStartupRefusesAccountMismatch(t *testing.T) {
	s := newStartup(t, nil)
	s.options.Config.Account.ID = 101
	if _, err := s.open(t); err == nil || !strings.Contains(err.Error(), "authenticates as tempest-bot (id 100)") {
		t.Fatalf("id mismatch: %v", err)
	}
	s = newStartup(t, nil)
	s.options.Config.Account.Login = "someone-else"
	if _, err := s.open(t); err == nil || !strings.Contains(err.Error(), "not account someone-else") {
		t.Fatalf("login mismatch: %v", err)
	}
}

func TestGitHubStartupRefusesWithoutPushPermission(t *testing.T) {
	s := newStartup(t, func(server *ghfake.Server) {
		server.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
			ghfake.WriteJSON(w, http.StatusOK, map[string]any{"login": startupBot.Login, "id": startupBot.ID, "type": "User"})
		})
		server.HandleFunc("GET /repos/acme/widgets", func(w http.ResponseWriter, r *http.Request) {
			ghfake.WriteJSON(w, http.StatusOK, map[string]any{"id": 7, "full_name": "acme/widgets", "default_branch": "main", "permissions": map[string]bool{"pull": true, "push": false}})
		})
	})
	if _, err := s.open(t); err == nil || !strings.Contains(err.Error(), "no push permission") {
		t.Fatalf("open: %v", err)
	}
}

func TestGitHubStartupRefusesMissingRepository(t *testing.T) {
	s := newStartup(t, func(server *ghfake.Server) {
		server.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
			ghfake.WriteJSON(w, http.StatusOK, map[string]any{"login": startupBot.Login, "id": startupBot.ID, "type": "User"})
		})
	})
	if _, err := s.open(t); err == nil || !strings.Contains(err.Error(), "read repository acme/widgets") {
		t.Fatalf("open: %v", err)
	}
}

func TestGitHubStartupRefusesWrongCloneRemote(t *testing.T) {
	s := newStartup(t, nil)
	if out, err := exec.Command("git", "-C", s.options.Config.Clone, "remote", "set-url", "origin", "https://github.com/acme/gadgets.git").CombinedOutput(); err != nil {
		t.Fatalf("set-url: %v\n%s", err, out)
	}
	if _, err := s.open(t); err == nil || !strings.Contains(err.Error(), "not github.com/acme/widgets") {
		t.Fatalf("wrong remote: %v", err)
	}
	s.options.Config.Workers.Remote = "upstream"
	if _, err := s.open(t); err == nil || !strings.Contains(err.Error(), `no remote "upstream"`) {
		t.Fatalf("missing remote: %v", err)
	}
}

func TestGitHubStartupRefusesLockedControlStore(t *testing.T) {
	s := newStartup(t, nil)
	if _, err := s.open(t); err != nil {
		t.Fatalf("first open: %v", err)
	}
	_, err := s.open(t)
	if !errors.Is(err, durable.ErrLocked) || !strings.Contains(err.Error(), "locked by another serve") {
		t.Fatalf("second open: %v", err)
	}
}

func TestGitHubStartupRefusesJournalOfAnotherAccount(t *testing.T) {
	s := newStartup(t, nil)
	service, err := s.open(t)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	other := newStartup(t, func(server *ghfake.Server) {
		ghfake.NewRepo(server, "acme/widgets", ghfake.User{Login: "other-bot", ID: 200, Type: ghfake.TypeUser}, startupStart)
	})
	other.options.Config.Account.Login, other.options.Config.Account.ID = "other-bot", 200
	other.options.Config.Clone, other.options.Config.StateDir = s.options.Config.Clone, s.options.Config.StateDir
	if _, err := other.open(t); err == nil || !strings.Contains(err.Error(), "journal belongs to") {
		t.Fatalf("open with another account: %v", err)
	}
}

// The state directory holds the control journal and the push repository:
// startup creates it private and refuses one other users can write.
func TestGitHubStartupStateDirectory(t *testing.T) {
	t.Run("created private", func(t *testing.T) {
		s := newStartup(t, nil)
		if _, err := s.open(t); err != nil {
			t.Fatalf("open: %v", err)
		}
		info, err := os.Stat(s.options.Config.StateDir)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o700 {
			t.Fatalf("state directory mode = %o, want 700", mode)
		}
	})
	for _, mode := range []os.FileMode{0o770, 0o757, 0o777} {
		t.Run("refuses "+mode.String(), func(t *testing.T) {
			s := newStartup(t, nil)
			if err := os.Mkdir(s.options.Config.StateDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(s.options.Config.StateDir, mode); err != nil {
				t.Fatal(err)
			}
			if _, err := s.open(t); err == nil || !strings.Contains(err.Error(), "writable by other users") {
				t.Fatalf("open with state directory mode %o: %v", mode, err)
			}
		})
	}
	t.Run("keeps an existing private directory", func(t *testing.T) {
		s := newStartup(t, nil)
		if err := os.Mkdir(s.options.Config.StateDir, 0o750); err != nil {
			t.Fatal(err)
		}
		if _, err := s.open(t); err != nil {
			t.Fatalf("open: %v", err)
		}
	})
}
