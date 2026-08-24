package firecracker

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/metrics"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// guestExitStub records the exit callback the runtime registers for each microVM
// it starts, so a test can report that incarnation's process as gone. Firing a
// specific one is the point: it is how "an old incarnation died" is told apart
// from "the current one died" without a real Firecracker process.
type guestExitStub struct {
	mu    sync.Mutex
	exits []func(error)
}

func (s *guestExitStub) install(rt *Runtime, proc process) {
	rt.deps.start = func(_ context.Context, onExit func(error), _ string, _ ...string) (process, error) {
		s.mu.Lock()
		s.exits = append(s.exits, onExit)
		s.mu.Unlock()
		return proc, nil
	}
}

// fire reports the exit of the nth microVM this runtime started, counting from 0.
func (s *guestExitStub) fire(t *testing.T, n int) {
	t.Helper()
	s.mu.Lock()
	count := len(s.exits)
	var onExit func(error)
	if n < count {
		onExit = s.exits[n]
	}
	s.mu.Unlock()
	if onExit == nil {
		t.Fatalf("microVM %d was never started (%d starts recorded)", n, count)
	}
	onExit(errors.New("signal: killed"))
}

func capacityOf(t *testing.T, rt *Runtime) runnerruntime.Capacity {
	t.Helper()
	capacity, err := rt.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity() failed: %v", err)
	}
	return capacity
}

func TestGuestDeathFreesSlotAndPinsSandboxToColdBoot(t *testing.T) {
	rt := testRuntimeT(t, 1)
	stubCreateDeps(rt)
	rec := metrics.NewRunnerRecorder(true)
	rt.SetMetricsRecorder(rec)

	proc := &fakeProcess{}
	proxy := &fakeProxy{}
	exits := &guestExitStub{}
	exits.install(rt, proc)
	rt.deps.newProxy = func(context.Context, string, string, string) (daemonProxy, error) { return proxy, nil }

	const sandboxID = "sandbox-id-123456"
	if _, err := rt.CreateSandbox(context.Background(), sandboxID, nil); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	if got := capacityOf(t, rt); got.Used != 1 {
		t.Fatalf("capacity used before crash = %d, want 1", got.Used)
	}

	exits.fire(t, 0)

	if got := capacityOf(t, rt); got.Used != 0 || got.Stopped != 1 {
		t.Fatalf("capacity after crash = %+v, want used 0 and stopped 1", got)
	}
	if !proc.killed {
		t.Fatal("crashed sandbox left its process group unkilled")
	}
	if !proxy.stopped {
		t.Fatal("crashed sandbox left its daemon proxy running")
	}
	if got := rec.GuestDeathCount(metrics.BackendFirecracker); got != 1 {
		t.Fatalf("guest death metric = %v, want 1", got)
	}

	// The crash has to reach a client as "not running" so a request drives the
	// wake path, and the wake path has to refuse rather than restore the snapshot
	// the guest wrote past.
	if _, err := rt.DaemonURL(context.Background(), sandboxID); !errors.Is(err, runnerruntime.ErrSandboxNotRunning) {
		t.Fatalf("DaemonURL() error = %v, want ErrSandboxNotRunning", err)
	}
	if err := rt.EnsureSandboxRunning(context.Background(), sandboxID); !errors.Is(err, errGuestCrashed) {
		t.Fatalf("EnsureSandboxRunning() error = %v, want errGuestCrashed", err)
	}

	// A crashed sandbox is already torn down, so the idle sweeper's stop must
	// succeed instead of failing against a dead API socket on every pass.
	rt.deps.pauseVM = func(context.Context, string) error {
		t.Fatal("StopSandbox paused a crashed sandbox")
		return nil
	}
	if err := rt.StopSandbox(context.Background(), sandboxID); err != nil {
		t.Fatalf("StopSandbox() after crash failed: %v", err)
	}
	if err := rt.DeleteSandbox(context.Background(), sandboxID); err != nil {
		t.Fatalf("DeleteSandbox() after crash failed: %v", err)
	}
}

func TestIntentionalStopIsNotReportedAsGuestDeath(t *testing.T) {
	rt := testRuntimeT(t, 1)
	stubCreateDeps(rt)
	rec := metrics.NewRunnerRecorder(true)
	rt.SetMetricsRecorder(rec)

	exits := &guestExitStub{}
	exits.install(rt, &fakeProcess{})

	const sandboxID = "sandbox-id-123456"
	if _, err := rt.CreateSandbox(context.Background(), sandboxID, nil); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	if err := rt.StopSandbox(context.Background(), sandboxID); err != nil {
		t.Fatalf("StopSandbox() failed: %v", err)
	}

	// The stop killed the microVM, so its exit callback fires afterwards. Taking
	// that for a crash would pin every idle-stopped sandbox to cold boot.
	exits.fire(t, 0)

	if got := rec.GuestDeathCount(metrics.BackendFirecracker); got != 0 {
		t.Fatalf("guest death metric = %v, want 0 after an intentional stop", got)
	}
	if err := rt.EnsureSandboxRunning(context.Background(), sandboxID); err != nil {
		t.Fatalf("EnsureSandboxRunning() after stop failed: %v", err)
	}
}

func TestStaleGuestExitDoesNotCrashTheCurrentMicroVM(t *testing.T) {
	rt := testRuntimeT(t, 1)
	stubCreateDeps(rt)
	rec := metrics.NewRunnerRecorder(true)
	rt.SetMetricsRecorder(rec)

	exits := &guestExitStub{}
	exits.install(rt, &fakeProcess{})

	const sandboxID = "sandbox-id-123456"
	if _, err := rt.CreateSandbox(context.Background(), sandboxID, nil); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	if err := rt.StopSandbox(context.Background(), sandboxID); err != nil {
		t.Fatalf("StopSandbox() failed: %v", err)
	}
	if err := rt.EnsureSandboxRunning(context.Background(), sandboxID); err != nil {
		t.Fatalf("EnsureSandboxRunning() failed: %v", err)
	}

	// The first incarnation's exit can arrive at any time, including after the
	// sandbox is back up on a new one.
	exits.fire(t, 0)

	if got := capacityOf(t, rt); got.Used != 1 {
		t.Fatalf("capacity after stale exit = %+v, want the woken sandbox still on its slot", got)
	}
	if _, err := rt.DaemonURL(context.Background(), sandboxID); err != nil {
		t.Fatalf("DaemonURL() after stale exit failed: %v", err)
	}
	if got := rec.GuestDeathCount(metrics.BackendFirecracker); got != 0 {
		t.Fatalf("guest death metric = %v, want 0 for a stale exit", got)
	}

	// The incarnation that is actually running still gets detected.
	exits.fire(t, 1)

	if got := rec.GuestDeathCount(metrics.BackendFirecracker); got != 1 {
		t.Fatalf("guest death metric = %v, want 1 after the current microVM died", got)
	}
	if err := rt.EnsureSandboxRunning(context.Background(), sandboxID); !errors.Is(err, errGuestCrashed) {
		t.Fatalf("EnsureSandboxRunning() error = %v, want errGuestCrashed", err)
	}
}
