package cmds

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/looplj/axonhub/cmd/axonclaw/conf"
)

func NewMCPCommand(opts StdioOptions) *cobra.Command {
	out := opts.Stdout
	if out == nil {
		out = os.Stdout
	}
	errOut := opts.Stderr
	if errOut == nil {
		errOut = os.Stderr
	}
	root := &cobra.Command{
		Use:   "mcp",
		Short: "Manage MCP server configuration",
		Long: `Manage MCP servers in dedicated JSON config.

MCP config file:
  .axonclaw/mcp_servers.json`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(newConfMCPPathCmd(out))
	root.AddCommand(newConfMCPListCmd(out))
	root.AddCommand(newConfMCPGetCmd(out))
	root.AddCommand(newConfMCPSetCmd(out, errOut))
	root.AddCommand(newConfMCPDeleteCmd(out, errOut))
	root.AddCommand(newConfMCPEnableCmd(out, errOut, true))
	root.AddCommand(newConfMCPEnableCmd(out, errOut, false))

	return root
}

func newConfMCPPathCmd(out *os.File) *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Show MCP config file path",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(out, conf.MCPPath())
			return nil
		},
	}
}

func newConfMCPListCmd(out *os.File) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List MCP servers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			servers, err := conf.LoadMCPServers()
			if err != nil {
				return err
			}
			if len(servers) == 0 {
				fmt.Fprintln(out, "No MCP servers configured.")
				return nil
			}

			names := make([]string, 0, len(servers))
			for name := range servers {
				names = append(names, name)
			}
			sort.Strings(names)

			for _, name := range names {
				s := servers[name]
				fmt.Fprintf(out, "%s\tenabled=%v\tcommand=%s\targs=%d\ttool_prefix=%s\n",
					name, s.IsEnabled(), s.Command, len(s.Args), s.ToolPrefix)
			}
			return nil
		},
	}
}

func newConfMCPGetCmd(out *os.File) *cobra.Command {
	return &cobra.Command{
		Use:   "get <name>",
		Short: "Get one MCP server config as JSON",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			servers, err := conf.LoadMCPServers()
			if err != nil {
				return err
			}
			s, ok := servers[name]
			if !ok {
				return fmt.Errorf("mcp server %q not found", name)
			}

			view := map[string]any{
				"name":            name,
				"enabled":         s.IsEnabled(),
				"command":         s.Command,
				"args":            s.Args,
				"env":             s.Env,
				"tool_prefix":     s.ToolPrefix,
				"request_timeout": s.RequestTimeout.String(),
				"connect_timeout": s.ConnectTimeout.String(),
			}
			raw, err := json.MarshalIndent(view, "", "  ")
			if err != nil {
				return err
			}
			fmt.Fprintln(out, string(raw))
			return nil
		},
	}
}

func newConfMCPSetCmd(out *os.File, errOut *os.File) *cobra.Command {
	var (
		command        string
		args           []string
		envs           []string
		toolPrefix     string
		requestTimeout time.Duration
		connectTimeout time.Duration
		enabled        bool
		clearArgs      bool
		clearEnv       bool
	)

	cmd := &cobra.Command{
		Use:   "set <name>",
		Short: "Create or update an MCP server",
		Long: `Create or update one MCP server in .axonclaw/mcp_servers.json.

Examples:
  axonclaw mcp set github --command npx --arg -y --arg @modelcontextprotocol/server-github
  axonclaw mcp set github --env GITHUB_TOKEN=${GITHUB_TOKEN}
  axonclaw mcp set github --request-timeout 2m --connect-timeout 15s --enabled true`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, in []string) error {
			name := strings.TrimSpace(in[0])
			if name == "" {
				return fmt.Errorf("name is required")
			}

			servers, err := conf.LoadMCPServers()
			if err != nil {
				return err
			}

			current, exists := servers[name]
			changed := false

			if cmd.Flags().Changed("command") {
				command = strings.TrimSpace(command)
				if command == "" {
					return fmt.Errorf("--command cannot be empty")
				}
				current.Command = command
				changed = true
			}
			if cmd.Flags().Changed("arg") {
				current.Args = args
				changed = true
			}
			if clearArgs {
				current.Args = nil
				changed = true
			}
			if cmd.Flags().Changed("env") {
				parsed, err := parseMCPEnv(envs)
				if err != nil {
					return err
				}
				current.Env = parsed
				changed = true
			}
			if clearEnv {
				current.Env = nil
				changed = true
			}
			if cmd.Flags().Changed("tool-prefix") {
				current.ToolPrefix = strings.TrimSpace(toolPrefix)
				changed = true
			}
			if cmd.Flags().Changed("request-timeout") {
				if requestTimeout <= 0 {
					return fmt.Errorf("--request-timeout must be greater than 0")
				}
				current.RequestTimeout = requestTimeout
				changed = true
			}
			if cmd.Flags().Changed("connect-timeout") {
				if connectTimeout <= 0 {
					return fmt.Errorf("--connect-timeout must be greater than 0")
				}
				current.ConnectTimeout = connectTimeout
				changed = true
			}
			if cmd.Flags().Changed("enabled") {
				current.Enabled = &enabled
				changed = true
			}

			if strings.TrimSpace(current.Command) == "" {
				if exists {
					return fmt.Errorf("mcp server %q has empty command; use --command to set it", name)
				}
				return fmt.Errorf("--command is required when creating a new mcp server")
			}
			if !exists && !changed {
				return fmt.Errorf("no fields changed")
			}

			servers[name] = current
			if err := conf.SaveMCPServers(servers); err != nil {
				return err
			}

			fmt.Fprintf(errOut, "config\t%s\n", conf.MCPPath())
			fmt.Fprintf(out, "mcp_server\t%s\n", name)
			return nil
		},
	}

	cmd.Flags().StringVar(&command, "command", "", "MCP server command")
	cmd.Flags().StringArrayVar(&args, "arg", nil, "MCP server argument (repeatable)")
	cmd.Flags().StringArrayVar(&envs, "env", nil, "Environment variable KEY=VALUE (repeatable, replaces full env map)")
	cmd.Flags().StringVar(&toolPrefix, "tool-prefix", "", "Tool name prefix exposed to agent (optional)")
	cmd.Flags().DurationVar(&requestTimeout, "request-timeout", 0, "Per-tool request timeout (e.g. 2m)")
	cmd.Flags().DurationVar(&connectTimeout, "connect-timeout", 0, "Connection timeout when starting MCP server (e.g. 15s)")
	cmd.Flags().BoolVar(&enabled, "enabled", true, "Enable or disable this MCP server")
	cmd.Flags().BoolVar(&clearArgs, "clear-args", false, "Clear all configured args")
	cmd.Flags().BoolVar(&clearEnv, "clear-env", false, "Clear all configured env variables")

	return cmd
}

func newConfMCPDeleteCmd(out *os.File, errOut *os.File) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete one MCP server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			servers, err := conf.LoadMCPServers()
			if err != nil {
				return err
			}
			if _, ok := servers[name]; !ok {
				return fmt.Errorf("mcp server %q not found", name)
			}

			delete(servers, name)
			if err := conf.SaveMCPServers(servers); err != nil {
				return err
			}

			fmt.Fprintf(errOut, "config\t%s\n", conf.MCPPath())
			fmt.Fprintf(out, "mcp_server deleted\t%s\n", name)
			return nil
		},
	}
}

func newConfMCPEnableCmd(out *os.File, errOut *os.File, enabled bool) *cobra.Command {
	use := "enable"
	short := "Enable an MCP server"
	if !enabled {
		use = "disable"
		short = "Disable an MCP server"
	}

	return &cobra.Command{
		Use:   use + " <name>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			servers, err := conf.LoadMCPServers()
			if err != nil {
				return err
			}
			s, ok := servers[name]
			if !ok {
				return fmt.Errorf("mcp server %q not found", name)
			}
			s.Enabled = &enabled
			servers[name] = s

			if err := conf.SaveMCPServers(servers); err != nil {
				return err
			}
			fmt.Fprintf(errOut, "config\t%s\n", conf.MCPPath())
			fmt.Fprintf(out, "mcp_server %s\t%s\n", use, name)
			return nil
		},
	}
}

func parseMCPEnv(items []string) (map[string]string, error) {
	env := make(map[string]string, len(items))
	for _, item := range items {
		parts := strings.SplitN(item, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid --env %q: expected KEY=VALUE", item)
		}
		key := strings.TrimSpace(parts[0])
		if key == "" {
			return nil, fmt.Errorf("invalid --env %q: key cannot be empty", item)
		}
		env[key] = parts[1]
	}
	return env, nil
}
