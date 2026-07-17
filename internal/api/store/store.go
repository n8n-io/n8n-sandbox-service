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
}

// SandboxStore exposes CRUD operations for SandboxRecord rows.
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
	List() ([]*SandboxRecord, error)
	Backend() Backend
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
	)
	if err != nil {
		return nil, err
	}
	return &r, nil
}
