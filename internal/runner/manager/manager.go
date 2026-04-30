package manager

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/n8n-io/sandbox-service/internal/runner/config"
	"github.com/n8n-io/sandbox-service/internal/runner/netrules"
)

// ErrSandboxNotFound is returned when a sandbox ID is not found or not running.
var ErrSandboxNotFound = errors.New("sandbox not found")

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
type CreateOptions struct {
	NetworkPolicy   *NetworkPolicy
	ResourceLimits  *ResourceLimits
	DockerfileSteps []string
}

// ContainerInfo represents information about a created container.
type ContainerInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	IP       string `json:"ip"`
	ImageTag string `json:"image_tag"`
}

// ImageInfo represents information about a built image.
type ImageInfo struct {
	ID            string `json:"id"`
	Tag           string `json:"tag"`
	DockerImageID string `json:"docker_image_id"`
}

// Manager orchestrates container lifecycle without persistent state.
type Manager struct {
	config    *config.Config
	gatewayIP string
}

type containerInspect struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
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
	limits := m.effectiveLimits(opts.ResourceLimits)
	imageName := m.config.DockerSandboxImage

	if len(opts.DockerfileSteps) > 0 {
		imageInfo, err := m.BuildImage(ctx, BuildImageOptions{
			BaseImage:       m.config.DockerSandboxImage,
			DockerfileSteps: opts.DockerfileSteps,
		})
		if err != nil {
			return nil, fmt.Errorf("build image: %w", err)
		}
		imageName = imageInfo.Tag
	}

	containerID, err := m.createContainer(ctx, sandboxID, containerName, imageName, limits)
	if err != nil {
		return nil, fmt.Errorf("create container: %w", err)
	}

	cleanupOnError := func(containerIP string) {
		if containerIP != "" {
			if err := netrules.Teardown(containerID, containerIP); err != nil {
				slog.Warn("teardown network rules", "container_id", containerID, "err", err)
			}
		} else if err := netrules.Teardown(containerID, ""); err != nil {
			slog.Warn("teardown network rules", "container_id", containerID, "err", err)
		}
		if err := m.removeContainer(ctx, containerID); err != nil {
			slog.Warn("remove container after create failure", "container_id", containerID, "err", err)
		}
	}

	if err := m.startContainer(ctx, containerID); err != nil {
		cleanupOnError("")
		return nil, fmt.Errorf("start container: %w", err)
	}

	containerIP, err := m.containerIP(ctx, containerID)
	if err != nil {
		cleanupOnError("")
		return nil, fmt.Errorf("inspect container ip: %w", err)
	}

	var allowedIPs, deniedIPs []string
	if opts.NetworkPolicy != nil {
		allowedIPs = opts.NetworkPolicy.AllowedIPs
		deniedIPs = opts.NetworkPolicy.DeniedIPs
	}
	if err := netrules.ApplyPolicy(containerID, containerIP, m.gatewayIP, daemonPort, allowedIPs, deniedIPs); err != nil {
		cleanupOnError(containerIP)
		return nil, fmt.Errorf("apply network rules: %w", err)
	}

	baseURL := fmt.Sprintf("http://%s:%d", containerIP, daemonPort)
	if err := waitForDaemon(ctx, baseURL); err != nil {
		cleanupOnError(containerIP)
		return nil, fmt.Errorf("connect to daemon: %w", err)
	}

	containerInfo := &ContainerInfo{
		ID:       containerID,
		Name:     containerName,
		IP:       containerIP,
		ImageTag: imageName,
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
		return nil, fmt.Errorf("container %s has no IP on %s", containerID, runnerBridgeNetwork)
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
	info, err := m.GetContainerInfo(ctx, containerID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("http://%s:%d", info.IP, daemonPort), nil
}

// DeleteDockerImage removes a Docker image by tag.
func (m *Manager) DeleteDockerImage(ctx context.Context, imageTag string) error {
	if _, err := m.runDocker(ctx, "image", "rm", imageTag); err != nil {
		if isDockerNotFound(err) {
			return ErrImageNotFound
		}
		return err
	}
	return nil
}

// BuildImage builds a custom Docker image and returns image information.
func (m *Manager) BuildImage(ctx context.Context, opts BuildImageOptions) (*ImageInfo, error) {
	if strings.TrimSpace(opts.BaseImage) == "" {
		return nil, fmt.Errorf("base image is required")
	}
	if len(opts.DockerfileSteps) == 0 {
		return nil, fmt.Errorf("at least one dockerfile step is required")
	}

	dockerfile, err := buildDockerfile(opts.BaseImage, opts.DockerfileSteps)
	if err != nil {
		return nil, err
	}

	buildID := strings.ReplaceAll(uuid.NewString(), "-", "")
	imageTag := "sandbox-custom-" + buildID
	imageID := "img-" + buildID

	buildCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	tmpDir, err := os.MkdirTemp("", "sandbox-image-build-*")
	if err != nil {
		return nil, fmt.Errorf("create build context dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	if _, err := m.runDockerWithStdin(buildCtx, strings.NewReader(dockerfile), "build", "-t", imageTag, "-f", "-", filepath.Clean(tmpDir)); err != nil {
		return nil, err
	}

	dockerImageID, err := m.inspectImageID(buildCtx, imageTag)
	if err != nil {
		return nil, err
	}

	return &ImageInfo{
		ID:            imageID,
		Tag:           imageTag,
		DockerImageID: dockerImageID,
	}, nil
}

// DeleteContainer stops and removes a container.
func (m *Manager) DeleteContainer(ctx context.Context, containerID, containerIP string) error {
	if err := netrules.Teardown(containerID, containerIP); err != nil {
		slog.Warn("teardown network rules", "container_id", containerID, "err", err)
	}

	if containerID != "" {
		if err := m.removeContainer(ctx, containerID); err != nil {
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

// effectiveLimits merges per-sandbox overrides with global defaults.
func (m *Manager) effectiveLimits(requested *ResourceLimits) *ResourceLimits {
	limits := &ResourceLimits{
		MemoryMB:   m.config.DefaultMemoryMB,
		CPUPercent: m.config.DefaultCPUPercent,
		PidsMax:    m.config.DefaultPidsMax,
	}
	if requested != nil {
		if requested.MemoryMB > 0 {
			limits.MemoryMB = requested.MemoryMB
		}
		if requested.CPUPercent > 0 {
			limits.CPUPercent = requested.CPUPercent
		}
		if requested.PidsMax > 0 {
			limits.PidsMax = requested.PidsMax
		}
	}
	return limits
}

func waitForDaemon(ctx context.Context, baseURL string) error {
	httpClient := &http.Client{Timeout: time.Second}
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
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
	}
}

func (m *Manager) reconcileContainers(ctx context.Context) error {
	ids, err := m.listManagedContainers(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		inspect, inspectErr := m.inspectContainer(ctx, id)
		if inspectErr == nil {
			if err := netrules.Teardown(id, inspect.NetworkSettings.Networks[runnerBridgeNetwork].IPAddress); err != nil {
				slog.Warn("teardown rules during reconcile", "container_id", id, "err", err)
			}
		} else if err := netrules.Teardown(id, ""); err != nil {
			slog.Warn("teardown rules during reconcile", "container_id", id, "err", err)
		}
		if err := m.removeContainer(ctx, id); err != nil {
			// Best effort: startup should continue even if one stale managed
			// container can't be removed immediately.
			slog.Warn("remove container during reconcile", "container_id", id, "err", err)
			continue
		}
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

func (m *Manager) ensureDockerImageExists(ctx context.Context, image string) error {
	if _, err := m.inspectImageID(ctx, image); err != nil {
		if isDockerNotFound(err) {
			return fmt.Errorf("custom image %s is missing from docker; rebuild it before reuse", image)
		}
		return err
	}
	return nil
}

func (m *Manager) inspectImageID(ctx context.Context, image string) (string, error) {
	out, err := m.runDocker(ctx, "image", "inspect", "--format", "{{.Id}}", image)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func buildDockerfile(baseImage string, dockerfileSteps []string) (string, error) {
	baseImage = strings.TrimSpace(baseImage)
	if baseImage == "" {
		return "", fmt.Errorf("base image is required")
	}

	lines := []string{
		"FROM " + baseImage,
	}
	for i, step := range dockerfileSteps {
		step = strings.TrimSpace(step)
		if step == "" {
			return "", fmt.Errorf("dockerfile_steps[%d] must be a non-empty string", i)
		}
		lines = append(lines, step)
	}
	lines = append(lines, "")
	return strings.Join(lines, "\n"), nil
}

func stepsHash(baseImage string, dockerfileSteps []string) string {
	h := sha256.New()
	h.Write([]byte(baseImage))
	h.Write([]byte{0})
	for _, step := range dockerfileSteps {
		h.Write([]byte(step))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
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

func shouldDeleteCachedImageRecord(err error) bool {
	return err != nil && isDockerNotFound(err)
}
