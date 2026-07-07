package firecracker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/runner/config"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
	fcnetwork "github.com/n8n-io/sandbox-service/internal/runner/runtime/firecracker.ee/network"
	"github.com/n8n-io/sandbox-service/internal/shellquote"
	"golang.org/x/sync/singleflight"
)

// Runtime manages Firecracker microVM sandboxes using the same runner-facing
// contract as the Docker/sysbox backend.
type Runtime struct {
	runnerConfig *config.Config
	config       Config
	deps         dependencies
	slots        []slotState

	mu          sync.Mutex
	sandboxes   map[string]*sandboxState
	wakeGroup   singleflight.Group
	metrics     *metrics.RunnerRecorder
	readyCh     chan struct{}
	hostNATOnce sync.Once
	hostNATErr  error
}

var _ runnerruntime.Runtime = (*Runtime)(nil)

// SetMetricsRecorder attaches the runner metrics recorder for operations that
// complete inside the runtime (for example LRU evictions).
func (r *Runtime) SetMetricsRecorder(rec *metrics.RunnerRecorder) {
	r.metrics = rec
}

func New(runnerConfig *config.Config, cfg Config) *Runtime {
	rt := &Runtime{
		runnerConfig: runnerConfig,
		config:       cfg,
		deps:         defaultDependencies(cfg),
		slots:        make([]slotState, maxInt32(runnerConfig.CapacityTotal, 0)),
		sandboxes:    make(map[string]*sandboxState),
		readyCh:      make(chan struct{}),
	}
	close(rt.readyCh)
	return rt
}

// slotState tracks one runner-local Firecracker slot. A slot reserves the host
// resources derived from its index: netns name and daemon proxy port.
type slotState struct {
	sandboxID string
}

func (s slotState) occupied() bool {
	return s.sandboxID != ""
}

// sandboxState holds the host resources backing one live microVM sandbox.
type sandboxState struct {
	id                string
	vmID              string
	slot              int
	info              *runnerruntime.SandboxInfo
	netnsName         string
	socketPath        string
	daemonURL         string
	dataDir           string
	rootfsPath        string
	snapshotMemPath   string
	snapshotStatePath string
	process           process
	proxy             daemonProxy
	running           bool
	stopped           bool
	stopping          bool
	stoppedAt         time.Time
	deleting          bool
}

// process is the minimum process handle needed for sandbox cleanup.
type process interface {
	Kill() error
}

// processGroup kills Firecracker and any children started in its process group.
type processGroup struct {
	process *os.Process
}

func (p *processGroup) Kill() error {
	if err := syscall.Kill(-p.process.Pid, syscall.SIGKILL); err != nil {
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
	return nil
}

// daemonProxy is the host-local proxy for a sandbox guest daemon.
type daemonProxy interface {
	Stop() error
}

// dependencies groups host operations so tests can replace shell, process, and network calls.
type dependencies struct {
	run                 func(ctx context.Context, name string, args ...string) error
	start               func(ctx context.Context, name string, args ...string) (process, error)
	pathExists          func(path string) bool
	cloneRootfs         func(ctx context.Context, templatePath, destPath string) error
	cloneGoldenSnapshot func(ctx context.Context, goldenMemPath, goldenStatePath, dataDir string) error
	pauseVM             func(ctx context.Context, socketPath string) error
	createSnapshot      func(ctx context.Context, socketPath string) error
	loadSnapshot        func(ctx context.Context, socketPath string, cfg Config) error
	newProxy            func(ctx context.Context, listenAddr string, netnsName string, guestAddr string) (daemonProxy, error)
	probeDaemon         func(ctx context.Context, baseURL string) error
	freeBytesInDir      func(path string) (int64, error)
}

func defaultDependencies(fc Config) dependencies {
	return dependencies{
		run:                 runCommand,
		start:               startCommand,
		pathExists:          pathExists,
		cloneRootfs:         cloneRootfs,
		cloneGoldenSnapshot: cloneGoldenSnapshotAssets,
		pauseVM:             pauseVM,
		createSnapshot:      createSnapshot,
		loadSnapshot:        loadSnapshot,
		newProxy: func(ctx context.Context, listenAddr string, netnsName string, guestAddr string) (daemonProxy, error) {
			return startDaemonProxy(ctx, listenAddr, netnsName, guestAddr)
		},
		probeDaemon: func(ctx context.Context, baseURL string) error {
			return probeDaemon(ctx, baseURL, fc.DaemonWaitTimeout)
		},
		freeBytesInDir: freeBytesInDir,
	}
}

// Prepare ensures host-level NAT prerequisites exist for sandbox netns uplinks.
func (r *Runtime) Prepare(ctx context.Context) {
	_ = r.ensureHostNATReady(ctx)
}

func (r *Runtime) ensureHostNATReady(ctx context.Context) error {
	r.hostNATOnce.Do(func() {
		if err := fcnetwork.EnsureHostNAT(ctx, r.deps.run); err != nil {
			slog.Error("firecracker host NAT setup failed", "error", err)
			r.hostNATErr = err
		}
	})
	return r.hostNATErr
}

// Ready checks that the host has the Firecracker binaries and snapshot assets
// needed to accept sandbox work.
func (r *Runtime) Ready(context.Context) error {
	requiredPaths := map[string]string{
		"jailer":          r.config.JailerBin,
		"firecracker":     r.config.FirecrackerBin,
		"template rootfs": filepath.Join(r.config.TemplateDir, "rootfs.ext4"),
		"snapshot memory": r.config.SnapshotMemPath,
		"snapshot state":  r.config.SnapshotStatePath,
	}
	for label, path := range requiredPaths {
		if !r.deps.pathExists(path) {
			return fmt.Errorf("firecracker %s path does not exist: %s", label, path)
		}
	}
	if len(r.slots) == 0 {
		return fmt.Errorf("firecracker runtime has no capacity")
	}
	return nil
}

// ReadyCh is already closed because the Firecracker backend has no asynchronous
// image-pull phase like the Docker backend.
func (r *Runtime) ReadyCh() <-chan struct{} {
	return r.readyCh
}

// Capacity reports slot-blocking sandboxes (Used) and stopped snapshots (Stopped).
func (r *Runtime) Capacity(context.Context) (runnerruntime.Capacity, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	stopped := 0
	for _, state := range r.sandboxes {
		if !state.deleting && state.stopped {
			stopped++
		}
	}
	return runnerruntime.Capacity{
		Used:    int32(r.occupiedSlotsLocked()),
		Total:   int32(len(r.slots)),
		Stopped: int32(stopped),
	}, nil
}

// CreateSandbox restores one microVM snapshot into a per-slot jail and netns,
// then exposes the guest daemon through a host-local TCP proxy.
func (r *Runtime) CreateSandbox(ctx context.Context, sandboxID string, _ *runnerruntime.CreateOptions) (*runnerruntime.SandboxInfo, error) {
	if len(sandboxID) < 12 {
		return nil, fmt.Errorf("sandbox ID must be at least 12 characters, got %d", len(sandboxID))
	}

	state, err := r.reserveSandbox(sandboxID)
	if err != nil {
		return nil, err
	}
	slog.Info(
		"firecracker sandbox create started",
		"sandbox_id", sandboxID,
		"vm_id", state.vmID,
		"slot", state.slot,
		"netns", state.netnsName,
		"daemon_url", state.daemonURL,
	)
	cleanupOnError := func() {
		_ = r.deleteSandbox(ctx, state)
	}

	templateRootfs := filepath.Join(r.config.TemplateDir, "rootfs.ext4")
	slog.Debug("firecracker cloning rootfs", "sandbox_id", sandboxID, "template", templateRootfs, "dest", state.rootfsPath)
	if err := r.deps.cloneRootfs(ctx, templateRootfs, state.rootfsPath); err != nil {
		cleanupOnError()
		return nil, fmt.Errorf("clone sandbox rootfs: %w", err)
	}
	slog.Debug("firecracker cloning golden snapshot", "sandbox_id", sandboxID, "data_dir", state.dataDir)
	if err := r.deps.cloneGoldenSnapshot(ctx, r.config.SnapshotMemPath, r.config.SnapshotStatePath, state.dataDir); err != nil {
		cleanupOnError()
		return nil, fmt.Errorf("clone sandbox snapshot assets: %w", err)
	}

	if err := r.activateSandboxVM(ctx, state); err != nil {
		cleanupOnError()
		return nil, err
	}

	r.mu.Lock()
	state.running = true
	info := *state.info
	r.mu.Unlock()
	slog.Info("firecracker sandbox created", "sandbox_id", sandboxID, "vm_id", state.vmID, "slot", state.slot, "daemon_url", state.daemonURL)
	return &info, nil
}

// GetSandboxInfo returns the runner-facing sandbox metadata tracked in memory.
func (r *Runtime) GetSandboxInfo(_ context.Context, sandboxID string) (*runnerruntime.SandboxInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	state, ok := r.sandboxes[sandboxID]
	if !ok || state.deleting {
		return nil, runnerruntime.ErrSandboxNotFound
	}
	info := *state.info
	return &info, nil
}

// DeleteSandbox tears down the microVM (if running), proxy, netns, jail state, and data dir.
func (r *Runtime) DeleteSandbox(ctx context.Context, sandboxID string) error {
	state, err := r.takeSandbox(sandboxID)
	if err != nil {
		return err
	}
	return r.deleteSandbox(ctx, state)
}

// DaemonURL returns the host-local proxy URL, not the guest IP directly.
func (r *Runtime) DaemonURL(_ context.Context, sandboxID string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	state, ok := r.sandboxes[sandboxID]
	if !ok || state.deleting {
		return "", runnerruntime.ErrSandboxNotFound
	}
	if state.stopping || state.stopped || !state.running {
		// Reject proxies while stopping, stopped, or not yet running (create in progress).
		return "", runnerruntime.ErrSandboxNotRunning
	}
	if state.daemonURL == "" {
		return "", runnerruntime.ErrSandboxNetworkUnavailable
	}
	return state.daemonURL, nil
}

// Shutdown best-effort deletes every sandbox currently tracked by this runtime.
func (r *Runtime) Shutdown(ctx context.Context) {
	r.mu.Lock()
	states := make([]*sandboxState, 0, len(r.sandboxes))
	for _, state := range r.sandboxes {
		if state.deleting {
			continue
		}
		states = append(states, state)
		state.running = false
		state.deleting = true
	}
	r.mu.Unlock()

	for _, state := range states {
		_ = r.deleteSandbox(ctx, state)
	}
}

// reserveSandbox assigns the sandbox to the first free slot and derives the
// deterministic per-slot host resources used for the VM.
func (r *Runtime) reserveSandbox(sandboxID string) (*sandboxState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.sandboxes[sandboxID]; ok {
		return nil, fmt.Errorf("sandbox already exists: %s", sandboxID)
	}
	slot := r.reserveSlotLocked(sandboxID)
	if slot < 0 {
		return nil, fmt.Errorf("firecracker runner capacity exhausted")
	}

	vmID := "sandbox-" + shortID(sandboxID)
	netnsName := fmt.Sprintf("fc-sb-%d", slot)
	socketPath := filepath.Join(r.config.JailerBaseDir, "firecracker", vmID, "root", "firecracker.socket")
	daemonURL := fmt.Sprintf("http://%s", net.JoinHostPort(r.config.ProxyListenIP, fmt.Sprintf("%d", r.config.ProxyPortStart+slot)))
	dataDir := sandboxDataDir(r.runnerConfig.DataDir, sandboxID)
	state := &sandboxState{
		id:                sandboxID,
		vmID:              vmID,
		slot:              slot,
		netnsName:         netnsName,
		socketPath:        socketPath,
		daemonURL:         daemonURL,
		dataDir:           dataDir,
		rootfsPath:        sandboxRootfsPath(r.runnerConfig.DataDir, sandboxID),
		snapshotMemPath:   sandboxSnapshotMemPath(dataDir),
		snapshotStatePath: sandboxSnapshotStatePath(dataDir),
		info: &runnerruntime.SandboxInfo{
			ID:   sandboxID,
			Name: vmID,
			IP:   r.config.GuestIP,
		},
	}
	r.sandboxes[sandboxID] = state
	return state, nil
}

// takeSandbox marks the sandbox as deleting while keeping its slot reserved
// until host cleanup finishes.
func (r *Runtime) takeSandbox(sandboxID string) (*sandboxState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	state, ok := r.sandboxes[sandboxID]
	if !ok || state.deleting {
		return nil, runnerruntime.ErrSandboxNotFound
	}
	state.running = false
	state.deleting = true
	return state, nil
}

// deleteSandbox stops host-side resources for one sandbox before releasing its slot.
func (r *Runtime) deleteSandbox(ctx context.Context, state *sandboxState) error {
	slog.Debug("firecracker sandbox cleanup started", "sandbox_id", state.id, "vm_id", state.vmID, "slot", state.slot)
	r.mu.Lock()
	if current, ok := r.sandboxes[state.id]; ok && current == state {
		state.running = false
		state.deleting = true
	}
	r.mu.Unlock()

	var errs []error
	if state.process != nil || state.proxy != nil {
		if err := r.teardownRunningVM(ctx, state); err != nil {
			slog.Warn("firecracker host cleanup failed", "sandbox_id", state.id, "err", err)
			errs = append(errs, err)
		}
	}
	if err := removeSandboxDataDir(ctx, state.dataDir); err != nil {
		slog.Warn("firecracker sandbox data cleanup failed", "sandbox_id", state.id, "data_dir", state.dataDir, "err", err)
		errs = append(errs, fmt.Errorf("remove sandbox data dir: %w", err))
	}
	slog.Debug("firecracker sandbox cleanup finished", "sandbox_id", state.id, "vm_id", state.vmID, "slot", state.slot)
	if err := joinErrors(errs); err != nil {
		r.mu.Lock()
		if current, ok := r.sandboxes[state.id]; ok && current == state {
			state.deleting = false
		}
		r.mu.Unlock()
		return err
	}

	r.mu.Lock()
	if current, ok := r.sandboxes[state.id]; ok && current == state {
		delete(r.sandboxes, state.id)
		if state.slot >= 0 && r.slotOwnedByLocked(state.slot, state.id) {
			r.releaseSlotLocked(state.slot)
		}
	}
	r.mu.Unlock()
	return nil
}

// reserveSlotLocked marks the first free Firecracker slot as occupied. r.mu
// must be held by the caller.
func (r *Runtime) reserveSlotLocked(sandboxID string) int {
	for i := range r.slots {
		if !r.slots[i].occupied() {
			r.slots[i].sandboxID = sandboxID
			return i
		}
	}
	return -1
}

// occupiedSlotsLocked counts slots reserved by active or deleting sandboxes.
func (r *Runtime) occupiedSlotsLocked() int {
	used := 0
	for i := range r.slots {
		if r.slots[i].occupied() {
			used++
		}
	}
	return used
}

// releaseSlotLocked marks a Firecracker slot as free. r.mu must be held by the
// caller.
func (r *Runtime) releaseSlotLocked(slot int) {
	if slot < 0 || slot >= len(r.slots) {
		panic(fmt.Sprintf("firecracker slot index out of range: %d", slot))
	}
	r.slots[slot] = slotState{}
}

// slotOwnedByLocked reports whether the slot is still reserved for the sandbox
// whose state is being cleaned up. r.mu must be held by the caller.
func (r *Runtime) slotOwnedByLocked(slot int, sandboxID string) bool {
	if slot < 0 || slot >= len(r.slots) {
		panic(fmt.Sprintf("firecracker slot index out of range: %d", slot))
	}
	return r.slots[slot].sandboxID == sandboxID
}

// daemonURLAddr returns the TCP listen address form expected by net.Listen.
func (s *sandboxState) daemonURLAddr() string {
	return strings.TrimPrefix(s.daemonURL, "http://")
}

// prepareJail creates the jail root and bind-mounts snapshot assets at the
// paths expected by the restored Firecracker snapshot.
func (r *Runtime) prepareJail(ctx context.Context, state *sandboxState) error {
	jailRoot := filepath.Join(r.config.JailerBaseDir, "firecracker", state.vmID, "root")
	rootfsTarget := filepath.Join(jailRoot, strings.TrimPrefix(r.config.SnapshotVirtioBlockPath, "/"))
	script := fmt.Sprintf(`
set -eu
mkdir -p %[1]s
mkdir -p %[5]s
touch %[1]s/snapshot_mem %[1]s/snapshot_state %[6]s
mount --bind %[2]s %[1]s/snapshot_mem
mount --bind %[3]s %[1]s/snapshot_state
mount --bind %[4]s %[6]s
chown 1000:1000 %[1]s/snapshot_mem %[1]s/snapshot_state %[6]s
chmod 0664 %[1]s/snapshot_mem %[1]s/snapshot_state %[6]s
`, shellquote.Quote(jailRoot), shellquote.Quote(state.snapshotMemPath), shellquote.Quote(state.snapshotStatePath), shellquote.Quote(state.rootfsPath), shellquote.Quote(filepath.Dir(rootfsTarget)), shellquote.Quote(rootfsTarget))
	return r.deps.run(ctx, "sudo", "/bin/sh", "-c", script)
}

// setupNetwork creates one network namespace per sandbox slot with TAP, veth
// uplink, and per-netns egress iptables matching Docker private-CIDR policy.
func (r *Runtime) setupNetwork(ctx context.Context, state *sandboxState) error {
	if err := r.ensureHostNATReady(ctx); err != nil {
		return fmt.Errorf("host NAT not configured: %w", err)
	}
	script := fcnetwork.SetupScript(state.slot, state.netnsName, r.config.HostTapDeviceName, r.config.HostTapIPCIDR)
	return r.deps.run(ctx, "sudo", "/bin/sh", "-c", script)
}

// startJailer starts Firecracker through jailer inside the sandbox netns.
func (r *Runtime) startJailer(ctx context.Context, state *sandboxState) (process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.deps.start(ctx,
		"sudo",
		r.config.JailerBin,
		"--id", state.vmID,
		"--exec-file", r.config.FirecrackerBin,
		"--uid", "1000",
		"--gid", "1000",
		"--chroot-base-dir", r.config.JailerBaseDir,
		"--netns", filepath.Join("/run/netns", state.netnsName),
		"--",
		"--api-sock", "/firecracker.socket",
	)
}

// waitForSocket polls for the Firecracker API Unix socket created by jailer.
func (r *Runtime) waitForSocket(ctx context.Context, socketPath string) error {
	ticker := time.NewTicker(r.config.SocketWaitInterval)
	defer ticker.Stop()

	for attempt := 0; attempt < r.config.SocketWaitAttempts; attempt++ {
		if r.deps.pathExists(socketPath) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	return fmt.Errorf("timed out waiting for %s", socketPath)
}

// cleanupHost removes the bind mounts, network namespace, and jail directory
// created for a sandbox. It is intentionally best-effort at the shell level.
func (r *Runtime) cleanupHost(ctx context.Context, state *sandboxState) error {
	jailDir := filepath.Join(r.config.JailerBaseDir, "firecracker", state.vmID)
	rootfsTarget := filepath.Join(jailDir, "root", strings.TrimPrefix(r.config.SnapshotVirtioBlockPath, "/"))
	script := fmt.Sprintf(`
set -eu
umount -l %[1]s/root/snapshot_mem 2>/dev/null || true
umount -l %[1]s/root/snapshot_state 2>/dev/null || true
umount -l %[3]s 2>/dev/null || true
%[4]s
rm -rf %[1]s
`, shellquote.Quote(jailDir), shellquote.Quote(state.netnsName), shellquote.Quote(rootfsTarget),
		strings.TrimSpace(fcnetwork.CleanupScript(state.netnsName, fcnetwork.HostVethName(state.slot))))
	return r.deps.run(ctx, "sudo", "/bin/sh", "-c", script)
}

// runCommand executes a host command and includes combined output in failures
// so setup problems are visible in runner logs.
func runCommand(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s failed: %w: %s", commandString(name, args), err, strings.TrimSpace(string(output)))
	}
	return nil
}

// startCommand starts a long-running host process without waiting for it.
func startCommand(ctx context.Context, name string, args ...string) (process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.Command(name, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s failed: %w", commandString(name, args), err)
	}
	go func() {
		_ = cmd.Wait()
	}()
	return &processGroup{process: cmd.Process}, nil
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func commandString(name string, args []string) string {
	return strings.TrimSpace(name + " " + strings.Join(args, " "))
}

func maxInt32(n int32, min int) int {
	if n < int32(min) {
		return min
	}
	return int(n)
}

func shortID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])[:12]
}

func joinErrors(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%w", errs[0])
}

// probeDaemon waits until the runner's host-local daemon proxy can reach the
// guest daemon after snapshot restore.
func probeDaemon(ctx context.Context, baseURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := http.Client{Timeout: 2 * time.Second}
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/healthz", nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				return err
			}
			return fmt.Errorf("daemon did not become healthy before timeout")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
