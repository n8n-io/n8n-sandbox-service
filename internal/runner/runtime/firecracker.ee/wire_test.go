package firecracker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptLog records every host script a runtime runs. Safe to share between the
// wirer goroutine and the test.
type scriptLog struct {
	mu      sync.Mutex
	scripts []string
}

func (l *scriptLog) run(_ context.Context, _ string, args ...string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.scripts = append(l.scripts, args[len(args)-1])
	return nil
}

// matching returns the recorded scripts containing substr.
func (l *scriptLog) matching(substr string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, s := range l.scripts {
		if strings.Contains(s, substr) {
			out = append(out, s)
		}
	}
	return out
}

func slotWired(rt *Runtime, slot int) bool {
	s := &rt.slots[slot]
	s.netMu.Lock()
	defer s.netMu.Unlock()
	return s.wired
}

func waitSlotWired(t *testing.T, rt *Runtime, slot int, want bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if slotWired(rt, slot) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("slot %d wired = %v, want %v", slot, !want, want)
}

// The whole point of the wirer: a free slot is built before any sandbox needs it,
// a busy one is left to the sandbox on it, and a create that lands on a built slot
// runs no network script at all.
func TestWirerBuildsFreeSlotsAndActivationSkipsTheBuild(t *testing.T) {
	rt := testRuntimeT(t, 3)
	stubCreateDeps(rt)
	log := &scriptLog{}
	rt.deps.run = log.run

	// Slot 1 belongs to a sandbox that is not part of this test. The wirer has to
	// leave it alone: its owner has built it or is about to.
	rt.slots[1].sandboxID = "sandbox-id-occupied"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rt.wireSlots(ctx)

	waitSlotWired(t, rt, 0, true)
	waitSlotWired(t, rt, 2, true)
	if slotWired(rt, 1) {
		t.Fatal("wirer built an occupied slot")
	}
	builds := log.matching("ip netns add")
	if len(builds) != 2 {
		t.Fatalf("network builds = %d, want the two free slots", len(builds))
	}
	for _, script := range builds {
		if strings.Contains(script, "'fc-sb-1'") {
			t.Fatal("wirer built the namespace of an occupied slot")
		}
	}

	before := len(log.matching("ip netns add"))
	if _, err := rt.CreateSandbox(context.Background(), "sandbox-id-123456", nil); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	if after := len(log.matching("ip netns add")); after != before {
		t.Fatalf("create on a wired slot ran %d network build(s), want none", after-before)
	}
}

// A release is what hands a slot back to the wirer. Teardown deletes the
// namespace and unwires the slot, the release wakes the wirer, and the slot comes
// back wired for the next sandbox without any create having paid for it.
func TestWirerRebuildsASlotAfterItsSandboxIsReleased(t *testing.T) {
	rt := testRuntimeT(t, 1)
	stubCreateDeps(rt)
	log := &scriptLog{}
	rt.deps.run = log.run

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rt.wireSlots(ctx)
	waitSlotWired(t, rt, 0, true)

	const sandboxID = "sandbox-id-123456"
	if _, err := rt.CreateSandbox(context.Background(), sandboxID, nil); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	if err := rt.DeleteSandbox(context.Background(), sandboxID); err != nil {
		t.Fatalf("DeleteSandbox() failed: %v", err)
	}

	waitSlotWired(t, rt, 0, true)
	if builds := log.matching("ip netns add"); len(builds) != 2 {
		t.Fatalf("network builds = %d, want the startup build and one rebuild after the delete", len(builds))
	}
	if cleanups := log.matching("ip netns delete 'fc-sb-0'"); len(cleanups) < 2 {
		// One from the delete's cleanup, one from the rebuild clearing the slot first.
		t.Fatalf("namespace deletions = %d, want the teardown and the rebuild's clear", len(cleanups))
	}
}

// A stop whose host cleanup failed hands its slot back with the namespace in an
// unknown state. The slot must come back unwired, so the next build on it clears
// the slot before creating it rather than trusting what the failed cleanup left.
func TestFailedCleanupLeavesTheSlotUnwiredForTheNextBuild(t *testing.T) {
	rt := testRuntimeT(t, 1)
	stubCreateDeps(rt)
	log := &scriptLog{}
	rt.deps.run = func(ctx context.Context, name string, args ...string) error {
		_ = log.run(ctx, name, args...)
		// cleanupHost is the only script that removes the jail directory.
		if strings.Contains(args[len(args)-1], "rm -rf") {
			return errors.New("rm: cannot remove jail dir: Device or resource busy")
		}
		return nil
	}

	const sandboxID = "sandbox-id-123456"
	if _, err := rt.CreateSandbox(context.Background(), sandboxID, nil); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	if !slotWired(rt, 0) {
		t.Fatal("slot is not wired after an inline build")
	}
	if err := rt.StopSandbox(context.Background(), sandboxID); err == nil {
		t.Fatal("StopSandbox() succeeded, want the host cleanup failure reported")
	}
	if slotWired(rt, 0) {
		t.Fatal("slot stayed wired after a failed cleanup, so the next sandbox would start in a namespace nobody verified")
	}
	if len(rt.wireCh) != 1 {
		t.Fatal("releasing the slot did not wake the wirer")
	}

	// No wirer runs in this test, so the next create builds inline, and its
	// script has to clear the slot before creating it.
	before := len(log.matching("ip netns add"))
	if _, err := rt.CreateSandbox(context.Background(), "sandbox-id-abcdef", nil); err != nil {
		t.Fatalf("CreateSandbox() on the released slot failed: %v", err)
	}
	builds := log.matching("ip netns add")
	if len(builds) != before+1 {
		t.Fatalf("network builds = %d, want one inline build on the unwired slot", len(builds)-before)
	}
	rebuild := builds[len(builds)-1]
	for _, want := range []string{
		"ip link delete 'fc-veth-0' 2>/dev/null || true",
		"ip netns delete 'fc-sb-0' 2>/dev/null || true",
	} {
		if !strings.Contains(rebuild, want) {
			t.Errorf("rebuild does not clear the slot first, missing %q", want)
		}
	}
}

// A stopped sandbox keeps the netns and veth names of the slot it gave back, and
// by the time it is deleted that slot may be running another sandbox, or be
// pre-wired for the next one. Its delete must not tear that namespace down.
func TestDeletingAStoppedSandboxLeavesItsFormerSlotAlone(t *testing.T) {
	rt := testRuntimeT(t, 1)
	stubCreateDeps(rt)
	log := &scriptLog{}
	rt.deps.run = log.run

	const stoppedID = "sandbox-id-123456"
	if _, err := rt.CreateSandbox(context.Background(), stoppedID, nil); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	if err := rt.StopSandbox(context.Background(), stoppedID); err != nil {
		t.Fatalf("StopSandbox() failed: %v", err)
	}

	// Another sandbox takes the freed slot and builds it.
	const liveID = "sandbox-id-abcdef"
	if _, err := rt.CreateSandbox(context.Background(), liveID, nil); err != nil {
		t.Fatalf("CreateSandbox() on the freed slot failed: %v", err)
	}
	if !slotWired(rt, 0) {
		t.Fatal("slot is not wired after the second sandbox built it")
	}

	cleanupsBefore := len(log.matching("ip netns delete"))
	if err := rt.DeleteSandbox(context.Background(), stoppedID); err != nil {
		t.Fatalf("DeleteSandbox() of the stopped sandbox failed: %v", err)
	}
	if got := len(log.matching("ip netns delete")); got != cleanupsBefore {
		t.Fatal("deleting the stopped sandbox deleted the namespace its former slot now runs")
	}
	if !slotWired(rt, 0) {
		t.Fatal("deleting the stopped sandbox unwired a slot it no longer owns")
	}
	// The jail is still the stopped sandbox's own and has to go.
	if got := log.matching("umount -l"); len(got) == 0 || !strings.Contains(got[len(got)-1], "rm -rf") {
		t.Fatal("deleting the stopped sandbox skipped its own jail cleanup")
	}
	if _, err := rt.DaemonURL(context.Background(), liveID); err != nil {
		t.Fatalf("DaemonURL() of the sandbox on the slot: %v", err)
	}
}

// hookProxy runs a function from Stop, which teardownRunningVM calls between
// reading the sandbox's slot and cleaning the host. It stands in for a concurrent
// teardown of the same sandbox winning that window.
type hookProxy struct{ onStop func() }

func (p *hookProxy) Stop() error {
	p.onStop()
	return nil
}

// Shutdown does not wait for claims, so two teardowns of one sandbox can overlap.
// The loser read its slot while it still held it, but by the time its cleanup runs
// the winner may have released the slot and a new sandbox taken it. Ownership has
// to be re-read under the slot's lock at that point, not trusted from the top.
func TestTeardownRechecksSlotOwnershipBeforeClearingTheNamespace(t *testing.T) {
	rt := testRuntimeT(t, 1)
	stubCreateDeps(rt)
	log := &scriptLog{}
	rt.deps.run = log.run

	const sandboxID = "sandbox-id-123456"
	const nextID = "sandbox-id-abcdef"
	rt.deps.newProxy = func(context.Context, string, string, string) (daemonProxy, error) {
		return &hookProxy{onStop: func() {
			// The winning teardown released slot 0 and the next sandbox reserved it.
			rt.mu.Lock()
			rt.slots[0].sandboxID = nextID
			rt.mu.Unlock()
		}}, nil
	}
	if _, err := rt.CreateSandbox(context.Background(), sandboxID, nil); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}

	cleanupsBefore := len(log.matching("ip netns delete"))
	if err := rt.DeleteSandbox(context.Background(), sandboxID); err != nil {
		t.Fatalf("DeleteSandbox() failed: %v", err)
	}
	if got := len(log.matching("ip netns delete")); got != cleanupsBefore {
		t.Fatal("teardown deleted the namespace of a slot another sandbox had taken")
	}
	if !slotWired(rt, 0) {
		t.Fatal("teardown unwired a slot another sandbox had taken")
	}
	if got := log.matching("umount -l"); len(got) == 0 || !strings.Contains(got[len(got)-1], "rm -rf") {
		t.Fatal("teardown skipped its own jail cleanup")
	}
	rt.mu.Lock()
	owner := rt.slots[0].sandboxID
	rt.mu.Unlock()
	if owner != nextID {
		t.Fatalf("slot 0 owner = %q after the delete, want %q left in place", owner, nextID)
	}
}

// A runner that exits must not leave the namespaces the wirer built for idle
// slots on the host. A slot still occupied after the sandboxes are deleted belongs
// to a delete that failed and kept it, and is left for that delete or for startup
// reconcile.
func TestShutdownClearsPreWiredFreeSlots(t *testing.T) {
	rt := testRuntimeT(t, 3)
	stubCreateDeps(rt)
	log := &scriptLog{}
	rt.deps.run = log.run

	ctx, cancel := context.WithCancel(context.Background())
	go rt.wireSlots(ctx)
	waitSlotWired(t, rt, 0, true)
	waitSlotWired(t, rt, 1, true)
	waitSlotWired(t, rt, 2, true)
	cancel()

	// A sandbox whose delete failed keeps its slot and is no longer tracked.
	rt.slots[2].sandboxID = "sandbox-id-occupied"

	// Each build clears its slot first, so only what Shutdown adds counts.
	before := len(log.matching("ip netns delete"))
	rt.Shutdown(context.Background())

	for _, slot := range []int{0, 1} {
		if slotWired(rt, slot) {
			t.Errorf("slot %d still wired after shutdown", slot)
		}
	}
	if !slotWired(rt, 2) {
		t.Error("shutdown cleared a slot a sandbox still holds")
	}
	cleanups := log.matching("ip netns delete")[before:]
	if len(cleanups) != 2 {
		t.Fatalf("namespace deletions = %d, want one per free wired slot", len(cleanups))
	}
	for _, script := range cleanups {
		if strings.Contains(script, "'fc-sb-2'") {
			t.Fatal("shutdown deleted the namespace of an occupied slot")
		}
	}
}
