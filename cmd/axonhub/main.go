package main

import (
	"bytes"
	"context"
	"fmt"
	"os"

	"github.com/andreazorzetto/yh/highlight"
	"github.com/hokaccha/go-prettyjson"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"gopkg.in/yaml.v3"

	sdk "go.opentelemetry.io/otel/sdk/metric"

	"github.com/looplj/axonhub/conf"
	"github.com/looplj/axonhub/internal/build"
	"github.com/looplj/axonhub/internal/dumper"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/migrate/dbmigrate"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/metrics"
	"github.com/looplj/axonhub/internal/server"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "config":
			handleConfigCommand()
			return
		case "version", "--version", "-v":
			showVersion()
			return
		case "help", "--help", "-h":
			showHelp()
		case "build-info":
			showBuildInfo()
			return
		case "migrate":
			handleMigrateCommand()
			return
		}
	}

	startServer()
}

func showBuildInfo() {
	fmt.Println(build.GetBuildInfo())
}

type logger struct{}

func (l *logger) LogEvent(event fxevent.Event) {
	log.Debug(context.Background(), "fx event", log.Any("event", event))
}

func startServer() {
	server.Run(
		fx.WithLogger(func() fxevent.Logger {
			return &logger{}
		}),
		fx.Provide(conf.Load),
		fx.Provide(metrics.NewProvider),
		fx.Provide(dumper.New),
		fx.Invoke(dumper.SetGlobal),
		fx.Invoke(func(lc fx.Lifecycle, server *server.Server, provider *sdk.MeterProvider, ent *ent.Client) {
			lc.Append(fx.Hook{
				OnStart: func(ctx context.Context) error {
					if provider != nil {
						return metrics.SetupMetrics(provider, server.Config.Name)
					}

					return nil
				},
				OnStop: func(ctx context.Context) error {
					if provider != nil {
						return provider.Shutdown(ctx)
					}

					return nil
				},
			})
			lc.Append(fx.Hook{
				OnStart: func(ctx context.Context) error {
					go func() {
						err := server.Run()
						if err != nil {
							log.Error(context.Background(), "server run error:", log.Cause(err))
							os.Exit(1)
						}
					}()

					return nil
				},
				OnStop: func(ctx context.Context) error {
					err := server.Shutdown(ctx)
					if err != nil {
						log.Error(context.Background(), "server shutdown error:", log.Cause(err))
					}

					err = ent.Close()
					if err != nil {
						log.Error(context.Background(), "ent close error:", log.Cause(err))
					}

					return nil
				},
			})
		}),
	)
}

func handleMigrateCommand() {
	if len(os.Args) < 5 {
		fmt.Println("Usage: axonhub migrate <src-dialect> <src-dsn> <dst-dialect> <dst-dsn> [options]")
		fmt.Println("")
		fmt.Println("Options:")
		fmt.Println("  --batch-size SIZE       Batch size for migration (default: 1000)")
		fmt.Println("  --skip-schema          Skip schema creation on destination")
		fmt.Println("  --dry-run              Perform a dry run without migrating data")
		fmt.Println("")
		fmt.Println("Supported dialects: postgres, sqlite, mysql")
		os.Exit(1)
	}

	srcDialect := os.Args[2]
	srcDSN := os.Args[3]
	dstDialect := os.Args[4]
	dstDSN := os.Args[5]

	// Parse options
	opts := dbmigrate.DefaultOptions()

	for i := 6; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--batch-size":
			if i+1 < len(os.Args) {
				var batchSize int
				_, err := fmt.Sscanf(os.Args[i+1], "%d", &batchSize)
				if err != nil {
					fmt.Printf("Invalid batch size: %v\n", err)
					os.Exit(1)
				}
				opts.BatchSize = batchSize
				i++ // Skip next argument
			}
		case "--skip-schema":
			opts.SkipSchema = true
		case "--dry-run":
			opts.DryRun = true
		}
	}

	// Validate dialects
	if _, err := dbmigrate.ParseDialect(srcDialect); err != nil {
		fmt.Printf("Invalid source dialect: %v\n", err)
		os.Exit(1)
	}

	if _, err := dbmigrate.ParseDialect(dstDialect); err != nil {
		fmt.Printf("Invalid destination dialect: %v\n", err)
		os.Exit(1)
	}

	// Create migrator
	srcCfg := dbmigrate.Config{
		Dialect: srcDialect,
		DSN:     srcDSN,
	}

	dstCfg := dbmigrate.Config{
		Dialect: dstDialect,
		DSN:     dstDSN,
	}

	migrator, err := dbmigrate.NewMigrator(srcCfg, dstCfg, opts)
	if err != nil {
		fmt.Printf("Failed to create migrator: %v\n", err)
		os.Exit(1)
	}
	defer migrator.Close()

	// Run migration
	ctx := context.Background()
	if err := migrator.Run(ctx); err != nil {
		fmt.Printf("Migration failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Migration completed successfully!")
}

func handleConfigCommand() {
	if len(os.Args) < 3 {
		fmt.Println("Usage: axonhub config <preview|validate>")
		os.Exit(1)
	}

	switch os.Args[2] {
	case "preview":
		configPreview()
	case "validate":
		configValidate()
	default:
		fmt.Println("Usage: axonhub config <preview|validate>")
		os.Exit(1)
	}
}

func configPreview() {
	format := "yml"

	for i := 3; i < len(os.Args); i++ {
		if os.Args[i] == "--format" || os.Args[i] == "-f" {
			if i+1 < len(os.Args) {
				format = os.Args[i+1]
			}
		}
	}

	config, err := conf.Load()
	if err != nil {
		fmt.Printf("Failed to load config: %v\n", err)
		os.Exit(1)
	}

	var output string

	switch format {
	case "json":
		b, err := prettyjson.Marshal(config)
		if err != nil {
			fmt.Printf("Failed to preview config: %v\n", err)
			os.Exit(1)
		}

		output = string(b)
	case "yml", "yaml":
		b, err := yaml.Marshal(config)
		if err != nil {
			fmt.Printf("Failed to preview config: %v\n", err)
			os.Exit(1)
		}

		output, err = highlight.Highlight(bytes.NewBuffer(b))
		if err != nil {
			fmt.Printf("Failed to preview config: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Printf("Unsupported format: %s\n", format)
		os.Exit(1)
	}

	fmt.Println(output)
}

func configValidate() {
	config, err := conf.Load()
	if err != nil {
		fmt.Printf("Failed to load config: %v\n", err)
		os.Exit(1)
	}

	errors := validateConfig(config)

	if len(errors) == 0 {
		fmt.Println("Configuration is valid!")
		return
	}

	fmt.Println("Configuration validation failed:")

	for _, err := range errors {
		fmt.Printf("  - %s\n", err)
	}

	os.Exit(1)
}

func validateConfig(config conf.Config) []string {
	var errors []string

	if config.APIServer.Port <= 0 || config.APIServer.Port > 65535 {
		errors = append(errors, "server.port must be between 1 and 65535")
	}

	if config.DB.DSN == "" {
		errors = append(errors, "db.dsn cannot be empty")
	}

	if config.Log.Name == "" {
		errors = append(errors, "log.name cannot be empty")
	}

	return errors
}

func showHelp() {
	fmt.Println("AxonHub AI Gateway")
	fmt.Println("")
	fmt.Println("Usage:")
	fmt.Println("  axonhub                    Start the server (default)")
	fmt.Println("  axonhub config preview     Preview configuration")
	fmt.Println("  axonhub config validate    Validate configuration")
	fmt.Println("  axonhub migrate            Migrate database from source to destination")
	fmt.Println("  axonhub version            Show version")
	fmt.Println("  axonhub help               Show this help message")
	fmt.Println("")
	fmt.Println("Migration Usage:")
	fmt.Println("  axonhub migrate <src-dialect> <src-dsn> <dst-dialect> <dst-dsn> [options]")
	fmt.Println("")
	fmt.Println("Migration Options:")
	fmt.Println("  --batch-size SIZE       Batch size for migration (default: 1000)")
	fmt.Println("  --skip-schema          Skip schema creation on destination")
	fmt.Println("  --dry-run              Perform a dry run without migrating data")
	fmt.Println("")
	fmt.Println("Supported Dialects: postgres, sqlite, mysql")
	fmt.Println("")
	fmt.Println("Options:")
	fmt.Println("  -f, --format FORMAT       Output format for config preview (yml, json)")
}

func showVersion() {
	fmt.Println(build.Version)
}
