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
	if _, err := db.Exec(postgresAddTenantIDCol); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: add tenant_id column: %w", err)
	}
	if _, err := db.Exec(postgresSandboxTenantIndex); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: sandboxes tenant index: %w", err)
	}
	if _, err := db.Exec(postgresRunnersSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: run runners schema: %w", err)
	}
	if _, err := db.Exec(postgresTenantsSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: run tenants schema: %w", err)
	}

	slog.Debug("store: opened postgres database", "host", cfg.Host, "db", cfg.Database)
	return &PostgresStore{db: db}, nil
}

// DB returns the underlying database handle (for registry and sweeper lock).
func (s *PostgresStore) DB() *sql.DB { return s.db }

func (s *PostgresStore) Backend() Backend { return BackendPostgres }

func (s *PostgresStore) Close() error { return s.db.Close() }

const pgSandboxCols = `id, status, created_at, last_active_at, rootfs_path, socket_path, container_ip, daemon_port, runner_id, runner_http_base_url, runner_control_grpc_addr, tenant_id`

func (s *PostgresStore) Create(record *SandboxRecord) error {
	const q = `
		INSERT INTO sandboxes
			(id, status, created_at, last_active_at, rootfs_path, socket_path, container_ip, daemon_port, runner_id, runner_http_base_url, runner_control_grpc_addr, tenant_id)
		VALUES
			($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`

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
		record.TenantID,
	)
	if err != nil {
		return fmt.Errorf("store: create sandbox %s: %w", record.ID, err)
	}
	return nil
}

func (s *PostgresStore) Get(id string) (*SandboxRecord, error) {
	q := `SELECT ` + pgSandboxCols + ` FROM sandboxes WHERE id = $1`
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
	q := `SELECT ` + pgSandboxCols + `
		FROM sandboxes
		WHERE status = 'stopped'
		  AND last_active_at <= $1`
	return s.querySandboxRecords(q, cutoff)
}

func (s *PostgresStore) ListForIdleReapStop(cutoff int64) ([]*SandboxRecord, error) {
	q := `SELECT ` + pgSandboxCols + `
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

func (s *PostgresStore) CountByTenant(tenantID string) (int64, error) {
	var n int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sandboxes WHERE tenant_id = $1`, tenantID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count sandboxes by tenant: %w", err)
	}
	return n, nil
}

func (s *PostgresStore) List() ([]*SandboxRecord, error) {
	q := `SELECT ` + pgSandboxCols + ` FROM sandboxes ORDER BY created_at DESC`
	return s.querySandboxRecords(q)
}

func (s *PostgresStore) ListByTenant(tenantID string) ([]*SandboxRecord, error) {
	q := `SELECT ` + pgSandboxCols + ` FROM sandboxes WHERE tenant_id = $1 ORDER BY created_at DESC`
	return s.querySandboxRecords(q, tenantID)
}

func (s *PostgresStore) CreateTenant(t *Tenant) error {
	const q = `INSERT INTO tenants (id, name, external_ref, max_sandboxes, created_at) VALUES ($1, $2, $3, $4, $5)`
	_, err := s.db.Exec(q, t.ID, t.Name, t.ExternalRef, t.MaxSandboxes, t.CreatedAt)
	if err != nil {
		return fmt.Errorf("store: create tenant %s: %w", t.ID, err)
	}
	return nil
}

func (s *PostgresStore) GetTenant(id string) (*Tenant, error) {
	row := s.db.QueryRow(`SELECT id, name, external_ref, max_sandboxes, created_at FROM tenants WHERE id = $1`, id)
	t, err := scanTenant(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: get tenant %s: %w", id, err)
	}
	return t, nil
}

func (s *PostgresStore) ListTenants() ([]*Tenant, error) {
	rows, err := s.db.Query(`SELECT id, name, external_ref, max_sandboxes, created_at FROM tenants ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: list tenants: %w", err)
	}
	defer rows.Close()
	var out []*Tenant
	for rows.Next() {
		t, err := scanTenant(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list tenants scan: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *PostgresStore) DeleteTenant(id string) error {
	if _, err := s.db.Exec(`DELETE FROM api_keys WHERE tenant_id = $1`, id); err != nil {
		return fmt.Errorf("store: delete tenant keys %s: %w", id, err)
	}
	if _, err := s.db.Exec(`DELETE FROM tenants WHERE id = $1`, id); err != nil {
		return fmt.Errorf("store: delete tenant %s: %w", id, err)
	}
	return nil
}

func (s *PostgresStore) CreateAPIKey(k *APIKey) error {
	var revoked any
	if k.RevokedAt > 0 {
		revoked = k.RevokedAt
	}
	const q = `INSERT INTO api_keys (id, tenant_id, key_hash, prefix, created_at, revoked_at) VALUES ($1, $2, $3, $4, $5, $6)`
	_, err := s.db.Exec(q, k.ID, k.TenantID, k.KeyHash, k.Prefix, k.CreatedAt, revoked)
	if err != nil {
		return fmt.Errorf("store: create api key %s: %w", k.ID, err)
	}
	return nil
}

func (s *PostgresStore) GetAPIKey(id string) (*APIKey, error) {
	row := s.db.QueryRow(`SELECT id, tenant_id, key_hash, prefix, created_at, revoked_at FROM api_keys WHERE id = $1`, id)
	k, err := scanAPIKey(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: get api key %s: %w", id, err)
	}
	return k, nil
}

func (s *PostgresStore) ListAPIKeysByTenant(tenantID string) ([]*APIKey, error) {
	rows, err := s.db.Query(`SELECT id, tenant_id, key_hash, prefix, created_at, revoked_at FROM api_keys WHERE tenant_id = $1 ORDER BY created_at DESC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: list api keys: %w", err)
	}
	defer rows.Close()
	return scanAPIKeyRows(rows)
}

func (s *PostgresStore) ListActiveAPIKeysByPrefix(prefix string) ([]*APIKey, error) {
	rows, err := s.db.Query(`SELECT id, tenant_id, key_hash, prefix, created_at, revoked_at FROM api_keys WHERE prefix = $1 AND revoked_at IS NULL`, prefix)
	if err != nil {
		return nil, fmt.Errorf("store: list api keys by prefix: %w", err)
	}
	defer rows.Close()
	return scanAPIKeyRows(rows)
}

func (s *PostgresStore) RevokeAPIKey(id string) error {
	now := time.Now().Unix()
	if _, err := s.db.Exec(`UPDATE api_keys SET revoked_at = $1 WHERE id = $2 AND revoked_at IS NULL`, now, id); err != nil {
		return fmt.Errorf("store: revoke api key %s: %w", id, err)
	}
	return nil
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
