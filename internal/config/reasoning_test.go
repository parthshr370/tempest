package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReasoningPrecedenceAndRoleInheritance(t *testing.T) {
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{"reasoning":"medium","smol_reasoning":"off"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	resolve := func(flags FlagValues, env map[string]string) *ResolvedConfig {
		t.Helper()
		resolved, err := Config{Flags: flags, SettingsPath: settingsPath, LookupEnv: func(key string) string { return env[key] }}.Resolve()
		if err != nil {
			t.Fatal(err)
		}
		return resolved
	}

	settings := resolve(FlagValues{}, nil)
	if settings.Reasoning() != "medium" || settings.PlanReasoning() != "medium" || settings.SmolReasoning() != "off" {
		t.Fatalf("settings: %q %q %q", settings.Reasoning(), settings.PlanReasoning(), settings.SmolReasoning())
	}
	env := resolve(FlagValues{}, map[string]string{"HARNESS_REASONING": "LOW", "HARNESS_PLAN_REASONING": "high"})
	if env.Reasoning() != "low" || env.PlanReasoning() != "high" || env.SmolReasoning() != "off" {
		t.Fatalf("environment: %q %q %q", env.Reasoning(), env.PlanReasoning(), env.SmolReasoning())
	}
	flags := resolve(FlagValues{Reasoning: StringInput{Value: "minimal", Set: true}, SmolReasoning: StringInput{Value: "high", Set: true}}, map[string]string{"HARNESS_REASONING": "low"})
	if flags.Reasoning() != "minimal" || flags.PlanReasoning() != "minimal" || flags.SmolReasoning() != "high" {
		t.Fatalf("flags: %q %q %q", flags.Reasoning(), flags.PlanReasoning(), flags.SmolReasoning())
	}

	unset, err := Config{SettingsPath: filepath.Join(t.TempDir(), "missing.json"), LookupEnv: func(string) string { return "" }}.Resolve()
	if err != nil || unset.Reasoning() != "" || unset.PlanReasoning() != "" {
		t.Fatalf("unset: %q %v", unset.Reasoning(), err)
	}
}

func TestReasoningRejectsUnknownLevel(t *testing.T) {
	for _, flags := range []FlagValues{
		{Reasoning: StringInput{Value: "xhigh", Set: true}},
		{PlanReasoning: StringInput{Value: "max", Set: true}},
		{SmolReasoning: StringInput{Value: "lots", Set: true}},
	} {
		_, err := Config{Flags: flags, SettingsPath: filepath.Join(t.TempDir(), "missing.json"), LookupEnv: func(string) string { return "" }}.Resolve()
		var configErr *Error
		if !errors.As(err, &configErr) || (configErr.Field != "reasoning" && configErr.Field != "plan_reasoning" && configErr.Field != "smol_reasoning") {
			t.Errorf("%+v: %v", flags, err)
		}
	}
}
