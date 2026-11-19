package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // register the "pgx" driver
	"github.com/n8n-io/sandbox-service/internal/api/config"
)

const defaultLockPoolSize = 5

// pgUniqueViolation is the SQLSTATE of a unique constraint violation.
const pgUniqueViolation = "23505"

// PostgresStore wraps a *sql.DB backed by Postgres.
type PostgresStore struct {
	db     *sql.DB
	lockDB *sql.DB
}

// NewPostgres opens Postgres using cfg, runs schema migrations, and returns a ready store.
func NewPostgres(cfg config.PostgresConfig) (*PostgresStore, error) {
	db, err := sql.Open("pgx", cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("store: open postgres: %w", err)
	}

	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping postgres: %w", err)
	}

	if _, err := db.Exec(postgresSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: run sandboxes schema: %w", err)
	}
	if _, err := db.Exec(postgresAddTenantIDCol); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: add tenant_id column: %w", err)
	}
	if _, err := db.Exec(postgresBackfillAdminTenantID); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: backfill admin tenant_id: %w", err)
	}
	if _, err := db.Exec(postgresAddEphemeralCol); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: add ephemeral column: %w", err)
	}
	if _, err := db.Exec(postgresAddEgressCol); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: add egress column: %w", err)
	}
	if _, err := db.Exec(postgresAddCodeSandboxCols); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: add code sandbox columns: %w", err)
	}
	if _, err := db.Exec(postgresCommentCodeSandboxCols); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: comment code sandbox columns: %w", err)
	}
	if _, err := db.Exec(postgresSandboxCodeNameIndex); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: sandboxes code name index: %w", err)
	}
	if _, err := db.Exec(postgresAddCreateOptionCols); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: add create option columns: %w", err)
	}
	if _, err := db.Exec(postgresSandboxTenantIndex); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: sandboxes tenant index: %w", err)
	}
	if _, err := db.Exec(postgresRunnersSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: run runners schema: %w", err)
	}
	if _, err := db.Exec(postgresAddRunnerStateCols); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: add runner state columns: %w", err)
	}
	if _, err := db.Exec(postgresTenantsSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: run tenants schema: %w", err)
	}
	if _, err := db.Exec(postgresAddTenantMaxIdleWindowCol); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: add tenant max_idle_window: %w", err)
	}

	lockDB, err := sql.Open("pgx", cfg.DSN())
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: open postgres lock pool: %w", err)
	}
	lockPoolSize := cfg.LockPoolSize
	if lockPoolSize <= 0 {
		lockPoolSize = defaultLockPoolSize
	}
	lockDB.SetMaxOpenConns(lockPoolSize)
	lockDB.SetMaxIdleConns(1)
	lockDB.SetConnMaxLifetime(30 * time.Minute)
	if err := lockDB.Ping(); err != nil {
		_ = lockDB.Close()
		_ = db.Close()
		return nil, fmt.Errorf("store: ping postgres lock pool: %w", err)
	}

	slog.Debug("store: opened postgres database", "host", cfg.Host, "db", cfg.Database)
	return &PostgresStore{db: db, lockDB: lockDB}, nil
}

// DB returns the underlying database handle (for registry and sweeper lock).
func (s *PostgresStore) DB() *sql.DB { return s.db }

func (s *PostgresStore) Backend() Backend { return BackendPostgres }

func (s *PostgresStore) Close() error { return errors.Join(s.db.Close(), s.lockDB.Close()) }

// idleSweepLockKey is the Postgres advisory lock key for the idle sandbox sweeper.
const idleSweepLockKey int64 = 8675309

// TryRun acquires a session advisory lock on a dedicated connection, runs fn if the
// lock is granted, then releases the lock. Returns (false, nil) when another holder
// has the lock.
func TryRun(ctx context.Context, db *sql.DB, fn func() error) (ran bool, err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return false, fmt.Errorf("store: acquire conn for advisory lock: %w", err)
	}
	defer conn.Close()

	var locked bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, idleSweepLockKey).Scan(&locked); err != nil {
		return false, fmt.Errorf("store: try advisory lock: %w", err)
	}
	if !locked {
		return false, nil
	}

	defer func() {
		_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, idleSweepLockKey)
	}()

	if err := fn(); err != nil {
		return true, err
	}
	return true, nil
}
