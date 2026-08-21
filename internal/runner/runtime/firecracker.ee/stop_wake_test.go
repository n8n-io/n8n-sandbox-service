package firecracker

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/metrics"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

func TestRuntimeStopSandboxEvictsOldestStoppedWhenDiskFull(t *testing.T) {
	rt := testRuntimeT(t, 4)
	stubCreateDeps(rt)
	rec := metrics.NewRunnerRecorder(true)
	rt.SetMetricsRecorder(rec)

	rt.deps.pauseVM = func(context.Context, string) error { return nil }
	rt.deps.createSnapshot = func(context.Context, string) error { return nil }

	const oldID = "sandbox-id-old123456"
	const newID = "sandbox-id-new123456"

	if _, err := rt.CreateSandbox(context.Background(), oldID, nil); err != nil {
		t.Fatalf("CreateSandbox(%s) failed: %v", oldID, err)
	}
	if err := rt.StopSandbox(context.Background(), oldID); err != nil {
		t.Fatalf("StopSandbox(%s) failed: %v", oldID, err)
	}
	rt.mu.Lock()
	rt.sandboxes[oldID].stoppedAt = time.Now().Add(-time.Hour)
	rt.mu.Unlock()

	if _, err := rt.CreateSandbox(context.Background(), newID, nil); err != nil {
		t.Fatalf("CreateSandbox(%s) failed: %v", newID, err)
	}

	var freeCalls atomic.Int32
	rt.deps.freeBytesInDir = func(string) (int64, error) {
		if freeCalls.Add(1) == 1 {
			return 0, nil
		}
		return 1 << 30, nil
	}

	if err := rt.StopSandbox(context.Background(), newID); err != nil {
		t.Fatalf("StopSandbox(%s) failed: %v", newID, err)
	}

	if _, err := rt.GetSandboxInfo(context.Background(), oldID); !errors.Is(err, runnerruntime.ErrSandboxNotFound) {
		t.Fatalf("GetSandboxInfo(%s) error = %v, want ErrSandboxNotFound after eviction", oldID, err)
	}
	if got := rec.ContainerOpCount(metrics.OpEvict, true); got != 1 {
		t.Fatalf("evict metric = %v, want 1", got)
	}
}

func TestRuntimeEnsureSandboxRunningWaitsForStop(t *testing.T) {
	rt := testRuntimeT(t, 2)
	stubCreateDeps(rt)

	pauseStarted := make(chan struct{})
	allowPauseDone := make(chan struct{})
	rt.deps.pauseVM = func(context.Context, string) error {
		close(pauseStarted)
		<-allowPauseDone
		return nil
	}
	rt.deps.createSnapshot = func(context.Context, string) error { return nil }
	rt.deps.loadSnapshot = func(context.Context, string, Config) error { return nil }

	const sandboxID = "sandbox-id-123456"
	if _, err := rt.CreateSandbox(context.Background(), sandboxID, nil); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}

	stopDone := make(chan error, 1)
	go func() {
		stopDone <- rt.StopSandbox(context.Background(), sandboxID)
	}()

	select {
	case <-pauseStarted:
	case <-time.After(time.Second):
		t.Fatal("StopSandbox did not reach pause")
	}

	wakeDone := make(chan error, 1)
	go func() {
		wakeDone <- rt.EnsureSandboxRunning(context.Background(), sandboxID)
	}()

	select {
	case err := <-wakeDone:
		t.Fatalf("EnsureSandboxRunning returned before stop finished: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(allowPauseDone)
	if err := <-stopDone; err != nil {
		t.Fatalf("StopSandbox() failed: %v", err)
	}
	if err := <-wakeDone; err != nil {
		t.Fatalf("EnsureSandboxRunning() failed: %v", err)
	}

	url, err := rt.DaemonURL(context.Background(), sandboxID)
	if err != nil {
		t.Fatalf("DaemonURL() failed: %v", err)
	}
	if url != "http://127.0.0.1:18081" {
		t.Fatalf("DaemonURL() = %s", url)
	}
}

func TestRuntimeDeleteSandboxWaitsForWake(t *testing.T) {
	rt := testRuntimeT(t, 2)
	stubCreateDeps(rt)

	proc := &fakeProcess{}
	proxy := &fakeProxy{}
	rt.deps.start = func(context.Context, string, ...string) (process, error) { return proc, nil }
	rt.deps.newProxy = func(context.Context, string, string, string) (daemonProxy, error) { return proxy, nil }

	wakeReachedSnapshot := make(chan struct{})
	allowWakeSnapshot := make(chan struct{})
	var loadCount int
	rt.deps.loadSnapshot = func(context.Context, string, Config) error {
		loadCount++
		if loadCount == 2 {
			close(wakeReachedSnapshot)
			<-allowWakeSnapshot
		}
		return nil
	}

	const sandboxID = "sandbox-id-123456"
	if _, err := rt.CreateSandbox(context.Background(), sandboxID, nil); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	if err := rt.StopSandbox(context.Background(), sandboxID); err != nil {
		t.Fatalf("StopSandbox() failed: %v", err)
	}

	wakeDone := make(chan error, 1)
	go func() {
		wakeDone <- rt.EnsureSandboxRunning(context.Background(), sandboxID)
	}()

	select {
	case <-wakeReachedSnapshot:
	case <-time.After(time.Second):
		t.Fatal("wake did not reach snapshot restore")
	}

	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- rt.DeleteSandbox(context.Background(), sandboxID)
	}()

	select {
	case err := <-deleteDone:
		t.Fatalf("DeleteSandbox returned while wake was still restoring: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(allowWakeSnapshot)
	if err := <-wakeDone; err != nil {
		t.Fatalf("EnsureSandboxRunning() failed: %v", err)
	}
	if err := <-deleteDone; err != nil {
		t.Fatalf("DeleteSandbox() failed: %v", err)
	}

	// The microVM the wake brought up must be torn down, not orphaned on a slot
	// the runner has already handed back.
	if !proc.killed {
		t.Fatal("expected woken microVM process to be killed by delete")
	}
	if !proxy.stopped {
		t.Fatal("expected woken sandbox proxy to be stopped by delete")
	}
	capacity, err := rt.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity() failed: %v", err)
	}
	if capacity.Used != 0 || capacity.Stopped != 0 {
		t.Fatalf("Capacity() = %+v, want no slots or stopped sandboxes left", capacity)
	}
	if _, err := rt.GetSandboxInfo(context.Background(), sandboxID); !errors.Is(err, runnerruntime.ErrSandboxNotFound) {
		t.Fatalf("GetSandboxInfo() error = %v, want ErrSandboxNotFound", err)
	}
}

// A delete claim must be visible the moment beginTransition grants it, otherwise
// Shutdown sweeps a sandbox whose DeleteSandbox is already tearing it down and
// both goroutines kill the VM, clean the host, and drop the data dir twice.
func TestRuntimeShutdownSkipsSandboxClaimedForDelete(t *testing.T) {
	rt := testRuntimeT(t, 2)
	stubCreateDeps(rt)

	proc := &fakeProcess{}
	proxy := &fakeProxy{}
	rt.deps.start = func(context.Context, string, ...string) (process, error) { return proc, nil }
	rt.deps.newProxy = func(context.Context, string, string, string) (daemonProxy, error) { return proxy, nil }

	const sandboxID = "sandbox-id-123456"
	if _, err := rt.CreateSandbox(context.Background(), sandboxID, nil); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}

	// Stands in for a DeleteSandbox that has won the claim but has not started
	// teardown yet.
	if _, _, _, err := rt.beginTransition(context.Background(), sandboxID, transitionDeleting); err != nil {
		t.Fatalf("beginTransition(delete) failed: %v", err)
	}

	rt.Shutdown(context.Background())

	if proc.killed || proxy.stopped {
		t.Fatal("Shutdown tore down a sandbox already claimed by an in-flight delete")
	}
}

// Teardown must not follow the caller: a client that disconnects mid-delete
// would otherwise leave the microVM running on a slot the runner still tracks.
func TestRuntimeDeleteSandboxCompletesAfterCallerContextCanceled(t *testing.T) {
	rt := testRuntimeT(t, 2)
	stubCreateDeps(rt)

	proc := &fakeProcess{}
	proxy := &fakeProxy{}
	rt.deps.start = func(context.Context, string, ...string) (process, error) { return proc, nil }
	rt.deps.newProxy = func(context.Context, string, string, string) (daemonProxy, error) { return proxy, nil }

	const sandboxID = "sandbox-id-123456"
	if _, err := rt.CreateSandbox(context.Background(), sandboxID, nil); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := rt.DeleteSandbox(ctx, sandboxID); err != nil {
		t.Fatalf("DeleteSandbox() with canceled caller context failed: %v", err)
	}
	if !proc.killed || !proxy.stopped {
		t.Fatal("expected microVM and proxy to be torn down despite canceled caller context")
	}
	if _, err := rt.GetSandboxInfo(context.Background(), sandboxID); !errors.Is(err, runnerruntime.ErrSandboxNotFound) {
		t.Fatalf("GetSandboxInfo() error = %v, want ErrSandboxNotFound", err)
	}
}

// A caller without a deadline must not be able to wait forever on a claim that
// another operation is holding, since the control-plane RPCs have no deadline.
func TestRuntimeStopSandboxBoundsWaitForHeldTransition(t *testing.T) {
	rt := testRuntimeT(t, 2)
	stubCreateDeps(rt)

	const sandboxID = "sandbox-id-123456"
	if _, err := rt.CreateSandbox(context.Background(), sandboxID, nil); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}

	// Stands in for a wake that has claimed the sandbox and never finishes.
	if _, _, _, err := rt.beginTransition(context.Background(), sandboxID, transitionWaking); err != nil {
		t.Fatalf("beginTransition(wake) failed: %v", err)
	}

	prev := transitionBudget
	transitionBudget = 100 * time.Millisecond
	t.Cleanup(func() { transitionBudget = prev })

	err := rt.StopSandbox(context.Background(), sandboxID)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("StopSandbox() error = %v, want context.DeadlineExceeded", err)
	}
}

func TestRuntimeCapacityReportsStoppedSeparatelyFromSlots(t *testing.T) {
	rt := testRuntimeT(t, 3)
	stubCreateDeps(rt)
	rt.deps.pauseVM = func(context.Context, string) error { return nil }
	rt.deps.createSnapshot = func(context.Context, string) error { return nil }

	if _, err := rt.CreateSandbox(context.Background(), "sandbox-id-aaa123456", nil); err != nil {
		t.Fatalf("CreateSandbox(a) failed: %v", err)
	}
	if _, err := rt.CreateSandbox(context.Background(), "sandbox-id-bbb123456", nil); err != nil {
		t.Fatalf("CreateSandbox(b) failed: %v", err)
	}
	if err := rt.StopSandbox(context.Background(), "sandbox-id-aaa123456"); err != nil {
		t.Fatalf("StopSandbox(a) failed: %v", err)
	}

	capacity, err := rt.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity() failed: %v", err)
	}
	if capacity.Used != 1 {
		t.Fatalf("Capacity().Used = %d, want 1 running slot", capacity.Used)
	}
	if capacity.Stopped != 1 {
		t.Fatalf("Capacity().Stopped = %d, want 1 stopped sandbox", capacity.Stopped)
	}
	if capacity.Total != 3 {
		t.Fatalf("Capacity().Total = %d, want 3", capacity.Total)
	}
}
