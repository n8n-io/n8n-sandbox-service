package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func (s *PostgresStore) CreateAPIKey(k *APIKey) error {
	var revoked any
	if k.RevokedAt > 0 {
		revoked = k.RevokedAt
	}
	const q = `INSERT INTO api_keys (id, tenant_id, key_hash, prefix, created_at, revoked_at) VALUES ($1, $2, $3, $4, $5, $6)`
	_, err := s.db.Exec(q, k.ID, k.TenantID, k.KeyHash, k.Prefix, k.CreatedAt, revoked)
	if err != nil {
		return fmt.Errorf("store: create api key %s: %w", k.ID, err)
	}
	return nil
}

func (s *PostgresStore) GetAPIKey(id string) (*APIKey, error) {
	row := s.db.QueryRow(`SELECT id, tenant_id, key_hash, prefix, created_at, revoked_at FROM api_keys WHERE id = $1`, id)
	k, err := scanAPIKey(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: get api key %s: %w", id, err)
	}
	return k, nil
}

func (s *PostgresStore) ListAPIKeysByTenant(tenantID string) ([]*APIKey, error) {
	rows, err := s.db.Query(`SELECT id, tenant_id, key_hash, prefix, created_at, revoked_at FROM api_keys WHERE tenant_id = $1 ORDER BY created_at DESC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: list api keys: %w", err)
	}
	defer rows.Close()
	return scanAPIKeyRows(rows)
}

func (s *PostgresStore) ListActiveAPIKeysByPrefix(prefix string) ([]*APIKey, error) {
	rows, err := s.db.Query(`SELECT id, tenant_id, key_hash, prefix, created_at, revoked_at FROM api_keys WHERE prefix = $1 AND revoked_at IS NULL`, prefix)
	if err != nil {
		return nil, fmt.Errorf("store: list api keys by prefix: %w", err)
	}
	defer rows.Close()
	return scanAPIKeyRows(rows)
}

func (s *PostgresStore) RevokeAPIKey(id string) error {
	now := time.Now().Unix()
	if _, err := s.db.Exec(`UPDATE api_keys SET revoked_at = $1 WHERE id = $2 AND revoked_at IS NULL`, now, id); err != nil {
		return fmt.Errorf("store: revoke api key %s: %w", id, err)
	}
	return nil
}
