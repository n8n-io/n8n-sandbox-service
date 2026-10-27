package registry

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// PostgresRegistry tracks runners in Postgres (multi API pod mode).
type PostgresRegistry struct {
	db             *sql.DB
	heartbeatGrace time.Duration
}

// NewPostgres returns a registry backed by the runners table in Postgres.
func NewPostgres(db *sql.DB, heartbeatGrace time.Duration) *PostgresRegistry {
	if heartbeatGrace <= 0 {
		heartbeatGrace = 45 * time.Second
	}
	return &PostgresRegistry{db: db, heartbeatGrace: heartbeatGrace}
}

func (r *PostgresRegistry) Upsert(id, httpBaseURL, controlGRPCAddr string, healthy, cordoned bool, capTotal, capUsed, capStopped int32) {
	if id == "" || httpBaseURL == "" {
		return
	}
	httpBaseURL = strings.TrimRight(httpBaseURL, "/")
	controlGRPCAddr = strings.TrimSpace(controlGRPCAddr)
	const q = `
		INSERT INTO runners
			(id, http_base_url, control_grpc_addr, healthy, cordoned, capacity_total, capacity_used, capacity_stopped, last_seen)
		VALUES
			($1, $2, $3, $4, $5, $6, $7, $8, now())
		ON CONFLICT (id) DO UPDATE SET
			http_base_url = EXCLUDED.http_base_url,
			control_grpc_addr = EXCLUDED.control_grpc_addr,
			healthy = EXCLUDED.healthy,
			cordoned = EXCLUDED.cordoned,
			capacity_total = EXCLUDED.capacity_total,
			capacity_used = EXCLUDED.capacity_used,
			capacity_stopped = EXCLUDED.capacity_stopped,
			last_seen = now()`
	_, _ = r.db.Exec(q, id, httpBaseURL, controlGRPCAddr, healthy, cordoned, capTotal, capUsed, capStopped)
}

// Remove is a no-op for Postgres; stale runners are detected via last_seen.
func (r *PostgresRegistry) Remove(id string) {}

func (r *PostgresRegistry) Get(id string) (*Runner, bool) {
	const q = `
		SELECT id, http_base_url, control_grpc_addr, healthy, cordoned, capacity_total, capacity_used, capacity_stopped, last_seen
		FROM runners
		WHERE id = $1`
	row := r.db.QueryRow(q, id)
	run, err := scanRunner(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false
		}
		return nil, false
	}
	return run, true
}

func (r *PostgresRegistry) GoneLongEnough(runnerID string, buffer time.Duration, now time.Time) bool {
	if runnerID == "" || buffer <= 0 {
		return false
	}
	const q = `SELECT last_seen FROM runners WHERE id = $1`
	var lastSeen time.Time
	err := r.db.QueryRow(q, runnerID).Scan(&lastSeen)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return true
		}
		return false
	}
	return !now.Before(lastSeen.Add(buffer))
}

func (r *PostgresRegistry) Len() int {
	graceSec := int64(r.heartbeatGrace.Seconds())
	const q = `
		SELECT COUNT(*)
		FROM runners
		WHERE last_seen >= now() - ($1 * interval '1 second')`
	var n int
	if err := r.db.QueryRow(q, graceSec).Scan(&n); err != nil {
		return 0
	}
	return n
}

func (r *PostgresRegistry) List() ([]Runner, error) {
	const q = `
		SELECT id, http_base_url, control_grpc_addr, healthy, cordoned, capacity_total, capacity_used, capacity_stopped, last_seen
		FROM runners
		ORDER BY id COLLATE "C"`
	rows, err := r.db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("registry: list runners: %w", err)
	}
	defer rows.Close()

	out := []Runner{}
	for rows.Next() {
		run, err := scanRunner(rows)
		if err != nil {
			return nil, fmt.Errorf("registry: scan runner: %w", err)
		}
		out = append(out, *run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("registry: list runners: %w", err)
	}
	return out, nil
}

func (r *PostgresRegistry) PickLowestUsed(exclude ...string) (*Runner, error) {
	graceSec := int64(r.heartbeatGrace.Seconds())
	// Never nil: pgx sends a nil slice as NULL, and NOT (id = ANY(NULL)) would
	// exclude every runner.
	excluded := append([]string{}, exclude...)

	tx, err := r.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("registry: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const q = `
		SELECT id, http_base_url, control_grpc_addr, healthy, cordoned, capacity_total, capacity_used, capacity_stopped, last_seen
		FROM runners
		WHERE healthy = true
		  AND cordoned = false
		  AND last_seen >= now() - ($1 * interval '1 second')
		  AND (capacity_total = 0 OR capacity_used < capacity_total)
		  AND NOT (id = ANY($2))
		ORDER BY capacity_used ASC, id ASC
		FOR UPDATE SKIP LOCKED
		LIMIT 1`

	row := tx.QueryRow(q, graceSec, excluded)
	run, err := scanRunner(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNoRunners
		}
		return nil, fmt.Errorf("registry: pick lowest used: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("registry: commit pick: %w", err)
	}
	return run, nil
}

type runnerScanner interface {
	Scan(dest ...any) error
}

func scanRunner(row runnerScanner) (*Runner, error) {
	var run Runner
	err := row.Scan(
		&run.ID,
		&run.HTTPBaseURL,
		&run.ControlGRPCAddr,
		&run.Healthy,
		&run.Cordoned,
		&run.CapacityTotal,
		&run.CapacityUsed,
		&run.CapacityStopped,
		&run.LastSeen,
	)
	if err != nil {
		return nil, err
	}
	return &run, nil
}
