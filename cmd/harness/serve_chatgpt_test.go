package main

import (
	"path/filepath"
	"testing"

	"go.harness.dev/harness/internal/adapter"
	"go.harness.dev/harness/internal/config"
)

func TestServerChatGPTGrantRequiresOperatorModel(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, role := range []string{"none", "default", "plan", "smol"} {
		t.Run(role, func(t *testing.T) {
			models := map[string]string{"HARNESS_MODEL": "openai:fixture"}
			switch role {
			case "default":
				models["HARNESS_MODEL"] = "openai-chatgpt:fixture"
			case "plan":
				models["HARNESS_PLAN_MODEL"] = "openai-chatgpt:fixture"
			case "smol":
				models["HARNESS_SMOL_MODEL"] = "openai-chatgpt:fixture"
			}
			cfg, err := (config.Config{SettingsPath: filepath.Join(t.TempDir(), "missing.json"), LookupEnv: func(name string) string { return models[name] }}).Resolve()
			if err != nil {
				t.Fatal(err)
			}
			provider := serverProvider(cfg)
			// ACP metadata and the MCP model argument both arrive at this router.
			provider.ModelID = "openai-chatgpt:client-selected"
			provider.Env = func(string) string { return "" }
			_, err = adapter.NewProviderRouter(provider)
			if role == "none" {
				if provider.ChatGPTCredentials != nil || err == nil {
					t.Fatal("client model override enabled the owner's unconfigured ChatGPT grant")
				}
			} else if provider.ChatGPTCredentials == nil || err != nil {
				t.Fatalf("operator's explicit ChatGPT model did not enable credentials: %v", err)
			}
		})
	}
}
