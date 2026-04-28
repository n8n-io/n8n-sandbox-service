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
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/n8n-io/sandbox-service/internal/config"
	"github.com/n8n-io/sandbox-service/internal/netrules"
	"github.com/n8n-io/sandbox-service/internal/store"
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

// Sandbox represents an active sandbox with its associated resources.
type Sandbox struct {
	ID            string
	Record        *store.SandboxRecord
	ContainerID   string
	ContainerName string
	ContainerIP   string
	DaemonBaseURL string
}

// Manager orchestrates sandbox lifecycle.
type Manager struct {
	mu        sync.RWMutex
	sandboxes map[string]*Sandbox
	store     *store.Store
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

// New creates a new Manager. It reconciles any previous containers, ensures
// the runner bridge exists, pulls the sandbox image, then marks stale DB rows
// terminated.
func New(s *store.Store, cfg *config.Config) (*Manager, error) {
	m := &Manager{
		sandboxes: make(map[string]*Sandbox),
		store:     s,
		config:    cfg,
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

	if err := s.MarkAllTerminated(); err != nil {
		return nil, fmt.Errorf("mark stale sandboxes: %w", err)
	}

	return m, nil
}

// Create creates and starts a new sandbox.
func (m *Manager) Create(ctx context.Context, opts *CreateOptions) (*store.SandboxRecord, error) {
	if opts == nil {
		opts = &CreateOptions{}
	}

	id := uuid.New().String()
	now := time.Now().Unix()
	containerName := "sandbox-" + id[:12]
	limits := m.effectiveLimits(opts.ResourceLimits)
	imageName := m.config.DockerSandboxImage
	imageID := ""

	if len(opts.DockerfileSteps) > 0 {
		imageRec, err := m.BuildImage(ctx, BuildImageOptions{
			BaseImage:       m.config.DockerSandboxImage,
			DockerfileSteps: opts.DockerfileSteps,
		})
		if err != nil {
			return nil, fmt.Errorf("build image: %w", err)
		}
		imageName = imageRec.Tag
		imageID = imageRec.ID
	}

	containerID, err := m.createContainer(ctx, id, containerName, imageName, limits)
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

	policyJSON := "{}"
	if opts.NetworkPolicy != nil {
		if b, err := json.Marshal(opts.NetworkPolicy); err == nil {
			policyJSON = string(b)
		}
	}

	limitsJSON := "{}"
	if b, err := json.Marshal(limits); err == nil {
		limitsJSON = string(b)
	}

	record := &store.SandboxRecord{
		ID:             id,
		Status:         StatusRunning,
		CreatedAt:      now,
		LastActiveAt:   now,
		ContainerID:    containerID,
		ContainerIP:    containerIP,
		DaemonPort:     daemonPort,
		ImageID:        imageID,
		NetworkPolicy:  policyJSON,
		ResourceLimits: limitsJSON,
	}
	if err := m.store.Create(record); err != nil {
		cleanupOnError(containerIP)
		return nil, fmt.Errorf("store create: %w", err)
	}

	sb := &Sandbox{
		ID:            id,
		Record:        record,
		ContainerID:   containerID,
		ContainerName: containerName,
		ContainerIP:   containerIP,
		DaemonBaseURL: baseURL,
	}

	m.mu.Lock()
	m.sandboxes[id] = sb
	m.mu.Unlock()

	slog.Info("sandbox created", "id", id, "container_id", containerID, "ip", containerIP)
	return record, nil
}

// Get returns the sandbox record and updates last_active_at.
func (m *Manager) Get(ctx context.Context, id string) (*store.SandboxRecord, error) {
	m.mu.RLock()
	sb, ok := m.sandboxes[id]
	m.mu.RUnlock()

	if !ok {
		rec, err := m.store.Get(id)
		if err != nil {
			return nil, fmt.Errorf("store get: %w", err)
		}
		if rec == nil {
			return nil, nil
		}
		return rec, nil
	}

	if err := m.store.UpdateLastActive(id); err != nil {
		slog.Warn("update last_active_at", "id", id, "err", err)
	}
	sb.Record.LastActiveAt = time.Now().Unix()

	return sb.Record, nil
}

// List returns all sandbox records from the store.
func (m *Manager) List(ctx context.Context) ([]*store.SandboxRecord, error) {
	return m.store.List()
}

// GetImage returns a custom image by ID or tag.
func (m *Manager) GetImage(ctx context.Context, id string) (*store.ImageRecord, error) {
	rec, err := m.store.GetImage(id)
	if err != nil {
		return nil, fmt.Errorf("store get image: %w", err)
	}
	if rec == nil {
		return nil, fmt.Errorf("%w: %s", ErrImageNotFound, id)
	}
	return rec, nil
}

// ListImages returns all custom images recorded in the store.
func (m *Manager) ListImages(ctx context.Context) ([]*store.ImageRecord, error) {
	return m.store.ListImages()
}

// DeleteImage removes a custom image if no running sandbox still references it.
func (m *Manager) DeleteImage(ctx context.Context, id string) error {
	rec, err := m.store.GetImage(id)
	if err != nil {
		return fmt.Errorf("store get image: %w", err)
	}
	if rec == nil {
		return fmt.Errorf("%w: %s", ErrImageNotFound, id)
	}

	count, err := m.store.CountSandboxesByImageID(rec.ID)
	if err != nil {
		return fmt.Errorf("count sandboxes by image: %w", err)
	}
	if count > 0 {
		return fmt.Errorf("%w: %s", ErrImageInUse, rec.ID)
	}

	if _, err := m.runDocker(ctx, "image", "rm", rec.Tag); err != nil && !isDockerNotFound(err) {
		return err
	}
	if err := m.store.DeleteImage(rec.ID); err != nil {
		return fmt.Errorf("store delete image: %w", err)
	}
	return nil
}

// BuildImage builds a custom Docker image (or returns a cached one) and persists it.
func (m *Manager) BuildImage(ctx context.Context, opts BuildImageOptions) (*store.ImageRecord, error) {
	if strings.TrimSpace(opts.BaseImage) == "" {
		return nil, fmt.Errorf("base image is required")
	}
	if len(opts.DockerfileSteps) == 0 {
		return nil, fmt.Errorf("at least one dockerfile step is required")
	}

	hash := stepsHash(opts.BaseImage, opts.DockerfileSteps)

	// Cache lookup.
	cached, err := m.store.GetImageByStepsHash(hash)
	if err != nil {
		return nil, fmt.Errorf("lookup cached image: %w", err)
	}
	if cached != nil {
		_, inspectErr := m.inspectImageID(ctx, cached.Tag)
		if inspectErr == nil {
			slog.Info("using cached image", "id", cached.ID, "tag", cached.Tag)
			return cached, nil
		}
		if shouldDeleteCachedImageRecord(inspectErr) {
			// Stale record: Docker image is gone.
			if delErr := m.store.DeleteImage(cached.ID); delErr != nil {
				slog.Warn("delete stale image record", "id", cached.ID, "err", delErr)
			}
		} else {
			return nil, fmt.Errorf("inspect cached image %s: %w", cached.Tag, inspectErr)
		}
	}

	dockerfile, err := buildDockerfile(opts.BaseImage, opts.DockerfileSteps)
	if err != nil {
		return nil, err
	}

	stepsJSON, err := json.Marshal(opts.DockerfileSteps)
	if err != nil {
		return nil, fmt.Errorf("marshal dockerfile steps: %w", err)
	}

	buildID := strings.ReplaceAll(uuid.NewString(), "-", "")
	record := &store.ImageRecord{
		ID:              "img-" + buildID,
		Tag:             "sandbox-custom-" + buildID,
		BaseImage:       opts.BaseImage,
		StepsHash:       hash,
		DockerfileSteps: string(stepsJSON),
		CreatedAt:       time.Now().Unix(),
	}

	buildCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	tmpDir, err := os.MkdirTemp("", "sandbox-image-build-*")
	if err != nil {
		return nil, fmt.Errorf("create build context dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	if _, err := m.runDockerWithStdin(buildCtx, strings.NewReader(dockerfile), "build", "-t", record.Tag, "-f", "-", filepath.Clean(tmpDir)); err != nil {
		return nil, err
	}

	record.DockerImageID, err = m.inspectImageID(buildCtx, record.Tag)
	if err != nil {
		return nil, err
	}

	cleanupBuiltImage := func() {
		if _, cleanupErr := m.runDocker(context.Background(), "image", "rm", record.Tag); cleanupErr != nil && !isDockerNotFound(cleanupErr) {
			slog.Warn("remove custom image after store failure", "tag", record.Tag, "err", cleanupErr)
		}
	}

	if err := m.store.CreateImage(record); err != nil {
		if errors.Is(err, store.ErrImageStepsHashConflict) {
			for attempt := 0; attempt < 2; attempt++ {
				existing, getErr := m.store.GetImageByStepsHash(record.StepsHash)
				if getErr != nil {
					cleanupBuiltImage()
					return nil, fmt.Errorf("lookup existing image after steps hash conflict: %w", getErr)
				}
				if existing != nil {
					if _, inspectErr := m.inspectImageID(ctx, existing.Tag); inspectErr == nil {
						cleanupBuiltImage()
						return existing, nil
					} else if shouldDeleteCachedImageRecord(inspectErr) {
						if delErr := m.store.DeleteImage(existing.ID); delErr != nil {
							cleanupBuiltImage()
							return nil, fmt.Errorf("delete stale image record after steps hash conflict: %w", delErr)
						}
					} else {
						cleanupBuiltImage()
						return nil, fmt.Errorf("inspect conflicting cached image %s: %w", existing.Tag, inspectErr)
					}
				}

				retryErr := m.store.CreateImage(record)
				if retryErr == nil {
					return record, nil
				}
				if !errors.Is(retryErr, store.ErrImageStepsHashConflict) {
					cleanupBuiltImage()
					return nil, fmt.Errorf("store create image after steps hash conflict: %w", retryErr)
				}
			}
		}
		cleanupBuiltImage()
		return nil, fmt.Errorf("store create image: %w", err)
	}
	return record, nil
}

// Delete stops and removes a sandbox.
func (m *Manager) Delete(ctx context.Context, id string) error {
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	if ok {
		delete(m.sandboxes, id)
	}
	m.mu.Unlock()

	if ok {
		return m.cleanupSandbox(sb)
	}

	rec, err := m.store.Get(id)
	if err != nil {
		return fmt.Errorf("store get: %w", err)
	}
	if rec == nil {
		return m.store.Delete(id)
	}
	return m.cleanupSandbox(&Sandbox{ID: id, Record: rec, ContainerID: rec.ContainerID, ContainerIP: rec.ContainerIP})
}

// DaemonURL returns the daemon base URL for the sandbox and updates last_active_at.
func (m *Manager) DaemonURL(ctx context.Context, id string) (string, error) {
	sb, err := m.getSandbox(id)
	if err != nil {
		return "", err
	}
	_ = m.store.UpdateLastActive(id)
	return sb.DaemonBaseURL, nil
}

// Shutdown stops all sandboxes gracefully.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	sandboxes := make([]*Sandbox, 0, len(m.sandboxes))
	for _, sb := range m.sandboxes {
		sandboxes = append(sandboxes, sb)
	}
	m.sandboxes = make(map[string]*Sandbox)
	m.mu.Unlock()

	for _, sb := range sandboxes {
		if err := m.cleanupSandbox(sb); err != nil {
			slog.Warn("shutdown sandbox cleanup", "id", sb.ID, "err", err)
		}
	}
}

func (m *Manager) cleanupSandbox(sb *Sandbox) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := netrules.Teardown(sb.ContainerID, sb.ContainerIP); err != nil {
		slog.Warn("teardown network rules", "id", sb.ID, "container_id", sb.ContainerID, "err", err)
	}

	if sb.ContainerID != "" {
		if err := m.removeContainer(ctx, sb.ContainerID); err != nil {
			slog.Warn("remove container", "id", sb.ID, "container_id", sb.ContainerID, "err", err)
		}
	}

	if err := m.store.Delete(sb.ID); err != nil {
		slog.Warn("store delete", "id", sb.ID, "err", err)
	}

	slog.Info("sandbox deleted", "id", sb.ID, "container_id", sb.ContainerID)
	return nil
}

// getSandbox returns the active sandbox or an error.
func (m *Manager) getSandbox(id string) (*Sandbox, error) {
	m.mu.RLock()
	sb, ok := m.sandboxes[id]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrSandboxNotFound, id)
	}
	return sb, nil
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
			return err
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
