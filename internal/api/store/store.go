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

// ErrImageStepsHashConflict is returned when an image with the same steps hash already exists.
var ErrImageStepsHashConflict = errors.New("image steps hash already exists")

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
	ImageID        string
	NetworkPolicy  string // JSON-serialized NetworkPolicy
	ResourceLimits string // JSON-serialized ResourceLimits
	RunnerID       string // Runner that hosts this sandbox (from registration)
	RunnerHTTPBase string // Base URL to reach that runner's HTTP API (for proxying)
}

// ImageRecord is the in-memory representation of a row in the images table.
type ImageRecord struct {
	ID              string
	Tag             string
	BaseImage       string
	DockerImageID   string
	StepsHash       string
	DockerfileSteps string // JSON-serialized []string
	CreatedAt       int64
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

	// Add resource_limits column if upgrading from an older schema.
	if _, err := db.Exec(addResourceLimitsCol); err != nil {
		// Ignore "duplicate column" errors — column already exists.
		if !strings.Contains(err.Error(), "duplicate column") {
			_ = db.Close()
			return nil, fmt.Errorf("store: add resource_limits column: %w", err)
		}
	}
	for _, stmt := range []struct {
		sql  string
		name string
	}{
		{sql: addContainerIPCol, name: "container_ip"},
		{sql: addDaemonPortCol, name: "daemon_port"},
		{sql: addImageIDCol, name: "image_id"},
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
			(id, status, created_at, last_active_at, rootfs_path, socket_path, container_ip, daemon_port, image_id, network_policy, resource_limits, runner_id, runner_http_base_url)
		VALUES
			(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	_, err := s.db.Exec(q,
		record.ID,
		record.Status,
		record.CreatedAt,
		record.LastActiveAt,
		record.RootfsPath,
		record.SocketPath,
		record.ContainerIP,
		record.DaemonPort,
		record.ImageID,
		record.NetworkPolicy,
		record.ResourceLimits,
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
		SELECT id, status, created_at, last_active_at, rootfs_path, socket_path, container_ip, daemon_port, image_id, network_policy, resource_limits, runner_id, runner_http_base_url
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
		SELECT id, status, created_at, last_active_at, rootfs_path, socket_path, container_ip, daemon_port, image_id, network_policy, resource_limits, runner_id, runner_http_base_url
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
		SELECT id, status, created_at, last_active_at, rootfs_path, socket_path, container_ip, daemon_port, image_id, network_policy, resource_limits, runner_id, runner_http_base_url
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
		&r.ImageID,
		&r.NetworkPolicy,
		&r.ResourceLimits,
		&r.RunnerID,
		&r.RunnerHTTPBase,
	)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// CreateImage inserts a new custom image record.
func (s *Store) CreateImage(record *ImageRecord) error {
	const q = `
		INSERT INTO images
			(id, tag, base_image, docker_image_id, steps_hash, dockerfile_steps, created_at)
		VALUES
			(?, ?, ?, ?, ?, ?, ?)`

	if _, err := s.db.Exec(q,
		record.ID,
		record.Tag,
		record.BaseImage,
		record.DockerImageID,
		record.StepsHash,
		record.DockerfileSteps,
		record.CreatedAt,
	); err != nil {
		if isUniqueStepsHashConstraint(err) {
			return fmt.Errorf("%w: %s", ErrImageStepsHashConflict, record.StepsHash)
		}
		return fmt.Errorf("store: create image %s: %w", record.ID, err)
	}
	return nil
}

// GetImage returns the custom image record with the given id or tag.
func (s *Store) GetImage(idOrTag string) (*ImageRecord, error) {
	const q = `
		SELECT id, tag, base_image, docker_image_id, steps_hash, dockerfile_steps, created_at
		FROM images
		WHERE id = ? OR tag = ?
		LIMIT 1`

	row := s.db.QueryRow(q, idOrTag, idOrTag)
	rec, err := scanImageRecord(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: get image %s: %w", idOrTag, err)
	}
	return rec, nil
}

// ListImages returns all custom image records ordered by creation time descending.
func (s *Store) ListImages() ([]*ImageRecord, error) {
	const q = `
		SELECT id, tag, base_image, docker_image_id, steps_hash, dockerfile_steps, created_at
		FROM images
		ORDER BY created_at DESC`

	rows, err := s.db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("store: list images: %w", err)
	}
	defer rows.Close()

	var records []*ImageRecord
	for rows.Next() {
		rec, err := scanImageRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list images scan: %w", err)
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list images rows: %w", err)
	}
	return records, nil
}

// DeleteImage removes the image record with the given id.
func (s *Store) DeleteImage(id string) error {
	const q = `DELETE FROM images WHERE id = ?`
	if _, err := s.db.Exec(q, id); err != nil {
		return fmt.Errorf("store: delete image %s: %w", id, err)
	}
	return nil
}

// CountSandboxesByImageID counts running sandboxes referencing the image.
func (s *Store) CountSandboxesByImageID(imageID string) (int, error) {
	const q = `SELECT COUNT(*) FROM sandboxes WHERE image_id = ? AND status = 'running'`

	var count int
	if err := s.db.QueryRow(q, imageID).Scan(&count); err != nil {
		return 0, fmt.Errorf("store: count sandboxes by image id %s: %w", imageID, err)
	}
	return count, nil
}

func scanImageRecord(row scanner) (*ImageRecord, error) {
	var r ImageRecord
	if err := row.Scan(
		&r.ID,
		&r.Tag,
		&r.BaseImage,
		&r.DockerImageID,
		&r.StepsHash,
		&r.DockerfileSteps,
		&r.CreatedAt,
	); err != nil {
		return nil, err
	}
	return &r, nil
}

// GetImageByStepsHash returns the image record matching the given steps hash, or (nil, nil).
func (s *Store) GetImageByStepsHash(hash string) (*ImageRecord, error) {
	const q = `
		SELECT id, tag, base_image, docker_image_id, steps_hash, dockerfile_steps, created_at
		FROM images
		WHERE steps_hash = ?
		LIMIT 1`

	row := s.db.QueryRow(q, hash)
	rec, err := scanImageRecord(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: get image by steps hash: %w", err)
	}
	return rec, nil
}

func isUniqueStepsHashConstraint(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "unique constraint failed: images.steps_hash")
}
