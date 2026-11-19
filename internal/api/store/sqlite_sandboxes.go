package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const sqliteSandboxCols = `id, status, created_at, last_active_at, rootfs_path, socket_path, container_ip, daemon_port, runner_id, runner_http_base_url, runner_control_grpc_addr, tenant_id, ephemeral, egress, kind, name, options_hash, idle_timeout_ms, max_age_ms, draining_since, template, idle_stop_after, idle_delete_after, egress_proxy, model_blocked, idle_archive_after, archive_object, archive_key, archive_size, archive_version, archived_at, restore_started_at`

type sqliteExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func (s *SQLiteStore) insertSandbox(e sqliteExecer, record *SandboxRecord) error {
	const q = `
		INSERT INTO sandboxes
			(id, status, created_at, last_active_at, rootfs_path, socket_path, container_ip, daemon_port, runner_id, runner_http_base_url, runner_control_grpc_addr, tenant_id, ephemeral, egress, kind, name, options_hash, idle_timeout_ms, max_age_ms, draining_since, template, idle_stop_after, idle_delete_after, egress_proxy, idle_archive_after)
		VALUES
			(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

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
		record.Ephemeral,
		record.Egress,
		string(record.Kind),
		record.Name,
		record.OptionsHash,
		record.IdleTimeoutMs,
		record.MaxAgeMs,
		record.DrainingSince,
		record.Template,
		record.IdleStopAfter,
		record.IdleDeleteAfter,
		record.EgressProxy,
		record.IdleArchiveAfter,
	)
	if isSQLiteCodeNameConflict(err) {
		return ErrSandboxNameTaken
	}
	if err != nil {
		return fmt.Errorf("store: create sandbox %s: %w", record.ID, err)
	}
	return nil
}

// isSQLiteCodeNameConflict reports whether err is a violation of the code
// name index. SQLite names the columns, not the index, of a partial index.
func isSQLiteCodeNameConflict(err error) bool {
	var serr *sqlite.Error
	return errors.As(err, &serr) &&
		serr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE &&
		strings.Contains(serr.Error(), "sandboxes.tenant_id, sandboxes.name")
}

func (s *SQLiteStore) Create(record *SandboxRecord) error {
	if err := checkKind(record.Kind); err != nil {
		return err
	}
	if IsAdminTenantID(record.TenantID) {
		return s.insertSandbox(s.db, record)
	}

	// BEGIN IMMEDIATE write-locks the DB so DeleteTenant cannot interleave
	// after its emptiness check, and so creates count and insert one at a time.
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

	var maxSandboxes int
	err = conn.QueryRowContext(context.Background(), `SELECT max_sandboxes FROM tenants WHERE id = ?`, record.TenantID).Scan(&maxSandboxes)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrTenantNotFound
	}
	if err != nil {
		return fmt.Errorf("store: get tenant %s: %w", record.TenantID, err)
	}
	if maxSandboxes > 0 {
		var n int64
		if err := conn.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM sandboxes WHERE tenant_id = ?`, record.TenantID).Scan(&n); err != nil {
			return fmt.Errorf("store: count sandboxes by tenant: %w", err)
		}
		if n >= int64(maxSandboxes) {
			return ErrTenantQuotaExceeded
		}
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

// sqlitePlaceholder returns the placeholder of a query argument.
func sqlitePlaceholder(int) string { return "?" }

func (s *SQLiteStore) GetAnyKind(id string) (*SandboxRecord, error) {
	return s.getSandbox(id, AllKinds()...)
}

func (s *SQLiteStore) GetGeneralSandbox(id string) (*SandboxRecord, error) {
	return s.getSandbox(id, KindGeneral)
}

func (s *SQLiteStore) GetCodeSandbox(id string) (*SandboxRecord, error) {
	return s.getSandbox(id, KindCode)
}

// getSandbox returns the record of the sandbox with the given id when it is
// of one of kinds, or nil.
func (s *SQLiteStore) getSandbox(id string, kinds ...Kind) (*SandboxRecord, error) {
	cond, args, err := KindFilter(kinds...).where([]string{"id = ?"}, []any{id}, sqlitePlaceholder)
	if err != nil {
		return nil, err
	}
	q := `SELECT ` + sqliteSandboxCols + ` FROM sandboxes WHERE ` + cond
	rec, err := scanRecord(s.db.QueryRow(q, args...))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: get sandbox %s: %w", id, err)
	}
	return rec, nil
}

func (s *SQLiteStore) GetCodeSandboxByName(tenantID, name string) (*SandboxRecord, error) {
	q := `SELECT ` + sqliteSandboxCols + ` FROM sandboxes
		WHERE tenant_id = ? AND name = ? AND kind = 'code' AND draining_since = 0`
	rec, err := scanRecord(s.db.QueryRow(q, tenantID, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get code sandbox by name: %w", err)
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

func (s *SQLiteStore) RecordActivity(ctx context.Context, lastActiveAt map[string]int64) error {
	if len(lastActiveAt) == 0 {
		return nil
	}
	batch, err := json.Marshal(lastActiveAt)
	if err != nil {
		return fmt.Errorf("store: record activity: %w", err)
	}
	const q = `
		UPDATE sandboxes
		   SET last_active_at = MAX(sandboxes.last_active_at, b.at), status = 'running'
		  FROM (SELECT key AS id, value AS at FROM json_each(?)) AS b
		 WHERE sandboxes.id = b.id
		   AND sandboxes.status NOT IN ('archived', 'restoring')
		   AND (sandboxes.last_active_at < b.at OR sandboxes.status <> 'running')`
	if _, err := s.db.ExecContext(ctx, q, string(batch)); err != nil {
		return fmt.Errorf("store: record activity for %d sandboxes: %w", len(lastActiveAt), err)
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

func (s *SQLiteStore) LockSandbox(ctx context.Context, id string) (func(), error) {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(id))
	lock := s.sandboxLocks[hash.Sum32()%uint32(len(s.sandboxLocks))]
	select {
	case lock <- struct{}{}:
		return func() { <-lock }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *SQLiteStore) ListForIdleReapDelete(cutoff int64) ([]*SandboxRecord, error) {
	q := `SELECT ` + sqliteSandboxCols + `
		FROM sandboxes
		WHERE status = 'stopped'
		  AND last_active_at <= ?`
	return s.querySandboxRecords(q, cutoff)
}

func (s *SQLiteStore) ListForIdleReapStop(cutoff int64, ephemeral bool) ([]*SandboxRecord, error) {
	q := `SELECT ` + sqliteSandboxCols + `
		FROM sandboxes
		WHERE status = 'running'
		  AND last_active_at <= ?
		  AND ephemeral = ?`
	return s.querySandboxRecords(q, cutoff, ephemeral)
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

func (s *SQLiteStore) Count(f SandboxFilter) (int64, error) {
	cond, args, err := f.where(nil, nil, sqlitePlaceholder)
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

func (s *SQLiteStore) List(f SandboxFilter) ([]*SandboxRecord, error) {
	cond, args, err := f.where(nil, nil, sqlitePlaceholder)
	if err != nil {
		return nil, err
	}
	q := `SELECT ` + sqliteSandboxCols + ` FROM sandboxes`
	if cond != "" {
		q += ` WHERE ` + cond
	}
	q += ` ORDER BY created_at DESC`
	return s.querySandboxRecords(q, args...)
}

func (s *SQLiteStore) ListIdle(phase IdlePhase, now, global int64) ([]*SandboxRecord, error) {
	q := `SELECT ` + sqliteSandboxCols + ` FROM sandboxes WHERE ` + idleFilter(phase)
	return s.querySandboxRecords(q, now, global, now, global)
}

func (s *SQLiteStore) UpdateEgressProxy(id, spec string) error {
	if _, err := s.db.Exec(`UPDATE sandboxes SET egress_proxy = ? WHERE id = ?`, spec, id); err != nil {
		return fmt.Errorf("store: update egress_proxy for %s: %w", id, err)
	}
	return nil
}

func (s *SQLiteStore) SetModelBlocked(id string, blocked bool) error {
	if _, err := s.db.Exec(`UPDATE sandboxes SET model_blocked = ? WHERE id = ?`, blocked, id); err != nil {
		return fmt.Errorf("store: update model_blocked for %s: %w", id, err)
	}
	return nil
}

func (s *SQLiteStore) TouchRunnerActive(runnerID string, ids []string) error {
	if runnerID == "" || len(ids) == 0 {
		return nil
	}
	q := `UPDATE sandboxes SET last_active_at = ? WHERE runner_id = ? AND status = 'running' AND id IN (?` +
		strings.Repeat(",?", len(ids)-1) + `)`
	args := make([]any, 0, len(ids)+2)
	args = append(args, time.Now().Unix(), runnerID)
	for _, id := range ids {
		args = append(args, id)
	}
	if _, err := s.db.Exec(q, args...); err != nil {
		return fmt.Errorf("store: touch active sandboxes of runner %s: %w", runnerID, err)
	}
	return nil
}

func (s *SQLiteStore) UpdateIdleWindows(id string, stopAfter, archiveAfter, deleteAfter int64) error {
	if _, err := s.db.Exec(`UPDATE sandboxes SET idle_stop_after = ?, idle_archive_after = ?, idle_delete_after = ? WHERE id = ?`, stopAfter, archiveAfter, deleteAfter, id); err != nil {
		return fmt.Errorf("store: update idle windows for %s: %w", id, err)
	}
	return nil
}

func (s *SQLiteStore) SetArchived(id, object, key, version string, size, at int64) error {
	const q = `UPDATE sandboxes SET status = 'archived', archive_object = ?, archive_key = ?, archive_version = ?, archive_size = ?, archived_at = ?, restore_started_at = 0 WHERE id = ?`
	if _, err := s.db.Exec(q, object, key, version, size, at, id); err != nil {
		return fmt.Errorf("store: set archived %s: %w", id, err)
	}
	return nil
}

func (s *SQLiteStore) ClearRunner(id string) error {
	const q = `UPDATE sandboxes SET runner_id = '', runner_http_base_url = '', runner_control_grpc_addr = '', container_ip = '' WHERE id = ? AND status = 'archived'`
	if _, err := s.db.Exec(q, id); err != nil {
		return fmt.Errorf("store: clear runner of %s: %w", id, err)
	}
	return nil
}

func (s *SQLiteStore) BeginRestore(id, from string, fromStarted, at int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE sandboxes SET status = 'restoring', restore_started_at = ? WHERE id = ? AND status = ? AND restore_started_at = ?`, at, id, from, fromStarted)
	if err != nil {
		return false, fmt.Errorf("store: begin restore of %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *SQLiteStore) SetRestored(id string, started int64, runnerID, httpBase, controlAddr, containerIP string, at int64) (bool, error) {
	const q = `UPDATE sandboxes SET status = 'running', runner_id = ?, runner_http_base_url = ?, runner_control_grpc_addr = ?, container_ip = ?,
		last_active_at = ?, archive_object = '', archive_key = '', archive_size = 0, archive_version = '', archived_at = 0, restore_started_at = 0
		WHERE id = ? AND status = 'restoring' AND restore_started_at = ?`
	res, err := s.db.Exec(q, runnerID, httpBase, controlAddr, containerIP, at, id, started)
	if err != nil {
		return false, fmt.Errorf("store: set restored %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *SQLiteStore) AbortRestore(id string, started int64) error {
	if _, err := s.db.Exec(`UPDATE sandboxes SET status = 'archived' WHERE id = ? AND status = 'restoring' AND restore_started_at = ?`, id, started); err != nil {
		return fmt.Errorf("store: abort restore of %s: %w", id, err)
	}
	return nil
}

func (s *SQLiteStore) ListByRunner(runnerID string) ([]*SandboxRecord, error) {
	return s.querySandboxRecords(`SELECT `+sqliteSandboxCols+` FROM sandboxes WHERE runner_id = ?`, runnerID)
}

func (s *SQLiteStore) ListArchivedOnRunner() ([]*SandboxRecord, error) {
	return s.querySandboxRecords(`SELECT ` + sqliteSandboxCols + ` FROM sandboxes WHERE status = 'archived' AND runner_id <> ''`)
}
