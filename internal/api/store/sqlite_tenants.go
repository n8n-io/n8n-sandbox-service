package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

func (s *SQLiteStore) CreateTenant(t *Tenant) error {
	const q = `INSERT INTO tenants (id, name, external_ref, max_sandboxes, created_at, max_idle_window) VALUES (?, ?, ?, ?, ?, ?)`
	_, err := s.db.Exec(q, t.ID, t.Name, t.ExternalRef, t.MaxSandboxes, t.CreatedAt, t.MaxIdleWindow)
	if err != nil {
		return fmt.Errorf("store: create tenant %s: %w", t.ID, err)
	}
	return nil
}

func (s *SQLiteStore) CreateTenantWithAPIKey(t *Tenant, k *APIKey) error {
	if k.TenantID != t.ID {
		return ErrAPIKeyTenantMismatch
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin tenant+key: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const tenantQ = `INSERT INTO tenants (id, name, external_ref, max_sandboxes, created_at, max_idle_window) VALUES (?, ?, ?, ?, ?, ?)`
	if _, err := tx.Exec(tenantQ, t.ID, t.Name, t.ExternalRef, t.MaxSandboxes, t.CreatedAt, t.MaxIdleWindow); err != nil {
		return fmt.Errorf("store: create tenant %s: %w", t.ID, err)
	}
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
	row := s.db.QueryRow(`SELECT id, name, external_ref, max_sandboxes, created_at, max_idle_window FROM tenants WHERE id = ?`, id)
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
	rows, err := s.db.Query(`SELECT id, name, external_ref, max_sandboxes, created_at, max_idle_window FROM tenants ORDER BY created_at DESC`)
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

func (s *SQLiteStore) SetTenantMaxIdleWindow(id string, seconds int64) error {
	res, err := s.db.Exec(`UPDATE tenants SET max_idle_window = ? WHERE id = ?`, seconds, id)
	if err != nil {
		return fmt.Errorf("store: set tenant max_idle_window: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrTenantNotFound
	}
	return nil
}

func (s *SQLiteStore) UpdateTenantLimits(id string, maxSandboxes int) error {
	res, err := s.db.Exec(`UPDATE tenants SET max_sandboxes = ? WHERE id = ?`, maxSandboxes, id)
	if err != nil {
		return fmt.Errorf("store: update tenant %s limits: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update tenant %s limits: %w", id, err)
	}
	if n == 0 {
		return ErrTenantNotFound
	}
	return nil
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
