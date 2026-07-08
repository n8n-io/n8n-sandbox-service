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
