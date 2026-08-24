package firecracker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/n8n-io/sandbox-service/internal/metrics"
)

// errGuestCrashed is what the wake path returns for a sandbox pinned to cold
// boot. Its files are intact and it is meant to come back, but only through a
// boot of its rootfs, which this runner cannot do yet; restoring the snapshot
// instead would corrupt the disk. Waking it is therefore refused rather than
// attempted, and the sandbox stays deletable.
var errGuestCrashed = errors.New("sandbox guest crashed and cannot be restored from its snapshot")

// handleGuestDeath reacts to a Firecracker process that exited without the
// runner killing it. It runs on the goroutine watching that process, so it takes
// the sandbox's transition claim like any other lifecycle operation: a stop or
// delete already in flight finishes first, and its own teardown then bumps the
// generation this call is checked against, which is how the two cannot both tear
// the same microVM down.
//
// What it leaves behind is exactly what an idle stop leaves behind, minus a
// usable snapshot: stopped, holding no slot, pinned to mustColdBoot. Every path
// that already handles a stopped sandbox then behaves without knowing a crash
// happened — StopSandbox is idempotent instead of looping on a dead API socket,
// the idle sweeper can delete it, and DaemonURL reports it not running so a
// request drives the wake path, which refuses it with errGuestCrashed. Freeing
// the slot here is what bounds the damage: a crashed sandbox costs nothing but
// disk, so a client retrying cannot accumulate slots.
func (r *Runtime) handleGuestDeath(died *sandboxState, generation uint64, waitErr error) {
	// Detached from any request: the process this reacts to has already exited,
	// and beginTransition caps both the wait for the claim and the teardown.
	state, ctx, cancel, err := r.beginTransition(context.Background(), died.id, transitionStopping)
	if err != nil {
		// Either the sandbox is gone, which needs nothing, or an operation held it
		// past the wait budget, which is long enough that something else is wrong.
		slog.Debug("firecracker guest death not claimable", "sandbox_id", died.id, "err", err)
		return
	}
	defer cancel()
	defer r.endTransition(state)

	r.mu.Lock()
	stale := state != died || state.generation != generation
	slot, vmID := state.slot, state.vmID
	r.mu.Unlock()
	if stale {
		return
	}

	slog.Error("firecracker guest died",
		"sandbox_id", state.id,
		"vm_id", vmID,
		"slot", slot,
		"err", waitErr,
	)
	r.metrics.ObserveGuestDeath(metrics.BackendFirecracker)

	if err := r.teardownRunningVM(ctx, state); err != nil {
		// Reported, not returned: the slot has to go back even when host cleanup
		// leaves a mount or a netns behind, and startup reconcile removes those.
		slog.Warn("firecracker crashed sandbox teardown failed", "sandbox_id", state.id, "err", err)
	}

	r.mu.Lock()
	state.running = false
	state.stopped = true
	state.mustColdBoot = true
	state.stoppedAt = time.Now()
	if state.slot >= 0 && r.slotOwnedByLocked(state.slot, state.id) {
		r.releaseSlotLocked(state.slot)
	}
	state.slot = -1
	r.mu.Unlock()
}
