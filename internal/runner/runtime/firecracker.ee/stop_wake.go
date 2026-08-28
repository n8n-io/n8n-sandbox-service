package firecracker

import (
	"context"
	"errors"
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
	fcnetwork "github.com/n8n-io/sandbox-service/internal/runner/runtime/firecracker.ee/network"
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
	// Taken from a paused guest, so this snapshot and the rootfs describe the same
	// moment. That makes this the one point at which restoring becomes safe again.
	r.mu.Lock()
	state.mustColdBoot = false
	r.mu.Unlock()

	// Reported, not returned as a reason to abandon the stop. The snapshot is
	// written and the microVM is gone whatever this leaves on the host, and
	// teardown has already dropped the process and proxy handles, so no retry can
	// pick up where it stopped. Keeping the sandbox marked running would hold its
	// slot for a microVM that no longer exists, hand out a daemon URL nothing
	// listens on, and fail every later stop against an API socket that died with
	// its guest, so nothing would ever reclaim it. The slot is safe to hand back
	// regardless of what cleanup left, because the next sandbox on it clears it
	// first: setupNetwork deletes both per-slot host names, netns and veth, before
	// creating them. What is left is keyed to the vmID, which reconcile removes.
	//
	// A failed delete keeps its slot instead, because a delete retry does arrive to
	// reclaim it (see the README); a stop has no such retry.
	teardownErr := r.teardownRunningVM(ctx, state)
	if teardownErr != nil {
		slog.Warn("firecracker stopped sandbox teardown failed", "sandbox_id", sandboxID, "err", teardownErr)
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
	return teardownErr
}

// EnsureSandboxRunning brings a stopped sandbox back: normally by restoring its
// per-sandbox snapshot, and for one whose guest died by cold booting its rootfs.
//
// The recovery flag comes back through the singleflight, which is what makes every
// request that arrived during a crash learn about it rather than only the one that
// happened to trigger the recovery.
func (r *Runtime) EnsureSandboxRunning(ctx context.Context, sandboxID string) (runnerruntime.WakeResult, error) {
	if err := ctx.Err(); err != nil {
		return runnerruntime.WakeResult{}, err
	}
	recovered, err, _ := r.wakeGroup.Do(sandboxID, func() (interface{}, error) {
		// beginTransition detaches the wake from the request that triggered it, so
		// the wake outlives that request. Concurrent callers share one wake, so the
		// wake event carries the trace id of whichever caller started it.
		recovering, err := r.ensureSandboxRunningOnce(ctx, sandboxID)
		// Inside the singleflight, which is what counts one recovery per crash rather
		// than one per request the crash stranded. Here rather than in
		// ensureSandboxRunningOnce so that every way that call can fail is counted,
		// including running out of capacity before the cold boot is even attempted.
		if recovering {
			r.metrics.ObserveRecovery(err == nil)
		}
		return recovering, err
	})
	// Comma-ok: a wake that failed before it knew whether it was a recovery returns
	// no value at all.
	wasRecovery, _ := recovered.(bool)
	return runnerruntime.WakeResult{Recovered: wasRecovery}, err
}

// ensureSandboxRunningOnce reports whether the sandbox came back through recovery,
// which is a cold boot on its existing rootfs: its files are intact, but everything
// that was in memory — running processes, execution history — is gone, and that loss
// is what the caller has to tell the client about.
func (r *Runtime) ensureSandboxRunningOnce(ctx context.Context, sandboxID string) (bool, error) {
	state, ctx, cancel, err := r.beginTransition(ctx, sandboxID, transitionWaking)
	if err != nil {
		return false, err
	}
	defer cancel()
	defer r.endTransition(state)

	r.mu.Lock()
	running, stopped, recovering := state.running, state.stopped, state.mustColdBoot
	r.mu.Unlock()
	if running {
		return false, nil
	}
	if !stopped {
		return false, runnerruntime.ErrSandboxNotRunning
	}

	// From here on failures return `recovering` rather than false: a recovery that
	// could not get a slot, or whose cold boot failed, is still a recovery attempt,
	// and a caller that could not tell would meter it as an ordinary wake.
	if err := r.reserveWakeSlot(state); err != nil {
		return recovering, err
	}

	// Recovery is timed under its own operation: it cold boots where a wake restores,
	// so sharing ensure_running's series would blur both. The step names differ too,
	// which is what makes the split visible in the per-step histogram.
	op := metrics.OpEnsureRunning
	if recovering {
		op = metrics.OpRecover
	}
	timer := newStepTimer(op, r.metrics)
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
		return recovering, err
	}

	r.mu.Lock()
	state.running = true
	state.stopped = false
	slot := state.slot
	r.mu.Unlock()
	slog.Info("firecracker sandbox woke",
		append([]any{"sandbox_id", sandboxID, "vm_id", state.vmID, "slot", slot, "recovered", recovering}, timer.attrsFor(ctx)...)...)
	return recovering, nil
}

// errActivationAbandoned ends an activation whose sandbox stopped being the
// runner's own while its microVM was starting. It is returned after the handle is
// published, never instead of publishing it, so the caller's rollback finds the
// microVM and kills it.
var errActivationAbandoned = errors.New("sandbox was torn down while its microVM was starting")

// activationAbandonedLocked reports whether the sandbox being activated has been
// taken away from this activation. Only Shutdown can do that: every other
// lifecycle operation waits for the activation's claim before touching the
// sandbox, while Shutdown overwrites the claim and deletes the sandbox rather than
// let a microVM outlive the runner. r.mu must be held.
func (r *Runtime) activationAbandonedLocked(state *sandboxState) bool {
	current, ok := r.sandboxes[state.id]
	return !ok || current != state || state.deleting()
}

// activateSandboxVM prepares jail/netns, starts Firecracker, loads snapshot, and
// exposes the guest daemon through the host proxy. Each phase is timed through
// t, which the caller emits once the operation finishes.
//
// Both handles are published before that ownership is rechecked, and a lost
// sandbox fails the activation rather than carrying on. Shutdown does not wait for
// the claim this runs under, so it can delete the sandbox and hand its slot back
// while the microVM is still coming up — and it decides whether to tear the microVM
// down by reading handles that do not exist yet. Carrying on would finish building
// a guest nothing tracks: jailer's children are their own process group, so it
// would outlive the runner, holding the netns and jail mounts of a slot already
// handed to someone else. Failing routes it into the caller's rollback, which is
// why the handle is published first — a rollback only tears down what it can see.
func (r *Runtime) activateSandboxVM(ctx context.Context, state *sandboxState, t *stepTimer) error {
	if err := t.step(stepPrepareJail, func() error { return r.prepareJail(ctx, state) }); err != nil {
		return fmt.Errorf("prepare firecracker jail: %w", err)
	}
	if err := t.step(stepSetupNetwork, func() error { return r.setupNetwork(ctx, state) }); err != nil {
		return fmt.Errorf("setup firecracker network: %w", err)
	}
	r.mu.Lock()
	generation := state.generation
	r.mu.Unlock()
	var process process
	err := t.step(stepStartJailer, func() error {
		var startErr error
		process, startErr = r.startJailer(ctx, state, func(waitErr error) {
			r.handleGuestDeath(state, generation, waitErr)
		})
		return startErr
	})
	if err != nil {
		return fmt.Errorf("start firecracker jailer: %w", err)
	}
	// Published under the lock because Shutdown reads it without waiting for this
	// activation's claim, and has to see a handle it can kill rather than a torn
	// read that lets the microVM outlive the runner.
	r.mu.Lock()
	state.process = process
	abandoned := r.activationAbandonedLocked(state)
	r.mu.Unlock()
	if abandoned {
		return errActivationAbandoned
	}
	if err := t.step(stepWaitSocket, func() error { return r.waitForSocket(ctx, state.socketPath) }); err != nil {
		return fmt.Errorf("wait for firecracker socket: %w", err)
	}
	if err := r.startGuest(ctx, state, t); err != nil {
		return err
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
	r.mu.Lock()
	state.proxy = proxy
	abandoned = r.activationAbandonedLocked(state)
	r.mu.Unlock()
	if abandoned {
		return errActivationAbandoned
	}
	if err := t.step(stepProbeDaemon, func() error { return r.deps.probeDaemon(ctx, state.daemonURL) }); err != nil {
		return fmt.Errorf("connect to firecracker daemon: %w", err)
	}
	return nil
}

// startGuest gets the guest running, from its snapshot or from its own rootfs.
//
// The pin decides which, and both outcomes leave it set: a restored guest resumes
// against a rootfs its snapshot stops describing the moment it runs, and a cold
// booted one never had a matching snapshot to begin with. So after this returns the
// sandbox is always pinned, and only the snapshot StopSandbox takes of the paused
// guest clears it — one rule for both paths, rather than a pin that has to be
// reasoned about per entry point.
//
// The restore is pinned before the request rather than after it, because that one
// call both loads the snapshot and resumes the guest: a load that resumes and then
// fails to report it — a deadline that expires while the response is read, a dropped
// connection — would otherwise roll back with the pin unset, and the next wake would
// restore that same snapshot onto a rootfs the guest had moved on from and corrupt it
// silently, which is the one outcome nothing downstream can detect. The cost is that
// a load which failed without resuming anything is pinned too, and pays a cold boot
// it did not need. Pinning here rather than earlier keeps that cost small: every step
// before this one fails with no guest having run, so those failures leave the sandbox
// restorable.
func (r *Runtime) startGuest(ctx context.Context, state *sandboxState, t *stepTimer) error {
	r.mu.Lock()
	coldBoot, params, kernel := state.mustColdBoot, state.bootParams, state.kernel
	state.mustColdBoot = true
	r.mu.Unlock()

	if coldBoot {
		// Checked on this branch alone: a restore boots the kernel out of its memory
		// image and never opens the file, so a wake that has a snapshot to return to
		// has no reason to care what the template holds now.
		if err := r.verifyKernelPin(kernel); err != nil {
			return err
		}
		if err := t.step(stepColdBoot, func() error {
			return r.deps.coldBoot(ctx, state.socketPath, params)
		}); err != nil {
			return fmt.Errorf("cold boot firecracker vm: %w", err)
		}
		return nil
	}
	if err := t.step(stepLoadSnapshot, func() error {
		return r.deps.loadSnapshot(ctx, state.socketPath, r.config)
	}); err != nil {
		return fmt.Errorf("load firecracker snapshot: %w", err)
	}
	return nil
}

// teardownRunningVM stops proxy, jailer, and jail state without deleting sandbox data.
//
// The generation bump has to come before the kill: it is what tells the exit
// callback of the process being killed here that the runner asked for this exit,
// so an ordinary stop, delete or wake rollback is not reported as a crash.
//
// The handles are taken off the state in the same critical section, which is what
// makes two teardowns of one microVM safe. Normally the transition claim keeps
// them apart, but Shutdown deliberately does not wait for a claim it finds held —
// it would rather race a stop, wake or guest death than leave a microVM running
// past the runner. Whoever takes the handles owns the stop and the kill; the other
// caller finds nil and only repeats cleanupHost, which is written to be repeatable.
func (r *Runtime) teardownRunningVM(ctx context.Context, state *sandboxState) error {
	r.mu.Lock()
	state.generation++
	proxy, process := state.proxy, state.process
	state.proxy, state.process = nil, nil
	r.mu.Unlock()

	var errs []error
	if proxy != nil {
		if err := proxy.Stop(); err != nil {
			errs = append(errs, fmt.Errorf("stop daemon proxy: %w", err))
		}
	}
	if process != nil {
		if err := process.Kill(); err != nil && !containsProcessFinished(err) {
			errs = append(errs, fmt.Errorf("kill firecracker process: %w", err))
		}
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
	state.hostVeth = fcnetwork.HostVethName(slot)
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
