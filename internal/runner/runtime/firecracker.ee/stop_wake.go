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

// transitionPollInterval paces waiting for another lifecycle transition on the
// same sandbox to finish.
const transitionPollInterval = 50 * time.Millisecond

// StopSandbox pauses the microVM, writes a per-sandbox snapshot, tears down host
// VM resources, and frees the runner slot.
func (r *Runtime) StopSandbox(ctx context.Context, sandboxID string) error {
	state, ctx, cancel, err := r.beginTransition(ctx, sandboxID, transitionStopping)
	if err != nil {
		return err
	}
	defer cancel()
	defer r.endTransition(state)

	r.mu.Lock()
	stopped, running := state.stopped, state.running
	r.mu.Unlock()
	if stopped {
		return nil
	}
	if !running {
		return runnerruntime.ErrSandboxNotRunning
	}

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
		// beginTransition detaches the wake from the request that triggered it, so
		// the wake outlives that request. Concurrent callers share one wake, so the
		// wake event carries the trace id of whichever caller started it.
		return nil, r.ensureSandboxRunningOnce(ctx, sandboxID)
	})
	return err
}

func (r *Runtime) ensureSandboxRunningOnce(ctx context.Context, sandboxID string) error {
	state, ctx, cancel, err := r.beginTransition(ctx, sandboxID, transitionWaking)
	if err != nil {
		return err
	}
	defer cancel()
	defer r.endTransition(state)

	r.mu.Lock()
	running, stopped := state.running, state.stopped
	r.mu.Unlock()
	if running {
		return nil
	}
	if !stopped {
		return runnerruntime.ErrSandboxNotRunning
	}

	if err := r.reserveWakeSlot(state); err != nil {
		return err
	}

	timer := newStepTimer(metrics.OpEnsureRunning, r.metrics)
	if err := r.activateSandboxVM(ctx, state, timer); err != nil {
		// The activation may have failed because the wake ran out of budget, so
		// teardown needs a context of its own; the slot is released below either
		// way, and reusing an expired one would hand it back with this sandbox's
		// jail mounts and netns still on it.
		cleanupCtx, cancelCleanup := withCleanupBudget(ctx)
		_ = r.teardownRunningVM(cleanupCtx, state)
		cancelCleanup()
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
	slot := state.slot
	r.mu.Unlock()
	slog.Info("firecracker sandbox woke",
		append([]any{"sandbox_id", sandboxID, "vm_id", state.vmID, "slot", slot}, timer.attrsFor(ctx)...)...)
	return nil
}

// activateSandboxVM prepares jail/netns, starts Firecracker, loads snapshot, and
// exposes the guest daemon through the host proxy. Each phase is timed through
// t, which the caller emits once the operation finishes.
func (r *Runtime) activateSandboxVM(ctx context.Context, state *sandboxState, t *stepTimer) error {
	if err := t.step(stepPrepareJail, func() error { return r.prepareJail(ctx, state) }); err != nil {
		return fmt.Errorf("prepare firecracker jail: %w", err)
	}
	if err := t.step(stepSetupNetwork, func() error { return r.setupNetwork(ctx, state) }); err != nil {
		return fmt.Errorf("setup firecracker network: %w", err)
	}
	var process process
	err := t.step(stepStartJailer, func() error {
		var startErr error
		process, startErr = r.startJailer(ctx, state)
		return startErr
	})
	if err != nil {
		return fmt.Errorf("start firecracker jailer: %w", err)
	}
	state.process = process
	if err := t.step(stepWaitSocket, func() error { return r.waitForSocket(ctx, state.socketPath) }); err != nil {
		return fmt.Errorf("wait for firecracker socket: %w", err)
	}
	if err := t.step(stepLoadSnapshot, func() error {
		return r.deps.loadSnapshot(ctx, state.socketPath, r.config)
	}); err != nil {
		return fmt.Errorf("load firecracker snapshot: %w", err)
	}
	guestAddr := net.JoinHostPort(r.config.GuestIP, fmt.Sprintf("%d", r.config.DaemonPort))
	var proxy daemonProxy
	err = t.step(stepStartProxy, func() error {
		var proxyErr error
		proxy, proxyErr = r.deps.newProxy(ctx, state.daemonURLAddr(), state.netnsName, guestAddr)
		return proxyErr
	})
	if err != nil {
		return fmt.Errorf("start firecracker daemon proxy: %w", err)
	}
	state.proxy = proxy
	if err := t.step(stepProbeDaemon, func() error { return r.deps.probeDaemon(ctx, state.daemonURL) }); err != nil {
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

// beginTransition waits until no other stop, wake, or delete is in flight for the
// sandbox, then claims it for the caller. Callers must release the claim with
// endTransition unless the transition is terminal (delete). The claim is taken
// under a single r.mu hold so exactly one caller can win it, while the poll loop
// keeps waiters off r.mu during the host commands the winner then runs.
//
// It returns the context the operation must run under: transitionBudget applied to
// the caller's, starting once the claim is won so that waiting cannot eat the time
// the operation needs for its host work. Handing the budget back with the claim is
// what keeps the two inseparable, so a new lifecycle operation cannot forget to
// bound itself and strand the sandbox. The wait itself is bounded separately by
// transitionWaitBudget. The returned cancel is nil when an error is returned.
func (r *Runtime) beginTransition(ctx context.Context, sandboxID string, want sandboxTransition) (*sandboxState, context.Context, context.CancelFunc, error) {
	waitCtx, cancelWait := withLifecycleBudget(ctx, transitionWaitBudget())
	defer cancelWait()

	ticker := time.NewTicker(transitionPollInterval)
	defer ticker.Stop()
	for {
		r.mu.Lock()
		state, ok := r.sandboxes[sandboxID]
		if !ok || state.deleting() {
			r.mu.Unlock()
			return nil, nil, nil, runnerruntime.ErrSandboxNotFound
		}
		if state.transition == transitionNone {
			state.transition = want
			r.mu.Unlock()
			opCtx, cancel := withLifecycleBudget(ctx, transitionBudget)
			return state, opCtx, cancel, nil
		}
		r.mu.Unlock()

		select {
		case <-waitCtx.Done():
			return nil, nil, nil, waitCtx.Err()
		case <-ticker.C:
		}
	}
}

// endTransition releases a claim taken by beginTransition.
func (r *Runtime) endTransition(state *sandboxState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state.transition = transitionNone
}

// reserveWakeSlot assigns a free slot to a stopped sandbox and publishes the host
// resources derived from it. The caller must hold the sandbox's wake transition.
func (r *Runtime) reserveWakeSlot(state *sandboxState) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	slot := r.reserveSlotLocked(state.id)
	if slot < 0 {
		return fmt.Errorf("firecracker runner capacity exhausted")
	}
	state.slot = slot
	state.netnsName = fmt.Sprintf("fc-sb-%d", slot)
	state.socketPath = filepath.Join(r.config.JailerBaseDir, "firecracker", state.vmID, "root", "firecracker.socket")
	state.daemonURL = fmt.Sprintf("http://%s", net.JoinHostPort(r.config.ProxyListenIP, fmt.Sprintf("%d", r.config.ProxyPortStart+slot)))
	return nil
}

func (r *Runtime) ensureDiskSpaceForSnapshot(ctx context.Context, state *sandboxState) error {
	needed, err := snapshotWriteBytes(state.snapshotMemPath)
	if err != nil {
		return err
	}
	needed += stoppedSnapshotHeadroomBytes

	r.mu.Lock()
	attempts := len(r.sandboxes) + 1
	r.mu.Unlock()

	for attempt := 0; attempt < attempts; attempt++ {
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
		if !state.stopped || state.running {
			continue
		}
		// A waking sandbox is still flagged stopped until its microVM is up.
		// Evicting it here would delete its data dir out from under the wake.
		// This also skips sandboxes already claimed for delete.
		if state.transition != transitionNone {
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
	oldest.transition = transitionDeleting
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
