package manager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
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
	StatusRunning    = "running"
	StatusTerminated = "terminated"
	daemonPort       = 8081
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
	docker    *dockerClient
}

// New creates a new Manager. It reconciles any previous containers and ensures
// the runner bridge exists.
func New(cfg *config.Config) (*Manager, error) {
	m := &Manager{
		config: cfg,
		docker: &dockerClient{host: cfg.DockerHost},
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

	if err := m.docker.pullImage(ctx, m.config.DockerSandboxImage); err != nil {
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

	containerID, err := m.docker.createContainer(ctx, sandboxID, containerName, m.config.DockerSandboxImage, limits, m.config.EnableCgroups)
	if err != nil {
		return nil, fmt.Errorf("create container: %w", err)
	}

	cleanupOnError := func(containerIP string) {
		if err := netrules.Teardown(containerID); err != nil {
			slog.Warn("teardown network rules", "container_id", containerID, "err", err)
		}
		if err := m.docker.removeContainer(ctx, containerID); err != nil {
			slog.Warn("remove container after create failure", "container_id", containerID, "err", err)
		}
	}

	if err := m.docker.startContainer(ctx, containerID); err != nil {
		cleanupOnError("")
		return nil, fmt.Errorf("start container: %w", err)
	}

	containerIP, err := m.docker.containerIP(ctx, containerID)
	if err != nil {
		cleanupOnError("")
		return nil, fmt.Errorf("inspect container ip: %w", err)
	}

	if err := netrules.ApplyPolicy(containerID, containerIP, m.gatewayIP, daemonPort); err != nil {
		cleanupOnError(containerIP)
		return nil, fmt.Errorf("apply network rules: %w", err)
	}

	baseURL := fmt.Sprintf("http://%s:%d", containerIP, daemonPort)
	if err := waitForDaemon(ctx, baseURL); err != nil {
		cleanupOnError(containerIP)
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
	inspect, err := m.docker.inspectContainer(ctx, containerID)
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

	inspect, err := m.docker.inspectContainer(ctx, containerID)
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
	if err := netrules.Teardown(containerID); err != nil {
		slog.Warn("teardown network rules", "container_id", containerID, "err", err)
	}

	if containerID != "" {
		if err := m.docker.removeContainer(ctx, containerID); err != nil {
			slog.Warn("remove container", "container_id", containerID, "err", err)
			return err
		}
	}

	slog.Info("container deleted", "container_id", containerID)
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
	return &ResourceLimits{
		MemoryMB:   m.config.DefaultMemoryMB,
		CPUPercent: m.config.DefaultCPUPercent,
		PidsMax:    m.config.DefaultPidsMax,
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
	ids, err := m.docker.listContainersByLabel(ctx, containerLabelManaged, containerLabelManagedVal)
	if err != nil {
		return err
	}
	for _, id := range ids {
		_, inspectErr := m.docker.inspectContainer(ctx, id)
		if inspectErr == nil {
			if err := netrules.Teardown(id); err != nil {
				slog.Warn("teardown rules during reconcile", "container_id", id, "err", err)
			}
		} else if err := netrules.Teardown(id); err != nil {
			slog.Warn("teardown rules during reconcile", "container_id", id, "err", err)
		}
		if err := m.docker.removeContainer(ctx, id); err != nil {
			// Best effort: startup should continue even if one stale managed
			// container can't be removed immediately.
			slog.Warn("remove container during reconcile", "container_id", id, "err", err)
			continue
		}
	}
	return nil
}

func (m *Manager) ensureRunnerBridge(ctx context.Context) (string, error) {
	inspect, err := m.docker.inspectNetwork(ctx, runnerBridgeNetwork)
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
		if _, err := m.docker.run(ctx, "network", "rm", runnerBridgeNetwork); err != nil {
			return "", err
		}
		return m.createRunnerBridge(ctx)
	}

	return firstGateway(inspect), nil
}

func (m *Manager) createRunnerBridge(ctx context.Context) (string, error) {
	icc := strconv.FormatBool(m.config.InterSandboxNetworkEnabled)
	if _, err := m.docker.run(ctx, "network", "create", "--driver", "bridge", "--opt", "com.docker.network.bridge.enable_icc="+icc, runnerBridgeNetwork); err != nil {
		return "", err
	}
	inspect, err := m.docker.inspectNetwork(ctx, runnerBridgeNetwork)
	if err != nil {
		return "", err
	}
	return firstGateway(inspect), nil
}

// ManagedContainerCount returns how many sandbox containers this runner is managing.
func (m *Manager) ManagedContainerCount(ctx context.Context) (int, error) {
	ids, err := m.docker.listContainersByLabel(ctx, containerLabelManaged, containerLabelManagedVal)
	if err != nil {
		return 0, err
	}
	return len(ids), nil
}

// FindContainerIDByLabel finds a container ID by sandbox ID using label filters.
func (m *Manager) FindContainerIDByLabel(ctx context.Context, sandboxID string) (string, error) {
	lines, err := m.docker.findContainerByLabels(ctx,
		"label="+containerLabelManaged+"="+containerLabelManagedVal,
		"label="+containerLabelSandboxID+"="+sandboxID)
	if err != nil {
		return "", err
	}
	if len(lines) == 0 {
		return "", ErrSandboxNotFound
	}
	return lines[0], nil
}
