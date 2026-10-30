package firecracker

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// A stop with no room for its snapshot fails rather than delete a stopped
// sandbox to make room: the runner cannot tell whose that sandbox is. It fails
// before the pause, so the guest it refused to stop keeps running.
func TestRuntimeStopSandboxFailsWithoutDeletingWhenDiskFull(t *testing.T) {
	rt := testRuntimeT(t, 4)
	stubCreateDeps(rt)

	const stoppedID = "sandbox-id-old123456"
	const runningID = "sandbox-id-new123456"

	if _, err := rt.CreateSandbox(context.Background(), stoppedID, nil); err != nil {
		t.Fatalf("CreateSandbox(%s) failed: %v", stoppedID, err)
	}
	if err := rt.StopSandbox(context.Background(), stoppedID); err != nil {
		t.Fatalf("StopSandbox(%s) failed: %v", stoppedID, err)
	}
	if _, err := rt.CreateSandbox(context.Background(), runningID, nil); err != nil {
		t.Fatalf("CreateSandbox(%s) failed: %v", runningID, err)
	}

	rt.deps.volumeSpace = func(string) (volumeSpace, error) {
		return volumeSpace{free: 0, total: testVolumeSpace.total}, nil
	}
	var paused atomic.Bool
	rt.deps.pauseVM = func(context.Context, string) error {
		paused.Store(true)
		return nil
	}

	if err := rt.StopSandbox(context.Background(), runningID); err == nil || !strings.Contains(err.Error(), "insufficient disk space") {
		t.Fatalf("StopSandbox(%s) error = %v, want insufficient disk space", runningID, err)
	}
	if paused.Load() {
		t.Fatal("StopSandbox paused the guest before failing for disk space")
	}

	if _, err := rt.GetSandboxInfo(context.Background(), stoppedID); err != nil {
		t.Fatalf("GetSandboxInfo(%s) error = %v, want the stopped sandbox kept", stoppedID, err)
	}
	if _, err := os.Stat(sandboxDataDir(rt.runnerConfig.DataDir, stoppedID)); err != nil {
		t.Fatalf("stopped sandbox data dir: %v, want it kept", err)
	}
	if _, err := rt.DaemonURL(context.Background(), runningID); err != nil {
		t.Fatalf("DaemonURL(%s) error = %v, want the refused sandbox still running", runningID, err)
	}
	if got := capacityOf(t, rt); got.Used != 1 || got.Stopped != 1 {
		t.Fatalf("capacity = %+v, want used 1 and stopped 1", got)
	}
}

// A snapshot that fails after the pause, here for want of space the check before
// it did not see go, fails the stop. The sandbox stays running, so its guest is
// resumed instead of being left paused under requests that would hang, and the
// half-written snapshot stays unusable: the pin to cold boot is not lifted.
func TestRuntimeStopSandboxResumesGuestWhenSnapshotFails(t *testing.T) {
	rt := testRuntimeT(t, 1)
	stubCreateDeps(rt)

	const sandboxID = "sandbox-id-123456"
	if _, err := rt.CreateSandbox(context.Background(), sandboxID, nil); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	rt.deps.createSnapshot = func(context.Context, string) error {
		return errors.New("write snapshot_mem: no space left on device")
	}
	var pausedSocket, resumedSocket string
	rt.deps.pauseVM = func(_ context.Context, socketPath string) error {
		pausedSocket = socketPath
		return nil
	}
	rt.deps.resumeVM = func(_ context.Context, socketPath string) error {
		resumedSocket = socketPath
		return nil
	}

	if err := rt.StopSandbox(context.Background(), sandboxID); err == nil {
		t.Fatal("StopSandbox() succeeded, want the snapshot failure reported")
	}
	if resumedSocket == "" || resumedSocket != pausedSocket {
		t.Fatalf("resumed %q after pausing %q, want the paused guest resumed", resumedSocket, pausedSocket)
	}
	if _, err := rt.DaemonURL(context.Background(), sandboxID); err != nil {
		t.Fatalf("DaemonURL() error = %v, want the sandbox still running", err)
	}
	rt.mu.Lock()
	pinned := rt.sandboxes[sandboxID].mustColdBoot
	rt.mu.Unlock()
	if !pinned {
		t.Fatal("a failed snapshot lifted the pin to cold boot")
	}
	if got := capacityOf(t, rt); got.Used != 1 || got.Stopped != 0 {
		t.Fatalf("capacity = %+v, want used 1 and stopped 0", got)
	}
}

// A guest that cannot be resumed after a failed snapshot may still be paused, so
// it is torn down and left stopped like a guest that died: the next request cold
// boots it and is told its memory is gone, instead of hanging on it.
func TestRuntimeStopSandboxTearsDownGuestThatCannotResume(t *testing.T) {
	rt := testRuntimeT(t, 1)
	stubCreateDeps(rt)
	proc := &fakeProcess{}
	rt.deps.start = func(context.Context, func(error), string, ...string) (process, error) {
		return proc, nil
	}

	const sandboxID = "sandbox-id-123456"
	if _, err := rt.CreateSandbox(context.Background(), sandboxID, nil); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	rt.deps.createSnapshot = func(context.Context, string) error {
		return errors.New("write snapshot_mem: no space left on device")
	}
	rt.deps.resumeVM = func(context.Context, string) error {
		return errors.New("firecracker API PATCH /vm: context deadline exceeded")
	}

	if err := rt.StopSandbox(context.Background(), sandboxID); err == nil {
		t.Fatal("StopSandbox() succeeded, want the snapshot failure reported")
	}
	if !proc.killed {
		t.Fatal("the guest that could not be resumed was not torn down")
	}
	if _, err := rt.DaemonURL(context.Background(), sandboxID); !errors.Is(err, runnerruntime.ErrSandboxNotRunning) {
		t.Fatalf("DaemonURL() error = %v, want ErrSandboxNotRunning", err)
	}
	if got := capacityOf(t, rt); got.Used != 0 || got.Stopped != 1 {
		t.Fatalf("capacity = %+v, want used 0 and stopped 1", got)
	}

	var coldBooted bool
	rt.deps.coldBoot = func(context.Context, string, *bootParams) error {
		coldBooted = true
		return nil
	}
	wake, err := rt.EnsureSandboxRunning(context.Background(), sandboxID)
	if err != nil {
		t.Fatalf("EnsureSandboxRunning() failed: %v", err)
	}
	if !coldBooted || !wake.Recovered {
		t.Fatalf("wake cold booted = %v, recovered = %v; want a cold boot reported as a recovery", coldBooted, wake.Recovered)
	}
}

// A resume stuck on an unresponsive Firecracker API gives up on its own limit, so
// the teardown after it still has time to clean the jail; an expired context would
// fail those host commands at once and leave the mounts behind.
func TestRuntimeStopSandboxStuckResumeLeavesTeardownTime(t *testing.T) {
	rt := testRuntimeT(t, 1)
	stubCreateDeps(rt)
	restore := resumeAfterFailedStopTimeout
	resumeAfterFailedStopTimeout = 10 * time.Millisecond
	t.Cleanup(func() { resumeAfterFailedStopTimeout = restore })

	const sandboxID = "sandbox-id-123456"
	if _, err := rt.CreateSandbox(context.Background(), sandboxID, nil); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	rt.deps.createSnapshot = func(context.Context, string) error {
		return errors.New("write snapshot_mem: no space left on device")
	}
	rt.deps.resumeVM = func(ctx context.Context, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	}
	var cleanupRan, cleanupCtxLive atomic.Bool
	rt.deps.run = func(ctx context.Context, _ string, args ...string) error {
		if len(args) > 0 && strings.Contains(args[len(args)-1], "rm -rf") {
			cleanupRan.Store(true)
			cleanupCtxLive.Store(ctx.Err() == nil)
		}
		return nil
	}

	if err := rt.StopSandbox(context.Background(), sandboxID); err == nil {
		t.Fatal("StopSandbox() succeeded, want the snapshot failure reported")
	}
	if !cleanupRan.Load() {
		t.Fatal("the jail was not cleaned after the resume gave up")
	}
	if !cleanupCtxLive.Load() {
		t.Fatal("jail cleanup ran with an expired context")
	}
}

// A pause whose call failed may still have paused the guest, so it is resumed
// the same way; a stop that succeeds resumes nothing.
func TestRuntimeStopSandboxResumesOnlyAfterAFailedStop(t *testing.T) {
	rt := testRuntimeT(t, 2)
	stubCreateDeps(rt)
	var resumes atomic.Int32
	rt.deps.resumeVM = func(context.Context, string) error {
		resumes.Add(1)
		return nil
	}

	const stoppedID = "sandbox-id-123456"
	if _, err := rt.CreateSandbox(context.Background(), stoppedID, nil); err != nil {
		t.Fatalf("CreateSandbox(%s) failed: %v", stoppedID, err)
	}
	if err := rt.StopSandbox(context.Background(), stoppedID); err != nil {
		t.Fatalf("StopSandbox(%s) failed: %v", stoppedID, err)
	}
	if got := resumes.Load(); got != 0 {
		t.Fatalf("resumes after a successful stop = %d, want 0", got)
	}

	const runningID = "sandbox-id-abcdef"
	if _, err := rt.CreateSandbox(context.Background(), runningID, nil); err != nil {
		t.Fatalf("CreateSandbox(%s) failed: %v", runningID, err)
	}
	rt.deps.pauseVM = func(context.Context, string) error { return context.DeadlineExceeded }
	if err := rt.StopSandbox(context.Background(), runningID); err == nil {
		t.Fatal("StopSandbox() succeeded, want the pause failure reported")
	}
	if got := resumes.Load(); got != 1 {
		t.Fatalf("resumes after a failed pause = %d, want 1", got)
	}
	if _, err := rt.DaemonURL(context.Background(), runningID); err != nil {
		t.Fatalf("DaemonURL(%s) error = %v, want the sandbox still running", runningID, err)
	}
}

// Host cleanup is the one part of a stop that can fail after the microVM is
// already gone. The stop has to finish anyway: teardown drops the process and
// proxy handles, so a sandbox left marked running would hold its slot for a
// microVM nothing can reach and fail every later stop against a dead API socket.
func TestRuntimeStopSandboxFinishesWhenHostCleanupFails(t *testing.T) {
	rt := testRuntimeT(t, 1)
	stubCreateDeps(rt)

	// cleanupHost is the only script that removes the jail directory, so a busy
	// mount under it fails the stop's teardown and nothing else.
	rt.deps.run = func(_ context.Context, _ string, args ...string) error {
		if len(args) > 0 && strings.Contains(args[len(args)-1], "rm -rf") {
			return errors.New("rm: cannot remove jail dir: Device or resource busy")
		}
		return nil
	}

	const sandboxID = "sandbox-id-123456"
	if _, err := rt.CreateSandbox(context.Background(), sandboxID, nil); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	if err := rt.StopSandbox(context.Background(), sandboxID); err == nil {
		t.Fatal("StopSandbox() succeeded, want the host cleanup failure reported")
	}

	if got := capacityOf(t, rt); got.Used != 0 || got.Stopped != 1 {
		t.Fatalf("capacity after the failed cleanup = %+v, want used 0 and stopped 1", got)
	}
	// Nothing is listening on the daemon URL any more, so a request has to be told
	// the sandbox is not running instead of being proxied into a refused connection.
	if _, err := rt.DaemonURL(context.Background(), sandboxID); !errors.Is(err, runnerruntime.ErrSandboxNotRunning) {
		t.Fatalf("DaemonURL() error = %v, want ErrSandboxNotRunning", err)
	}

	// The snapshot was written before the cleanup failed, so the sandbox is a
	// normal stopped one and has to wake.
	if _, err := rt.EnsureSandboxRunning(context.Background(), sandboxID); err != nil {
		t.Fatalf("EnsureSandboxRunning() after the failed cleanup: %v", err)
	}
	if got := capacityOf(t, rt); got.Used != 1 {
		t.Fatalf("capacity after the wake = %+v, want the sandbox back on a slot", got)
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
		_, wakeErr := rt.EnsureSandboxRunning(context.Background(), sandboxID)
		wakeDone <- wakeErr
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
	rt.deps.start = func(context.Context, func(error), string, ...string) (process, error) { return proc, nil }
	rt.deps.newProxy = func(context.Context, string, string, string, string) (daemonProxy, error) { return proxy, nil }

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
		_, wakeErr := rt.EnsureSandboxRunning(context.Background(), sandboxID)
		wakeDone <- wakeErr
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
	rt.deps.start = func(context.Context, func(error), string, ...string) (process, error) { return proc, nil }
	rt.deps.newProxy = func(context.Context, string, string, string, string) (daemonProxy, error) { return proxy, nil }

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
	rt.deps.start = func(context.Context, func(error), string, ...string) (process, error) { return proc, nil }
	rt.deps.newProxy = func(context.Context, string, string, string, string) (daemonProxy, error) { return proxy, nil }

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

	shrinkBudgets(t, 50*time.Millisecond, 50*time.Millisecond)

	err := rt.StopSandbox(context.Background(), sandboxID)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("StopSandbox() error = %v, want context.DeadlineExceeded", err)
	}
}

// Waiting for a claim must not consume the waiter's own budget, and must outlast
// the longest hold: a create may legitimately hold the sandbox for longer than one
// transition budget, and a delete that gives up early leaves the sandbox tracked.
func TestRuntimeDeleteWaitsOutCreateHoldingLongerThanTransitionBudget(t *testing.T) {
	rt := testRuntimeT(t, 2)
	stubCreateDeps(rt)
	shrinkBudgets(t, 3*time.Second, time.Second)

	createReachedSnapshot := make(chan struct{})
	allowCreateSnapshot := make(chan struct{})
	rt.deps.loadSnapshot = func(context.Context, string, Config) error {
		close(createReachedSnapshot)
		<-allowCreateSnapshot
		return nil
	}

	const sandboxID = "sandbox-id-123456"
	createDone := make(chan error, 1)
	go func() {
		_, err := rt.CreateSandbox(context.Background(), sandboxID, nil)
		createDone <- err
	}()

	select {
	case <-createReachedSnapshot:
	case <-time.After(2 * time.Second):
		t.Fatal("create did not reach snapshot restore")
	}

	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- rt.DeleteSandbox(context.Background(), sandboxID)
	}()

	// Hold the create claim past one transition budget, which is what a delete
	// used to time out on.
	select {
	case err := <-deleteDone:
		t.Fatalf("DeleteSandbox returned while create still held the sandbox: %v", err)
	case <-time.After(1500 * time.Millisecond):
	}

	close(allowCreateSnapshot)
	if err := <-createDone; err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	if err := <-deleteDone; err != nil {
		t.Fatalf("DeleteSandbox() failed after waiting for create: %v", err)
	}
	if _, err := rt.GetSandboxInfo(context.Background(), sandboxID); !errors.Is(err, runnerruntime.ErrSandboxNotFound) {
		t.Fatalf("GetSandboxInfo() error = %v, want ErrSandboxNotFound", err)
	}
}

// A wake that fails because it ran out of budget must still clean up the host
// state it created, since the slot is handed back either way.
func TestRuntimeWakeFailureCleansHostAfterBudgetExpires(t *testing.T) {
	rt := testRuntimeT(t, 2)
	stubCreateDeps(rt)

	const sandboxID = "sandbox-id-123456"
	if _, err := rt.CreateSandbox(context.Background(), sandboxID, nil); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	if err := rt.StopSandbox(context.Background(), sandboxID); err != nil {
		t.Fatalf("StopSandbox() failed: %v", err)
	}

	var mu sync.Mutex
	cleanupRan := false
	// Mimics exec.CommandContext: a host command fails immediately once its
	// context is done.
	rt.deps.run = func(ctx context.Context, _ string, args ...string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, a := range args {
			if strings.Contains(a, "umount") {
				mu.Lock()
				cleanupRan = true
				mu.Unlock()
			}
		}
		return nil
	}
	rt.deps.loadSnapshot = func(ctx context.Context, _ string, _ Config) error {
		<-ctx.Done()
		return ctx.Err()
	}
	shrinkBudgets(t, 3*time.Second, 200*time.Millisecond)

	if _, err := rt.EnsureSandboxRunning(context.Background(), sandboxID); err == nil {
		t.Fatal("expected wake to fail once its budget expired")
	}

	mu.Lock()
	defer mu.Unlock()
	if !cleanupRan {
		t.Fatal("expected wake failure to clean up host state on a fresh context")
	}
}

// shrinkBudgets scales the lifecycle budgets down for tests that would otherwise
// wait minutes. Both matter: the wait budget is derived from them.
func shrinkBudgets(t *testing.T, create, transition time.Duration) {
	t.Helper()
	prevCreate, prevTransition := createBudget, transitionBudget
	createBudget, transitionBudget = create, transition
	t.Cleanup(func() { createBudget, transitionBudget = prevCreate, prevTransition })
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
