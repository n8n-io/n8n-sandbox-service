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
	"sync/atomic"
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
	readyOnce   sync.Once
	admissionOK atomic.Bool
	hostNATMu   sync.Mutex
	hostNATOK   bool
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
	ctx, cancel := reconcileContext()
	defer cancel()
	rt.reconcileOnStartup(ctx)
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

// sandboxTransition is the lifecycle operation currently in flight for a
// sandbox. Stop, wake, and delete must be mutually exclusive: wake reassigns
// slot, netns, socket, and daemon URL as it goes, so an overlapping delete could
// read half of the old identity, skip teardown because the new process and proxy
// are not published yet, and free a slot whose microVM is still coming up.
//
// transitionDeleting is terminal: it is never released back to transitionNone
// except when a failed delete leaves the sandbox tracked, so it doubles as the
// tombstone that hides the sandbox from lookups while teardown runs.
type sandboxTransition uint8

const (
	transitionNone sandboxTransition = iota
	transitionCreating
	transitionStopping
	transitionWaking
	transitionDeleting
)

// Lifecycle budgets bound how long an operation may hold a sandbox's transition
// claim, including the wait to acquire it. Nothing else caps them: the host
// commands and Firecracker API calls these operations run inherit their context,
// and the callers that drive them have no deadline of their own (the control-plane
// RPCs carry the API request context, the idle sweeper's context lives until the
// API exits), so a wedged host command would otherwise hold the claim forever and
// fail every later operation on that sandbox.
//
// beginTransition applies transitionBudget to stop, wake, and delete. Create has
// its own, larger budget because it clones the rootfs and snapshot before booting,
// and it claims the sandbox in reserveSandbox rather than through beginTransition.
// Vars rather than consts so tests can shrink them.
var (
	createBudget     = 3 * time.Minute
	transitionBudget = 2 * time.Minute
)

// transitionWaitBudget bounds how long an operation waits for another one to
// release the sandbox. It is separate from, and longer than, the budget the winner
// then runs under, for two reasons: waiting must not eat the time the operation
// needs for its own host work, and a waiter must not give up on a claim that is
// still guaranteed to be released. The longest possible hold is a create that
// exhausts createBudget and then runs its cleanup, hence the sum.
func transitionWaitBudget() time.Duration {
	return createBudget + transitionBudget
}

// withLifecycleBudget detaches a lifecycle operation from its caller's
// cancellation and caps how long it may run. Detaching is what stops a client
// disconnect from abandoning a sandbox with its host resources half torn down,
// and the cap is what eventually frees the claim, since exec.CommandContext kills
// a stuck host command once the context expires. Context values survive, so the
// trace id follows the operation.
//
// The budget is a ceiling, not a replacement: a caller that asked for less time
// keeps its own deadline (the admission canary bounds its cleanup deliberately so
// a stuck umount cannot stall startup). Strip the deadline with
// context.WithoutCancel before calling to opt out of that.
func withLifecycleBudget(ctx context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < budget {
			budget = remaining
		}
	}
	return context.WithTimeout(context.WithoutCancel(ctx), budget)
}

// withCleanupBudget derives a context for teardown that has to run even when the
// operation's own budget is what expired. Dropping the caller's deadline is the
// point: reusing an exhausted context would skip the host commands and leave the
// sandbox's jail mounts, netns, and data dir behind while its slot goes back into
// circulation.
func withCleanupBudget(ctx context.Context) (context.Context, context.CancelFunc) {
	return withLifecycleBudget(context.WithoutCancel(ctx), transitionBudget)
}

// sandboxState holds the host resources backing one live microVM sandbox.
type sandboxState struct {
	id                string
	vmID              string
	slot              int
	info              *runnerruntime.SandboxInfo
	netnsName         string
	hostVeth          string
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
	transition        sandboxTransition
	stoppedAt         time.Time

	// generation counts the microVM incarnations this sandbox has had. It is
	// bumped in teardownRunningVM, before the process is killed, and captured by
	// the exit callback of the process being started, so an exit the runner
	// caused is told apart from a guest that died on its own. One counter covers
	// stop, delete, wake rollback and shutdown, and it also stops a dying old
	// incarnation from marking a freshly started one dead.
	generation uint64

	// mustColdBoot pins a sandbox away from its snapshot. Restoring a memory image
	// whose cached filesystem metadata no longer matches the disk corrupts it
	// silently, with nothing to detect the mismatch, and the guest has been writing
	// to the rootfs ever since the restore that resumed it. So it is set once the
	// snapshot is restored and cleared only by the snapshot a stop takes of the
	// paused guest: anything else that ends the microVM in between — a crash, or a
	// wake that failed after the restore — leaves the pair mismatched.
	mustColdBoot bool
}

// deleting reports whether the sandbox has been claimed for teardown. r.mu must
// be held by the caller.
func (s *sandboxState) deleting() bool {
	return s.transition == transitionDeleting
}

// process is the minimum process handle needed for sandbox cleanup.
type process interface {
	Kill() error
}

// processGroup kills Firecracker and any children started in its process group.
type processGroup struct {
	process *os.Process
	// reaped is set once wait has collected the process, after which its pid is no
	// longer ours to signal.
	reaped atomic.Bool
}

func (p *processGroup) Kill() error {
	// The pid identifies this process group only until it is reaped; the kernel is
	// then free to hand it to something else, and signalling a whole group by a
	// recycled id could kill unrelated processes. Crash handling reaches here after
	// the exit it reacts to, having waited for the sandbox's claim, so this is the
	// ordinary case rather than a corner of one.
	if p.reaped.Load() {
		return os.ErrProcessDone
	}
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
	start               func(ctx context.Context, onExit func(error), name string, args ...string) (process, error)
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

// Prepare is implemented in admission.go: host NAT + pin + snapshot + canary.

// ensureHostNATReady runs host NAT setup once successfully. Failures are not
// cached so admission backoff (and later CreateSandbox) can retry transient errors.
func (r *Runtime) ensureHostNATReady(ctx context.Context) error {
	r.hostNATMu.Lock()
	defer r.hostNATMu.Unlock()
	if r.hostNATOK {
		return nil
	}
	if err := fcnetwork.EnsureHostNAT(ctx, r.deps.run); err != nil {
		slog.Error("firecracker host NAT setup failed", "error", err)
		return err
	}
	r.hostNATOK = true
	return nil
}

// Ready checks that pinned guest assets exist and the admission canary has passed.
func (r *Runtime) Ready(context.Context) error {
	requiredPaths := map[string]string{
		"jailer":           r.config.JailerBin,
		"firecracker":      r.config.FirecrackerBin,
		"template rootfs":  filepath.Join(r.config.TemplateDir, "rootfs.ext4"),
		"template vmlinux": filepath.Join(r.config.TemplateDir, "vmlinux"),
		"snapshot memory":  r.config.SnapshotMemPath,
		"snapshot state":   r.config.SnapshotStatePath,
	}
	for label, path := range requiredPaths {
		if !r.deps.pathExists(path) {
			return fmt.Errorf("firecracker %s path does not exist: %s", label, path)
		}
	}
	if len(r.slots) == 0 {
		return fmt.Errorf("firecracker runtime has no capacity")
	}
	if !r.admissionOK.Load() {
		return fmt.Errorf("firecracker admission canary has not passed")
	}
	return nil
}

// ReadyCh is closed after Prepare completes admission successfully.
func (r *Runtime) ReadyCh() <-chan struct{} {
	return r.readyCh
}

// Capacity reports slot-blocking sandboxes (Used) and stopped snapshots (Stopped).
func (r *Runtime) Capacity(context.Context) (runnerruntime.Capacity, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	stopped := 0
	for _, state := range r.sandboxes {
		if !state.deleting() && state.stopped {
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

	ctx, cancel := withLifecycleBudget(ctx, createBudget)
	defer cancel()

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
		cleanupCtx, cancelCleanup := withCleanupBudget(ctx)
		defer cancelCleanup()
		if err := r.deleteSandbox(cleanupCtx, state); err != nil {
			// A delete that fails normally keeps its slot because a retry arrives to
			// reclaim it, but no retry can arrive here: this create is about to return
			// an error, so the API stores no record for the sandbox and neither an
			// explicit delete nor the idle sweeper can name it again. Holding the slot
			// would strand runner capacity until the process restarts.
			//
			// So it goes back on the same terms a failed stop hands its slot back. What
			// makes that safe is that the next sandbox here clears the slot before
			// building it: setupNetwork deletes both per-slot host names, netns and
			// veth, whatever state they were left in. The jail directory is keyed to
			// this vmID, so it collides with nothing and startup reconcile sweeps it.
			// The proxy port is per-slot and freed by the Stop that teardown performs on
			// every handle it claims; if that ever left the port bound, the next create
			// on this slot fails loudly on bind rather than sharing it.
			slog.Warn("firecracker create cleanup failed, releasing slot anyway",
				"sandbox_id", sandboxID, "vm_id", state.vmID, "slot", state.slot, "err", err)
			r.untrackSandbox(state)
		}
	}

	timer := newStepTimer(metrics.OpCreate, r.metrics)
	templateRootfs := filepath.Join(r.config.TemplateDir, "rootfs.ext4")
	slog.Debug("firecracker cloning rootfs", "sandbox_id", sandboxID, "template", templateRootfs, "dest", state.rootfsPath)
	if err := timer.step(stepCloneRootfs, func() error {
		return r.deps.cloneRootfs(ctx, templateRootfs, state.rootfsPath)
	}); err != nil {
		cleanupOnError()
		return nil, fmt.Errorf("clone sandbox rootfs: %w", err)
	}
	slog.Debug("firecracker cloning golden snapshot", "sandbox_id", sandboxID, "data_dir", state.dataDir)
	if err := timer.step(stepCloneSnapshot, func() error {
		return r.deps.cloneGoldenSnapshot(ctx, r.config.SnapshotMemPath, r.config.SnapshotStatePath, state.dataDir)
	}); err != nil {
		cleanupOnError()
		return nil, fmt.Errorf("clone sandbox snapshot assets: %w", err)
	}

	if err := r.activateSandboxVM(ctx, state, timer); err != nil {
		cleanupOnError()
		return nil, err
	}

	r.mu.Lock()
	state.running = true
	state.transition = transitionNone
	info := *state.info
	r.mu.Unlock()
	slog.Info("firecracker sandbox created",
		append([]any{
			"sandbox_id", sandboxID,
			"vm_id", state.vmID,
			"slot", state.slot,
			"daemon_url", state.daemonURL,
		}, timer.attrsFor(ctx)...)...)
	return &info, nil
}

// GetSandboxInfo returns the runner-facing sandbox metadata tracked in memory.
func (r *Runtime) GetSandboxInfo(_ context.Context, sandboxID string) (*runnerruntime.SandboxInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	state, ok := r.sandboxes[sandboxID]
	if !ok || state.deleting() {
		return nil, runnerruntime.ErrSandboxNotFound
	}
	info := *state.info
	return &info, nil
}

// DeleteSandbox tears down the microVM (if running), proxy, netns, jail state, and data dir.
// It waits for any in-flight stop or wake to finish first, which is what
// guarantees teardown sees the microVM a concurrent wake was bringing up instead
// of leaving it orphaned on a slot the runner has already handed back.
func (r *Runtime) DeleteSandbox(ctx context.Context, sandboxID string) error {
	state, ctx, cancel, err := r.beginTransition(ctx, sandboxID, transitionDeleting)
	if err != nil {
		return err
	}
	defer cancel()
	return r.deleteSandbox(ctx, state)
}

// DaemonURL returns the host-local proxy URL, not the guest IP directly.
func (r *Runtime) DaemonURL(_ context.Context, sandboxID string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	state, ok := r.sandboxes[sandboxID]
	if !ok || state.deleting() {
		return "", runnerruntime.ErrSandboxNotFound
	}
	if state.transition != transitionNone || state.stopped || !state.running {
		// Reject proxies while a lifecycle transition is in flight, stopped, or not
		// yet running (create in progress).
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
		// A sandbox already claimed for delete is skipped so its teardown does not
		// run twice. Unlike DeleteSandbox this does not wait for an in-flight stop
		// or wake: the process is exiting, and startup reconcile removes whatever
		// leaks.
		if state.deleting() {
			continue
		}
		states = append(states, state)
		state.running = false
		state.transition = transitionDeleting
	}
	r.mu.Unlock()

	for _, state := range states {
		_ = r.deleteSandbox(ctx, state)
	}
}

// reserveSandbox assigns the sandbox to the first free slot and derives the
// deterministic per-slot host resources used for the VM. The new sandbox starts
// out claimed for creation so a delete arriving mid-create waits for the microVM
// to be published instead of tearing down around it.
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
		hostVeth:          fcnetwork.HostVethName(slot),
		socketPath:        socketPath,
		daemonURL:         daemonURL,
		dataDir:           dataDir,
		rootfsPath:        sandboxRootfsPath(r.runnerConfig.DataDir, sandboxID),
		snapshotMemPath:   sandboxSnapshotMemPath(dataDir),
		snapshotStatePath: sandboxSnapshotStatePath(dataDir),
		transition:        transitionCreating,
		info: &runnerruntime.SandboxInfo{
			ID:   sandboxID,
			Name: vmID,
			IP:   r.config.GuestIP,
		},
	}
	r.sandboxes[sandboxID] = state
	return state, nil
}

// deleteSandbox stops host-side resources for one sandbox before releasing its
// slot. The caller must already hold the sandbox's transition claim; the claim
// is upgraded to transitionDeleting here for the create-cleanup path, which
// deletes under its own creation claim.
func (r *Runtime) deleteSandbox(ctx context.Context, state *sandboxState) error {
	r.mu.Lock()
	if current, ok := r.sandboxes[state.id]; ok && current == state {
		state.running = false
		state.transition = transitionDeleting
	}
	// Read under the lock, once, and used for the rest of this call: on the Shutdown
	// path this delete may not hold the sandbox's claim, so another teardown can be
	// releasing the slot while it runs.
	slot := state.slot
	r.mu.Unlock()

	slog.Debug("firecracker sandbox cleanup started", "sandbox_id", state.id, "vm_id", state.vmID, "slot", slot)

	var errs []error
	// Run unconditionally, including for a sandbox whose handles are already gone. A
	// stop or crash whose host cleanup failed still marks the sandbox stopped and
	// drops those handles, so skipping cleanup here on the strength of them would
	// leave the jail's bind mounts in place and the data dir removed below out from
	// under them — the snapshot files stay allocated, unreclaimable, and startup
	// reconcile cannot free them either, because its rm -rf fails on a directory
	// holding an active mount. Repeating cleanup is safe: every step is guarded, and
	// what it removes is keyed to this vmID.
	if err := r.teardownRunningVM(ctx, state); err != nil {
		slog.Warn("firecracker host cleanup failed", "sandbox_id", state.id, "err", err)
		errs = append(errs, err)
	}
	if err := removeSandboxDataDir(ctx, state.dataDir); err != nil {
		slog.Warn("firecracker sandbox data cleanup failed", "sandbox_id", state.id, "data_dir", state.dataDir, "err", err)
		errs = append(errs, fmt.Errorf("remove sandbox data dir: %w", err))
	}
	slog.Debug("firecracker sandbox cleanup finished", "sandbox_id", state.id, "vm_id", state.vmID, "slot", slot)
	if err := joinErrors(errs); err != nil {
		r.mu.Lock()
		if current, ok := r.sandboxes[state.id]; ok && current == state {
			state.transition = transitionNone
		}
		r.mu.Unlock()
		return err
	}

	r.untrackSandbox(state)
	return nil
}

// untrackSandbox drops the sandbox from the runner and hands its slot back. It is
// a no-op once another state has taken over the id or the slot, so it cannot
// reclaim capacity a later sandbox is already using.
func (r *Runtime) untrackSandbox(state *sandboxState) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if current, ok := r.sandboxes[state.id]; !ok || current != state {
		return
	}
	delete(r.sandboxes, state.id)
	if state.slot >= 0 && r.slotOwnedByLocked(state.slot, state.id) {
		r.releaseSlotLocked(state.slot)
	}
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
// onExit fires once the microVM is gone: jailer execs Firecracker in place, so
// the process started here lives exactly as long as the guest does.
func (r *Runtime) startJailer(ctx context.Context, state *sandboxState, onExit func(error)) (process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.deps.start(ctx,
		onExit,
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
//
// Every name it needs is one the slot reservation wrote and no teardown touches,
// which is why hostVeth is carried on the state rather than derived from the slot
// here: two teardowns can run this concurrently — a guest death racing shutdown —
// and the one that arrives second would otherwise read a slot the first has already
// released and set to -1, naming a device that does not exist.
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
		strings.TrimSpace(fcnetwork.CleanupScript(state.netnsName, state.hostVeth)))
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

// startCommand starts a long-running host process without waiting for it, and
// reports its exit to onExit. The wait has to happen regardless, to reap the
// child; handing its error to the caller is what turns it into crash detection.
func startCommand(ctx context.Context, onExit func(error), name string, args ...string) (process, error) {
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
	group := &processGroup{process: cmd.Process}
	go func() {
		err := cmd.Wait()
		// Marked before the exit is reported, so the crash handling that follows
		// cannot signal a pid the kernel has already taken back.
		group.reaped.Store(true)
		if onExit != nil {
			onExit(err)
		}
	}()
	return group, nil
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
