package firecracker

import (
	"context"
	"fmt"
	"log/slog"

	fcnetwork "github.com/n8n-io/sandbox-service/internal/runner/runtime/firecracker.ee/network"
)

// The slot wirer moves the per-slot network build off the create and wake path.
//
// Nothing in SetupScript is specific to the sandbox that will use the slot: the
// TAP name and address, the guest IP, the uplink subnet and the egress rules all
// derive from runner config or the slot index. So a slot's namespace can be built
// before any sandbox is assigned to it. wireSlots builds every free slot at
// startup and rebuilds a slot after each release; setupNetwork, which activation
// still calls, finds the slot wired and skips the build. The one sandbox-specific
// rule, the egress "none" DROP, is added by activation on top of the built
// namespace and deleted with it, so the pool only holds the default policy.
//
// Every sandbox still gets a fresh namespace: teardown deletes the slot's netns
// and veth as before and marks the slot unwired, and the wirer rebuilds it from
// scratch. Readiness does not wait for wiring; a slot the wirer has not reached is
// built inline by the sandbox that lands on it, as every create did before.
// Shutdown clears the namespaces of free slots, best-effort; startup reconcile
// sweeps whatever that leaves. The slots_unwired gauge reports how many slots
// are not built at any moment; zero means the wirer has nothing left to do.

// setupNetwork makes sure the slot's network namespace exists with TAP, veth
// uplink, and per-netns egress iptables matching the Docker private-CIDR policy,
// building it if the slot is unwired. With blockEgress it then inserts the DROP
// ahead of those rules; on a wired slot that insert is all it runs.
//
// The slot's netMu is held throughout. That is what keeps two builders off one
// slot, and what makes the wired flag trustworthy to whoever waits: a sandbox
// that blocks here behind the wirer is handed the lock with the flag set and the
// namespace in place. Holding it through the DROP means nothing can rebuild the
// namespace in between.
func (r *Runtime) setupNetwork(ctx context.Context, slot int, blockEgress bool) error {
	s := &r.slots[slot]
	s.netMu.Lock()
	defer s.netMu.Unlock()
	if !s.wired.Load() {
		if err := r.ensureHostNATReady(ctx); err != nil {
			return fmt.Errorf("host NAT not configured: %w", err)
		}
		script := fcnetwork.SetupScript(slot, r.config.HostTapDeviceName, r.config.HostTapIPCIDR)
		if err := r.deps.run(ctx, "sudo", "/bin/sh", "-c", script); err != nil {
			return err
		}
		s.wired.Store(true)
	}
	if !blockEgress {
		return nil
	}
	script := fcnetwork.BlockEgressScript(slot, r.config.HostTapDeviceName)
	if err := r.deps.run(ctx, "sudo", "/bin/sh", "-c", script); err != nil {
		return fmt.Errorf("block egress: %w", err)
	}
	return nil
}

// unwiredSlots counts the slots whose namespace is not currently built. Lock-free,
// so a scrape never waits behind a build.
func (r *Runtime) unwiredSlots() int {
	n := 0
	for i := range r.slots {
		if !r.slots[i].wired.Load() {
			n++
		}
	}
	return n
}

// wireSlots builds the namespace of every free slot, then waits for a release
// to run again, until ctx ends. Prepare starts it in its own goroutine.
//
// A slot found occupied is left alone: the sandbox on it has either built it or
// is about to. Occupancy is read under r.mu but not held across the build, so a
// create can reserve the slot in between; that is safe, because setupNetwork is
// what both of them run, and it serialises them on the slot's own lock.
//
// Each build runs under transitionBudget so a wedged ip or iptables cannot hold
// a slot's netMu, and with it any sandbox activating on that slot, past what an
// inline build could today. A failed build is logged and skipped: the next
// sandbox on the slot builds inline and reports the error to its caller.
func (r *Runtime) wireSlots(ctx context.Context) {
	for {
		for slot := range r.slots {
			if ctx.Err() != nil {
				return
			}
			r.mu.Lock()
			occupied := r.slots[slot].occupied()
			r.mu.Unlock()
			if occupied {
				continue
			}
			buildCtx, cancel := context.WithTimeout(ctx, transitionBudget)
			err := r.setupNetwork(buildCtx, slot, false)
			cancel()
			if err != nil && ctx.Err() == nil {
				slog.Warn("firecracker slot pre-wire failed; the next sandbox on it builds inline", "slot", slot, "err", err)
			}
		}
		select {
		case <-r.wireCh:
		case <-ctx.Done():
			return
		}
	}
}

// clearSlotNetwork deletes the slot's netns and veth and marks it unwired, unless
// a sandbox other than state holds the slot. Teardown passes its own sandbox;
// Shutdown passes nil to clear only free slots.
//
// It runs under the slot's netMu so it cannot interleave with a build of the same
// slot, and re-reads the owner under that lock because the slot a teardown read
// when it began may have changed hands by the time it gets here: Shutdown does not
// wait for claims, so it can release a slot from under a concurrent teardown of
// the same sandbox while the next sandbox takes it. Ownership is the slot holding
// state's ID while no other incarnation of that ID is tracked: the ID alone would
// also match a later sandbox created under it, which is how the API re-creates
// one a runner lost. A slot found free is still cleared; only Shutdown produces
// that, and the namespace on it then has nothing left to keep it. Whatever a
// skipped cleanup leaves behind is cleared by the next build on the slot, which
// deletes both names before creating them.
func (r *Runtime) clearSlotNetwork(ctx context.Context, slot int, state *sandboxState) error {
	s := &r.slots[slot]
	s.netMu.Lock()
	defer s.netMu.Unlock()
	r.mu.Lock()
	free := !s.occupied()
	ours := false
	if state != nil && s.sandboxID == state.id {
		current, tracked := r.sandboxes[state.id]
		ours = !tracked || current == state
	}
	r.mu.Unlock()
	if !free && !ours {
		return nil
	}
	// Unwired whether or not the script succeeds: a failed cleanup leaves the
	// namespace in an unknown state, and the next build clears the slot before
	// building it, so treating it as gone is the safe reading either way.
	s.wired.Store(false)
	return r.deps.run(ctx, "sudo", "/bin/sh", "-c", fcnetwork.CleanupScript(slot))
}
