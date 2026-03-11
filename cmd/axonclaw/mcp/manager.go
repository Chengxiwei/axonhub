package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/looplj/axonhub/axon/agent"
	"github.com/looplj/axonhub/cmd/axonclaw/conf"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/samber/lo"
)

const (
	FileName = "mcp_servers.json"

	defaultConnectTimeout = 15 * time.Second
	defaultRequestTimeout = 2 * time.Minute
)

// ServerConfig holds the runtime configuration for a single MCP server.
type ServerConfig struct {
	Enabled        *bool             `json:"enabled,omitempty"`
	Command        string            `json:"command,omitempty"`
	Args           []string          `json:"args,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	ToolPrefix     string            `json:"tool_prefix,omitempty"`
	RequestTimeout time.Duration     `json:"-"`
	ConnectTimeout time.Duration     `json:"-"`
}

func (c ServerConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

func (c ServerConfig) MarshalJSON() ([]byte, error) {
	type Alias ServerConfig
	aux := struct {
		Alias
		RequestTimeout string `json:"request_timeout,omitempty"`
		ConnectTimeout string `json:"connect_timeout,omitempty"`
	}{
		Alias: Alias(c),
	}
	if c.RequestTimeout > 0 {
		aux.RequestTimeout = c.RequestTimeout.String()
	}
	if c.ConnectTimeout > 0 {
		aux.ConnectTimeout = c.ConnectTimeout.String()
	}
	return json.Marshal(aux)
}

func (c *ServerConfig) UnmarshalJSON(data []byte) error {
	type Alias ServerConfig
	aux := struct {
		*Alias
		RequestTimeout string `json:"request_timeout,omitempty"`
		ConnectTimeout string `json:"connect_timeout,omitempty"`
	}{
		Alias: (*Alias)(c),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	c.Command = strings.TrimSpace(c.Command)
	if aux.RequestTimeout != "" {
		d, err := time.ParseDuration(aux.RequestTimeout)
		if err != nil {
			return fmt.Errorf("parse request_timeout: %w", err)
		}
		c.RequestTimeout = d
	}
	if aux.ConnectTimeout != "" {
		d, err := time.ParseDuration(aux.ConnectTimeout)
		if err != nil {
			return fmt.Errorf("parse connect_timeout: %w", err)
		}
		c.ConnectTimeout = d
	}
	return nil
}

// Manager manages MCP server configurations and active client sessions.
type Manager struct {
	logger   *slog.Logger
	sessions []*namedSession
}

type namedSession struct {
	serverName string
	session    *mcpsdk.ClientSession
}

// NewManager creates a new Manager.
func NewManager(logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{logger: logger}
}

// Close terminates all active MCP sessions.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}

	var errOut error
	for _, s := range m.sessions {
		if s == nil || s.session == nil {
			continue
		}
		if err := s.session.Close(); err != nil {
			errOut = errors.Join(errOut, fmt.Errorf("close mcp server %q: %w", s.serverName, err))
		}
	}
	m.sessions = nil
	return errOut
}

// RegisterTools loads MCP server configs, connects to each enabled server,
// and registers the discovered tools on the given agent.
// known tracks already-registered tool names to avoid conflicts.
func (m *Manager) RegisterTools(a *agent.Agent, workspace string, known map[string]struct{}) {
	servers, err := LoadServers()
	if err != nil {
		m.logger.Warn("load mcp config failed", "path", ConfigPath(), "error", err)
		return
	}
	if len(servers) == 0 {
		return
	}

	serverNames := lo.Keys(servers)
	sort.Strings(serverNames)

	for _, serverName := range serverNames {
		serverCfg := servers[serverName]
		if !serverCfg.IsEnabled() {
			m.logger.Debug("mcp server disabled", "server", serverName)
			continue
		}

		command := strings.TrimSpace(serverCfg.Command)
		if command == "" {
			m.logger.Warn("skip mcp server: command is empty", "server", serverName)
			continue
		}

		connectTimeout := serverCfg.ConnectTimeout
		if connectTimeout <= 0 {
			connectTimeout = defaultConnectTimeout
		}
		requestTimeout := serverCfg.RequestTimeout
		if requestTimeout <= 0 {
			requestTimeout = defaultRequestTimeout
		}

		cmd := exec.Command(command, serverCfg.Args...)
		cmd.Dir = workspace
		cmd.Env = mergeEnvs(serverCfg.Env)

		mcpClient := mcpsdk.NewClient(&mcpsdk.Implementation{
			Name:    "axonclaw",
			Version: "v1.0.0",
		}, &mcpsdk.ClientOptions{
			ToolListChangedHandler: func(_ context.Context, _ *mcpsdk.ToolListChangedRequest) {
				m.logger.Info("mcp tools changed on server; restart axonclaw to reload mcp tools", "server", serverName)
			},
		})

		connectCtx, cancelConnect := context.WithTimeout(context.Background(), connectTimeout)
		session, err := mcpClient.Connect(connectCtx, &mcpsdk.CommandTransport{Command: cmd}, nil)
		cancelConnect()
		if err != nil {
			m.logger.Warn("connect mcp server failed", "server", serverName, "error", err)
			continue
		}

		tools, err := listTools(session, connectTimeout)
		if err != nil {
			_ = session.Close()
			m.logger.Warn("list mcp tools failed", "server", serverName, "error", err)
			continue
		}

		prefix := strings.TrimSpace(serverCfg.ToolPrefix)
		if prefix == "" {
			prefix = sanitizeToolNameSegment(serverName) + "__"
		}

		registeredCount := 0
		for _, tool := range tools {
			if tool == nil || strings.TrimSpace(tool.Name) == "" {
				continue
			}

			localName := prefix + sanitizeToolNameSegment(tool.Name)
			if _, exists := known[localName]; exists {
				m.logger.Warn("skip mcp tool due to name conflict", "server", serverName, "tool", tool.Name, "local_name", localName)
				continue
			}

			def, err := convertToolDefinition(localName, tool)
			if err != nil {
				m.logger.Warn("skip invalid mcp tool schema", "server", serverName, "tool", tool.Name, "error", err)
				continue
			}

			a.RegisterTool(&mcpTool{
				def:            def,
				remoteToolName: tool.Name,
				serverName:     serverName,
				session:        session,
				timeout:        requestTimeout,
			})
			known[localName] = struct{}{}
			registeredCount++
		}

		if registeredCount == 0 {
			_ = session.Close()
			m.logger.Warn("mcp server connected but no tools were registered", "server", serverName)
			continue
		}

		m.sessions = append(m.sessions, &namedSession{
			serverName: serverName,
			session:    session,
		})
		m.logger.Info("mcp server connected", "server", serverName, "tools", registeredCount)
	}
}

// ---------------------------------------------------------------------------
// Config persistence
// ---------------------------------------------------------------------------

type Config struct {
	Servers map[string]ServerConfig `json:"servers"`
}

// ConfigPath returns the full path to the MCP config file.
func ConfigPath() string {
	return filepath.Join(conf.DefaultDir, FileName)
}

// LoadServers reads MCP server configurations from disk.
func LoadServers() (map[string]ServerConfig, error) {
	path := ConfigPath()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]ServerConfig{}, nil
		}
		return nil, err
	}
	if strings.TrimSpace(string(b)) == "" {
		return map[string]ServerConfig{}, nil
	}

	var doc Config
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse mcp config %s: %w", path, err)
	}

	return doc.Servers, nil
}

// SaveServers persists MCP server configurations to disk.
func SaveServers(servers map[string]ServerConfig) error {
	doc := Config{Servers: servers}

	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal mcp config: %w", err)
	}
	b = append(b, '\n')

	if err := os.MkdirAll(conf.DefaultDir, 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	return os.WriteFile(ConfigPath(), b, 0o600)
}

// ---------------------------------------------------------------------------
// Tool helpers
// ---------------------------------------------------------------------------

func listTools(session *mcpsdk.ClientSession, timeout time.Duration) ([]*mcpsdk.Tool, error) {
	listCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	tools := make([]*mcpsdk.Tool, 0)
	for tool, err := range session.Tools(listCtx, nil) {
		if err != nil {
			return nil, err
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

func mergeEnvs(extra map[string]string) []string {
	out := append([]string{}, os.Environ()...)
	if len(extra) == 0 {
		return out
	}

	keys := lo.Keys(extra)
	sort.Strings(keys)
	for _, k := range keys {
		key := strings.TrimSpace(k)
		if key == "" {
			continue
		}
		value := os.ExpandEnv(extra[k])
		out = append(out, key+"="+value)
	}
	return out
}

func sanitizeToolNameSegment(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "mcp"
	}

	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '_' || r == '-' || r == '.' {
			b.WriteRune(r)
			continue
		}
		b.WriteRune('_')
	}

	name := strings.Trim(b.String(), "_")
	if name == "" {
		return "mcp"
	}
	return name
}

func convertToolDefinition(localName string, in *mcpsdk.Tool) (agent.ToolDefinition, error) {
	schema := jsonschema.Schema{}
	if in.InputSchema != nil {
		raw, err := json.Marshal(in.InputSchema)
		if err != nil {
			return agent.ToolDefinition{}, err
		}
		if len(raw) > 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, &schema); err != nil {
				return agent.ToolDefinition{}, err
			}
		}
	}

	if schema.Type == "" {
		schema = jsonschema.Schema{
			Schema:               "https://json-schema.org/draft/2020-12/schema",
			Type:                 "object",
			AdditionalProperties: &jsonschema.Schema{},
		}
	}

	desc := strings.TrimSpace(in.Description)
	if desc == "" {
		desc = fmt.Sprintf("MCP tool %q", in.Name)
	}

	return agent.ToolDefinition{
		Name:        localName,
		Description: desc,
		Parameters:  schema,
	}, nil
}

// ---------------------------------------------------------------------------
// mcpTool – agent.Tool implementation for a remote MCP tool
// ---------------------------------------------------------------------------

type mcpTool struct {
	def            agent.ToolDefinition
	remoteToolName string
	serverName     string
	session        *mcpsdk.ClientSession
	timeout        time.Duration
}

func (t *mcpTool) Definition() agent.ToolDefinition {
	return t.def
}

func (t *mcpTool) Execute(ctx context.Context, arguments json.RawMessage) agent.ToolResult {
	if t.session == nil {
		return agent.ToolResult{Error: fmt.Errorf("mcp server %q is not connected", t.serverName)}
	}

	callCtx := ctx
	cancel := func() {}
	if t.timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, t.timeout)
	}
	defer cancel()

	args := map[string]any{}
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &args); err != nil {
			return agent.ToolResult{Error: fmt.Errorf("invalid arguments: %w", err)}
		}
	}

	result, err := t.session.CallTool(callCtx, &mcpsdk.CallToolParams{
		Name:      t.remoteToolName,
		Arguments: args,
	})
	if err != nil {
		return agent.ToolResult{Error: fmt.Errorf("mcp call %q on %q failed: %w", t.remoteToolName, t.serverName, err)}
	}

	text := callToolResultToText(result)
	if result.IsError {
		return agent.ToolResult{Error: fmt.Errorf("mcp tool %q returned error: %s", t.remoteToolName, text)}
	}
	return agent.ToolResult{
		Content: agent.Content{Text: &text},
	}
}

func callToolResultToText(result *mcpsdk.CallToolResult) string {
	if result == nil {
		return "{}"
	}

	parts := make([]string, 0, len(result.Content)+1)
	for _, c := range result.Content {
		if c == nil {
			continue
		}
		switch v := c.(type) {
		case *mcpsdk.TextContent:
			if strings.TrimSpace(v.Text) != "" {
				parts = append(parts, v.Text)
			}
		default:
			raw, err := json.Marshal(v)
			if err != nil {
				parts = append(parts, fmt.Sprintf("%v", v))
			} else {
				parts = append(parts, string(raw))
			}
		}
	}

	if result.StructuredContent != nil {
		raw, err := json.Marshal(result.StructuredContent)
		if err == nil && string(raw) != "null" && string(raw) != "{}" {
			parts = append(parts, string(raw))
		}
	}

	if len(parts) == 0 {
		return "{}"
	}
	return strings.Join(parts, "\n")
}
