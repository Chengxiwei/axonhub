// Package dbmigrate provides database-to-database migration functionality.
// It supports migrating all data from one database to another, including
// SQLite, PostgreSQL, and MySQL databases.
package dbmigrate

import (
	"context"
	"database/sql"
	"fmt"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/schema"
	"github.com/samber/lo"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/migrate"
	"github.com/looplj/axonhub/internal/ent/migrate/schemahook"
	"github.com/looplj/axonhub/internal/ent/privacy"
	_ "github.com/looplj/axonhub/internal/ent/runtime"
	"github.com/looplj/axonhub/internal/log"
	_ "github.com/looplj/axonhub/internal/pkg/sqlite"
)

// Config holds the database connection configuration.
type Config struct {
	Dialect string
	DSN     string
}

// MigrateOptions holds options for the migration process.
type MigrateOptions struct {
	// BatchSize is the number of records to process in each batch.
	BatchSize int
	// SkipSchema skips schema creation on the destination database.
	SkipSchema bool
	// DryRun performs a dry run without actually migrating data.
	DryRun bool
}

// DefaultOptions returns the default migration options.
func DefaultOptions() MigrateOptions {
	return MigrateOptions{
		BatchSize:  1000,
		SkipSchema: false,
		DryRun:     false,
	}
}

// Migrator handles database-to-database migration.
type Migrator struct {
	src     *ent.Client
	dst     *ent.Client
	options MigrateOptions
}

// NewMigrator creates a new Migrator instance.
func NewMigrator(srcCfg, dstCfg Config, opts MigrateOptions) (*Migrator, error) {
	srcClient, err := newEntClient(srcCfg, false)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to source database: %w", err)
	}

	dstClient, err := newEntClient(dstCfg, !opts.SkipSchema)
	if err != nil {
		srcClient.Close()
		return nil, fmt.Errorf("failed to connect to destination database: %w", err)
	}

	return &Migrator{
		src:     srcClient,
		dst:     dstClient,
		options: opts,
	}, nil
}

// Close closes both source and destination database connections.
func (m *Migrator) Close() error {
	var errs []error
	if err := m.src.Close(); err != nil {
		errs = append(errs, fmt.Errorf("failed to close source: %w", err))
	}
	if err := m.dst.Close(); err != nil {
		errs = append(errs, fmt.Errorf("failed to close destination: %w", err))
	}
	if len(errs) > 0 {
		return fmt.Errorf("close errors: %v", errs)
	}
	return nil
}

// Run executes the full database migration.
func (m *Migrator) Run(ctx context.Context) error {
	ctx = privacy.DecisionContext(ctx, privacy.Allow)

	if m.options.DryRun {
		log.Info(ctx, "dry run mode enabled, no data will be migrated")
	}

	// Migrate in order to respect foreign key constraints
	// Tables without dependencies first, then tables with dependencies
	migrators := []func(context.Context) (int, error){
		m.migrateSystem,
		m.migrateRoles,
		m.migrateUsers,
		m.migrateUserRoles,
		m.migrateProjects,
		m.migrateUserProjects,
		m.migrateAPIKeys,
		m.migrateChannels,
		m.migrateDataStorages,
	}

	totalMigrated := 0
	for _, migrator := range migrators {
		count, err := migrator(ctx)
		if err != nil {
			return err
		}
		totalMigrated += count
	}

	log.Info(ctx, "migration completed", log.Int("total_records", totalMigrated))
	return nil
}

func (m *Migrator) migrateSystem(ctx context.Context) (int, error) {
	log.Info(ctx, "migrating system table")

	systems, err := m.src.System.Query().All(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to query systems: %w", err)
	}

	if m.options.DryRun {
		log.Info(ctx, "would migrate systems", log.Int("count", len(systems)))
		return len(systems), nil
	}

	for _, s := range systems {
		err := m.dst.System.Create().
			SetKey(s.Key).
			SetValue(s.Value).
			SetCreatedAt(s.CreatedAt).
			SetUpdatedAt(s.UpdatedAt).
			OnConflict().
			UpdateNewValues().
			Exec(ctx)
		if err != nil {
			return 0, fmt.Errorf("failed to create system: %w", err)
		}
	}

	log.Info(ctx, "migrated systems", log.Int("count", len(systems)))
	return len(systems), nil
}

func (m *Migrator) migrateRoles(ctx context.Context) (int, error) {
	log.Info(ctx, "migrating roles table")

	roles, err := m.src.Role.Query().All(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to query roles: %w", err)
	}

	if m.options.DryRun {
		log.Info(ctx, "would migrate roles", log.Int("count", len(roles)))
		return len(roles), nil
	}

	for _, r := range roles {
		err := m.dst.Role.Create().
			SetCreatedAt(r.CreatedAt).
			SetUpdatedAt(r.UpdatedAt).
			SetName(r.Name).
			SetLevel(r.Level).
			SetProjectID(lo.FromPtr(r.ProjectID)).
			SetScopes(r.Scopes).
			OnConflict().
			UpdateNewValues().
			Exec(ctx)
		if err != nil {
			return 0, fmt.Errorf("failed to create role: %w", err)
		}
	}

	log.Info(ctx, "migrated roles", log.Int("count", len(roles)))
	return len(roles), nil
}

func (m *Migrator) migrateUsers(ctx context.Context) (int, error) {
	log.Info(ctx, "migrating users table")

	users, err := m.src.User.Query().All(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to query users: %w", err)
	}

	if m.options.DryRun {
		log.Info(ctx, "would migrate users", log.Int("count", len(users)))
		return len(users), nil
	}

	for _, u := range users {
		err := m.dst.User.Create().
			SetCreatedAt(u.CreatedAt).
			SetUpdatedAt(u.UpdatedAt).
			SetFirstName(u.FirstName).
			SetLastName(u.LastName).
			SetEmail(u.Email).
			SetPassword(u.Password).
			SetAvatar(u.Avatar).
			SetStatus(u.Status).
			SetIsOwner(u.IsOwner).
			OnConflict().
			UpdateNewValues().
			Exec(ctx)
		if err != nil {
			return 0, fmt.Errorf("failed to create user: %w", err)
		}
	}

	log.Info(ctx, "migrated users", log.Int("count", len(users)))
	return len(users), nil
}

func (m *Migrator) migrateUserRoles(ctx context.Context) (int, error) {
	log.Info(ctx, "migrating user_roles table")

	userRoles, err := m.src.UserRole.Query().All(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to query user_roles: %w", err)
	}

	if m.options.DryRun {
		log.Info(ctx, "would migrate user_roles", log.Int("count", len(userRoles)))
		return len(userRoles), nil
	}

	for _, ur := range userRoles {
		err := m.dst.UserRole.Create().
			SetCreatedAt(lo.FromPtr(ur.CreatedAt)).
			SetUpdatedAt(lo.FromPtr(ur.UpdatedAt)).
			SetUserID(ur.UserID).
			SetRoleID(ur.RoleID).
			OnConflict().
			UpdateNewValues().
			Exec(ctx)
		if err != nil {
			return 0, fmt.Errorf("failed to create user_role: %w", err)
		}
	}

	log.Info(ctx, "migrated user_roles", log.Int("count", len(userRoles)))
	return len(userRoles), nil
}

func (m *Migrator) migrateProjects(ctx context.Context) (int, error) {
	log.Info(ctx, "migrating projects table")

	projects, err := m.src.Project.Query().All(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to query projects: %w", err)
	}

	if m.options.DryRun {
		log.Info(ctx, "would migrate projects", log.Int("count", len(projects)))
		return len(projects), nil
	}

	for _, p := range projects {
		err := m.dst.Project.Create().
			SetCreatedAt(p.CreatedAt).
			SetUpdatedAt(p.UpdatedAt).
			SetName(p.Name).
			SetDescription(p.Description).
			SetStatus(p.Status).
			OnConflict().
			UpdateNewValues().
			Exec(ctx)
		if err != nil {
			return 0, fmt.Errorf("failed to create project: %w", err)
		}
	}

	log.Info(ctx, "migrated projects", log.Int("count", len(projects)))
	return len(projects), nil
}

func (m *Migrator) migrateUserProjects(ctx context.Context) (int, error) {
	log.Info(ctx, "migrating user_projects table")

	userProjects, err := m.src.UserProject.Query().All(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to query user_projects: %w", err)
	}

	if m.options.DryRun {
		log.Info(ctx, "would migrate user_projects", log.Int("count", len(userProjects)))
		return len(userProjects), nil
	}

	for _, up := range userProjects {
		err := m.dst.UserProject.Create().
			SetCreatedAt(up.CreatedAt).
			SetUpdatedAt(up.UpdatedAt).
			SetUserID(up.UserID).
			SetProjectID(up.ProjectID).
			SetIsOwner(up.IsOwner).
			OnConflict().
			UpdateNewValues().
			Exec(ctx)
		if err != nil {
			return 0, fmt.Errorf("failed to create user_project: %w", err)
		}
	}

	log.Info(ctx, "migrated user_projects", log.Int("count", len(userProjects)))
	return len(userProjects), nil
}

func (m *Migrator) migrateAPIKeys(ctx context.Context) (int, error) {
	log.Info(ctx, "migrating api_keys table")

	apiKeys, err := m.src.APIKey.Query().All(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to query api_keys: %w", err)
	}

	if m.options.DryRun {
		log.Info(ctx, "would migrate api_keys", log.Int("count", len(apiKeys)))
		return len(apiKeys), nil
	}

	for _, ak := range apiKeys {
		err := m.dst.APIKey.Create().
			SetCreatedAt(ak.CreatedAt).
			SetUpdatedAt(ak.UpdatedAt).
			SetName(ak.Name).
			SetKey(ak.Key).
			SetProjectID(ak.ProjectID).
			SetUserID(ak.UserID).
			SetStatus(ak.Status).
			OnConflict().
			UpdateNewValues().
			Exec(ctx)
		if err != nil {
			return 0, fmt.Errorf("failed to create api_key: %w", err)
		}
	}

	log.Info(ctx, "migrated api_keys", log.Int("count", len(apiKeys)))
	return len(apiKeys), nil
}

func (m *Migrator) migrateChannels(ctx context.Context) (int, error) {
	log.Info(ctx, "migrating channels table")

	channels, err := m.src.Channel.Query().All(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to query channels: %w", err)
	}

	if m.options.DryRun {
		log.Info(ctx, "would migrate channels", log.Int("count", len(channels)))
		return len(channels), nil
	}

	for _, c := range channels {
		err := m.dst.Channel.Create().
			SetCreatedAt(c.CreatedAt).
			SetUpdatedAt(c.UpdatedAt).
			SetName(c.Name).
			SetType(c.Type).
			SetBaseURL(c.BaseURL).
			SetCredentials(c.Credentials).
			SetOrderingWeight(c.OrderingWeight).
			SetSupportedModels(c.SupportedModels).
			SetDefaultTestModel(c.DefaultTestModel).
			SetSettings(c.Settings).
			SetStatus(c.Status).
			OnConflict().
			UpdateNewValues().
			Exec(ctx)
		if err != nil {
			return 0, fmt.Errorf("failed to create channel: %w", err)
		}
	}

	log.Info(ctx, "migrated channels", log.Int("count", len(channels)))
	return len(channels), nil
}

func (m *Migrator) migrateDataStorages(ctx context.Context) (int, error) {
	log.Info(ctx, "migrating data_storages table")

	dataStorages, err := m.src.DataStorage.Query().All(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to query data_storages: %w", err)
	}

	if m.options.DryRun {
		log.Info(ctx, "would migrate data_storages", log.Int("count", len(dataStorages)))
		return len(dataStorages), nil
	}

	for _, ds := range dataStorages {
		err := m.dst.DataStorage.Create().
			SetCreatedAt(ds.CreatedAt).
			SetUpdatedAt(ds.UpdatedAt).
			SetName(ds.Name).
			SetDescription(ds.Description).
			SetType(ds.Type).
			SetStatus(ds.Status).
			SetSettings(ds.Settings).
			OnConflict().
			UpdateNewValues().
			Exec(ctx)
		if err != nil {
			return 0, fmt.Errorf("failed to create data_storage: %w", err)
		}
	}

	log.Info(ctx, "migrated data_storages", log.Int("count", len(dataStorages)))
	return len(dataStorages), nil
}

// newEntClient creates a new ent client for the given configuration.
func newEntClient(cfg Config, createSchema bool) (*ent.Client, error) {
	var (
		sqlDB     *sql.DB
		dbDialect string
		err       error
	)

	switch cfg.Dialect {
	case "postgres", "pgx", "postgresdb", "pg", "postgresql":
		sqlDB, err = sql.Open("pgx", cfg.DSN)
		if err != nil {
			return nil, err
		}
		dbDialect = dialect.Postgres
	case "sqlite3", "sqlite":
		sqlDB, err = sql.Open("sqlite3", cfg.DSN)
		if err != nil {
			return nil, err
		}
		dbDialect = dialect.SQLite
	case "mysql", "tidb":
		sqlDB, err = sql.Open("mysql", cfg.DSN)
		if err != nil {
			return nil, err
		}
		dbDialect = dialect.MySQL
	default:
		return nil, fmt.Errorf("invalid dialect: %s", cfg.Dialect)
	}

	// Test connection
	if err := sqlDB.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	drv := entsql.OpenDB(dbDialect, sqlDB)
	client := ent.NewClient(ent.Driver(drv))

	if createSchema {
		err = client.Schema.Create(
			context.Background(),
			migrate.WithGlobalUniqueID(false),
			migrate.WithForeignKeys(false),
			migrate.WithDropIndex(true),
			migrate.WithDropColumn(true),
			schema.WithHooks(schemahook.V0_3_0),
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create schema: %w", err)
		}
	}

	return client, nil
}

// ParseDialect parses a dialect string and returns the normalized dialect name.
func ParseDialect(d string) (string, error) {
	validDialects := []string{
		"postgres", "pgx", "postgresdb", "pg", "postgresql",
		"sqlite3", "sqlite",
		"mysql", "tidb",
	}

	if lo.Contains(validDialects, d) {
		return d, nil
	}

	return "", fmt.Errorf("invalid dialect: %s, valid options: %v", d, validDialects)
}
