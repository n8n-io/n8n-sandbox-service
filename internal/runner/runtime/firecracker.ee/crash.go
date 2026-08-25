package firecracker

import (
	"context"
	"errors"
	"log/slog"
	"time"
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
// usable snapshot: stopped, holding no slot, and still pinned to mustColdBoot by
// the restore that resumed this guest. Every path that already handles a stopped
// sandbox then behaves without knowing a crash happened — StopSandbox is
// idempotent instead of looping on a dead API socket, the idle sweeper can delete
// it, and DaemonURL reports it not running so a request drives the wake path,
// which refuses it with errGuestCrashed. Freeing the slot here is what bounds the
// damage: a crashed sandbox costs nothing but disk, so a client retrying cannot
// accumulate slots.
func (r *Runtime) handleGuestDeath(died *sandboxState, generation uint64, waitErr error) {
	if !r.recordGuestDeath(died, generation, waitErr) {
		return
	}

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

	// Stale means something tore this incarnation down while this call waited for
	// the claim. The case that matters is a wake that failed because of this very
	// death: its rollback has already stopped the sandbox and freed the slot, and
	// the restore pinned it, so nothing is left to do here.
	r.mu.Lock()
	stale := state != died || state.generation != generation
	r.mu.Unlock()
	if stale {
		return
	}

	if err := r.teardownRunningVM(ctx, state); err != nil {
		// Reported, not returned: the slot has to go back even when host cleanup
		// leaves a mount or a netns behind, and startup reconcile removes those.
		slog.Warn("firecracker crashed sandbox teardown failed", "sandbox_id", state.id, "err", err)
	}

	r.mu.Lock()
	state.running = false
	state.stopped = true
	state.stoppedAt = time.Now()
	if state.slot >= 0 && r.slotOwnedByLocked(state.slot, state.id) {
		r.releaseSlotLocked(state.slot)
	}
	state.slot = -1
	r.mu.Unlock()
}

// recordGuestDeath logs and counts a guest that died on its own, reporting
// whether this exit was one the runner did not ask for.
//
// It runs before the transition claim is contended for, because the claim is what
// makes the generation unreliable: an exit that arrives while a lifecycle
// operation holds the sandbox is only checked once that operation has released it,
// by which point its own teardown has bumped the generation past the one the exit
// carries. A wake that fails because its guest just died goes exactly that way, so
// checking after the claim would drop the death that caused it.
//
// False for an exit the runner caused: teardownRunningVM bumps the generation
// before killing, so a stop, delete, wake rollback or shutdown of a live guest
// reaches here with a generation that no longer matches.
func (r *Runtime) recordGuestDeath(state *sandboxState, generation uint64, waitErr error) bool {
	r.mu.Lock()
	current, slot, vmID := state.generation, state.slot, state.vmID
	r.mu.Unlock()
	if current != generation {
		return false
	}

	slog.Error("firecracker guest died",
		"sandbox_id", state.id,
		"vm_id", vmID,
		"slot", slot,
		"err", waitErr,
	)
	r.metrics.ObserveGuestDeath()
	return true
}
