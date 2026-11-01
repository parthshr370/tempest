package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.harness.dev/harness/internal/mcpconfig"
)

// MCPServers returns a detached copy, including secret-bearing maps.
func (c *ResolvedConfig) MCPServers() []mcpconfig.Server {
	s, _ := mcpconfig.Normalize(c.mcpServers)
	return s
}

// MCPSamplingModel returns the model spec that answers MCP server sampling
// requests; empty uses the smol role.
func (c *ResolvedConfig) MCPSamplingModel() string { return c.mcpSamplingModel }

// MCPSamplingMaxTokens caps the output of one MCP sampling request; it
// defaults to max_tokens.
func (c *ResolvedConfig) MCPSamplingMaxTokens() int { return c.mcpSamplingMaxTokens }

// MCPSamplingAllowHeadless reports whether opted-in servers may sample
// without asking when the session has no user to ask.
func (c *ResolvedConfig) MCPSamplingAllowHeadless() bool { return c.mcpSamplingHeadless }

func resolveMCP(flag StringInput, env string, settings fileSettings) ([]mcpconfig.Server, error) {
	if settings.MCPConfig != nil && settings.MCPServers != nil {
		return nil, errors.New("mcp_config and mcp_servers conflict")
	}
	path := ""
	selected := false
	switch {
	case flag.Set:
		path = flag.Value
		selected = true
	case env != "":
		path = env
		selected = true
	case settings.MCPConfig != nil:
		path = *settings.MCPConfig
		selected = true
	}
	if !selected {
		return mcpconfig.Normalize(settings.MCPServers)
	}
	if strings.TrimSpace(path) == "" {
		return []mcpconfig.Server{}, nil
	}
	path = ExpandTildePath(path)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, errors.New("MCP config exceeds 1 MiB")
	}
	servers, err := mcpconfig.ParseFile(data)
	if err != nil {
		return nil, fmt.Errorf("invalid MCP file %s: %w", path, err)
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	for i := range servers {
		s := &servers[i]
		if s.Transport == "stdio" && strings.ContainsAny(s.Command, "/\\") && !filepath.IsAbs(s.Command) {
			s.Command = filepath.Join(base, s.Command)
		}
	}
	return mcpconfig.Normalize(servers)
}
