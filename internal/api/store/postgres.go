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

// PostgresStore wraps a *sql.DB backed by Postgres.
type PostgresStore struct {
	db *sql.DB
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
	if _, err := db.Exec(postgresRunnersSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: run runners schema: %w", err)
	}

	slog.Debug("store: opened postgres database", "host", cfg.Host, "db", cfg.Database)
	return &PostgresStore{db: db}, nil
}

// DB returns the underlying database handle (for registry and sweeper lock).
func (s *PostgresStore) DB() *sql.DB { return s.db }

func (s *PostgresStore) Backend() Backend { return BackendPostgres }

func (s *PostgresStore) Close() error { return s.db.Close() }

func (s *PostgresStore) Create(record *SandboxRecord) error {
	const q = `
		INSERT INTO sandboxes
			(id, status, created_at, last_active_at, rootfs_path, socket_path, container_ip, daemon_port, runner_id, runner_http_base_url, runner_control_grpc_addr)
		VALUES
			($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`

	_, err := s.db.Exec(q,
		record.ID,
		record.Status,
		record.CreatedAt,
		record.LastActiveAt,
		record.RootfsPath,
		record.SocketPath,
		record.ContainerIP,
		record.DaemonPort,
		record.RunnerID,
		record.RunnerHTTPBase,
		record.RunnerControlGRPCAddr,
	)
	if err != nil {
		return fmt.Errorf("store: create sandbox %s: %w", record.ID, err)
	}
	return nil
}

func (s *PostgresStore) Get(id string) (*SandboxRecord, error) {
	const q = `
		SELECT id, status, created_at, last_active_at, rootfs_path, socket_path, container_ip, daemon_port, runner_id, runner_http_base_url, runner_control_grpc_addr
		FROM sandboxes
		WHERE id = $1`

	row := s.db.QueryRow(q, id)
	rec, err := scanRecord(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: get sandbox %s: %w", id, err)
	}
	return rec, nil
}

func (s *PostgresStore) UpdateStatus(id, status string) error {
	const q = `UPDATE sandboxes SET status = $1 WHERE id = $2`
	if _, err := s.db.Exec(q, status, id); err != nil {
		return fmt.Errorf("store: update status for %s: %w", id, err)
	}
	return nil
}

func (s *PostgresStore) UpdateLastActive(id string) error {
	const q = `UPDATE sandboxes SET last_active_at = $1 WHERE id = $2`
	now := time.Now().Unix()
	if _, err := s.db.Exec(q, now, id); err != nil {
		return fmt.Errorf("store: update last_active_at for %s: %w", id, err)
	}
	return nil
}

func (s *PostgresStore) Delete(id string) error {
	const q = `DELETE FROM sandboxes WHERE id = $1`
	if _, err := s.db.Exec(q, id); err != nil {
		return fmt.Errorf("store: delete sandbox %s: %w", id, err)
	}
	return nil
}

func (s *PostgresStore) ListForIdleReapDelete(cutoff int64) ([]*SandboxRecord, error) {
	const q = `
		SELECT id, status, created_at, last_active_at, rootfs_path, socket_path, container_ip, daemon_port, runner_id, runner_http_base_url, runner_control_grpc_addr
		FROM sandboxes
		WHERE status = 'stopped'
		  AND last_active_at <= $1`
	return s.querySandboxRecords(q, cutoff)
}

func (s *PostgresStore) ListForIdleReapStop(cutoff int64) ([]*SandboxRecord, error) {
	const q = `
		SELECT id, status, created_at, last_active_at, rootfs_path, socket_path, container_ip, daemon_port, runner_id, runner_http_base_url, runner_control_grpc_addr
		FROM sandboxes
		WHERE status = 'running'
		  AND last_active_at <= $1`
	return s.querySandboxRecords(q, cutoff)
}

func (s *PostgresStore) querySandboxRecords(q string, args ...any) ([]*SandboxRecord, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query sandboxes: %w", err)
	}
	defer rows.Close()

	var records []*SandboxRecord
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("store: query sandboxes scan: %w", err)
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: query sandboxes rows: %w", err)
	}
	return records, nil
}

func (s *PostgresStore) Count() (int64, error) {
	var n int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sandboxes`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count sandboxes: %w", err)
	}
	return n, nil
}

func (s *PostgresStore) List() ([]*SandboxRecord, error) {
	const q = `
		SELECT id, status, created_at, last_active_at, rootfs_path, socket_path, container_ip, daemon_port, runner_id, runner_http_base_url, runner_control_grpc_addr
		FROM sandboxes
		ORDER BY created_at DESC`

	rows, err := s.db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("store: list sandboxes: %w", err)
	}
	defer rows.Close()

	var records []*SandboxRecord
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list sandboxes scan: %w", err)
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list sandboxes rows: %w", err)
	}
	return records, nil
}

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
