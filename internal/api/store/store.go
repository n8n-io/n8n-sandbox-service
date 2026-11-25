// Package store provides persistent storage for sandbox records (SQLite or Postgres).
package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

var (
	errInvalidKind = errors.New("store: invalid sandbox kind")
	errNoKinds     = errors.New("store: no sandbox kinds given")
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

// Kind is the kind of a sandbox.
type Kind string

// Sandbox kinds.
const (
	KindGeneral Kind = "general"
	KindCode    Kind = "code"
)

// AllKinds returns every sandbox kind.
func AllKinds() []Kind { return []Kind{KindGeneral, KindCode} }

func checkKind(kind Kind) error {
	if kind != KindGeneral && kind != KindCode {
		return fmt.Errorf("%w %q", errInvalidKind, kind)
	}
	return nil
}

// SandboxFilter selects the sandboxes of List and Count.
type SandboxFilter struct {
	// Kinds is required: no kinds is an error, not every kind.
	Kinds []Kind
	// TenantID selects the sandboxes of one tenant; "" selects every tenant.
	TenantID string
}

// KindFilter selects the sandboxes of kinds, whatever their tenant.
func KindFilter(kinds ...Kind) SandboxFilter { return SandboxFilter{Kinds: kinds} }

// where returns the condition of conds and f and the query arguments: args,
// those of conds, followed by those of f. placeholder returns the placeholder
// of the nth argument. Kinds that include every kind add no condition, and
// without any condition it returns "": callers then leave out WHERE, so
// SQLite keeps its fast COUNT(*) path.
func (f SandboxFilter) where(conds []string, args []any, placeholder func(n int) string) (string, []any, error) {
	if len(f.Kinds) == 0 {
		return "", nil, errNoKinds
	}
	for _, kind := range f.Kinds {
		if err := checkKind(kind); err != nil {
			return "", nil, err
		}
	}
	conds = slices.Clip(conds)
	args = slices.Clip(args)
	if !includesAllKinds(f.Kinds) {
		marks := make([]string, len(f.Kinds))
		for i, kind := range f.Kinds {
			args = append(args, string(kind))
			marks[i] = placeholder(len(args))
		}
		conds = append(conds, "kind IN ("+strings.Join(marks, ", ")+")")
	}
	if f.TenantID != "" {
		args = append(args, f.TenantID)
		conds = append(conds, "tenant_id = "+placeholder(len(args)))
	}
	return strings.Join(conds, " AND "), args, nil
}

func includesAllKinds(kinds []Kind) bool {
	for _, kind := range AllKinds() {
		if !slices.Contains(kinds, kind) {
			return false
		}
	}
	return true
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

// SandboxStore is a SQLite or Postgres store. Sandbox ids are unique across
// kinds. The Get methods name the kinds they find; the other methods on one
// sandbox work on a sandbox of any kind. None checks the tenant: callers do,
// on the row they read. List and Count take a SandboxFilter; the idle reap
// lists cover every kind.
type SandboxStore interface {
	io.Closer
	Backend() Backend

	// Create inserts the record, whose Kind is required. For a tenant-owned
	// record it returns ErrTenantNotFound when the tenant is gone, and
	// ErrTenantQuotaExceeded when the tenant already holds max_sandboxes
	// sandboxes of any kind (0 = unlimited). Creates for the same tenant count
	// and insert one at a time, so concurrent creates cannot take a tenant past
	// its quota. It returns ErrSandboxNameTaken when a code sandbox of the
	// tenant that is not draining already has the name.
	Create(record *SandboxRecord) error
	// GetAnyKind returns the record of the sandbox with the given id, whatever
	// its kind, or nil when there is none.
	GetAnyKind(id string) (*SandboxRecord, error)
	// GetGeneralSandbox returns the record of the sandbox with the given id
	// when it is a general sandbox, or nil.
	GetGeneralSandbox(id string) (*SandboxRecord, error)
	// GetCodeSandbox returns the record of the sandbox with the given id when
	// it is a code sandbox, or nil.
	GetCodeSandbox(id string) (*SandboxRecord, error)
	// GetCodeSandboxByName returns the code sandbox of the tenant that has the
	// name and is not draining, or nil when there is none.
	GetCodeSandboxByName(tenantID, name string) (*SandboxRecord, error)
	// ListCodeSandboxesByName returns all code sandboxes of the tenant that
	// have the name, draining or not, oldest first.
	ListCodeSandboxesByName(tenantID, name string) ([]*SandboxRecord, error)
	// List returns the sandboxes f selects, newest first.
	List(f SandboxFilter) ([]*SandboxRecord, error)
	Count(f SandboxFilter) (int64, error)
	UpdateStatus(id, status string) error
	Delete(id string) error
	// RecordActivity sets each sandbox in lastActiveAt (id to unix seconds),
	// whatever its kind, running and moves its last_active_at forward to that
	// time, never back, in one statement. Ids without a row are skipped.
	RecordActivity(ctx context.Context, lastActiveAt map[string]int64) error
	// LockSandbox locks the sandbox id, whatever its kind: it reads no row.
	LockSandbox(ctx context.Context, id string) (unlock func(), err error)
	// ListForIdleReapDelete returns the stopped sandboxes of every kind that
	// are idle since cutoff.
	ListForIdleReapDelete(cutoff int64) ([]*SandboxRecord, error)
	// ListForIdleReapStop returns the running sandboxes of every kind that are
	// idle since cutoff and have the given ephemeral flag: regular rows are
	// stop candidates, ephemeral rows delete candidates.
	ListForIdleReapStop(cutoff int64, ephemeral bool) ([]*SandboxRecord, error)

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

	CreateAPIKey(k *APIKey) error
	GetAPIKey(id string) (*APIKey, error)
	ListAPIKeysByTenant(tenantID string) ([]*APIKey, error)
	ListActiveAPIKeysByPrefix(prefix string) ([]*APIKey, error)
	RevokeAPIKey(id string) error
}

var (
	_ SandboxStore = (*SQLiteStore)(nil)
	_ SandboxStore = (*PostgresStore)(nil)
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
