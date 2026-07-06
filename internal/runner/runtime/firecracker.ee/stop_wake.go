package firecracker

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/n8n-io/sandbox-service/internal/metrics"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// stoppedSnapshotHeadroomBytes is extra free-space reserved on top of the
// current per-sandbox snapshot_mem size before StopSandbox writes a new full
// snapshot. snapshot/create rewrites mem/state in place and may briefly need
// more disk than the prior file size (metadata, partial writes, filesystem
// rounding). The headroom avoids failing stop on a full volume when free space
// only barely equals the existing snapshot size.
const stoppedSnapshotHeadroomBytes = 64 << 20 // 64 MiB

// StopSandbox pauses the microVM, writes a per-sandbox snapshot, tears down host
// VM resources, and frees the runner slot.
func (r *Runtime) StopSandbox(ctx context.Context, sandboxID string) error {
	state, err := r.sandboxForLifecycle(sandboxID)
	if err != nil {
		return err
	}
	if state.stopped {
		return nil
	}
	if !state.running {
		return runnerruntime.ErrSandboxNotRunning
	}

	r.mu.Lock()
	if state.stopping {
		r.mu.Unlock()
		return nil
	}
	state.stopping = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		state.stopping = false
		r.mu.Unlock()
	}()

	if err := r.ensureDiskSpaceForSnapshot(ctx, state); err != nil {
		return err
	}
	if err := r.deps.pauseVM(ctx, state.socketPath); err != nil {
		return fmt.Errorf("pause firecracker vm: %w", err)
	}
	if err := r.deps.createSnapshot(ctx, state.socketPath); err != nil {
		return fmt.Errorf("create firecracker snapshot: %w", err)
	}
	if err := r.teardownRunningVM(ctx, state); err != nil {
		return err
	}

	r.mu.Lock()
	state.running = false
	state.stopped = true
	state.stoppedAt = time.Now()
	if state.slot >= 0 && r.slotOwnedByLocked(state.slot, state.id) {
		r.releaseSlotLocked(state.slot)
	}
	state.slot = -1
	state.process = nil
	state.proxy = nil
	r.mu.Unlock()

	slog.Info("firecracker sandbox stopped", "sandbox_id", sandboxID, "vm_id", state.vmID)
	return nil
}

// EnsureSandboxRunning restores a stopped sandbox from its per-sandbox snapshot.
func (r *Runtime) EnsureSandboxRunning(ctx context.Context, sandboxID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err, _ := r.wakeGroup.Do(sandboxID, func() (interface{}, error) {
		wakeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		return nil, r.ensureSandboxRunningOnce(wakeCtx, sandboxID)
	})
	return err
}

func (r *Runtime) ensureSandboxRunningOnce(ctx context.Context, sandboxID string) error {
	if err := r.waitWhileStopping(ctx, sandboxID); err != nil {
		return err
	}
	state, err := r.sandboxForLifecycle(sandboxID)
	if err != nil {
		return err
	}
	if state.running {
		return nil
	}
	if !state.stopped {
		return runnerruntime.ErrSandboxNotRunning
	}

	slot, netnsName, socketPath, daemonURL, err := r.reserveWakeSlotLocked(sandboxID)
	if err != nil {
		return err
	}
	state.slot = slot
	state.netnsName = netnsName
	state.socketPath = socketPath
	state.daemonURL = daemonURL

	if err := r.activateSandboxVM(ctx, state); err != nil {
		_ = r.teardownRunningVM(ctx, state)
		r.mu.Lock()
		state.running = false
		state.stopped = true
		if state.slot >= 0 && r.slotOwnedByLocked(state.slot, state.id) {
			r.releaseSlotLocked(state.slot)
		}
		state.slot = -1
		state.process = nil
		state.proxy = nil
		r.mu.Unlock()
		return err
	}

	r.mu.Lock()
	state.running = true
	state.stopped = false
	r.mu.Unlock()
	slog.Info("firecracker sandbox woke", "sandbox_id", sandboxID, "vm_id", state.vmID, "slot", slot)
	return nil
}

// activateSandboxVM prepares jail/netns, starts Firecracker, loads snapshot, and
// exposes the guest daemon through the host proxy.
func (r *Runtime) activateSandboxVM(ctx context.Context, state *sandboxState) error {
	if err := r.prepareJail(ctx, state); err != nil {
		return fmt.Errorf("prepare firecracker jail: %w", err)
	}
	if err := r.setupNetwork(ctx, state); err != nil {
		return fmt.Errorf("setup firecracker network: %w", err)
	}
	process, err := r.startJailer(ctx, state)
	if err != nil {
		return fmt.Errorf("start firecracker jailer: %w", err)
	}
	state.process = process
	if err := r.waitForSocket(ctx, state.socketPath); err != nil {
		return fmt.Errorf("wait for firecracker socket: %w", err)
	}
	if err := r.deps.loadSnapshot(ctx, state.socketPath, r.config); err != nil {
		return fmt.Errorf("load firecracker snapshot: %w", err)
	}
	guestAddr := net.JoinHostPort(r.config.GuestIP, fmt.Sprintf("%d", r.config.DaemonPort))
	proxy, err := r.deps.newProxy(ctx, state.daemonURLAddr(), state.netnsName, guestAddr)
	if err != nil {
		return fmt.Errorf("start firecracker daemon proxy: %w", err)
	}
	state.proxy = proxy
	if err := r.deps.probeDaemon(ctx, state.daemonURL); err != nil {
		return fmt.Errorf("connect to firecracker daemon: %w", err)
	}
	return nil
}

// teardownRunningVM stops proxy, jailer, and jail state without deleting sandbox data.
func (r *Runtime) teardownRunningVM(ctx context.Context, state *sandboxState) error {
	var errs []error
	if state.proxy != nil {
		if err := state.proxy.Stop(); err != nil {
			errs = append(errs, fmt.Errorf("stop daemon proxy: %w", err))
		}
		state.proxy = nil
	}
	if state.process != nil {
		if err := state.process.Kill(); err != nil && !containsProcessFinished(err) {
			errs = append(errs, fmt.Errorf("kill firecracker process: %w", err))
		}
		state.process = nil
	}
	if err := r.cleanupHost(ctx, state); err != nil {
		errs = append(errs, fmt.Errorf("cleanup firecracker host state: %w", err))
	}
	return joinErrors(errs)
}

func (r *Runtime) sandboxForLifecycle(sandboxID string) (*sandboxState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	state, ok := r.sandboxes[sandboxID]
	if !ok || state.deleting {
		return nil, runnerruntime.ErrSandboxNotFound
	}
	return state, nil
}

// waitWhileStopping blocks until StopSandbox finishes for the sandbox.
func (r *Runtime) waitWhileStopping(ctx context.Context, sandboxID string) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		r.mu.Lock()
		state, ok := r.sandboxes[sandboxID]
		stopping := ok && !state.deleting && state.stopping
		r.mu.Unlock()
		if !ok {
			return runnerruntime.ErrSandboxNotFound
		}
		if !stopping {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// reserveWakeSlotLocked assigns a free slot to a stopped sandbox. r.mu must be held.
func (r *Runtime) reserveWakeSlotLocked(sandboxID string) (slot int, netnsName, socketPath, daemonURL string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	state, ok := r.sandboxes[sandboxID]
	if !ok || state.deleting {
		return 0, "", "", "", runnerruntime.ErrSandboxNotFound
	}
	if state.running {
		return 0, "", "", "", nil
	}
	slot = r.reserveSlotLocked(sandboxID)
	if slot < 0 {
		return 0, "", "", "", fmt.Errorf("firecracker runner capacity exhausted")
	}
	netnsName = fmt.Sprintf("fc-sb-%d", slot)
	socketPath = filepath.Join(r.config.JailerBaseDir, "firecracker", state.vmID, "root", "firecracker.socket")
	daemonURL = fmt.Sprintf("http://%s", net.JoinHostPort(r.config.ProxyListenIP, fmt.Sprintf("%d", r.config.ProxyPortStart+slot)))
	return slot, netnsName, socketPath, daemonURL, nil
}

func (r *Runtime) ensureDiskSpaceForSnapshot(ctx context.Context, state *sandboxState) error {
	needed, err := snapshotWriteBytes(state.snapshotMemPath)
	if err != nil {
		return err
	}
	needed += stoppedSnapshotHeadroomBytes

	for attempt := 0; attempt < len(r.sandboxes)+1; attempt++ {
		free, err := r.deps.freeBytesInDir(r.runnerConfig.DataDir)
		if err != nil {
			return err
		}
		if free >= needed {
			return nil
		}
		if !r.evictOldestStoppedSandbox(ctx) {
			return fmt.Errorf("insufficient disk space for firecracker snapshot")
		}
	}
	return fmt.Errorf("insufficient disk space for firecracker snapshot")
}

func (r *Runtime) evictOldestStoppedSandbox(ctx context.Context) bool {
	r.mu.Lock()
	var candidates []*sandboxState
	for _, state := range r.sandboxes {
		if state.deleting || !state.stopped || state.running {
			continue
		}
		candidates = append(candidates, state)
	}
	if len(candidates) == 0 {
		r.mu.Unlock()
		return false
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].stoppedAt.Before(candidates[j].stoppedAt)
	})
	oldest := candidates[0]
	oldest.deleting = true
	r.mu.Unlock()

	slog.Warn(
		"evicting oldest stopped firecracker sandbox for disk space",
		"sandbox_id", oldest.id,
		"stopped_at", oldest.stoppedAt,
	)
	if r.metrics != nil {
		r.metrics.ObserveContainerOp(metrics.OpEvict, true, 0)
	}
	_ = r.deleteSandbox(ctx, oldest)
	return true
}

func snapshotWriteBytes(memPath string) (int64, error) {
	info, err := os.Stat(memPath)
	if err != nil {
		return 0, fmt.Errorf("stat snapshot mem: %w", err)
	}
	return info.Size(), nil
}

func freeBytesInDir(path string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	return int64(stat.Bavail) * int64(stat.Bsize), nil
}

func containsProcessFinished(err error) bool {
	return err != nil && (err.Error() == "process already finished" || err == os.ErrProcessDone)
}
