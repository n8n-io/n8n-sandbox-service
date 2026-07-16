package store

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	_ "modernc.org/sqlite" // register the "sqlite" driver
)

// SQLiteStore wraps a *sql.DB and exposes CRUD operations for SandboxRecord rows.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLite opens (or creates) the SQLite database at dbPath, runs schema migrations,
// and returns a ready store. Use ":memory:" for an in-process ephemeral database.
func NewSQLite(dbPath string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("store: open db %s: %w", dbPath, err)
	}

	// SQLite performs best with a single writer connection; cap the pool.
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping db: %w", err)
	}

	if _, err := db.Exec(sqliteSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: run migrations: %w", err)
	}

	for _, stmt := range []struct {
		sql  string
		name string
	}{
		{sql: sqliteAddContainerIPCol, name: "container_ip"},
		{sql: sqliteAddDaemonPortCol, name: "daemon_port"},
		{sql: sqliteDropContainerIDCol, name: "drop_container_id"},
		{sql: sqliteAddRunnerIDCol, name: "runner_id"},
		{sql: sqliteAddRunnerHTTPBaseURLCol, name: "runner_http_base_url"},
		{sql: sqliteAddRunnerControlGRPCAddrCol, name: "runner_control_grpc_addr"},
		{sql: sqliteAddTenantIDCol, name: "tenant_id"},
	} {
		if _, err := db.Exec(stmt.sql); err != nil {
			if strings.Contains(err.Error(), "duplicate column") ||
				strings.Contains(err.Error(), "no such column") {
				continue
			}
			_ = db.Close()
			return nil, fmt.Errorf("store: migration %s: %w", stmt.name, err)
		}
	}

	if _, err := db.Exec(sqliteTenantsSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: tenants schema: %w", err)
	}

	// FK enforcement for api_keys → tenants
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: enable foreign_keys: %w", err)
	}

	slog.Debug("store: opened sqlite database", "path", dbPath)
	return &SQLiteStore{db: db}, nil
}

// New opens SQLite at dbPath. It is an alias for NewSQLite for backward compatibility.
func New(dbPath string) (*SQLiteStore, error) {
	return NewSQLite(dbPath)
}

func (s *SQLiteStore) Backend() Backend { return BackendSQLite }

func (s *SQLiteStore) Close() error { return s.db.Close() }

const sqliteSandboxCols = `id, status, created_at, last_active_at, rootfs_path, socket_path, container_ip, daemon_port, runner_id, runner_http_base_url, runner_control_grpc_addr, tenant_id`

func (s *SQLiteStore) Create(record *SandboxRecord) error {
	const q = `
		INSERT INTO sandboxes
			(id, status, created_at, last_active_at, rootfs_path, socket_path, container_ip, daemon_port, runner_id, runner_http_base_url, runner_control_grpc_addr, tenant_id)
		VALUES
			(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

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

func (s *SQLiteStore) Get(id string) (*SandboxRecord, error) {
	q := `SELECT ` + sqliteSandboxCols + ` FROM sandboxes WHERE id = ?`
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

func (s *SQLiteStore) UpdateStatus(id, status string) error {
	const q = `UPDATE sandboxes SET status = ? WHERE id = ?`
	if _, err := s.db.Exec(q, status, id); err != nil {
		return fmt.Errorf("store: update status for %s: %w", id, err)
	}
	return nil
}

func (s *SQLiteStore) UpdateLastActive(id string) error {
	const q = `UPDATE sandboxes SET last_active_at = ? WHERE id = ?`
	now := time.Now().Unix()
	if _, err := s.db.Exec(q, now, id); err != nil {
		return fmt.Errorf("store: update last_active_at for %s: %w", id, err)
	}
	return nil
}

func (s *SQLiteStore) Delete(id string) error {
	const q = `DELETE FROM sandboxes WHERE id = ?`
	if _, err := s.db.Exec(q, id); err != nil {
		return fmt.Errorf("store: delete sandbox %s: %w", id, err)
	}
	return nil
}

func (s *SQLiteStore) ListForIdleReapDelete(cutoff int64) ([]*SandboxRecord, error) {
	q := `SELECT ` + sqliteSandboxCols + `
		FROM sandboxes
		WHERE status = 'stopped'
		  AND last_active_at <= ?`
	return s.querySandboxRecords(q, cutoff)
}

func (s *SQLiteStore) ListForIdleReapStop(cutoff int64) ([]*SandboxRecord, error) {
	q := `SELECT ` + sqliteSandboxCols + `
		FROM sandboxes
		WHERE status = 'running'
		  AND last_active_at <= ?`
	return s.querySandboxRecords(q, cutoff)
}

func (s *SQLiteStore) querySandboxRecords(q string, args ...any) ([]*SandboxRecord, error) {
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

func (s *SQLiteStore) Count() (int64, error) {
	var n int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sandboxes`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count sandboxes: %w", err)
	}
	return n, nil
}

func (s *SQLiteStore) CountByTenant(tenantID string) (int64, error) {
	var n int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sandboxes WHERE tenant_id = ?`, tenantID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count sandboxes by tenant: %w", err)
	}
	return n, nil
}

func (s *SQLiteStore) List() ([]*SandboxRecord, error) {
	q := `SELECT ` + sqliteSandboxCols + ` FROM sandboxes ORDER BY created_at DESC`
	return s.querySandboxRecords(q)
}

func (s *SQLiteStore) ListByTenant(tenantID string) ([]*SandboxRecord, error) {
	q := `SELECT ` + sqliteSandboxCols + ` FROM sandboxes WHERE tenant_id = ? ORDER BY created_at DESC`
	return s.querySandboxRecords(q, tenantID)
}

func (s *SQLiteStore) CreateTenant(t *Tenant) error {
	const q = `INSERT INTO tenants (id, name, external_ref, max_sandboxes, created_at) VALUES (?, ?, ?, ?, ?)`
	_, err := s.db.Exec(q, t.ID, t.Name, t.ExternalRef, t.MaxSandboxes, t.CreatedAt)
	if err != nil {
		return fmt.Errorf("store: create tenant %s: %w", t.ID, err)
	}
	return nil
}

func (s *SQLiteStore) GetTenant(id string) (*Tenant, error) {
	row := s.db.QueryRow(`SELECT id, name, external_ref, max_sandboxes, created_at FROM tenants WHERE id = ?`, id)
	t, err := scanTenant(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: get tenant %s: %w", id, err)
	}
	return t, nil
}

func (s *SQLiteStore) ListTenants() ([]*Tenant, error) {
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

func (s *SQLiteStore) DeleteTenant(id string) error {
	if _, err := s.db.Exec(`DELETE FROM api_keys WHERE tenant_id = ?`, id); err != nil {
		return fmt.Errorf("store: delete tenant keys %s: %w", id, err)
	}
	if _, err := s.db.Exec(`DELETE FROM tenants WHERE id = ?`, id); err != nil {
		return fmt.Errorf("store: delete tenant %s: %w", id, err)
	}
	return nil
}

func (s *SQLiteStore) CreateAPIKey(k *APIKey) error {
	var revoked any
	if k.RevokedAt > 0 {
		revoked = k.RevokedAt
	}
	const q = `INSERT INTO api_keys (id, tenant_id, key_hash, prefix, created_at, revoked_at) VALUES (?, ?, ?, ?, ?, ?)`
	_, err := s.db.Exec(q, k.ID, k.TenantID, k.KeyHash, k.Prefix, k.CreatedAt, revoked)
	if err != nil {
		return fmt.Errorf("store: create api key %s: %w", k.ID, err)
	}
	return nil
}

func (s *SQLiteStore) GetAPIKey(id string) (*APIKey, error) {
	row := s.db.QueryRow(`SELECT id, tenant_id, key_hash, prefix, created_at, revoked_at FROM api_keys WHERE id = ?`, id)
	k, err := scanAPIKey(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: get api key %s: %w", id, err)
	}
	return k, nil
}

func (s *SQLiteStore) ListAPIKeysByTenant(tenantID string) ([]*APIKey, error) {
	rows, err := s.db.Query(`SELECT id, tenant_id, key_hash, prefix, created_at, revoked_at FROM api_keys WHERE tenant_id = ? ORDER BY created_at DESC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: list api keys: %w", err)
	}
	defer rows.Close()
	return scanAPIKeyRows(rows)
}

func (s *SQLiteStore) ListActiveAPIKeysByPrefix(prefix string) ([]*APIKey, error) {
	rows, err := s.db.Query(`SELECT id, tenant_id, key_hash, prefix, created_at, revoked_at FROM api_keys WHERE prefix = ? AND revoked_at IS NULL`, prefix)
	if err != nil {
		return nil, fmt.Errorf("store: list api keys by prefix: %w", err)
	}
	defer rows.Close()
	return scanAPIKeyRows(rows)
}

func (s *SQLiteStore) RevokeAPIKey(id string) error {
	now := time.Now().Unix()
	if _, err := s.db.Exec(`UPDATE api_keys SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, now, id); err != nil {
		return fmt.Errorf("store: revoke api key %s: %w", id, err)
	}
	return nil
}

func scanAPIKeyRows(rows *sql.Rows) ([]*APIKey, error) {
	var out []*APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan api key: %w", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
