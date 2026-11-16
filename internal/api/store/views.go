package store

import (
	"context"
	"fmt"
	"slices"
)

// DefaultSandboxStore is the store of the default sandbox routes. It reads
// and writes default sandboxes only.
type DefaultSandboxStore interface {
	Create(record *SandboxRecord) error
	Get(id string) (*SandboxRecord, error)
	// IDTaken reports whether a sandbox of any kind has the id, for a create
	// whose client picks the id.
	IDTaken(id string) (bool, error)
	List() ([]*SandboxRecord, error)
	ListByTenant(tenantID string) ([]*SandboxRecord, error)
	Delete(id string) error
	LockSandbox(ctx context.Context, id string) (unlock func(), err error)
}

// CodeSandboxStore is the store of the code sandbox routes. It reads and
// writes code sandboxes only.
type CodeSandboxStore interface {
	Create(record *SandboxRecord) error
	Get(id string) (*SandboxRecord, error)
	// GetByName returns the code sandbox of the tenant that has the name and
	// is not draining, or nil when there is none.
	GetByName(tenantID, name string) (*SandboxRecord, error)
	Delete(id string) error
	LockSandbox(ctx context.Context, id string) (unlock func(), err error)
}

// SweeperStore is the store of the idle sweeper. It lists the sandboxes of
// the kinds it was made with, and reads and writes a sandbox by its kind.
type SweeperStore interface {
	ListForIdleReapDelete(cutoff int64) ([]*SandboxRecord, error)
	ListForIdleReapStop(cutoff int64, ephemeral bool) ([]*SandboxRecord, error)
	GetByIDAndKind(id string, kind Kind) (*SandboxRecord, error)
	UpdateStatusByIDAndKind(id string, kind Kind, status string) error
	DeleteByIDAndKind(id string, kind Kind) error
	LockSandbox(ctx context.Context, id string) (unlock func(), err error)
}

// ActivityStore is the store of the activity recorder.
type ActivityStore interface {
	RecordActivityByKind(ctx context.Context, kind Kind, lastActiveAt map[string]int64) error
}

// NewDefaultSandboxStore returns the default sandbox view of p.
func NewDefaultSandboxStore(p SandboxPersistence) DefaultSandboxStore {
	return kindView{p: p, kind: KindDefault}
}

// NewCodeSandboxStore returns the code sandbox view of p.
func NewCodeSandboxStore(p SandboxPersistence) CodeSandboxStore {
	return codeView{kindView{p: p, kind: KindCode}}
}

// NewSweeperStore returns the view of p that sweeps sandboxes of kinds.
func NewSweeperStore(p SandboxPersistence, kinds []Kind) SweeperStore {
	return sweepView{p: p, kinds: slices.Clone(kinds)}
}

// kindView reads and writes the sandboxes of one kind.
type kindView struct {
	p    SandboxPersistence
	kind Kind
}

func (v kindView) Create(record *SandboxRecord) error {
	if record.Kind != v.kind {
		return fmt.Errorf("store: create %q sandbox in the %s sandbox store", record.Kind, v.kind)
	}
	return v.p.Create(record)
}

func (v kindView) Get(id string) (*SandboxRecord, error) { return v.p.GetByIDAndKind(id, v.kind) }

func (v kindView) IDTaken(id string) (bool, error) { return v.p.ExistsByIDAndKinds(id, AllKinds()) }

func (v kindView) List() ([]*SandboxRecord, error) { return v.p.ListByKind(v.kind) }

func (v kindView) ListByTenant(tenantID string) ([]*SandboxRecord, error) {
	return v.p.ListByTenantAndKind(tenantID, v.kind)
}

func (v kindView) Delete(id string) error { return v.p.DeleteByIDAndKind(id, v.kind) }

func (v kindView) LockSandbox(ctx context.Context, id string) (func(), error) {
	return v.p.LockSandbox(ctx, id)
}

// codeView reads and writes code sandboxes.
type codeView struct{ kindView }

func (v codeView) GetByName(tenantID, name string) (*SandboxRecord, error) {
	return v.p.GetCodeSandboxByName(tenantID, name)
}

// sweepView sweeps the sandboxes of kinds.
type sweepView struct {
	p     SandboxPersistence
	kinds []Kind
}

func (v sweepView) ListForIdleReapDelete(cutoff int64) ([]*SandboxRecord, error) {
	return v.p.ListForIdleReapDeleteByKinds(v.kinds, cutoff)
}

func (v sweepView) ListForIdleReapStop(cutoff int64, ephemeral bool) ([]*SandboxRecord, error) {
	return v.p.ListForIdleReapStopByKinds(v.kinds, cutoff, ephemeral)
}

func (v sweepView) GetByIDAndKind(id string, kind Kind) (*SandboxRecord, error) {
	if err := v.checkSwept(kind); err != nil {
		return nil, err
	}
	return v.p.GetByIDAndKind(id, kind)
}

func (v sweepView) UpdateStatusByIDAndKind(id string, kind Kind, status string) error {
	if err := v.checkSwept(kind); err != nil {
		return err
	}
	return v.p.UpdateStatusByIDAndKind(id, kind, status)
}

func (v sweepView) DeleteByIDAndKind(id string, kind Kind) error {
	if err := v.checkSwept(kind); err != nil {
		return err
	}
	return v.p.DeleteByIDAndKind(id, kind)
}

func (v sweepView) LockSandbox(ctx context.Context, id string) (func(), error) {
	return v.p.LockSandbox(ctx, id)
}

func (v sweepView) checkSwept(kind Kind) error {
	if !slices.Contains(v.kinds, kind) {
		return fmt.Errorf("store: %q sandboxes are not swept here", kind)
	}
	return nil
}

var (
	_ DefaultSandboxStore = kindView{}
	_ CodeSandboxStore    = codeView{}
	_ SweeperStore        = sweepView{}
	_ ActivityStore       = SandboxPersistence(nil)
)
