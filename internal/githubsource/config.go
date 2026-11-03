// Package githubsource runs the GitHub ticket runner inside serve: it loads
// the operator file, checks the account, repository and clone before any
// poll or dispatch, and owns the control journal, the poll loop, the
// dispatcher and the outbox of one repository.
package githubsource

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
	"go.harness.dev/harness/internal/permission"
)

// Defaults the loader fills in, so the validated file is the effective one.
const (
	defaultPollInterval = 60 * time.Second
	defaultWorkers      = 3
	defaultRemote       = "origin"
	maxWorkers          = 16
	minPollInterval     = 60 * time.Second
	maxPollInterval     = time.Hour
	maxConfigBytes      = 1 << 20
)

// LoadConfig reads the operator file at path strictly (unknown fields are
// errors), fills defaults and validates it for repository, the `-github`
// flag. sessionDir supplies the default state directory
// <sessionDir>/github/<owner>/<repo>.
func LoadConfig(path, repository, sessionDir string) (model.Config, error) {
	if !filepath.IsAbs(path) {
		return model.Config{}, fmt.Errorf("github config %q: path must be absolute", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return model.Config{}, fmt.Errorf("github config: %w", err)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	if err != nil {
		return model.Config{}, fmt.Errorf("github config %s: %w", path, err)
	}
	if len(raw) > maxConfigBytes {
		return model.Config{}, fmt.Errorf("github config %s: larger than %d bytes", path, maxConfigBytes)
	}
	config, err := ParseConfig(raw, repository, sessionDir)
	if err != nil {
		return model.Config{}, fmt.Errorf("github config %s: %w", path, err)
	}
	return config, nil
}

// ParseConfig decodes and validates one operator file; see LoadConfig.
func ParseConfig(raw []byte, repository, sessionDir string) (model.Config, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var config model.Config
	if err := decoder.Decode(&config); err != nil {
		return model.Config{}, fmt.Errorf("decode: %w", err)
	}
	// More is false before a closing delimiter, so only a second decode
	// reaching EOF proves nothing follows the object.
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return model.Config{}, errors.New("decode: trailing data after the JSON object")
	}
	if err := ValidateConfig(&config, repository, sessionDir); err != nil {
		return model.Config{}, err
	}
	return config, nil
}

// ValidateConfig fills defaults into config and checks every rule of the
// operator file. Each error names the offending field.
func ValidateConfig(config *model.Config, repository, sessionDir string) error {
	if config.Version != model.ConfigVersion {
		return fmt.Errorf("version: must be %d, not %d", model.ConfigVersion, config.Version)
	}
	owner, name, err := splitRepository(repository)
	if err != nil {
		return fmt.Errorf("-github: %w", err)
	}
	if !strings.EqualFold(config.Repository, repository) {
		return fmt.Errorf("repository: %q does not match -github %q", config.Repository, repository)
	}
	if err := validateAccount(config.Account); err != nil {
		return err
	}
	if err := absolute("clone", config.Clone); err != nil {
		return err
	}
	if config.StateDir == "" {
		if !filepath.IsAbs(sessionDir) {
			return errors.New("stateDir: not set and the session directory is not absolute")
		}
		config.StateDir = filepath.Join(sessionDir, "github", owner, name)
	} else if err := absolute("stateDir", config.StateDir); err != nil {
		return err
	}
	if err := validatePoll(&config.Poll); err != nil {
		return err
	}
	if err := validateAdmission(config.Admission, config.Account); err != nil {
		return err
	}
	if err := validateWorkers(&config.Workers); err != nil {
		return err
	}
	if err := validateBudgets(config.Budgets); err != nil {
		return err
	}
	if config.Limits.MaxEventBodyBytes < 0 {
		return errors.New("limits.maxEventBodyBytes: must not be negative")
	}
	if config.Limits.MaxWakeBytes < 0 {
		return errors.New("limits.maxWakeBytes: must not be negative")
	}
	if config.Merge != "manual" {
		return fmt.Errorf("merge: must be \"manual\", not %q: workers never merge", config.Merge)
	}
	return nil
}

func splitRepository(repository string) (owner, name string, err error) {
	owner, name, found := strings.Cut(repository, "/")
	if !found || !repositoryPart(owner) || !repositoryPart(name) {
		return "", "", fmt.Errorf("%q is not owner/repo", repository)
	}
	return owner, name, nil
}

// repositoryPart accepts GitHub owner and repository name characters.
func repositoryPart(part string) bool {
	if part == "" || part == "." || part == ".." || strings.HasPrefix(part, "-") {
		return false
	}
	for _, r := range part {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

func absolute(field, path string) error {
	if path == "" {
		return fmt.Errorf("%s: required", field)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%s: %q must be absolute", field, path)
	}
	return nil
}

func validateAccount(account model.AccountConfig) error {
	if account.Login == "" {
		return errors.New("account.login: required")
	}
	if account.ID <= 0 {
		return errors.New("account.id: must be positive")
	}
	if err := absolute("account.ghPath", account.GHPath); err != nil {
		return err
	}
	return absolute("account.ghConfigDir", account.GHConfigDir)
}

func validatePoll(poll *model.PollConfig) error {
	if poll.Interval == 0 {
		poll.Interval = model.Duration(defaultPollInterval)
	}
	if interval := time.Duration(poll.Interval); interval < minPollInterval || interval > maxPollInterval {
		return fmt.Errorf("poll.interval: %s is outside %s..%s", interval, minPollInterval, maxPollInterval)
	}
	for _, field := range []struct {
		name  string
		value model.Duration
	}{{"poll.lookback", poll.Lookback}, {"poll.retention", poll.Retention}, {"poll.callTimeout", poll.CallTimeout}} {
		if field.value < 0 {
			return fmt.Errorf("%s: must not be negative", field.name)
		}
	}
	if poll.MaxPages < 0 {
		return errors.New("poll.maxPages: must not be negative")
	}
	return nil
}

func validateAdmission(admission model.AdmissionConfig, account model.AccountConfig) error {
	switch admission.Mode {
	case model.ModeOff, model.ModeObserve:
	case "auto":
		return errors.New("admission.mode: \"auto\" is not available in this build")
	default:
		return fmt.Errorf("admission.mode: must be \"off\" or \"observe\", not %q", admission.Mode)
	}
	if len(admission.Actors) == 0 {
		return errors.New("admission.actors: at least one actor is required")
	}
	actors := make(map[int64]bool, len(admission.Actors))
	for i, actor := range admission.Actors {
		field := fmt.Sprintf("admission.actors[%d]", i)
		if err := validateActor(field, actor); err != nil {
			return err
		}
		if actor.ID == account.ID {
			return fmt.Errorf("%s: id %d is the account itself", field, actor.ID)
		}
		actors[actor.ID] = true
	}
	for i, bot := range admission.Bots {
		field := fmt.Sprintf("admission.bots[%d]", i)
		if err := validateActor(field, bot); err != nil {
			return err
		}
		if actors[bot.ID] {
			return fmt.Errorf("%s: id %d is also an actor; actors and bots must be disjoint", field, bot.ID)
		}
		if bot.ID == account.ID {
			return fmt.Errorf("%s: id %d is the account itself", field, bot.ID)
		}
	}
	return nil
}

func validateActor(field string, actor model.ActorID) error {
	if actor.ID <= 0 {
		return fmt.Errorf("%s.id: must be positive", field)
	}
	if actor.Login == "" {
		return fmt.Errorf("%s.login: required", field)
	}
	return nil
}

func validateWorkers(workers *model.WorkersConfig) error {
	if workers.Max == 0 {
		workers.Max = defaultWorkers
	}
	if workers.Max < 1 || workers.Max > maxWorkers {
		return fmt.Errorf("workers.max: %d is outside 1..%d", workers.Max, maxWorkers)
	}
	mode, err := permission.ParseMode(workers.PermissionMode)
	if err != nil {
		return fmt.Errorf("workers.permissionMode: %w", err)
	}
	if mode == permission.ModeBypass {
		return errors.New("workers.permissionMode: \"bypass\" is not allowed for GitHub workers")
	}
	workers.PermissionMode = string(mode)
	if len(workers.Prepare) > 0 && workers.Prepare[0] == "" {
		return errors.New("workers.prepare: the program (first element) is empty")
	}
	if workers.PrepareTimeout < 0 {
		return errors.New("workers.prepareTimeout: must not be negative")
	}
	if strings.HasPrefix(workers.BaseBranch, "-") {
		return fmt.Errorf("workers.baseBranch: %q must not start with '-'", workers.BaseBranch)
	}
	if workers.Remote == "" {
		workers.Remote = defaultRemote
	}
	if strings.HasPrefix(workers.Remote, "-") || strings.ContainsAny(workers.Remote, " \t\n/:") {
		return fmt.Errorf("workers.remote: %q is not a remote name", workers.Remote)
	}
	return nil
}

func validateBudgets(budgets model.BudgetsConfig) error {
	for _, amount := range []struct {
		name  string
		value float64
	}{{"budgets.run.maxCostUsd", budgets.Run.MaxCostUSD}, {"budgets.ticketUsd", budgets.TicketUSD}, {"budgets.repoUsd", budgets.RepoUSD}} {
		if math.IsNaN(amount.value) || math.IsInf(amount.value, 0) || amount.value <= 0 {
			return fmt.Errorf("%s: must be a finite amount above zero", amount.name)
		}
	}
	if budgets.Run.MaxTurns <= 0 {
		return errors.New("budgets.run.maxTurns: must be positive")
	}
	if budgets.Run.MaxWallTime <= 0 {
		return errors.New("budgets.run.maxWallTime: must be positive")
	}
	if budgets.RepoPeriod <= 0 {
		return errors.New("budgets.repoPeriod: must be positive")
	}
	if math.IsNaN(budgets.MinRunUSD) || math.IsInf(budgets.MinRunUSD, 0) || budgets.MinRunUSD < 0 {
		return errors.New("budgets.minRunUsd: must be a finite amount, zero for the default")
	}
	if budgets.MinRunUSD > budgets.Run.MaxCostUSD {
		return errors.New("budgets.minRunUsd: must not exceed budgets.run.maxCostUsd, or no run could ever start")
	}
	for _, count := range []struct {
		name  string
		value int64
	}{
		{"budgets.run.maxTotalTokens", int64(budgets.Run.MaxTotalTokens)},
		{"budgets.run.maxToolCalls", int64(budgets.Run.MaxToolCalls)},
		{"budgets.ticketTokens", budgets.TicketTokens},
		{"budgets.repoTokens", budgets.RepoTokens},
		{"budgets.maxAutoWakesPerDay", int64(budgets.MaxAutoWakesPerDay)},
	} {
		if count.value < 0 {
			return fmt.Errorf("%s: must not be negative", count.name)
		}
	}
	return nil
}
