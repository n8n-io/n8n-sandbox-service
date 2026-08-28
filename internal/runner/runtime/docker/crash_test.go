package docker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/runner/config"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

const (
	crashSandboxID   = "sandbox-id-123456"
	crashContainerID = "container-1"
)

// crashBackend is a Docker whose container states the test drives, which is what
// crash handling reads: a container that died and came back looks running again, on
// an address it did not have before.
type crashBackend struct {
	mu        sync.Mutex
	states    []containerState // consumed in order; the last one repeats
	ip        string
	stops     int
	stopErr   error
	removeErr error
	watch     func(ctx context.Context, onDie func(containerID, sandboxID string)) error
	inspects  int
}

func runningState() containerState {
	return containerState{Status: containerStatusRunning, Running: true}
}

func (f *crashBackend) inspectContainer(context.Context, string) (*containerInspect, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspects++
	state := f.states[0]
	if len(f.states) > 1 {
		f.states = f.states[1:]
	}
	inspect := &containerInspect{ID: crashContainerID, State: state}
	inspect.NetworkSettings.Networks = map[string]struct {
		IPAddress string `json:"IPAddress"`
	}{runnerBridgeNetwork: {IPAddress: f.ip}}
	return inspect, nil
}

func (f *crashBackend) findContainerByLabels(context.Context, ...string) ([]string, error) {
	return []string{crashContainerID}, nil
}

func (f *crashBackend) containerIP(context.Context, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ip, nil
}

func (f *crashBackend) stopContainer(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	return f.stopErr
}

func (f *crashBackend) watchContainerDeaths(ctx context.Context, onDie func(string, string)) error {
	if f.watch == nil {
		return errors.New("unexpected watchContainerDeaths")
	}
	return f.watch(ctx, onDie)
}

func (f *crashBackend) startContainer(context.Context, string) error { return nil }
func (f *crashBackend) ping(context.Context) error                   { return nil }
func (f *crashBackend) removeContainer(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.removeErr
}
func (f *crashBackend) createContainer(context.Context, string, string, string, *ResourceLimits, bool) (string, error) {
	return "", errors.New("unexpected createContainer")
}
func (f *crashBackend) inspectNetwork(context.Context, string) (*networkInspect, error) {
	return nil, errors.New("unexpected inspectNetwork")
}
func (f *crashBackend) pullImage(context.Context, string) error { return nil }
func (f *crashBackend) run(context.Context, ...string) (string, error) {
	return "", errors.New("unexpected run")
}

// newCrashRuntime returns a runtime over backend with network policy and daemon
// readiness stubbed, recording the addresses the policy was applied for. The
// addresses are the point: after a restart the rules have to follow the container.
func newCrashRuntime(t *testing.T, backend *crashBackend) (*Runtime, *[]string) {
	t.Helper()
	m := newRuntime(&config.Config{}, Config{}, backend)
	policyIPs := &[]string{}
	m.applyPolicy = func(_, _, sourceIP, _ string, _ int) error {
		*policyIPs = append(*policyIPs, sourceIP)
		return nil
	}
	m.teardownRules = func(string) error { return nil }
	m.waitForDaemon = func(context.Context, string) error { return nil }
	return m, policyIPs
}

func TestARestartedContainerIsNotServedUntilTheRunnerHasReAdmittedIt(t *testing.T) {
	backend := &crashBackend{states: []containerState{runningState()}, ip: "172.18.0.2"}
	m, policyIPs := newCrashRuntime(t, backend)
	rec := metrics.NewRunnerRecorder(true)
	m.SetMetricsRecorder(rec)

	// The container is running throughout: Docker restarted it before any request
	// arrived, which is what makes the death invisible without the event.
	m.handleContainerDeath(crashContainerID, crashSandboxID)
	backend.ip = "172.18.0.7"

	if _, err := m.DaemonURL(context.Background(), crashSandboxID); !errors.Is(err, ErrSandboxNotRunning) {
		t.Fatalf("DaemonURL() error = %v, want %v; a restarted sandbox must not be proxied to", err, ErrSandboxNotRunning)
	}

	wake, err := m.EnsureSandboxRunning(context.Background(), crashSandboxID)
	if err != nil {
		t.Fatalf("EnsureSandboxRunning() failed: %v", err)
	}
	if !wake.Recovered {
		t.Error("wake did not report the restart, so the request that drove it would be proxied to a sandbox that lost its state")
	}
	if want := []string{"172.18.0.7"}; len(*policyIPs) != 1 || (*policyIPs)[0] != want[0] {
		t.Errorf("network policy applied for %v, want %v: the rules have to follow the container's new address", *policyIPs, want)
	}

	// Re-admitted: from here the sandbox is an ordinary one again, and the restart is
	// reported once rather than to every later request.
	url, err := m.DaemonURL(context.Background(), crashSandboxID)
	if err != nil {
		t.Fatalf("DaemonURL() after recovery failed: %v", err)
	}
	if want := "http://172.18.0.7:8081"; url != want {
		t.Errorf("DaemonURL() = %q, want %q", url, want)
	}
	again, err := m.EnsureSandboxRunning(context.Background(), crashSandboxID)
	if err != nil {
		t.Fatalf("second EnsureSandboxRunning() failed: %v", err)
	}
	if again.Recovered {
		t.Error("the restart was reported twice")
	}

	if got := rec.GuestDeathCount(); got != 1 {
		t.Errorf("guest death metric = %v, want 1", got)
	}
	if got := rec.RecoveryCount(true); got != 1 {
		t.Errorf("recovery metric = %v, want 1", got)
	}
}

func TestAWakeThatCannotRepairARestartedSandboxLeavesItForTheNextRequest(t *testing.T) {
	backend := &crashBackend{states: []containerState{runningState()}, ip: "172.18.0.2"}
	m, _ := newCrashRuntime(t, backend)
	rec := metrics.NewRunnerRecorder(true)
	m.SetMetricsRecorder(rec)
	m.applyPolicy = func(string, string, string, string, int) error {
		return errors.New("iptables failed")
	}

	m.handleContainerDeath(crashContainerID, crashSandboxID)

	wake, err := m.EnsureSandboxRunning(context.Background(), crashSandboxID)
	if err == nil {
		t.Fatal("expected the wake to fail")
	}
	if !wake.Recovered {
		t.Error("a failed recovery did not report itself as one, so it would be metered as an ordinary wake")
	}
	if got := rec.RecoveryCount(false); got != 1 {
		t.Errorf("failed recovery metric = %v, want 1", got)
	}
	if _, err := m.DaemonURL(context.Background(), crashSandboxID); !errors.Is(err, ErrSandboxNotRunning) {
		t.Errorf("DaemonURL() error = %v, want %v; the sandbox is still unrepaired", err, ErrSandboxNotRunning)
	}
}

func TestAWakeWaitsOutDockersRestartRatherThanFailingTheRequest(t *testing.T) {
	backend := &crashBackend{
		states: []containerState{
			{Status: containerStatusRestarting, Restarting: true},
			{Status: containerStatusRestarting, Restarting: true},
			runningState(),
		},
		ip: "172.18.0.2",
	}
	m, policyIPs := newCrashRuntime(t, backend)

	m.handleContainerDeath(crashContainerID, crashSandboxID)

	wake, err := m.EnsureSandboxRunning(context.Background(), crashSandboxID)
	if err != nil {
		t.Fatalf("EnsureSandboxRunning() failed: %v", err)
	}
	if !wake.Recovered {
		t.Error("wake did not report the restart")
	}
	if len(*policyIPs) != 1 {
		t.Errorf("network policy applied %d times, want 1", len(*policyIPs))
	}
}

func TestStopsAndDeletesTheRunnerAsksForAreNotCrashes(t *testing.T) {
	tests := map[string]func(*Runtime) error{
		"stop": func(m *Runtime) error {
			return m.StopSandboxContainer(context.Background(), crashSandboxID)
		},
		"delete": func(m *Runtime) error {
			return m.DeleteSandbox(context.Background(), crashSandboxID)
		},
		"wake failure cleanup": func(m *Runtime) error {
			m.cleanupWakeFailure(crashContainerID)
			return nil
		},
	}
	for name, stop := range tests {
		t.Run(name, func(t *testing.T) {
			backend := &crashBackend{states: []containerState{runningState()}, ip: "172.18.0.2"}
			m, _ := newCrashRuntime(t, backend)
			rec := metrics.NewRunnerRecorder(true)
			m.SetMetricsRecorder(rec)

			if err := stop(m); err != nil {
				t.Fatalf("stop failed: %v", err)
			}
			// Docker reports the death of a container the runner stopped exactly as it
			// reports a crash; only the runner knows which it asked for.
			m.handleContainerDeath(crashContainerID, crashSandboxID)

			if m.wasRestarted(crashSandboxID) {
				t.Error("a deliberate stop was read as a crash, which would refuse the next request with a 409")
			}
			if got := rec.GuestDeathCount(); got != 0 {
				t.Errorf("guest death metric = %v, want 0", got)
			}
		})
	}
}

func TestAStopThatNeverDiedDoesNotMaskALaterCrash(t *testing.T) {
	backend := &crashBackend{states: []containerState{runningState()}, ip: "172.18.0.2"}
	m, _ := newCrashRuntime(t, backend)

	// A stop whose death never arrives — a container that was already exited when it
	// was removed — leaves a mark behind, and Docker reuses nothing but the runner
	// would keep it forever. Aged past its TTL it stops excusing deaths.
	token := m.expectStop(crashContainerID)
	m.mu.Lock()
	m.expectedStops[crashContainerID] = expectedStop{token: token, at: time.Now().Add(-2 * expectedStopTTL)}
	m.mu.Unlock()

	m.handleContainerDeath(crashContainerID, crashSandboxID)
	if !m.wasRestarted(crashSandboxID) {
		t.Error("a stop from another lifetime swallowed a real crash")
	}
	m.mu.Lock()
	_, kept := m.expectedStops[crashContainerID]
	m.mu.Unlock()
	if kept {
		t.Error("the expired stop was kept, so the map grows for every stop that never died")
	}
}

// The other half of that: a stop the runner asked for and did not get leaves the
// container running, so its mark has to go immediately rather than waiting out the
// TTL. Kept for even a moment past the failure it excuses the next real crash of the
// same container — served with no 409, and with network rules still naming the
// address the container had before Docker restarted it.
func TestAStopThatFailedDoesNotExcuseALaterCrash(t *testing.T) {
	failed := errors.New("docker stop timed out")
	tests := map[string]func(*crashBackend, *Runtime){
		"stop": func(b *crashBackend, m *Runtime) {
			b.stopErr = failed
			if err := m.StopSandboxContainer(context.Background(), crashSandboxID); err == nil {
				t.Fatal("expected the stop to fail")
			}
		},
		"delete": func(b *crashBackend, m *Runtime) {
			b.removeErr = failed
			if err := m.DeleteSandbox(context.Background(), crashSandboxID); err == nil {
				t.Fatal("expected the delete to fail")
			}
		},
		"wake failure cleanup": func(b *crashBackend, m *Runtime) {
			b.stopErr = failed
			m.cleanupWakeFailure(crashContainerID)
		},
	}
	for name, failStop := range tests {
		t.Run(name, func(t *testing.T) {
			backend := &crashBackend{states: []containerState{runningState()}, ip: "172.18.0.2"}
			m, policyIPs := newCrashRuntime(t, backend)
			rec := metrics.NewRunnerRecorder(true)
			m.SetMetricsRecorder(rec)

			failStop(backend, m)

			// The container survived the failed stop and then died on its own, well
			// inside expectedStopTTL. Docker restarts it on a new address.
			m.handleContainerDeath(crashContainerID, crashSandboxID)
			backend.ip = "172.18.0.9"

			if !m.wasRestarted(crashSandboxID) {
				t.Fatal("a stop that never happened swallowed a real crash")
			}
			if got := rec.GuestDeathCount(); got != 1 {
				t.Errorf("guest death metric = %v, want 1", got)
			}
			if _, err := m.DaemonURL(context.Background(), crashSandboxID); !errors.Is(err, ErrSandboxNotRunning) {
				t.Errorf("DaemonURL() error = %v, want %v; the crashed sandbox must not be proxied to", err, ErrSandboxNotRunning)
			}

			backend.stopErr, backend.removeErr = nil, nil
			wake, err := m.EnsureSandboxRunning(context.Background(), crashSandboxID)
			if err != nil {
				t.Fatalf("EnsureSandboxRunning() failed: %v", err)
			}
			if !wake.Recovered {
				t.Error("the wake did not report the restart, so the request that drove it would be proxied to a sandbox that lost its state")
			}
			if want := []string{"172.18.0.9"}; len(*policyIPs) != 1 || (*policyIPs)[0] != want[0] {
				t.Errorf("network policy applied for %v, want %v: the rules have to follow the container's new address", *policyIPs, want)
			}
		})
	}
}

// Stopping a container Docker is between restarts of is the one stop that gets no
// death of its own: the crash already emitted it, and the stop only cancels the
// restart. Docker reports such a container as running, so the stop is not a no-op —
// but it must record no expected death, or the mark nothing ever claims is spent on
// the next death of the same container, which is the crash after the wake below —
// served with no 409, and with network rules still naming the address it had.
func TestAStopDuringDockersRestartDoesNotExcuseALaterCrash(t *testing.T) {
	backend := &crashBackend{
		states: []containerState{
			// The stop and the wake both inspect: restarting first, then exited once
			// the stop has cancelled the restart.
			{Status: containerStatusRestarting, Running: true, Restarting: true},
			{Status: containerStatusExited},
		},
		ip: "172.18.0.2",
	}
	m, policyIPs := newCrashRuntime(t, backend)
	rec := metrics.NewRunnerRecorder(true)
	m.SetMetricsRecorder(rec)

	// The crash that put it into the restart loop, reported as one.
	m.handleContainerDeath(crashContainerID, crashSandboxID)
	if got := rec.GuestDeathCount(); got != 1 {
		t.Fatalf("guest death metric = %v, want 1", got)
	}

	// Idle long enough to be swept up while Docker was still restarting it.
	if err := m.StopSandboxContainer(context.Background(), crashSandboxID); err != nil {
		t.Fatalf("StopSandboxContainer() failed: %v", err)
	}
	if backend.stops != 1 {
		t.Errorf("docker stop called %d times, want 1: a restarting container is reported running, so the stop is not a no-op", backend.stops)
	}

	// The client comes back and the sandbox is started again.
	if _, err := m.EnsureSandboxRunning(context.Background(), crashSandboxID); err != nil {
		t.Fatalf("EnsureSandboxRunning() failed: %v", err)
	}
	backend.ip = "172.18.0.9"

	// It crashes again, still inside expectedStopTTL of that stop.
	m.handleContainerDeath(crashContainerID, crashSandboxID)

	if !m.wasRestarted(crashSandboxID) {
		t.Fatal("a stop that never had a death of its own swallowed a real crash")
	}
	if got := rec.GuestDeathCount(); got != 2 {
		t.Errorf("guest death metric = %v, want 2", got)
	}
	if _, err := m.DaemonURL(context.Background(), crashSandboxID); !errors.Is(err, ErrSandboxNotRunning) {
		t.Errorf("DaemonURL() error = %v, want %v; the crashed sandbox must not be proxied to", err, ErrSandboxNotRunning)
	}
	wake, err := m.EnsureSandboxRunning(context.Background(), crashSandboxID)
	if err != nil {
		t.Fatalf("second EnsureSandboxRunning() failed: %v", err)
	}
	if !wake.Recovered {
		t.Error("the wake did not report the second crash, so the request that drove it would be proxied to a sandbox that lost its state")
	}
	if want := "172.18.0.9"; len(*policyIPs) == 0 || (*policyIPs)[len(*policyIPs)-1] != want {
		t.Errorf("network policy applied for %v, want %v last: the rules have to follow the container's new address", *policyIPs, want)
	}
}

// Nothing in this runtime serializes a stop against a delete or against the wake
// path's own cleanup — the gateway lock that separates stop from delete is not held
// over the proxy path a wake comes from — so two of them can be recording expected
// stops for one container at once, and the second overwrites the one mark there is.
// The call that fails has to take back only its own: dropping the other's leaves a
// death the runner did ask for read as a crash, and the next request paying a 409
// for a restart that never happened.
func TestAFailedStopDoesNotTakeBackAConcurrentStopsMark(t *testing.T) {
	backend := &crashBackend{states: []containerState{runningState()}, ip: "172.18.0.2"}
	m, _ := newCrashRuntime(t, backend)
	rec := metrics.NewRunnerRecorder(true)
	m.SetMetricsRecorder(rec)

	// One call records and is still in flight; a second records over it and its stop
	// is the one that reaches Docker.
	failing := m.expectStop(crashContainerID)
	surviving := m.expectStop(crashContainerID)
	if failing == surviving {
		t.Fatal("expectStop handed both calls the same token, so a failed call cannot tell its own mark from another's")
	}
	m.forgetExpectedStop(crashContainerID, failing)

	// The death the surviving stop asked for.
	m.handleContainerDeath(crashContainerID, crashSandboxID)

	if m.wasRestarted(crashSandboxID) {
		t.Error("a death the runner asked for was recorded as a crash, so the next request is served a 409 for a restart that never happened")
	}
	if got := rec.GuestDeathCount(); got != 0 {
		t.Errorf("guest death metric = %v, want 0: a stop the runner asked for is not a crash", got)
	}
}

// A stop's die event reaches the runner through a `docker events` subprocess, so it
// can still be in that pipe when the next request wakes the sandbox and starts the
// container again — an idle stop followed by a client coming back is exactly that
// sequence. The mark has to survive the start: dropping it there left the stop's own
// death to be read as a crash, and the request after it paying a 409 for a restart
// that never happened.
func TestAStopsDeathIsStillExcusedWhenAWakeBeatsTheEvent(t *testing.T) {
	backend := &crashBackend{
		states: []containerState{
			// The stop inspects a healthy container; the wake that follows finds it
			// exited, because the stop it is racing has already landed.
			runningState(),
			{Status: containerStatusExited},
		},
		ip: "172.18.0.2",
	}
	m, _ := newCrashRuntime(t, backend)
	rec := metrics.NewRunnerRecorder(true)
	m.SetMetricsRecorder(rec)

	if err := m.StopSandboxContainer(context.Background(), crashSandboxID); err != nil {
		t.Fatalf("StopSandboxContainer() failed: %v", err)
	}
	if _, err := m.EnsureSandboxRunning(context.Background(), crashSandboxID); err != nil {
		t.Fatalf("EnsureSandboxRunning() failed: %v", err)
	}

	// Only now does the stop's death come out of the pipe.
	m.handleContainerDeath(crashContainerID, crashSandboxID)

	if m.wasRestarted(crashSandboxID) {
		t.Error("an idle stop's own death was read as a crash, so the next request is refused with a 409 for a restart that never happened")
	}
	if got := rec.GuestDeathCount(); got != 0 {
		t.Errorf("guest death metric = %v, want 0: a stop the runner asked for is not a crash", got)
	}
}

func TestTheDeathWatcherReconnectsUntilTheRunnerStops(t *testing.T) {
	connects := make(chan struct{}, 8)
	backend := &crashBackend{
		states: []containerState{runningState()},
		watch: func(_ context.Context, onDie func(string, string)) error {
			select {
			case connects <- struct{}{}:
			default:
			}
			onDie(crashContainerID, crashSandboxID)
			return errors.New("stream broke")
		},
	}
	m, _ := newCrashRuntime(t, backend)
	m.watchBackoff = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.watchGuestDeaths(ctx)
		close(done)
	}()

	// Two connects: losing the event stream is silent, so the runner has to come back
	// to it rather than stop reporting crashes for the rest of its life.
	for i := range 2 {
		select {
		case <-connects:
		case <-time.After(5 * time.Second):
			t.Fatalf("watcher did not connect %d time(s)", i+1)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not stop with its context")
	}
	if !m.wasRestarted(crashSandboxID) {
		t.Error("a death read from the stream was not recorded against its sandbox")
	}
}

var _ dockerBackend = (*crashBackend)(nil)
var _ runnerruntime.Runtime = (*Runtime)(nil)
