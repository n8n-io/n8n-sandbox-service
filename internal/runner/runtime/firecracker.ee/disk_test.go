package firecracker

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// Below the floor a create is refused the way a full runner refuses one, so the
// API places it elsewhere, and it leaves nothing behind. At the floor it goes
// ahead.
func TestRuntimeCreateSandboxRefusesBelowDiskFloor(t *testing.T) {
	rt := testRuntimeT(t, 4)
	stubCreateDeps(rt)
	floor := testVolumeSpace.total / createFloorDivisor
	free := floor - 1
	rt.deps.volumeSpace = func(string) (volumeSpace, error) {
		return volumeSpace{free: free, total: testVolumeSpace.total}, nil
	}
	var cloned bool
	rt.deps.cloneRootfs = func(context.Context, string, string) error {
		cloned = true
		return nil
	}

	const refusedID = "sandbox-id-123456"
	if _, err := rt.CreateSandbox(context.Background(), refusedID, nil); !errors.Is(err, runnerruntime.ErrCapacityExhausted) {
		t.Fatalf("CreateSandbox() below the floor: error = %v, want ErrCapacityExhausted", err)
	}
	rt.mu.Lock()
	_, tracked := rt.sandboxes[refusedID]
	occupied := rt.occupiedSlotsLocked()
	rt.mu.Unlock()
	if tracked || occupied != 0 || cloned {
		t.Fatalf("after the refusal: tracked = %v, occupied slots = %d, cloned = %v; want nothing", tracked, occupied, cloned)
	}
	if _, err := os.Stat(sandboxDataDir(rt.runnerConfig.DataDir, refusedID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused sandbox data dir: stat error = %v, want it absent", err)
	}

	free = floor
	if _, err := rt.CreateSandbox(context.Background(), refusedID, nil); err != nil {
		t.Fatalf("CreateSandbox() at the floor: %v", err)
	}
}

// Stopped sandboxes hold no slot, but they do hold disk, so a runner with free
// slots still refuses once the volume is below the floor. Waking one needs no
// new space, so the wake is not refused.
func TestRuntimeDiskFloorRefusesCreatesButNotWakes(t *testing.T) {
	rt := testRuntimeT(t, 2)
	stubCreateDeps(rt)

	const stoppedID = "sandbox-id-123456"
	if _, err := rt.CreateSandbox(context.Background(), stoppedID, nil); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	if err := rt.StopSandbox(context.Background(), stoppedID); err != nil {
		t.Fatalf("StopSandbox() failed: %v", err)
	}

	rt.deps.volumeSpace = func(string) (volumeSpace, error) {
		return volumeSpace{free: 0, total: testVolumeSpace.total}, nil
	}
	if _, err := rt.CreateSandbox(context.Background(), "sandbox-id-abcdef", nil); !errors.Is(err, runnerruntime.ErrCapacityExhausted) {
		t.Fatalf("CreateSandbox() with free slots and a full volume: error = %v, want ErrCapacityExhausted", err)
	}
	if _, err := rt.EnsureSandboxRunning(context.Background(), stoppedID); err != nil {
		t.Fatalf("EnsureSandboxRunning() with a full volume: %v", err)
	}
}

// A sandbox the runner already holds is never refused for disk: the API would
// create it again on another runner.
func TestRuntimeCreateSandboxReportsDuplicateBeforeDiskFloor(t *testing.T) {
	rt := testRuntimeT(t, 2)
	stubCreateDeps(rt)

	const sandboxID = "sandbox-id-123456"
	if _, err := rt.CreateSandbox(context.Background(), sandboxID, nil); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	rt.deps.volumeSpace = func(string) (volumeSpace, error) {
		return volumeSpace{free: 0, total: testVolumeSpace.total}, nil
	}
	_, err := rt.CreateSandbox(context.Background(), sandboxID, nil)
	if err == nil || errors.Is(err, runnerruntime.ErrCapacityExhausted) || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("CreateSandbox() of a held sandbox: error = %v, want already exists and no refusal", err)
	}
}

// When the volume cannot be read the create fails, but not as a refusal: nothing
// says another runner would do better.
func TestRuntimeCreateSandboxFailsWhenVolumeUnreadable(t *testing.T) {
	rt := testRuntimeT(t, 2)
	stubCreateDeps(rt)
	rt.deps.volumeSpace = func(string) (volumeSpace, error) {
		return volumeSpace{}, errors.New("statfs /var/sandboxes: no such file or directory")
	}

	_, err := rt.CreateSandbox(context.Background(), "sandbox-id-123456", nil)
	if err == nil || errors.Is(err, runnerruntime.ErrCapacityExhausted) {
		t.Fatalf("CreateSandbox() error = %v, want a failure that is not a refusal", err)
	}
	if got := capacityOf(t, rt); got.Used != 0 {
		t.Fatalf("capacity after the failure = %+v, want no slot held", got)
	}
}

func TestStatVolumeReadsTheFilesystem(t *testing.T) {
	space, err := statVolume(t.TempDir())
	if err != nil {
		t.Fatalf("statVolume() failed: %v", err)
	}
	if space.total <= 0 || space.free < 0 || space.free > space.total {
		t.Fatalf("statVolume() = %+v, want 0 <= free <= total and total > 0", space)
	}
}
