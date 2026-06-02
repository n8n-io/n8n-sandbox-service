package firecracker

import (
	"context"
	"errors"
	"fmt"

	"github.com/n8n-io/sandbox-service/internal/runner/config"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

var errNotImplemented = errors.New("firecracker runtime is not implemented yet")

// Runtime is a placeholder Firecracker backend. It wires the runner process to
// the future Firecracker implementation without accepting sandbox work yet.
type Runtime struct {
	cfg     *config.Config
	readyCh chan struct{}
}

var _ runnerruntime.Runtime = (*Runtime)(nil)

func New(cfg *config.Config) *Runtime {
	return &Runtime{
		cfg:     cfg,
		readyCh: make(chan struct{}),
	}
}

func (r *Runtime) Prepare(context.Context) {}

func (r *Runtime) Ready(context.Context) error {
	return errNotImplemented
}

func (r *Runtime) ReadyCh() <-chan struct{} {
	return r.readyCh
}

func (r *Runtime) Capacity(context.Context) (runnerruntime.Capacity, error) {
	return runnerruntime.Capacity{Total: r.cfg.CapacityTotal}, nil
}

func (r *Runtime) CreateSandbox(context.Context, string, *runnerruntime.CreateOptions) (*runnerruntime.SandboxInfo, error) {
	return nil, errNotImplemented
}

func (r *Runtime) GetSandboxInfo(context.Context, string) (*runnerruntime.SandboxInfo, error) {
	return nil, runnerruntime.ErrSandboxNotFound
}

func (r *Runtime) DeleteSandbox(context.Context, string) error {
	return runnerruntime.ErrSandboxNotFound
}

func (r *Runtime) StopSandbox(context.Context, string) error {
	return runnerruntime.ErrSandboxNotFound
}

func (r *Runtime) EnsureSandboxRunning(context.Context, string) error {
	return fmt.Errorf("%w: %w", errNotImplemented, runnerruntime.ErrSandboxNotFound)
}

func (r *Runtime) DaemonURL(context.Context, string) (string, error) {
	return "", runnerruntime.ErrSandboxNotFound
}

func (r *Runtime) Shutdown(context.Context) {}
