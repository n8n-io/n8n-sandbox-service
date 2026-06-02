package runtime

import (
	"context"
	"errors"
)

// ErrSandboxNotFound is returned when a sandbox ID is not found.
var ErrSandboxNotFound = errors.New("sandbox not found")

// ErrSandboxNetworkUnavailable is returned when a sandbox exists but has no
// reachable network attachment yet.
var ErrSandboxNetworkUnavailable = errors.New("sandbox network unavailable")

// ErrSandboxNotRunning is returned when a sandbox exists but is not running.
var ErrSandboxNotRunning = errors.New("sandbox not running")

// CreateOptions holds optional parameters for sandbox creation.
type CreateOptions struct{}

// SandboxInfo represents information about a created sandbox.
type SandboxInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	IP   string `json:"ip"`
}

// Capacity reports the current sandbox usage and total schedulable slots.
type Capacity struct {
	Used  int32
	Total int32
}

// Runtime is the sandbox backend contract used by the shared runner process.
type Runtime interface {
	Prepare(ctx context.Context)
	Ready(ctx context.Context) error
	ReadyCh() <-chan struct{}
	Capacity(ctx context.Context) (Capacity, error)

	CreateSandbox(ctx context.Context, sandboxID string, opts *CreateOptions) (*SandboxInfo, error)
	GetSandboxInfo(ctx context.Context, sandboxID string) (*SandboxInfo, error)
	DeleteSandbox(ctx context.Context, sandboxID string) error
	StopSandbox(ctx context.Context, sandboxID string) error
	EnsureSandboxRunning(ctx context.Context, sandboxID string) error
	DaemonURL(ctx context.Context, sandboxID string) (string, error)

	Shutdown(ctx context.Context)
}
