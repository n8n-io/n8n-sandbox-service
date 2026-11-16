package store

import (
	"context"
	"errors"
)

// kindStore reads and writes the sandboxes of one kind.
type kindStore struct {
	s    Store
	kind Kind
}

func (k kindStore) filter() SandboxFilter { return KindFilter(k.kind) }

// Create inserts the record as a sandbox of the store's kind.
func (k kindStore) Create(record *SandboxRecord) error {
	record.Kind = k.kind
	return k.s.Create(record)
}

func (k kindStore) Get(id string) (*SandboxRecord, error) { return k.s.Get(id, k.filter()) }

func (k kindStore) List() ([]*SandboxRecord, error) { return k.s.List(k.filter()) }

// ListByTenant lists the sandboxes of tenantID, which is required.
func (k kindStore) ListByTenant(tenantID string) ([]*SandboxRecord, error) {
	if tenantID == "" {
		return nil, errors.New("store: no tenant given")
	}
	return k.s.List(SandboxFilter{Kinds: []Kind{k.kind}, TenantID: tenantID})
}

func (k kindStore) Delete(id string) error { return k.s.Delete(id, k.filter()) }

func (k kindStore) LockSandbox(ctx context.Context, id string) (func(), error) {
	return k.s.LockSandbox(ctx, id)
}

// DefaultSandboxStore is the store of the default sandbox routes. It reads and
// writes default sandboxes only.
type DefaultSandboxStore struct{ kindStore }

// NewDefaultSandboxStore returns the default sandboxes of s.
func NewDefaultSandboxStore(s Store) *DefaultSandboxStore {
	return &DefaultSandboxStore{kindStore{s: s, kind: KindDefault}}
}

// IDTaken reports whether a sandbox of any kind has the id, for a create
// whose client picks the id.
func (d *DefaultSandboxStore) IDTaken(id string) (bool, error) {
	rec, err := d.s.Get(id, KindFilter(AllKinds()...))
	return rec != nil, err
}

// CodeSandboxStore is the store of the code sandbox routes. It reads and
// writes code sandboxes only.
type CodeSandboxStore struct{ kindStore }

// NewCodeSandboxStore returns the code sandboxes of s.
func NewCodeSandboxStore(s Store) *CodeSandboxStore {
	return &CodeSandboxStore{kindStore{s: s, kind: KindCode}}
}

// GetByName returns the code sandbox of the tenant that has the name and is
// not draining, or nil when there is none.
func (c *CodeSandboxStore) GetByName(tenantID, name string) (*SandboxRecord, error) {
	return c.s.GetCodeSandboxByName(tenantID, name)
}
