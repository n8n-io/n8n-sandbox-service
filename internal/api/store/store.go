// Package store provides persistent storage for sandbox records (SQLite or Postgres).
package store

import (
	"context"
	"errors"
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

// IdlePhase is one pass of the idle sweeper.
type IdlePhase int

const (
	// IdleStop: running regular rows, by idle_stop_after.
	IdleStop IdlePhase = iota
	// IdleEphemeral: running ephemeral rows, deleted by idle_stop_after.
	IdleEphemeral
	// IdleDelete: stopped rows, by idle_delete_after.
	IdleDelete
)

// idleFilter is the WHERE clause of phase with ? placeholders for now,
// global, now and global, in that order. The column names are fixed here,
// never caller input.
func idleFilter(phase IdlePhase) string {
	var where, col string
	switch phase {
	case IdleStop:
		where, col = `status = 'running' AND ephemeral = false`, "idle_stop_after"
	case IdleEphemeral:
		where, col = `status = 'running' AND ephemeral = true`, "idle_stop_after"
	default:
		where, col = `status = 'stopped'`, "idle_delete_after"
	}
	// Casts let Postgres type the bare parameters.
	return where + ` AND ((` + col + ` > 0 AND last_active_at <= CAST(? AS BIGINT) - ` + col + `) OR (` +
		col + ` = 0 AND CAST(? AS BIGINT) > 0 AND last_active_at <= CAST(? AS BIGINT) - CAST(? AS BIGINT)))`
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
	Egress                string // Outbound network policy: "public", "none" or "proxy"; fixed at creation
	Template              string // Image the sandbox boots; fixed at creation
	IdleStopAfter         int64  // Seconds; 0 = the API's global window
	IdleDeleteAfter       int64  // Seconds; 0 = the API's global window
	EgressProxy           string // Validated EgressProxySpec as JSON when Egress is "proxy", else empty
	ModelBlocked          bool   // New requests to the proxy sandbox's model hosts are refused
}

// Tenant is a provisioned consumer of the sandbox API (e.g. an n8n instance).
type Tenant struct {
	ID           string
	Name         string
	ExternalRef  string
	MaxSandboxes int // 0 = unlimited
	CreatedAt    int64
	// MaxIdleWindow caps the idle windows of the tenant's sandboxes, in
	// seconds; 0 is the server's cap.
	MaxIdleWindow int64
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
	// Create inserts the record. For a tenant-owned record it returns
	// ErrTenantNotFound when the tenant is gone, and ErrTenantQuotaExceeded
	// when the tenant already holds max_sandboxes sandboxes (0 = unlimited).
	// Creates for the same tenant count and insert one at a time, so
	// concurrent creates cannot take a tenant past its quota.
	Create(record *SandboxRecord) error
	Get(id string) (*SandboxRecord, error)
	UpdateStatus(id, status string) error
	UpdateLastActive(id string) error
	// UpdateIdleWindows sets a sandbox's own idle windows (seconds, 0 = the
	// API's global window).
	UpdateIdleWindows(id string, stopAfter, deleteAfter int64) error
	// UpdateEgressProxy stores a proxy sandbox's replaced spec (canonical JSON).
	UpdateEgressProxy(id, spec string) error
	// SetModelBlocked records whether a proxy sandbox's model egress is blocked.
	SetModelBlocked(id string, blocked bool) error
	// TouchRunnerActive marks running sandboxes active now, but only those
	// stored as placed on runnerID: a runner vouches for its own sandboxes alone.
	TouchRunnerActive(runnerID string, ids []string) error
	Delete(id string) error
	LockSandbox(ctx context.Context, id string) (unlock func(), err error)
	// ListIdle returns phase's sweep candidates at now (Unix seconds): rows
	// idle for longer than their own window, or than global (seconds) when they
	// have none. global 0 leaves rows without their own window out.
	ListIdle(phase IdlePhase, now, global int64) ([]*SandboxRecord, error)
	Count() (int64, error)
	CountByTenant(tenantID string) (int64, error)
	List() ([]*SandboxRecord, error)
	ListByTenant(tenantID string) ([]*SandboxRecord, error)
	Backend() Backend

	CreateTenant(t *Tenant) error
	// CreateTenantWithAPIKey inserts the tenant and its first API key in one transaction.
	// k.TenantID must equal t.ID.
	CreateTenantWithAPIKey(t *Tenant, k *APIKey) error
	GetTenant(id string) (*Tenant, error)
	ListTenants() ([]*Tenant, error)
	// UpdateTenantLimits sets the tenant's limits and leaves its sandboxes
	// alone. Returns ErrTenantNotFound when no tenant has that id.
	UpdateTenantLimits(id string, maxSandboxes int) error
	// SetTenantMaxIdleWindow sets the tenant's idle window cap (seconds, 0 =
	// the server's); existing sandboxes keep their windows.
	SetTenantMaxIdleWindow(id string, seconds int64) error
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
		&r.Ephemeral,
		&r.Egress,
		&r.Template,
		&r.IdleStopAfter,
		&r.IdleDeleteAfter,
		&r.EgressProxy,
		&r.ModelBlocked,
	)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func scanTenant(row scanner) (*Tenant, error) {
	var t Tenant
	err := row.Scan(&t.ID, &t.Name, &t.ExternalRef, &t.MaxSandboxes, &t.CreatedAt, &t.MaxIdleWindow)
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
