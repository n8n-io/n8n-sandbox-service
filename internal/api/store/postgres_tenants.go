package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// lockTenantForUpdate takes a row lock on tenants.id until tx ends and returns
// the tenant's max_sandboxes. ok is false when the tenant row does not exist.
func lockTenantForUpdate(tx *sql.Tx, id string) (maxSandboxes int, ok bool, err error) {
	err = tx.QueryRow(`SELECT max_sandboxes FROM tenants WHERE id = $1 FOR UPDATE`, id).Scan(&maxSandboxes)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return maxSandboxes, true, nil
}

func (s *PostgresStore) CreateTenant(t *Tenant) error {
	const q = `INSERT INTO tenants (id, name, external_ref, max_sandboxes, created_at, max_idle_window) VALUES ($1, $2, $3, $4, $5, $6)`
	_, err := s.db.Exec(q, t.ID, t.Name, t.ExternalRef, t.MaxSandboxes, t.CreatedAt, t.MaxIdleWindow)
	if err != nil {
		return fmt.Errorf("store: create tenant %s: %w", t.ID, err)
	}
	return nil
}

func (s *PostgresStore) CreateTenantWithAPIKey(t *Tenant, k *APIKey) error {
	if k.TenantID != t.ID {
		return ErrAPIKeyTenantMismatch
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin tenant+key: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const tenantQ = `INSERT INTO tenants (id, name, external_ref, max_sandboxes, created_at, max_idle_window) VALUES ($1, $2, $3, $4, $5, $6)`
	if _, err := tx.Exec(tenantQ, t.ID, t.Name, t.ExternalRef, t.MaxSandboxes, t.CreatedAt, t.MaxIdleWindow); err != nil {
		return fmt.Errorf("store: create tenant %s: %w", t.ID, err)
	}
	var revoked any
	if k.RevokedAt > 0 {
		revoked = k.RevokedAt
	}
	const keyQ = `INSERT INTO api_keys (id, tenant_id, key_hash, prefix, created_at, revoked_at) VALUES ($1, $2, $3, $4, $5, $6)`
	if _, err := tx.Exec(keyQ, k.ID, k.TenantID, k.KeyHash, k.Prefix, k.CreatedAt, revoked); err != nil {
		return fmt.Errorf("store: create api key %s: %w", k.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit tenant+key: %w", err)
	}
	return nil
}

func (s *PostgresStore) GetTenant(id string) (*Tenant, error) {
	row := s.db.QueryRow(`SELECT id, name, external_ref, max_sandboxes, created_at, max_idle_window FROM tenants WHERE id = $1`, id)
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

func (s *PostgresStore) SetTenantMaxIdleWindow(id string, seconds int64) error {
	res, err := s.db.Exec(`UPDATE tenants SET max_idle_window = $1 WHERE id = $2`, seconds, id)
	if err != nil {
		return fmt.Errorf("store: set tenant max_idle_window: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrTenantNotFound
	}
	return nil
}

func (s *PostgresStore) UpdateTenantLimits(id string, maxSandboxes int) error {
	res, err := s.db.Exec(`UPDATE tenants SET max_sandboxes = $1 WHERE id = $2`, maxSandboxes, id)
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

func (s *PostgresStore) DeleteTenant(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin delete tenant: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Lock the tenant row so a concurrent Create cannot insert a sandbox after
	// the emptiness check and before this delete commits.
	_, ok, err := lockTenantForUpdate(tx, id)
	if err != nil {
		return fmt.Errorf("store: lock tenant %s: %w", id, err)
	}
	if !ok {
		return nil
	}

	var n int64
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sandboxes WHERE tenant_id = $1`, id).Scan(&n); err != nil {
		return fmt.Errorf("store: count sandboxes by tenant: %w", err)
	}
	if n > 0 {
		return ErrTenantHasSandboxes
	}
	// api_keys cascade via ON DELETE CASCADE
	if _, err := tx.Exec(`DELETE FROM tenants WHERE id = $1`, id); err != nil {
		return fmt.Errorf("store: delete tenant %s: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit delete tenant: %w", err)
	}
	return nil
}
