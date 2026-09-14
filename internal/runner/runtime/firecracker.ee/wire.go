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
// still calls, finds the slot wired and returns without running anything.
//
// Every sandbox still gets a fresh namespace: teardown deletes the slot's netns
// and veth as before and marks the slot unwired, and the wirer rebuilds it from
// scratch. Readiness does not wait for wiring; a slot the wirer has not reached is
// built inline by the sandbox that lands on it, as every create did before.

// setupNetwork makes sure the slot's network namespace exists with TAP, veth
// uplink, and per-netns egress iptables matching the Docker private-CIDR policy.
// It builds the namespace if the slot is unwired and returns at once if it is
// wired, so calling it on a slot the wirer already built costs one lock.
//
// The slot's netMu is held for the whole build. That is what keeps two builders
// off one slot, and what makes the wired flag trustworthy to whoever waits: a
// sandbox that blocks here behind the wirer is handed the lock with the flag set
// and the namespace in place.
func (r *Runtime) setupNetwork(ctx context.Context, slot int) error {
	s := &r.slots[slot]
	s.netMu.Lock()
	defer s.netMu.Unlock()
	if s.wired {
		return nil
	}
	if err := r.ensureHostNATReady(ctx); err != nil {
		return fmt.Errorf("host NAT not configured: %w", err)
	}
	script := fcnetwork.SetupScript(slot, fcnetwork.NetnsName(slot), r.config.HostTapDeviceName, r.config.HostTapIPCIDR)
	if err := r.deps.run(ctx, "sudo", "/bin/sh", "-c", script); err != nil {
		return err
	}
	s.wired = true
	return nil
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
			err := r.setupNetwork(buildCtx, slot)
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

// unwireSlot records that the slot's namespace has been deleted, so the next
// setupNetwork on it builds rather than skips. Called from teardownRunningVM
// after cleanupHost; the release that follows is what wakes the wirer.
func (r *Runtime) unwireSlot(slot int) {
	s := &r.slots[slot]
	s.netMu.Lock()
	s.wired = false
	s.netMu.Unlock()
}
