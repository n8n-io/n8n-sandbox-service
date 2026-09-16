// Package docker implements the sandbox runtime on Docker containers. Behaviour
// overview in README.md of this directory.
package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/runner/config"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
	"github.com/n8n-io/sandbox-service/internal/runner/runtime/docker/netrules"
	"golang.org/x/sync/singleflight"
)

// Aliases of the runnerruntime sentinel errors, kept for this package's callers.
var (
	ErrSandboxNotFound           = runnerruntime.ErrSandboxNotFound
	ErrSandboxNetworkUnavailable = runnerruntime.ErrSandboxNetworkUnavailable
	ErrSandboxNotRunning         = runnerruntime.ErrSandboxNotRunning
)

const (
	containerStatusRunning    = "running"
	containerStatusCreated    = "created"
	containerStatusExited     = "exited"
	containerStatusPaused     = "paused"
	containerStatusRestarting = "restarting"
	containerStatusRemoving   = "removing"
	containerStatusDead       = "dead"
	daemonPort                = 8081

	// cleanupBudget is the deadline for removing a half-built container after a
	// failed create. The create context cannot be used: it may be what failed.
	cleanupBudget = 2 * time.Minute
)

// CreateOptions holds optional parameters for sandbox creation.
type CreateOptions = runnerruntime.CreateOptions

// ContainerInfo represents information about a created container.
type ContainerInfo = runnerruntime.SandboxInfo

// Runtime orchestrates container lifecycle without persistent state.
type Runtime struct {
	runnerConfig *config.Config
	config       Config
	// The public bridge, runnerBridgeNetwork, and the no-egress bridge,
	// runnerNoEgressNetwork. A sandbox is attached to one of them at creation
	// according to its egress mode and stays there for life.
	gatewayIP           string
	bridgeIface         string
	noEgressGatewayIP   string
	noEgressBridgeIface string
	wakeGroup           singleflight.Group
	docker              dockerBackend
	applyPolicy         func(bridgeIface, containerID, sourceIP, gatewayIP string, daemonPort int, blockEgress bool) error
	teardownRules       func(containerID string) error
	waitForDaemon       func(ctx context.Context, baseURL string) error
	imageReady          atomic.Bool
	imageReadyCh        chan struct{}
	metrics             *metrics.RunnerRecorder
	watchBackoff        time.Duration

	// mu guards what the event watcher and request-serving goroutines share: which
	// containers the runner is stopping on purpose, and which sandboxes Docker
	// restarted under it. Everything else about a sandbox is read back from Docker.
	mu            sync.Mutex
	expectedStops map[string]expectedStop
	stopToken     uint64
	restarted     map[string]struct{}
}

var _ runnerruntime.Runtime = (*Runtime)(nil)

// New creates a new Docker runtime. It reconciles any previous containers and ensures
// the runner bridge exists.
func New(runnerConfig *config.Config, cfg Config) (*Runtime, error) {
	m := newRuntime(runnerConfig, cfg, &dockerClient{host: cfg.Host})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := m.reconcileContainers(ctx); err != nil {
		return nil, fmt.Errorf("reconcile managed containers: %w", err)
	}

	bridge, err := m.ensureRunnerBridge(ctx, runnerBridgeNetwork, runnerBridgeNetwork, false)
	if err != nil {
		return nil, fmt.Errorf("ensure runner bridge: %w", err)
	}
	m.gatewayIP = firstGateway(bridge)
	m.bridgeIface = bridgeInterface(bridge)
	if m.bridgeIface == "" {
		return nil, fmt.Errorf("cannot determine host interface for network %s", runnerBridgeNetwork)
	}
	noEgress, err := m.ensureRunnerBridge(ctx, runnerNoEgressNetwork, runnerNoEgressBridge, true)
	if err != nil {
		return nil, fmt.Errorf("ensure no-egress bridge: %w", err)
	}
	m.noEgressGatewayIP = firstGateway(noEgress)
	m.noEgressBridgeIface = bridgeInterface(noEgress)
	if m.noEgressBridgeIface == "" {
		return nil, fmt.Errorf("cannot determine host interface for network %s", runnerNoEgressNetwork)
	}

	// Here, and not on the first sandbox: building the shared chains starts by
	// flushing whatever an earlier runner process left in them, which must
	// happen while the bridges have no container on them. reconcileContainers
	// above has just removed them all, and none can be created before New returns.
	if err := netrules.EnsureBridgePolicy(m.bridgeIface, false); err != nil {
		return nil, fmt.Errorf("ensure bridge policy: %w", err)
	}
	if err := netrules.EnsureBridgePolicy(m.noEgressBridgeIface, true); err != nil {
		return nil, fmt.Errorf("ensure no-egress bridge policy: %w", err)
	}

	return m, nil
}

// bridgeFor returns the host interface, gateway and policy of the runner network
// a sandbox is on. An unknown network resolves to the public bridge; nothing
// managed can be on neither, since reconcile removes every container the runner
// did not create in this configuration.
func (m *Runtime) bridgeFor(network string) (bridgeIface, gatewayIP string, blockEgress bool) {
	if network == runnerNoEgressNetwork {
		return m.noEgressBridgeIface, m.noEgressGatewayIP, true
	}
	return m.bridgeIface, m.gatewayIP, false
}

// newRuntime centralizes Runtime dependency wiring so tests can override Docker,
// network policy, or daemon readiness behavior without nil defaults.
func newRuntime(runnerConfig *config.Config, cfg Config, docker dockerBackend) *Runtime {
	return &Runtime{
		runnerConfig:  runnerConfig,
		config:        cfg,
		docker:        docker,
		applyPolicy:   netrules.ApplyPolicy,
		teardownRules: netrules.Teardown,
		waitForDaemon: waitForDaemon,
		imageReadyCh:  make(chan struct{}),
		watchBackoff:  2 * time.Second,
		expectedStops: make(map[string]expectedStop),
		restarted:     make(map[string]struct{}),
	}
}

// SetMetricsRecorder attaches the runner metrics recorder. A nil recorder is safe;
// every observation on it is a no-op.
func (m *Runtime) SetMetricsRecorder(rec *metrics.RunnerRecorder) {
	m.metrics = rec
}

// EnsureSandboxImage pulls the sandbox image with retries. It is intended to
// run in a goroutine after the HTTP server is listening.
func (m *Runtime) EnsureSandboxImage(ctx context.Context) {
	image := m.config.SandboxImage

	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			slog.Error("image pull canceled", "image", image, "error", err)
			return
		}

		if err := m.docker.pullImage(ctx, image); err != nil {
			backoff := min(time.Duration(attempt)*5*time.Second, 60*time.Second)
			slog.Warn("sandbox image pull failed, retrying",
				"image", image, "attempt", attempt, "retry_in", backoff, "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
				continue
			}
		}

		m.imageReady.Store(true)
		close(m.imageReadyCh)
		slog.Info("sandbox image ready", "image", image)
		return
	}
}

// ImageReady reports whether the sandbox image has been pulled successfully.
func (m *Runtime) ImageReady() bool {
	return m.imageReady.Load()
}

// ImageReadyCh returns a channel that is closed when the sandbox image becomes available.
func (m *Runtime) ImageReadyCh() <-chan struct{} {
	return m.imageReadyCh
}

// Prepare readies the Docker sandbox image for future sandbox creation and starts
// watching for guest deaths. The watcher starts before the image is pulled, not
// after: the pull can take minutes and retries forever, and a runner that served a
// sandbox while not watching would restart it silently.
func (m *Runtime) Prepare(ctx context.Context) {
	go m.watchGuestDeaths(ctx)
	m.EnsureSandboxImage(ctx)
}

// Ready reports whether the Docker-backed runtime can accept sandbox work.
func (m *Runtime) Ready(ctx context.Context) error {
	if !m.ImageReady() {
		return fmt.Errorf("sandbox image not ready")
	}
	if err := m.DockerHealthy(ctx); err != nil {
		return fmt.Errorf("docker unavailable")
	}
	return nil
}

// ReadyCh returns a channel that is closed when the runtime first becomes ready.
func (m *Runtime) ReadyCh() <-chan struct{} {
	return m.ImageReadyCh()
}

// Capacity reports how many managed containers exist on this runner. Stopped
// containers are counted: on Docker they keep their slot until deleted.
func (m *Runtime) Capacity(ctx context.Context) (runnerruntime.Capacity, error) {
	n, err := m.ManagedContainerCount(ctx)
	if err != nil {
		return runnerruntime.Capacity{Total: m.runnerConfig.CapacityTotal}, err
	}
	return runnerruntime.Capacity{Used: int32(n), Total: m.runnerConfig.CapacityTotal}, nil
}

// CreateSandbox creates and starts a new sandbox.
func (m *Runtime) CreateSandbox(ctx context.Context, sandboxID string, opts *runnerruntime.CreateOptions) (*runnerruntime.SandboxInfo, error) {
	return m.CreateContainer(ctx, sandboxID, opts)
}

// GetSandboxInfo returns information about a sandbox by its sandbox ID.
func (m *Runtime) GetSandboxInfo(ctx context.Context, sandboxID string) (*runnerruntime.SandboxInfo, error) {
	containerID, err := m.FindContainerIDByLabel(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	return m.GetContainerInfo(ctx, containerID)
}

// DeleteSandbox removes a sandbox by its sandbox ID.
func (m *Runtime) DeleteSandbox(ctx context.Context, sandboxID string) error {
	containerID, err := m.FindContainerIDByLabel(ctx, sandboxID)
	if err != nil {
		return err
	}
	if err := m.DeleteContainer(ctx, containerID); err != nil {
		return err
	}
	m.clearRestarted(sandboxID)
	return nil
}

// StopSandbox stops a sandbox without removing it.
func (m *Runtime) StopSandbox(ctx context.Context, sandboxID string) error {
	return m.StopSandboxContainer(ctx, sandboxID)
}

// CreateContainer creates and starts a new container.
func (m *Runtime) CreateContainer(ctx context.Context, sandboxID string, opts *CreateOptions) (*ContainerInfo, error) {
	if !m.imageReady.Load() {
		return nil, fmt.Errorf("sandbox image not yet available")
	}

	if opts == nil {
		opts = &CreateOptions{}
	}

	// Validate sandboxID length before slicing to prevent panic
	if len(sandboxID) < 12 {
		return nil, fmt.Errorf("sandbox ID must be at least 12 characters, got %d", len(sandboxID))
	}

	containerName := "sandbox-" + sandboxID[:12]
	limits := m.defaultLimits()
	network := runnerBridgeNetwork
	if opts.BlockEgress() {
		network = runnerNoEgressNetwork
	}
	bridgeIface, gatewayIP, blockEgress := m.bridgeFor(network)

	containerID, err := m.docker.createContainer(ctx, sandboxID, containerName, m.config.SandboxImage, network, limits, m.config.EnableCgroups)
	if err != nil {
		return nil, fmt.Errorf("create container: %w", err)
	}

	cleanupOnError := func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupBudget)
		defer cancel()
		_ = m.removeContainerAndTeardownRules(cleanupCtx, containerID)
	}

	if err := m.docker.startContainer(ctx, containerID); err != nil {
		cleanupOnError()
		return nil, fmt.Errorf("start container: %w", err)
	}

	containerIP, err := m.docker.containerIP(ctx, containerID)
	if err != nil {
		cleanupOnError()
		return nil, fmt.Errorf("inspect container ip: %w", err)
	}

	if err := m.applyPolicy(bridgeIface, containerID, containerIP, gatewayIP, daemonPort, blockEgress); err != nil {
		cleanupOnError()
		return nil, fmt.Errorf("apply network rules: %w", err)
	}

	baseURL := fmt.Sprintf("http://%s:%d", containerIP, daemonPort)
	if err := m.waitForDaemon(ctx, baseURL); err != nil {
		cleanupOnError()
		return nil, fmt.Errorf("connect to daemon: %w", err)
	}

	containerInfo := &ContainerInfo{
		ID:   containerID,
		Name: containerName,
		IP:   containerIP,
	}

	slog.Info("container created", "sandbox_id", sandboxID, "container_id", containerID, "ip", containerIP, "network", network)
	return containerInfo, nil
}

// DockerHealthy checks whether the inner Docker daemon is reachable.
func (m *Runtime) DockerHealthy(ctx context.Context) error {
	return m.docker.ping(ctx)
}

// GetContainerInfo returns information about a container by its ID.
func (m *Runtime) GetContainerInfo(ctx context.Context, containerID string) (*ContainerInfo, error) {
	inspect, err := m.docker.inspectContainer(ctx, containerID)
	if err != nil {
		if isDockerNotFound(err) {
			return nil, ErrSandboxNotFound
		}
		return nil, err
	}

	_, ip := sandboxAttachment(inspect)
	if ip == "" {
		return nil, fmt.Errorf("%w: container %s has no IP on a runner network", ErrSandboxNetworkUnavailable, containerID)
	}

	return &ContainerInfo{
		ID:   inspect.ID,
		Name: inspect.Name,
		IP:   ip,
	}, nil
}

// EnsureSandboxRunning starts a stopped container if needed, reapplies network
// policy for its current IP, and waits until the daemon accepts traffic.
//
// It also re-admits a container Docker restarted on its own after a crash, which is
// the only path that can: `--restart unless-stopped` brings the container back
// without asking the runner, on a new IP the sandbox's network rules do not know.
func (m *Runtime) EnsureSandboxRunning(ctx context.Context, sandboxID string) (runnerruntime.WakeResult, error) {
	if err := ctx.Err(); err != nil {
		return runnerruntime.WakeResult{}, err
	}
	recovered, err, _ := m.wakeGroup.Do(sandboxID, func() (interface{}, error) {
		// singleflight runs this once for all waiters; do not use the caller's ctx
		// here or one canceled/short-lived request fails everyone else waiting on
		// the same key.
		wakeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		recovering, err := m.ensureSandboxRunningOnce(wakeCtx, sandboxID)
		// Inside the singleflight, so a burst of requests stranded by one crash counts
		// as the one recovery it is.
		if recovering {
			m.metrics.ObserveRecovery(err == nil)
		}
		return recovering, err
	})
	// Comma-ok: a wake that failed before it knew whether it was a recovery returns
	// no value at all.
	wasRecovery, _ := recovered.(bool)
	return runnerruntime.WakeResult{Recovered: wasRecovery}, err
}

// ensureSandboxRunningOnce reports whether the sandbox came back from a crash rather
// than from an idle stop. Docker has already restarted it in that case, so what is
// left to do is what Docker does not: point the sandbox's network rules at the IP it
// came back on, and wait for a daemon that has lost every process it was running.
func (m *Runtime) ensureSandboxRunningOnce(ctx context.Context, sandboxID string) (bool, error) {
	containerID, err := m.FindContainerIDByLabel(ctx, sandboxID)
	if err != nil {
		return false, err
	}
	inspect, err := m.docker.inspectContainer(ctx, containerID)
	if err != nil {
		if isDockerNotFound(err) {
			return false, ErrSandboxNotFound
		}
		return false, err
	}

	// From here on failures return `recovering`: a recovery whose repair failed is
	// still a recovery attempt, and the mark stays so the next request retries it.
	recovering := m.wasRestarted(sandboxID)
	if !recovering && isContainerReady(inspect.State) {
		return false, nil
	}
	if recovering {
		// The container is usually already running, and never startable while Docker
		// is between restarts, so a wake that lands in that window waits for it. This
		// is the request that has to come back with the restart, so failing it here
		// would report a crash as a plain error.
		if inspect, err = m.awaitContainerRestart(ctx, containerID); err != nil {
			return recovering, err
		}
	}
	if !isContainerReady(inspect.State) {
		if !canStartContainer(inspect.State) {
			return recovering, fmt.Errorf("sandbox container is not startable from docker state %q", inspect.State.Status)
		}
		// A mark recorded for this container is deliberately left alone. It belongs to
		// a stop whose die event may still be in the events pipe, and dropping it here
		// would leave that death to be read as a crash; the stop that can never produce
		// a death records nothing in the first place, so there is nothing to clean up.
		if err := m.docker.startContainer(ctx, containerID); err != nil {
			return recovering, fmt.Errorf("start container: %w", err)
		}
	}
	containerIP, err := m.docker.containerIP(ctx, containerID)
	if err != nil {
		m.cleanupWakeFailure(containerID)
		return recovering, err
	}
	// The network is fixed at creation and stays on the container while it is
	// stopped, so the inspect from before the start names it even though the
	// address there is only assigned by the start.
	network, _ := sandboxAttachment(inspect)
	bridgeIface, gatewayIP, blockEgress := m.bridgeFor(network)
	if err := m.applyPolicy(bridgeIface, containerID, containerIP, gatewayIP, daemonPort, blockEgress); err != nil {
		m.cleanupWakeFailure(containerID)
		return recovering, fmt.Errorf("apply network rules: %w", err)
	}
	baseURL := fmt.Sprintf("http://%s:%d", containerIP, daemonPort)
	if err := m.waitForDaemon(ctx, baseURL); err != nil {
		m.cleanupWakeFailure(containerID)
		return recovering, err
	}
	if recovering {
		// Cleared only now: until the rules match the container's IP, nothing may be
		// proxied to it, and DaemonURL keeps reporting it not running while the mark is
		// set. Clearing it is also what spends the one restart report a client gets.
		m.clearRestarted(sandboxID)
		slog.Info("docker sandbox recovered", "sandbox_id", sandboxID, "container_id", containerID, "ip", containerIP)
	}
	return recovering, nil
}

// awaitContainerRestart waits out the gap between a container dying and Docker's
// restart policy bringing it back, where it is neither ready nor startable.
func (m *Runtime) awaitContainerRestart(ctx context.Context, containerID string) (*containerInspect, error) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(60 * time.Second)

	for {
		inspect, err := m.docker.inspectContainer(ctx, containerID)
		if err != nil {
			if isDockerNotFound(err) {
				return nil, ErrSandboxNotFound
			}
			return nil, err
		}
		// Exited is a settled state: the restart policy gave up, or the container was
		// stopped since. Either way the caller starts it rather than waiting.
		if !inspect.State.Restarting {
			return inspect, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline:
			return nil, fmt.Errorf("timeout waiting for container %s to finish restarting", containerID)
		case <-ticker.C:
		}
	}
}

func (m *Runtime) cleanupWakeFailure(containerID string) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	token := m.expectStop(containerID)
	if err := m.docker.stopContainer(cleanupCtx, containerID); err != nil {
		m.forgetExpectedStop(containerID, token)
		slog.Warn("stop container after wake failure", "container_id", containerID, "err", err)
		return
	}
	// Rules only come down after the container is stopped; otherwise a still-running
	// sandbox could continue without network policy.
	if err := m.teardownRules(containerID); err != nil {
		slog.Warn("teardown network rules after wake failure", "container_id", containerID, "err", err)
	}
}

// StopSandboxContainer stops a running sandbox container without removing it.
func (m *Runtime) StopSandboxContainer(ctx context.Context, sandboxID string) error {
	containerID, err := m.FindContainerIDByLabel(ctx, sandboxID)
	if err != nil {
		return err
	}
	inspect, err := m.docker.inspectContainer(ctx, containerID)
	if err != nil {
		if isDockerNotFound(err) {
			return ErrSandboxNotFound
		}
		return err
	}
	if !inspect.State.Running {
		return nil
	}
	// A container Docker is between restarts of is reported running, so this stop is
	// not a no-op — it cancels the pending restart. What it cannot do is produce a
	// death: the process that would have been restarted died at crash time and
	// emitted the container's one die event already. Recording a mark for a death
	// that never arrives is what would leave it to excuse the next real crash of this
	// container, once a wake has started it again.
	if inspect.State.Restarting {
		return m.docker.stopContainer(ctx, containerID)
	}
	token := m.expectStop(containerID)
	if err := m.docker.stopContainer(ctx, containerID); err != nil {
		m.forgetExpectedStop(containerID, token)
		return err
	}
	return nil
}

// DaemonURL returns the daemon URL for a container by sandbox ID.
func (m *Runtime) DaemonURL(ctx context.Context, sandboxID string) (string, error) {
	// A container Docker restarted after a crash looks perfectly healthy here, which
	// is the problem: its network rules still point at the IP it had, and the request
	// that reaches it would find a sandbox that quietly lost everything it was
	// running. Reporting it not running routes the caller through the wake path, which
	// repairs the rules and reports the restart.
	if m.wasRestarted(sandboxID) {
		return "", ErrSandboxNotRunning
	}

	containerID, err := m.FindContainerIDByLabel(ctx, sandboxID)
	if err != nil {
		return "", err
	}

	inspect, err := m.docker.inspectContainer(ctx, containerID)
	if err != nil {
		if isDockerNotFound(err) {
			return "", ErrSandboxNotFound
		}
		return "", err
	}
	if !isContainerReady(inspect.State) {
		return "", ErrSandboxNotRunning
	}

	_, ip := sandboxAttachment(inspect)
	if ip == "" {
		return "", fmt.Errorf("%w: container %s has no IP on a runner network", ErrSandboxNetworkUnavailable, containerID)
	}

	baseURL := fmt.Sprintf("http://%s:%d", ip, daemonPort)
	return baseURL, nil
}

// DeleteContainer stops and removes a container.
func (m *Runtime) DeleteContainer(ctx context.Context, containerID string) error {
	if err := m.removeContainerAndTeardownRules(ctx, containerID); err != nil {
		return err
	}

	slog.Info("container deleted", "container_id", containerID)
	return nil
}

// removeContainerAndTeardownRules removes the container, then tears down its network
// rules. Order matters: rules must outlive the container so it cannot run
// unconfined during teardown. Both failure paths are logged; the
// removeContainer error is also returned so callers decide whether to bail.
func (m *Runtime) removeContainerAndTeardownRules(ctx context.Context, containerID string) error {
	if containerID == "" {
		return nil
	}
	// Removing a running container kills it, and that death is the runner's doing.
	token := m.expectStop(containerID)
	if err := m.docker.removeContainer(ctx, containerID); err != nil {
		m.forgetExpectedStop(containerID, token)
		slog.Warn("remove sandbox container", "container_id", containerID, "err", err)
		return err
	}
	if err := m.teardownRules(containerID); err != nil {
		// TODO: consider adding metrics to track this in the future.
		slog.Warn("teardown network rules", "container_id", containerID, "err", err)
	}
	return nil
}

// Shutdown cleans up all managed containers.
func (m *Runtime) Shutdown(ctx context.Context) {
	if err := m.reconcileContainers(ctx); err != nil {
		slog.Warn("shutdown container cleanup", "err", err)
	}
}

func (m *Runtime) defaultLimits() *ResourceLimits {
	var diskMB int64
	if m.config.DiskQuotaActive {
		diskMB = m.config.DefaultDiskQuotaMB
	}
	return &ResourceLimits{
		MemoryMB:   m.config.DefaultMemoryMB,
		CPUPercent: m.config.DefaultCPUPercent,
		PidsMax:    m.config.DefaultPidsMax,
		DiskMB:     diskMB,
	}
}

func isContainerReady(state containerState) bool {
	return state.Status == containerStatusRunning &&
		state.Running &&
		!state.Paused &&
		!state.Restarting &&
		!state.Dead
}

func canStartContainer(state containerState) bool {
	if state.Running || state.Paused || state.Restarting || state.Dead {
		return false
	}
	switch state.Status {
	case containerStatusCreated, containerStatusExited:
		return true
	default:
		return false
	}
}

func waitForDaemon(ctx context.Context, baseURL string) error {
	httpClient := &http.Client{Timeout: 3 * time.Second}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(60 * time.Second)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("timeout waiting for daemon health at %s/healthz", baseURL)
		case <-ticker.C:
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/healthz", nil)
			if err != nil {
				return err
			}
			resp, err := httpClient.Do(req)
			if err != nil {
				continue
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				continue
			}

			// /healthz can become ready slightly before command execution is fully
			// usable under load; require a tiny exec round-trip before returning.
			execReq, err := http.NewRequestWithContext(
				ctx,
				http.MethodPost,
				baseURL+"/executions",
				bytes.NewBufferString(`{"command":"true","timeout_ms":2000}`),
			)
			if err != nil {
				return err
			}
			execReq.Header.Set("Content-Type", "application/json")
			execResp, err := httpClient.Do(execReq)
			if err != nil {
				continue
			}
			body, _ := io.ReadAll(execResp.Body)
			execResp.Body.Close()
			if execResp.StatusCode != http.StatusOK {
				continue
			}
			// Daemon /executions streams NDJSON events; require a successful exit event.
			if isSuccessfulExit(body) {
				return nil
			}
		}

	}
}

func isSuccessfulExit(body []byte) bool {
	return bytes.Contains(body, []byte(`"type":"exit"`)) && bytes.Contains(body, []byte(`"exit_code":0`))
}

func (m *Runtime) reconcileContainers(ctx context.Context) error {
	ids, err := m.docker.findContainerByLabels(ctx, managedLabelFilter)
	if err != nil {
		return err
	}
	// Best effort: startup should continue even if one stale managed
	// container can't be removed immediately. removeContainerAndTeardownRules logs.
	for _, id := range ids {
		_ = m.removeContainerAndTeardownRules(ctx, id)
	}
	return nil
}

func (m *Runtime) ensureRunnerBridge(ctx context.Context, name, bridgeName string, internal bool) (*networkInspect, error) {
	inspect, err := m.docker.inspectNetwork(ctx, name)
	if err != nil {
		if !isDockerNotFound(err) {
			return nil, err
		}
		return m.createRunnerBridge(ctx, name, bridgeName, internal)
	}

	return inspect, nil
}

// createRunnerBridge creates a runner network whose bridge device is bridgeName.
// internal makes it a Docker internal network: no default route in its
// containers, no NAT, and an embedded DNS that does not forward upstream. Every
// runner network disables inter-container communication.
func (m *Runtime) createRunnerBridge(ctx context.Context, name, bridgeName string, internal bool) (*networkInspect, error) {
	args := []string{"network", "create", "--driver", "bridge"}
	if internal {
		args = append(args, "--internal")
	}
	args = append(args,
		"--opt", "com.docker.network.bridge.enable_icc=false",
		"--opt", bridgeNameOption+"="+bridgeName,
		name)
	if _, err := m.docker.run(ctx, args...); err != nil {
		return nil, err
	}
	return m.docker.inspectNetwork(ctx, name)
}

// ManagedContainerCount returns how many sandbox containers this runner is managing.
func (m *Runtime) ManagedContainerCount(ctx context.Context) (int, error) {
	ids, err := m.docker.findContainerByLabels(ctx, managedLabelFilter)
	if err != nil {
		return 0, err
	}
	return len(ids), nil
}

// FindContainerIDByLabel finds a container ID by sandbox ID using label filters.
func (m *Runtime) FindContainerIDByLabel(ctx context.Context, sandboxID string) (string, error) {
	lines, err := m.docker.findContainerByLabels(ctx,
		managedLabelFilter,
		"label="+containerLabelSandboxID+"="+sandboxID)
	if err != nil {
		return "", err
	}
	if len(lines) == 0 {
		return "", ErrSandboxNotFound
	}
	return lines[0], nil
}
