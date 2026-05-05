// Package store provides a SQLite-backed persistent store for sandbox records.
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

// SandboxRecord is the in-memory representation of a row in the sandboxes table.
type SandboxRecord struct {
	ID             string
	Status         string
	CreatedAt      int64
	LastActiveAt   int64
	RootfsPath     string
	SocketPath     string
	ContainerIP    string
	DaemonPort     int
	RunnerID       string // Runner that hosts this sandbox (from registration)
	RunnerHTTPBase string // Base URL to reach that runner's HTTP API (for proxying)
}

// Store wraps a *sql.DB and exposes CRUD operations for SandboxRecord rows.
type Store struct {
	db *sql.DB
}

// New opens (or creates) the SQLite database at dbPath, runs the schema
// migrations, and returns a ready Store. Use ":memory:" for an in-process
// ephemeral database (useful in tests).
func New(dbPath string) (*Store, error) {
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

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: run migrations: %w", err)
	}

	for _, stmt := range []struct {
		sql  string
		name string
	}{
		{sql: addContainerIPCol, name: "container_ip"},
		{sql: addDaemonPortCol, name: "daemon_port"},
		{sql: dropContainerIDCol, name: "drop_container_id"},
		{sql: addRunnerIDCol, name: "runner_id"},
		{sql: addRunnerHTTPBaseURLCol, name: "runner_http_base_url"},
	} {
		if _, err := db.Exec(stmt.sql); err != nil {
			// Ignore expected errors for idempotent migrations
			if strings.Contains(err.Error(), "duplicate column") ||
				strings.Contains(err.Error(), "no such column") {
				continue
			}
			_ = db.Close()
			return nil, fmt.Errorf("store: migration %s: %w", stmt.name, err)
		}
	}

	slog.Debug("store: opened database", "path", dbPath)
	return &Store{db: db}, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// Create inserts a new SandboxRecord. Returns an error if a record with the
// same ID already exists.
func (s *Store) Create(record *SandboxRecord) error {
	const q = `
		INSERT INTO sandboxes
			(id, status, created_at, last_active_at, rootfs_path, socket_path, container_ip, daemon_port, runner_id, runner_http_base_url)
		VALUES
			(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

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
	)
	if err != nil {
		return fmt.Errorf("store: create sandbox %s: %w", record.ID, err)
	}
	return nil
}

// Get returns the SandboxRecord with the given id, or (nil, nil) if no such
// record exists.
func (s *Store) Get(id string) (*SandboxRecord, error) {
	const q = `
		SELECT id, status, created_at, last_active_at, rootfs_path, socket_path, container_ip, daemon_port, runner_id, runner_http_base_url
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

// UpdateStatus sets the status column for the sandbox with the given id.
func (s *Store) UpdateStatus(id, status string) error {
	const q = `UPDATE sandboxes SET status = ? WHERE id = ?`
	if _, err := s.db.Exec(q, status, id); err != nil {
		return fmt.Errorf("store: update status for %s: %w", id, err)
	}
	return nil
}

// UpdateLastActive sets last_active_at to the current Unix timestamp (seconds)
// for the sandbox with the given id.
func (s *Store) UpdateLastActive(id string) error {
	const q = `UPDATE sandboxes SET last_active_at = ? WHERE id = ?`
	now := time.Now().Unix()
	if _, err := s.db.Exec(q, now, id); err != nil {
		return fmt.Errorf("store: update last_active_at for %s: %w", id, err)
	}
	return nil
}

// Delete removes the sandbox record with the given id from the database.
func (s *Store) Delete(id string) error {
	const q = `DELETE FROM sandboxes WHERE id = ?`
	if _, err := s.db.Exec(q, id); err != nil {
		return fmt.Errorf("store: delete sandbox %s: %w", id, err)
	}
	return nil
}

// List returns all sandbox records, ordered by creation time descending.
func (s *Store) List() ([]*SandboxRecord, error) {
	const q = `
		SELECT id, status, created_at, last_active_at, rootfs_path, socket_path, container_ip, daemon_port, runner_id, runner_http_base_url
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

// MarkAllTerminated sets status = "terminated" for every sandbox whose status
// is not already "terminated". This is called on startup to reconcile any
// sandboxes that were running when the process previously exited.
func (s *Store) MarkAllTerminated() error {
	const q = `UPDATE sandboxes SET status = 'terminated' WHERE status != 'terminated'`
	res, err := s.db.Exec(q)
	if err != nil {
		return fmt.Errorf("store: mark all terminated: %w", err)
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		slog.Info("store: marked stale sandboxes as terminated", "count", n)
	}
	return nil
}

// ListStale returns all sandboxes whose last_active_at is older than
// (now - maxAge) seconds and whose status is not "terminated".
func (s *Store) ListStale(maxAge int64) ([]*SandboxRecord, error) {
	const q = `
		SELECT id, status, created_at, last_active_at, rootfs_path, socket_path, container_ip, daemon_port, runner_id, runner_http_base_url
		FROM sandboxes
		WHERE last_active_at < ?
		  AND status != 'terminated'`

	cutoff := time.Now().Unix() - maxAge
	rows, err := s.db.Query(q, cutoff)
	if err != nil {
		return nil, fmt.Errorf("store: list stale: %w", err)
	}
	defer rows.Close()

	var records []*SandboxRecord
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list stale scan: %w", err)
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list stale rows: %w", err)
	}
	return records, nil
}

// scanner is the common interface satisfied by both *sql.Row and *sql.Rows,
// allowing scanRecord to be used with both.
type scanner interface {
	Scan(dest ...any) error
}

// scanRecord reads a single sandbox row from a scanner into a SandboxRecord.
func scanRecord(row scanner) (*SandboxRecord, error) {
	var r SandboxRecord
	err := row.Scan(
		&r.ID,
		&r.Status,
		&r.CreatedAt,
		&r.LastActiveAt,
		&r.RootfsPath,
		&r.SocketPath,
		&r.ContainerIP,
		&r.DaemonPort,
		&r.RunnerID,
		&r.RunnerHTTPBase,
	)
	if err != nil {
		return nil, err
	}
	return &r, nil
}
