package firecracker

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/runner/config"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

type fakeProcess struct {
	killed bool
}

func (p *fakeProcess) Kill() error {
	p.killed = true
	return nil
}

type fakeProxy struct {
	stopped bool
}

func (p *fakeProxy) Stop() error {
	p.stopped = true
	return nil
}

type recordedCommand struct {
	name string
	args []string
}

func testRunnerConfig(capacity int32) *config.Config {
	return &config.Config{
		CapacityTotal: capacity,
	}
}

func testConfig() Config {
	return Config{
		JailerBin:               "/opt/firecracker/bin/jailer",
		FirecrackerBin:          "/opt/firecracker/bin/firecracker",
		JailerBaseDir:           "/srv/jailer",
		TemplateDir:             "/srv/firecracker/template",
		SnapshotMemPath:         "/srv/firecracker/snapshots/mem",
		SnapshotStatePath:       "/srv/firecracker/snapshots/state",
		SnapshotVirtioBlockPath: "/rootfs.ext4",
		GuestIP:                 "172.16.0.10",
		HostTapDeviceName:       "fc-tap-0",
		HostTapIPCIDR:           "172.16.0.1/24",
		DaemonPort:              8081,
		ProxyListenIP:           "127.0.0.1",
		ProxyPortStart:          18081,
		SocketWaitAttempts:      1,
		SocketWaitInterval:      time.Nanosecond,
		DaemonWaitTimeout:       time.Second,
	}
}

func testRuntime(capacity int32) *Runtime {
	return New(testRunnerConfig(capacity), testConfig())
}

func TestRuntimeReadyChecksFirecrackerAssets(t *testing.T) {
	rt := testRuntime(1)
	rt.deps.pathExists = func(path string) bool {
		return path != "/srv/firecracker/snapshots/state"
	}

	if err := rt.Ready(context.Background()); err == nil || !strings.Contains(err.Error(), "snapshot state") {
		t.Fatalf("Ready() error = %v, want missing snapshot state", err)
	}
}

func TestRuntimeCreateSandboxStartsFirecrackerAndProxy(t *testing.T) {
	rt := testRuntime(2)
	proc := &fakeProcess{}
	proxy := &fakeProxy{}
	var commands []recordedCommand
	var started []recordedCommand
	var loadedSocket string
	var proxyListenAddr string
	var proxyNetNS string
	var proxyGuestAddr string
	rt.deps.run = func(_ context.Context, name string, args ...string) error {
		commands = append(commands, recordedCommand{name: name, args: args})
		return nil
	}
	rt.deps.start = func(_ context.Context, name string, args ...string) (process, error) {
		started = append(started, recordedCommand{name: name, args: args})
		return proc, nil
	}
	rt.deps.pathExists = func(string) bool { return true }
	rt.deps.loadSnapshot = func(_ context.Context, socketPath string, _ Config) error {
		loadedSocket = socketPath
		return nil
	}
	rt.deps.newProxy = func(_ context.Context, listenAddr string, netnsName string, guestAddr string) (daemonProxy, error) {
		proxyListenAddr = listenAddr
		proxyNetNS = netnsName
		proxyGuestAddr = guestAddr
		return proxy, nil
	}
	rt.deps.probeDaemon = func(_ context.Context, baseURL string) error {
		if baseURL != "http://127.0.0.1:18081" {
			t.Fatalf("probeDaemon baseURL = %s", baseURL)
		}
		return nil
	}

	info, err := rt.CreateSandbox(context.Background(), "sandbox-id-123456", nil)
	if err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	if info.ID != "sandbox-id-123456" || info.IP != "172.16.0.10" {
		t.Fatalf("CreateSandbox() info = %+v", info)
	}
	if len(commands) != 2 {
		t.Fatalf("run commands = %d, want prepare and network setup", len(commands))
	}
	if len(started) != 1 {
		t.Fatalf("start commands = %d, want jailer start", len(started))
	}
	if !containsArg(started[0].args, "--netns") || !containsArg(started[0].args, "/run/netns/fc-sb-0") {
		t.Fatalf("jailer args = %v, want per-slot netns", started[0].args)
	}
	if loadedSocket != "/srv/jailer/firecracker/"+info.Name+"/root/firecracker.socket" {
		t.Fatalf("loaded socket = %s", loadedSocket)
	}
	if proxyListenAddr != "127.0.0.1:18081" || proxyNetNS != "fc-sb-0" || proxyGuestAddr != "172.16.0.10:8081" {
		t.Fatalf("proxy = listen %s netns %s guest %s", proxyListenAddr, proxyNetNS, proxyGuestAddr)
	}

	url, err := rt.DaemonURL(context.Background(), "sandbox-id-123456")
	if err != nil {
		t.Fatalf("DaemonURL() failed: %v", err)
	}
	if url != "http://127.0.0.1:18081" {
		t.Fatalf("DaemonURL() = %s", url)
	}

	capacity, err := rt.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity() failed: %v", err)
	}
	if capacity.Used != 1 || capacity.Total != 2 {
		t.Fatalf("Capacity() = %+v, want used=1 total=2", capacity)
	}

	if err := rt.DeleteSandbox(context.Background(), "sandbox-id-123456"); err != nil {
		t.Fatalf("DeleteSandbox() failed: %v", err)
	}
	if !proc.killed {
		t.Fatal("expected process to be killed")
	}
	if !proxy.stopped {
		t.Fatal("expected proxy to be stopped")
	}
}

func TestRuntimeCreateSandboxEnforcesCapacity(t *testing.T) {
	rt := testRuntime(1)
	rt.deps.run = func(context.Context, string, ...string) error { return nil }
	rt.deps.start = func(context.Context, string, ...string) (process, error) { return &fakeProcess{}, nil }
	rt.deps.pathExists = func(string) bool { return true }
	rt.deps.loadSnapshot = func(context.Context, string, Config) error { return nil }
	rt.deps.newProxy = func(context.Context, string, string, string) (daemonProxy, error) { return &fakeProxy{}, nil }
	rt.deps.probeDaemon = func(context.Context, string) error { return nil }

	if _, err := rt.CreateSandbox(context.Background(), "sandbox-id-123456", nil); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	if _, err := rt.CreateSandbox(context.Background(), "sandbox-id-abcdef", nil); err == nil {
		t.Fatal("expected capacity exhausted error")
	}
}

func TestRuntimeDeleteHoldsSlotUntilCleanupCompletes(t *testing.T) {
	rt := testRuntime(1)
	proc := &fakeProcess{}
	proxy := &fakeProxy{}
	cleanupStarted := make(chan struct{})
	allowCleanup := make(chan struct{})
	var runCount int
	rt.deps.run = func(context.Context, string, ...string) error {
		runCount++
		if runCount == 3 {
			close(cleanupStarted)
			<-allowCleanup
		}
		return nil
	}
	rt.deps.start = func(context.Context, string, ...string) (process, error) { return proc, nil }
	rt.deps.pathExists = func(string) bool { return true }
	rt.deps.loadSnapshot = func(context.Context, string, Config) error { return nil }
	rt.deps.newProxy = func(context.Context, string, string, string) (daemonProxy, error) { return proxy, nil }
	rt.deps.probeDaemon = func(context.Context, string) error { return nil }

	if _, err := rt.CreateSandbox(context.Background(), "sandbox-id-123456", nil); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}

	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- rt.DeleteSandbox(context.Background(), "sandbox-id-123456")
	}()

	<-cleanupStarted

	if _, err := rt.CreateSandbox(context.Background(), "sandbox-id-abcdef", nil); err == nil {
		t.Fatal("expected capacity exhausted while cleanup still owns the slot")
	}
	capacity, err := rt.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity() failed: %v", err)
	}
	if capacity.Used != 1 {
		t.Fatalf("Capacity().Used = %d, want 1 while cleanup still owns the slot", capacity.Used)
	}

	close(allowCleanup)
	if err := <-deleteDone; err != nil {
		t.Fatalf("DeleteSandbox() failed: %v", err)
	}
	if _, err := rt.CreateSandbox(context.Background(), "sandbox-id-abcdef", nil); err != nil {
		t.Fatalf("CreateSandbox() after cleanup failed: %v", err)
	}
}

func TestRuntimeReleaseSlotPanicsForInvalidSlot(t *testing.T) {
	rt := testRuntime(1)
	defer func() {
		if recover() == nil {
			t.Fatal("expected releaseSlotLocked to panic for invalid slot")
		}
	}()

	rt.releaseSlotLocked(1)
}

func TestRuntimeDeleteSandboxDoesNotReleaseReassignedSlot(t *testing.T) {
	rt := testRuntime(1)
	rt.deps.run = func(context.Context, string, ...string) error { return nil }

	oldState := &sandboxState{id: "sandbox-id-old123", slot: 0}
	newState := &sandboxState{id: "sandbox-id-new456", slot: 0}
	rt.sandboxes[newState.id] = newState
	rt.slots[0].sandboxID = newState.id

	if err := rt.deleteSandbox(context.Background(), oldState); err != nil {
		t.Fatalf("deleteSandbox() failed: %v", err)
	}

	capacity, err := rt.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity() failed: %v", err)
	}
	if capacity.Used != 1 {
		t.Fatalf("Capacity().Used = %d, want reassigned slot to remain occupied", capacity.Used)
	}
	if rt.slots[0].sandboxID != newState.id {
		t.Fatalf("slot owner = %q, want %q", rt.slots[0].sandboxID, newState.id)
	}
}

func TestRuntimeCreateSandboxCleansUpOnFailure(t *testing.T) {
	rt := testRuntime(1)
	proc := &fakeProcess{}
	proxy := &fakeProxy{}
	var runCount int
	rt.deps.run = func(context.Context, string, ...string) error {
		runCount++
		return nil
	}
	rt.deps.start = func(context.Context, string, ...string) (process, error) { return proc, nil }
	rt.deps.pathExists = func(string) bool { return true }
	rt.deps.loadSnapshot = func(context.Context, string, Config) error { return nil }
	rt.deps.newProxy = func(context.Context, string, string, string) (daemonProxy, error) { return proxy, nil }
	rt.deps.probeDaemon = func(context.Context, string) error { return errors.New("daemon down") }

	if _, err := rt.CreateSandbox(context.Background(), "sandbox-id-123456", nil); err == nil {
		t.Fatal("expected CreateSandbox() failure")
	}
	if !proc.killed {
		t.Fatal("expected process to be killed during cleanup")
	}
	if !proxy.stopped {
		t.Fatal("expected proxy to be stopped during cleanup")
	}
	if runCount != 3 {
		t.Fatalf("runCount = %d, want prepare, network, and cleanup", runCount)
	}

	capacity, err := rt.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity() failed: %v", err)
	}
	if capacity.Used != 0 {
		t.Fatalf("Capacity().Used = %d, want 0 after cleanup", capacity.Used)
	}
	if _, err := rt.GetSandboxInfo(context.Background(), "sandbox-id-123456"); !errors.Is(err, runnerruntime.ErrSandboxNotFound) {
		t.Fatalf("GetSandboxInfo() error = %v, want ErrSandboxNotFound", err)
	}
}

func TestProbeDaemonRejectsUnhealthyStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := probeDaemon(ctx, server.URL, time.Second); err == nil {
		t.Fatal("expected unhealthy status to fail readiness probe")
	}
}

func TestStartCommandRunsInOwnProcessGroup(t *testing.T) {
	proc, err := startCommand(context.Background(), "sh", "-c", "sleep 10")
	if err != nil {
		t.Fatalf("startCommand() failed: %v", err)
	}
	defer proc.Kill()

	groupProc, ok := proc.(*processGroup)
	if !ok {
		t.Fatalf("startCommand() returned %T, want *processGroup", proc)
	}
	pgid, err := syscall.Getpgid(groupProc.process.Pid)
	if err != nil {
		t.Fatalf("Getpgid() failed: %v", err)
	}
	if pgid != groupProc.process.Pid {
		t.Fatalf("process group = %d, want process pid %d", pgid, groupProc.process.Pid)
	}
}

func TestTCPDaemonProxyStopCancelsActiveGuestDial(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() failed: %v", err)
	}
	proxyCtx, cancel := context.WithCancel(context.Background())
	dialStarted := make(chan struct{})
	proxy := &tcpDaemonProxy{
		listener: listener,
		done:     make(chan struct{}),
		ctx:      proxyCtx,
		cancel:   cancel,
		dial: func(ctx context.Context, _ string, _ string, _ string) (net.Conn, error) {
			close(dialStarted)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	go proxy.serve("fc-sb-0", "172.16.0.10:8081")
	clientConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial() failed: %v", err)
	}
	defer clientConn.Close()

	select {
	case <-dialStarted:
	case <-time.After(time.Second):
		t.Fatal("guest dial did not start")
	}

	stopped := make(chan error, 1)
	go func() {
		stopped <- proxy.Stop()
	}()

	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Stop() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop() did not cancel active guest dial")
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
