// Package store provides persistent storage for sandbox records (SQLite or Postgres).
package store

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// ErrTenantHasSandboxes is returned when DeleteTenant is called while the tenant
// still owns sandbox rows.
var ErrTenantHasSandboxes = errors.New("tenant has sandboxes")

// ErrTenantNotFound is returned when persisting a tenant-owned sandbox whose
// tenant row no longer exists (e.g. deleted concurrently during create), and
// when updating a tenant that does not exist.
var ErrTenantNotFound = errors.New("tenant not found")

// ErrTenantQuotaExceeded is returned when persisting a tenant-owned sandbox
// while the tenant already holds max_sandboxes sandboxes.
var ErrTenantQuotaExceeded = errors.New("tenant sandbox quota exceeded")

// ErrAPIKeyTenantMismatch is returned when CreateTenantWithAPIKey is called with
// k.TenantID != t.ID.
var ErrAPIKeyTenantMismatch = errors.New("api key tenant_id does not match tenant")

// ErrSandboxNameTaken is returned when creating a code sandbox whose name
// already points to a code sandbox of the tenant that is not draining.
var ErrSandboxNameTaken = errors.New("sandbox name taken")

// Kind is the kind of a sandbox. Every sandbox method of the store names the
// kinds it reads or writes.
type Kind string

// Sandbox kinds.
const (
	KindDefault Kind = "default"
	KindCode    Kind = "code"
)

// AllKinds returns every sandbox kind.
func AllKinds() []Kind { return []Kind{KindDefault, KindCode} }

func checkKind(kind Kind) error {
	if kind != KindDefault && kind != KindCode {
		return fmt.Errorf("store: invalid sandbox kind %q", kind)
	}
	return nil
}

// kindArgs checks kinds and returns them as query arguments. An empty list
// is an error, not a filter that matches every kind.
func kindArgs(kinds []Kind) ([]string, error) {
	if len(kinds) == 0 {
		return nil, errors.New("store: no sandbox kinds given")
	}
	args := make([]string, len(kinds))
	for i, kind := range kinds {
		if err := checkKind(kind); err != nil {
			return nil, err
		}
		args[i] = string(kind)
	}
	return args, nil
}

// codeNameIndex is the unique index on the names of code sandboxes.
const codeNameIndex = "sandboxes_code_name_uniq"

// Backend identifies the sandbox store implementation.
type Backend string

const (
	BackendSQLite   Backend = "sqlite"
	BackendPostgres Backend = "postgres"

	// AdminTenantID is stored on sandboxes created with an admin API key (not
	// owned by any tenant). Chosen to be non-UUID so it cannot collide with
	// real tenant ids.
	AdminTenantID = "__admin__"
)

// IsAdminTenantID reports whether tenantID marks an admin-owned sandbox.
// Empty is treated the same for rows that predate the sentinel backfill.
func IsAdminTenantID(tenantID string) bool {
	return tenantID == "" || tenantID == AdminTenantID
}

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
	TenantID              string // AdminTenantID = admin-owned; otherwise a tenants.id UUID
	Ephemeral             bool   // Deleted instead of stopped when idle; never enters "stopped"
	Egress                string // Outbound network policy: "public" or "none"; fixed at creation
	Kind                  Kind   // Required on create
	Name                  string // Code sandboxes: chosen by the client, unique per tenant among those not draining
	OptionsHash           string // Code sandboxes: hash of the options the sandbox was created with
	IdleTimeoutMs         int64  // Code sandboxes: 0 = not set
	MaxAgeMs              int64  // Code sandboxes: 0 = not set
	DrainingSince         int64  // Unix seconds the sandbox started draining; 0 = not draining
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

// SandboxPersistence stores sandbox rows. Every method that reads or writes
// rows names the kinds it works on: a row of another kind is as good as
// missing.
type SandboxPersistence interface {
	// Create inserts the record, whose Kind is required. For a tenant-owned
	// record it returns ErrTenantNotFound when the tenant is gone, and
	// ErrTenantQuotaExceeded when the tenant already holds max_sandboxes
	// sandboxes of any kind (0 = unlimited). Creates for the same tenant count
	// and insert one at a time, so concurrent creates cannot take a tenant past
	// its quota. It returns ErrSandboxNameTaken when a code sandbox of the
	// tenant that is not draining already has the name.
	Create(record *SandboxRecord) error
	GetByIDAndKind(id string, kind Kind) (*SandboxRecord, error)
	// ExistsByIDAndKinds reports whether a sandbox of one of kinds has the id.
	ExistsByIDAndKinds(id string, kinds []Kind) (bool, error)
	// GetCodeSandboxByName returns the code sandbox of the tenant that has the
	// name and is not draining, or nil when there is none.
	GetCodeSandboxByName(tenantID, name string) (*SandboxRecord, error)
	ListByKind(kind Kind) ([]*SandboxRecord, error)
	ListByTenantAndKind(tenantID string, kind Kind) ([]*SandboxRecord, error)
	UpdateStatusByIDAndKind(id string, kind Kind, status string) error
	// RecordActivityByKind sets each sandbox of the kind in lastActiveAt (id
	// to unix seconds) running and moves its last_active_at forward to that
	// time, never back, in one statement. Ids without a row of the kind are
	// skipped.
	RecordActivityByKind(ctx context.Context, kind Kind, lastActiveAt map[string]int64) error
	DeleteByIDAndKind(id string, kind Kind) error
	// LockSandbox locks the sandbox id, whatever its kind: it reads no row.
	LockSandbox(ctx context.Context, id string) (unlock func(), err error)
	ListForIdleReapDeleteByKinds(kinds []Kind, cutoff int64) ([]*SandboxRecord, error)
	// ListForIdleReapStopByKinds returns running rows idle since cutoff with
	// the given ephemeral flag: regular rows are stop candidates, ephemeral
	// rows delete candidates.
	ListForIdleReapStopByKinds(kinds []Kind, cutoff int64, ephemeral bool) ([]*SandboxRecord, error)
	CountByKinds(kinds []Kind) (int64, error)
	CountByTenantAndKinds(tenantID string, kinds []Kind) (int64, error)
}

// TenantStore stores tenants.
type TenantStore interface {
	CreateTenant(t *Tenant) error
	// CreateTenantWithAPIKey inserts the tenant and its first API key in one transaction.
	// k.TenantID must equal t.ID.
	CreateTenantWithAPIKey(t *Tenant, k *APIKey) error
	GetTenant(id string) (*Tenant, error)
	ListTenants() ([]*Tenant, error)
	// UpdateTenantLimits sets the tenant's limits and leaves its sandboxes
	// alone. Returns ErrTenantNotFound when no tenant has that id.
	UpdateTenantLimits(id string, maxSandboxes int) error
	// DeleteTenant returns ErrTenantHasSandboxes while the tenant owns
	// sandboxes of any kind.
	DeleteTenant(id string) error
}

// APIKeyStore stores tenant API keys.
type APIKeyStore interface {
	CreateAPIKey(k *APIKey) error
	GetAPIKey(id string) (*APIKey, error)
	ListAPIKeysByTenant(tenantID string) ([]*APIKey, error)
	ListActiveAPIKeysByPrefix(prefix string) ([]*APIKey, error)
	RevokeAPIKey(id string) error
}

// TenantPersistence stores tenants and their API keys.
type TenantPersistence interface {
	TenantStore
	APIKeyStore
}

// Persistence is a whole store: SQLite or Postgres. Only startup wiring holds
// one; everything else gets a narrower interface (see views.go).
type Persistence interface {
	io.Closer
	SandboxPersistence
	TenantPersistence
	Backend() Backend
}

var (
	_ Persistence = (*SQLiteStore)(nil)
	_ Persistence = (*PostgresStore)(nil)
)

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
		&r.Ephemeral,
		&r.Egress,
		&r.Kind,
		&r.Name,
		&r.OptionsHash,
		&r.IdleTimeoutMs,
		&r.MaxAgeMs,
		&r.DrainingSince,
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
