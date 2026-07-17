// Package store provides persistent storage for sandbox records (SQLite or Postgres).
package store

import "io"

// Backend identifies the sandbox store implementation.
type Backend string

const (
	BackendSQLite   Backend = "sqlite"
	BackendPostgres Backend = "postgres"
)

// SandboxRecord is the in-memory representation of a row in the sandboxes table.
type SandboxRecord struct {
	ID                    string
	Status                string
	CreatedAt             int64
	LastActiveAt          int64
	RootfsPath            string
	SocketPath            string
	ContainerIP           string
	DaemonPort            int
	RunnerID              string // Runner that hosts this sandbox (from registration)
	RunnerHTTPBase        string // Base URL to reach that runner's HTTP API (for proxying)
	RunnerControlGRPCAddr string // host:port for SandboxControl gRPC
	TenantID              string // empty = admin-created / legacy
}

// Tenant is a provisioned consumer of the sandbox API (e.g. an n8n instance).
type Tenant struct {
	ID           string
	Name         string
	ExternalRef  string
	MaxSandboxes int // 0 = unlimited
	CreatedAt    int64
}

// APIKey is a hashed tenant credential. Plaintext is never stored.
type APIKey struct {
	ID        string
	TenantID  string
	KeyHash   string
	Prefix    string
	CreatedAt int64
	RevokedAt int64 // 0 = active
}

// SandboxStore exposes CRUD operations for SandboxRecord rows and tenant keys.
type SandboxStore interface {
	io.Closer
	Create(record *SandboxRecord) error
	Get(id string) (*SandboxRecord, error)
	UpdateStatus(id, status string) error
	UpdateLastActive(id string) error
	Delete(id string) error
	ListForIdleReapDelete(cutoff int64) ([]*SandboxRecord, error)
	ListForIdleReapStop(cutoff int64) ([]*SandboxRecord, error)
	Count() (int64, error)
	CountByTenant(tenantID string) (int64, error)
	List() ([]*SandboxRecord, error)
	ListByTenant(tenantID string) ([]*SandboxRecord, error)
	Backend() Backend

	CreateTenant(t *Tenant) error
	GetTenant(id string) (*Tenant, error)
	ListTenants() ([]*Tenant, error)
	DeleteTenant(id string) error

	CreateAPIKey(k *APIKey) error
	GetAPIKey(id string) (*APIKey, error)
	ListAPIKeysByTenant(tenantID string) ([]*APIKey, error)
	ListActiveAPIKeysByPrefix(prefix string) ([]*APIKey, error)
	RevokeAPIKey(id string) error
}

// scanner is the common interface satisfied by both *sql.Row and *sql.Rows.
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
		&r.RunnerControlGRPCAddr,
		&r.TenantID,
	)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func scanTenant(row scanner) (*Tenant, error) {
	var t Tenant
	err := row.Scan(&t.ID, &t.Name, &t.ExternalRef, &t.MaxSandboxes, &t.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func scanAPIKey(row scanner) (*APIKey, error) {
	var k APIKey
	var revokedAt *int64
	err := row.Scan(&k.ID, &k.TenantID, &k.KeyHash, &k.Prefix, &k.CreatedAt, &revokedAt)
	if err != nil {
		return nil, err
	}
	if revokedAt != nil {
		k.RevokedAt = *revokedAt
	}
	return &k, nil
}
