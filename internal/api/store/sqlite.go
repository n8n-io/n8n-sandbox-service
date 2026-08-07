package store

import (
	"context"
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
	db, err := sql.Open("sqlite", sqliteDSN(dbPath))
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

	if _, err := db.Exec(sqliteBackfillAdminTenantID); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: backfill admin tenant_id: %w", err)
	}

	if _, err := db.Exec(sqliteTenantsSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: tenants schema: %w", err)
	}

	slog.Debug("store: opened sqlite database", "path", dbPath)
	return &SQLiteStore{db: db}, nil
}

// sqliteDSN adds modernc _pragma flags so every pooled connection gets them.
// A one-shot Exec("PRAGMA …") only affects the connection that ran it.
func sqliteDSN(dbPath string) string {
	const fk = "_pragma=foreign_keys(1)"
	if dbPath == ":memory:" {
		return ":memory:?" + fk
	}
	if strings.Contains(dbPath, "?") {
		return dbPath + "&" + fk
	}
	return dbPath + "?" + fk
}

// New opens SQLite at dbPath. It is an alias for NewSQLite for backward compatibility.
func New(dbPath string) (*SQLiteStore, error) {
	return NewSQLite(dbPath)
}

func (s *SQLiteStore) Backend() Backend { return BackendSQLite }

func (s *SQLiteStore) Close() error { return s.db.Close() }

const sqliteSandboxCols = `id, status, created_at, last_active_at, rootfs_path, socket_path, container_ip, daemon_port, runner_id, runner_http_base_url, runner_control_grpc_addr, tenant_id`

type sqliteExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func (s *SQLiteStore) insertSandbox(e sqliteExecer, record *SandboxRecord) error {
	const q = `
		INSERT INTO sandboxes
			(id, status, created_at, last_active_at, rootfs_path, socket_path, container_ip, daemon_port, runner_id, runner_http_base_url, runner_control_grpc_addr, tenant_id)
		VALUES
			(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	_, err := e.ExecContext(context.Background(), q,
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

// beginImmediate starts a transaction with a RESERVED write lock (BEGIN IMMEDIATE).
// Caller must Commit or Rollback, then Close the conn.
func (s *SQLiteStore) beginImmediate() (*sql.Conn, error) {
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func commitConn(conn *sql.Conn) error {
	_, err := conn.ExecContext(context.Background(), "COMMIT")
	cerr := conn.Close()
	if err != nil {
		return err
	}
	return cerr
}

func rollbackConn(conn *sql.Conn) {
	_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
	_ = conn.Close()
}

func (s *SQLiteStore) Create(record *SandboxRecord) error {
	if IsAdminTenantID(record.TenantID) {
		return s.insertSandbox(s.db, record)
	}

	// BEGIN IMMEDIATE write-locks the DB so DeleteTenant cannot interleave
	// after its emptiness check.
	conn, err := s.beginImmediate()
	if err != nil {
		return fmt.Errorf("store: begin create sandbox: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			rollbackConn(conn)
		}
	}()

	var tenantID string
	err = conn.QueryRowContext(context.Background(), `SELECT id FROM tenants WHERE id = ?`, record.TenantID).Scan(&tenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrTenantNotFound
	}
	if err != nil {
		return fmt.Errorf("store: get tenant %s: %w", record.TenantID, err)
	}
	if err := s.insertSandbox(conn, record); err != nil {
		return err
	}
	if err := commitConn(conn); err != nil {
		return fmt.Errorf("store: commit create sandbox: %w", err)
	}
	committed = true
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

func (s *SQLiteStore) CreateTenantWithAPIKey(t *Tenant, k *APIKey) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin tenant+key: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const tenantQ = `INSERT INTO tenants (id, name, external_ref, max_sandboxes, created_at) VALUES (?, ?, ?, ?, ?)`
	if _, err := tx.Exec(tenantQ, t.ID, t.Name, t.ExternalRef, t.MaxSandboxes, t.CreatedAt); err != nil {
		return fmt.Errorf("store: create tenant %s: %w", t.ID, err)
	}
	k.TenantID = t.ID
	var revoked any
	if k.RevokedAt > 0 {
		revoked = k.RevokedAt
	}
	const keyQ = `INSERT INTO api_keys (id, tenant_id, key_hash, prefix, created_at, revoked_at) VALUES (?, ?, ?, ?, ?, ?)`
	if _, err := tx.Exec(keyQ, k.ID, k.TenantID, k.KeyHash, k.Prefix, k.CreatedAt, revoked); err != nil {
		return fmt.Errorf("store: create api key %s: %w", k.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit tenant+key: %w", err)
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
	// BEGIN IMMEDIATE write-locks the DB so a concurrent Create cannot insert
	// a sandbox after the emptiness check.
	conn, err := s.beginImmediate()
	if err != nil {
		return fmt.Errorf("store: begin delete tenant: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			rollbackConn(conn)
		}
	}()

	var tenantID string
	err = conn.QueryRowContext(context.Background(), `SELECT id FROM tenants WHERE id = ?`, id).Scan(&tenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: get tenant %s: %w", id, err)
	}

	var n int64
	if err := conn.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM sandboxes WHERE tenant_id = ?`, id).Scan(&n); err != nil {
		return fmt.Errorf("store: count sandboxes by tenant: %w", err)
	}
	if n > 0 {
		return ErrTenantHasSandboxes
	}
	// api_keys cascade via ON DELETE CASCADE (foreign_keys enabled via DSN _pragma)
	if _, err := conn.ExecContext(context.Background(), `DELETE FROM tenants WHERE id = ?`, id); err != nil {
		return fmt.Errorf("store: delete tenant %s: %w", id, err)
	}
	if err := commitConn(conn); err != nil {
		return fmt.Errorf("store: commit delete tenant: %w", err)
	}
	committed = true
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
