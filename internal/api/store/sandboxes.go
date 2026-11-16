package store

// CodeSandboxStore holds the queries of the code sandbox routes.
type CodeSandboxStore struct{ s SandboxStore }

// NewCodeSandboxStore returns the code sandboxes of s.
func NewCodeSandboxStore(s SandboxStore) *CodeSandboxStore { return &CodeSandboxStore{s: s} }

// Create inserts the record as a code sandbox.
func (c *CodeSandboxStore) Create(record *SandboxRecord) error {
	record.Kind = KindCode
	return c.s.Create(record)
}

// GetByName returns the code sandbox of the tenant that has the name and is
// not draining, or nil when there is none.
func (c *CodeSandboxStore) GetByName(tenantID, name string) (*SandboxRecord, error) {
	return c.s.GetCodeSandboxByName(tenantID, name)
}
