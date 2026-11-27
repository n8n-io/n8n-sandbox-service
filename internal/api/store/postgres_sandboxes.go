package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5/pgconn"
)

const pgSandboxCols = `id, status, created_at, last_active_at, rootfs_path, socket_path, container_ip, daemon_port, runner_id, runner_http_base_url, runner_control_grpc_addr, tenant_id, ephemeral, egress, kind, name, options_hash, idle_timeout_ms, max_age_ms, draining_since, volumes`

type pgExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func (s *PostgresStore) insertSandbox(e pgExecer, record *SandboxRecord) error {
	const q = `
		INSERT INTO sandboxes
			(` + pgSandboxCols + `)
		VALUES
			($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)`

	_, err := e.Exec(q,
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
		record.Ephemeral,
		record.Egress,
		string(record.Kind),
		record.Name,
		record.OptionsHash,
		record.IdleTimeoutMs,
		record.MaxAgeMs,
		record.DrainingSince,
		cmp.Or(record.Volumes, "[]"),
	)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == codeNameIndex {
		return ErrSandboxNameTaken
	}
	if err != nil {
		return fmt.Errorf("store: create sandbox %s: %w", record.ID, err)
	}
	return nil
}

func (s *PostgresStore) Create(record *SandboxRecord) error {
	if err := checkKind(record.Kind); err != nil {
		return err
	}
	if IsAdminTenantID(record.TenantID) {
		return s.insertSandbox(s.db, record)
	}

	// Read committed, whatever the server default: each statement then sees
	// what was committed before it started, so the count below, taken after
	// the row lock, includes every sandbox the previous holder inserted.
	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("store: begin create sandbox: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Lock the tenant row so DeleteTenant cannot commit until this insert
	// finishes, and so creates for this tenant count and insert one at a time.
	maxSandboxes, ok, err := lockTenantForUpdate(tx, record.TenantID)
	if err != nil {
		return fmt.Errorf("store: lock tenant %s: %w", record.TenantID, err)
	}
	if !ok {
		return ErrTenantNotFound
	}
	if maxSandboxes > 0 {
		var n int64
		if err := tx.QueryRow(`SELECT COUNT(*) FROM sandboxes WHERE tenant_id = $1`, record.TenantID).Scan(&n); err != nil {
			return fmt.Errorf("store: count sandboxes by tenant: %w", err)
		}
		if n >= int64(maxSandboxes) {
			return ErrTenantQuotaExceeded
		}
	}
	if err := s.insertSandbox(tx, record); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit create sandbox: %w", err)
	}
	return nil
}

// pgPlaceholder returns the placeholder of the nth query argument.
func pgPlaceholder(n int) string { return "$" + strconv.Itoa(n) }

func (s *PostgresStore) GetAnyKind(id string) (*SandboxRecord, error) {
	return s.getSandbox(id, AllKinds()...)
}

func (s *PostgresStore) GetGeneralSandbox(id string) (*SandboxRecord, error) {
	return s.getSandbox(id, KindGeneral)
}

func (s *PostgresStore) GetCodeSandbox(id string) (*SandboxRecord, error) {
	return s.getSandbox(id, KindCode)
}

// getSandbox returns the record of the sandbox with the given id when it is
// of one of kinds, or nil.
func (s *PostgresStore) getSandbox(id string, kinds ...Kind) (*SandboxRecord, error) {
	cond, args, err := KindFilter(kinds...).where([]string{"id = $1"}, []any{id}, pgPlaceholder)
	if err != nil {
		return nil, err
	}
	q := `SELECT ` + pgSandboxCols + ` FROM sandboxes WHERE ` + cond
	rec, err := scanRecord(s.db.QueryRow(q, args...))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: get sandbox %s: %w", id, err)
	}
	return rec, nil
}

func (s *PostgresStore) GetCodeSandboxByName(tenantID, name string) (*SandboxRecord, error) {
	q := `SELECT ` + pgSandboxCols + ` FROM sandboxes
		WHERE tenant_id = $1 AND name = $2 AND kind = 'code' AND draining_since = 0`
	rec, err := scanRecord(s.db.QueryRow(q, tenantID, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get code sandbox by name: %w", err)
	}
	return rec, nil
}

func (s *PostgresStore) ListCodeSandboxesByName(tenantID, name string) ([]*SandboxRecord, error) {
	q := `SELECT ` + pgSandboxCols + ` FROM sandboxes
		WHERE tenant_id = $1 AND name = $2 AND kind = 'code'
		ORDER BY created_at, id`
	return s.querySandboxRecords(q, tenantID, name)
}

func (s *PostgresStore) UpdateStatus(id, status string) error {
	const q = `UPDATE sandboxes SET status = $1 WHERE id = $2`
	if _, err := s.db.Exec(q, status, id); err != nil {
		return fmt.Errorf("store: update status for %s: %w", id, err)
	}
	return nil
}

func (s *PostgresStore) RecordActivity(ctx context.Context, lastActiveAt map[string]int64) error {
	if len(lastActiveAt) == 0 {
		return nil
	}
	ids := make([]string, 0, len(lastActiveAt))
	ats := make([]int64, 0, len(lastActiveAt))
	for id, at := range lastActiveAt {
		ids = append(ids, id)
		ats = append(ats, at)
	}
	// Rows that another API pod already moved past the given time are left
	// alone. Each API pod writes a batch like this, so the rows are locked in id
	// order: two batches that share sandboxes then cannot deadlock.
	const q = `
		WITH batch AS (
			SELECT * FROM unnest($1::text[], $2::bigint[]) AS b(id, at)
		), locked AS (
			SELECT s.id, b.at
			  FROM sandboxes s JOIN batch b ON b.id = s.id
			 WHERE s.last_active_at < b.at OR s.status <> 'running'
			 ORDER BY s.id
			   FOR UPDATE OF s
		)
		UPDATE sandboxes s
		   SET last_active_at = GREATEST(s.last_active_at, locked.at), status = 'running'
		  FROM locked
		 WHERE s.id = locked.id`
	if _, err := s.db.ExecContext(ctx, q, ids, ats); err != nil {
		return fmt.Errorf("store: record activity for %d sandboxes: %w", len(ids), err)
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

func (s *PostgresStore) LockSandbox(ctx context.Context, id string) (func(), error) {
	conn, err := s.lockDB.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: acquire conn for sandbox lock: %w", err)
	}
	tx, err := conn.BeginTx(context.Background(), nil)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("store: begin sandbox lock transaction: %w", err)
	}
	unlock := func() {
		_ = tx.Rollback()
		_ = conn.Close()
	}
	lockID := "sandbox:" + id
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockID); err != nil {
		unlock()
		return nil, fmt.Errorf("store: lock sandbox %s: %w", id, err)
	}
	return unlock, nil
}

func (s *PostgresStore) ListForIdleReapDelete(cutoff int64) ([]*SandboxRecord, error) {
	q := `SELECT ` + pgSandboxCols + `
		FROM sandboxes
		WHERE status = 'stopped'
		  AND last_active_at <= $1`
	return s.querySandboxRecords(q, cutoff)
}

func (s *PostgresStore) ListForIdleReapStop(cutoff int64, ephemeral bool) ([]*SandboxRecord, error) {
	q := `SELECT ` + pgSandboxCols + `
		FROM sandboxes
		WHERE status = 'running'
		  AND last_active_at <= $1
		  AND ephemeral = $2`
	return s.querySandboxRecords(q, cutoff, ephemeral)
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

func (s *PostgresStore) Count(f SandboxFilter) (int64, error) {
	cond, args, err := f.where(nil, nil, pgPlaceholder)
	if err != nil {
		return 0, err
	}
	q := `SELECT COUNT(*) FROM sandboxes`
	if cond != "" {
		q += ` WHERE ` + cond
	}
	var n int64
	if err := s.db.QueryRow(q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count sandboxes: %w", err)
	}
	return n, nil
}

func (s *PostgresStore) List(f SandboxFilter) ([]*SandboxRecord, error) {
	cond, args, err := f.where(nil, nil, pgPlaceholder)
	if err != nil {
		return nil, err
	}
	q := `SELECT ` + pgSandboxCols + ` FROM sandboxes`
	if cond != "" {
		q += ` WHERE ` + cond
	}
	q += ` ORDER BY created_at DESC`
	return s.querySandboxRecords(q, args...)
}
