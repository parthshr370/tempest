package githubsource

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
)

// validConfig is the design's example operator file with absolute paths.
func validConfig() model.Config {
	return model.Config{
		Version:    1,
		Repository: "acme/widgets",
		Account:    model.AccountConfig{Login: "acme-tempest", ID: 123456789, GHPath: "/usr/local/bin/gh", GHConfigDir: "/home/tempest/.config/gh-acme"},
		Clone:      "/home/tempest/clones/widgets",
		StateDir:   "/home/tempest/state/acme-widgets",
		Poll:       model.PollConfig{Interval: model.Duration(60 * time.Second), Lookback: model.Duration(time.Hour), Retention: model.Duration(336 * time.Hour), CallTimeout: model.Duration(30 * time.Second), MaxPages: 10},
		Admission:  model.AdmissionConfig{Mode: model.ModeObserve, Actors: []model.ActorID{{Login: "parth", ID: 1111}}, Bots: []model.ActorID{{Login: "coderabbitai[bot]", ID: 2222}}},
		Workers:    model.WorkersConfig{Max: 3, PermissionMode: "default", Prepare: []string{"make", "deps"}, PrepareTimeout: model.Duration(10 * time.Minute), Remote: "origin"},
		Budgets: model.BudgetsConfig{
			Run:       model.RunBudget{MaxTurns: 60, MaxCostUSD: 2, MaxWallTime: model.Duration(45 * time.Minute), MaxToolCalls: 300},
			TicketUSD: 10, RepoUSD: 100, RepoPeriod: model.Duration(720 * time.Hour), MinRunUSD: 0.2, MaxAutoWakesPerDay: 12,
		},
		Limits: model.LimitsConfig{MaxEventBodyBytes: 65536, MaxWakeBytes: 65536},
		Merge:  "manual",
	}
}

func encode(t *testing.T, config model.Config) []byte {
	t.Helper()
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestGitHubConfigValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*model.Config)
		// want is a substring of the error naming the field; empty is valid.
		want string
	}{
		{name: "valid", mutate: func(*model.Config) {}},
		{name: "version", mutate: func(c *model.Config) { c.Version = 2 }, want: "version:"},
		{name: "repository differs from flag", mutate: func(c *model.Config) { c.Repository = "acme/gadgets" }, want: "repository:"},
		{name: "repository case differs", mutate: func(c *model.Config) { c.Repository = "Acme/Widgets" }},
		{name: "account login", mutate: func(c *model.Config) { c.Account.Login = "" }, want: "account.login"},
		{name: "account id", mutate: func(c *model.Config) { c.Account.ID = 0 }, want: "account.id"},
		{name: "relative gh path", mutate: func(c *model.Config) { c.Account.GHPath = "gh" }, want: "account.ghPath"},
		{name: "missing gh config dir", mutate: func(c *model.Config) { c.Account.GHConfigDir = "" }, want: "account.ghConfigDir"},
		{name: "relative clone", mutate: func(c *model.Config) { c.Clone = "clones/widgets" }, want: "clone:"},
		{name: "relative state dir", mutate: func(c *model.Config) { c.StateDir = "state" }, want: "stateDir:"},
		{name: "auto mode", mutate: func(c *model.Config) { c.Admission.Mode = "auto" }, want: "not available in this build"},
		{name: "unknown mode", mutate: func(c *model.Config) { c.Admission.Mode = "all" }, want: "admission.mode"},
		{name: "off mode", mutate: func(c *model.Config) { c.Admission.Mode = model.ModeOff }},
		{name: "no actors", mutate: func(c *model.Config) { c.Admission.Actors = nil }, want: "admission.actors: at least one"},
		{name: "actor id zero", mutate: func(c *model.Config) { c.Admission.Actors[0].ID = 0 }, want: "admission.actors[0].id"},
		{name: "actor without login", mutate: func(c *model.Config) { c.Admission.Actors[0].Login = "" }, want: "admission.actors[0].login"},
		{name: "bot id negative", mutate: func(c *model.Config) { c.Admission.Bots[0].ID = -1 }, want: "admission.bots[0].id"},
		{name: "actor is bot", mutate: func(c *model.Config) { c.Admission.Bots[0].ID = 1111 }, want: "disjoint"},
		{name: "account is actor", mutate: func(c *model.Config) { c.Admission.Actors[0].ID = 123456789 }, want: "account itself"},
		{name: "workers zero takes default", mutate: func(c *model.Config) { c.Workers.Max = 0 }},
		{name: "workers above 16", mutate: func(c *model.Config) { c.Workers.Max = 17 }, want: "workers.max"},
		{name: "workers negative", mutate: func(c *model.Config) { c.Workers.Max = -1 }, want: "workers.max"},
		{name: "bypass", mutate: func(c *model.Config) { c.Workers.PermissionMode = "bypass" }, want: "workers.permissionMode"},
		{name: "unknown permission mode", mutate: func(c *model.Config) { c.Workers.PermissionMode = "yolo" }, want: "workers.permissionMode"},
		{name: "empty prepare program", mutate: func(c *model.Config) { c.Workers.Prepare = []string{""} }, want: "workers.prepare"},
		{name: "remote with slash", mutate: func(c *model.Config) { c.Workers.Remote = "a/b" }, want: "workers.remote"},
		{name: "base branch option", mutate: func(c *model.Config) { c.Workers.BaseBranch = "--upload-pack=x" }, want: "workers.baseBranch"},
		{name: "interval below 60s", mutate: func(c *model.Config) { c.Poll.Interval = model.Duration(59 * time.Second) }, want: "poll.interval"},
		{name: "interval above 1h", mutate: func(c *model.Config) { c.Poll.Interval = model.Duration(time.Hour + time.Second) }, want: "poll.interval"},
		{name: "negative lookback", mutate: func(c *model.Config) { c.Poll.Lookback = model.Duration(-time.Second) }, want: "poll.lookback"},
		{name: "negative pages", mutate: func(c *model.Config) { c.Poll.MaxPages = -1 }, want: "poll.maxPages"},
		{name: "ticket usd zero", mutate: func(c *model.Config) { c.Budgets.TicketUSD = 0 }, want: "budgets.ticketUsd"},
		{name: "repo usd negative", mutate: func(c *model.Config) { c.Budgets.RepoUSD = -1 }, want: "budgets.repoUsd"},
		{name: "run cost zero", mutate: func(c *model.Config) { c.Budgets.Run.MaxCostUSD = 0 }, want: "budgets.run.maxCostUsd"},
		{name: "run turns zero", mutate: func(c *model.Config) { c.Budgets.Run.MaxTurns = 0 }, want: "budgets.run.maxTurns"},
		{name: "run wall time zero", mutate: func(c *model.Config) { c.Budgets.Run.MaxWallTime = 0 }, want: "budgets.run.maxWallTime"},
		{name: "repo period zero", mutate: func(c *model.Config) { c.Budgets.RepoPeriod = 0 }, want: "budgets.repoPeriod"},
		{name: "min run above run cap", mutate: func(c *model.Config) { c.Budgets.MinRunUSD = 3 }, want: "budgets.minRunUsd"},
		{name: "negative wakes", mutate: func(c *model.Config) { c.Budgets.MaxAutoWakesPerDay = -1 }, want: "budgets.maxAutoWakesPerDay"},
		{name: "negative tokens", mutate: func(c *model.Config) { c.Budgets.TicketTokens = -5 }, want: "budgets.ticketTokens"},
		{name: "negative body limit", mutate: func(c *model.Config) { c.Limits.MaxEventBodyBytes = -1 }, want: "limits.maxEventBodyBytes"},
		{name: "merge auto", mutate: func(c *model.Config) { c.Merge = "auto" }, want: "merge:"},
		{name: "merge missing", mutate: func(c *model.Config) { c.Merge = "" }, want: "merge:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config := validConfig()
			tc.mutate(&config)
			_, err := ParseConfig(encode(t, config), "acme/widgets", "/home/tempest/sessions")
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("valid config rejected: %v", err)
			case tc.want != "" && err == nil:
				t.Fatalf("accepted; want an error naming %q", tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("error %q does not name %q", err, tc.want)
			}
		})
	}
}

func TestGitHubConfigNonFiniteBudgetRejected(t *testing.T) {
	config := validConfig()
	config.Budgets.TicketUSD = math.Inf(1)
	if err := ValidateConfig(&config, "acme/widgets", "/s"); err == nil || !strings.Contains(err.Error(), "budgets.ticketUsd") {
		t.Fatalf("infinite ticketUsd: %v", err)
	}
	config = validConfig()
	config.Budgets.Run.MaxCostUSD = math.NaN()
	if err := ValidateConfig(&config, "acme/widgets", "/s"); err == nil || !strings.Contains(err.Error(), "budgets.run.maxCostUsd") {
		t.Fatalf("NaN maxCostUsd: %v", err)
	}
}

func TestGitHubConfigDecodeIsStrict(t *testing.T) {
	valid := string(encode(t, validConfig()))
	cases := map[string]string{
		"unknown field":       strings.Replace(valid, `"merge":"manual"`, `"merge":"manual","autoMerge":true`, 1),
		"bare number":         strings.Replace(valid, `"interval":"1m0s"`, `"interval":60`, 1),
		"trailing data":       valid + `{}`,
		"trailing brace":      valid + `}`,
		"trailing bracket":    valid + `]`,
		"trailing brace text": valid + `} trailing`,
		"trailing garbage":    valid + ` x`,
		"not an object":       `[]`,
		"bad -github flag":    valid,
		"unknown nested key":  strings.Replace(valid, `"login":"parth"`, `"login":"parth","role":"admin"`, 1),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			repository := "acme/widgets"
			if name == "bad -github flag" {
				repository = "acme"
			}
			if _, err := ParseConfig([]byte(raw), repository, "/s"); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestGitHubConfigDefaults(t *testing.T) {
	config := validConfig()
	config.StateDir, config.Workers.Max, config.Workers.Remote, config.Workers.PermissionMode, config.Poll.Interval = "", 0, "", "", 0
	parsed, err := ParseConfig(encode(t, config), "acme/widgets", "/home/tempest/sessions")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.StateDir != "/home/tempest/sessions/github/acme/widgets" {
		t.Errorf("stateDir default = %q", parsed.StateDir)
	}
	if parsed.Workers.Max != 3 || parsed.Workers.Remote != "origin" || parsed.Workers.PermissionMode != "default" || time.Duration(parsed.Poll.Interval) != time.Minute {
		t.Errorf("defaults = max %d remote %q mode %q interval %s", parsed.Workers.Max, parsed.Workers.Remote, parsed.Workers.PermissionMode, time.Duration(parsed.Poll.Interval))
	}
}

func TestGitHubLoadConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "github.json")
	if err := os.WriteFile(path, encode(t, validConfig()), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path, "acme/widgets", dir); err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := LoadConfig("github.json", "acme/widgets", dir); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative path: %v", err)
	}
	if _, err := LoadConfig(filepath.Join(dir, "missing.json"), "acme/widgets", dir); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestGitHubRemoteRepository(t *testing.T) {
	cases := map[string]string{
		"https://github.com/acme/widgets.git":     "acme/widgets",
		"https://github.com/acme/widgets":         "acme/widgets",
		"git@github.com:acme/widgets.git":         "acme/widgets",
		"ssh://git@github.com/acme/widgets.git":   "acme/widgets",
		"https://gitlab.com/acme/widgets.git":     "",
		"/srv/git/widgets.git":                    "",
		"file:///srv/git/acme/widgets.git":        "",
		"https://github.com/acme/widgets/extra":   "",
		"https://github.com.evil.io/acme/widgets": "",
	}
	for raw, want := range cases {
		got, ok := remoteRepository(raw)
		if (want == "") == ok || got != want {
			t.Errorf("remoteRepository(%q) = %q, %v; want %q", raw, got, ok, want)
		}
	}
}
