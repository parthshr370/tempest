package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"go.harness.dev/harness/internal/cloudcmd"
	"go.harness.dev/harness/internal/lsp"
)

// maxOperatorFileBytes bounds the LSP and cloud policy files.
const maxOperatorFileBytes = 1 << 20

// LSP returns a detached copy of the resolved language-server
// configuration, including its secret-bearing env maps.
func (c *ResolvedConfig) LSP() lsp.Config {
	out := lsp.Config{Enabled: c.lsp.Enabled}
	if c.lsp.Servers == nil {
		return out
	}
	out.Servers = make([]lsp.ServerConfig, len(c.lsp.Servers))
	for i, server := range c.lsp.Servers {
		copied := server
		copied.Args = append([]string(nil), server.Args...)
		copied.Env = maps.Clone(server.Env)
		copied.InitializationOptions = append(json.RawMessage(nil), server.InitializationOptions...)
		copied.Settings = append(json.RawMessage(nil), server.Settings...)
		out.Servers[i] = copied
	}
	return out
}

// CloudRules returns a detached copy of the operator's kube-context rules.
// It is non-nil and empty when no policy file is selected.
func (c *ResolvedConfig) CloudRules() []cloudcmd.Rule {
	return append([]cloudcmd.Rule{}, c.cloudRules...)
}

// resolveLSP applies flag > HARNESS_ENABLE_LSP > enable_lsp to enablement and
// the same order to the server file. A disabled configuration does not read
// the file, so -enable-lsp=false always starts without language servers. An
// empty or absent file selects the built-in server definitions.
func resolveLSP(enableFlag BoolInput, enableEnv string, fileFlag StringInput, fileEnv string, settings fileSettings) (lsp.Config, error) {
	enabled, err := resolveBool(enableFlag, enableEnv, settings.EnableLSP, false)
	if err != nil {
		return lsp.Config{}, &Error{Field: "enable_lsp", Err: err}
	}
	if !enabled {
		return lsp.Normalize(lsp.Config{})
	}
	path := resolveString(fileFlag, fileEnv, settings.LSPConfig, "")
	if path == "" {
		return lsp.Normalize(lsp.Config{Enabled: true})
	}
	path = ExpandTildePath(path)
	var file struct {
		Servers []lsp.ServerConfig `json:"servers"`
	}
	if err := decodeOperatorFile(path, &file); err != nil {
		return lsp.Config{}, &Error{Field: "lsp_config", Err: err}
	}
	if file.Servers == nil {
		return lsp.Config{}, &Error{Field: "lsp_config", Err: fmt.Errorf("%s: \"servers\" must be an array", path)}
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return lsp.Config{}, &Error{Field: "lsp_config", Err: err}
	}
	for i := range file.Servers {
		server := &file.Servers[i]
		if strings.ContainsAny(server.Command, `/\`) && !filepath.IsAbs(server.Command) {
			server.Command = filepath.Join(base, server.Command)
		}
	}
	normalized, err := lsp.Normalize(lsp.Config{Enabled: true, Servers: file.Servers})
	if err != nil {
		return lsp.Config{}, &Error{Field: "lsp_config", Err: fmt.Errorf("%s: %w", path, err)}
	}
	return normalized, nil
}

// resolveCloudRules reads the operator's cloud policy file. No file, or an
// explicitly empty selection, means no scoped grants or denies; the cloud
// classifier itself stays on either way.
func resolveCloudRules(fileFlag StringInput, fileEnv string, settings fileSettings) ([]cloudcmd.Rule, error) {
	path := resolveString(fileFlag, fileEnv, settings.CloudPolicy, "")
	if path == "" {
		return []cloudcmd.Rule{}, nil
	}
	path = ExpandTildePath(path)
	var file struct {
		Rules []cloudcmd.Rule `json:"rules"`
	}
	if err := decodeOperatorFile(path, &file); err != nil {
		return nil, &Error{Field: "cloud_policy", Err: err}
	}
	if file.Rules == nil {
		return nil, &Error{Field: "cloud_policy", Err: fmt.Errorf("%s: \"rules\" must be an array", path)}
	}
	rules, err := cloudcmd.NormalizeRules(file.Rules)
	if err != nil {
		return nil, &Error{Field: "cloud_policy", Err: fmt.Errorf("%s: %w", path, err)}
	}
	return rules, nil
}

// decodeOperatorFile strictly decodes one JSON object of at most 1 MiB:
// unknown fields and trailing data are errors, not ignored.
func decodeOperatorFile(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maxOperatorFileBytes+1))
	if err != nil {
		return err
	}
	if len(content) > maxOperatorFileBytes {
		return fmt.Errorf("%s exceeds 1 MiB", path)
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s: trailing data after the JSON object", path)
	}
	return nil
}
