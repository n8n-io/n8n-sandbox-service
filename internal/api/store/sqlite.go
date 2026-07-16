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

	slog.Debug("store: opened sqlite database", "path", dbPath)
	return &SQLiteStore{db: db}, nil
}

// New opens SQLite at dbPath. It is an alias for NewSQLite for backward compatibility.
func New(dbPath string) (*SQLiteStore, error) {
	return NewSQLite(dbPath)
}

func (s *SQLiteStore) Backend() Backend { return BackendSQLite }

func (s *SQLiteStore) Close() error { return s.db.Close() }

func (s *SQLiteStore) Create(record *SandboxRecord) error {
	const q = `
		INSERT INTO sandboxes
			(id, status, created_at, last_active_at, rootfs_path, socket_path, container_ip, daemon_port, runner_id, runner_http_base_url, runner_control_grpc_addr)
		VALUES
			(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

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

func (s *SQLiteStore) Get(id string) (*SandboxRecord, error) {
	const q = `
		SELECT id, status, created_at, last_active_at, rootfs_path, socket_path, container_ip, daemon_port, runner_id, runner_http_base_url, runner_control_grpc_addr
		FROM sandboxes
		WHERE id = ?`

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
	const q = `
		SELECT id, status, created_at, last_active_at, rootfs_path, socket_path, container_ip, daemon_port, runner_id, runner_http_base_url, runner_control_grpc_addr
		FROM sandboxes
		WHERE status = 'stopped'
		  AND last_active_at <= ?`
	return s.querySandboxRecords(q, cutoff)
}

func (s *SQLiteStore) ListForIdleReapStop(cutoff int64) ([]*SandboxRecord, error) {
	const q = `
		SELECT id, status, created_at, last_active_at, rootfs_path, socket_path, container_ip, daemon_port, runner_id, runner_http_base_url, runner_control_grpc_addr
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

func (s *SQLiteStore) List() ([]*SandboxRecord, error) {
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
