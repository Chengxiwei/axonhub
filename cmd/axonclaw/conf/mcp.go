package conf

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const MCPFileName = "mcp_servers.json"

type mcpConfigDocument struct {
	Servers map[string]mcpServerConfigJSON `json:"servers"`
}

type mcpServerConfigJSON struct {
	Enabled        *bool             `json:"enabled,omitempty"`
	Command        string            `json:"command,omitempty"`
	Args           []string          `json:"args,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	ToolPrefix     string            `json:"tool_prefix,omitempty"`
	RequestTimeout string            `json:"request_timeout,omitempty"`
	ConnectTimeout string            `json:"connect_timeout,omitempty"`
}

func MCPPath() string {
	return filepath.Join(DefaultDir, MCPFileName)
}

func LoadMCPServers() (map[string]MCPServerConfig, error) {
	path := MCPPath()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]MCPServerConfig{}, nil
		}
		return nil, err
	}
	if strings.TrimSpace(string(b)) == "" {
		return map[string]MCPServerConfig{}, nil
	}

	var doc mcpConfigDocument
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse mcp config %s: %w", path, err)
	}

	servers := make(map[string]MCPServerConfig, len(doc.Servers))
	for name, in := range doc.Servers {
		cfg := MCPServerConfig{
			Enabled:    in.Enabled,
			Command:    strings.TrimSpace(in.Command),
			Args:       in.Args,
			Env:        in.Env,
			ToolPrefix: in.ToolPrefix,
		}

		if in.RequestTimeout != "" {
			d, err := time.ParseDuration(in.RequestTimeout)
			if err != nil {
				return nil, fmt.Errorf("parse request_timeout for mcp server %q: %w", name, err)
			}
			cfg.RequestTimeout = d
		}
		if in.ConnectTimeout != "" {
			d, err := time.ParseDuration(in.ConnectTimeout)
			if err != nil {
				return nil, fmt.Errorf("parse connect_timeout for mcp server %q: %w", name, err)
			}
			cfg.ConnectTimeout = d
		}

		servers[name] = cfg
	}

	return servers, nil
}

func SaveMCPServers(servers map[string]MCPServerConfig) error {
	doc := mcpConfigDocument{
		Servers: make(map[string]mcpServerConfigJSON, len(servers)),
	}
	for name, in := range servers {
		out := mcpServerConfigJSON{
			Enabled:    in.Enabled,
			Command:    strings.TrimSpace(in.Command),
			Args:       in.Args,
			Env:        in.Env,
			ToolPrefix: in.ToolPrefix,
		}
		if in.RequestTimeout > 0 {
			out.RequestTimeout = in.RequestTimeout.String()
		}
		if in.ConnectTimeout > 0 {
			out.ConnectTimeout = in.ConnectTimeout.String()
		}
		doc.Servers[name] = out
	}

	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal mcp config: %w", err)
	}
	b = append(b, '\n')

	if err := os.MkdirAll(DefaultDir, 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	return os.WriteFile(MCPPath(), b, 0o600)
}
