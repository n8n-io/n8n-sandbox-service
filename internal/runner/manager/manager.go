package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/n8n-io/sandbox-service/internal/runner/config"
	"github.com/n8n-io/sandbox-service/internal/runner/netrules"
)

// ErrSandboxNotFound is returned when a sandbox ID is not found.
var ErrSandboxNotFound = errors.New("sandbox not found")

// ErrSandboxNetworkUnavailable is returned when a container exists but has no
// network attachment/IP yet.
var ErrSandboxNetworkUnavailable = errors.New("sandbox network unavailable")

// ErrSandboxNotRunning is returned when a sandbox container exists but is not running.
var ErrSandboxNotRunning = errors.New("sandbox not running")

const (
	StatusRunning            = "running"
	StatusTerminated         = "terminated"
	runnerBridgeNetwork      = "runner-bridge"
	daemonPort               = 8081
	containerLabelManaged    = "sandbox-service.managed"
	containerLabelManagedVal = "true"
	containerLabelSandboxID  = "sandbox-service.id"
)

// CreateOptions holds optional parameters for sandbox creation.
type CreateOptions struct{}

// ContainerInfo represents information about a created container.
type ContainerInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	IP   string `json:"ip"`
}

// Manager orchestrates container lifecycle without persistent state.
type Manager struct {
	config    *config.Config
	gatewayIP string
}

type containerInspect struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

type networkInspect struct {
	Name    string            `json:"Name"`
	Options map[string]string `json:"Options"`
	IPAM    struct {
		Config []struct {
			Gateway string `json:"Gateway"`
		} `json:"Config"`
	} `json:"IPAM"`
}

// New creates a new Manager. It reconciles any previous containers and ensures
// the runner bridge exists.
func New(cfg *config.Config) (*Manager, error) {
	m := &Manager{
		config: cfg,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := m.reconcileContainers(ctx); err != nil {
		return nil, fmt.Errorf("reconcile managed containers: %w", err)
	}

	gatewayIP, err := m.ensureRunnerBridge(ctx)
	if err != nil {
		return nil, fmt.Errorf("ensure runner bridge: %w", err)
	}
	m.gatewayIP = gatewayIP

	if err := m.pullSandboxImage(ctx); err != nil {
		return nil, fmt.Errorf("pull sandbox image: %w", err)
	}

	return m, nil
}

// CreateContainer creates and starts a new container.
func (m *Manager) CreateContainer(ctx context.Context, sandboxID string, opts *CreateOptions) (*ContainerInfo, error) {
	if opts == nil {
		opts = &CreateOptions{}
	}

	// Validate sandboxID length before slicing to prevent panic
	if len(sandboxID) < 12 {
		return nil, fmt.Errorf("sandbox ID must be at least 12 characters, got %d", len(sandboxID))
	}

	containerName := "sandbox-" + sandboxID[:12]
	limits := m.defaultLimits()

	containerID, err := m.createContainer(ctx, sandboxID, containerName, m.config.DockerSandboxImage, limits)
	if err != nil {
		return nil, fmt.Errorf("create container: %w", err)
	}

	cleanupOnError := func() {
		_ = m.stopAndCleanContainer(ctx, containerID)
	}

	if err := m.startContainer(ctx, containerID); err != nil {
		cleanupOnError()
		return nil, fmt.Errorf("start container: %w", err)
	}

	containerIP, err := m.containerIP(ctx, containerID)
	if err != nil {
		cleanupOnError()
		return nil, fmt.Errorf("inspect container ip: %w", err)
	}

	if err := netrules.ApplyPolicy(containerID, containerIP, m.gatewayIP, daemonPort); err != nil {
		cleanupOnError()
		return nil, fmt.Errorf("apply network rules: %w", err)
	}

	baseURL := fmt.Sprintf("http://%s:%d", containerIP, daemonPort)
	if err := waitForDaemon(ctx, baseURL); err != nil {
		cleanupOnError()
		return nil, fmt.Errorf("connect to daemon: %w", err)
	}

	containerInfo := &ContainerInfo{
		ID:   containerID,
		Name: containerName,
		IP:   containerIP,
	}

	slog.Info("container created", "sandbox_id", sandboxID, "container_id", containerID, "ip", containerIP)
	return containerInfo, nil
}

// GetContainerInfo returns information about a container by its ID.
func (m *Manager) GetContainerInfo(ctx context.Context, containerID string) (*ContainerInfo, error) {
	inspect, err := m.inspectContainer(ctx, containerID)
	if err != nil {
		if isDockerNotFound(err) {
			return nil, ErrSandboxNotFound
		}
		return nil, err
	}

	network, ok := inspect.NetworkSettings.Networks[runnerBridgeNetwork]
	if !ok || network.IPAddress == "" {
		return nil, fmt.Errorf("%w: container %s has no IP on %s", ErrSandboxNetworkUnavailable, containerID, runnerBridgeNetwork)
	}

	return &ContainerInfo{
		ID:   inspect.ID,
		Name: inspect.Name,
		IP:   network.IPAddress,
	}, nil
}

// DaemonURL returns the daemon URL for a container by sandbox ID.
func (m *Manager) DaemonURL(ctx context.Context, sandboxID string) (string, error) {
	containerID, err := m.FindContainerIDByLabel(ctx, sandboxID)
	if err != nil {
		return "", err
	}

	inspect, err := m.inspectContainer(ctx, containerID)
	if err != nil {
		if isDockerNotFound(err) {
			return "", ErrSandboxNotFound
		}
		return "", err
	}
	if !inspect.State.Running {
		return "", ErrSandboxNotRunning
	}

	network, ok := inspect.NetworkSettings.Networks[runnerBridgeNetwork]
	if !ok || network.IPAddress == "" {
		return "", fmt.Errorf("%w: container %s has no IP on %s", ErrSandboxNetworkUnavailable, containerID, runnerBridgeNetwork)
	}

	baseURL := fmt.Sprintf("http://%s:%d", network.IPAddress, daemonPort)
	return baseURL, nil
}

// DeleteContainer stops and removes a container.
func (m *Manager) DeleteContainer(ctx context.Context, containerID string) error {
	if err := m.stopAndCleanContainer(ctx, containerID); err != nil {
		return err
	}

	slog.Info("container deleted", "container_id", containerID)
	return nil
}

// stopAndCleanContainer removes the container, then tears down its network
// rules. Order matters: rules must outlive the container so it cannot run
// unconfined during teardown. Both failure paths are logged; the
// removeContainer error is also returned so callers decide whether to bail.
func (m *Manager) stopAndCleanContainer(ctx context.Context, containerID string) error {
	if containerID == "" {
		return nil
	}
	if err := m.removeContainer(ctx, containerID); err != nil {
		slog.Warn("remove sandbox container", "container_id", containerID, "err", err)
		return err
	}
	if err := netrules.Teardown(containerID); err != nil {
		// TODO: consider adding metrics to track this in the future.
		slog.Warn("teardown network rules", "container_id", containerID, "err", err)
	}
	return nil
}

// Shutdown cleans up all managed containers.
func (m *Manager) Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := m.reconcileContainers(ctx); err != nil {
		slog.Warn("shutdown container cleanup", "err", err)
	}
}

func (m *Manager) defaultLimits() *ResourceLimits {
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

func waitForDaemon(ctx context.Context, baseURL string) error {
	httpClient := &http.Client{Timeout: 3 * time.Second}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(30 * time.Second)

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

func (m *Manager) reconcileContainers(ctx context.Context) error {
	ids, err := m.listManagedContainers(ctx)
	if err != nil {
		return err
	}
	// Best effort: startup should continue even if one stale managed
	// container can't be removed immediately. stopAndCleanContainer logs.
	for _, id := range ids {
		_ = m.stopAndCleanContainer(ctx, id)
	}
	return nil
}

func (m *Manager) ensureRunnerBridge(ctx context.Context) (string, error) {
	inspect, err := m.inspectRunnerBridge(ctx)
	if err != nil {
		if !isDockerNotFound(err) {
			return "", err
		}
		return m.createRunnerBridge(ctx)
	}

	wantICC := strconv.FormatBool(m.config.InterSandboxNetworkEnabled)
	gotICC, ok := inspect.Options["com.docker.network.bridge.enable_icc"]
	if !ok {
		gotICC = "true"
	}
	if gotICC != wantICC {
		if _, err := m.runDocker(ctx, "network", "rm", runnerBridgeNetwork); err != nil {
			return "", err
		}
		return m.createRunnerBridge(ctx)
	}

	return firstGateway(inspect), nil
}

func firstGateway(inspect *networkInspect) string {
	if inspect != nil && len(inspect.IPAM.Config) > 0 {
		return inspect.IPAM.Config[0].Gateway
	}
	return ""
}

func (m *Manager) createRunnerBridge(ctx context.Context) (string, error) {
	icc := strconv.FormatBool(m.config.InterSandboxNetworkEnabled)
	if _, err := m.runDocker(ctx, "network", "create", "--driver", "bridge", "--opt", "com.docker.network.bridge.enable_icc="+icc, runnerBridgeNetwork); err != nil {
		return "", err
	}
	inspect, err := m.inspectRunnerBridge(ctx)
	if err != nil {
		return "", err
	}
	return firstGateway(inspect), nil
}

func (m *Manager) pullSandboxImage(ctx context.Context) error {
	if _, err := m.runDocker(ctx, "image", "inspect", m.config.DockerSandboxImage); err == nil {
		slog.Info("sandbox image already present, skipping pull", "image", m.config.DockerSandboxImage)
		return nil
	}
	_, err := m.runDocker(ctx, "pull", m.config.DockerSandboxImage)
	return err
}

func (m *Manager) createContainer(ctx context.Context, sandboxID, containerName, image string, limits *ResourceLimits) (string, error) {
	args := []string{
		"container", "create",
		"--name", containerName,
		"--hostname", "sandbox",
		"--restart", "unless-stopped",
		"--network", runnerBridgeNetwork,
		"--label", containerLabelManaged + "=" + containerLabelManagedVal,
		"--label", containerLabelSandboxID + "=" + sandboxID,
		"--user", "1000:1000",
		"--env", "HOME=/home/user",
		"--env", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
	if m.config.EnableCgroups {
		args = append(args, dockerLimitArgs(limits)...)
	}
	args = append(args, dockerDiskQuotaArgs(limits)...)
	args = append(args, image)

	out, err := m.runDocker(ctx, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func dockerLimitArgs(limits *ResourceLimits) []string {
	if limits == nil {
		return nil
	}

	args := make([]string, 0, 6)
	if limits.MemoryMB > 0 {
		args = append(args, "--memory", fmt.Sprintf("%dm", limits.MemoryMB))
	}
	if limits.CPUPercent > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(float64(limits.CPUPercent)/100, 'f', 2, 64))
	}
	if limits.PidsMax > 0 {
		args = append(args, "--pids-limit", strconv.Itoa(limits.PidsMax))
	}
	return args
}

func dockerDiskQuotaArgs(limits *ResourceLimits) []string {
	if limits == nil || limits.DiskMB <= 0 {
		return nil
	}
	return []string{"--storage-opt", fmt.Sprintf("size=%dm", limits.DiskMB)}
}

func (m *Manager) startContainer(ctx context.Context, containerID string) error {
	_, err := m.runDocker(ctx, "container", "start", containerID)
	return err
}

func (m *Manager) removeContainer(ctx context.Context, containerID string) error {
	if containerID == "" {
		return nil
	}
	_, err := m.runDocker(ctx, "container", "rm", "-f", containerID)
	if isDockerNotFound(err) {
		return nil
	}
	return err
}

func (m *Manager) containerIP(ctx context.Context, containerID string) (string, error) {
	inspect, err := m.inspectContainer(ctx, containerID)
	if err != nil {
		return "", err
	}
	network, ok := inspect.NetworkSettings.Networks[runnerBridgeNetwork]
	if !ok || network.IPAddress == "" {
		return "", fmt.Errorf("container %s has no IP on %s", containerID, runnerBridgeNetwork)
	}
	return network.IPAddress, nil
}

func (m *Manager) inspectContainer(ctx context.Context, containerID string) (*containerInspect, error) {
	out, err := m.runDocker(ctx, "container", "inspect", containerID)
	if err != nil {
		return nil, err
	}
	var items []containerInspect
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		return nil, fmt.Errorf("decode container inspect: %w", err)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("container inspect returned no results for %s", containerID)
	}
	return &items[0], nil
}

func (m *Manager) inspectRunnerBridge(ctx context.Context) (*networkInspect, error) {
	out, err := m.runDocker(ctx, "network", "inspect", runnerBridgeNetwork)
	if err != nil {
		return nil, err
	}
	var items []networkInspect
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		return nil, fmt.Errorf("decode network inspect: %w", err)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("network inspect returned no results for %s", runnerBridgeNetwork)
	}
	return &items[0], nil
}

// ManagedContainerCount returns how many sandbox containers this runner is managing.
func (m *Manager) ManagedContainerCount(ctx context.Context) (int, error) {
	ids, err := m.listManagedContainers(ctx)
	if err != nil {
		return 0, err
	}
	return len(ids), nil
}

func (m *Manager) listManagedContainers(ctx context.Context) ([]string, error) {
	out, err := m.runDocker(ctx, "ps", "-aq", "--filter", "label="+containerLabelManaged+"="+containerLabelManagedVal)
	if err != nil {
		return nil, err
	}
	lines := strings.Fields(strings.TrimSpace(out))
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil
	}
	return lines, nil
}

// FindContainerIDByLabel finds a container ID by sandbox ID using label filters.
func (m *Manager) FindContainerIDByLabel(ctx context.Context, sandboxID string) (string, error) {
	out, err := m.runDocker(ctx, "ps", "-aq",
		"--filter", "label="+containerLabelManaged+"="+containerLabelManagedVal,
		"--filter", "label="+containerLabelSandboxID+"="+sandboxID)
	if err != nil {
		return "", err
	}
	lines := strings.Fields(strings.TrimSpace(out))
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		return "", ErrSandboxNotFound
	}
	return lines[0], nil
}

func (m *Manager) runDocker(ctx context.Context, args ...string) (string, error) {
	return m.runDockerWithStdin(ctx, nil, args...)
}

func (m *Manager) runDockerWithStdin(ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = append(os.Environ(), "DOCKER_HOST="+m.config.DockerHost)
	if stdin != nil {
		cmd.Stdin = stdin
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return "", fmt.Errorf("docker %s: %s: %w", strings.Join(args, " "), msg, err)
	}
	return stdout.String(), nil
}

func isDockerNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such container") ||
		strings.Contains(msg, "no such network") ||
		strings.Contains(msg, "no such image") ||
		strings.Contains(msg, "not found")
}
