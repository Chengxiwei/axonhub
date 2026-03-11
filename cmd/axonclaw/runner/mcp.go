package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/looplj/axonhub/axon/agent"
	"github.com/looplj/axonhub/cmd/axonclaw/conf"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/samber/lo"
)

const (
	defaultMCPConnectTimeout = 15 * time.Second
	defaultMCPRequestTimeout = 2 * time.Minute
)

type mcpRuntime struct {
	logger   *slog.Logger
	sessions []*namedMCPSession
}

type namedMCPSession struct {
	serverName string
	session    *mcp.ClientSession
}

func (r *mcpRuntime) Close() error {
	if r == nil {
		return nil
	}

	var errOut error
	for _, s := range r.sessions {
		if s == nil || s.session == nil {
			continue
		}
		if err := s.session.Close(); err != nil {
			errOut = errors.Join(errOut, fmt.Errorf("close mcp server %q: %w", s.serverName, err))
		}
	}
	r.sessions = nil
	return errOut
}

func registerMCPTools(
	a *agent.Agent,
	workspace string,
	logger *slog.Logger,
	known map[string]struct{},
) *mcpRuntime {
	if logger == nil {
		logger = slog.Default()
	}

	runtime := &mcpRuntime{logger: logger}
	servers, err := conf.LoadMCPServers()
	if err != nil {
		logger.Warn("load mcp config failed", "path", conf.MCPPath(), "error", err)
		return runtime
	}
	if len(servers) == 0 {
		return runtime
	}

	serverNames := lo.Keys(servers)
	sort.Strings(serverNames)

	for _, serverName := range serverNames {
		serverCfg := servers[serverName]
		if !serverCfg.IsEnabled() {
			logger.Debug("mcp server disabled", "server", serverName)
			continue
		}

		command := strings.TrimSpace(serverCfg.Command)
		if command == "" {
			logger.Warn("skip mcp server: command is empty", "server", serverName)
			continue
		}

		connectTimeout := serverCfg.ConnectTimeout
		if connectTimeout <= 0 {
			connectTimeout = defaultMCPConnectTimeout
		}
		requestTimeout := serverCfg.RequestTimeout
		if requestTimeout <= 0 {
			requestTimeout = defaultMCPRequestTimeout
		}

		cmd := exec.Command(command, serverCfg.Args...)
		cmd.Dir = workspace
		cmd.Env = mergeMCPEnvs(serverCfg.Env)

		mcpClient := mcp.NewClient(&mcp.Implementation{
			Name:    "axonclaw",
			Version: "v1.0.0",
		}, &mcp.ClientOptions{
			ToolListChangedHandler: func(_ context.Context, _ *mcp.ToolListChangedRequest) {
				logger.Info("mcp tools changed on server; restart axonclaw to reload mcp tools", "server", serverName)
			},
		})

		connectCtx, cancelConnect := context.WithTimeout(context.Background(), connectTimeout)
		session, err := mcpClient.Connect(connectCtx, &mcp.CommandTransport{Command: cmd}, nil)
		cancelConnect()
		if err != nil {
			logger.Warn("connect mcp server failed", "server", serverName, "error", err)
			continue
		}

		tools, err := listMCPTools(session, connectTimeout)
		if err != nil {
			_ = session.Close()
			logger.Warn("list mcp tools failed", "server", serverName, "error", err)
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
				logger.Warn("skip mcp tool due to name conflict", "server", serverName, "tool", tool.Name, "local_name", localName)
				continue
			}

			def, err := convertMCPToolDefinition(localName, tool)
			if err != nil {
				logger.Warn("skip invalid mcp tool schema", "server", serverName, "tool", tool.Name, "error", err)
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
			logger.Warn("mcp server connected but no tools were registered", "server", serverName)
			continue
		}

		runtime.sessions = append(runtime.sessions, &namedMCPSession{
			serverName: serverName,
			session:    session,
		})
		logger.Info("mcp server connected", "server", serverName, "tools", registeredCount)
	}

	return runtime
}

func listMCPTools(session *mcp.ClientSession, timeout time.Duration) ([]*mcp.Tool, error) {
	listCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	tools := make([]*mcp.Tool, 0)
	for tool, err := range session.Tools(listCtx, nil) {
		if err != nil {
			return nil, err
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

func mergeMCPEnvs(extra map[string]string) []string {
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

func convertMCPToolDefinition(localName string, in *mcp.Tool) (agent.ToolDefinition, error) {
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

type mcpTool struct {
	def            agent.ToolDefinition
	remoteToolName string
	serverName     string
	session        *mcp.ClientSession
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

	result, err := t.session.CallTool(callCtx, &mcp.CallToolParams{
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

func callToolResultToText(result *mcp.CallToolResult) string {
	if result == nil {
		return "{}"
	}

	parts := make([]string, 0, len(result.Content)+1)
	for _, c := range result.Content {
		if c == nil {
			continue
		}
		switch v := c.(type) {
		case *mcp.TextContent:
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
